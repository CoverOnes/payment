package postgres

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

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
// transactional access to all settlement stores.
// The plan and allocation stores passed to fn are tx-scoped and expose
// GetByIDForUpdate / ListByPlanIDForUpdate; these methods are unavailable on
// pool-backed stores, enforcing that FOR UPDATE only runs inside a transaction.
// The disbursements store and the transactions store are also tx-scoped so that
// the transactions row + the settlement_milestone_disbursements row are written
// atomically in ONE DB transaction (Critical atomicity fix).
// outbox is a tx-scoped OutboxStore so event_outbox rows are committed atomically
// with the business operation (same-tx enqueue — loss-free on server restart).
// If fn returns an error the transaction is rolled back; otherwise it is committed.
func (m *SettlementTxManager) WithSettlementTx(
	ctx context.Context,
	fn func(
		ctx context.Context,
		plans store.TxSettlementPlanStore,
		allocs store.TxSettlementAllocationStore,
		disbursements store.SettlementMilestoneDisbursementStore,
		txTxStore store.TransactionStore,
		audit store.SettlementAuditStore,
		outbox store.OutboxStore,
	) error,
) error {
	tx, err := m.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("begin settlement transaction: %w", err)
	}

	defer func() {
		if rbErr := tx.Rollback(ctx); rbErr != nil && !errors.Is(rbErr, pgx.ErrTxClosed) {
			slog.Warn("settlement tx rollback failed", "err", rbErr)
		}
	}()

	txPlans := &txSettlementPlanStore{tx: tx}
	txAllocs := &txSettlementAllocationStore{tx: tx}
	txDisburse := &txSettlementMilestoneDisbursementStore{tx: tx}
	txTxStore := &txTransactionStore{tx: tx}
	txAudit := &txSettlementAuditStore{tx: tx}
	txOutbox := &txOutboxStore{tx: tx}

	if fnErr := fn(ctx, txPlans, txAllocs, txDisburse, txTxStore, txAudit, txOutbox); fnErr != nil {
		return fnErr
	}

	if commitErr := tx.Commit(ctx); commitErr != nil {
		return fmt.Errorf("commit settlement transaction: %w", commitErr)
	}

	return nil
}
