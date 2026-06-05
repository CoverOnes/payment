-- Revert migration 000004: drop the relaxed CHECK constraint on total_amount.
-- NOTE: we intentionally do NOT restore CHECK (total_amount > 0) because plans now
-- legitimately start at 0 (milestone-driven model). Restoring a stricter constraint
-- would fail on any row that was created under the relaxed rule. Leave unconstrained.
ALTER TABLE settlement_plans
    DROP CONSTRAINT IF EXISTS settlement_plans_total_amount_check;
