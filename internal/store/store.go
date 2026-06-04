// Package store defines the storage interfaces for the payment domain.
package store

import (
	"context"

	"github.com/CoverOnes/payment/internal/domain"
	"github.com/google/uuid"
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
	// CountByMultiContractID returns the number of non-canceled plans for a contract.
	CountByMultiContractID(ctx context.Context, multiContractID uuid.UUID) (int, error)
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

// SettlementTxManager runs a function inside a single Postgres transaction, providing
// transactional access to all three settlement stores atomically.
// The tx-scoped plan and allocation stores expose FOR UPDATE methods unavailable
// on the pool-backed stores, ensuring row-level locking can only happen inside a tx.
// Used by the disburse service: lock plan + lock allocations + write audit in one tx.
type SettlementTxManager interface {
	WithSettlementTx(
		ctx context.Context,
		fn func(
			ctx context.Context,
			plans TxSettlementPlanStore,
			allocs TxSettlementAllocationStore,
			audit SettlementAuditStore,
		) error,
	) error
}
