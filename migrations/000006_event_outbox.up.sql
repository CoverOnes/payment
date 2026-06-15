-- Migration 000006: transactional outbox for payment settlement lifecycle events.
-- NO FOREIGN KEY constraints platform-wide (CONVENTIONS §11 / CLAUDE.md #9).
-- Referential integrity enforced at the service layer on enqueue.
--
-- Design:
--   * Same-transaction enqueue: INSERT into event_outbox inside the same DB tx as
--     the business operation so events are never lost on server restart between
--     commit and publish.
--   * Atomic-claim poller: FetchPending uses a CTE that atomically sets
--     claimed_until = now() + 30s for rows it claims, so concurrent pollers skip
--     rows already in-flight. claimed_until is cleared by MarkPublished /
--     MarkFailed, making the row eligible for the next poll cycle.
--   * Retention: published rows deleted after 7 days by the poller housekeeping
--     pass. Unpublished rows are NEVER deleted by retention — manual reconciliation
--     required if they age > 1 hour (slog.Warn alert in poller).
--   * Consumer-side dedup: event_id is a UUID unique key. Consumers MUST check
--     event_id for idempotency; at-least-once delivery is guaranteed, exactly-once
--     is NOT (duplicate publishes can occur after crash between publish and mark).
--   * Payment idempotency: the poller consumer uses (aggregate_id, channel) as the
--     idempotency key so replaying the same settlement event does NOT double-disburse.
--     settlement_milestone_disbursements ON CONFLICT DO NOTHING is the DB-level guard.

CREATE TABLE event_outbox (
    id              uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    aggregate_type  text        NOT NULL CHECK (char_length(aggregate_type) BETWEEN 1 AND 127),
    aggregate_id    uuid        NOT NULL,
    event_id        uuid        NOT NULL,
    channel         text        NOT NULL CHECK (char_length(channel) BETWEEN 1 AND 255),
    payload         bytea       NOT NULL,
    created_at      timestamptz NOT NULL DEFAULT now(),
    published_at    timestamptz,
    attempts        int         NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    last_error      text,
    next_attempt_at timestamptz NOT NULL DEFAULT now(),
    claimed_until   timestamptz,
    CONSTRAINT event_outbox_event_id_unique UNIQUE (event_id)
);

-- Covering index for the poller: unpublished rows eligible for delivery, ordered
-- by next_attempt_at then created_at (fairness). The partial WHERE clause matches
-- the poller's WHERE published_at IS NULL condition exactly, keeping the index small.
CREATE INDEX event_outbox_poll_idx
    ON event_outbox (next_attempt_at, created_at)
    WHERE published_at IS NULL;
