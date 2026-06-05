-- Migration 000005: per-milestone disbursement records.
--
-- ROOT CAUSE fixed: 000002 stored per-PLAN status on settlement_allocations.
-- After milestone 1 set all allocs→DISBURSED, milestone 2+ found them already
-- DISBURSED and silently paid nothing. This migration introduces a per-milestone
-- disbursement record so each milestone is tracked independently.
--
-- Design:
--   settlement_allocations → FROZEN ROSTER only (vendor_user_id, share_bps frozen
--     at CreatePlan; allocation.status is no longer the disburse guard).
--   settlement_milestone_disbursements → one row per (plan, milestone, vendor) per
--     disburse attempt; status tracks the per-milestone payout state.
--
-- NO FOREIGN KEY constraints anywhere (CONVENTIONS §11 / CLAUDE.md #9).
-- Money: numeric(14,2) matching the rest of the schema.
-- Retention: permanent records (regulatory compliance). No TTL.

CREATE TABLE settlement_milestone_disbursements (
    id               uuid          PRIMARY KEY DEFAULT gen_random_uuid(),
    -- soft ref to settlement_plans (no FK, code-validated)
    plan_id          uuid          NOT NULL,
    -- soft ref to workspace milestone (no FK, code-validated)
    milestone_id     uuid          NOT NULL,
    -- soft ref to user service (no FK, code-validated)
    vendor_user_id   uuid          NOT NULL,
    amount           numeric(14,2) NOT NULL CHECK (amount >= 0),
    -- soft ref to transactions.id (no FK, code-validated); nullable until DISBURSED
    tx_id            uuid,
    status           text          NOT NULL DEFAULT 'PENDING'
                         CHECK (status IN ('PENDING', 'DISBURSED', 'FAILED')),
    -- content-addressed: disburse:<plan_id>:<milestone_id>:<vendor_user_id>
    idempotency_key  varchar(511)  NOT NULL,
    created_at       timestamptz   NOT NULL DEFAULT now(),
    updated_at       timestamptz   NOT NULL DEFAULT now()
);

-- Idempotency: exactly one row per (plan, milestone, vendor).
-- ON CONFLICT on this unique key → genuine skip, return success without re-creating.
CREATE UNIQUE INDEX settlement_milestone_disbursements_plan_milestone_vendor_idx
    ON settlement_milestone_disbursements (plan_id, milestone_id, vendor_user_id);

-- Global idempotency key uniqueness (content-addressed by triple key).
CREATE UNIQUE INDEX settlement_milestone_disbursements_idempotency_key_idx
    ON settlement_milestone_disbursements (idempotency_key);

-- Lookup all disbursements for a plan (admin / audit queries).
CREATE INDEX settlement_milestone_disbursements_plan_id_idx
    ON settlement_milestone_disbursements (plan_id, created_at DESC);

-- Lookup all disbursements for a specific milestone.
CREATE INDEX settlement_milestone_disbursements_milestone_id_idx
    ON settlement_milestone_disbursements (milestone_id, plan_id);

-- Find FAILED disbursements for retry worker.
CREATE INDEX settlement_milestone_disbursements_status_idx
    ON settlement_milestone_disbursements (status, plan_id)
    WHERE status IN ('PENDING', 'FAILED');

-- Lookup by vendor for per-user disbursement history.
CREATE INDEX settlement_milestone_disbursements_vendor_user_id_idx
    ON settlement_milestone_disbursements (vendor_user_id, created_at DESC);
