package domain

import (
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"
)

// TransactionStatusChangedEvent is published on payment.transaction_status_changed
// when a transaction transitions between states.
// Per CONVENTIONS §14 envelope: eventId, occurredAt, version, data.
// Best-effort: publish failure MUST NOT roll back the state transition.
// The transactions row is the authoritative record.
type TransactionStatusChangedEvent struct {
	EventID    uuid.UUID                    `json:"eventId"`
	OccurredAt time.Time                    `json:"occurredAt"`
	Version    int                          `json:"version"`
	Data       TransactionStatusChangedData `json:"data"`
}

// TransactionStatusChangedData carries the transition payload.
type TransactionStatusChangedData struct {
	TransactionID uuid.UUID       `json:"transactionId"`
	PayerUserID   uuid.UUID       `json:"payerUserId"`
	PayeeUserID   uuid.UUID       `json:"payeeUserId"`
	ContractID    *uuid.UUID      `json:"contractId,omitempty"`
	MilestoneID   *uuid.UUID      `json:"milestoneId,omitempty"`
	Amount        decimal.Decimal `json:"amount"`
	Currency      string          `json:"currency"`
	FromStatus    Status          `json:"fromStatus"`
	ToStatus      Status          `json:"toStatus"`
	ActorUserID   uuid.UUID       `json:"actorUserId"`
	At            time.Time       `json:"at"`
}
