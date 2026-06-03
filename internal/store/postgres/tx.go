package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/CoverOnes/payment/internal/store"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// TxManager implements store.TxManager using pgxpool.Pool.
type TxManager struct {
	pool *pgxpool.Pool
}

// NewTxManager returns a TxManager backed by the given pool.
func NewTxManager(pool *pgxpool.Pool) *TxManager {
	return &TxManager{pool: pool}
}

// WithTx runs fn inside a single Postgres transaction.
// If fn returns an error the transaction is rolled back; otherwise it is committed.
func (m *TxManager) WithTx(
	ctx context.Context,
	fn func(ctx context.Context, txs store.TransactionStore, audits store.AuditStore) error,
) error {
	tx, err := m.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}

	defer func() {
		if rbErr := tx.Rollback(ctx); rbErr != nil && !errors.Is(rbErr, pgx.ErrTxClosed) {
			_ = rbErr
		}
	}()

	txStore := &txTransactionStore{tx: tx}
	txAudits := &txAuditStore{tx: tx}

	if fnErr := fn(ctx, txStore, txAudits); fnErr != nil {
		return fnErr
	}

	if commitErr := tx.Commit(ctx); commitErr != nil {
		return fmt.Errorf("commit transaction: %w", commitErr)
	}

	return nil
}
