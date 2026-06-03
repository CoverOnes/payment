CREATE EXTENSION IF NOT EXISTS pgcrypto; -- gen_random_uuid()

-- NO FOREIGN KEY constraints platform-wide (CONVENTIONS §11 / CLAUDE.md #9).
-- Referential integrity enforced in service layer.

-- Money representation: numeric(14,2) stores minor units in TWD cents.
-- The Go layer uses shopspring/decimal for all arithmetic — never float64.
-- PENDING: transaction created, awaiting escrow hold
-- HELD:    funds held in escrow
-- RELEASED: funds released to payee
-- REFUNDED: funds returned to payer
-- FAILED:  terminal failure state

CREATE TABLE transactions (
    id               uuid          PRIMARY KEY DEFAULT gen_random_uuid(),
    payer_user_id    uuid          NOT NULL,
    payee_user_id    uuid          NOT NULL,
    contract_id      uuid,             -- soft ref to workspace contract (nullable)
    milestone_id     uuid,             -- soft ref to milestone (nullable)
    amount           numeric(14,2) NOT NULL CHECK (amount > 0),
    currency         char(3)       NOT NULL DEFAULT 'TWD',
    status           text          NOT NULL DEFAULT 'PENDING' CHECK (status IN ('PENDING','HELD','RELEASED','REFUNDED','FAILED')),
    idempotency_key  text          NOT NULL,
    created_at       timestamptz   NOT NULL DEFAULT now(),
    updated_at       timestamptz   NOT NULL DEFAULT now()
);

-- Idempotency key is unique per payer (not globally) to prevent cross-user key leaks.
-- A payer retrying the same request gets the same tx; different payers with the same
-- key string create two distinct transactions (no victim tx leak).
CREATE UNIQUE INDEX transactions_payer_idempotency_key_idx ON transactions (payer_user_id, idempotency_key);

-- Fast lookup by payer and payee for list queries.
CREATE INDEX transactions_payer_user_id_idx ON transactions (payer_user_id, created_at DESC);
CREATE INDEX transactions_payee_user_id_idx ON transactions (payee_user_id, created_at DESC);

-- Append-only audit: one row per state transition.
-- actor_user_id is the user who triggered the transition (payer, payee, or system).
--
-- Partitioned by RANGE (occurred_at) for high-volume write throughput and efficient
-- retention pruning (drop old monthly partitions instead of row-by-row DELETE).
--
-- Retention: 730 days (2-year compliance window).
-- Managed by Taskfile partition:drop-old target — drops monthly child tables whose
-- upper bound is older than now() - interval '730 days'.
--
-- PK includes occurred_at because PG requires all partitioned-table unique indexes
-- to include the partition key column.
--
-- transactions is intentionally NON-partitioned. Hash-partitioning is a future
-- follow-up blocked by UNIQUE(payer_user_id, idempotency_key): PG requires unique
-- indexes on partitioned tables to include the partition key, which would force
-- (payer_user_id, idempotency_key, <shard_col>) — a schema change requiring a
-- new migration and application changes.
CREATE TABLE transaction_audit (
    id             uuid        NOT NULL DEFAULT gen_random_uuid(),
    transaction_id uuid        NOT NULL,
    from_status    text        NOT NULL,
    to_status      text        NOT NULL,
    actor_user_id  uuid        NOT NULL,
    occurred_at    timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (id, occurred_at)
) PARTITION BY RANGE (occurred_at);

-- Index on the parent propagates to all partitions automatically.
CREATE INDEX transaction_audit_transaction_id_idx ON transaction_audit (transaction_id, occurred_at DESC);

-- Bootstrap partitions: prior month, current month, and next 3 months.
-- A DEFAULT partition catches any occurred_at outside the explicit range so that
-- inserts never fail due to a missing partition (e.g. during a missed maintenance run).
--
-- Partition naming convention: transaction_audit_pYYYYMM (e.g. transaction_audit_p202605).
-- Maintenance: run Taskfile partition:create-next monthly (cron / Kubernetes CronJob).
DO $$
DECLARE
    month_start  timestamptz;
    month_end    timestamptz;
    part_name    text;
    i            int;
BEGIN
    -- Create partitions from one month before now through three months ahead.
    FOR i IN -1..3 LOOP
        month_start := date_trunc('month', now()) + (i || ' months')::interval;
        month_end   := month_start + interval '1 month';
        part_name   := 'transaction_audit_p' || to_char(month_start, 'YYYYMM');

        EXECUTE format(
            'CREATE TABLE IF NOT EXISTS %I PARTITION OF transaction_audit '
            'FOR VALUES FROM (%L) TO (%L)',
            part_name, month_start, month_end
        );
    END LOOP;

    -- Safety-net DEFAULT partition: catches rows with occurred_at outside all explicit ranges.
    EXECUTE 'CREATE TABLE IF NOT EXISTS transaction_audit_default '
            'PARTITION OF transaction_audit DEFAULT';
END;
$$;
