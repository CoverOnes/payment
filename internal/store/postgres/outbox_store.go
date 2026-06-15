package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/CoverOnes/payment/internal/domain"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	// outboxMaxBackoff caps the exponential backoff at 10 minutes.
	outboxMaxBackoff = 10 * time.Minute

	// outboxClaimDuration is how long a claimed row is invisible to other pollers.
	// After this window expires (e.g. poller crashed before MarkPublished) the row
	// becomes eligible again for the next poll cycle.
	outboxClaimDuration = 30 * time.Second
)

// OutboxStore is a pool-backed outbox store.
type OutboxStore struct {
	q querier
}

// NewOutboxStore returns an OutboxStore backed by pool.
func NewOutboxStore(pool *pgxpool.Pool) *OutboxStore {
	return &OutboxStore{q: pool}
}

// txOutboxStore is a transaction-scoped OutboxStore used inside WithSettlementTx.
type txOutboxStore struct {
	tx querier
}

func (s *txOutboxStore) Enqueue(ctx context.Context, e *domain.OutboxEvent) error {
	return enqueueOutboxEvent(ctx, s.tx, e)
}

func (s *txOutboxStore) PollReady(ctx context.Context, limit int) ([]*domain.OutboxEvent, error) {
	return pollReadyOutboxEvents(ctx, s.tx, limit)
}

func (s *txOutboxStore) MarkPublished(ctx context.Context, id uuid.UUID) error {
	return markOutboxPublished(ctx, s.tx, id)
}

func (s *txOutboxStore) MarkFailed(ctx context.Context, id uuid.UUID, lastErr string) error {
	return markOutboxFailed(ctx, s.tx, id, lastErr)
}

func (s *txOutboxStore) DeletePublishedBefore(ctx context.Context, cutoff time.Time) (int64, error) {
	return deleteOutboxPublishedBefore(ctx, s.tx, cutoff)
}

// --- pool-backed methods ---

// Enqueue inserts a new outbox event row.
func (s *OutboxStore) Enqueue(ctx context.Context, e *domain.OutboxEvent) error {
	return enqueueOutboxEvent(ctx, s.q, e)
}

// PollReady atomically claims up to limit unpublished rows by setting
// claimed_until = now() + outboxClaimDuration via a single UPDATE...RETURNING CTE.
// Concurrent pollers skip rows already claimed (claimed_until > now()), preventing
// duplicate delivery within the claim window.
func (s *OutboxStore) PollReady(ctx context.Context, limit int) ([]*domain.OutboxEvent, error) {
	return pollReadyOutboxEvents(ctx, s.q, limit)
}

// MarkPublished sets published_at = now() and clears claimed_until for the given row.
func (s *OutboxStore) MarkPublished(ctx context.Context, id uuid.UUID) error {
	return markOutboxPublished(ctx, s.q, id)
}

// MarkFailed increments attempts, records last_error, advances next_attempt_at,
// and clears claimed_until so the row is visible to the next poll cycle.
func (s *OutboxStore) MarkFailed(ctx context.Context, id uuid.UUID, lastErr string) error {
	return markOutboxFailed(ctx, s.q, id, lastErr)
}

// DeletePublishedBefore removes published rows older than cutoff. Returns the number of rows deleted.
func (s *OutboxStore) DeletePublishedBefore(ctx context.Context, cutoff time.Time) (int64, error) {
	return deleteOutboxPublishedBefore(ctx, s.q, cutoff)
}

// --- shared helpers ---

func enqueueOutboxEvent(ctx context.Context, q querier, e *domain.OutboxEvent) error {
	const query = `
INSERT INTO event_outbox
	(id, aggregate_type, aggregate_id, event_id, channel, payload, created_at, next_attempt_at)
VALUES
	($1, $2, $3, $4, $5, $6, $7, $8)
ON CONFLICT (event_id) DO NOTHING
`

	_, err := q.Exec(
		ctx, query,
		e.ID, e.AggregateType, e.AggregateID, e.EventID,
		e.Channel, e.Payload, e.CreatedAt, e.NextAttemptAt,
	)
	if err != nil {
		return fmt.Errorf("enqueue outbox event: %w", err)
	}

	return nil
}

// pollReadyOutboxEvents uses a CTE to atomically claim rows by setting
// claimed_until = now() + outboxClaimDuration in the same statement that reads
// them.  Only rows where claimed_until IS NULL OR claimed_until < now() are
// eligible, so a second concurrent poller will skip in-flight rows even though
// the SELECT and UPDATE happen outside an explicit long-lived transaction.
func pollReadyOutboxEvents(ctx context.Context, q querier, limit int) ([]*domain.OutboxEvent, error) {
	if limit <= 0 {
		limit = 10
	}

	const query = `
WITH claimed AS (
    UPDATE event_outbox
    SET claimed_until = now() + $2
    WHERE id IN (
        SELECT id
        FROM event_outbox
        WHERE published_at IS NULL
          AND next_attempt_at <= now()
          AND (claimed_until IS NULL OR claimed_until < now())
        ORDER BY next_attempt_at ASC, created_at ASC
        LIMIT $1
        FOR UPDATE SKIP LOCKED
    )
    RETURNING id, aggregate_type, aggregate_id, event_id, channel, payload,
              created_at, published_at, attempts, last_error, next_attempt_at, claimed_until
)
SELECT * FROM claimed
ORDER BY next_attempt_at ASC, created_at ASC
`

	rows, err := q.Query(ctx, query, limit, outboxClaimDuration)
	if err != nil {
		return nil, fmt.Errorf("poll outbox: %w", err)
	}

	defer rows.Close()

	var out []*domain.OutboxEvent

	for rows.Next() {
		e, scanErr := scanOutboxEvent(rows)
		if scanErr != nil {
			return nil, scanErr
		}

		out = append(out, e)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate outbox rows: %w", err)
	}

	return out, nil
}

// markOutboxPublished sets published_at = now() and clears claimed_until.
func markOutboxPublished(ctx context.Context, q querier, id uuid.UUID) error {
	const query = `
UPDATE event_outbox
SET published_at  = now(),
    claimed_until = NULL
WHERE id = $1
  AND published_at IS NULL
`

	_, err := q.Exec(ctx, query, id)
	if err != nil {
		return fmt.Errorf("mark outbox published: %w", err)
	}

	return nil
}

// markOutboxFailed increments attempts, records last_error, advances next_attempt_at
// with exponential backoff, and clears claimed_until so the row is eligible again.
func markOutboxFailed(ctx context.Context, q querier, id uuid.UUID, lastErr string) error {
	// Exponential backoff: 2^attempts seconds, capped at outboxMaxBackoff.
	const query = `
UPDATE event_outbox
SET attempts        = attempts + 1,
    last_error      = $2,
    next_attempt_at = now() + make_interval(secs => LEAST(
                          POWER(2, attempts)::double precision,
                          $3::double precision
                      )),
    claimed_until   = NULL
WHERE id = $1
  AND published_at IS NULL
`

	_, err := q.Exec(ctx, query, id, lastErr, outboxMaxBackoff.Seconds())
	if err != nil {
		return fmt.Errorf("mark outbox failed: %w", err)
	}

	return nil
}

func deleteOutboxPublishedBefore(ctx context.Context, q querier, cutoff time.Time) (int64, error) {
	const query = `
DELETE FROM event_outbox
WHERE published_at IS NOT NULL
  AND published_at < $1
`

	tag, err := q.Exec(ctx, query, cutoff)
	if err != nil {
		return 0, fmt.Errorf("delete published outbox rows: %w", err)
	}

	return tag.RowsAffected(), nil
}

func scanOutboxEvent(row rowScanner) (*domain.OutboxEvent, error) {
	var e domain.OutboxEvent

	err := row.Scan(
		&e.ID, &e.AggregateType, &e.AggregateID, &e.EventID,
		&e.Channel, &e.Payload,
		&e.CreatedAt, &e.PublishedAt,
		&e.Attempts, &e.LastError, &e.NextAttemptAt, &e.ClaimedUntil,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil //nolint:nilnil // pgx.ErrNoRows means no row; nil outbox event is not an error
		}

		return nil, fmt.Errorf("scan outbox event: %w", err)
	}

	return &e, nil
}
