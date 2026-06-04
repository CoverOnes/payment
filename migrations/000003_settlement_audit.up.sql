-- settlement_audit: append-only event log for settlement plan + allocation lifecycle.
--
-- Partitioned by RANGE (occurred_at) — mirrors transaction_audit partition approach.
-- Retention: 730 days (2-year compliance window).
-- Managed by Taskfile partition:drop-old-settlement target — drops monthly child tables
-- whose upper bound is older than now() - interval '730 days'.
--
-- PK includes occurred_at because PG requires all partitioned-table unique indexes
-- to include the partition key column.
--
-- NO FOREIGN KEY constraints (CONVENTIONS §11 / CLAUDE.md #9).
-- plan_id and allocation_id are soft uuid refs validated at application layer.
--
-- event_type values: PLAN_CREATED, PLAN_COMPLETED, PLAN_CANCELED,
--   ALLOCATION_DISBURSED, ALLOCATION_FAILED.
--   Note: CANCELED (single L) matches the Go domain PlanStatus constant.
-- actor_service: the service that triggered the event (e.g. "payment", "workspace").
-- payload: arbitrary JSON for structured event context (amounts, reason, etc.).

CREATE TABLE settlement_audit (
    id             uuid        NOT NULL DEFAULT gen_random_uuid(),
    -- soft ref to settlement_plans (no FK, code-validated)
    plan_id        uuid        NOT NULL,
    -- soft ref to settlement_allocations; nullable — plan-level events have no allocation
    allocation_id  uuid,
    event_type     varchar(64) NOT NULL
                       CHECK (event_type IN (
                           'PLAN_CREATED',
                           'PLAN_COMPLETED',
                           'PLAN_CANCELED',
                           'ALLOCATION_DISBURSED',
                           'ALLOCATION_FAILED'
                       )),
    actor_service  varchar(64) NOT NULL,
    payload        jsonb       NOT NULL DEFAULT '{}',
    occurred_at    timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (id, occurred_at)
) PARTITION BY RANGE (occurred_at);

-- Index on parent propagates to all partitions automatically.
-- Primary access pattern: fetch all audit entries for a plan, newest-first.
CREATE INDEX settlement_audit_plan_id_idx
    ON settlement_audit (plan_id, occurred_at DESC);

-- Secondary: filter by allocation_id for per-allocation audit trail.
CREATE INDEX settlement_audit_allocation_id_idx
    ON settlement_audit (allocation_id, occurred_at DESC)
    WHERE allocation_id IS NOT NULL;

-- Bootstrap partitions: prior month, current month, and next 3 months.
-- A DEFAULT partition catches any occurred_at outside the explicit range so that
-- inserts never fail due to a missing partition (e.g. during a missed maintenance run).
--
-- Partition naming convention: settlement_audit_pYYYYMM (e.g. settlement_audit_p202605).
-- Maintenance: run Taskfile partition:create-next-settlement monthly.
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
        part_name   := 'settlement_audit_p' || to_char(month_start, 'YYYYMM');

        EXECUTE format(
            'CREATE TABLE IF NOT EXISTS %I PARTITION OF settlement_audit '
            'FOR VALUES FROM (%L) TO (%L)',
            part_name, month_start, month_end
        );
    END LOOP;

    -- Safety-net DEFAULT partition: catches rows with occurred_at outside all explicit ranges.
    EXECUTE 'CREATE TABLE IF NOT EXISTS settlement_audit_default '
            'PARTITION OF settlement_audit DEFAULT';
END;
$$;
