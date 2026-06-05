-- Revert migration 000005: drop the settlement_milestone_disbursements table and all its indexes.
DROP TABLE IF EXISTS settlement_milestone_disbursements;
