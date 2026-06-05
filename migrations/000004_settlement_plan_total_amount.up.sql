-- Migration: relax settlement_plans.total_amount check to allow 0.
-- Rationale: plans are created at contract_activated time before any milestone is known.
-- total_amount starts at 0 and reflects cumulative disbursed amounts per milestone.
-- The CHECK (total_amount > 0) from 000002 was too strict for the milestone-driven model.
-- This corrective migration uses ALTER TABLE / DROP CONSTRAINT / ADD CONSTRAINT (immutable-migration §6.4).

ALTER TABLE settlement_plans
    DROP CONSTRAINT IF EXISTS settlement_plans_total_amount_check;

ALTER TABLE settlement_plans
    ADD CONSTRAINT settlement_plans_total_amount_check CHECK (total_amount >= 0);
