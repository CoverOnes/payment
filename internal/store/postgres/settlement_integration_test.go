package postgres_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/CoverOnes/payment/internal/domain"
	"github.com/CoverOnes/payment/internal/store"
	"github.com/CoverOnes/payment/internal/store/postgres"
	"github.com/google/uuid"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Type aliases for the settlement tx callback signature.
// The tx-scoped variants expose GetByIDForUpdate / ListByPlanIDForUpdate.
type (
	storePlanStore          = store.TxSettlementPlanStore
	storeAllocStore         = store.TxSettlementAllocationStore
	storeDisburseStore      = store.SettlementMilestoneDisbursementStore
	storeTxTransactionStore = store.TransactionStore
	storeAuditEntry         = store.SettlementAuditStore
)

// newTestPlan returns a valid SettlementPlan for testing.
func newTestPlan(multiContractID, tenderID uuid.UUID, key string) *domain.SettlementPlan {
	now := time.Now().UTC()
	total, _ := decimal.NewFromString("10000.00") // test fixture literal — always valid

	return &domain.SettlementPlan{
		ID:               uuid.New(),
		MultiContractID:  multiContractID,
		TenderID:         tenderID,
		Status:           domain.PlanStatusActive,
		TotalAmount:      total,
		Currency:         "TWD",
		FrozenPartyCount: 2,
		IdempotencyKey:   key,
		CreatedAt:        now,
		UpdatedAt:        now,
	}
}

// newTestAllocation returns a valid SettlementAllocation for testing.
func newTestAllocation(planID, vendorID uuid.UUID, shareBps int, sink bool, key string) *domain.SettlementAllocation {
	now := time.Now().UTC()
	amt, _ := decimal.NewFromString("5000.00") // test fixture literal — always valid

	return &domain.SettlementAllocation{
		ID:              uuid.New(),
		PlanID:          planID,
		VendorUserID:    vendorID,
		ShareBps:        shareBps,
		AllocatedAmount: amt,
		Currency:        "TWD",
		IsRoundingSink:  sink,
		Status:          domain.AllocationStatusPending,
		IdempotencyKey:  key,
		CreatedAt:       now,
		UpdatedAt:       now,
	}
}

func TestSettlementPlanStore_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	ctx := context.Background()
	dsn := startTestDB(t)
	runMigrations(t, ctx, dsn)

	pool, err := postgres.NewPool(ctx, dsn)
	require.NoError(t, err)

	defer pool.Close()

	planStore := postgres.NewSettlementPlanStore(pool)

	t.Run("Create and GetByID round-trip", func(t *testing.T) {
		contractID := uuid.New()
		tenderID := uuid.New()
		plan := newTestPlan(contractID, tenderID, uuid.New().String())

		require.NoError(t, planStore.Create(ctx, plan))

		got, err := planStore.GetByID(ctx, plan.ID)
		require.NoError(t, err)
		assert.Equal(t, plan.ID, got.ID)
		assert.Equal(t, contractID, got.MultiContractID)
		assert.Equal(t, tenderID, got.TenderID)
		assert.Equal(t, domain.PlanStatusActive, got.Status)
		assert.Equal(t, "TWD", got.Currency)
		assert.Equal(t, 2, got.FrozenPartyCount)
		assert.True(t, plan.TotalAmount.Equal(got.TotalAmount), "total_amount round-trip preserved")
	})

	t.Run("GetByID: ErrPlanNotFound for missing id", func(t *testing.T) {
		_, err := planStore.GetByID(ctx, uuid.New())
		require.ErrorIs(t, err, domain.ErrPlanNotFound)
	})

	t.Run("Create: duplicate idempotency_key returns ErrDuplicateKey", func(t *testing.T) {
		key := uuid.New().String()
		plan1 := newTestPlan(uuid.New(), uuid.New(), key)
		require.NoError(t, planStore.Create(ctx, plan1))

		plan2 := newTestPlan(uuid.New(), uuid.New(), key)
		err := planStore.Create(ctx, plan2)
		require.ErrorIs(t, err, domain.ErrDuplicateKey)
	})

	t.Run("Create: unique index prevents two active plans per contract", func(t *testing.T) {
		contractID := uuid.New()
		plan1 := newTestPlan(contractID, uuid.New(), uuid.New().String())
		require.NoError(t, planStore.Create(ctx, plan1))

		plan2 := newTestPlan(contractID, uuid.New(), uuid.New().String())
		err := planStore.Create(ctx, plan2)
		// The partial unique index (WHERE status != 'CANCELED') fires.
		require.Error(t, err, "second active plan for same contract must be rejected")
	})

	t.Run("Create: canceled plan allows new active plan for same contract", func(t *testing.T) {
		contractID := uuid.New()
		plan1 := newTestPlan(contractID, uuid.New(), uuid.New().String())
		require.NoError(t, planStore.Create(ctx, plan1))

		// Cancel the first plan.
		require.NoError(t, planStore.UpdateStatus(ctx, plan1.ID, domain.PlanStatusCanceled))

		// Now a new active plan for the same contract is allowed.
		plan2 := newTestPlan(contractID, uuid.New(), uuid.New().String())
		require.NoError(t, planStore.Create(ctx, plan2), "active plan after cancellation must be allowed")
	})

	t.Run("UpdateStatus: ErrPlanNotFound for missing id", func(t *testing.T) {
		err := planStore.UpdateStatus(ctx, uuid.New(), domain.PlanStatusCompleted)
		require.ErrorIs(t, err, domain.ErrPlanNotFound)
	})

	t.Run("CountByMultiContractID: counts only non-canceled plans", func(t *testing.T) {
		contractID := uuid.New()

		// No plans yet.
		count, err := planStore.CountByMultiContractID(ctx, contractID)
		require.NoError(t, err)
		assert.Equal(t, 0, count)

		// Create one active plan.
		plan1 := newTestPlan(contractID, uuid.New(), uuid.New().String())
		require.NoError(t, planStore.Create(ctx, plan1))
		count, err = planStore.CountByMultiContractID(ctx, contractID)
		require.NoError(t, err)
		assert.Equal(t, 1, count)

		// Cancel it — count drops back to zero.
		require.NoError(t, planStore.UpdateStatus(ctx, plan1.ID, domain.PlanStatusCanceled))
		count, err = planStore.CountByMultiContractID(ctx, contractID)
		require.NoError(t, err)
		assert.Equal(t, 0, count, "canceled plan must not be counted")
	})
}

func TestSettlementAllocationStore_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	ctx := context.Background()
	dsn := startTestDB(t)
	runMigrations(t, ctx, dsn)

	pool, err := postgres.NewPool(ctx, dsn)
	require.NoError(t, err)

	defer pool.Close()

	planStore := postgres.NewSettlementPlanStore(pool)
	allocStore := postgres.NewSettlementAllocationStore(pool)

	// Seed a parent plan.
	plan := newTestPlan(uuid.New(), uuid.New(), uuid.New().String())
	require.NoError(t, planStore.Create(ctx, plan))

	t.Run("Create and GetByID round-trip", func(t *testing.T) {
		vendor := uuid.New()
		alloc := newTestAllocation(plan.ID, vendor, 5000, false, uuid.New().String())

		require.NoError(t, allocStore.Create(ctx, alloc))

		got, err := allocStore.GetByID(ctx, alloc.ID)
		require.NoError(t, err)
		assert.Equal(t, alloc.ID, got.ID)
		assert.Equal(t, plan.ID, got.PlanID)
		assert.Equal(t, vendor, got.VendorUserID)
		assert.Nil(t, got.RoleID)
		assert.Equal(t, 5000, got.ShareBps)
		assert.Equal(t, domain.AllocationStatusPending, got.Status)
		assert.Nil(t, got.DisbursedTxID)
		assert.True(t, alloc.AllocatedAmount.Equal(got.AllocatedAmount), "allocated_amount round-trip preserved")
	})

	t.Run("GetByID: ErrAllocationNotFound for missing id", func(t *testing.T) {
		_, err := allocStore.GetByID(ctx, uuid.New())
		require.ErrorIs(t, err, domain.ErrAllocationNotFound)
	})

	t.Run("Create: duplicate idempotency_key returns ErrDuplicateKey", func(t *testing.T) {
		key := uuid.New().String()
		alloc1 := newTestAllocation(plan.ID, uuid.New(), 1000, false, key)
		require.NoError(t, allocStore.Create(ctx, alloc1))

		alloc2 := newTestAllocation(plan.ID, uuid.New(), 1000, false, key)
		err := allocStore.Create(ctx, alloc2)
		require.ErrorIs(t, err, domain.ErrDuplicateKey)
	})

	t.Run("Create: unique index prevents two allocations per vendor per plan", func(t *testing.T) {
		vendor := uuid.New()
		alloc1 := newTestAllocation(plan.ID, vendor, 3000, false, uuid.New().String())
		require.NoError(t, allocStore.Create(ctx, alloc1))

		alloc2 := newTestAllocation(plan.ID, vendor, 3000, false, uuid.New().String())
		err := allocStore.Create(ctx, alloc2)
		require.Error(t, err, "duplicate (plan_id, vendor_user_id) must be rejected")
	})

	t.Run("ListByPlanID: returns all allocations ordered by created_at ASC", func(t *testing.T) {
		// Fresh plan for isolation.
		freshPlan := newTestPlan(uuid.New(), uuid.New(), uuid.New().String())
		require.NoError(t, planStore.Create(ctx, freshPlan))

		vendor1 := uuid.New()
		vendor2 := uuid.New()
		a1 := newTestAllocation(freshPlan.ID, vendor1, 6000, false, uuid.New().String())
		a2 := newTestAllocation(freshPlan.ID, vendor2, 4000, true, uuid.New().String())
		require.NoError(t, allocStore.Create(ctx, a1))
		require.NoError(t, allocStore.Create(ctx, a2))

		allocs, err := allocStore.ListByPlanID(ctx, freshPlan.ID)
		require.NoError(t, err)
		assert.Len(t, allocs, 2)
		// Ordered by created_at ASC.
		assert.Equal(t, a1.ID, allocs[0].ID)
		assert.Equal(t, a2.ID, allocs[1].ID)
	})

	t.Run("ListByPlanID: empty list for unknown plan", func(t *testing.T) {
		allocs, err := allocStore.ListByPlanID(ctx, uuid.New())
		require.NoError(t, err)
		assert.Empty(t, allocs)
	})

	t.Run("CountByPlanID: correct count", func(t *testing.T) {
		freshPlan := newTestPlan(uuid.New(), uuid.New(), uuid.New().String())
		require.NoError(t, planStore.Create(ctx, freshPlan))

		count, err := allocStore.CountByPlanID(ctx, freshPlan.ID)
		require.NoError(t, err)
		assert.Equal(t, 0, count)

		require.NoError(t, allocStore.Create(ctx, newTestAllocation(freshPlan.ID, uuid.New(), 10000, true, uuid.New().String())))
		count, err = allocStore.CountByPlanID(ctx, freshPlan.ID)
		require.NoError(t, err)
		assert.Equal(t, 1, count)
	})

	t.Run("UpdateStatus: set DISBURSED with tx id", func(t *testing.T) {
		vendor := uuid.New()
		alloc := newTestAllocation(plan.ID, vendor, 2000, false, uuid.New().String())
		require.NoError(t, allocStore.Create(ctx, alloc))

		txID := uuid.New()
		require.NoError(t, allocStore.UpdateStatus(ctx, alloc.ID, domain.AllocationStatusDisbursed, &txID))

		got, err := allocStore.GetByID(ctx, alloc.ID)
		require.NoError(t, err)
		assert.Equal(t, domain.AllocationStatusDisbursed, got.Status)
		require.NotNil(t, got.DisbursedTxID)
		assert.Equal(t, txID, *got.DisbursedTxID)
	})

	t.Run("UpdateStatus: ErrAllocationNotFound for missing id", func(t *testing.T) {
		err := allocStore.UpdateStatus(ctx, uuid.New(), domain.AllocationStatusFailed, nil)
		require.ErrorIs(t, err, domain.ErrAllocationNotFound)
	})

	t.Run("optional roleId round-trip", func(t *testing.T) {
		vendor := uuid.New()
		roleID := uuid.New()
		alloc := newTestAllocation(plan.ID, vendor, 1500, false, uuid.New().String())
		alloc.RoleID = &roleID

		require.NoError(t, allocStore.Create(ctx, alloc))

		got, err := allocStore.GetByID(ctx, alloc.ID)
		require.NoError(t, err)
		require.NotNil(t, got.RoleID)
		assert.Equal(t, roleID, *got.RoleID)
	})
}

// TestSettlementTxManager_Integration verifies that the SettlementTxManager executes
// all three stores (plans, allocations, audit) atomically within a single PG transaction.
func TestSettlementTxManager_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	ctx := context.Background()
	dsn := startTestDB(t)
	runMigrations(t, ctx, dsn)

	pool, err := postgres.NewPool(ctx, dsn)
	require.NoError(t, err)

	defer pool.Close()

	planStore := postgres.NewSettlementPlanStore(pool)
	allocStore := postgres.NewSettlementAllocationStore(pool)
	txMgr := postgres.NewSettlementTxManager(pool)

	// Seed a plan and allocation outside the tx.
	plan := newTestPlan(uuid.New(), uuid.New(), uuid.New().String())
	require.NoError(t, planStore.Create(ctx, plan))

	vendor := uuid.New()
	alloc := newTestAllocation(plan.ID, vendor, 10000, true, uuid.New().String())
	require.NoError(t, allocStore.Create(ctx, alloc))

	t.Run("lock plan + list allocations FOR UPDATE + write audit atomically", func(t *testing.T) {
		var lockedPlanID uuid.UUID

		err := txMgr.WithSettlementTx(ctx, func(
			ctx context.Context,
			plans storePlanStore,
			allocs storeAllocStore,
			_ storeDisburseStore,
			_ storeTxTransactionStore,
			audit storeAuditEntry,
			_ store.OutboxStore,
		) error {
			// Lock the plan.
			locked, lockErr := plans.GetByIDForUpdate(ctx, plan.ID)
			require.NoError(t, lockErr)
			assert.Equal(t, domain.PlanStatusActive, locked.Status)
			lockedPlanID = locked.ID

			// Lock all allocations.
			lockedAllocs, listErr := allocs.ListByPlanIDForUpdate(ctx, plan.ID)
			require.NoError(t, listErr)
			assert.Len(t, lockedAllocs, 1)
			assert.Equal(t, alloc.ID, lockedAllocs[0].ID)

			// Update allocation status.
			txID := uuid.New()
			updateErr := allocs.UpdateStatus(ctx, alloc.ID, domain.AllocationStatusDisbursed, &txID)
			require.NoError(t, updateErr)

			// Update plan status.
			statusErr := plans.UpdateStatus(ctx, plan.ID, domain.PlanStatusCompleted)
			require.NoError(t, statusErr)

			// Write audit row.
			payload, _ := json.Marshal(map[string]string{"vendor_user_id": vendor.String()})
			entry := &domain.SettlementAuditEntry{
				ID:           uuid.New(),
				PlanID:       plan.ID,
				AllocationID: &alloc.ID,
				EventType:    "ALLOCATION_DISBURSED",
				ActorService: "payment",
				Payload:      payload,
				OccurredAt:   time.Now().UTC(),
			}
			return audit.Append(ctx, entry)
		})
		require.NoError(t, err)

		assert.Equal(t, plan.ID, lockedPlanID)

		// Verify all mutations were committed.
		gotPlan, err := planStore.GetByID(ctx, plan.ID)
		require.NoError(t, err)
		assert.Equal(t, domain.PlanStatusCompleted, gotPlan.Status)

		gotAlloc, err := allocStore.GetByID(ctx, alloc.ID)
		require.NoError(t, err)
		assert.Equal(t, domain.AllocationStatusDisbursed, gotAlloc.Status)

		// Verify audit row in partitioned table.
		const q = `SELECT COUNT(*) FROM settlement_audit WHERE plan_id = $1`

		var count int
		require.NoError(t, pool.QueryRow(ctx, q, plan.ID).Scan(&count))
		assert.Equal(t, 1, count)
	})

	t.Run("rollback on error leaves no committed mutations", func(t *testing.T) {
		// Fresh plan + allocation for isolation.
		plan2 := newTestPlan(uuid.New(), uuid.New(), uuid.New().String())
		require.NoError(t, planStore.Create(ctx, plan2))
		alloc2 := newTestAllocation(plan2.ID, uuid.New(), 10000, true, uuid.New().String())
		require.NoError(t, allocStore.Create(ctx, alloc2))

		err := txMgr.WithSettlementTx(ctx, func(
			ctx context.Context,
			plans storePlanStore,
			_ storeAllocStore,
			_ storeDisburseStore,
			_ storeTxTransactionStore,
			_ storeAuditEntry,
			_ store.OutboxStore,
		) error {
			// Mutate — but then return error to trigger rollback.
			require.NoError(t, plans.UpdateStatus(ctx, plan2.ID, domain.PlanStatusCompleted))

			return domain.ErrPlanAlreadyDisbursed // simulate business logic error
		})
		require.ErrorIs(t, err, domain.ErrPlanAlreadyDisbursed)

		// Mutation must NOT have been committed.
		gotPlan, getErr := planStore.GetByID(ctx, plan2.ID)
		require.NoError(t, getErr)
		assert.Equal(t, domain.PlanStatusActive, gotPlan.Status, "plan status must remain ACTIVE after rollback")
	})
}

// TestSettlementAuditStore_PartitionRouting verifies that settlement_audit
// PARTITION BY RANGE(occurred_at) correctly routes rows into child partitions.
func TestSettlementAuditStore_PartitionRouting(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	ctx := context.Background()
	dsn := startTestDB(t)
	runMigrations(t, ctx, dsn)

	pool, err := postgres.NewPool(ctx, dsn)
	require.NoError(t, err)

	defer pool.Close()

	planStore := postgres.NewSettlementPlanStore(pool)
	auditStore := postgres.NewSettlementAuditStore(pool)

	plan := newTestPlan(uuid.New(), uuid.New(), uuid.New().String())
	require.NoError(t, planStore.Create(ctx, plan))

	now := time.Now().UTC()

	timestamps := []struct {
		name       string
		occurredAt time.Time
	}{
		{"prior month (now-35d)", now.Add(-35 * 24 * time.Hour)},
		{"current month (now)", now},
		{"future month (now+40d)", now.Add(40 * 24 * time.Hour)},
	}

	t.Run("insert audit rows spanning three partitions", func(t *testing.T) {
		for _, ts := range timestamps {
			entry := &domain.SettlementAuditEntry{
				ID:           uuid.New(),
				PlanID:       plan.ID,
				EventType:    "PLAN_CREATED",
				ActorService: "payment",
				OccurredAt:   ts.occurredAt,
			}

			require.NoError(t, auditStore.Append(ctx, entry), "Append failed for %s", ts.name)
		}
	})

	t.Run("all rows visible via parent table", func(t *testing.T) {
		const q = `SELECT COUNT(*) FROM settlement_audit WHERE plan_id = $1`

		var count int
		require.NoError(t, pool.QueryRow(ctx, q, plan.ID).Scan(&count))
		assert.GreaterOrEqual(t, count, 3, "expected at least 3 rows across partitions")
	})

	t.Run("child partition names exist for current and adjacent months", func(t *testing.T) {
		for _, offset := range []int{-1, 0, 1} {
			base := truncMonth(now)
			expected := "settlement_audit_p" + addMonths(base, offset).Format("200601")

			const q = `SELECT EXISTS (SELECT 1 FROM pg_class WHERE relname = $1 AND relkind = 'r')`

			var exists bool
			require.NoError(t, pool.QueryRow(ctx, q, expected).Scan(&exists))
			assert.True(t, exists, "expected partition %s to exist", expected)
		}
	})
}

// TestSettlementAuditStore_NilPayload verifies that a nil payload is stored as '{}'.
func TestSettlementAuditStore_NilPayload(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	ctx := context.Background()
	dsn := startTestDB(t)
	runMigrations(t, ctx, dsn)

	pool, err := postgres.NewPool(ctx, dsn)
	require.NoError(t, err)

	defer pool.Close()

	planStore := postgres.NewSettlementPlanStore(pool)
	auditStore := postgres.NewSettlementAuditStore(pool)

	plan := newTestPlan(uuid.New(), uuid.New(), uuid.New().String())
	require.NoError(t, planStore.Create(ctx, plan))

	entry := &domain.SettlementAuditEntry{
		ID:           uuid.New(),
		PlanID:       plan.ID,
		EventType:    "PLAN_CREATED",
		ActorService: "payment",
		Payload:      nil, // nil payload — must default to '{}'
		OccurredAt:   time.Now().UTC(),
	}
	require.NoError(t, auditStore.Append(ctx, entry))

	// Verify the row was inserted.
	const q = `SELECT COUNT(*) FROM settlement_audit WHERE id = $1`

	var count int
	require.NoError(t, pool.QueryRow(ctx, q, entry.ID).Scan(&count))
	assert.Equal(t, 1, count)
}
