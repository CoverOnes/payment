package outbox

// Integration tests for the outbox Poller's dispatch path.
// These tests exercise the event-replay acceptance requirement:
// dispatching the same payment.contract_completed event TWICE via
// p.dispatch() must result in exactly N disbursement rows (one per vendor
// allocation) — not 2N — proving the end-to-end idempotency guarantee.
//
// Container: a single testcontainers Postgres instance is shared across all
// tests in this package (started by TestMain). Migrations are applied once.

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/CoverOnes/payment/internal/domain"
	"github.com/CoverOnes/payment/internal/service"
	"github.com/CoverOnes/payment/internal/store/postgres"
	migrations "github.com/CoverOnes/payment/migrations"
	"github.com/google/uuid"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
)

// ─── TestMain singleton container ────────────────────────────────────────────

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

// ─── helpers ─────────────────────────────────────────────────────────────────

var testPlatformUID = uuid.MustParse("00000000-0000-0000-0000-000000000002")

// stubRoster is a minimal WorkspaceRosterClient for outbox-level tests.
type stubRoster struct {
	entries []service.RosterEntry
}

func (s *stubRoster) GetPartyRoster(_ context.Context, _ uuid.UUID) ([]service.RosterEntry, error) {
	cp := make([]service.RosterEntry, len(s.entries))
	copy(cp, s.entries)

	return cp, nil
}

func (s *stubRoster) GetMilestoneAmountsSum(_ context.Context, _ uuid.UUID) (decimal.Decimal, error) {
	return decimal.Zero, nil // uncapped for these tests
}

func buildOutboxStore(t *testing.T, ctx context.Context) *postgres.OutboxStore {
	t.Helper()

	pool, err := postgres.NewPool(ctx, sharedDSN)
	require.NoError(t, err)

	t.Cleanup(pool.Close)

	return postgres.NewOutboxStore(pool)
}

// ─── F1: Event-replay integration test ───────────────────────────────────────

// TestPoller_EventReplay_ExactlyOneDisbursePerAllocation is the acceptance test
// for the payment recovery outbox (GTD #15 / PR #16).
//
// It exercises the end-to-end event-replay path via p.dispatch() — the same code
// path the Poller uses on every poll cycle — and asserts that dispatching the same
// payment.contract_completed event TWICE results in exactly N disbursement rows
// (one per vendor allocation), not 2N.
//
// This covers the scenario that existing service-layer tests do NOT cover:
// the poller calling DisburseMilestone via the outbox event path, not directly.
func TestPoller_EventReplay_ExactlyOneDisbursePerAllocation(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	ctx := context.Background()
	outboxStore := buildOutboxStore(t, ctx)

	// Step 1: Create a 2-vendor plan (60/40 split).
	vendor1, vendor2 := uuid.New(), uuid.New()

	pool, err := postgres.NewPool(ctx, sharedDSN)
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	roster := &stubRoster{
		entries: []service.RosterEntry{
			{VendorUserID: vendor1, ShareBps: 6000},
			{VendorUserID: vendor2, ShareBps: 4000},
		},
	}

	svcWithRoster := service.NewSettlementService(
		postgres.NewSettlementPlanStore(pool),
		postgres.NewSettlementAllocationStore(pool),
		postgres.NewSettlementMilestoneDisbursementStore(pool),
		postgres.NewSettlementAuditStore(pool),
		postgres.NewSettlementTxManager(pool),
		roster,
		testPlatformUID,
	)

	plan, err := svcWithRoster.CreatePlan(ctx, &service.CreatePlanInput{
		MultiContractID: uuid.New(),
		TenderID:        uuid.New(),
		Currency:        "TWD",
		IdempotencyKey:  "contract_activated:" + uuid.New().String(),
	})
	require.NoError(t, err)
	require.NotNil(t, plan)

	milestoneID := uuid.New()
	amount, _ := decimal.NewFromString("1000.00")

	// Step 2: Build a contract_completed outbox event payload (same payload used twice).
	evtPayload, err := json.Marshal(map[string]any{
		"plan_id":      plan.ID,
		"milestone_id": milestoneID,
		"amount":       amount.StringFixed(2),
		"currency":     "TWD",
		"actor":        "test-event-replay",
	})
	require.NoError(t, err)

	now := time.Now().UTC()

	// Step 3: Build the outbox event. Use a fixed EventID so both dispatches
	// represent the same logical business event (same event delivered twice).
	fixedEventID := uuid.New()

	evt := &domain.OutboxEvent{
		ID:            uuid.New(),
		AggregateType: "settlement_plan",
		AggregateID:   plan.ID,
		EventID:       fixedEventID,
		Channel:       channelContractCompleted,
		Payload:       evtPayload,
		CreatedAt:     now,
		NextAttemptAt: now,
	}

	// Step 4: Build the Poller using the real SettlementService on testcontainers PG.
	poller := NewPoller(outboxStore, svcWithRoster, time.Second)

	// Step 5: Dispatch the same event TWICE via the Poller's dispatch path.
	// First dispatch: should process and disburse to all N vendors.
	err = poller.dispatch(ctx, evt)
	require.NoError(t, err, "first dispatch of contract_completed must succeed")

	// Second dispatch (replay): same event — DisburseMilestone is idempotent.
	// Must not error and must not create additional disbursement rows.
	err = poller.dispatch(ctx, evt)
	require.NoError(t, err, "second dispatch (replay) of same contract_completed must succeed (idempotent)")

	// Step 6: Assert exactly N rows — one per vendor allocation, NOT doubled.
	disbursements, err := svcWithRoster.GetMilestoneDisbursements(ctx, plan.ID, milestoneID)
	require.NoError(t, err)
	require.Len(t, disbursements, 2,
		"exactly 2 disbursement rows expected (one per vendor allocation), not 4 (double-disburse)")

	for _, d := range disbursements {
		assert.Equal(t, domain.MilestoneDisbursementStatusDisbursed, d.Status,
			"vendor %s must be DISBURSED", d.VendorUserID)
		require.NotNil(t, d.TxID, "vendor %s must have a tx_id", d.VendorUserID)
	}

	// Step 7: Assert sum of disbursed amounts equals the original amount.
	totalDisbursed := decimal.Zero
	for _, d := range disbursements {
		totalDisbursed = totalDisbursed.Add(d.Amount)
	}

	assert.True(t, totalDisbursed.Equal(amount),
		"Σ disbursed %s must equal milestone amount %s",
		totalDisbursed.StringFixed(2), amount.StringFixed(2))
}

// TestPoller_Dispatch_UnknownChannelReturnsError verifies F4:
// an unknown channel returns an error (causing MarkFailed) instead of nil
// (which would cause MarkPublished and silently swallow mis-routed events).
func TestPoller_Dispatch_UnknownChannelReturnsError(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	ctx := context.Background()
	outboxStore := buildOutboxStore(t, ctx)

	pool, err := postgres.NewPool(ctx, sharedDSN)
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	roster := &stubRoster{
		entries: []service.RosterEntry{
			{VendorUserID: uuid.New(), ShareBps: 10000},
		},
	}

	svc := service.NewSettlementService(
		postgres.NewSettlementPlanStore(pool),
		postgres.NewSettlementAllocationStore(pool),
		postgres.NewSettlementMilestoneDisbursementStore(pool),
		postgres.NewSettlementAuditStore(pool),
		postgres.NewSettlementTxManager(pool),
		roster,
		testPlatformUID,
	)

	poller := NewPoller(outboxStore, svc, time.Second)

	now := time.Now().UTC()
	evt := &domain.OutboxEvent{
		ID:            uuid.New(),
		AggregateType: "settlement_plan",
		AggregateID:   uuid.New(),
		EventID:       uuid.New(),
		Channel:       "payment.unknown_channel",
		Payload:       []byte(`{}`),
		CreatedAt:     now,
		NextAttemptAt: now,
	}

	err = poller.dispatch(ctx, evt)
	require.Error(t, err, "unknown channel must return error so MarkFailed is called (not MarkPublished)")
	assert.Contains(t, err.Error(), "unknown outbox channel",
		"error message must identify the unknown channel")
}
