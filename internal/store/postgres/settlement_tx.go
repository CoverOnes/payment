package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/CoverOnes/payment/internal/store"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// SettlementTxManager implements store.SettlementTxManager using pgxpool.Pool.
type SettlementTxManager struct {
	pool *pgxpool.Pool
}

// NewSettlementTxManager returns a SettlementTxManager backed by the given pool.
func NewSettlementTxManager(pool *pgxpool.Pool) *SettlementTxManager {
	return &SettlementTxManager{pool: pool}
}

// WithSettlementTx runs fn inside a single Postgres transaction providing
// transactional access to all three settlement stores.
// If fn returns an error the transaction is rolled back; otherwise it is committed.
func (m *SettlementTxManager) WithSettlementTx(
	ctx context.Context,
	fn func(
		ctx context.Context,
		plans store.SettlementPlanStore,
		allocs store.SettlementAllocationStore,
		audit store.SettlementAuditStore,
	) error,
) error {
	tx, err := m.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("begin settlement transaction: %w", err)
	}

	defer func() {
		if rbErr := tx.Rollback(ctx); rbErr != nil && !errors.Is(rbErr, pgx.ErrTxClosed) {
			_ = rbErr
		}
	}()

	txPlans := &txSettlementPlanStore{tx: tx}
	txAllocs := &txSettlementAllocationStore{tx: tx}
	txAudit := &txSettlementAuditStore{tx: tx}

	if fnErr := fn(ctx, txPlans, txAllocs, txAudit); fnErr != nil {
		return fnErr
	}

	if commitErr := tx.Commit(ctx); commitErr != nil {
		return fmt.Errorf("commit settlement transaction: %w", commitErr)
	}

	return nil
}
