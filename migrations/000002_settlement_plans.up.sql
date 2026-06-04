-- NO FOREIGN KEY constraints platform-wide (CONVENTIONS §11 / CLAUDE.md #9).
-- Referential integrity enforced in service layer (code validation + nullable JOIN).

-- Settlement plan lifecycle:
--   ACTIVE    : plan created, allocations locked, awaiting disburse trigger
--   COMPLETED : all allocations disbursed successfully
--   CANCELED : plan abandoned before disburse (e.g. contract canceled)
--
-- Single-currency v1 (TWD). Multi-currency deferred to v2.
-- Money: numeric(14,2) minor units (TWD cents), mirroring transactions table.
-- Shares: basis_points (int), Σ across all allocations in a plan = 10000.
--
-- Retention: settlement_plans and settlement_allocations are permanent records
-- (regulatory compliance). No TTL. Pruning policy: archive, not delete.

CREATE TABLE settlement_plans (
    id                  uuid          PRIMARY KEY DEFAULT gen_random_uuid(),
    -- soft ref to workspace multi_party_contracts (no FK, code-validated)
    multi_contract_id   uuid          NOT NULL,
    -- soft ref to marketplace tender (no FK, code-validated)
    tender_id           uuid          NOT NULL,
    status              text          NOT NULL DEFAULT 'ACTIVE'
                            CHECK (status IN ('ACTIVE', 'COMPLETED', 'CANCELED')),
    total_amount        numeric(14,2) NOT NULL CHECK (total_amount > 0),
    currency            char(3)       NOT NULL DEFAULT 'TWD',
    frozen_party_count  int           NOT NULL CHECK (frozen_party_count > 0),
    idempotency_key     text          NOT NULL,
    created_at          timestamptz   NOT NULL DEFAULT now(),
    updated_at          timestamptz   NOT NULL DEFAULT now()
);

-- Prevent two active plans for the same contract (uniqueness scoped to non-canceled).
-- Allows re-creating a plan after cancellation.
CREATE UNIQUE INDEX settlement_plans_multi_contract_active_idx
    ON settlement_plans (multi_contract_id)
    WHERE status != 'CANCELED';

-- Global idempotency key uniqueness (caller-controlled deduplication).
CREATE UNIQUE INDEX settlement_plans_idempotency_key_idx
    ON settlement_plans (idempotency_key);

-- Lookup by tender for listing / admin queries.
CREATE INDEX settlement_plans_tender_id_idx
    ON settlement_plans (tender_id, created_at DESC);

-- Lookup by status for settlement worker (find ACTIVE plans to process).
CREATE INDEX settlement_plans_status_idx
    ON settlement_plans (status, created_at DESC);


-- Settlement allocation: one row per vendor per plan.
-- Allocated amount computed from share_bps at plan-creation time; stored for auditability.
-- is_rounding_sink=true marks the allocation that absorbs the integer-division residual
--   (last-allocation-absorbs-rounding rule from the SA spec).
--
-- Allocation statuses:
--   PENDING   : not yet disbursed
--   DISBURSED : funds moved (disbursed_tx_id references the transactions row)
--   FAILED    : disburse attempt failed (terminal; requires operator intervention)

CREATE TABLE settlement_allocations (
    id               uuid          PRIMARY KEY DEFAULT gen_random_uuid(),
    -- soft ref to settlement_plans (no FK, code-validated)
    plan_id          uuid          NOT NULL,
    -- soft ref to user service (no FK, code-validated)
    vendor_user_id   uuid          NOT NULL,
    -- soft ref to workspace role; nullable — collaborators without explicit role are allowed
    role_id          uuid,
    share_bps        int           NOT NULL CHECK (share_bps >= 0 AND share_bps <= 10000),
    allocated_amount numeric(14,2) NOT NULL CHECK (allocated_amount >= 0),
    currency         char(3)       NOT NULL DEFAULT 'TWD',
    is_rounding_sink bool          NOT NULL DEFAULT FALSE,
    status           text          NOT NULL DEFAULT 'PENDING'
                        CHECK (status IN ('PENDING', 'DISBURSED', 'FAILED')),
    -- soft ref to transactions.id after disburse (no FK, code-validated)
    disbursed_tx_id  uuid,
    idempotency_key  text          NOT NULL,
    created_at       timestamptz   NOT NULL DEFAULT now(),
    updated_at       timestamptz   NOT NULL DEFAULT now()
);

-- One allocation per vendor per plan (prevent duplicate share rows).
CREATE UNIQUE INDEX settlement_allocations_plan_vendor_idx
    ON settlement_allocations (plan_id, vendor_user_id);

-- Global idempotency key uniqueness for allocation creation.
CREATE UNIQUE INDEX settlement_allocations_idempotency_key_idx
    ON settlement_allocations (idempotency_key);

-- Lookup all allocations for a plan (primary access pattern for disburse loop).
CREATE INDEX settlement_allocations_plan_id_idx
    ON settlement_allocations (plan_id, created_at ASC);

-- Find allocations by status for worker / retry logic.
CREATE INDEX settlement_allocations_status_idx
    ON settlement_allocations (status, plan_id);

-- Lookup by vendor for per-user settlement history.
CREATE INDEX settlement_allocations_vendor_user_id_idx
    ON settlement_allocations (vendor_user_id, created_at DESC);
