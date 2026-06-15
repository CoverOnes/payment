// Package store defines the storage interfaces for the payment domain.
package store

import (
	"context"
	"time"

	"github.com/CoverOnes/payment/internal/domain"
	"github.com/google/uuid"
	"github.com/shopspring/decimal"
)

// TransactionStore defines persistence operations for transactions.
type TransactionStore interface {
	// Create inserts a new transaction. Returns ErrDuplicateKey if idempotency_key already exists.
	Create(ctx context.Context, tx *domain.Transaction) error
	// GetByID fetches a transaction by its primary key. Returns ErrTransactionNotFound if none.
	GetByID(ctx context.Context, id uuid.UUID) (*domain.Transaction, error)
	// GetByIDForUpdate fetches a transaction by ID with SELECT ... FOR UPDATE inside a tx.
	GetByIDForUpdate(ctx context.Context, id uuid.UUID) (*domain.Transaction, error)
	// GetByIdempotencyKey fetches a transaction by (payer_user_id, idempotency_key).
	// Scoping by payer prevents cross-user idempotency key leaks (IDOR via shared key string).
	// Returns ErrTransactionNotFound if none.
	GetByIdempotencyKey(ctx context.Context, payerUserID uuid.UUID, key string) (*domain.Transaction, error)
	// UpdateStatus updates only the status and updated_at columns.
	UpdateStatus(ctx context.Context, id uuid.UUID, status domain.Status) error
	// ListByUserID returns all transactions where payer_user_id or payee_user_id == userID, ordered by created_at desc.
	ListByUserID(ctx context.Context, userID uuid.UUID) ([]*domain.Transaction, error)
}

// AuditStore defines persistence operations for the append-only transaction audit log.
type AuditStore interface {
	// Append inserts a new audit log entry.
	Append(ctx context.Context, entry *domain.TransactionAudit) error
}

// TxManager runs a function inside a single Postgres transaction, providing
// transactional access to all stores.
type TxManager interface {
	WithTx(ctx context.Context, fn func(ctx context.Context, txs TransactionStore, audits AuditStore) error) error
}

// SettlementPlanStore defines persistence operations for settlement plans.
// GetByIDForUpdate is intentionally absent — it is only callable from within
// SettlementTxManager.WithSettlementTx via the tx-scoped store, ensuring
// SELECT ... FOR UPDATE is always executed inside an explicit transaction.
type SettlementPlanStore interface {
	// Create inserts a new settlement plan. Returns ErrDuplicateKey if idempotency_key conflicts.
	Create(ctx context.Context, plan *domain.SettlementPlan) error
	// GetByID fetches a plan by primary key. Returns ErrPlanNotFound if none.
	GetByID(ctx context.Context, id uuid.UUID) (*domain.SettlementPlan, error)
	// UpdateStatus updates only the status and updated_at columns.
	UpdateStatus(ctx context.Context, id uuid.UUID, status domain.PlanStatus) error
	// CompletePlanAtomic atomically transitions a plan from ACTIVE to COMPLETED using
	// UPDATE ... WHERE id=$1 AND status='ACTIVE'. Returns ErrInvalidTransition when
	// the plan exists but is not ACTIVE (concurrent completion already won the race).
	// Returns ErrPlanNotFound when no row matches id at all.
	CompletePlanAtomic(ctx context.Context, id uuid.UUID) error
	// CountByMultiContractID returns the number of non-canceled plans for a contract.
	CountByMultiContractID(ctx context.Context, multiContractID uuid.UUID) (int, error)
	// GetByMultiContractID returns the active (non-canceled) settlement plan for a contract.
	// Returns nil, nil if no plan exists.
	GetByMultiContractID(ctx context.Context, multiContractID uuid.UUID) (*domain.SettlementPlan, error)
}

// SettlementAllocationStore defines persistence operations for settlement allocations.
// ListByPlanIDForUpdate is intentionally absent — it is only callable from within
// SettlementTxManager.WithSettlementTx via the tx-scoped store, ensuring
// SELECT ... FOR UPDATE is always executed inside an explicit transaction.
type SettlementAllocationStore interface {
	// Create inserts a new allocation. Returns ErrDuplicateKey if idempotency_key conflicts.
	Create(ctx context.Context, alloc *domain.SettlementAllocation) error
	// GetByID fetches an allocation by primary key. Returns ErrAllocationNotFound if none.
	GetByID(ctx context.Context, id uuid.UUID) (*domain.SettlementAllocation, error)
	// ListByPlanID returns all allocations for a plan ordered by created_at ASC.
	ListByPlanID(ctx context.Context, planID uuid.UUID) ([]*domain.SettlementAllocation, error)
	// UpdateStatus updates allocation status, updated_at, and optionally disbursed_tx_id.
	UpdateStatus(ctx context.Context, id uuid.UUID, status domain.AllocationStatus, disbursedTxID *uuid.UUID) error
	// CountByPlanID returns the total number of allocations for a plan.
	CountByPlanID(ctx context.Context, planID uuid.UUID) (int, error)
}

// SettlementAuditStore defines persistence for the append-only settlement audit log.
type SettlementAuditStore interface {
	// Append inserts a new audit entry. The partitioned table routes by occurred_at.
	Append(ctx context.Context, entry *domain.SettlementAuditEntry) error
}

// TxSettlementPlanStore extends SettlementPlanStore with SELECT ... FOR UPDATE,
// available only inside a SettlementTxManager.WithSettlementTx callback.
// Callers outside a transaction MUST use SettlementPlanStore (no FOR UPDATE).
type TxSettlementPlanStore interface {
	SettlementPlanStore
	// GetByIDForUpdate fetches a plan with SELECT ... FOR UPDATE inside a DB transaction.
	// Used by the disburse service to lock the plan before checking/updating allocations (TOCTOU).
	GetByIDForUpdate(ctx context.Context, id uuid.UUID) (*domain.SettlementPlan, error)
	// GetByMultiContractIDForUpdate fetches the active plan for a contract with FOR UPDATE.
	// Used by the disburse service when the plan_id is unknown and must be resolved under lock.
	GetByMultiContractIDForUpdate(ctx context.Context, multiContractID uuid.UUID) (*domain.SettlementPlan, error)
}

// TxSettlementAllocationStore extends SettlementAllocationStore with SELECT ... FOR UPDATE,
// available only inside a SettlementTxManager.WithSettlementTx callback.
// Callers outside a transaction MUST use SettlementAllocationStore (no FOR UPDATE).
type TxSettlementAllocationStore interface {
	SettlementAllocationStore
	// ListByPlanIDForUpdate returns all allocations for a plan with SELECT ... FOR UPDATE.
	// Used by the disburse service to lock rows before updating statuses (TOCTOU).
	ListByPlanIDForUpdate(ctx context.Context, planID uuid.UUID) ([]*domain.SettlementAllocation, error)
}

// SettlementMilestoneDisbursementStore defines persistence for per-milestone disbursement records.
// Each (plan_id, milestone_id, vendor_user_id) triple has at most one row — the UNIQUE index
// enforces idempotency at the DB level.
type SettlementMilestoneDisbursementStore interface {
	// Create inserts a new disbursement record using ON CONFLICT DO NOTHING.
	// Returns (true, nil) when the row was inserted, (false, nil) when it already existed
	// (idempotent skip — no transaction abort), or (false, err) on unexpected error.
	Create(ctx context.Context, d *domain.SettlementMilestoneDisbursement) (inserted bool, err error)
	// GetByPlanMilestoneVendor fetches a disbursement by the (plan, milestone, vendor) triple.
	// Returns ErrMilestoneDisbursementNotFound if none.
	GetByPlanMilestoneVendor(ctx context.Context, planID, milestoneID, vendorUserID uuid.UUID) (*domain.SettlementMilestoneDisbursement, error)
	// ListByPlanMilestone returns all disbursements for a (plan, milestone) pair, ordered by created_at ASC.
	ListByPlanMilestone(ctx context.Context, planID, milestoneID uuid.UUID) ([]*domain.SettlementMilestoneDisbursement, error)
	// UpdateStatus updates the status, tx_id, and updated_at columns.
	UpdateStatus(ctx context.Context, id uuid.UUID, status domain.MilestoneDisbursementStatus, txID *uuid.UUID) error
	// SumDisbursedByPlanID returns the SUM(amount) of all rows WHERE plan_id=$1 AND status='DISBURSED'.
	// Used by DisburseMilestone to enforce the per-plan cumulative cap against plan.TotalAmount (M-1).
	SumDisbursedByPlanID(ctx context.Context, planID uuid.UUID) (decimal.Decimal, error)
}

// OutboxStore defines persistence operations for the transactional outbox table.
// All writes happen inside the same DB transaction as the business operation they record
// (same-tx enqueue pattern). Reads (PollReady) are issued by the in-process poller on
// its own connection — not inside a business transaction.
type OutboxStore interface {
	// Enqueue inserts a new outbox event. ON CONFLICT (event_id) DO NOTHING makes
	// re-enqueue of the same EventID a safe no-op (idempotent at DB level).
	Enqueue(ctx context.Context, e *domain.OutboxEvent) error
	// PollReady atomically claims up to limit unpublished rows eligible for delivery.
	// Uses SELECT ... FOR UPDATE SKIP LOCKED so concurrent pollers claim disjoint sets.
	PollReady(ctx context.Context, limit int) ([]*domain.OutboxEvent, error)
	// MarkPublished sets published_at = now() and clears claimed_until.
	MarkPublished(ctx context.Context, id uuid.UUID) error
	// MarkFailed increments attempts, records last_error, advances next_attempt_at
	// with exponential backoff, and clears claimed_until so the row is re-pollable.
	MarkFailed(ctx context.Context, id uuid.UUID, lastErr string) error
	// DeletePublishedBefore removes published rows older than cutoff (retention janitor).
	// Returns the count of deleted rows.
	DeletePublishedBefore(ctx context.Context, cutoff time.Time) (int64, error)
	// CountStaleUnpublished returns the number of unpublished outbox rows whose
	// created_at is older than the given threshold. Used by the poller to alert on
	// events that are stuck and never entering the poll batch (DB-side stale check).
	CountStaleUnpublished(ctx context.Context, olderThan time.Time) (int64, error)
}

// SettlementTxManager runs a function inside a single Postgres transaction, providing
// transactional access to all settlement stores atomically.
// The tx-scoped plan and allocation stores expose FOR UPDATE methods unavailable
// on the pool-backed stores, ensuring row-level locking can only happen inside a tx.
// txTxStore is a tx-scoped TransactionStore ensuring the transactions row and the
// settlement_milestone_disbursements row are written in ONE transaction (atomicity fix).
// outbox is a tx-scoped OutboxStore so the event_outbox row is committed atomically
// with the business operation (same-tx enqueue: enqueue + business write in one tx).
// Used by the service: write plan/disbursement + enqueue outbox event atomically.
type SettlementTxManager interface {
	WithSettlementTx(
		ctx context.Context,
		fn func(
			ctx context.Context,
			plans TxSettlementPlanStore,
			allocs TxSettlementAllocationStore,
			disbursements SettlementMilestoneDisbursementStore,
			txTxStore TransactionStore,
			audit SettlementAuditStore,
			outbox OutboxStore,
		) error,
	) error
}
