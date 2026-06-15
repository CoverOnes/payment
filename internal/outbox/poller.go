// Package outbox provides an in-process transactional outbox poller for the payment service.
// The poller reads unpublished events from the event_outbox table and dispatches them to
// the settlement service — achieving at-least-once delivery even across server restarts.
//
// # At-least-once guarantee
//
// A crash between event processing and MarkPublished causes the event to be re-delivered
// on the next poll cycle. All consumers (CreatePlan, DisburseMilestone) are idempotent,
// so double-dispatch is safe — no double-disburse.
//
// # Idempotency key
//
// The payment idempotency key for each event is (aggregate_id + channel):
//   - "payment.contract_activated" → CreatePlan idempotency: CountByMultiContractID > 0 → no-op
//   - "payment.contract_completed" → DisburseMilestone idempotency: settlement_milestone_disbursements
//     ON CONFLICT DO NOTHING + DISBURSED status check → no-op on replay
//
// # Triple sum-check on replay
//
// On every DisburseMilestone replay, the service verifies Σ allocations == milestone amount
// and Σ post-disburse == expected (via verifySumEquals). Rounding residual assignment is
// deterministic: computeAllocatedAmounts iterates lockedAllocs in FOR UPDATE order (sorted
// by id), so the rounding sink is always the same allocation across replays.
//
// # Concurrent pollers
//
// PollReady uses SELECT ... FOR UPDATE SKIP LOCKED so multiple poller goroutines (or
// processes) each claim a disjoint set of rows — no double-delivery from locking alone.
//
// # Retention
//
// Published rows older than 7 days are deleted on each poll cycle.
// Unpublished rows older than 1 hour trigger a slog.Warn alert.
package outbox

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/CoverOnes/payment/internal/domain"
	"github.com/CoverOnes/payment/internal/service"
	"github.com/CoverOnes/payment/internal/store"
	"github.com/google/uuid"
	"github.com/shopspring/decimal"
)

const (
	// retentionPeriod is how long published outbox rows are kept before deletion.
	retentionPeriod = 7 * 24 * time.Hour

	// staleUnpublishedThreshold triggers a slog.Warn when an unpublished event
	// is older than this duration.
	staleUnpublishedThreshold = time.Hour

	// pollBatchSize is the maximum number of rows claimed per poll cycle.
	pollBatchSize = 100

	// event channel names — must match the constants used by service.
	channelContractActivated = "payment.contract_activated"
	channelContractCompleted = "payment.contract_completed"
)

// settlementServicer is the minimal interface needed by the poller.
// Using an interface rather than *service.SettlementService makes the poller
// testable without a real DB (integration tests inject the real service).
type settlementServicer interface {
	CreatePlan(ctx context.Context, in *service.CreatePlanInput) (*domain.SettlementPlan, error)
	GetPlanByContractID(ctx context.Context, contractID uuid.UUID) (*domain.SettlementPlan, error)
	DisburseMilestone(ctx context.Context, in *service.DisburseMilestoneInput) (*service.DisburseResult, error)
}

// Poller is an in-process outbox poller. Start it with Run; stop it by canceling ctx.
type Poller struct {
	outbox   store.OutboxStore
	svc      settlementServicer
	interval time.Duration
}

// NewPoller returns a Poller.
// interval is the poll frequency (default 2s when <= 0).
func NewPoller(outbox store.OutboxStore, svc *service.SettlementService, interval time.Duration) *Poller {
	if interval <= 0 {
		interval = 2 * time.Second
	}

	return &Poller{
		outbox:   outbox,
		svc:      svc,
		interval: interval,
	}
}

// Run starts the poller loop. It blocks until ctx is canceled.
// Safe to call from a goroutine; uses context.Background()-derived timeouts for
// each DB + service operation (backend-security §5: goroutine must not inherit
// request context so it is not canceled on client disconnect).
func (p *Poller) Run(ctx context.Context) {
	ticker := time.NewTicker(p.interval)
	defer ticker.Stop()

	slog.Info("payment outbox poller started", "interval", p.interval)

	for {
		select {
		case <-ctx.Done():
			slog.Info("payment outbox poller stopping")

			return
		case <-ticker.C:
			p.tick() //nolint:contextcheck // tick intentionally uses context.Background() per backend-security §5: poller goroutine must not inherit request context
		}
	}
}

// tick is one poll cycle: poll + dispatch + housekeeping.
func (p *Poller) tick() {
	pollCtx, pollCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer pollCancel()

	events, err := p.outbox.PollReady(pollCtx, pollBatchSize)
	if err != nil {
		slog.Warn("payment outbox poll failed", "err", err)

		return
	}

	for _, evt := range events {
		p.processEvent(evt)
	}

	p.deletePublished()
	p.alertStale()
}

// processEvent dispatches one outbox event to the settlement service and marks it published or failed.
func (p *Poller) processEvent(evt *domain.OutboxEvent) {
	dispatchCtx, dispatchCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer dispatchCancel()

	dispatchErr := p.dispatch(dispatchCtx, evt)

	markCtx, markCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer markCancel()

	if dispatchErr != nil {
		slog.Warn(
			"payment outbox dispatch failed; will retry",
			"event_id", evt.EventID,
			"channel", evt.Channel,
			"attempts", evt.Attempts+1,
			"err", dispatchErr,
		)

		if markErr := p.outbox.MarkFailed(markCtx, evt.ID, dispatchErr.Error()); markErr != nil {
			slog.Warn("payment outbox mark-failed failed", "outbox_id", evt.ID, "err", markErr)
		}

		return
	}

	if markErr := p.outbox.MarkPublished(markCtx, evt.ID); markErr != nil {
		slog.Warn(
			"payment outbox mark-published failed; event was processed but may be re-processed",
			"outbox_id", evt.ID,
			"event_id", evt.EventID,
			"err", markErr,
		)
	}
}

// dispatch routes an outbox event to the correct settlement service method.
// All service methods are idempotent so replaying an already-processed event is safe.
func (p *Poller) dispatch(ctx context.Context, evt *domain.OutboxEvent) error {
	switch evt.Channel {
	case channelContractActivated:
		return p.handleContractActivated(ctx, evt)
	case channelContractCompleted:
		return p.handleContractCompleted(ctx, evt)
	default:
		// F4: return error so processEvent calls MarkFailed (retry + visible in stale alert)
		// instead of MarkPublished (which silently swallows mis-routed events).
		return fmt.Errorf("unknown outbox channel %q", evt.Channel)
	}
}

// contractActivatedPayload is the payload stored in event_outbox for contract_activated events.
type contractActivatedPayload struct {
	MultiContractID uuid.UUID `json:"multi_contract_id"`
	TenderID        uuid.UUID `json:"tender_id"`
	PlanID          uuid.UUID `json:"plan_id"`
	IdempotencyKey  string    `json:"idempotency_key"`
	// Currency is the ISO 4217 settlement currency written by persistPlan at enqueue time.
	// Falls back to "TWD" for events written before this field was added (F2 fix).
	Currency string `json:"currency"`
}

// contractCompletedPayload is the payload stored in event_outbox for contract_completed events.
type contractCompletedPayload struct {
	PlanID      uuid.UUID `json:"plan_id"`
	MilestoneID uuid.UUID `json:"milestone_id"`
	Amount      string    `json:"amount"`
	Currency    string    `json:"currency"`
	Actor       string    `json:"actor"`
}

// handleContractActivated replays a contract_activated event by calling CreatePlan.
// CreatePlan is idempotent: if a plan already exists for the contract, it returns nil, nil.
func (p *Poller) handleContractActivated(ctx context.Context, evt *domain.OutboxEvent) error {
	var pl contractActivatedPayload
	if err := json.Unmarshal(evt.Payload, &pl); err != nil {
		return fmt.Errorf("unmarshal contract_activated payload: %w", err)
	}

	// Idempotency key from the original event — ensures CreatePlan deduplicates correctly.
	if pl.IdempotencyKey == "" {
		pl.IdempotencyKey = "outbox_replay:" + evt.EventID.String()
	}

	// F2: read currency from payload (written by persistPlan since this fix).
	// Fall back to "TWD" only for events written before the currency field was added.
	currency := pl.Currency
	if currency == "" {
		currency = "TWD"
	}

	_, err := p.svc.CreatePlan(ctx, &service.CreatePlanInput{
		MultiContractID: pl.MultiContractID,
		TenderID:        pl.TenderID,
		Currency:        currency,
		IdempotencyKey:  pl.IdempotencyKey,
	})
	if err != nil {
		return fmt.Errorf("outbox replay CreatePlan: %w", err)
	}

	return nil
}

// handleContractCompleted replays a contract_completed event by calling DisburseMilestone.
// DisburseMilestone is idempotent: per-(plan, milestone, vendor) disbursement records use
// ON CONFLICT DO NOTHING so a replay does NOT double-disburse.
//
// Triple sum-check on every replay: DisburseMilestone calls verifySumEquals pre- and
// post-disburse. Rounding residual is stable because computeAllocatedAmounts iterates
// allocations in FOR UPDATE order (sorted by id), making the rounding sink deterministic.
func (p *Poller) handleContractCompleted(ctx context.Context, evt *domain.OutboxEvent) error {
	var pl contractCompletedPayload
	if err := json.Unmarshal(evt.Payload, &pl); err != nil {
		return fmt.Errorf("unmarshal contract_completed payload: %w", err)
	}

	amount, err := decimal.NewFromString(pl.Amount)
	if err != nil {
		return fmt.Errorf("parse amount %q: %w", pl.Amount, err)
	}

	currency := pl.Currency
	if currency == "" {
		currency = "TWD"
	}

	actor := pl.Actor
	if actor == "" {
		actor = "payment-outbox-poller"
	}

	_, err = p.svc.DisburseMilestone(ctx, &service.DisburseMilestoneInput{
		PlanID:       pl.PlanID,
		MilestoneID:  pl.MilestoneID,
		Amount:       amount,
		Currency:     currency,
		ActorService: actor,
	})
	if err != nil {
		return fmt.Errorf("outbox replay DisburseMilestone: %w", err)
	}

	return nil
}

// deletePublished removes published rows older than retentionPeriod.
func (p *Poller) deletePublished() {
	retCtx, retCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer retCancel()

	cutoff := time.Now().UTC().Add(-retentionPeriod)

	n, err := p.outbox.DeletePublishedBefore(retCtx, cutoff)
	if err != nil {
		slog.Warn("payment outbox retention delete failed", "err", err)

		return
	}

	if n > 0 {
		slog.Info("payment outbox retention: deleted published rows", "count", n)
	}
}

// alertStale logs a warning for stale unpublished events via a DB-side count query.
// This catches all stuck rows — including those that never enter the poll batch because
// their next_attempt_at is in the future or claimed_until is set by a crashed poller.
// The batch-scan approach (checking only polled events) was a blind spot: events stuck
// in backoff or claim-window were invisible until they re-entered the batch.
func (p *Poller) alertStale() {
	alertCtx, alertCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer alertCancel()

	threshold := time.Now().UTC().Add(-staleUnpublishedThreshold)

	count, err := p.outbox.CountStaleUnpublished(alertCtx, threshold)
	if err != nil {
		slog.Warn("payment outbox: stale alert query failed", "err", err)

		return
	}

	if count > 0 {
		slog.Warn("payment outbox: stale unpublished events detected (DB-side count)",
			"count", count,
			"older_than", threshold)
	}
}
