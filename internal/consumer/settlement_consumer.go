// Package consumer provides Redis pub/sub event consumers for the payment service.
package consumer

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"time"

	"github.com/CoverOnes/payment/internal/domain"
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

// settlementServicer is the minimal interface that SettlementConsumer needs from
// SettlementService. Declared here so handler tests can inject a stub without a DB.
type settlementServicer interface {
	CreatePlan(ctx context.Context, in *service.CreatePlanInput) (*domain.SettlementPlan, error)
	GetPlanByContractID(ctx context.Context, contractID uuid.UUID) (*domain.SettlementPlan, error)
	DisburseMilestone(ctx context.Context, in *service.DisburseMilestoneInput) (*service.DisburseResult, error)
}

// SettlementConsumer subscribes to workspace Redis channels and drives the
// SettlementService: contract_activated → CreatePlan, contract_completed → DisburseMilestone.
// It is best-effort and idempotent: events may replay without side effects.
type SettlementConsumer struct {
	rdb *redis.Client
	svc settlementServicer
}

// NewSettlementConsumer returns a SettlementConsumer.
func NewSettlementConsumer(rdb *redis.Client, svc *service.SettlementService) *SettlementConsumer {
	return &SettlementConsumer{rdb: rdb, svc: svc}
}

// newSettlementConsumerWithService returns a SettlementConsumer with an injected service stub.
// For use in tests only — production callers use NewSettlementConsumer.
func newSettlementConsumerWithService(svc settlementServicer) *SettlementConsumer {
	return &SettlementConsumer{svc: svc}
}

// Start subscribes to workspace channels and processes events until ctx is canceled.
// MUST be called in a goroutine. Uses context.Background()-derived sub-contexts for
// per-event calls so individual event processing outlives the caller's goroutine lifecycle.
//
// Graceful drain: each incoming message is dispatched in its own goroutine tracked by a
// sync.WaitGroup. When ctx is canceled the loop stops accepting new messages and
// waits for all in-flight handlers to finish before returning, so that a SIGTERM
// cannot cut off a disbursement mid-flight (leaving a PENDING row).
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

	var wg sync.WaitGroup

	for {
		select {
		case <-ctx.Done():
			slog.Info("settlement event consumer stopping (context canceled); draining in-flight handlers")
			wg.Wait()
			slog.Info("settlement event consumer: all in-flight handlers finished")

			return

		case msg, ok := <-ch:
			if !ok {
				slog.Warn("settlement consumer channel closed; stopping; draining in-flight handlers")
				wg.Wait()

				return
			}

			wg.Add(1)

			go func(m *redis.Message) {
				defer wg.Done()
				c.dispatch(ctx, m)
			}(msg)
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
// Always attempts plan creation and lets the workspace roster S2S response determine
// whether the contract is multiparty (≥1 party in the roster). We do NOT use
// partyCount from the event for the multiparty decision because the event field is
// caller-supplied and could be forged or stale; the workspace roster is authoritative.
// If the roster is empty, CreatePlan returns ErrValidation and nothing is persisted.
// Idempotency key: "contract_activated:<eventId>".
//
//nolint:contextcheck // intentional: detached context with timeout; event processing must not inherit loop ctx
func (c *SettlementConsumer) handleContractActivated(_ context.Context, payload string) {
	var evt contractActivatedEvent
	if err := json.Unmarshal([]byte(payload), &evt); err != nil {
		slog.Error("settlement consumer: decode contract_activated failed", "err", err)
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
		// WARNING: Redis pub/sub has NO retry mechanism — this event is permanently lost.
		// Manual recovery is required via: POST /internal/v1/settlements/plans (re-create plan).
		slog.Error("settlement consumer: CreatePlan failed — event permanently lost (pub/sub has no retry); manual recovery required",
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

	// Fix #3 (Critical): all-or-nothing model. DisburseMilestone now returns an error
	// on any vendor DB failure, rolling back all vendors atomically. The partial-failure
	// log branch has been removed because it was unreachable in production.
	_, err = c.svc.DisburseMilestone(callCtx, &service.DisburseMilestoneInput{
		PlanID:       plan.ID,
		MilestoneID:  evt.Data.MilestoneID,
		Amount:       evt.Data.Amount,
		Currency:     currency,
		ActorService: "payment-consumer",
	})
	if err != nil {
		// WARNING: Redis pub/sub has NO retry mechanism — this event is permanently lost.
		// Manual recovery: POST /internal/v1/settlements/milestones/disburse
		slog.Error("settlement consumer: DisburseMilestone failed — permanently lost (pub/sub no retry); manual recovery required",
			"event_id", evt.EventID,
			"plan_id", plan.ID,
			"milestone_id", evt.Data.MilestoneID,
			"recovery_endpoint", "POST /internal/v1/settlements/milestones/disburse",
			"err", err)

		return
	}

	slog.Info("settlement consumer: milestone disbursed",
		"event_id", evt.EventID,
		"plan_id", plan.ID,
		"milestone_id", evt.Data.MilestoneID,
		"amount", evt.Data.Amount.StringFixed(2))
}
