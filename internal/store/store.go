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
