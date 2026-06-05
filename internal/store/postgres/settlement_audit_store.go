package postgres

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/CoverOnes/payment/internal/domain"
	"github.com/jackc/pgx/v5/pgxpool"
)

// SettlementAuditStore is a pool-backed append-only audit log for settlement events.
type SettlementAuditStore struct {
	q querier
}

// NewSettlementAuditStore returns a SettlementAuditStore backed by pool.
func NewSettlementAuditStore(pool *pgxpool.Pool) *SettlementAuditStore {
	return &SettlementAuditStore{q: pool}
}

// txSettlementAuditStore is a transaction-scoped settlement audit store.
type txSettlementAuditStore struct {
	tx querier
}

func (s *txSettlementAuditStore) Append(ctx context.Context, entry *domain.SettlementAuditEntry) error {
	return settlementAuditAppend(ctx, s.tx, entry)
}

// Append inserts a new settlement audit entry (pool-backed).
func (s *SettlementAuditStore) Append(ctx context.Context, entry *domain.SettlementAuditEntry) error {
	return settlementAuditAppend(ctx, s.q, entry)
}

func settlementAuditAppend(ctx context.Context, q querier, entry *domain.SettlementAuditEntry) error {
	const query = `
INSERT INTO settlement_audit (id, plan_id, allocation_id, event_type, actor_service, payload, occurred_at)
VALUES ($1, $2, $3, $4, $5, $6, $7)
`

	// Ensure payload is never NULL — default to empty JSON object if nil.
	payload := entry.Payload
	if len(payload) == 0 {
		payload = json.RawMessage("{}")
	}

	_, err := q.Exec(
		ctx, query,
		entry.ID, entry.PlanID, entry.AllocationID,
		entry.EventType, entry.ActorService,
		[]byte(payload),
		entry.OccurredAt,
	)
	if err != nil {
		return fmt.Errorf("insert settlement_audit: %w", err)
	}

	return nil
}
