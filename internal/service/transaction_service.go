// Package service implements business logic for the payment service.
package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/CoverOnes/payment/internal/domain"
	"github.com/CoverOnes/payment/internal/events"
	"github.com/CoverOnes/payment/internal/store"
	"github.com/google/uuid"
	"github.com/shopspring/decimal"
)

// TransactionService handles transaction lifecycle business logic.
type TransactionService struct {
	txStore    store.TransactionStore
	auditStore store.AuditStore
	txManager  store.TxManager
	publisher  events.Publisher
}

// NewTransactionService returns a TransactionService.
func NewTransactionService(
	txStore store.TransactionStore,
	auditStore store.AuditStore,
	txManager store.TxManager,
	publisher events.Publisher,
) *TransactionService {
	return &TransactionService{
		txStore:    txStore,
		auditStore: auditStore,
		txManager:  txManager,
		publisher:  publisher,
	}
}

// CreateRequest carries the validated input for creating a transaction.
type CreateRequest struct {
	PayerUserID    uuid.UUID
	PayeeUserID    uuid.UUID
	ContractID     *uuid.UUID
	MilestoneID    *uuid.UUID
	Amount         decimal.Decimal
	Currency       string
	IdempotencyKey string
}

// Create creates a new transaction in HELD status (direct escrow hold).
// Idempotent via idempotency_key: if the key already exists, returns the existing transaction.
func (s *TransactionService) Create(ctx context.Context, req *CreateRequest) (*domain.Transaction, error) {
	if err := validateCreateRequest(req); err != nil {
		return nil, err
	}

	// Idempotency check: scope by payer to prevent cross-user key leaks.
	existing, err := s.txStore.GetByIdempotencyKey(ctx, req.PayerUserID, req.IdempotencyKey)
	if err != nil && !errors.Is(err, domain.ErrTransactionNotFound) {
		return nil, fmt.Errorf("check idempotency key: %w", err)
	}

	if existing != nil {
		return existing, nil
	}

	now := time.Now().UTC()
	tx := &domain.Transaction{
		ID:             uuid.New(),
		PayerUserID:    req.PayerUserID,
		PayeeUserID:    req.PayeeUserID,
		ContractID:     req.ContractID,
		MilestoneID:    req.MilestoneID,
		Amount:         req.Amount,
		Currency:       req.Currency,
		Status:         domain.StatusHeld, // direct escrow hold on create
		IdempotencyKey: req.IdempotencyKey,
		CreatedAt:      now,
		UpdatedAt:      now,
	}

	audit := &domain.TransactionAudit{
		ID:            uuid.New(),
		TransactionID: tx.ID,
		FromStatus:    domain.StatusPending,
		ToStatus:      domain.StatusHeld,
		ActorUserID:   req.PayerUserID,
		OccurredAt:    now,
	}

	err = s.txManager.WithTx(ctx, func(ctx context.Context, txs store.TransactionStore, audits store.AuditStore) error {
		if createErr := txs.Create(ctx, tx); createErr != nil {
			if errors.Is(createErr, domain.ErrDuplicateKey) {
				return createErr
			}

			return fmt.Errorf("create transaction: %w", createErr)
		}

		if auditErr := audits.Append(ctx, audit); auditErr != nil {
			return fmt.Errorf("append audit: %w", auditErr)
		}

		return nil
	})
	if err != nil {
		// Concurrent creation hit the unique constraint — fetch existing (scoped by payer).
		if errors.Is(err, domain.ErrDuplicateKey) {
			existing, fetchErr := s.txStore.GetByIdempotencyKey(ctx, req.PayerUserID, req.IdempotencyKey)
			if fetchErr != nil {
				return nil, fmt.Errorf("fetch after idempotency collision: %w", fetchErr)
			}

			return existing, nil
		}

		return nil, err
	}

	// Best-effort event publish (CONVENTIONS §14). Do not roll back on publish failure.
	s.publishStatusChanged(ctx, tx, domain.StatusPending, domain.StatusHeld, req.PayerUserID)

	return tx, nil
}

// Release transitions a transaction from HELD to RELEASED.
// actorUserID is the user triggering the release (must be validated by handler).
func (s *TransactionService) Release(ctx context.Context, id, actorUserID uuid.UUID) (*domain.Transaction, error) {
	return s.transition(ctx, id, actorUserID, domain.StatusReleased)
}

// Refund transitions a transaction from HELD to REFUNDED.
func (s *TransactionService) Refund(ctx context.Context, id, actorUserID uuid.UUID) (*domain.Transaction, error) {
	return s.transition(ctx, id, actorUserID, domain.StatusRefunded)
}

// transition executes a state transition inside a pgx transaction with SELECT FOR UPDATE.
// The current status is re-checked under the lock to prevent race conditions.
func (s *TransactionService) transition(ctx context.Context, id, actorUserID uuid.UUID, next domain.Status) (*domain.Transaction, error) {
	var updated *domain.Transaction

	now := time.Now().UTC()

	err := s.txManager.WithTx(ctx, func(ctx context.Context, txs store.TransactionStore, audits store.AuditStore) error {
		// SELECT ... FOR UPDATE: serialize concurrent transitions on the same row.
		current, lockErr := txs.GetByIDForUpdate(ctx, id)
		if lockErr != nil {
			return lockErr
		}

		// Ownership check under lock: return 404 (not 403) to prevent existence leaks (IDOR posture).
		if actorUserID != current.PayerUserID && actorUserID != current.PayeeUserID {
			return domain.ErrTransactionNotFound
		}

		from := current.Status

		// Re-validate the transition under lock.
		_, validErr := domain.Transition(from, next)
		if validErr != nil {
			return validErr
		}

		if updateErr := txs.UpdateStatus(ctx, id, next); updateErr != nil {
			return fmt.Errorf("update status: %w", updateErr)
		}

		audit := &domain.TransactionAudit{
			ID:            uuid.New(),
			TransactionID: id,
			FromStatus:    from,
			ToStatus:      next,
			ActorUserID:   actorUserID,
			OccurredAt:    now,
		}

		if auditErr := audits.Append(ctx, audit); auditErr != nil {
			return fmt.Errorf("append audit: %w", auditErr)
		}

		current.Status = next
		current.UpdatedAt = now
		updated = current

		return nil
	})
	if err != nil {
		return nil, err
	}

	// Best-effort event publish.
	prev := reversePrevious(next)
	s.publishStatusChanged(ctx, updated, prev, next, actorUserID)

	return updated, nil
}

// reversePrevious infers the previous status from the next for event publishing.
// This is only called after a successful transition, so the mapping is exact.
func reversePrevious(next domain.Status) domain.Status {
	switch next {
	case domain.StatusHeld:
		return domain.StatusPending
	case domain.StatusReleased, domain.StatusRefunded, domain.StatusFailed:
		return domain.StatusHeld
	default:
		return domain.StatusPending
	}
}

// GetByID returns a transaction by ID. Returns ErrTransactionNotFound if not found.
func (s *TransactionService) GetByID(ctx context.Context, id uuid.UUID) (*domain.Transaction, error) {
	return s.txStore.GetByID(ctx, id)
}

// ListByUserID returns all transactions where the user is payer or payee.
func (s *TransactionService) ListByUserID(ctx context.Context, userID uuid.UUID) ([]*domain.Transaction, error) {
	return s.txStore.ListByUserID(ctx, userID)
}

// publishStatusChanged publishes the event in the background using a detached context.
// ctx is accepted to satisfy the contextcheck linter but is intentionally NOT passed to
// the goroutine — the goroutine must outlive the request context (backend-security-design
// §goroutine: goroutines MUST NOT inherit request context for async fire-and-forget work).
// A fresh context.Background() + timeout is used inside the goroutine instead.
// The nolint below is required: goroutine uses context.Background() — inheriting request ctx
// would cancel the publish when the HTTP response completes (backend-security-design §goroutine).
func (s *TransactionService) publishStatusChanged( //nolint:contextcheck // intentional: detached goroutine must not inherit request context
	ctx context.Context,
	tx *domain.Transaction,
	from, to domain.Status,
	actorUserID uuid.UUID,
) {
	_ = ctx // ctx received for call-site ergonomics; goroutine creates a detached context
	evt := &domain.TransactionStatusChangedEvent{
		EventID:    uuid.New(),
		OccurredAt: time.Now().UTC(),
		Version:    1,
		Data: domain.TransactionStatusChangedData{
			TransactionID: tx.ID,
			PayerUserID:   tx.PayerUserID,
			PayeeUserID:   tx.PayeeUserID,
			ContractID:    tx.ContractID,
			MilestoneID:   tx.MilestoneID,
			Amount:        tx.Amount,
			Currency:      tx.Currency,
			FromStatus:    from,
			ToStatus:      to,
			ActorUserID:   actorUserID,
			At:            time.Now().UTC(),
		},
	}

	// Use a detached context with timeout so publish does not inherit the (now-done) request context.
	publishCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)

	go func() {
		defer cancel()

		if err := s.publisher.PublishTransactionStatusChanged(publishCtx, evt); err != nil {
			slog.Warn("publish transaction_status_changed failed (best-effort)", "err", err,
				"transaction_id", tx.ID, "from", from, "to", to)
		}
	}()
}

// maxIdempotencyKeyLen is the maximum allowed length for an idempotency key.
const maxIdempotencyKeyLen = 255

// maxAmount is the largest amount accepted per transaction (100 million).
var maxAmount = decimal.NewFromFloat(100_000_000.00)

// allowedCurrencies is the explicit allowlist for currency codes (ISO 4217 subset).
var allowedCurrencies = map[string]bool{
	"TWD": true,
	"USD": true,
	"EUR": true,
}

func validateCreateRequest(req *CreateRequest) error {
	if req.PayerUserID == uuid.Nil {
		return fmt.Errorf("%w: payer_user_id is required", domain.ErrValidation)
	}

	if req.PayeeUserID == uuid.Nil {
		return fmt.Errorf("%w: payee_user_id is required", domain.ErrValidation)
	}

	if req.PayerUserID == req.PayeeUserID {
		return fmt.Errorf("%w: payer and payee must be different users", domain.ErrValidation)
	}

	if req.Amount.LessThanOrEqual(decimal.Zero) {
		return fmt.Errorf("%w: amount must be positive", domain.ErrValidation)
	}

	if req.Amount.GreaterThan(maxAmount) {
		return fmt.Errorf("%w: amount must not exceed 100000000.00", domain.ErrValidation)
	}

	if req.Currency == "" {
		return fmt.Errorf("%w: currency is required", domain.ErrValidation)
	}

	if !allowedCurrencies[req.Currency] {
		return fmt.Errorf("%w: currency must be one of TWD, USD, EUR", domain.ErrValidation)
	}

	if req.IdempotencyKey == "" {
		return fmt.Errorf("%w: idempotency_key is required", domain.ErrValidation)
	}

	if len(req.IdempotencyKey) > maxIdempotencyKeyLen {
		return fmt.Errorf("%w: idempotency_key must not exceed %d characters", domain.ErrValidation, maxIdempotencyKeyLen)
	}

	return nil
}
