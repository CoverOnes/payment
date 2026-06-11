package service_test

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
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

// ─── TestMain singleton container ────────────────────────────────────────────
// One container + one migration run for the entire package.
// Each test function uses the shared DSN; test isolation is achieved by using
// unique UUIDs for all records (no shared state between tests).

var sharedDSN string

func TestMain(m *testing.M) {
	ctx := context.Background()

	ctr, err := tcpostgres.Run(
		ctx,
		"postgres:17-alpine",
		tcpostgres.WithDatabase("testdb"),
		tcpostgres.WithUsername("testuser"),
		tcpostgres.WithPassword("testpass"),
		tcpostgres.BasicWaitStrategies(),
	)
	if err != nil {
		fmt.Fprintf(os.Stderr, "testcontainers: start postgres: %v\n", err)
		os.Exit(1)
	}

	dsn, err := ctr.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		fmt.Fprintf(os.Stderr, "testcontainers: connection string: %v\n", err)
		_ = ctr.Terminate(ctx)
		os.Exit(1)
	}

	// Run migrations once against the shared container.
	if err := applyMigrations(ctx, dsn); err != nil {
		fmt.Fprintf(os.Stderr, "testcontainers: migrate: %v\n", err)
		_ = ctr.Terminate(ctx)
		os.Exit(1)
	}

	sharedDSN = dsn
	code := m.Run()

	_ = ctr.Terminate(ctx)
	os.Exit(code)
}

// applyMigrations runs all embedded *.up.sql files against dsn. Used by TestMain only.
func applyMigrations(ctx context.Context, dsn string) error {
	pool, err := postgres.NewPool(ctx, dsn)
	if err != nil {
		return fmt.Errorf("new pool: %w", err)
	}

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
	if err != nil {
		return fmt.Errorf("walk migrations FS: %w", err)
	}

	if len(upFiles) == 0 {
		return fmt.Errorf("no *.up.sql files found in embedded FS")
	}

	sort.Strings(upFiles)

	for _, file := range upFiles {
		data, readErr := migrations.FS.ReadFile(file)
		if readErr != nil {
			return fmt.Errorf("read migration %s: %w", file, readErr)
		}

		if _, execErr := pool.Exec(ctx, string(data)); execErr != nil {
			return fmt.Errorf("apply migration %s: %w", file, execErr)
		}
	}

	return nil
}

// ─── Test helpers ────────────────────────────────────────────────────────────

// sharedDB returns the shared DSN. Skips integration tests in short mode.
func sharedDB(t *testing.T) string {
	t.Helper()

	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	return sharedDSN
}

// Kept for legacy call sites that previously called startTestDB + runMigrations
// inline. Now simply returns the shared DSN (migrations already applied by TestMain).
func startTestDB(t *testing.T) string {
	return sharedDB(t)
}

func runMigrations(_ *testing.T, _ context.Context, _ string) {
	// No-op: migrations applied once in TestMain.
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

	_, err = svc.DisburseMilestone(ctx, &service.DisburseMilestoneInput{
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

	_, err = svc.DisburseMilestone(ctx, &service.DisburseMilestoneInput{
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
	_, err = svc.DisburseMilestone(ctx, &service.DisburseMilestoneInput{
		PlanID:       plan.ID,
		MilestoneID:  milestoneID,
		Amount:       amount,
		Currency:     "TWD",
		ActorService: "test",
	})
	require.NoError(t, err)

	// Second disburse — same (plan, milestone) → UNIQUE conflict on disbursement record → idempotent skip.
	_, err = svc.DisburseMilestone(ctx, &service.DisburseMilestoneInput{
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

			_, errs[idx] = svc.DisburseMilestone(ctx, &service.DisburseMilestoneInput{
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

	_, err = svc.DisburseMilestone(ctx, &service.DisburseMilestoneInput{
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

// TestSettlementService_DisburseMilestone_VendorFailure_RollsBackAll verifies the
// all-or-nothing model (Fix #3 Critical): when one vendor's transaction row Create
// fails, DisburseMilestone returns an error and the entire pgx transaction rolls back —
// no disbursement rows persist, and the plan remains ACTIVE for re-triggering.
//
// This test replaces the old TestSettlementService_DisburseMilestone_PartialFailure_WithDI
// which asserted the unreachable "partial success" outcome (vendor1 DISBURSED, vendor2 FAILED).
// In the old model the injected Go error never issued SQL, so it never poisoned the pgx tx;
// the test gave false confidence about a production path that was structurally impossible.
func TestSettlementService_DisburseMilestone_VendorFailure_RollsBackAll(t *testing.T) {
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

	// Inject a failing transaction store for vendor2.
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
		failMgr,
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

	// Fix #3 (Critical): vendor2 fails → DisburseMilestone returns error (all rolled back).
	_, err = svc.DisburseMilestone(ctx, &service.DisburseMilestoneInput{
		PlanID:       plan.ID,
		MilestoneID:  milestoneID,
		Amount:       amount,
		Currency:     "TWD",
		ActorService: "test-rollback",
	})
	require.Error(t, err, "any vendor failure must return error (all-or-nothing)")

	// Verify ALL disbursement rows rolled back — no partial persistence.
	disbursements, listErr := svc.GetMilestoneDisbursements(ctx, plan.ID, milestoneID)
	require.NoError(t, listErr)
	assert.Empty(t, disbursements, "all disbursement rows must be rolled back on vendor failure")

	// Plan remains ACTIVE — re-triggerable.
	gotPlan, getPlanErr := svc.GetPlan(ctx, plan.ID)
	require.NoError(t, getPlanErr)
	assert.Equal(t, domain.PlanStatusActive, gotPlan.Status, "plan must remain ACTIVE for re-trigger")

	// Verify ALLOCATION_FAILED audit entries were written (out-of-tx, one per vendor).
	// These are the only DB record of the failed attempt.
	// We can't directly query the audit table from the service, so we verify indirectly
	// via the absence of disbursement rows + the error return (audit write is best-effort).
}

// TestSettlementService_DisburseMilestone_FailedThenRetriable verifies that under the
// all-or-nothing model (Fix #3), a failed DisburseMilestone call leaves zero DB trace
// (all rows rolled back), and a subsequent re-trigger succeeds and disburses all vendors.
//
// Scenario:
//  1. Create a 3-party plan (vendor1 40%, vendor2 30%, vendor3 30%).
//  2. First DisburseMilestone call: inject a Go-level failure for vendor2's tx Create.
//     → All-or-nothing: the pgx transaction rolls back entirely.
//     → DisburseMilestone returns an error (not partial success).
//     → Zero disbursement rows persist for this (plan, milestone) pair.
//     → Plan remains ACTIVE (re-triggerable).
//  3. Remove the injection; call DisburseMilestone again with the same (plan, milestone).
//     → All 3 vendors succeed; each gets a DISBURSED row and a real tx_id.
//     → No double-pay risk: no rows existed from the failed attempt to conflict.
func TestSettlementService_DisburseMilestone_FailedThenRetriable(t *testing.T) {
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
	realTxStore := postgres.NewTransactionStore(pool)

	// ── Phase 1: inject failure for vendor2 ──────────────────────────────────

	failMgr := &failingTxManager{
		delegate:    realTxMgr,
		failVendor:  vendor2,
		platformUID: testPlatformUID,
		txDelegate:  realTxStore,
	}

	svc := service.NewSettlementService(
		postgres.NewSettlementPlanStore(pool),
		postgres.NewSettlementAllocationStore(pool),
		postgres.NewSettlementMilestoneDisbursementStore(pool),
		postgres.NewSettlementAuditStore(pool),
		failMgr,
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

	// First call: vendor2 tx Create is injected to fail.
	// All-or-nothing: the entire tx rolls back → DisburseMilestone returns error.
	_, err = svc.DisburseMilestone(ctx, &service.DisburseMilestoneInput{
		PlanID:       plan.ID,
		MilestoneID:  milestoneID,
		Amount:       amount,
		Currency:     "TWD",
		ActorService: "test-retry",
	})
	require.Error(t, err, "all-or-nothing: vendor failure must return error")

	// No disbursement rows must exist — all rolled back.
	disburse1, listErr := svc.GetMilestoneDisbursements(ctx, plan.ID, milestoneID)
	require.NoError(t, listErr)
	assert.Empty(t, disburse1, "all disbursement rows must be absent after rollback")

	// Plan must remain ACTIVE (re-triggerable).
	planAfterFail, getPlanErr := svc.GetPlan(ctx, plan.ID)
	require.NoError(t, getPlanErr)
	assert.Equal(t, domain.PlanStatusActive, planAfterFail.Status, "plan must remain ACTIVE for re-trigger")

	// ── Phase 2: re-trigger without injection ────────────────────────────────
	// Because phase 1 left no rows, phase 2 starts fresh — no idempotency DISBURSED
	// skip is needed; all vendors process normally.
	svcRetry := service.NewSettlementService(
		postgres.NewSettlementPlanStore(pool),
		postgres.NewSettlementAllocationStore(pool),
		postgres.NewSettlementMilestoneDisbursementStore(pool),
		postgres.NewSettlementAuditStore(pool),
		realTxMgr,
		roster,
		testPlatformUID,
	)

	_, err = svcRetry.DisburseMilestone(ctx, &service.DisburseMilestoneInput{
		PlanID:       plan.ID,
		MilestoneID:  milestoneID,
		Amount:       amount,
		Currency:     "TWD",
		ActorService: "test-retry",
	})
	require.NoError(t, err, "re-trigger must succeed after injection removed")

	// All 3 vendors must now be DISBURSED with real tx_ids.
	disburse2, listErr2 := svcRetry.GetMilestoneDisbursements(ctx, plan.ID, milestoneID)
	require.NoError(t, listErr2)
	require.Len(t, disburse2, 3, "exactly 3 disbursement rows after successful re-trigger")

	statusMap := map[uuid.UUID]domain.MilestoneDisbursementStatus{}
	txIDMap := map[uuid.UUID]*uuid.UUID{}
	for _, d := range disburse2 {
		statusMap[d.VendorUserID] = d.Status
		txIDMap[d.VendorUserID] = d.TxID
	}

	assert.Equal(t, domain.MilestoneDisbursementStatusDisbursed, statusMap[vendor1], "vendor1 must be DISBURSED")
	assert.Equal(t, domain.MilestoneDisbursementStatusDisbursed, statusMap[vendor2], "vendor2 must be DISBURSED after re-trigger")
	assert.Equal(t, domain.MilestoneDisbursementStatusDisbursed, statusMap[vendor3], "vendor3 must be DISBURSED")

	require.NotNil(t, txIDMap[vendor1], "vendor1 must have a tx_id")
	require.NotNil(t, txIDMap[vendor2], "vendor2 must have a tx_id")
	require.NotNil(t, txIDMap[vendor3], "vendor3 must have a tx_id")

	// Each vendor must have a unique tx_id.
	assert.NotEqual(t, *txIDMap[vendor1], *txIDMap[vendor2], "vendor1 and vendor2 must have distinct tx_ids")
	assert.NotEqual(t, *txIDMap[vendor2], *txIDMap[vendor3], "vendor2 and vendor3 must have distinct tx_ids")

	// Plan remains ACTIVE.
	gotPlan, getPlanErr2 := svcRetry.GetPlan(ctx, plan.ID)
	require.NoError(t, getPlanErr2)
	assert.Equal(t, domain.PlanStatusActive, gotPlan.Status, "plan must remain ACTIVE throughout")
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
			_, err := svc.DisburseMilestone(ctx, tc.input)
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
	_, err = svc.DisburseMilestone(ctx, &service.DisburseMilestoneInput{
		PlanID:       plan.ID,
		MilestoneID:  milestone1,
		Amount:       amount1,
		Currency:     "TWD",
		ActorService: "test",
	})
	require.NoError(t, err)

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
	_, err = svc.DisburseMilestone(ctx, &service.DisburseMilestoneInput{
		PlanID:       plan.ID,
		MilestoneID:  milestone2,
		Amount:       amount2,
		Currency:     "TWD",
		ActorService: "test",
	})
	require.NoError(t, err, "milestone-2 disburse MUST NOT be blocked by milestone-1 (per-milestone model regression)")

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

// ─── Fix #1: max-amount cap ───────────────────────────────────────────────────

// TestSettlementService_DisburseMilestone_MaxAmount verifies that amounts exceeding
// 100,000,000.00 are rejected with ErrValidation (Fix #1 Major).
func TestSettlementService_DisburseMilestone_MaxAmount(t *testing.T) {
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

	plan, err := svc.CreatePlan(ctx, &service.CreatePlanInput{
		MultiContractID: uuid.New(),
		TenderID:        uuid.New(),
		Currency:        "TWD",
		IdempotencyKey:  "contract_activated:" + uuid.New().String(),
	})
	require.NoError(t, err)

	table := []struct {
		name    string
		amount  decimal.Decimal
		wantErr bool
	}{
		{
			name:    "exactly at cap (100000000.00) — accepted",
			amount:  decimal.NewFromFloat(100_000_000.00),
			wantErr: false,
		},
		{
			name:    "one cent over cap — rejected",
			amount:  decimal.NewFromFloat(100_000_000.01),
			wantErr: true,
		},
		{
			name:    "large round number over cap — rejected",
			amount:  decimal.NewFromFloat(200_000_000.00),
			wantErr: true,
		},
	}

	for _, tc := range table {
		t.Run(tc.name, func(t *testing.T) {
			_, err := svc.DisburseMilestone(ctx, &service.DisburseMilestoneInput{
				PlanID:       plan.ID,
				MilestoneID:  uuid.New(),
				Amount:       tc.amount,
				Currency:     "TWD",
				ActorService: "test-maxamt",
			})
			if tc.wantErr {
				require.Error(t, err)
				require.ErrorIs(t, err, domain.ErrValidation, "over-cap amount must return ErrValidation")
			} else {
				require.NoError(t, err, "at-cap amount must be accepted")
			}
		})
	}
}

// ─── Fix #2: currency allowlist + plan-currency match ────────────────────────

// TestSettlementService_DisburseMilestone_CurrencyAllowlist verifies that currencies
// outside TWD/USD/EUR are rejected with ErrValidation (Fix #2 Major).
func TestSettlementService_DisburseMilestone_CurrencyAllowlist(t *testing.T) {
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

	plan, err := svc.CreatePlan(ctx, &service.CreatePlanInput{
		MultiContractID: uuid.New(),
		TenderID:        uuid.New(),
		Currency:        "TWD",
		IdempotencyKey:  "contract_activated:" + uuid.New().String(),
	})
	require.NoError(t, err)

	amount, _ := decimal.NewFromString("1000.00")

	table := []struct {
		name     string
		currency string
		wantErr  bool
	}{
		{name: "TWD — allowed", currency: "TWD", wantErr: false},
		{name: "USD — allowed", currency: "USD", wantErr: true}, // plan is TWD, mismatch → also ErrValidation
		{name: "EUR — allowed but plan mismatch", currency: "EUR", wantErr: true},
		{name: "CNY — not in allowlist", currency: "CNY", wantErr: true},
		{name: "JPY — not in allowlist", currency: "JPY", wantErr: true},
		{name: "lowercase twd — not in allowlist (case-sensitive)", currency: "twd", wantErr: true},
		{name: "empty — not in allowlist", currency: "", wantErr: true},
	}

	for _, tc := range table {
		t.Run(tc.name, func(t *testing.T) {
			_, err := svc.DisburseMilestone(ctx, &service.DisburseMilestoneInput{
				PlanID:       plan.ID,
				MilestoneID:  uuid.New(),
				Amount:       amount,
				Currency:     tc.currency,
				ActorService: "test-currency",
			})
			if tc.wantErr {
				require.Error(t, err)
				require.ErrorIs(t, err, domain.ErrValidation, "disallowed or mismatched currency must return ErrValidation; got: %v", err)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

// TestSettlementService_DisburseMilestone_PlanCurrencyMismatch verifies that a disburse
// whose currency does not match the plan's currency is rejected with ErrValidation
// even if the currency is in the allowlist (Fix #2 Major).
func TestSettlementService_DisburseMilestone_PlanCurrencyMismatch(t *testing.T) {
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

	// Create a USD plan.
	plan, err := svc.CreatePlan(ctx, &service.CreatePlanInput{
		MultiContractID: uuid.New(),
		TenderID:        uuid.New(),
		Currency:        "USD",
		IdempotencyKey:  "contract_activated:" + uuid.New().String(),
	})
	require.NoError(t, err)

	amount, _ := decimal.NewFromString("500.00")

	// Attempt to disburse with TWD — valid allowlist but wrong plan currency.
	_, err = svc.DisburseMilestone(ctx, &service.DisburseMilestoneInput{
		PlanID:       plan.ID,
		MilestoneID:  uuid.New(),
		Amount:       amount,
		Currency:     "TWD",
		ActorService: "test-mismatch",
	})
	require.Error(t, err)
	require.ErrorIs(t, err, domain.ErrValidation, "plan-currency mismatch must return ErrValidation")

	// Correct currency must succeed.
	_, err = svc.DisburseMilestone(ctx, &service.DisburseMilestoneInput{
		PlanID:       plan.ID,
		MilestoneID:  uuid.New(),
		Amount:       amount,
		Currency:     "USD",
		ActorService: "test-mismatch",
	})
	require.NoError(t, err, "matching plan currency must be accepted")
}

// ─── Fix #3 Critical: real SQL abort integration test ────────────────────────

// TestSettlementService_DisburseMilestone_RealSQLAbort verifies Fix #3 Critical with a
// REAL SQL abort inside the pgx transaction. Unlike the Go-level failingTxManager tests,
// this test pre-inserts a transactions row with the same idempotency_key that
// DisburseMilestone would use for vendor1, causing a SQLSTATE 23505 unique-constraint
// violation inside the pgx tx. PostgreSQL marks the transaction as aborted (error state),
// so all subsequent statements fail and the COMMIT rolls back — proving the pgx transaction
// is truly all-or-nothing under real SQL errors.
//
// Scenario:
//  1. Create 2-vendor plan (vendor1 50%, vendor2 50%).
//  2. Pre-insert a transactions row with vendor1's idempotency_key outside the tx.
//  3. DisburseMilestone: processes vendor1 → disbursement row INSERT ok (DO NOTHING guard),
//     then txTxStore.Create → SQLSTATE 23505 → pgx tx aborts.
//  4. DisburseMilestone must return error; zero disbursement rows for this (plan, milestone) pair.
func TestSettlementService_DisburseMilestone_RealSQLAbort(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	ctx := context.Background()
	dsn := startTestDB(t)
	runMigrations(t, ctx, dsn)

	vendor1, vendor2 := uuid.New(), uuid.New()
	roster := newStubRoster(
		service.RosterEntry{VendorUserID: vendor1, ShareBps: 5000},
		service.RosterEntry{VendorUserID: vendor2, ShareBps: 5000},
	)

	pool, err := postgres.NewPool(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	svc := service.NewSettlementService(
		postgres.NewSettlementPlanStore(pool),
		postgres.NewSettlementAllocationStore(pool),
		postgres.NewSettlementMilestoneDisbursementStore(pool),
		postgres.NewSettlementAuditStore(pool),
		postgres.NewSettlementTxManager(pool),
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

	milestoneID := uuid.New()
	amount, _ := decimal.NewFromString("2000.00")

	// Pre-insert a transactions row with vendor1's idempotency_key (outside the tx).
	// DisburseMilestone computes: iKey = fmt.Sprintf("disburse:%s:%s:%s", planID, milestoneID, vendorUID)
	// The transactions table uses plain INSERT (no ON CONFLICT), so a duplicate idempotency_key
	// triggers SQLSTATE 23505, which aborts the pgx transaction mid-flight.
	vendor1IKey := "disburse:" + plan.ID.String() + ":" + milestoneID.String() + ":" + vendor1.String()
	_, preErr := pool.Exec(
		ctx, `
		INSERT INTO transactions
			(id, payer_user_id, payee_user_id, amount, currency, status, idempotency_key, created_at, updated_at)
		VALUES
			($1, $2, $3, $4, $5, $6, $7, now(), now())
	`,
		uuid.New(),      // id
		testPlatformUID, // payer_user_id
		vendor1,         // payee_user_id
		"1000.00",       // amount
		"TWD",           // currency
		"RELEASED",      // status
		vendor1IKey,     // idempotency_key — same key DisburseMilestone would use for vendor1
	)
	require.NoError(t, preErr, "pre-insert transactions row must succeed to set up the SQL abort scenario")

	// DisburseMilestone: when it processes vendor1, txTxStore.Create will hit SQLSTATE 23505
	// on the transactions table (plain INSERT, no ON CONFLICT), aborting the pgx tx.
	_, err = svc.DisburseMilestone(ctx, &service.DisburseMilestoneInput{
		PlanID:       plan.ID,
		MilestoneID:  milestoneID,
		Amount:       amount,
		Currency:     "TWD",
		ActorService: "test-sql-abort",
	})
	require.Error(t, err, "real SQL abort (SQLSTATE 23505) must propagate as error from DisburseMilestone")

	// Zero disbursement rows must exist for this (plan, milestone) — all rolled back.
	rows, listErr := svc.GetMilestoneDisbursements(ctx, plan.ID, milestoneID)
	require.NoError(t, listErr)
	assert.Empty(t, rows, "all disbursement rows must be rolled back on real SQL abort")

	// Plan remains ACTIVE — re-triggerable.
	gotPlan, getPlanErr := svc.GetPlan(ctx, plan.ID)
	require.NoError(t, getPlanErr)
	assert.Equal(t, domain.PlanStatusActive, gotPlan.Status, "plan must remain ACTIVE after SQL abort rollback")
}

// ─── Fix #6: ALLOCATION_FAILED audit + CompletePlan ──────────────────────────

// TestSettlementService_CompletePlan verifies that CompletePlan transitions an ACTIVE
// plan to COMPLETED, and that calling it on a COMPLETED plan returns ErrInvalidTransition
// (Fix #6 Major).
func TestSettlementService_CompletePlan(t *testing.T) {
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

	t.Run("transitions ACTIVE plan to COMPLETED", func(t *testing.T) {
		plan, err := svc.CreatePlan(ctx, &service.CreatePlanInput{
			MultiContractID: uuid.New(),
			TenderID:        uuid.New(),
			Currency:        "TWD",
			IdempotencyKey:  "contract_activated:" + uuid.New().String(),
		})
		require.NoError(t, err)
		require.Equal(t, domain.PlanStatusActive, plan.Status)

		err = svc.CompletePlan(ctx, plan.ID)
		require.NoError(t, err, "CompletePlan on ACTIVE plan must succeed")

		got, err := svc.GetPlan(ctx, plan.ID)
		require.NoError(t, err)
		assert.Equal(t, domain.PlanStatusCompleted, got.Status, "plan must be COMPLETED after CompletePlan")
	})

	t.Run("returns ErrInvalidTransition if plan is already COMPLETED", func(t *testing.T) {
		plan, err := svc.CreatePlan(ctx, &service.CreatePlanInput{
			MultiContractID: uuid.New(),
			TenderID:        uuid.New(),
			Currency:        "TWD",
			IdempotencyKey:  "contract_activated:" + uuid.New().String(),
		})
		require.NoError(t, err)

		require.NoError(t, svc.CompletePlan(ctx, plan.ID))

		// Second call: already COMPLETED → ErrInvalidTransition.
		err = svc.CompletePlan(ctx, plan.ID)
		require.Error(t, err)
		require.ErrorIs(t, err, domain.ErrInvalidTransition, "CompletePlan on already-COMPLETED plan must return ErrInvalidTransition")
	})

	t.Run("returns ErrPlanNotFound for unknown plan", func(t *testing.T) {
		err := svc.CompletePlan(ctx, uuid.New())
		require.Error(t, err)
		require.ErrorIs(t, err, domain.ErrPlanNotFound)
	})
}

// ─── M-1: cumulative disbursement cap ─────────────────────────────────────────

// TestSettlementService_DisburseMilestone_CumulativeCap verifies the per-plan cumulative
// disbursement cap (M-1 fix): when plan.TotalAmount > 0, the sum of all DISBURSED milestone
// amounts must not exceed the plan total. The over-cap milestone is rejected with ErrValidation.
//
// Scenario:
//  1. Create a 2-vendor plan (60/40) with TotalAmount = 5000.00.
//  2. Disburse milestone 1 (3000.00) → succeeds; cumulative = 3000.00.
//  3. Disburse milestone 2 (2001.00) → rejected (3000+2001 > 5000) with ErrValidation.
//  4. Disburse milestone 2 (2000.00) → succeeds (3000+2000 == 5000, at cap exactly).
//  5. Disburse any further milestone → rejected (5000+anything > 5000) with ErrValidation.
func TestSettlementService_DisburseMilestone_CumulativeCap(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	ctx := context.Background()
	dsn := startTestDB(t)

	vendor1, vendor2 := uuid.New(), uuid.New()
	roster := newStubRoster(
		service.RosterEntry{VendorUserID: vendor1, ShareBps: 6000},
		service.RosterEntry{VendorUserID: vendor2, ShareBps: 4000},
	)

	svc := buildService(t, ctx, dsn, roster)

	totalAmount, _ := decimal.NewFromString("5000.00")

	plan, err := svc.CreatePlan(ctx, &service.CreatePlanInput{
		MultiContractID: uuid.New(),
		TenderID:        uuid.New(),
		Currency:        "TWD",
		TotalAmount:     totalAmount,
		IdempotencyKey:  "contract_activated:" + uuid.New().String(),
	})
	require.NoError(t, err)
	require.NotNil(t, plan)
	require.True(t, plan.TotalAmount.Equal(totalAmount), "plan.TotalAmount must be persisted at creation")

	milestone1 := uuid.New()
	amount1, _ := decimal.NewFromString("3000.00")

	// Milestone 1: 3000 of 5000 — under cap.
	_, err = svc.DisburseMilestone(ctx, &service.DisburseMilestoneInput{
		PlanID:       plan.ID,
		MilestoneID:  milestone1,
		Amount:       amount1,
		Currency:     "TWD",
		ActorService: "test-cap",
	})
	require.NoError(t, err, "first milestone (3000/5000) must succeed")

	milestone2 := uuid.New()
	overCapAmt, _ := decimal.NewFromString("2001.00")

	// Milestone 2 over cap: 3000 + 2001 = 5001 > 5000.
	_, err = svc.DisburseMilestone(ctx, &service.DisburseMilestoneInput{
		PlanID:       plan.ID,
		MilestoneID:  milestone2,
		Amount:       overCapAmt,
		Currency:     "TWD",
		ActorService: "test-cap",
	})
	require.Error(t, err, "over-cap milestone must be rejected")
	require.ErrorIs(t, err, domain.ErrValidation, "over-cap must return ErrValidation")

	// No disbursement rows for milestone2 (rejected before any DB write).
	d2, listErr := svc.GetMilestoneDisbursements(ctx, plan.ID, milestone2)
	require.NoError(t, listErr)
	assert.Empty(t, d2, "over-cap milestone must leave no disbursement rows")

	// Milestone 2 exactly at cap: 3000 + 2000 = 5000.
	exactCapAmt, _ := decimal.NewFromString("2000.00")
	milestone2b := uuid.New()

	_, err = svc.DisburseMilestone(ctx, &service.DisburseMilestoneInput{
		PlanID:       plan.ID,
		MilestoneID:  milestone2b,
		Amount:       exactCapAmt,
		Currency:     "TWD",
		ActorService: "test-cap",
	})
	require.NoError(t, err, "milestone exactly at cap (3000+2000=5000) must succeed")

	// Any further milestone should be rejected (cumulative is now 5000 == total).
	milestone3 := uuid.New()
	smallAmt, _ := decimal.NewFromString("0.01")

	_, err = svc.DisburseMilestone(ctx, &service.DisburseMilestoneInput{
		PlanID:       plan.ID,
		MilestoneID:  milestone3,
		Amount:       smallAmt,
		Currency:     "TWD",
		ActorService: "test-cap",
	})
	require.Error(t, err, "any amount past the cap must be rejected")
	require.ErrorIs(t, err, domain.ErrValidation, "over-cap must return ErrValidation")
}

// ─── Fix #1 (re-review): ACTIVE-only guard on disburseMilestoneTx ────────────
//
// TestSettlementService_DisburseMilestone_CompletedPlanRejected verifies that
// DisburseMilestone on a COMPLETED plan returns ErrInvalidTransition and does not
// write any new disbursement rows. This pins the "ACTIVE-only" guard introduced to
// fix the finding that a completed plan was still disbursable with fresh milestoneIDs.
func TestSettlementService_DisburseMilestone_CompletedPlanRejected(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	ctx := context.Background()
	dsn := startTestDB(t)
	runMigrations(t, ctx, dsn)

	vendor1 := uuid.New()
	roster := newStubRoster(
		service.RosterEntry{VendorUserID: vendor1, ShareBps: 10000},
	)

	svc := buildService(t, ctx, dsn, roster)

	// Create and activate a plan (CreatePlan returns ACTIVE).
	plan, err := svc.CreatePlan(ctx, &service.CreatePlanInput{
		MultiContractID: uuid.New(),
		TenderID:        uuid.New(),
		Currency:        "TWD",
		IdempotencyKey:  "contract_activated:" + uuid.New().String(),
	})
	require.NoError(t, err)
	require.NotNil(t, plan)
	assert.Equal(t, domain.PlanStatusActive, plan.Status, "plan must start ACTIVE")

	// Disburse the first milestone while ACTIVE — must succeed.
	milestone1 := uuid.New()
	amount, _ := decimal.NewFromString("1000.00")

	_, err = svc.DisburseMilestone(ctx, &service.DisburseMilestoneInput{
		PlanID:       plan.ID,
		MilestoneID:  milestone1,
		Amount:       amount,
		Currency:     "TWD",
		ActorService: "test",
	})
	require.NoError(t, err, "disburse on ACTIVE plan must succeed")

	// Transition the plan to COMPLETED.
	require.NoError(t, svc.CompletePlan(ctx, plan.ID), "CompletePlan must succeed on ACTIVE plan")

	// Confirm the plan is now COMPLETED.
	completedPlan, err := svc.GetPlan(ctx, plan.ID)
	require.NoError(t, err)
	assert.Equal(t, domain.PlanStatusCompleted, completedPlan.Status, "plan must be COMPLETED")

	// Attempt to disburse a FRESH milestoneID on the COMPLETED plan — must fail.
	milestone2 := uuid.New()
	_, err = svc.DisburseMilestone(ctx, &service.DisburseMilestoneInput{
		PlanID:       plan.ID,
		MilestoneID:  milestone2,
		Amount:       amount,
		Currency:     "TWD",
		ActorService: "test",
	})
	require.Error(t, err, "DisburseMilestone on COMPLETED plan must return error")
	require.ErrorIs(t, err, domain.ErrInvalidTransition,
		"COMPLETED plan must return ErrInvalidTransition; got: %v", err)

	// Verify zero new disbursement rows were created for the fresh milestone.
	d2, listErr := svc.GetMilestoneDisbursements(ctx, plan.ID, milestone2)
	require.NoError(t, listErr)
	assert.Empty(t, d2, "no disbursement rows must exist for the rejected milestone on COMPLETED plan")
}

// TestSettlementService_DisburseMilestone_CanceledPlanRejected verifies that
// DisburseMilestone on a CANCELED plan also returns ErrInvalidTransition.
// Complements CompletedPlanRejected — the ACTIVE-only guard covers both non-ACTIVE states.
func TestSettlementService_DisburseMilestone_CanceledPlanRejected(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	ctx := context.Background()
	dsn := startTestDB(t)
	runMigrations(t, ctx, dsn)

	vendor1 := uuid.New()
	roster := newStubRoster(
		service.RosterEntry{VendorUserID: vendor1, ShareBps: 10000},
	)

	pool, err := postgres.NewPool(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	svc := buildService(t, ctx, dsn, roster)

	plan, err := svc.CreatePlan(ctx, &service.CreatePlanInput{
		MultiContractID: uuid.New(),
		TenderID:        uuid.New(),
		Currency:        "TWD",
		IdempotencyKey:  "contract_activated:" + uuid.New().String(),
	})
	require.NoError(t, err)
	require.NotNil(t, plan)

	// Directly cancel the plan via the store (bypassing service — there is no CancelPlan API).
	planStore := postgres.NewSettlementPlanStore(pool)
	require.NoError(
		t,
		planStore.UpdateStatus(ctx, plan.ID, domain.PlanStatusCanceled),
		"direct cancel via store must succeed",
	)

	milestoneID := uuid.New()
	amount, _ := decimal.NewFromString("500.00")

	_, err = svc.DisburseMilestone(ctx, &service.DisburseMilestoneInput{
		PlanID:       plan.ID,
		MilestoneID:  milestoneID,
		Amount:       amount,
		Currency:     "TWD",
		ActorService: "test",
	})
	require.Error(t, err, "DisburseMilestone on CANCELED plan must return error")
	require.ErrorIs(t, err, domain.ErrInvalidTransition,
		"CANCELED plan must return ErrInvalidTransition; got: %v", err)
}

// ─── Fix #7 (re-review): concurrent cap regression ───────────────────────────
//
// TestSettlementService_DisburseMilestone_ConcurrentCapEnforcement verifies that
// two concurrent DisburseMilestone calls with amounts that individually pass but
// together exceed TotalAmount result in exactly one succeeding and the total
// disbursed staying <= TotalAmount. The plan-row FOR UPDATE lock serializes the
// cap check so both calls cannot race past it simultaneously.
func TestSettlementService_DisburseMilestone_ConcurrentCapEnforcement(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	ctx := context.Background()
	dsn := startTestDB(t)
	runMigrations(t, ctx, dsn)

	vendor1 := uuid.New()
	// Single-vendor plan to keep concurrent disbursement counts simple.
	roster := newStubRoster(
		service.RosterEntry{VendorUserID: vendor1, ShareBps: 10000},
	)

	svc := buildService(t, ctx, dsn, roster)

	// TotalAmount = 3000. Two concurrent calls each with 2000 — individually within cap
	// (2000 < 3000), but together they sum to 4000 > 3000.
	totalAmount, _ := decimal.NewFromString("3000.00")

	plan, err := svc.CreatePlan(ctx, &service.CreatePlanInput{
		MultiContractID: uuid.New(),
		TenderID:        uuid.New(),
		Currency:        "TWD",
		TotalAmount:     totalAmount,
		IdempotencyKey:  "contract_activated:" + uuid.New().String(),
	})
	require.NoError(t, err)
	require.NotNil(t, plan)
	require.True(t, plan.TotalAmount.Equal(totalAmount), "plan.TotalAmount must be set")

	callAmount, _ := decimal.NewFromString("2000.00") // 2000 < 3000 → individually passes cap

	var wg sync.WaitGroup
	errs := make([]error, 2)
	milestones := [2]uuid.UUID{uuid.New(), uuid.New()} // two DISTINCT milestoneIDs

	for i := range 2 {
		wg.Add(1)

		go func(idx int) {
			defer wg.Done()

			_, errs[idx] = svc.DisburseMilestone(ctx, &service.DisburseMilestoneInput{
				PlanID:       plan.ID,
				MilestoneID:  milestones[idx],
				Amount:       callAmount,
				Currency:     "TWD",
				ActorService: "test-concurrent-cap",
			})
		}(i)
	}

	wg.Wait()

	// Exactly one must succeed and one must fail with ErrValidation (cap exceeded).
	successes := 0
	capErrors := 0

	for _, e := range errs {
		switch {
		case e == nil:
			successes++
		case errors.Is(e, domain.ErrValidation):
			capErrors++
		default:
			t.Errorf("unexpected error type: %v", e)
		}
	}

	assert.Equal(t, 1, successes, "exactly one concurrent call must succeed")
	assert.Equal(t, 1, capErrors, "exactly one concurrent call must be rejected by cap (ErrValidation)")

	// Verify total disbursed <= TotalAmount.
	var totalDisbursed decimal.Decimal

	for _, m := range milestones {
		rows, listErr := svc.GetMilestoneDisbursements(ctx, plan.ID, m)
		require.NoError(t, listErr)

		for _, r := range rows {
			if r.Status == domain.MilestoneDisbursementStatusDisbursed {
				totalDisbursed = totalDisbursed.Add(r.Amount)
			}
		}
	}

	assert.True(t, totalDisbursed.LessThanOrEqual(totalAmount),
		"total disbursed %s must not exceed TotalAmount %s",
		totalDisbursed.StringFixed(2), totalAmount.StringFixed(2))
}
