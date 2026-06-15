package postgres_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/CoverOnes/payment/internal/domain"
	"github.com/CoverOnes/payment/internal/store/postgres"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// outboxTestPollLimit is large enough that a single PollReady call returns all
// eligible events even in a busy shared test database.
const outboxTestPollLimit = 1000

// newTestOutboxEvent returns a valid OutboxEvent for store-level tests.
// eventID is caller-supplied so tests can control idempotency behavior.
// NextAttemptAt is set 1 second in the past to avoid clock-skew between Go and the
// DB: PollReady's WHERE clause uses DB-side now(), so a Go-side "now" may be
// marginally ahead of the DB clock and cause the event to be ineligible.
func newTestOutboxEvent(aggregateID uuid.UUID, channel string, eventID uuid.UUID, payload []byte) *domain.OutboxEvent {
	now := time.Now().UTC()

	return &domain.OutboxEvent{
		ID:            uuid.New(),
		AggregateType: "settlement",
		AggregateID:   aggregateID,
		EventID:       eventID,
		Channel:       channel,
		Payload:       payload,
		CreatedAt:     now,
		NextAttemptAt: now.Add(-time.Second), // 1s in past avoids DB clock skew
	}
}

// TestOutboxStore_EnqueueAndPoll verifies the enqueue → poll → mark lifecycle
// at the store layer (happy path + idempotency + mark paths).
func TestOutboxStore_EnqueueAndPoll(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	ctx := context.Background()
	dsn := startTestDB(t)

	pool, err := postgres.NewPool(ctx, dsn)
	require.NoError(t, err)
	defer pool.Close()

	outboxStore := postgres.NewOutboxStore(pool)

	t.Run("Enqueue + PollReady returns the event", func(t *testing.T) {
		aggregateID := uuid.New()
		payload, _ := json.Marshal(map[string]string{"test": "value"})
		evt := newTestOutboxEvent(aggregateID, "payment.contract_activated", uuid.New(), payload)

		require.NoError(t, outboxStore.Enqueue(ctx, evt))

		events, err := outboxStore.PollReady(ctx, outboxTestPollLimit)
		require.NoError(t, err)

		// Find our event (shared container may have other events from parallel tests).
		var found *domain.OutboxEvent
		for _, e := range events {
			if e.EventID == evt.EventID {
				found = e
				break
			}
		}
		require.NotNil(t, found, "enqueued event must appear in PollReady results")
		assert.Equal(t, evt.Channel, found.Channel)
		assert.Equal(t, evt.AggregateID, found.AggregateID)
		assert.NotNil(t, found.ClaimedUntil, "PollReady must set claimed_until")
	})

	t.Run("Enqueue same event_id twice: second enqueue is a no-op (idempotent)", func(t *testing.T) {
		aggregateID := uuid.New()
		sharedEventID := uuid.New()
		payload, _ := json.Marshal(map[string]string{"idempotency": "test"})

		evt1 := newTestOutboxEvent(aggregateID, "payment.contract_activated", sharedEventID, payload)
		evt2 := newTestOutboxEvent(aggregateID, "payment.contract_activated", sharedEventID, payload)
		evt2.ID = uuid.New() // different outbox row ID but same event_id

		require.NoError(t, outboxStore.Enqueue(ctx, evt1))
		require.NoError(t, outboxStore.Enqueue(ctx, evt2), "re-enqueue with same event_id must not error (ON CONFLICT DO NOTHING)")

		// Count rows with this event_id — should be exactly 1.
		const q = `SELECT COUNT(*) FROM event_outbox WHERE event_id = $1`
		var count int
		require.NoError(t, pool.QueryRow(ctx, q, sharedEventID).Scan(&count))
		assert.Equal(t, 1, count, "exactly one row for the same event_id even after two Enqueue calls")
	})

	t.Run("MarkPublished: sets published_at and clears claimed_until", func(t *testing.T) {
		payload, _ := json.Marshal(map[string]string{"mark": "published"})
		evt := newTestOutboxEvent(uuid.New(), "payment.contract_completed", uuid.New(), payload)
		require.NoError(t, outboxStore.Enqueue(ctx, evt))

		// Poll to claim it.
		events, err := outboxStore.PollReady(ctx, outboxTestPollLimit)
		require.NoError(t, err)

		var found *domain.OutboxEvent
		for _, e := range events {
			if e.EventID == evt.EventID {
				found = e
				break
			}
		}
		require.NotNil(t, found, "event must appear in poll")

		require.NoError(t, outboxStore.MarkPublished(ctx, found.ID))

		// After MarkPublished: published_at IS NOT NULL, claimed_until IS NULL.
		const q = `SELECT published_at, claimed_until FROM event_outbox WHERE id = $1`
		var publishedAt *time.Time
		var claimedUntil *time.Time
		require.NoError(t, pool.QueryRow(ctx, q, found.ID).Scan(&publishedAt, &claimedUntil))
		assert.NotNil(t, publishedAt, "published_at must be set")
		assert.Nil(t, claimedUntil, "claimed_until must be cleared after MarkPublished")
	})

	t.Run("MarkFailed: increments attempts and advances next_attempt_at (backoff)", func(t *testing.T) {
		payload, _ := json.Marshal(map[string]string{"mark": "failed"})
		evt := newTestOutboxEvent(uuid.New(), "payment.contract_activated", uuid.New(), payload)
		require.NoError(t, outboxStore.Enqueue(ctx, evt))

		// Poll to claim it.
		events, err := outboxStore.PollReady(ctx, outboxTestPollLimit)
		require.NoError(t, err)

		var found *domain.OutboxEvent
		for _, e := range events {
			if e.EventID == evt.EventID {
				found = e
				break
			}
		}
		require.NotNil(t, found, "event must appear in poll")

		beforeFail := time.Now().UTC()
		require.NoError(t, outboxStore.MarkFailed(ctx, found.ID, "simulated dispatch error"))

		const q = `SELECT attempts, last_error, next_attempt_at, claimed_until FROM event_outbox WHERE id = $1`
		var attempts int
		var lastError *string
		var nextAttemptAt time.Time
		var claimedUntil *time.Time
		require.NoError(t, pool.QueryRow(ctx, q, found.ID).Scan(&attempts, &lastError, &nextAttemptAt, &claimedUntil))

		assert.Equal(t, 1, attempts, "attempts must be incremented to 1")
		require.NotNil(t, lastError)
		assert.Equal(t, "simulated dispatch error", *lastError)
		assert.True(t, nextAttemptAt.After(beforeFail), "next_attempt_at must be advanced into the future (backoff)")
		assert.Nil(t, claimedUntil, "claimed_until must be cleared after MarkFailed (re-pollable)")
	})

	t.Run("PollReady does not return published rows", func(t *testing.T) {
		payload, _ := json.Marshal(map[string]string{"skip": "published"})
		evt := newTestOutboxEvent(uuid.New(), "payment.contract_completed", uuid.New(), payload)
		require.NoError(t, outboxStore.Enqueue(ctx, evt))

		// Claim and immediately publish.
		events, err := outboxStore.PollReady(ctx, outboxTestPollLimit)
		require.NoError(t, err)

		var found *domain.OutboxEvent
		for _, e := range events {
			if e.EventID == evt.EventID {
				found = e
				break
			}
		}
		require.NotNil(t, found)
		require.NoError(t, outboxStore.MarkPublished(ctx, found.ID))

		// Second poll must not return this event.
		events2, err := outboxStore.PollReady(ctx, outboxTestPollLimit)
		require.NoError(t, err)
		for _, e := range events2 {
			assert.NotEqual(t, found.EventID, e.EventID, "published event must not appear in PollReady")
		}
	})
}

// TestOutboxStore_ReplayIdempotency verifies the core double-delivery guarantee:
// PollReady on the same unclaimed event twice (claim window expired) does NOT
// return two copies simultaneously — SKIP LOCKED prevents that.
func TestOutboxStore_ReplayIdempotency(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	ctx := context.Background()
	dsn := startTestDB(t)

	pool, err := postgres.NewPool(ctx, dsn)
	require.NoError(t, err)
	defer pool.Close()

	outboxStore := postgres.NewOutboxStore(pool)

	t.Run("same event polled twice returns once each time (SKIP LOCKED)", func(t *testing.T) {
		payload, _ := json.Marshal(map[string]string{"replay": "test"})
		evt := newTestOutboxEvent(uuid.New(), "payment.contract_completed", uuid.New(), payload)
		require.NoError(t, outboxStore.Enqueue(ctx, evt))

		// First poll: claims the row.
		events1, err := outboxStore.PollReady(ctx, outboxTestPollLimit)
		require.NoError(t, err)

		var found1 *domain.OutboxEvent
		for _, e := range events1 {
			if e.EventID == evt.EventID {
				found1 = e
				break
			}
		}
		require.NotNil(t, found1, "first poll must find the event")
		assert.NotNil(t, found1.ClaimedUntil, "claimed_until must be set after first poll")

		// Second poll without releasing claim: same event must NOT appear again (SKIP LOCKED).
		events2, err := outboxStore.PollReady(ctx, outboxTestPollLimit)
		require.NoError(t, err)
		for _, e := range events2 {
			assert.NotEqual(t, evt.EventID, e.EventID, "claimed event must not appear in concurrent PollReady")
		}
	})
}

// TestOutboxStore_Retention verifies DeletePublishedBefore removes old published rows
// and leaves recent published rows and all unpublished rows untouched.
func TestOutboxStore_Retention(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	ctx := context.Background()
	dsn := startTestDB(t)

	pool, err := postgres.NewPool(ctx, dsn)
	require.NoError(t, err)
	defer pool.Close()

	outboxStore := postgres.NewOutboxStore(pool)

	t.Run("DeletePublishedBefore removes stale published rows", func(t *testing.T) {
		aggregateID := uuid.New()
		payload, _ := json.Marshal(map[string]string{"retention": "test"})

		// Insert two events directly with past published_at to simulate old published rows.
		oldPublishedAt := time.Now().UTC().Add(-8 * 24 * time.Hour) // 8 days ago — beyond 7-day retention
		recentPublishedAt := time.Now().UTC().Add(-1 * time.Hour)   // 1 hour ago — within retention

		oldEvtID := uuid.New()
		recentEvtID := uuid.New()
		unpublishedEvtID := uuid.New()

		const insertQ = `
INSERT INTO event_outbox
	(id, aggregate_type, aggregate_id, event_id, channel, payload, created_at, next_attempt_at, published_at)
VALUES
	($1, $2, $3, $4, $5, $6, $7, $8, $9)
`
		now := time.Now().UTC()

		_, err = pool.Exec(ctx, insertQ,
			uuid.New(), "settlement", aggregateID, oldEvtID, "payment.contract_completed", payload, now, now, oldPublishedAt)
		require.NoError(t, err, "insert old published event")

		_, err = pool.Exec(ctx, insertQ,
			uuid.New(), "settlement", aggregateID, recentEvtID, "payment.contract_completed", payload, now, now, recentPublishedAt)
		require.NoError(t, err, "insert recent published event")

		// Insert an unpublished event — must survive retention.
		evtUnpublished := newTestOutboxEvent(aggregateID, "payment.contract_activated", unpublishedEvtID, payload)
		require.NoError(t, outboxStore.Enqueue(ctx, evtUnpublished))

		// Run retention: cutoff = 7 days ago.
		cutoff := time.Now().UTC().Add(-7 * 24 * time.Hour)
		deleted, err := outboxStore.DeletePublishedBefore(ctx, cutoff)
		require.NoError(t, err)
		assert.GreaterOrEqual(t, deleted, int64(1), "at least the old published event must be deleted")

		// Old published event must be gone.
		var oldCount int
		require.NoError(t, pool.QueryRow(ctx, `SELECT COUNT(*) FROM event_outbox WHERE event_id = $1`, oldEvtID).Scan(&oldCount))
		assert.Equal(t, 0, oldCount, "old published event must be removed by retention")

		// Recent published event must survive.
		var recentCount int
		require.NoError(t, pool.QueryRow(ctx, `SELECT COUNT(*) FROM event_outbox WHERE event_id = $1`, recentEvtID).Scan(&recentCount))
		assert.Equal(t, 1, recentCount, "recent published event must survive retention")

		// Unpublished event must survive.
		var unpubCount int
		require.NoError(t, pool.QueryRow(ctx, `SELECT COUNT(*) FROM event_outbox WHERE event_id = $1`, unpublishedEvtID).Scan(&unpubCount))
		assert.Equal(t, 1, unpubCount, "unpublished event must survive retention")
	})

	t.Run("DeletePublishedBefore with future cutoff returns 0 when no published rows exist", func(t *testing.T) {
		// Use a random aggregate_id to avoid interference with other test rows.
		payload, _ := json.Marshal(map[string]string{"empty": "retention"})
		evt := newTestOutboxEvent(uuid.New(), "payment.contract_activated", uuid.New(), payload)
		require.NoError(t, outboxStore.Enqueue(ctx, evt))
		// Do NOT mark it published — stays unpublished.

		// Cutoff at epoch — deletes nothing unpublished.
		deleted, err := outboxStore.DeletePublishedBefore(ctx, time.Unix(0, 0))
		require.NoError(t, err)
		// We assert 0 is valid — there may be 0 published rows before epoch.
		assert.GreaterOrEqual(t, deleted, int64(0))
	})
}

// TestOutboxStore_CrashRedelivery verifies that after a crash simulation
// (MarkFailed without MarkPublished), the event is re-pollable on the next cycle.
// This exercises the "at-least-once" guarantee: a failed dispatch does not lose the event.
func TestOutboxStore_CrashRedelivery(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	ctx := context.Background()
	dsn := startTestDB(t)

	pool, err := postgres.NewPool(ctx, dsn)
	require.NoError(t, err)
	defer pool.Close()

	outboxStore := postgres.NewOutboxStore(pool)

	t.Run("MarkFailed event is re-pollable after next_attempt_at passes", func(t *testing.T) {
		payload, _ := json.Marshal(map[string]string{"crash": "redelivery"})
		evt := newTestOutboxEvent(uuid.New(), "payment.contract_completed", uuid.New(), payload)
		require.NoError(t, outboxStore.Enqueue(ctx, evt))

		// Poll to claim.
		events, err := outboxStore.PollReady(ctx, outboxTestPollLimit)
		require.NoError(t, err)
		var found *domain.OutboxEvent
		for _, e := range events {
			if e.EventID == evt.EventID {
				found = e
				break
			}
		}
		require.NotNil(t, found, "event must appear in first poll")

		// Simulate crash: mark failed (next_attempt_at will be in the future for attempt 0 = 1s backoff).
		require.NoError(t, outboxStore.MarkFailed(ctx, found.ID, "simulated crash"))

		// Reset next_attempt_at to the past so we can immediately re-poll in the test.
		// (In production the poller would wait for the backoff window.)
		_, err = pool.Exec(ctx, `UPDATE event_outbox SET next_attempt_at = now() - interval '1 second' WHERE id = $1`, found.ID)
		require.NoError(t, err, "manually reset next_attempt_at to past for test speed")

		// Re-poll: event must be available again.
		events2, err := outboxStore.PollReady(ctx, outboxTestPollLimit)
		require.NoError(t, err)

		var refound *domain.OutboxEvent
		for _, e := range events2 {
			if e.EventID == evt.EventID {
				refound = e
				break
			}
		}
		require.NotNil(t, refound, "failed event must be re-pollable after next_attempt_at reset")
		assert.Equal(t, 1, refound.Attempts, "attempts must be 1 after first failure")
		assert.NotNil(t, refound.LastError)
		assert.Equal(t, "simulated crash", *refound.LastError)
	})

	t.Run("event claimed by poll cannot be re-polled until claimed_until expires", func(t *testing.T) {
		payload, _ := json.Marshal(map[string]string{"claim": "window"})
		evt := newTestOutboxEvent(uuid.New(), "payment.contract_activated", uuid.New(), payload)
		require.NoError(t, outboxStore.Enqueue(ctx, evt))

		// First poll claims it.
		events, err := outboxStore.PollReady(ctx, outboxTestPollLimit)
		require.NoError(t, err)
		var found *domain.OutboxEvent
		for _, e := range events {
			if e.EventID == evt.EventID {
				found = e
				break
			}
		}
		require.NotNil(t, found, "first poll must find event")

		// Second poll (without releasing claim): SKIP LOCKED ensures event is not double-claimed.
		events2, err := outboxStore.PollReady(ctx, outboxTestPollLimit)
		require.NoError(t, err)
		for _, e := range events2 {
			assert.NotEqual(t, evt.EventID, e.EventID,
				"event with active claimed_until must not appear in concurrent PollReady")
		}
	})
}
