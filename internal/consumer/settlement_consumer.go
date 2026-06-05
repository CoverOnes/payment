// Package consumer provides Redis pub/sub event consumers for the payment service.
package consumer

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/CoverOnes/payment/internal/service"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"github.com/shopspring/decimal"
)

const (
	channelContractActivated = "workspace.contract_activated"
	channelContractCompleted = "workspace.contract_completed"
)

// contractActivatedData is the data payload of workspace.contract_activated.
// partyCount > 0 signals a multiparty contract.
type contractActivatedData struct {
	ContractID uuid.UUID `json:"contractId"`
	TenderID   uuid.UUID `json:"tenderId"`
	PartyCount int       `json:"partyCount"`
}

// contractActivatedEvent is the full workspace.contract_activated envelope.
type contractActivatedEvent struct {
	EventID    uuid.UUID             `json:"eventId"`
	OccurredAt time.Time             `json:"occurredAt"`
	Version    int                   `json:"version"`
	Data       contractActivatedData `json:"data"`
}

// contractCompletedData is the data payload of workspace.contract_completed.
type contractCompletedData struct {
	ContractID  uuid.UUID       `json:"contractId"`
	TenderID    uuid.UUID       `json:"tenderId"`
	MilestoneID uuid.UUID       `json:"milestoneId"`
	Amount      decimal.Decimal `json:"amount"`
	Currency    string          `json:"currency"`
}

// contractCompletedEvent is the full workspace.contract_completed envelope.
type contractCompletedEvent struct {
	EventID    uuid.UUID             `json:"eventId"`
	OccurredAt time.Time             `json:"occurredAt"`
	Version    int                   `json:"version"`
	Data       contractCompletedData `json:"data"`
}

// SettlementConsumer subscribes to workspace Redis channels and drives the
// SettlementService: contract_activated → CreatePlan, contract_completed → DisburseMilestone.
// It is best-effort and idempotent: events may replay without side effects.
type SettlementConsumer struct {
	rdb *redis.Client
	svc *service.SettlementService
}

// NewSettlementConsumer returns a SettlementConsumer.
func NewSettlementConsumer(rdb *redis.Client, svc *service.SettlementService) *SettlementConsumer {
	return &SettlementConsumer{rdb: rdb, svc: svc}
}

// Start subscribes to workspace channels and processes events until ctx is canceled.
// MUST be called in a goroutine. Uses context.Background()-derived sub-contexts for
// per-event calls so individual event processing outlives the caller's goroutine lifecycle.
func (c *SettlementConsumer) Start(ctx context.Context) {
	pubsub := c.rdb.Subscribe(ctx, channelContractActivated, channelContractCompleted)

	defer func() {
		if err := pubsub.Close(); err != nil {
			slog.Warn("settlement consumer pubsub close error", "err", err)
		}
	}()

	slog.Info("settlement event consumer started",
		"channels", []string{channelContractActivated, channelContractCompleted})

	ch := pubsub.Channel()

	for {
		select {
		case <-ctx.Done():
			slog.Info("settlement event consumer stopping (context canceled)")
			return

		case msg, ok := <-ch:
			if !ok {
				slog.Warn("settlement consumer channel closed; stopping")
				return
			}

			c.dispatch(ctx, msg)
		}
	}
}

// dispatch routes an incoming Redis pub/sub message to the correct handler.
func (c *SettlementConsumer) dispatch(ctx context.Context, msg *redis.Message) {
	switch msg.Channel {
	case channelContractActivated:
		c.handleContractActivated(ctx, msg.Payload)

	case channelContractCompleted:
		c.handleContractCompleted(ctx, msg.Payload)

	default:
		slog.Warn("settlement consumer received unknown channel", "channel", msg.Channel)
	}
}

// handleContractActivated processes workspace.contract_activated.
// Ignores non-multiparty contracts (partyCount == 0).
// Idempotency key: "contract_activated:<eventId>".
//
//nolint:contextcheck // intentional: detached context with timeout; event processing must not inherit loop ctx
func (c *SettlementConsumer) handleContractActivated(_ context.Context, payload string) {
	var evt contractActivatedEvent
	if err := json.Unmarshal([]byte(payload), &evt); err != nil {
		slog.Error("settlement consumer: decode contract_activated failed", "err", err)
		return
	}

	// Ignore single-party / dual-sign contracts — only multiparty (partyCount > 0).
	if evt.Data.PartyCount == 0 {
		slog.Debug("settlement consumer: skipping non-multiparty contract_activated",
			"contract_id", evt.Data.ContractID)

		return
	}

	iKey := "contract_activated:" + evt.EventID.String()

	// Detached context with timeout so the S2S call and DB write are bounded.
	// Must NOT inherit parent context to prevent premature cancellation.
	callCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	plan, err := c.svc.CreatePlan(callCtx, &service.CreatePlanInput{
		MultiContractID: evt.Data.ContractID,
		TenderID:        evt.Data.TenderID,
		Currency:        "TWD", // milestone carries its own currency; plan records the roster currency
		IdempotencyKey:  iKey,
	})
	if err != nil {
		slog.Error("settlement consumer: CreatePlan failed",
			"event_id", evt.EventID,
			"contract_id", evt.Data.ContractID,
			"err", err)

		return
	}

	if plan == nil {
		// Already exists — idempotent skip.
		slog.Info("settlement consumer: CreatePlan skipped (idempotent)",
			"event_id", evt.EventID,
			"contract_id", evt.Data.ContractID)

		return
	}

	slog.Info("settlement consumer: plan created",
		"event_id", evt.EventID,
		"plan_id", plan.ID,
		"contract_id", evt.Data.ContractID)
}

// handleContractCompleted processes workspace.contract_completed (milestone payout).
// Idempotency: per-(plan, milestoneID, vendor) keys prevent double-disburse.
//
//nolint:contextcheck // intentional: detached context with timeout; event processing must not inherit loop ctx
func (c *SettlementConsumer) handleContractCompleted(_ context.Context, payload string) {
	var evt contractCompletedEvent
	if err := json.Unmarshal([]byte(payload), &evt); err != nil {
		slog.Error("settlement consumer: decode contract_completed failed", "err", err)
		return
	}

	// Detached context with timeout.
	callCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	plan, err := c.svc.GetPlanByContractID(callCtx, evt.Data.ContractID)
	if err != nil {
		slog.Error("settlement consumer: GetPlanByContractID failed",
			"event_id", evt.EventID,
			"contract_id", evt.Data.ContractID,
			"err", err)

		return
	}

	if plan == nil {
		slog.Warn("settlement consumer: no active plan found for contract; skipping milestone disburse",
			"event_id", evt.EventID,
			"contract_id", evt.Data.ContractID,
			"milestone_id", evt.Data.MilestoneID)

		return
	}

	currency := evt.Data.Currency
	if currency == "" {
		currency = "TWD"
	}

	err = c.svc.DisburseMilestone(callCtx, &service.DisburseMilestoneInput{
		PlanID:               plan.ID,
		MilestoneID:          evt.Data.MilestoneID,
		Amount:               evt.Data.Amount,
		Currency:             currency,
		IdempotencyKeySuffix: evt.EventID.String(),
		ActorService:         "payment-consumer",
	})
	if err != nil {
		slog.Error("settlement consumer: DisburseMilestone failed",
			"event_id", evt.EventID,
			"plan_id", plan.ID,
			"milestone_id", evt.Data.MilestoneID,
			"err", err)

		return
	}

	slog.Info("settlement consumer: milestone disbursed",
		"event_id", evt.EventID,
		"plan_id", plan.ID,
		"milestone_id", evt.Data.MilestoneID,
		"amount", evt.Data.Amount.StringFixed(2))
}
