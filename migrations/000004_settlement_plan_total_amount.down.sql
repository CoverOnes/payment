-- Revert: restore CHECK (total_amount > 0) on settlement_plans.
ALTER TABLE settlement_plans
    DROP CONSTRAINT IF EXISTS settlement_plans_total_amount_check;

ALTER TABLE settlement_plans
    ADD CONSTRAINT settlement_plans_total_amount_check CHECK (total_amount > 0);
