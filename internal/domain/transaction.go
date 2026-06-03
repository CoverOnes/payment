package domain

import (
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"
)

// Status represents the lifecycle state of a transaction.
// Money representation choice: shopspring/decimal for all arithmetic — NEVER float64.
// The DB stores numeric(14,2) which maps to decimal exactly.
type Status string

const (
	StatusPending  Status = "PENDING"
	StatusHeld     Status = "HELD"
	StatusReleased Status = "RELEASED"
	StatusRefunded Status = "REFUNDED"
	StatusFailed   Status = "FAILED"
)

// Transaction is the core escrow/payment domain entity.
// amount is decimal.Decimal (backed by shopspring/decimal) — never float64.
// idempotency_key is provided by the caller and stored unique; duplicate keys return the existing tx.
type Transaction struct {
	ID             uuid.UUID       `json:"id"`
	PayerUserID    uuid.UUID       `json:"payerUserId"`
	PayeeUserID    uuid.UUID       `json:"payeeUserId"`
	ContractID     *uuid.UUID      `json:"contractId,omitempty"`  // soft ref, nullable
	MilestoneID    *uuid.UUID      `json:"milestoneId,omitempty"` // soft ref, nullable
	Amount         decimal.Decimal `json:"amount"`
	Currency       string          `json:"currency"`
	Status         Status          `json:"status"`
	IdempotencyKey string          `json:"idempotencyKey"`
	CreatedAt      time.Time       `json:"createdAt"`
	UpdatedAt      time.Time       `json:"updatedAt"`
}

// TransactionAudit is an append-only record of a state transition.
type TransactionAudit struct {
	ID            uuid.UUID `json:"id"`
	TransactionID uuid.UUID `json:"transactionId"`
	FromStatus    Status    `json:"fromStatus"`
	ToStatus      Status    `json:"toStatus"`
	ActorUserID   uuid.UUID `json:"actorUserId"`
	OccurredAt    time.Time `json:"occurredAt"`
}
