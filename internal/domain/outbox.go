package domain

import (
	"time"

	"github.com/google/uuid"
)

// OutboxEvent is a row in the event_outbox table.
// It is enqueued inside the same DB transaction as the business operation and
// delivered by the in-process poller (at-least-once; consumer MUST dedup on EventID).
//
// Consumer-side dedup: EventID is a unique key. Consumers must record seen event_ids
// in their own idempotency table and skip duplicates. The outbox guarantees
// at-least-once delivery; exactly-once is NOT guaranteed (a crash between publish
// and mark can cause a re-deliver).
//
// ClaimedUntil is set atomically by PollReady to prevent concurrent pollers from
// picking up the same row. MarkPublished and MarkFailed both clear it.
type OutboxEvent struct {
	ID            uuid.UUID
	AggregateType string
	AggregateID   uuid.UUID
	EventID       uuid.UUID
	Channel       string
	Payload       []byte
	CreatedAt     time.Time
	PublishedAt   *time.Time
	Attempts      int
	LastError     *string
	NextAttemptAt time.Time
	ClaimedUntil  *time.Time
}
