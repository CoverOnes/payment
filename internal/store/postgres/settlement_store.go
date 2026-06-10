package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/CoverOnes/payment/internal/domain"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"
)

// -----------------------------------------------------------------------
// SettlementPlanStore — pool-backed
// -----------------------------------------------------------------------

// SettlementPlanStore is a pool-backed store for settlement plans.
type SettlementPlanStore struct {
	q querier
}

// NewSettlementPlanStore returns a SettlementPlanStore backed by pool.
func NewSettlementPlanStore(pool *pgxpool.Pool) *SettlementPlanStore {
	return &SettlementPlanStore{q: pool}
}

// txSettlementPlanStore is a transaction-scoped store.
type txSettlementPlanStore struct {
	tx querier
}

func (s *txSettlementPlanStore) Create(ctx context.Context, plan *domain.SettlementPlan) error {
	return settlementPlanCreate(ctx, s.tx, plan)
}

func (s *txSettlementPlanStore) GetByID(ctx context.Context, id uuid.UUID) (*domain.SettlementPlan, error) {
	return settlementPlanGetByID(ctx, s.tx, id)
}

func (s *txSettlementPlanStore) GetByIDForUpdate(ctx context.Context, id uuid.UUID) (*domain.SettlementPlan, error) {
	return settlementPlanGetByIDForUpdate(ctx, s.tx, id)
}

func (s *txSettlementPlanStore) UpdateStatus(ctx context.Context, id uuid.UUID, status domain.PlanStatus) error {
	return settlementPlanUpdateStatus(ctx, s.tx, id, status)
}

func (s *txSettlementPlanStore) CompletePlanAtomic(ctx context.Context, id uuid.UUID) error {
	return settlementPlanCompleteAtomic(ctx, s.tx, id)
}

func (s *txSettlementPlanStore) CountByMultiContractID(ctx context.Context, multiContractID uuid.UUID) (int, error) {
	return settlementPlanCountByMultiContractID(ctx, s.tx, multiContractID)
}

func (s *txSettlementPlanStore) GetByMultiContractID(ctx context.Context, multiContractID uuid.UUID) (*domain.SettlementPlan, error) {
	return settlementPlanGetByMultiContractID(ctx, s.tx, multiContractID)
}

func (s *txSettlementPlanStore) GetByMultiContractIDForUpdate(ctx context.Context, multiContractID uuid.UUID) (*domain.SettlementPlan, error) {
	return settlementPlanGetByMultiContractIDForUpdate(ctx, s.tx, multiContractID)
}

// Pool-backed methods.

// Create inserts a new settlement plan.
func (s *SettlementPlanStore) Create(ctx context.Context, plan *domain.SettlementPlan) error {
	return settlementPlanCreate(ctx, s.q, plan)
}

// GetByID fetches a plan by primary key.
func (s *SettlementPlanStore) GetByID(ctx context.Context, id uuid.UUID) (*domain.SettlementPlan, error) {
	return settlementPlanGetByID(ctx, s.q, id)
}

// UpdateStatus updates the status and updated_at of a plan.
func (s *SettlementPlanStore) UpdateStatus(ctx context.Context, id uuid.UUID, status domain.PlanStatus) error {
	return settlementPlanUpdateStatus(ctx, s.q, id, status)
}

// CompletePlanAtomic atomically transitions a plan from ACTIVE to COMPLETED.
// Returns ErrInvalidTransition if the plan exists but is not ACTIVE.
// Returns ErrPlanNotFound if no plan with that ID exists.
func (s *SettlementPlanStore) CompletePlanAtomic(ctx context.Context, id uuid.UUID) error {
	return settlementPlanCompleteAtomic(ctx, s.q, id)
}

// CountByMultiContractID returns the number of non-canceled plans for a contract.
func (s *SettlementPlanStore) CountByMultiContractID(ctx context.Context, multiContractID uuid.UUID) (int, error) {
	return settlementPlanCountByMultiContractID(ctx, s.q, multiContractID)
}

// GetByMultiContractID returns the active (non-canceled) settlement plan for a contract.
// Returns nil, nil if no plan exists.
func (s *SettlementPlanStore) GetByMultiContractID(ctx context.Context, multiContractID uuid.UUID) (*domain.SettlementPlan, error) {
	return settlementPlanGetByMultiContractID(ctx, s.q, multiContractID)
}

// --- helpers ---

func settlementPlanCreate(ctx context.Context, q querier, plan *domain.SettlementPlan) error {
	const query = `
INSERT INTO settlement_plans
    (id, multi_contract_id, tender_id, status, total_amount, currency, frozen_party_count, idempotency_key, created_at, updated_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
`

	_, err := q.Exec(
		ctx, query,
		plan.ID, plan.MultiContractID, plan.TenderID,
		string(plan.Status),
		plan.TotalAmount.StringFixed(2), plan.Currency,
		plan.FrozenPartyCount,
		plan.IdempotencyKey,
		plan.CreatedAt, plan.UpdatedAt,
	)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == pgUniqueViolation {
			return domain.ErrDuplicateKey
		}

		return fmt.Errorf("insert settlement_plan: %w", err)
	}

	return nil
}

func settlementPlanGetByID(ctx context.Context, q querier, id uuid.UUID) (*domain.SettlementPlan, error) {
	const query = `
SELECT id, multi_contract_id, tender_id, status, total_amount, currency, frozen_party_count, idempotency_key, created_at, updated_at
FROM settlement_plans
WHERE id = $1
`

	return scanSettlementPlan(q.QueryRow(ctx, query, id))
}

func settlementPlanGetByIDForUpdate(ctx context.Context, q querier, id uuid.UUID) (*domain.SettlementPlan, error) {
	const query = `
SELECT id, multi_contract_id, tender_id, status, total_amount, currency, frozen_party_count, idempotency_key, created_at, updated_at
FROM settlement_plans
WHERE id = $1
FOR UPDATE
`

	return scanSettlementPlan(q.QueryRow(ctx, query, id))
}

func settlementPlanUpdateStatus(ctx context.Context, q querier, id uuid.UUID, status domain.PlanStatus) error {
	const query = `UPDATE settlement_plans SET status = $2, updated_at = $3 WHERE id = $1`

	tag, err := q.Exec(ctx, query, id, string(status), time.Now().UTC())
	if err != nil {
		return fmt.Errorf("update settlement_plan status: %w", err)
	}

	if tag.RowsAffected() == 0 {
		return domain.ErrPlanNotFound
	}

	return nil
}

// settlementPlanCompleteAtomic performs an atomic ACTIVE → COMPLETED transition.
// The WHERE clause guards against concurrent completion (TOCTOU): only one concurrent
// call can observe RowsAffected == 1; the other observes 0 and returns ErrInvalidTransition.
// A separate existence check distinguishes "not found" from "not ACTIVE".
func settlementPlanCompleteAtomic(ctx context.Context, q querier, id uuid.UUID) error {
	const query = `
UPDATE settlement_plans
SET status = 'COMPLETED', updated_at = $2
WHERE id = $1 AND status = 'ACTIVE'
`

	tag, err := q.Exec(ctx, query, id, time.Now().UTC())
	if err != nil {
		return fmt.Errorf("complete settlement_plan atomic: %w", err)
	}

	if tag.RowsAffected() == 1 {
		return nil
	}

	// 0 rows affected: either the plan doesn't exist or it's not ACTIVE.
	// Perform a lightweight existence check to distinguish the two cases.
	plan, getErr := settlementPlanGetByID(ctx, q, id)
	if getErr != nil {
		return getErr // propagates ErrPlanNotFound on pgx.ErrNoRows
	}

	// Plan exists but is not ACTIVE (already COMPLETED or CANCELED).
	return fmt.Errorf("%w: plan is %s, cannot transition to COMPLETED", domain.ErrInvalidTransition, plan.Status)
}

func settlementPlanCountByMultiContractID(ctx context.Context, q querier, multiContractID uuid.UUID) (int, error) {
	const query = `
SELECT COUNT(*) FROM settlement_plans
WHERE multi_contract_id = $1 AND status != 'CANCELED'
`

	var count int
	if err := q.QueryRow(ctx, query, multiContractID).Scan(&count); err != nil {
		return 0, fmt.Errorf("count settlement_plans by multi_contract_id: %w", err)
	}

	return count, nil
}

func settlementPlanGetByMultiContractID(ctx context.Context, q querier, multiContractID uuid.UUID) (*domain.SettlementPlan, error) {
	const query = `
SELECT id, multi_contract_id, tender_id, status, total_amount, currency, frozen_party_count, idempotency_key, created_at, updated_at
FROM settlement_plans
WHERE multi_contract_id = $1 AND status != 'CANCELED'
LIMIT 1
`

	plan, err := scanSettlementPlan(q.QueryRow(ctx, query, multiContractID))
	if err != nil {
		if errors.Is(err, domain.ErrPlanNotFound) {
			return nil, nil //nolint:nilnil // nil,nil means "no plan exists" — intentional absence signal
		}

		return nil, fmt.Errorf("get settlement_plan by multi_contract_id: %w", err)
	}

	return plan, nil
}

func settlementPlanGetByMultiContractIDForUpdate(ctx context.Context, q querier, multiContractID uuid.UUID) (*domain.SettlementPlan, error) {
	const query = `
SELECT id, multi_contract_id, tender_id, status, total_amount, currency, frozen_party_count, idempotency_key, created_at, updated_at
FROM settlement_plans
WHERE multi_contract_id = $1 AND status != 'CANCELED'
LIMIT 1
FOR UPDATE
`

	plan, err := scanSettlementPlan(q.QueryRow(ctx, query, multiContractID))
	if err != nil {
		if errors.Is(err, domain.ErrPlanNotFound) {
			return nil, nil //nolint:nilnil // nil,nil means "no plan exists" — intentional absence signal
		}

		return nil, fmt.Errorf("get settlement_plan by multi_contract_id for update: %w", err)
	}

	return plan, nil
}

func scanSettlementPlan(row rowScanner) (*domain.SettlementPlan, error) {
	var p domain.SettlementPlan
	var totalAmountStr string
	var statusStr string

	err := row.Scan(
		&p.ID, &p.MultiContractID, &p.TenderID,
		&statusStr, &totalAmountStr, &p.Currency,
		&p.FrozenPartyCount, &p.IdempotencyKey,
		&p.CreatedAt, &p.UpdatedAt,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrPlanNotFound
		}

		return nil, fmt.Errorf("scan settlement_plan: %w", err)
	}

	amt, parseErr := decimal.NewFromString(totalAmountStr)
	if parseErr != nil {
		return nil, fmt.Errorf("parse total_amount %q: %w", totalAmountStr, parseErr)
	}

	p.TotalAmount = amt
	p.Status = domain.PlanStatus(statusStr)

	return &p, nil
}

// -----------------------------------------------------------------------
// SettlementAllocationStore — pool-backed
// -----------------------------------------------------------------------

// SettlementAllocationStore is a pool-backed store for settlement allocations.
type SettlementAllocationStore struct {
	q querier
}

// NewSettlementAllocationStore returns a SettlementAllocationStore backed by pool.
func NewSettlementAllocationStore(pool *pgxpool.Pool) *SettlementAllocationStore {
	return &SettlementAllocationStore{q: pool}
}

// txSettlementAllocationStore is a transaction-scoped store.
type txSettlementAllocationStore struct {
	tx querier
}

func (s *txSettlementAllocationStore) Create(ctx context.Context, alloc *domain.SettlementAllocation) error {
	return settlementAllocationCreate(ctx, s.tx, alloc)
}

func (s *txSettlementAllocationStore) GetByID(ctx context.Context, id uuid.UUID) (*domain.SettlementAllocation, error) {
	return settlementAllocationGetByID(ctx, s.tx, id)
}

func (s *txSettlementAllocationStore) ListByPlanID(ctx context.Context, planID uuid.UUID) ([]*domain.SettlementAllocation, error) {
	return settlementAllocationListByPlanID(ctx, s.tx, planID, false)
}

func (s *txSettlementAllocationStore) ListByPlanIDForUpdate(ctx context.Context, planID uuid.UUID) ([]*domain.SettlementAllocation, error) {
	return settlementAllocationListByPlanID(ctx, s.tx, planID, true)
}

func (s *txSettlementAllocationStore) UpdateStatus(ctx context.Context, id uuid.UUID, status domain.AllocationStatus, disbursedTxID *uuid.UUID) error {
	return settlementAllocationUpdateStatus(ctx, s.tx, id, status, disbursedTxID)
}

func (s *txSettlementAllocationStore) CountByPlanID(ctx context.Context, planID uuid.UUID) (int, error) {
	return settlementAllocationCountByPlanID(ctx, s.tx, planID)
}

// Pool-backed methods.

// Create inserts a new allocation.
func (s *SettlementAllocationStore) Create(ctx context.Context, alloc *domain.SettlementAllocation) error {
	return settlementAllocationCreate(ctx, s.q, alloc)
}

// GetByID fetches an allocation by primary key.
func (s *SettlementAllocationStore) GetByID(ctx context.Context, id uuid.UUID) (*domain.SettlementAllocation, error) {
	return settlementAllocationGetByID(ctx, s.q, id)
}

// ListByPlanID returns all allocations for a plan ordered by created_at ASC.
func (s *SettlementAllocationStore) ListByPlanID(ctx context.Context, planID uuid.UUID) ([]*domain.SettlementAllocation, error) {
	return settlementAllocationListByPlanID(ctx, s.q, planID, false)
}

// UpdateStatus updates allocation status and optional disbursed_tx_id.
func (s *SettlementAllocationStore) UpdateStatus(ctx context.Context, id uuid.UUID, status domain.AllocationStatus, disbursedTxID *uuid.UUID) error {
	return settlementAllocationUpdateStatus(ctx, s.q, id, status, disbursedTxID)
}

// CountByPlanID returns the total number of allocations for a plan.
func (s *SettlementAllocationStore) CountByPlanID(ctx context.Context, planID uuid.UUID) (int, error) {
	return settlementAllocationCountByPlanID(ctx, s.q, planID)
}

// --- helpers ---

func settlementAllocationCreate(ctx context.Context, q querier, alloc *domain.SettlementAllocation) error {
	const query = `
INSERT INTO settlement_allocations
    (id, plan_id, vendor_user_id, role_id,
     share_bps, allocated_amount, currency, is_rounding_sink,
     status, disbursed_tx_id, idempotency_key, created_at, updated_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)
`

	_, err := q.Exec(
		ctx, query,
		alloc.ID, alloc.PlanID, alloc.VendorUserID, alloc.RoleID,
		alloc.ShareBps,
		alloc.AllocatedAmount.StringFixed(2), alloc.Currency,
		alloc.IsRoundingSink,
		string(alloc.Status),
		alloc.DisbursedTxID,
		alloc.IdempotencyKey,
		alloc.CreatedAt, alloc.UpdatedAt,
	)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == pgUniqueViolation {
			return domain.ErrDuplicateKey
		}

		return fmt.Errorf("insert settlement_allocation: %w", err)
	}

	return nil
}

func settlementAllocationGetByID(ctx context.Context, q querier, id uuid.UUID) (*domain.SettlementAllocation, error) {
	const query = `
SELECT id, plan_id, vendor_user_id, role_id,
       share_bps, allocated_amount, currency, is_rounding_sink,
       status, disbursed_tx_id, idempotency_key, created_at, updated_at
FROM settlement_allocations
WHERE id = $1
`

	return scanSettlementAllocation(q.QueryRow(ctx, query, id))
}

// settlementAllocationListByPlanID fetches all allocations for a plan.
// forUpdate=true appends FOR UPDATE (used by disburse service for TOCTOU safety).
func settlementAllocationListByPlanID(ctx context.Context, q querier, planID uuid.UUID, forUpdate bool) ([]*domain.SettlementAllocation, error) {
	query := `
SELECT id, plan_id, vendor_user_id, role_id,
       share_bps, allocated_amount, currency, is_rounding_sink,
       status, disbursed_tx_id, idempotency_key, created_at, updated_at
FROM settlement_allocations
WHERE plan_id = $1
ORDER BY created_at ASC
`
	if forUpdate {
		query += " FOR UPDATE"
	}

	rows, err := q.Query(ctx, query, planID)
	if err != nil {
		return nil, fmt.Errorf("list settlement_allocations: %w", err)
	}

	defer rows.Close()

	var allocs []*domain.SettlementAllocation

	for rows.Next() {
		alloc, scanErr := scanSettlementAllocationRow(rows)
		if scanErr != nil {
			return nil, scanErr
		}

		allocs = append(allocs, alloc)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate settlement_allocations: %w", err)
	}

	return allocs, nil
}

func settlementAllocationUpdateStatus(ctx context.Context, q querier, id uuid.UUID, status domain.AllocationStatus, disbursedTxID *uuid.UUID) error {
	const query = `
UPDATE settlement_allocations
SET status = $2, disbursed_tx_id = $3, updated_at = $4
WHERE id = $1
`

	tag, err := q.Exec(ctx, query, id, string(status), disbursedTxID, time.Now().UTC())
	if err != nil {
		return fmt.Errorf("update settlement_allocation status: %w", err)
	}

	if tag.RowsAffected() == 0 {
		return domain.ErrAllocationNotFound
	}

	return nil
}

func settlementAllocationCountByPlanID(ctx context.Context, q querier, planID uuid.UUID) (int, error) {
	const query = `SELECT COUNT(*) FROM settlement_allocations WHERE plan_id = $1`

	var count int
	if err := q.QueryRow(ctx, query, planID).Scan(&count); err != nil {
		return 0, fmt.Errorf("count settlement_allocations by plan_id: %w", err)
	}

	return count, nil
}

func scanSettlementAllocation(row rowScanner) (*domain.SettlementAllocation, error) {
	var a domain.SettlementAllocation
	var allocAmtStr string
	var statusStr string

	err := row.Scan(
		&a.ID, &a.PlanID, &a.VendorUserID, &a.RoleID,
		&a.ShareBps, &allocAmtStr, &a.Currency,
		&a.IsRoundingSink, &statusStr,
		&a.DisbursedTxID,
		&a.IdempotencyKey,
		&a.CreatedAt, &a.UpdatedAt,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrAllocationNotFound
		}

		return nil, fmt.Errorf("scan settlement_allocation: %w", err)
	}

	amt, parseErr := decimal.NewFromString(allocAmtStr)
	if parseErr != nil {
		return nil, fmt.Errorf("parse allocated_amount %q: %w", allocAmtStr, parseErr)
	}

	a.AllocatedAmount = amt
	a.Status = domain.AllocationStatus(statusStr)

	return &a, nil
}

func scanSettlementAllocationRow(rows pgx.Rows) (*domain.SettlementAllocation, error) {
	var a domain.SettlementAllocation
	var allocAmtStr string
	var statusStr string

	err := rows.Scan(
		&a.ID, &a.PlanID, &a.VendorUserID, &a.RoleID,
		&a.ShareBps, &allocAmtStr, &a.Currency,
		&a.IsRoundingSink, &statusStr,
		&a.DisbursedTxID,
		&a.IdempotencyKey,
		&a.CreatedAt, &a.UpdatedAt,
	)
	if err != nil {
		return nil, fmt.Errorf("scan settlement_allocation row: %w", err)
	}

	amt, parseErr := decimal.NewFromString(allocAmtStr)
	if parseErr != nil {
		return nil, fmt.Errorf("parse allocated_amount %q: %w", allocAmtStr, parseErr)
	}

	a.AllocatedAmount = amt
	a.Status = domain.AllocationStatus(statusStr)

	return &a, nil
}
