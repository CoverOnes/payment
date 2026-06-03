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

// querier is satisfied by both pgxpool.Pool and pgx.Tx.
type querier interface {
	Exec(ctx context.Context, sql string, arguments ...any) (pgconn.CommandTag, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// TransactionStore is a pool-backed transaction store.
type TransactionStore struct {
	q querier
}

// NewTransactionStore returns a TransactionStore backed by pool.
func NewTransactionStore(pool *pgxpool.Pool) *TransactionStore {
	return &TransactionStore{q: pool}
}

// txTransactionStore is a transaction-scoped store.
type txTransactionStore struct {
	tx querier
}

func (s *txTransactionStore) Create(ctx context.Context, tx *domain.Transaction) error {
	return txCreate(ctx, s.tx, tx)
}

func (s *txTransactionStore) GetByID(ctx context.Context, id uuid.UUID) (*domain.Transaction, error) {
	return txGetByID(ctx, s.tx, id)
}

func (s *txTransactionStore) GetByIDForUpdate(ctx context.Context, id uuid.UUID) (*domain.Transaction, error) {
	return txGetByIDForUpdate(ctx, s.tx, id)
}

func (s *txTransactionStore) GetByIdempotencyKey(ctx context.Context, payerUserID uuid.UUID, key string) (*domain.Transaction, error) {
	return txGetByIdempotencyKey(ctx, s.tx, payerUserID, key)
}

func (s *txTransactionStore) UpdateStatus(ctx context.Context, id uuid.UUID, status domain.Status) error {
	return txUpdateStatus(ctx, s.tx, id, status)
}

func (s *txTransactionStore) ListByUserID(ctx context.Context, userID uuid.UUID) ([]*domain.Transaction, error) {
	return txListByUserID(ctx, s.tx, userID)
}

// Pool-backed methods delegate to helpers.

// Create inserts a new transaction.
func (s *TransactionStore) Create(ctx context.Context, tx *domain.Transaction) error {
	return txCreate(ctx, s.q, tx)
}

// GetByID fetches a transaction by its primary key.
func (s *TransactionStore) GetByID(ctx context.Context, id uuid.UUID) (*domain.Transaction, error) {
	return txGetByID(ctx, s.q, id)
}

// GetByIDForUpdate fetches a transaction with SELECT ... FOR UPDATE.
func (s *TransactionStore) GetByIDForUpdate(ctx context.Context, id uuid.UUID) (*domain.Transaction, error) {
	return txGetByIDForUpdate(ctx, s.q, id)
}

// GetByIdempotencyKey fetches by (payer_user_id, idempotency_key).
func (s *TransactionStore) GetByIdempotencyKey(ctx context.Context, payerUserID uuid.UUID, key string) (*domain.Transaction, error) {
	return txGetByIdempotencyKey(ctx, s.q, payerUserID, key)
}

// UpdateStatus updates the status of a transaction.
func (s *TransactionStore) UpdateStatus(ctx context.Context, id uuid.UUID, status domain.Status) error {
	return txUpdateStatus(ctx, s.q, id, status)
}

// ListByUserID returns all transactions for a given user.
func (s *TransactionStore) ListByUserID(ctx context.Context, userID uuid.UUID) ([]*domain.Transaction, error) {
	return txListByUserID(ctx, s.q, userID)
}

// --- helpers shared by pool and tx stores ---

// pgUniqueViolation is the Postgres unique constraint violation SQLSTATE code.
const pgUniqueViolation = "23505"

func txCreate(ctx context.Context, q querier, tx *domain.Transaction) error {
	const insertQuery = `
INSERT INTO transactions
    (id, payer_user_id, payee_user_id, contract_id, milestone_id, amount, currency, status, idempotency_key, created_at, updated_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
`

	_, err := q.Exec(
		ctx, insertQuery,
		tx.ID, tx.PayerUserID, tx.PayeeUserID,
		tx.ContractID, tx.MilestoneID,
		tx.Amount.StringFixed(2), tx.Currency, string(tx.Status),
		tx.IdempotencyKey, tx.CreatedAt, tx.UpdatedAt,
	)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == pgUniqueViolation {
			return domain.ErrDuplicateKey
		}

		return fmt.Errorf("insert transaction: %w", err)
	}

	return nil
}

func txGetByID(ctx context.Context, q querier, id uuid.UUID) (*domain.Transaction, error) {
	const query = `
SELECT id, payer_user_id, payee_user_id, contract_id, milestone_id,
       amount, currency, status, idempotency_key, created_at, updated_at
FROM transactions
WHERE id = $1
`

	return scanTransaction(q.QueryRow(ctx, query, id))
}

func txGetByIDForUpdate(ctx context.Context, q querier, id uuid.UUID) (*domain.Transaction, error) {
	const query = `
SELECT id, payer_user_id, payee_user_id, contract_id, milestone_id,
       amount, currency, status, idempotency_key, created_at, updated_at
FROM transactions
WHERE id = $1
FOR UPDATE
`

	return scanTransaction(q.QueryRow(ctx, query, id))
}

func txGetByIdempotencyKey(ctx context.Context, q querier, payerUserID uuid.UUID, key string) (*domain.Transaction, error) {
	const query = `
SELECT id, payer_user_id, payee_user_id, contract_id, milestone_id,
       amount, currency, status, idempotency_key, created_at, updated_at
FROM transactions
WHERE payer_user_id = $1 AND idempotency_key = $2
`

	return scanTransaction(q.QueryRow(ctx, query, payerUserID, key))
}

func txUpdateStatus(ctx context.Context, q querier, id uuid.UUID, status domain.Status) error {
	const query = `
UPDATE transactions SET status = $2, updated_at = $3 WHERE id = $1
`

	tag, err := q.Exec(ctx, query, id, string(status), time.Now().UTC())
	if err != nil {
		return fmt.Errorf("update transaction status: %w", err)
	}

	if tag.RowsAffected() == 0 {
		return domain.ErrTransactionNotFound
	}

	return nil
}

func txListByUserID(ctx context.Context, q querier, userID uuid.UUID) ([]*domain.Transaction, error) {
	const query = `
SELECT id, payer_user_id, payee_user_id, contract_id, milestone_id,
       amount, currency, status, idempotency_key, created_at, updated_at
FROM transactions
WHERE payer_user_id = $1 OR payee_user_id = $1
ORDER BY created_at DESC
`

	rows, err := q.Query(ctx, query, userID)
	if err != nil {
		return nil, fmt.Errorf("list transactions: %w", err)
	}

	defer rows.Close()

	var txs []*domain.Transaction

	for rows.Next() {
		tx, scanErr := scanTransactionRow(rows)
		if scanErr != nil {
			return nil, scanErr
		}

		txs = append(txs, tx)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate transactions: %w", err)
	}

	return txs, nil
}

// rowScanner is satisfied by pgx.Row and pgx.Rows.
type rowScanner interface {
	Scan(dest ...any) error
}

func scanTransaction(row rowScanner) (*domain.Transaction, error) {
	var tx domain.Transaction
	var amountStr string
	var statusStr string

	err := row.Scan(
		&tx.ID, &tx.PayerUserID, &tx.PayeeUserID,
		&tx.ContractID, &tx.MilestoneID,
		&amountStr, &tx.Currency, &statusStr,
		&tx.IdempotencyKey, &tx.CreatedAt, &tx.UpdatedAt,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrTransactionNotFound
		}

		return nil, fmt.Errorf("scan transaction: %w", err)
	}

	amt, parseErr := decimal.NewFromString(amountStr)
	if parseErr != nil {
		return nil, fmt.Errorf("parse amount %q: %w", amountStr, parseErr)
	}

	tx.Amount = amt
	tx.Status = domain.Status(statusStr)

	return &tx, nil
}

func scanTransactionRow(rows pgx.Rows) (*domain.Transaction, error) {
	var tx domain.Transaction
	var amountStr string
	var statusStr string

	err := rows.Scan(
		&tx.ID, &tx.PayerUserID, &tx.PayeeUserID,
		&tx.ContractID, &tx.MilestoneID,
		&amountStr, &tx.Currency, &statusStr,
		&tx.IdempotencyKey, &tx.CreatedAt, &tx.UpdatedAt,
	)
	if err != nil {
		return nil, fmt.Errorf("scan transaction row: %w", err)
	}

	amt, parseErr := decimal.NewFromString(amountStr)
	if parseErr != nil {
		return nil, fmt.Errorf("parse amount %q: %w", amountStr, parseErr)
	}

	tx.Amount = amt
	tx.Status = domain.Status(statusStr)

	return &tx, nil
}
