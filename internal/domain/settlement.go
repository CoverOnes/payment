package domain

import (
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"
)

// Settlement plan and allocation sentinel errors.
var (
	ErrPlanNotFound                  = errors.New("settlement plan not found")
	ErrPlanAlreadyDisbursed          = errors.New("settlement plan already disbursed")
	ErrSumInvariantViolation         = errors.New("allocation share_bps do not sum to 10000")
	ErrAllocationNotFound            = errors.New("settlement allocation not found")
	ErrMilestoneDisbursementNotFound = errors.New("settlement milestone disbursement not found")
)

// PlanStatus represents the lifecycle state of a settlement plan.
type PlanStatus string

const (
	PlanStatusActive    PlanStatus = "ACTIVE"
	PlanStatusCompleted PlanStatus = "COMPLETED"
	PlanStatusCanceled  PlanStatus = "CANCELED"
)

// AllocationStatus represents the lifecycle state of a single allocation.
type AllocationStatus string

const (
	AllocationStatusPending   AllocationStatus = "PENDING"
	AllocationStatusDisbursed AllocationStatus = "DISBURSED"
	AllocationStatusFailed    AllocationStatus = "FAILED"
)

// SettlementPlan is the aggregate root for a multi-party split settlement.
//
// total_amount is in numeric(14,2) minor units (TWD cents by default).
// currency is a 3-char ISO 4217 code; v1 supports TWD only.
// frozen_party_count is the number of allocations locked at plan creation time —
// used as a sanity check that no party was added/removed before disburse.
// idempotency_key is caller-controlled; globally unique (deduplicates plan creation).
// multi_contract_id and tender_id are soft uuid refs (no FK, code-validated).
type SettlementPlan struct {
	ID               uuid.UUID       `json:"id"`
	MultiContractID  uuid.UUID       `json:"multiContractId"` // soft ref to workspace contract
	TenderID         uuid.UUID       `json:"tenderId"`        // soft ref to marketplace tender
	Status           PlanStatus      `json:"status"`
	TotalAmount      decimal.Decimal `json:"totalAmount"`
	Currency         string          `json:"currency"`
	FrozenPartyCount int             `json:"frozenPartyCount"`
	IdempotencyKey   string          `json:"idempotencyKey"`
	CreatedAt        time.Time       `json:"createdAt"`
	UpdatedAt        time.Time       `json:"updatedAt"`
}

// SettlementAllocation is a single vendor's share within a plan.
//
// share_bps is the basis-points share (0–10000). Σ across all allocations = 10000.
// allocated_amount is pre-computed at plan creation from share_bps × total_amount.
// is_rounding_sink=true marks the allocation that absorbs the integer-division residual
//
//	(last-allocation-absorbs-rounding rule; exactly one per plan).
//
// disbursed_tx_id is set after a successful disburse (soft ref to transactions.id).
// role_id is optional; nil means vendor participates without an explicit role.
type SettlementAllocation struct {
	ID              uuid.UUID        `json:"id"`
	PlanID          uuid.UUID        `json:"planId"`           // soft ref to settlement_plans
	VendorUserID    uuid.UUID        `json:"vendorUserId"`     // soft ref to user service
	RoleID          *uuid.UUID       `json:"roleId,omitempty"` // nullable
	ShareBps        int              `json:"shareBps"`
	AllocatedAmount decimal.Decimal  `json:"allocatedAmount"`
	Currency        string           `json:"currency"`
	IsRoundingSink  bool             `json:"isRoundingSink"`
	Status          AllocationStatus `json:"status"`
	DisbursedTxID   *uuid.UUID       `json:"disbursedTxId,omitempty"` // nullable
	IdempotencyKey  string           `json:"idempotencyKey"`
	CreatedAt       time.Time        `json:"createdAt"`
	UpdatedAt       time.Time        `json:"updatedAt"`
}

// MilestoneDisbursementStatus represents the state of a single per-milestone disburse record.
type MilestoneDisbursementStatus string

const (
	MilestoneDisbursementStatusPending   MilestoneDisbursementStatus = "PENDING"
	MilestoneDisbursementStatusDisbursed MilestoneDisbursementStatus = "DISBURSED"
	MilestoneDisbursementStatusFailed    MilestoneDisbursementStatus = "FAILED"
)

// SettlementMilestoneDisbursement is a per-(plan, milestone, vendor) disbursement record.
//
// The per-milestone design fixes the multi-milestone correctness bug where the old
// per-plan allocation status caused milestone 2+ to silently pay nothing because all
// allocations were already DISBURSED after milestone 1.
//
// idempotency_key is content-addressed: "disburse:<plan_id>:<milestone_id>:<vendor_user_id>".
// UNIQUE(plan_id, milestone_id, vendor_user_id) prevents duplicate rows.
// NO FK constraints (backend-security-design §1.1).
type SettlementMilestoneDisbursement struct {
	ID             uuid.UUID                   `json:"id"`
	PlanID         uuid.UUID                   `json:"planId"`
	MilestoneID    uuid.UUID                   `json:"milestoneId"`
	VendorUserID   uuid.UUID                   `json:"vendorUserId"`
	Amount         decimal.Decimal             `json:"amount"`
	TxID           *uuid.UUID                  `json:"txId,omitempty"`
	Status         MilestoneDisbursementStatus `json:"status"`
	IdempotencyKey string                      `json:"idempotencyKey"`
	CreatedAt      time.Time                   `json:"createdAt"`
	UpdatedAt      time.Time                   `json:"updatedAt"`
}

// SettlementAuditEntry is an append-only event row in settlement_audit.
//
// allocation_id is nullable — plan-level events (PLAN_CREATED, PLAN_COMPLETED) have
// no associated allocation. payload is arbitrary JSON for structured context.
type SettlementAuditEntry struct {
	ID           uuid.UUID       `json:"id"`
	PlanID       uuid.UUID       `json:"planId"`
	AllocationID *uuid.UUID      `json:"allocationId,omitempty"` // nullable
	EventType    string          `json:"eventType"`
	ActorService string          `json:"actorService"`
	Payload      json.RawMessage `json:"payload"`
	OccurredAt   time.Time       `json:"occurredAt"`
}
