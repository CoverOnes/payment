package postgres

import (
	"context"
	"fmt"

	"github.com/CoverOnes/payment/internal/domain"
	"github.com/jackc/pgx/v5/pgxpool"
)

// AuditStore is a pool-backed append-only audit log store.
type AuditStore struct {
	q querier
}

// NewAuditStore returns an AuditStore backed by pool.
func NewAuditStore(pool *pgxpool.Pool) *AuditStore {
	return &AuditStore{q: pool}
}

// txAuditStore is a transaction-scoped audit store.
type txAuditStore struct {
	tx querier
}

func (s *txAuditStore) Append(ctx context.Context, entry *domain.TransactionAudit) error {
	return auditAppend(ctx, s.tx, entry)
}

// Append inserts a new audit entry (pool-backed).
func (s *AuditStore) Append(ctx context.Context, entry *domain.TransactionAudit) error {
	return auditAppend(ctx, s.q, entry)
}

func auditAppend(ctx context.Context, q querier, entry *domain.TransactionAudit) error {
	const query = `
INSERT INTO transaction_audit (id, transaction_id, from_status, to_status, actor_user_id, occurred_at)
VALUES ($1, $2, $3, $4, $5, $6)
`

	_, err := q.Exec(
		ctx, query,
		entry.ID, entry.TransactionID,
		string(entry.FromStatus), string(entry.ToStatus),
		entry.ActorUserID, entry.OccurredAt,
	)
	if err != nil {
		return fmt.Errorf("insert transaction_audit: %w", err)
	}

	return nil
}
