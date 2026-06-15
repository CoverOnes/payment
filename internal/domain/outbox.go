package domain

import (
	"time"

	"github.com/google/uuid"
)

// OutboxEvent is a row in the event_outbox table.
// It is enqueued inside the same DB transaction as the business operation and
// delivered by the in-process poller (at-least-once; consumer MUST dedup on EventID).
//
// # Dedup strategy (outbox-level)
//
// EventID is a deterministic UUID derived from the business key:
//   - contract_activated: uuid.NewSHA1(NameSpaceURL, "outbox:plan_created:<plan_id>")
//   - contract_completed: uuid.NewSHA1(NameSpaceURL, "outbox:disburse:<plan_id>:<milestone_id>")
//
// The enqueue query uses ON CONFLICT (event_id) DO NOTHING, so a crash-retry that
// calls persistPlan or disburseMilestoneTx again inserts the same EventID and the
// duplicate row is silently dropped. Without deterministic EventIDs, every retry
// produces a new random UUID and the ON CONFLICT guard never fires.
//
// # Consumer-side dedup
//
// The settlement service methods (CreatePlan, DisburseMilestone) are independently
// idempotent via their own DB guards, so even if the same event is delivered twice
// (at-least-once guarantee), no double-disburse occurs.
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
