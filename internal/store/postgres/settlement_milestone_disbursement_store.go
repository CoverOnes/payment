package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/CoverOnes/payment/internal/domain"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"
)

// SettlementMilestoneDisbursementStore is a pool-backed store for per-milestone disbursement records.
type SettlementMilestoneDisbursementStore struct {
	q querier
}

// NewSettlementMilestoneDisbursementStore returns a SettlementMilestoneDisbursementStore backed by pool.
func NewSettlementMilestoneDisbursementStore(pool *pgxpool.Pool) *SettlementMilestoneDisbursementStore {
	return &SettlementMilestoneDisbursementStore{q: pool}
}

// txSettlementMilestoneDisbursementStore is a transaction-scoped store.
type txSettlementMilestoneDisbursementStore struct {
	tx querier
}

func (s *txSettlementMilestoneDisbursementStore) Create(ctx context.Context, d *domain.SettlementMilestoneDisbursement) (bool, error) {
	return settlementMilestoneDisbursementCreate(ctx, s.tx, d)
}

func (s *txSettlementMilestoneDisbursementStore) GetByPlanMilestoneVendor(
	ctx context.Context, planID, milestoneID, vendorUserID uuid.UUID,
) (*domain.SettlementMilestoneDisbursement, error) {
	return settlementMilestoneDisbursementGetByTriple(ctx, s.tx, planID, milestoneID, vendorUserID)
}

func (s *txSettlementMilestoneDisbursementStore) ListByPlanMilestone(
	ctx context.Context, planID, milestoneID uuid.UUID,
) ([]*domain.SettlementMilestoneDisbursement, error) {
	return settlementMilestoneDisbursementListByPlanMilestone(ctx, s.tx, planID, milestoneID)
}

func (s *txSettlementMilestoneDisbursementStore) UpdateStatus(
	ctx context.Context, id uuid.UUID, status domain.MilestoneDisbursementStatus, txID *uuid.UUID,
) error {
	return settlementMilestoneDisbursementUpdateStatus(ctx, s.tx, id, status, txID)
}

func (s *txSettlementMilestoneDisbursementStore) SumDisbursedByPlanID(ctx context.Context, planID uuid.UUID) (decimal.Decimal, error) {
	return settlementMilestoneDisbursementSumByPlanID(ctx, s.tx, planID)
}

// Pool-backed methods.

// Create inserts a new disbursement record using ON CONFLICT DO NOTHING.
func (s *SettlementMilestoneDisbursementStore) Create(ctx context.Context, d *domain.SettlementMilestoneDisbursement) (bool, error) {
	return settlementMilestoneDisbursementCreate(ctx, s.q, d)
}

// GetByPlanMilestoneVendor fetches a disbursement by the (plan, milestone, vendor) triple.
func (s *SettlementMilestoneDisbursementStore) GetByPlanMilestoneVendor(
	ctx context.Context, planID, milestoneID, vendorUserID uuid.UUID,
) (*domain.SettlementMilestoneDisbursement, error) {
	return settlementMilestoneDisbursementGetByTriple(ctx, s.q, planID, milestoneID, vendorUserID)
}

// ListByPlanMilestone returns all disbursements for a (plan, milestone) pair.
func (s *SettlementMilestoneDisbursementStore) ListByPlanMilestone(
	ctx context.Context, planID, milestoneID uuid.UUID,
) ([]*domain.SettlementMilestoneDisbursement, error) {
	return settlementMilestoneDisbursementListByPlanMilestone(ctx, s.q, planID, milestoneID)
}

// UpdateStatus updates the status, tx_id, and updated_at columns.
func (s *SettlementMilestoneDisbursementStore) UpdateStatus(
	ctx context.Context, id uuid.UUID, status domain.MilestoneDisbursementStatus, txID *uuid.UUID,
) error {
	return settlementMilestoneDisbursementUpdateStatus(ctx, s.q, id, status, txID)
}

// SumDisbursedByPlanID returns the SUM(amount) of all DISBURSED rows for a plan.
func (s *SettlementMilestoneDisbursementStore) SumDisbursedByPlanID(ctx context.Context, planID uuid.UUID) (decimal.Decimal, error) {
	return settlementMilestoneDisbursementSumByPlanID(ctx, s.q, planID)
}

// --- helpers ---

// settlementMilestoneDisbursementCreate inserts a new disbursement record.
// Uses ON CONFLICT DO NOTHING so that an idempotent replay does not abort the transaction
// (a plain INSERT would leave the tx in SQLSTATE 25P02 after a UNIQUE violation, poisoning
// all subsequent statements in the same tx). Returns (true, nil) when a new row was inserted,
// (false, nil) when the row already existed (conflict skipped), or (false, err) on error.
func settlementMilestoneDisbursementCreate(ctx context.Context, q querier, d *domain.SettlementMilestoneDisbursement) (inserted bool, err error) {
	const query = `
INSERT INTO settlement_milestone_disbursements
    (id, plan_id, milestone_id, vendor_user_id, amount, tx_id, status, idempotency_key, created_at, updated_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
ON CONFLICT (plan_id, milestone_id, vendor_user_id) DO NOTHING
`

	tag, execErr := q.Exec(
		ctx, query,
		d.ID, d.PlanID, d.MilestoneID, d.VendorUserID,
		d.Amount.StringFixed(2),
		d.TxID,
		string(d.Status),
		d.IdempotencyKey,
		d.CreatedAt, d.UpdatedAt,
	)
	if execErr != nil {
		return false, fmt.Errorf("insert settlement_milestone_disbursement: %w", execErr)
	}

	return tag.RowsAffected() == 1, nil
}

func settlementMilestoneDisbursementGetByTriple(
	ctx context.Context, q querier, planID, milestoneID, vendorUserID uuid.UUID,
) (*domain.SettlementMilestoneDisbursement, error) {
	const query = `
SELECT id, plan_id, milestone_id, vendor_user_id, amount, tx_id, status, idempotency_key, created_at, updated_at
FROM settlement_milestone_disbursements
WHERE plan_id = $1 AND milestone_id = $2 AND vendor_user_id = $3
`

	return scanSettlementMilestoneDisbursement(q.QueryRow(ctx, query, planID, milestoneID, vendorUserID))
}

func settlementMilestoneDisbursementListByPlanMilestone(
	ctx context.Context, q querier, planID, milestoneID uuid.UUID,
) ([]*domain.SettlementMilestoneDisbursement, error) {
	const query = `
SELECT id, plan_id, milestone_id, vendor_user_id, amount, tx_id, status, idempotency_key, created_at, updated_at
FROM settlement_milestone_disbursements
WHERE plan_id = $1 AND milestone_id = $2
ORDER BY created_at ASC
`

	rows, err := q.Query(ctx, query, planID, milestoneID)
	if err != nil {
		return nil, fmt.Errorf("list settlement_milestone_disbursements: %w", err)
	}

	defer rows.Close()

	var disbursements []*domain.SettlementMilestoneDisbursement

	for rows.Next() {
		d, scanErr := scanSettlementMilestoneDisbursementRow(rows)
		if scanErr != nil {
			return nil, scanErr
		}

		disbursements = append(disbursements, d)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate settlement_milestone_disbursements: %w", err)
	}

	return disbursements, nil
}

func settlementMilestoneDisbursementUpdateStatus(
	ctx context.Context, q querier, id uuid.UUID, status domain.MilestoneDisbursementStatus, txID *uuid.UUID,
) error {
	const query = `
UPDATE settlement_milestone_disbursements
SET status = $2, tx_id = $3, updated_at = $4
WHERE id = $1
`

	tag, err := q.Exec(ctx, query, id, string(status), txID, time.Now().UTC())
	if err != nil {
		return fmt.Errorf("update settlement_milestone_disbursement status: %w", err)
	}

	if tag.RowsAffected() == 0 {
		return domain.ErrMilestoneDisbursementNotFound
	}

	return nil
}

// settlementMilestoneDisbursementSumByPlanID returns SUM(amount) WHERE plan_id=$1 AND status='DISBURSED'.
// Returns decimal.Zero when no disbursed rows exist.
func settlementMilestoneDisbursementSumByPlanID(ctx context.Context, q querier, planID uuid.UUID) (decimal.Decimal, error) {
	const query = `
SELECT COALESCE(SUM(amount), 0)::text
FROM settlement_milestone_disbursements
WHERE plan_id = $1 AND status = 'DISBURSED'
`

	var amtStr string
	if err := q.QueryRow(ctx, query, planID).Scan(&amtStr); err != nil {
		return decimal.Zero, fmt.Errorf("sum disbursed by plan_id: %w", err)
	}

	amt, parseErr := decimal.NewFromString(amtStr)
	if parseErr != nil {
		return decimal.Zero, fmt.Errorf("parse sum disbursed %q: %w", amtStr, parseErr)
	}

	return amt, nil
}

func scanSettlementMilestoneDisbursement(row rowScanner) (*domain.SettlementMilestoneDisbursement, error) {
	var d domain.SettlementMilestoneDisbursement
	var amtStr string
	var statusStr string

	err := row.Scan(
		&d.ID, &d.PlanID, &d.MilestoneID, &d.VendorUserID,
		&amtStr, &d.TxID,
		&statusStr,
		&d.IdempotencyKey,
		&d.CreatedAt, &d.UpdatedAt,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrMilestoneDisbursementNotFound
		}

		return nil, fmt.Errorf("scan settlement_milestone_disbursement: %w", err)
	}

	amt, parseErr := decimal.NewFromString(amtStr)
	if parseErr != nil {
		return nil, fmt.Errorf("parse disbursement amount %q: %w", amtStr, parseErr)
	}

	d.Amount = amt
	d.Status = domain.MilestoneDisbursementStatus(statusStr)

	return &d, nil
}

func scanSettlementMilestoneDisbursementRow(rows pgx.Rows) (*domain.SettlementMilestoneDisbursement, error) {
	var d domain.SettlementMilestoneDisbursement
	var amtStr string
	var statusStr string

	err := rows.Scan(
		&d.ID, &d.PlanID, &d.MilestoneID, &d.VendorUserID,
		&amtStr, &d.TxID,
		&statusStr,
		&d.IdempotencyKey,
		&d.CreatedAt, &d.UpdatedAt,
	)
	if err != nil {
		return nil, fmt.Errorf("scan settlement_milestone_disbursement row: %w", err)
	}

	amt, parseErr := decimal.NewFromString(amtStr)
	if parseErr != nil {
		return nil, fmt.Errorf("parse disbursement amount %q: %w", amtStr, parseErr)
	}

	d.Amount = amt
	d.Status = domain.MilestoneDisbursementStatus(statusStr)

	return &d, nil
}
