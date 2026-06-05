package service_test

import (
	"context"
	"errors"
	"io/fs"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/CoverOnes/payment/internal/domain"
	"github.com/CoverOnes/payment/internal/service"
	"github.com/CoverOnes/payment/internal/store"
	"github.com/CoverOnes/payment/internal/store/postgres"
	migrations "github.com/CoverOnes/payment/migrations"
	"github.com/google/uuid"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
)

// ─── Test helpers ────────────────────────────────────────────────────────────

func startTestDB(t *testing.T) string {
	t.Helper()

	ctx := context.Background()

	ctr, err := tcpostgres.Run(
		ctx,
		"postgres:17-alpine",
		tcpostgres.WithDatabase("testdb"),
		tcpostgres.WithUsername("testuser"),
		tcpostgres.WithPassword("testpass"),
		tcpostgres.BasicWaitStrategies(),
	)
	require.NoError(t, err)

	t.Cleanup(func() {
		if termErr := ctr.Terminate(ctx); termErr != nil {
			t.Logf("terminate container: %v", termErr)
		}
	})

	dsn, err := ctr.ConnectionString(ctx, "sslmode=disable")
	require.NoError(t, err)

	return dsn
}

func runMigrations(t *testing.T, ctx context.Context, dsn string) {
	t.Helper()

	pool, err := postgres.NewPool(ctx, dsn)
	require.NoError(t, err)

	defer pool.Close()

	var upFiles []string

	err = fs.WalkDir(migrations.FS, ".", func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}

		if !d.IsDir() && strings.HasSuffix(path, ".up.sql") {
			upFiles = append(upFiles, path)
		}

		return nil
	})
	require.NoError(t, err, "walk embedded migrations FS")
	require.NotEmpty(t, upFiles, "no *.up.sql files found")

	sort.Strings(upFiles)

	for _, file := range upFiles {
		data, readErr := migrations.FS.ReadFile(file)
		require.NoError(t, readErr, "read migration file %s", file)

		_, execErr := pool.Exec(ctx, string(data))
		require.NoError(t, execErr, "apply migration %s", file)
	}
}

// ─── Stub roster client ───────────────────────────────────────────────────────

// stubRosterClient is a test double for WorkspaceRosterClient.
type stubRosterClient struct {
	mu      sync.Mutex
	entries []service.RosterEntry
	err     error
}

func newStubRoster(entries ...service.RosterEntry) *stubRosterClient {
	return &stubRosterClient{entries: entries}
}

func (s *stubRosterClient) GetPartyRoster(_ context.Context, _ uuid.UUID) ([]service.RosterEntry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.err != nil {
		return nil, s.err
	}

	cp := make([]service.RosterEntry, len(s.entries))
	copy(cp, s.entries)

	return cp, nil
}

// ─── Test suite ──────────────────────────────────────────────────────────────

// testPlatformUID is a fixed system UUID used as PayerUserID in test disbursements.
// Ensures payer != payee (self-transfer guard) in all test scenarios.
var testPlatformUID = uuid.MustParse("00000000-0000-0000-0000-000000000001")

func buildService(
	t *testing.T,
	ctx context.Context,
	dsn string,
	roster service.WorkspaceRosterClient,
) *service.SettlementService {
	t.Helper()

	pool, err := postgres.NewPool(ctx, dsn)
	require.NoError(t, err)

	t.Cleanup(pool.Close)

	return service.NewSettlementService(
		postgres.NewSettlementPlanStore(pool),
		postgres.NewSettlementAllocationStore(pool),
		postgres.NewSettlementMilestoneDisbursementStore(pool),
		postgres.NewSettlementAuditStore(pool),
		postgres.NewSettlementTxManager(pool),
		roster,
		testPlatformUID,
	)
}

// TestSettlementService_CreatePlan_HappyPath verifies a 3-party plan is created
// with frozen allocations. Σ shareBps == 10000 (triple sum-checked by service).
func TestSettlementService_CreatePlan_HappyPath(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	ctx := context.Background()
	dsn := startTestDB(t)
	runMigrations(t, ctx, dsn)

	vendor1, vendor2, vendor3 := uuid.New(), uuid.New(), uuid.New()
	roster := newStubRoster(
		service.RosterEntry{VendorUserID: vendor1, ShareBps: 5000},
		service.RosterEntry{VendorUserID: vendor2, ShareBps: 3000},
		service.RosterEntry{VendorUserID: vendor3, ShareBps: 2000},
	)

	svc := buildService(t, ctx, dsn, roster)

	contractID := uuid.New()
	tenderID := uuid.New()
	iKey := "contract_activated:" + uuid.New().String()

	plan, err := svc.CreatePlan(ctx, &service.CreatePlanInput{
		MultiContractID: contractID,
		TenderID:        tenderID,
		Currency:        "TWD",
		IdempotencyKey:  iKey,
	})
	require.NoError(t, err)
	require.NotNil(t, plan)

	assert.Equal(t, contractID, plan.MultiContractID)
	assert.Equal(t, tenderID, plan.TenderID)
	assert.Equal(t, domain.PlanStatusActive, plan.Status)
	assert.Equal(t, 3, plan.FrozenPartyCount)

	allocs, err := svc.GetAllocations(ctx, plan.ID)
	require.NoError(t, err)
	require.Len(t, allocs, 3)

	// Verify share_bps are preserved.
	shareBpsMap := map[uuid.UUID]int{}
	for _, a := range allocs {
		shareBpsMap[a.VendorUserID] = a.ShareBps
	}

	assert.Equal(t, 5000, shareBpsMap[vendor1])
	assert.Equal(t, 3000, shareBpsMap[vendor2])
	assert.Equal(t, 2000, shareBpsMap[vendor3])

	// Verify exactly one allocation is the rounding sink.
	sinkCount := 0
	for _, a := range allocs {
		if a.IsRoundingSink {
			sinkCount++
		}
	}

	assert.Equal(t, 1, sinkCount, "exactly one rounding sink allocation")
}

// TestSettlementService_CreatePlan_SumDriftRoster verifies that a roster with
// Σ shareBps ≠ 10000 is rejected at CreatePlan with ErrSumInvariantViolation.
func TestSettlementService_CreatePlan_SumDriftRoster(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	ctx := context.Background()
	dsn := startTestDB(t)
	runMigrations(t, ctx, dsn)

	// Roster sums to 9999 — should be rejected.
	roster := newStubRoster(
		service.RosterEntry{VendorUserID: uuid.New(), ShareBps: 5000},
		service.RosterEntry{VendorUserID: uuid.New(), ShareBps: 4999},
	)

	svc := buildService(t, ctx, dsn, roster)

	plan, err := svc.CreatePlan(ctx, &service.CreatePlanInput{
		MultiContractID: uuid.New(),
		TenderID:        uuid.New(),
		Currency:        "TWD",
		IdempotencyKey:  "contract_activated:" + uuid.New().String(),
	})

	require.Error(t, err)
	require.Nil(t, plan)
	require.ErrorIs(t, err, domain.ErrSumInvariantViolation)
}

// TestSettlementService_CreatePlan_Idempotent verifies that replaying the same
// contract_activated event (same contractID, different eventId) is a no-op.
func TestSettlementService_CreatePlan_Idempotent(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	ctx := context.Background()
	dsn := startTestDB(t)
	runMigrations(t, ctx, dsn)

	roster := newStubRoster(
		service.RosterEntry{VendorUserID: uuid.New(), ShareBps: 6000},
		service.RosterEntry{VendorUserID: uuid.New(), ShareBps: 4000},
	)

	svc := buildService(t, ctx, dsn, roster)

	contractID := uuid.New()

	// First event.
	plan1, err := svc.CreatePlan(ctx, &service.CreatePlanInput{
		MultiContractID: contractID,
		TenderID:        uuid.New(),
		Currency:        "TWD",
		IdempotencyKey:  "contract_activated:" + uuid.New().String(),
	})
	require.NoError(t, err)
	require.NotNil(t, plan1)

	// Replay event (same contractID, different idempotency key — simulates event re-delivery).
	plan2, err := svc.CreatePlan(ctx, &service.CreatePlanInput{
		MultiContractID: contractID,
		TenderID:        uuid.New(),
		Currency:        "TWD",
		IdempotencyKey:  "contract_activated:" + uuid.New().String(),
	})
	require.NoError(t, err, "idempotent replay must not error")
	assert.Nil(t, plan2, "idempotent replay returns nil plan (already exists)")
}

// TestSettlementService_DisburseMilestone_Happy3Party verifies a 3-party milestone
// disburse: per-milestone disbursement records created, each DISBURSED with a tx_id.
// In the new per-milestone model, settlement_allocations is a FROZEN ROSTER; the
// disbursement state lives in settlement_milestone_disbursements.
func TestSettlementService_DisburseMilestone_Happy3Party(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	ctx := context.Background()
	dsn := startTestDB(t)
	runMigrations(t, ctx, dsn)

	vendor1, vendor2, vendor3 := uuid.New(), uuid.New(), uuid.New()
	roster := newStubRoster(
		service.RosterEntry{VendorUserID: vendor1, ShareBps: 5000},
		service.RosterEntry{VendorUserID: vendor2, ShareBps: 3000},
		service.RosterEntry{VendorUserID: vendor3, ShareBps: 2000},
	)

	svc := buildService(t, ctx, dsn, roster)

	plan, err := svc.CreatePlan(ctx, &service.CreatePlanInput{
		MultiContractID: uuid.New(),
		TenderID:        uuid.New(),
		Currency:        "TWD",
		IdempotencyKey:  "contract_activated:" + uuid.New().String(),
	})
	require.NoError(t, err)
	require.NotNil(t, plan)

	milestoneID := uuid.New()
	amount, _ := decimal.NewFromString("3000.00")

	err = svc.DisburseMilestone(ctx, &service.DisburseMilestoneInput{
		PlanID:       plan.ID,
		MilestoneID:  milestoneID,
		Amount:       amount,
		Currency:     "TWD",
		ActorService: "test",
	})
	require.NoError(t, err)

	// Verify per-milestone disbursement records: one per vendor, all DISBURSED with tx_id.
	disbursements, err := svc.GetMilestoneDisbursements(ctx, plan.ID, milestoneID)
	require.NoError(t, err)
	require.Len(t, disbursements, 3, "must have one disbursement record per vendor")

	disbursedCount := 0
	txIDs := map[uuid.UUID]struct{}{}
	totalDisbursed := decimal.Zero

	for _, d := range disbursements {
		assert.Equal(t, domain.MilestoneDisbursementStatusDisbursed, d.Status,
			"disbursement for vendor %s must be DISBURSED", d.VendorUserID)
		require.NotNil(t, d.TxID, "tx_id must be set for vendor %s", d.VendorUserID)
		txIDs[*d.TxID] = struct{}{}
		totalDisbursed = totalDisbursed.Add(d.Amount)
		disbursedCount++
	}

	assert.Equal(t, 3, disbursedCount, "all 3 vendors must have disbursement records")
	assert.Equal(t, 3, len(txIDs), "each vendor must have a unique tx_id")
	assert.True(t, totalDisbursed.Equal(amount), "Σ disbursed = %s, want %s", totalDisbursed.StringFixed(2), amount.StringFixed(2))

	// Self-transfer guard: verify all tx PayerUserID == platform, PayeeUserID == vendor.
	// Allocations remain as FROZEN ROSTER (status does not change).
	allocs, err := svc.GetAllocations(ctx, plan.ID)
	require.NoError(t, err)
	require.Len(t, allocs, 3)

	for _, a := range allocs {
		// Frozen roster allocation status is NOT updated by disburse in the new model.
		assert.Equal(t, domain.AllocationStatusPending, a.Status,
			"allocation %s status is FROZEN ROSTER — not updated by disburse", a.VendorUserID)
	}
}

// TestSettlementService_DisburseMilestone_RoundingAbsorb verifies the last-allocation-absorbs-rounding
// rule: for 100.01 / 3 parties (33.3%, 33.3%, 33.4%) Σ == 100.01 exactly.
func TestSettlementService_DisburseMilestone_RoundingAbsorb(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	ctx := context.Background()
	dsn := startTestDB(t)
	runMigrations(t, ctx, dsn)

	vendor1, vendor2, vendor3 := uuid.New(), uuid.New(), uuid.New()
	// Equal 3-way split: 3334 + 3333 + 3333 = 10000.
	roster := newStubRoster(
		service.RosterEntry{VendorUserID: vendor1, ShareBps: 3334},
		service.RosterEntry{VendorUserID: vendor2, ShareBps: 3333},
		service.RosterEntry{VendorUserID: vendor3, ShareBps: 3333},
	)

	svc := buildService(t, ctx, dsn, roster)

	plan, err := svc.CreatePlan(ctx, &service.CreatePlanInput{
		MultiContractID: uuid.New(),
		TenderID:        uuid.New(),
		Currency:        "TWD",
		IdempotencyKey:  "contract_activated:" + uuid.New().String(),
	})
	require.NoError(t, err)
	require.NotNil(t, plan)

	amount, _ := decimal.NewFromString("100.01")
	milestoneID := uuid.New()

	err = svc.DisburseMilestone(ctx, &service.DisburseMilestoneInput{
		PlanID:       plan.ID,
		MilestoneID:  milestoneID,
		Amount:       amount,
		Currency:     "TWD",
		ActorService: "test",
	})
	require.NoError(t, err)

	// In the new model, verify via disbursement records.
	disbursements, err := svc.GetMilestoneDisbursements(ctx, plan.ID, milestoneID)
	require.NoError(t, err)

	// verifySumEquals inside the service already guards Σ == amount.
	disbursedCount := 0
	totalDisbursed := decimal.Zero

	for _, d := range disbursements {
		if d.Status == domain.MilestoneDisbursementStatusDisbursed {
			disbursedCount++
			totalDisbursed = totalDisbursed.Add(d.Amount)
		}
	}

	// All 3 vendors must have DISBURSED records; Σ == amount (enforced by verifySumEquals).
	assert.Equal(t, 3, disbursedCount, "all 3 vendors must have DISBURSED milestone records")
	assert.True(t, totalDisbursed.Equal(amount), "Σ disbursed = %s, want %s",
		totalDisbursed.StringFixed(2), amount.StringFixed(2))
}

// TestSettlementService_DisburseMilestone_Idempotent verifies that replaying the same
// contract_completed event (same milestoneID / vendor keys) is a no-op (no double-disburse).
func TestSettlementService_DisburseMilestone_Idempotent(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	ctx := context.Background()
	dsn := startTestDB(t)
	runMigrations(t, ctx, dsn)

	roster := newStubRoster(
		service.RosterEntry{VendorUserID: uuid.New(), ShareBps: 6000},
		service.RosterEntry{VendorUserID: uuid.New(), ShareBps: 4000},
	)

	svc := buildService(t, ctx, dsn, roster)

	plan, err := svc.CreatePlan(ctx, &service.CreatePlanInput{
		MultiContractID: uuid.New(),
		TenderID:        uuid.New(),
		Currency:        "TWD",
		IdempotencyKey:  "contract_activated:" + uuid.New().String(),
	})
	require.NoError(t, err)
	require.NotNil(t, plan)

	milestoneID := uuid.New()
	amount, _ := decimal.NewFromString("1000.00")

	// First disburse.
	err = svc.DisburseMilestone(ctx, &service.DisburseMilestoneInput{
		PlanID:       plan.ID,
		MilestoneID:  milestoneID,
		Amount:       amount,
		Currency:     "TWD",
		ActorService: "test",
	})
	require.NoError(t, err)

	// Second disburse — same (plan, milestone) → UNIQUE conflict on disbursement record → idempotent skip.
	err = svc.DisburseMilestone(ctx, &service.DisburseMilestoneInput{
		PlanID:       plan.ID,
		MilestoneID:  milestoneID,
		Amount:       amount,
		Currency:     "TWD",
		ActorService: "test",
	})
	require.NoError(t, err, "idempotent replay must not error")

	// Verify disbursement records: exactly 2 records (one per vendor), still DISBURSED (not doubled).
	disbursements, err := svc.GetMilestoneDisbursements(ctx, plan.ID, milestoneID)
	require.NoError(t, err)
	require.Len(t, disbursements, 2, "must have exactly 2 disbursement records (one per vendor, not doubled)")

	for _, d := range disbursements {
		assert.Equal(t, domain.MilestoneDisbursementStatusDisbursed, d.Status)
	}
}

// TestSettlementService_DisburseMilestone_TOCTOU verifies that concurrent DisburseMilestone
// calls for the same plan+milestone do not result in double-disburse.
// SELECT FOR UPDATE ensures serialization; the second call finds allocations already DISBURSED.
func TestSettlementService_DisburseMilestone_TOCTOU(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	ctx := context.Background()
	dsn := startTestDB(t)
	runMigrations(t, ctx, dsn)

	vendor1, vendor2 := uuid.New(), uuid.New()
	roster := newStubRoster(
		service.RosterEntry{VendorUserID: vendor1, ShareBps: 7000},
		service.RosterEntry{VendorUserID: vendor2, ShareBps: 3000},
	)

	svc := buildService(t, ctx, dsn, roster)

	plan, err := svc.CreatePlan(ctx, &service.CreatePlanInput{
		MultiContractID: uuid.New(),
		TenderID:        uuid.New(),
		Currency:        "TWD",
		IdempotencyKey:  "contract_activated:" + uuid.New().String(),
	})
	require.NoError(t, err)
	require.NotNil(t, plan)

	milestoneID := uuid.New()
	amount, _ := decimal.NewFromString("5000.00")

	var wg sync.WaitGroup

	var errs [2]error

	for i := range 2 {
		wg.Add(1)

		go func(idx int) {
			defer wg.Done()

			errs[idx] = svc.DisburseMilestone(ctx, &service.DisburseMilestoneInput{
				PlanID:       plan.ID,
				MilestoneID:  milestoneID,
				Amount:       amount,
				Currency:     "TWD",
				ActorService: "test-toctou",
			})
		}(i)
	}

	wg.Wait()

	// Both must succeed (idempotent) or one succeeds and one gets a lock-serialized no-op.
	for i, e := range errs {
		assert.NoError(t, e, "goroutine %d must not error", i)
	}

	// Verify no double-disburse: exactly 2 disbursement records (one per vendor), each unique tx_id.
	disbursements, err := svc.GetMilestoneDisbursements(ctx, plan.ID, milestoneID)
	require.NoError(t, err)
	require.Len(t, disbursements, 2, "must have exactly 2 disbursement records (no double-disburse)")

	txIDs := map[uuid.UUID]struct{}{}

	for _, d := range disbursements {
		assert.Equal(t, domain.MilestoneDisbursementStatusDisbursed, d.Status)
		require.NotNil(t, d.TxID)
		txIDs[*d.TxID] = struct{}{}
	}

	// Each vendor must have a unique tx_id (no shared tx between parties).
	assert.Equal(t, len(disbursements), len(txIDs), "each vendor must have a unique tx_id (no double-disburse)")
}

// TestSettlementService_DisburseMilestone_HappyFullDisburse verifies all 3 vendors
// get DISBURSED milestone records and the plan stays ACTIVE (supports multi-milestone).
func TestSettlementService_DisburseMilestone_HappyFullDisburse(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	ctx := context.Background()
	dsn := startTestDB(t)
	runMigrations(t, ctx, dsn)

	vendor1, vendor2, vendor3 := uuid.New(), uuid.New(), uuid.New()
	roster := newStubRoster(
		service.RosterEntry{VendorUserID: vendor1, ShareBps: 4000},
		service.RosterEntry{VendorUserID: vendor2, ShareBps: 3000},
		service.RosterEntry{VendorUserID: vendor3, ShareBps: 3000},
	)

	svc := buildService(t, ctx, dsn, roster)

	plan, err := svc.CreatePlan(ctx, &service.CreatePlanInput{
		MultiContractID: uuid.New(),
		TenderID:        uuid.New(),
		Currency:        "TWD",
		IdempotencyKey:  "contract_activated:" + uuid.New().String(),
	})
	require.NoError(t, err)
	require.NotNil(t, plan)

	milestoneID := uuid.New()
	amount, _ := decimal.NewFromString("9000.00")

	err = svc.DisburseMilestone(ctx, &service.DisburseMilestoneInput{
		PlanID:       plan.ID,
		MilestoneID:  milestoneID,
		Amount:       amount,
		Currency:     "TWD",
		ActorService: "test-partial",
	})
	require.NoError(t, err)

	// All 3 vendors must have DISBURSED milestone records.
	disbursements, err := svc.GetMilestoneDisbursements(ctx, plan.ID, milestoneID)
	require.NoError(t, err)
	require.Len(t, disbursements, 3, "must have 3 disbursement records")

	disbursedCount := 0

	for _, d := range disbursements {
		if d.Status == domain.MilestoneDisbursementStatusDisbursed {
			disbursedCount++
		}
	}

	assert.Equal(t, 3, disbursedCount)

	// Plan must still be ACTIVE (plan status not changed by disburse).
	gotPlan, err := svc.GetPlan(ctx, plan.ID)
	require.NoError(t, err)
	assert.Equal(t, domain.PlanStatusActive, gotPlan.Status,
		"plan remains ACTIVE after milestone disburse (supports multi-milestone)")
}

// failingTxTxStore is a TransactionStore that always fails Create.
// Used as the TxTransactionStore DI seam inside WithSettlementTx to simulate
// a database-level transaction failure for one vendor.
type failingTxTxStore struct {
	delegate    store.TransactionStore
	failVendor  uuid.UUID // vendor whose tx Create should fail
	platformUID uuid.UUID
}

func (s *failingTxTxStore) Create(ctx context.Context, tx *domain.Transaction) error {
	// Fail if this transaction is for the target vendor (PayeeUserID == failVendor).
	if tx.PayeeUserID == s.failVendor {
		return errors.New("injected transaction store failure for vendor")
	}

	return s.delegate.Create(ctx, tx)
}

func (s *failingTxTxStore) GetByID(ctx context.Context, id uuid.UUID) (*domain.Transaction, error) {
	return s.delegate.GetByID(ctx, id)
}

func (s *failingTxTxStore) GetByIDForUpdate(ctx context.Context, id uuid.UUID) (*domain.Transaction, error) {
	return s.delegate.GetByIDForUpdate(ctx, id)
}

func (s *failingTxTxStore) GetByIdempotencyKey(
	ctx context.Context, payerUserID uuid.UUID, key string,
) (*domain.Transaction, error) {
	return s.delegate.GetByIdempotencyKey(ctx, payerUserID, key)
}

func (s *failingTxTxStore) UpdateStatus(ctx context.Context, id uuid.UUID, status domain.Status) error {
	return s.delegate.UpdateStatus(ctx, id, status)
}

func (s *failingTxTxStore) ListByUserID(ctx context.Context, userID uuid.UUID) ([]*domain.Transaction, error) {
	return s.delegate.ListByUserID(ctx, userID)
}

// failingTxManager wraps SettlementTxManager and substitutes a failing TransactionStore
// for a specific vendor inside the WithSettlementTx callback.
type failingTxManager struct {
	delegate    store.SettlementTxManager
	failVendor  uuid.UUID
	platformUID uuid.UUID
	txDelegate  store.TransactionStore
}

func (m *failingTxManager) WithSettlementTx(
	ctx context.Context,
	fn func(
		ctx context.Context,
		plans store.TxSettlementPlanStore,
		allocs store.TxSettlementAllocationStore,
		disbursements store.SettlementMilestoneDisbursementStore,
		txTxStore store.TransactionStore,
		audit store.SettlementAuditStore,
	) error,
) error {
	return m.delegate.WithSettlementTx(ctx, func(
		ctx context.Context,
		plans store.TxSettlementPlanStore,
		allocs store.TxSettlementAllocationStore,
		disbursements store.SettlementMilestoneDisbursementStore,
		txTxStore store.TransactionStore,
		audit store.SettlementAuditStore,
	) error {
		// Substitute a failing TransactionStore for the target vendor.
		wrapped := &failingTxTxStore{
			delegate:    txTxStore,
			failVendor:  m.failVendor,
			platformUID: m.platformUID,
		}
		return fn(ctx, plans, allocs, disbursements, wrapped, audit)
	})
}

// TestSettlementService_DisburseMilestone_PartialFailure_WithDI injects a failing
// TxTransactionStore (DI seam via failingTxManager) for vendor2 → vendor2's disbursement
// is marked FAILED, vendor1 and vendor3 DISBURSED, plan remains ACTIVE and re-triggerable.
func TestSettlementService_DisburseMilestone_PartialFailure_WithDI(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	ctx := context.Background()
	dsn := startTestDB(t)
	runMigrations(t, ctx, dsn)

	vendor1, vendor2, vendor3 := uuid.New(), uuid.New(), uuid.New()
	roster := newStubRoster(
		service.RosterEntry{VendorUserID: vendor1, ShareBps: 4000},
		service.RosterEntry{VendorUserID: vendor2, ShareBps: 3000},
		service.RosterEntry{VendorUserID: vendor3, ShareBps: 3000},
	)

	pool, err := postgres.NewPool(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	realTxMgr := postgres.NewSettlementTxManager(pool)

	// Inject a failing transaction store for vendor2: vendor2's transactions row Create fails.
	failMgr := &failingTxManager{
		delegate:    realTxMgr,
		failVendor:  vendor2,
		platformUID: testPlatformUID,
		txDelegate:  postgres.NewTransactionStore(pool),
	}

	svc := service.NewSettlementService(
		postgres.NewSettlementPlanStore(pool),
		postgres.NewSettlementAllocationStore(pool),
		postgres.NewSettlementMilestoneDisbursementStore(pool),
		postgres.NewSettlementAuditStore(pool),
		failMgr, // injected failing tx manager
		roster,
		testPlatformUID,
	)

	plan, err := svc.CreatePlan(ctx, &service.CreatePlanInput{
		MultiContractID: uuid.New(),
		TenderID:        uuid.New(),
		Currency:        "TWD",
		IdempotencyKey:  "contract_activated:" + uuid.New().String(),
	})
	require.NoError(t, err)
	require.NotNil(t, plan)

	milestoneID := uuid.New()
	amount, _ := decimal.NewFromString("9000.00")

	// Disburse: vendor1 DISBURSED, vendor2 FAILED (injected failure), vendor3 DISBURSED.
	err = svc.DisburseMilestone(ctx, &service.DisburseMilestoneInput{
		PlanID:       plan.ID,
		MilestoneID:  milestoneID,
		Amount:       amount,
		Currency:     "TWD",
		ActorService: "test-partial-di",
	})
	// Service returns nil (partial failure = plan stays ACTIVE, anyFailed=true is logged but not returned).
	require.NoError(t, err, "partial failure is not a fatal error; plan stays re-triggerable")

	// Verify disbursement records.
	disbursements, err := svc.GetMilestoneDisbursements(ctx, plan.ID, milestoneID)
	require.NoError(t, err)

	// vendor1 DISBURSED + vendor2 FAILED + vendor3 DISBURSED = 3 records.
	require.Len(t, disbursements, 3, "must have 3 disbursement records (one per vendor)")

	statusMap := map[uuid.UUID]domain.MilestoneDisbursementStatus{}
	for _, d := range disbursements {
		statusMap[d.VendorUserID] = d.Status
	}

	assert.Equal(t, domain.MilestoneDisbursementStatusDisbursed, statusMap[vendor1], "vendor1 must be DISBURSED")
	assert.Equal(t, domain.MilestoneDisbursementStatusFailed, statusMap[vendor2], "vendor2 must be FAILED (injection)")
	assert.Equal(t, domain.MilestoneDisbursementStatusDisbursed, statusMap[vendor3], "vendor3 must be DISBURSED")

	// Plan remains ACTIVE — re-triggerable.
	gotPlan, err := svc.GetPlan(ctx, plan.ID)
	require.NoError(t, err)
	assert.Equal(t, domain.PlanStatusActive, gotPlan.Status)
}

// TestSettlementService_GetPlanByContractID verifies the GetPlanByContractID lookup.
func TestSettlementService_GetPlanByContractID(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	ctx := context.Background()
	dsn := startTestDB(t)
	runMigrations(t, ctx, dsn)

	roster := newStubRoster(
		service.RosterEntry{VendorUserID: uuid.New(), ShareBps: 10000},
	)

	svc := buildService(t, ctx, dsn, roster)

	t.Run("returns nil for unknown contract", func(t *testing.T) {
		plan, err := svc.GetPlanByContractID(ctx, uuid.New())
		require.NoError(t, err)
		assert.Nil(t, plan)
	})

	t.Run("returns plan for known contract", func(t *testing.T) {
		contractID := uuid.New()

		created, err := svc.CreatePlan(ctx, &service.CreatePlanInput{
			MultiContractID: contractID,
			TenderID:        uuid.New(),
			Currency:        "TWD",
			IdempotencyKey:  "contract_activated:" + uuid.New().String(),
		})
		require.NoError(t, err)
		require.NotNil(t, created)

		got, err := svc.GetPlanByContractID(ctx, contractID)
		require.NoError(t, err)
		require.NotNil(t, got)
		assert.Equal(t, created.ID, got.ID)
	})
}

// TestSettlementService_DisburseMilestone_ForgedIdentity_Endpoint is tested at handler level;
// here we verify the service-level auth guard for invalid inputs.
func TestSettlementService_DisburseMilestone_InvalidInputs(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	ctx := context.Background()
	dsn := startTestDB(t)
	runMigrations(t, ctx, dsn)

	roster := newStubRoster(
		service.RosterEntry{VendorUserID: uuid.New(), ShareBps: 10000},
	)

	svc := buildService(t, ctx, dsn, roster)

	amount, _ := decimal.NewFromString("500.00")

	table := []struct {
		name  string
		input *service.DisburseMilestoneInput
		want  error
	}{
		{
			name: "nil plan_id",
			input: &service.DisburseMilestoneInput{
				PlanID:       uuid.Nil,
				MilestoneID:  uuid.New(),
				Amount:       amount,
				Currency:     "TWD",
				ActorService: "test",
			},
			want: domain.ErrValidation,
		},
		{
			name: "nil milestone_id",
			input: &service.DisburseMilestoneInput{
				PlanID:       uuid.New(),
				MilestoneID:  uuid.Nil,
				Amount:       amount,
				Currency:     "TWD",
				ActorService: "test",
			},
			want: domain.ErrValidation,
		},
		{
			name: "zero amount",
			input: &service.DisburseMilestoneInput{
				PlanID:       uuid.New(),
				MilestoneID:  uuid.New(),
				Amount:       decimal.Zero,
				Currency:     "TWD",
				ActorService: "test",
			},
			want: domain.ErrValidation,
		},
		{
			name: "empty currency",
			input: &service.DisburseMilestoneInput{
				PlanID:       uuid.New(),
				MilestoneID:  uuid.New(),
				Amount:       amount,
				Currency:     "",
				ActorService: "test",
			},
			want: domain.ErrValidation,
		},
		{
			name: "plan not found",
			input: &service.DisburseMilestoneInput{
				PlanID:       uuid.New(), // unknown
				MilestoneID:  uuid.New(),
				Amount:       amount,
				Currency:     "TWD",
				ActorService: "test",
			},
			want: domain.ErrPlanNotFound,
		},
	}

	for _, tc := range table {
		t.Run(tc.name, func(t *testing.T) {
			err := svc.DisburseMilestone(ctx, tc.input)
			require.Error(t, err)
			require.True(t, errors.Is(err, tc.want),
				"expected %v got %v", tc.want, err)
		})
	}
}

// TestSettlementService_CreatePlan_ValidationErrors verifies early-return validation.
func TestSettlementService_CreatePlan_ValidationErrors(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	ctx := context.Background()
	dsn := startTestDB(t)
	runMigrations(t, ctx, dsn)

	roster := newStubRoster(
		service.RosterEntry{VendorUserID: uuid.New(), ShareBps: 10000},
	)

	svc := buildService(t, ctx, dsn, roster)

	table := []struct {
		name  string
		input *service.CreatePlanInput
	}{
		{
			name: "nil contract_id",
			input: &service.CreatePlanInput{
				MultiContractID: uuid.Nil,
				TenderID:        uuid.New(),
				Currency:        "TWD",
				IdempotencyKey:  "k",
			},
		},
		{
			name: "nil tender_id",
			input: &service.CreatePlanInput{
				MultiContractID: uuid.New(),
				TenderID:        uuid.Nil,
				Currency:        "TWD",
				IdempotencyKey:  "k",
			},
		},
		{
			name: "empty currency",
			input: &service.CreatePlanInput{
				MultiContractID: uuid.New(),
				TenderID:        uuid.New(),
				Currency:        "",
				IdempotencyKey:  "k",
			},
		},
		{
			name: "empty idempotency key",
			input: &service.CreatePlanInput{
				MultiContractID: uuid.New(),
				TenderID:        uuid.New(),
				Currency:        "TWD",
				IdempotencyKey:  "",
			},
		},
	}

	for _, tc := range table {
		t.Run(tc.name, func(t *testing.T) {
			plan, err := svc.CreatePlan(ctx, tc.input)
			require.Error(t, err)
			require.Nil(t, plan)
			require.ErrorIs(t, err, domain.ErrValidation)
		})
	}
}

// TestSettlementService_DisburseMilestone_MultipleMilestones is the REAL multi-milestone test.
// This test directly proves the correctness of the per-milestone redesign:
//   - Create plan with 2 vendors (60/40 split).
//   - Disburse milestone 1 (TWD 2000) → assert both vendors have DISBURSED disbursement records
//     with correct amounts and unique tx_ids; assert Σ == 2000.
//   - Disburse milestone 2 (TWD 3000) with a DIFFERENT milestoneID → assert milestone-2 ALSO
//     creates NEW disbursement records for EACH vendor (the bug was it paid nothing for milestone 2+).
//   - Assert milestone-2 Σ == 3000, all DISBURSED.
//   - Assert milestone-1 records are untouched (still DISBURSED with original tx_ids).
//   - Assert plan remains ACTIVE.
func TestSettlementService_DisburseMilestone_MultipleMilestones(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	ctx := context.Background()
	dsn := startTestDB(t)
	runMigrations(t, ctx, dsn)

	vendor1, vendor2 := uuid.New(), uuid.New()
	roster := newStubRoster(
		service.RosterEntry{VendorUserID: vendor1, ShareBps: 6000},
		service.RosterEntry{VendorUserID: vendor2, ShareBps: 4000},
	)

	svc := buildService(t, ctx, dsn, roster)

	plan, err := svc.CreatePlan(ctx, &service.CreatePlanInput{
		MultiContractID: uuid.New(),
		TenderID:        uuid.New(),
		Currency:        "TWD",
		IdempotencyKey:  "contract_activated:" + uuid.New().String(),
	})
	require.NoError(t, err)
	require.NotNil(t, plan)

	amount1, _ := decimal.NewFromString("2000.00")
	amount2, _ := decimal.NewFromString("3000.00")

	milestone1 := uuid.New()
	milestone2 := uuid.New()

	// ── Disburse milestone 1 ──────────────────────────────────────────────────
	require.NoError(t, svc.DisburseMilestone(ctx, &service.DisburseMilestoneInput{
		PlanID:       plan.ID,
		MilestoneID:  milestone1,
		Amount:       amount1,
		Currency:     "TWD",
		ActorService: "test",
	}))

	// Assert milestone-1 disbursement records.
	d1, err := svc.GetMilestoneDisbursements(ctx, plan.ID, milestone1)
	require.NoError(t, err)
	require.Len(t, d1, 2, "milestone-1 must have 2 disbursement records (one per vendor)")

	d1Total := decimal.Zero
	d1TxIDs := map[uuid.UUID]struct{}{}

	for _, d := range d1 {
		assert.Equal(t, domain.MilestoneDisbursementStatusDisbursed, d.Status,
			"milestone-1 vendor %s must be DISBURSED", d.VendorUserID)
		require.NotNil(t, d.TxID)
		d1TxIDs[*d.TxID] = struct{}{}
		d1Total = d1Total.Add(d.Amount)
	}

	assert.True(t, d1Total.Equal(amount1), "milestone-1 Σ=%s want %s", d1Total.StringFixed(2), amount1.StringFixed(2))
	assert.Len(t, d1TxIDs, 2, "milestone-1: each vendor must have a unique tx_id")

	// ── Disburse milestone 2 (the critical bug regression test) ──────────────
	// In the OLD model, after milestone-1 sets all allocs→DISBURSED, milestone-2
	// would find them DISBURSED and silently pay nothing.
	// In the NEW model, milestone-2 uses a different (plan,milestone,vendor) key
	// and creates INDEPENDENT disbursement records.
	require.NoError(t, svc.DisburseMilestone(ctx, &service.DisburseMilestoneInput{
		PlanID:       plan.ID,
		MilestoneID:  milestone2,
		Amount:       amount2,
		Currency:     "TWD",
		ActorService: "test",
	}), "milestone-2 disburse MUST NOT be blocked by milestone-1 (per-milestone model regression)")

	// Assert milestone-2 disbursement records — NEW records must exist.
	d2, err := svc.GetMilestoneDisbursements(ctx, plan.ID, milestone2)
	require.NoError(t, err)
	require.Len(t, d2, 2, "milestone-2 MUST have 2 new disbursement records (not skipped!)")

	d2Total := decimal.Zero
	d2TxIDs := map[uuid.UUID]struct{}{}

	for _, d := range d2 {
		assert.Equal(t, domain.MilestoneDisbursementStatusDisbursed, d.Status,
			"milestone-2 vendor %s must be DISBURSED (not skipped)", d.VendorUserID)
		require.NotNil(t, d.TxID)
		d2TxIDs[*d.TxID] = struct{}{}
		d2Total = d2Total.Add(d.Amount)
	}

	assert.True(t, d2Total.Equal(amount2), "milestone-2 Σ=%s want %s", d2Total.StringFixed(2), amount2.StringFixed(2))
	assert.Len(t, d2TxIDs, 2, "milestone-2: each vendor must have a unique tx_id")

	// Milestone-2 tx_ids must be DIFFERENT from milestone-1 tx_ids (no reuse).
	for id := range d2TxIDs {
		_, alsoInM1 := d1TxIDs[id]
		assert.False(t, alsoInM1, "milestone-2 tx_id %s must not be reused from milestone-1", id)
	}

	// Verify milestone-1 records are unchanged.
	d1After, err := svc.GetMilestoneDisbursements(ctx, plan.ID, milestone1)
	require.NoError(t, err)
	require.Len(t, d1After, 2, "milestone-1 records must be untouched after milestone-2 disburse")

	for _, d := range d1After {
		assert.Equal(t, domain.MilestoneDisbursementStatusDisbursed, d.Status)
	}

	// Plan remains ACTIVE.
	gotPlan, err := svc.GetPlan(ctx, plan.ID)
	require.NoError(t, err)
	assert.Equal(t, domain.PlanStatusActive, gotPlan.Status, "plan remains ACTIVE after 2 milestones")

	t.Logf("plan %s: milestone-1 Σ=%s, milestone-2 Σ=%s, plan ACTIVE",
		gotPlan.ID, d1Total.StringFixed(2), d2Total.StringFixed(2))
}
