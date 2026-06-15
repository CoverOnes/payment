package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/CoverOnes/payment/internal/domain"
	"github.com/CoverOnes/payment/internal/store"
	"github.com/google/uuid"
	"github.com/shopspring/decimal"
)

// SettlementService implements the business logic for multi-party split settlement.
// CreatePlan freezes the party roster at contract activation time.
// DisburseMilestone computes and disburses per-milestone amounts to each vendor using
// a per-(plan, milestone, vendor) disbursement record — fixing the multi-milestone bug
// where the old per-plan allocation status caused milestone 2+ to silently pay nothing.
type SettlementService struct {
	plans         store.SettlementPlanStore
	allocs        store.SettlementAllocationStore
	disburseStore store.SettlementMilestoneDisbursementStore
	audit         store.SettlementAuditStore
	txMgr         store.SettlementTxManager
	roster        WorkspaceRosterClient
	platformUID   uuid.UUID // PayerUserID for all disburse transactions (self-transfer guard)
}

// WorkspaceRosterClient fetches roster and milestone aggregate data from the workspace service.
// Abstracted so tests can inject a stub without a real HTTP server.
type WorkspaceRosterClient interface {
	// GetPartyRoster calls GET <workspace>/internal/v1/contracts/:id/parties
	// and returns the frozen [{vendorUserId, shareBps}] roster.
	GetPartyRoster(ctx context.Context, contractID uuid.UUID) ([]RosterEntry, error)
	// GetMilestoneAmountsSum calls GET <workspace>/internal/v1/contracts/:id/milestones/amounts
	// and returns the sum of ALL milestone amounts for the contract (the escrow cap).
	// Returns decimal.Zero when no milestones exist (uncapped passthrough in CreatePlan).
	GetMilestoneAmountsSum(ctx context.Context, contractID uuid.UUID) (decimal.Decimal, error)
}

// RosterEntry is one party in the frozen ACTIVE roster returned by the workspace S2S endpoint.
type RosterEntry struct {
	VendorUserID uuid.UUID `json:"vendorUserId"`
	ShareBps     int       `json:"shareBps"`
}

// NewSettlementService returns a SettlementService.
// platformUID is the system payer identity used in disburse transactions (self-transfer guard:
// PayerUserID = platformUID, PayeeUserID = vendor, so payer != payee always).
// The transactions.Create call inside disburse uses the tx-scoped TransactionStore passed
// by WithSettlementTx — no pool-backed TransactionStore is needed by the service itself.
func NewSettlementService(
	plans store.SettlementPlanStore,
	allocs store.SettlementAllocationStore,
	disburseStore store.SettlementMilestoneDisbursementStore,
	audit store.SettlementAuditStore,
	txMgr store.SettlementTxManager,
	roster WorkspaceRosterClient,
	platformUID uuid.UUID,
) *SettlementService {
	return &SettlementService{
		plans:         plans,
		allocs:        allocs,
		disburseStore: disburseStore,
		audit:         audit,
		txMgr:         txMgr,
		roster:        roster,
		platformUID:   platformUID,
	}
}

// CreatePlanInput carries the validated input for CreatePlan.
type CreatePlanInput struct {
	// MultiContractID is the workspace multiparty contract ID.
	MultiContractID uuid.UUID
	// TenderID is the marketplace tender ID.
	TenderID uuid.UUID
	// Currency is the ISO 4217 settlement currency (e.g. "TWD").
	Currency string
	// TotalAmount is the contract total disbursement cap (M-1 fix).
	// Must be set at plan creation so DisburseMilestone can enforce the cumulative cap.
	// When the contract_activated event carries the total, pass it here; otherwise
	// workspace MUST add totalAmount to the contract_activated event (cross-service follow-up).
	TotalAmount decimal.Decimal
	// IdempotencyKey scopes plan creation; use "contract_activated:<eventId>".
	IdempotencyKey string
}

// CreatePlan creates a settlement plan for a multiparty contract by:
//  1. Fetching the frozen ACTIVE-party roster from the workspace S2S endpoint.
//  2. Triple sum-checking Σ(shareBps) == 10000.
//  3. Persisting the plan + all allocations (allocated_amount = 0, informational only).
//
// Idempotent: if a plan already exists for this contract, returns nil, nil (no-op).
// No FK constraints anywhere (backend-security-design §1.1).
func (s *SettlementService) CreatePlan(ctx context.Context, in *CreatePlanInput) (*domain.SettlementPlan, error) {
	if err := validateCreatePlanInput(in); err != nil {
		return nil, err
	}

	// Idempotency guard: if a non-canceled plan already exists for this contract, skip.
	existingCount, err := s.plans.CountByMultiContractID(ctx, in.MultiContractID)
	if err != nil {
		return nil, fmt.Errorf("check existing plans: %w", err)
	}

	if existingCount > 0 {
		slog.Info("settlement plan already exists for contract; skipping creation (idempotent)",
			"multi_contract_id", in.MultiContractID)

		return nil, nil //nolint:nilnil // nil,nil signals "already exists, nothing to do" to the caller (event consumer)
	}

	// Fetch and validate the frozen ACTIVE-party roster from workspace.
	roster, err := s.roster.GetPartyRoster(ctx, in.MultiContractID)
	if err != nil {
		return nil, fmt.Errorf("fetch party roster from workspace: %w", err)
	}

	if len(roster) == 0 {
		return nil, fmt.Errorf("%w: party roster is empty for contract %s", domain.ErrValidation, in.MultiContractID)
	}

	if err := validateRosterSum(roster); err != nil {
		return nil, err
	}

	// Fetch the escrow disbursement cap: Σ of ALL milestone amounts known at CreatePlan time.
	// This is the authoritative cap — fetched S2S from workspace so it cannot be forged via
	// an unsigned Redis event. decimal.Zero means no milestones yet; DisburseMilestone treats
	// TotalAmount==0 as "uncapped" for backwards-compat (see disburseMilestoneTx).
	milestoneSum, err := s.roster.GetMilestoneAmountsSum(ctx, in.MultiContractID)
	if err != nil {
		return nil, fmt.Errorf("fetch milestone amounts sum from workspace: %w", err)
	}

	in.TotalAmount = milestoneSum

	plan, allocationRows := buildPlanWithAllocations(in, roster)

	if err := s.persistPlan(ctx, plan, allocationRows, in); err != nil {
		if errors.Is(err, domain.ErrDuplicateKey) {
			return nil, nil //nolint:nilnil // idempotent: concurrent creation raced us; caller should treat as success
		}

		return nil, err
	}

	slog.Info("settlement plan created",
		"plan_id", plan.ID,
		"multi_contract_id", in.MultiContractID,
		"party_count", len(roster))

	return plan, nil
}

// buildPlanWithAllocations constructs the SettlementPlan and its allocation rows
// from the validated roster. Extracted to reduce CreatePlan cyclomatic complexity.
// settlement_allocations is now a FROZEN ROSTER only; allocation.status is NOT used
// as the per-milestone disburse guard (see settlement_milestone_disbursements).
func buildPlanWithAllocations(in *CreatePlanInput, roster []RosterEntry) (*domain.SettlementPlan, []*domain.SettlementAllocation) {
	now := time.Now().UTC()
	plan := &domain.SettlementPlan{
		ID:               uuid.New(),
		MultiContractID:  in.MultiContractID,
		TenderID:         in.TenderID,
		Status:           domain.PlanStatusActive,
		TotalAmount:      in.TotalAmount,
		Currency:         in.Currency,
		FrozenPartyCount: len(roster),
		IdempotencyKey:   in.IdempotencyKey,
		CreatedAt:        now,
		UpdatedAt:        now,
	}

	allocationRows := make([]*domain.SettlementAllocation, len(roster))

	for i, party := range roster {
		allocationRows[i] = &domain.SettlementAllocation{
			ID:              uuid.New(),
			PlanID:          plan.ID,
			VendorUserID:    party.VendorUserID,
			ShareBps:        party.ShareBps,
			AllocatedAmount: decimal.Zero,
			Currency:        in.Currency,
			IsRoundingSink:  i == len(roster)-1,
			Status:          domain.AllocationStatusPending,
			IdempotencyKey:  fmt.Sprintf("plan:%s:vendor:%s", plan.ID, party.VendorUserID),
			CreatedAt:       now,
			UpdatedAt:       now,
		}
	}

	return plan, allocationRows
}

// persistPlan atomically writes the plan, allocations, PLAN_CREATED audit entry,
// and a contract_activated outbox event so the plan creation is replayable via
// the outbox poller (at-least-once, idempotent).
func (s *SettlementService) persistPlan(
	ctx context.Context,
	plan *domain.SettlementPlan,
	allocationRows []*domain.SettlementAllocation,
	in *CreatePlanInput,
) error {
	return s.txMgr.WithSettlementTx(ctx, func(
		ctx context.Context,
		plans store.TxSettlementPlanStore,
		allocs store.TxSettlementAllocationStore,
		_ store.SettlementMilestoneDisbursementStore,
		_ store.TransactionStore,
		audit store.SettlementAuditStore,
		outbox store.OutboxStore,
	) error {
		if planErr := plans.Create(ctx, plan); planErr != nil {
			if errors.Is(planErr, domain.ErrDuplicateKey) {
				return planErr
			}

			return fmt.Errorf("create settlement plan: %w", planErr)
		}

		for _, a := range allocationRows {
			if allocErr := allocs.Create(ctx, a); allocErr != nil {
				return fmt.Errorf("create allocation for vendor %s: %w", a.VendorUserID, allocErr)
			}
		}

		auditPayload, _ := json.Marshal(map[string]any{
			"multi_contract_id":  in.MultiContractID,
			"tender_id":          in.TenderID,
			"frozen_party_count": len(allocationRows),
		})

		if auditErr := audit.Append(ctx, &domain.SettlementAuditEntry{
			ID:           uuid.New(),
			PlanID:       plan.ID,
			EventType:    "PLAN_CREATED",
			ActorService: "payment",
			Payload:      auditPayload,
			OccurredAt:   time.Now().UTC(),
		}); auditErr != nil {
			return fmt.Errorf("append PLAN_CREATED audit: %w", auditErr)
		}

		// Same-tx outbox enqueue: the contract_activated event is recorded atomically
		// with the plan write. The poller replays it if the server crashes before
		// marking it published; CreatePlan is idempotent so replay is safe.
		outboxPayload, _ := json.Marshal(map[string]any{
			"multi_contract_id": in.MultiContractID,
			"tender_id":         in.TenderID,
			"plan_id":           plan.ID,
			"idempotency_key":   in.IdempotencyKey,
		})
		now := time.Now().UTC()

		return outbox.Enqueue(ctx, &domain.OutboxEvent{
			ID:            uuid.New(),
			AggregateType: "settlement_plan",
			AggregateID:   plan.ID,
			EventID:       uuid.New(),
			Channel:       "payment.contract_activated",
			Payload:       outboxPayload,
			CreatedAt:     now,
			NextAttemptAt: now,
		})
	})
}

// DisburseMilestoneInput carries the validated input for DisburseMilestone.
// IdempotencyKeySuffix has been removed — idempotency is content-addressed by
// (plan_id, milestone_id, vendor_user_id), which is intrinsically unique per payout.
// Caller-supplied suffix was misleading and created an ORPHAN PENDING risk.
type DisburseMilestoneInput struct {
	// PlanID is the settlement plan to disburse against.
	PlanID uuid.UUID
	// MilestoneID is the workspace milestone being paid.
	MilestoneID uuid.UUID
	// Amount is the total milestone payout (decimal string at call site, decimal.Decimal here).
	Amount decimal.Decimal
	// Currency is the ISO 4217 settlement currency.
	Currency string
	// ActorService identifies the caller for the audit trail.
	ActorService string
}

// VendorDisburseOutcome records the per-vendor result of a single DisburseMilestone call.
// In the all-or-nothing model all outcomes in a successful call are always DISBURSED.
type VendorDisburseOutcome struct {
	// VendorUserID identifies the vendor.
	VendorUserID uuid.UUID `json:"vendorUserId"`
	// Status is "DISBURSED" (or "FAILED" on legacy retried rows from before the model change).
	Status string `json:"status"`
}

// DisburseResult is the structured outcome of DisburseMilestone.
// Fix #3 (Critical): in the all-or-nothing model, FailedCount is always 0 on a
// successful call — any vendor error rolls back the whole transaction and returns
// an error from DisburseMilestone instead.
type DisburseResult struct {
	// Outcomes is one entry per vendor allocation, in allocation order.
	Outcomes []VendorDisburseOutcome
	// DisbursedCount is the number of vendors successfully disbursed (or idempotently skipped).
	DisbursedCount int
	// FailedCount is always 0 in the all-or-nothing model; retained for API compat.
	FailedCount int
}

// DisburseMilestone disburses a milestone payout to all frozen allocations of a plan.
//
// Algorithm (per-milestone model — fixes multi-milestone bug):
//  1. SELECT FOR UPDATE on the plan (TOCTOU serialization).
//  2. SELECT FOR UPDATE on all allocations (frozen roster).
//  3. For each allocation: compute amount × shareBps / 10000 (last absorbs rounding).
//  4. Idempotency: check settlement_milestone_disbursements for (plan, milestone, vendor);
//     if already DISBURSED → genuine skip (return success WITHOUT re-creating).
//  5. Create one settlement_milestone_disbursements row + one transactions row per vendor
//     in the SAME transaction (atomicity).
//  6. Fix #3 (Critical): all-or-nothing model — any vendor DB error returns an error,
//     rolling back the entire pgx transaction. The 207 partial-failure path has been
//     removed because it was structurally unreachable in production (a real SQL error
//     aborts the shared pgx tx, making "already-paid vendors" an illusion).
//  7. Triple sum-check pre-tx and post-tx.
//
// Returns a *DisburseResult describing the per-vendor outcome so the caller can
// render an honest HTTP response (200 all-paid / error all-rolled-back).
func (s *SettlementService) DisburseMilestone(ctx context.Context, in *DisburseMilestoneInput) (*DisburseResult, error) {
	if err := validateDisburseMilestoneInput(in); err != nil {
		return nil, err
	}

	var disbursedAmounts []decimal.Decimal

	var outcomes []VendorDisburseOutcome

	txErr := s.txMgr.WithSettlementTx(ctx, func(
		ctx context.Context,
		plans store.TxSettlementPlanStore,
		allocs store.TxSettlementAllocationStore,
		disbursements store.SettlementMilestoneDisbursementStore,
		txTxStore store.TransactionStore,
		audit store.SettlementAuditStore,
		outbox store.OutboxStore,
	) error {
		return s.disburseMilestoneTx(ctx, in, plans, allocs, disbursements, txTxStore, audit, outbox, &disbursedAmounts, &outcomes)
	})

	if txErr != nil {
		// Fix #6 (Major): write ALLOCATION_FAILED audit entries for all vendors when the
		// transaction rolls back. The rolled-back tx leaves no DB trace of the attempt,
		// so these out-of-tx audit writes are the only DB record of the failure.
		// Best-effort: audit failures are logged but not returned to the caller.
		s.appendAllocationFailedAudit(ctx, in, txErr)

		return nil, txErr
	}

	result := buildDisburseResult(outcomes)

	if err := s.postDisburseCheck(in, disbursedAmounts); err != nil {
		return nil, err
	}

	return result, nil
}

// disburseMilestoneTx runs the disburse logic inside a DB transaction.
// Extracted to reduce DisburseMilestone's cyclomatic complexity.
func (s *SettlementService) disburseMilestoneTx(
	ctx context.Context,
	in *DisburseMilestoneInput,
	plans store.TxSettlementPlanStore,
	allocs store.TxSettlementAllocationStore,
	disbursements store.SettlementMilestoneDisbursementStore,
	txTxStore store.TransactionStore,
	audit store.SettlementAuditStore,
	outbox store.OutboxStore,
	disbursedAmounts *[]decimal.Decimal,
	outcomes *[]VendorDisburseOutcome,
) error {
	plan, lockErr := plans.GetByIDForUpdate(ctx, in.PlanID)
	if lockErr != nil {
		return lockErr
	}

	// Security: only ACTIVE plans may be disbursed.
	// A COMPLETED plan must not be disbursable — a fresh set of milestoneIDs could drain
	// escrow again because per-milestone idempotency only blocks same-(plan,milestone,vendor)
	// replays, not new milestoneIDs against a completed plan.
	if plan.Status != domain.PlanStatusActive {
		return fmt.Errorf("%w: plan is %s (expected ACTIVE)", domain.ErrInvalidTransition, plan.Status)
	}

	// M-1 (Major): cumulative disbursement cap. Only enforced when plan.TotalAmount > 0
	// (zero means cap was not set at creation — treated as uncapped, backwards-compatible).
	//
	// IMPORTANT: the correctness of this cap check depends on the plan-row FOR UPDATE lock
	// (acquired above by GetByIDForUpdate) being held through the entire transaction.
	// Without that lock, two concurrent DisburseMilestone calls could both read the same
	// sumDisbursed, both pass the cap check, and together exceed plan.TotalAmount.
	// The FOR UPDATE serializes concurrent calls: the second caller waits for the first
	// to commit (updating sumDisbursed) before re-reading — ensuring the cap is enforced.
	if plan.TotalAmount.GreaterThan(decimal.Zero) {
		sumDisbursed, sumErr := disbursements.SumDisbursedByPlanID(ctx, in.PlanID)
		if sumErr != nil {
			return fmt.Errorf("query cumulative disbursed sum: %w", sumErr)
		}

		if sumDisbursed.Add(in.Amount).GreaterThan(plan.TotalAmount) {
			return fmt.Errorf("%w: cumulative disbursed %s + incoming %s exceeds plan total_amount %s",
				domain.ErrValidation,
				sumDisbursed.StringFixed(2),
				in.Amount.StringFixed(2),
				plan.TotalAmount.StringFixed(2))
		}
	}

	// Fix #2 (Major): assert currency matches the frozen plan currency so that a
	// forged/buggy event with a mismatched currency cannot corrupt accounting.
	if in.Currency != plan.Currency {
		return fmt.Errorf("%w: disburse currency %q does not match plan currency %q",
			domain.ErrValidation, in.Currency, plan.Currency)
	}

	lockedAllocs, listErr := allocs.ListByPlanIDForUpdate(ctx, in.PlanID)
	if listErr != nil {
		return fmt.Errorf("lock allocations for plan: %w", listErr)
	}

	if len(lockedAllocs) == 0 {
		return fmt.Errorf("%w: no allocations found for plan %s", domain.ErrValidation, in.PlanID)
	}

	if plan.FrozenPartyCount != len(lockedAllocs) {
		return fmt.Errorf("%w: frozen_party_count %d != actual allocation count %d",
			domain.ErrValidation, plan.FrozenPartyCount, len(lockedAllocs))
	}

	amounts := computeAllocatedAmounts(in.Amount, lockedAllocs)

	if err := verifySumEquals(amounts, in.Amount, "pre-disburse"); err != nil {
		return err
	}

	*disbursedAmounts = make([]decimal.Decimal, 0, len(lockedAllocs))
	*outcomes = make([]VendorDisburseOutcome, 0, len(lockedAllocs))

	// Fix #3 (Critical): all-or-nothing loop — any error propagates immediately,
	// causing WithSettlementTx to roll back the entire pgx transaction.
	for i, alloc := range lockedAllocs {
		disbursed, allocErr := s.disburseAllocation(ctx, in, disbursements, txTxStore, audit, plan, alloc, amounts[i])
		if allocErr != nil {
			return fmt.Errorf("disburse vendor %s: %w", alloc.VendorUserID, allocErr)
		}

		*disbursedAmounts = append(*disbursedAmounts, disbursed)
		*outcomes = append(*outcomes, VendorDisburseOutcome{
			VendorUserID: alloc.VendorUserID,
			Status:       string(domain.MilestoneDisbursementStatusDisbursed),
		})
	}

	// Same-tx outbox enqueue: the completed event is recorded atomically with
	// the disbursement writes. The poller replays it on crash-recovery;
	// DisburseMilestone is idempotent (ON CONFLICT DO NOTHING on disbursement rows).
	//
	// Triple sum-check on every replay: the poller calls DisburseMilestone again;
	// the existing settlement_milestone_disbursements rows are DISBURSED so each
	// allocation is skipped (idempotent). The outbox event carries the milestone
	// amount + currency so the replay call uses the identical input.
	//
	// Deterministic rounding: computeAllocatedAmounts sorts by iteration order
	// of lockedAllocs (ORDER BY id via ListByPlanIDForUpdate) so the rounding
	// sink (last allocation by id) is stable across replays.
	outboxPayload, _ := json.Marshal(map[string]any{
		"plan_id":      in.PlanID,
		"milestone_id": in.MilestoneID,
		"amount":       in.Amount.StringFixed(2),
		"currency":     in.Currency,
		"actor":        in.ActorService,
	})
	now := time.Now().UTC()

	return outbox.Enqueue(ctx, &domain.OutboxEvent{
		ID:            uuid.New(),
		AggregateType: "settlement_plan",
		AggregateID:   in.PlanID,
		EventID:       uuid.New(),
		Channel:       "payment.contract_completed",
		Payload:       outboxPayload,
		CreatedAt:     now,
		NextAttemptAt: now,
	})
}

// buildDisburseResult aggregates per-vendor outcomes into a DisburseResult summary.
// In the all-or-nothing model all outcomes are always DISBURSED (or the call errored).
func buildDisburseResult(outcomes []VendorDisburseOutcome) *DisburseResult {
	r := &DisburseResult{Outcomes: outcomes}

	for _, o := range outcomes {
		if o.Status == string(domain.MilestoneDisbursementStatusDisbursed) {
			r.DisbursedCount++
		} else {
			r.FailedCount++
		}
	}

	return r
}

// disburseAllocation disburses a single allocation using the per-milestone disbursement model.
// Returns (amount, nil) on success or idempotent skip, or (zero, error) on failure.
//
// Fix #3 (Critical): the return type changed from (bool, decimal.Decimal) to
// (decimal.Decimal, error). Errors now propagate to the caller (disburseMilestoneTx),
// which returns them from within the pgx transaction, triggering an honest rollback.
// The old pattern swallowed errors and used a bool flag to indicate failure, giving the
// false impression that "some vendors paid, others failed" could persist atomically —
// it cannot, because a real SQL error aborts the shared pgx transaction entirely.
//
// Idempotency: if (plan, milestone, vendor) already has a DISBURSED record → genuine skip.
// Atomicity: the transactions row + the settlement_milestone_disbursements row are written
// inside the same DB transaction via the tx-scoped txTxStore and disbursements stores.
func (s *SettlementService) disburseAllocation(
	ctx context.Context,
	in *DisburseMilestoneInput,
	disbursements store.SettlementMilestoneDisbursementStore,
	txTxStore store.TransactionStore,
	audit store.SettlementAuditStore,
	plan *domain.SettlementPlan,
	alloc *domain.SettlementAllocation,
	allocAmt decimal.Decimal,
) (decimal.Decimal, error) {
	// Content-addressed idempotency key: unique per (plan, milestone, vendor).
	iKey := fmt.Sprintf("disburse:%s:%s:%s", in.PlanID, in.MilestoneID, alloc.VendorUserID)

	now := time.Now().UTC()

	// Create the per-milestone disbursement record FIRST (PENDING).
	// Uses ON CONFLICT DO NOTHING so an idempotent replay does NOT abort the transaction
	// (a plain INSERT UNIQUE violation would leave the tx in SQLSTATE 25P02, poisoning
	// all subsequent statements). Returns inserted=false when the row already existed.
	disburseRow := &domain.SettlementMilestoneDisbursement{
		ID:             uuid.New(),
		PlanID:         in.PlanID,
		MilestoneID:    in.MilestoneID,
		VendorUserID:   alloc.VendorUserID,
		Amount:         allocAmt,
		Status:         domain.MilestoneDisbursementStatusPending,
		IdempotencyKey: iKey,
		CreatedAt:      now,
		UpdatedAt:      now,
	}

	inserted, createErr := disbursements.Create(ctx, disburseRow)
	if createErr != nil {
		return decimal.Zero, fmt.Errorf("create milestone disbursement row: %w", createErr)
	}

	if !inserted {
		// Row already existed — fetch it and branch on its current status.
		// DISBURSED: genuine idempotent skip — vendor was already paid; do NOT re-pay.
		// PENDING: anomalous (a committed tx must have succeeded or failed); treat as retryable.
		existing, fetchErr := disbursements.GetByPlanMilestoneVendor(ctx, in.PlanID, in.MilestoneID, alloc.VendorUserID)
		if fetchErr != nil {
			return decimal.Zero, fmt.Errorf("fetch existing disbursement: %w", fetchErr)
		}

		if existing.Status == domain.MilestoneDisbursementStatusDisbursed {
			// Already paid — idempotent skip; add to running sum without re-creating a tx.
			slog.Info("per-milestone disbursement already DISBURSED; skipping (idempotent)",
				"plan_id", in.PlanID,
				"milestone_id", in.MilestoneID,
				"vendor_user_id", alloc.VendorUserID)

			return existing.Amount, nil
		}

		// PENDING (anomalous stale row): attempt to pay now.
		slog.Info("per-milestone disbursement is stale PENDING; attempting pay",
			"plan_id", in.PlanID,
			"milestone_id", in.MilestoneID,
			"vendor_user_id", alloc.VendorUserID,
			"status", existing.Status)

		disburseRow = existing // use the existing row's ID for UpdateStatus calls below
	}

	// Self-transfer guard: PayerUserID = platform system account,
	// PayeeUserID = vendor. This ensures payer != payee always.
	txRow := &domain.Transaction{
		ID:             uuid.New(),
		PayerUserID:    s.platformUID,
		PayeeUserID:    alloc.VendorUserID,
		ContractID:     &plan.MultiContractID,
		MilestoneID:    &in.MilestoneID,
		Amount:         allocAmt,
		Currency:       in.Currency,
		Status:         domain.StatusReleased,
		IdempotencyKey: iKey,
		CreatedAt:      now,
		UpdatedAt:      now,
	}

	// txTxStore.Create is tx-scoped (passed via WithSettlementTx 5th param).
	// This ensures the transactions row + disbursement row are written atomically.
	//
	// NOTE: ErrDuplicateKey from txTxStore.Create is intentionally NOT handled here.
	// In the single-tx model, the disbursement row and transaction row are written in the
	// same DB transaction. If txTxStore.Create were to return ErrDuplicateKey, it would
	// mean a transaction with the same idempotency_key already exists — but since we only
	// reach this point on a fresh INSERT (inserted=true) or a PENDING retry (where
	// no tx row exists), a duplicate is structurally impossible. If this assumption
	// ever breaks, the ErrDuplicateKey will surface as a real error and roll back correctly.
	if createErr = txTxStore.Create(ctx, txRow); createErr != nil {
		return decimal.Zero, fmt.Errorf("create transaction row: %w", createErr)
	}

	txID := txRow.ID

	// Update disbursement record to DISBURSED with the tx_id.
	if updErr := disbursements.UpdateStatus(ctx, disburseRow.ID, domain.MilestoneDisbursementStatusDisbursed, &txID); updErr != nil {
		return decimal.Zero, fmt.Errorf("update disbursement to DISBURSED: %w", updErr)
	}

	payload, _ := json.Marshal(map[string]any{
		"milestone_id":   in.MilestoneID,
		"amount":         allocAmt.StringFixed(2),
		"currency":       in.Currency,
		"tx_id":          txID,
		"vendor_user_id": alloc.VendorUserID,
	})

	if auditErr := audit.Append(ctx, &domain.SettlementAuditEntry{
		ID:           uuid.New(),
		PlanID:       in.PlanID,
		AllocationID: &alloc.ID,
		EventType:    "ALLOCATION_DISBURSED",
		ActorService: in.ActorService,
		Payload:      payload,
		OccurredAt:   time.Now().UTC(),
	}); auditErr != nil {
		slog.Error("append allocation_disbursed audit failed", "allocation_id", alloc.ID, "err", auditErr)
	}

	return allocAmt, nil
}

// postDisburseCheck runs post-tx sum verification and logs final outcome.
func (s *SettlementService) postDisburseCheck(
	in *DisburseMilestoneInput,
	disbursedAmounts []decimal.Decimal,
) error {
	if len(disbursedAmounts) > 0 {
		if err := verifySumEquals(disbursedAmounts, in.Amount, "post-disburse"); err != nil {
			slog.Error("POST-DISBURSE SUM INVARIANT VIOLATED — possible money-drift",
				"plan_id", in.PlanID,
				"milestone_id", in.MilestoneID,
				"err", err)

			return err
		}
	}

	slog.Info("milestone disburse completed successfully",
		"plan_id", in.PlanID,
		"milestone_id", in.MilestoneID,
		"amount", in.Amount.StringFixed(2))

	return nil
}

// appendAllocationFailedAudit writes an ALLOCATION_FAILED audit entry for each vendor
// allocation after a disburse transaction rolls back.
// Fix #6 (Major): these out-of-tx writes are the only DB record of a failed disburse attempt.
// M-2 (Major): uses an independent context.WithTimeout(context.Background(), 10*time.Second)
// so the audit write is not silently dropped when the caller ctx is already canceled or timed out.
// Best-effort: fetching allocations or individual audit writes may fail (e.g., DB unavailable
// during the same outage that caused the tx failure); each error is logged but not propagated.
//
//nolint:contextcheck // intentional: independent context so failure-audit survives caller ctx cancellation (M-2)
func (s *SettlementService) appendAllocationFailedAudit(_ context.Context, in *DisburseMilestoneInput, txErr error) {
	auditCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	allocs, listErr := s.allocs.ListByPlanID(auditCtx, in.PlanID)
	if listErr != nil {
		slog.Error("appendAllocationFailedAudit: cannot list allocations for audit",
			"plan_id", in.PlanID,
			"milestone_id", in.MilestoneID,
			"err", listErr)

		return
	}

	for _, alloc := range allocs {
		// M-4: store only error type/class, not raw error string (which may contain SQL internals).
		payload, _ := json.Marshal(map[string]any{
			"milestone_id":   in.MilestoneID,
			"vendor_user_id": alloc.VendorUserID,
			"reason":         fmt.Sprintf("tx_error:%T", txErr),
		})

		allocID := alloc.ID

		if auditErr := s.audit.Append(auditCtx, &domain.SettlementAuditEntry{
			ID:           uuid.New(),
			PlanID:       in.PlanID,
			AllocationID: &allocID,
			EventType:    "ALLOCATION_FAILED",
			ActorService: in.ActorService,
			Payload:      payload,
			OccurredAt:   time.Now().UTC(),
		}); auditErr != nil {
			slog.Error("appendAllocationFailedAudit: failed to write audit entry",
				"plan_id", in.PlanID,
				"allocation_id", alloc.ID,
				"err", auditErr)
		}
	}
}

// CompletePlan transitions a plan from ACTIVE to COMPLETED and writes a PLAN_COMPLETED audit entry.
// Fix #6 (Major): implements the plan-completion transition that was missing (plans stayed ACTIVE
// forever, never reaching PlanStatusCompleted).
//
// Callers: the workspace service should invoke this (via the manual disburse endpoint or a
// dedicated "plan complete" endpoint) after confirming all milestones have been disbursed.
// Returns ErrPlanNotFound if the plan does not exist.
// Returns ErrInvalidTransition if the plan is not ACTIVE (already COMPLETED or CANCELED).
// CompletePlan transitions a plan from ACTIVE to COMPLETED and writes a PLAN_COMPLETED audit entry.
// Fix #6 (Major): implements the plan-completion transition that was missing (plans stayed ACTIVE
// forever, never reaching PlanStatusCompleted).
// TOCTOU fix: uses CompletePlanAtomic (UPDATE ... WHERE id=$1 AND status='ACTIVE') so that two
// concurrent calls cannot both observe ACTIVE and both write a PLAN_COMPLETED audit entry —
// exactly one wins the race (RowsAffected == 1); the other receives ErrInvalidTransition.
//
// Callers: the workspace service should invoke this (via the manual disburse endpoint or a
// dedicated "plan complete" endpoint) after confirming all milestones have been disbursed.
// Returns ErrPlanNotFound if the plan does not exist.
// Returns ErrInvalidTransition if the plan is not ACTIVE (already COMPLETED or CANCELED).
func (s *SettlementService) CompletePlan(ctx context.Context, planID uuid.UUID) error {
	// Fetch the plan first so we have multi_contract_id for the audit payload.
	// This read is non-locking; the atomic transition below races correctly via the DB guard.
	plan, err := s.plans.GetByID(ctx, planID)
	if err != nil {
		return err
	}

	// Atomic ACTIVE → COMPLETED transition: UPDATE ... WHERE id=$1 AND status='ACTIVE'.
	// If two concurrent calls both pass the GetByID check above, only one will update a row;
	// the other sees RowsAffected == 0 and receives ErrInvalidTransition from the store.
	if atomicErr := s.plans.CompletePlanAtomic(ctx, planID); atomicErr != nil {
		return atomicErr
	}

	payload, _ := json.Marshal(map[string]any{
		"multi_contract_id": plan.MultiContractID,
	})

	if auditErr := s.audit.Append(ctx, &domain.SettlementAuditEntry{
		ID:           uuid.New(),
		PlanID:       planID,
		EventType:    "PLAN_COMPLETED",
		ActorService: "payment",
		Payload:      payload,
		OccurredAt:   time.Now().UTC(),
	}); auditErr != nil {
		slog.Error("CompletePlan: failed to write PLAN_COMPLETED audit entry",
			"plan_id", planID,
			"err", auditErr)
	}

	slog.Info("settlement plan completed", "plan_id", planID)

	return nil
}

// GetPlan returns a settlement plan by ID (non-locking read).
func (s *SettlementService) GetPlan(ctx context.Context, id uuid.UUID) (*domain.SettlementPlan, error) {
	return s.plans.GetByID(ctx, id)
}

// GetPlanByContractID returns the active (non-canceled) settlement plan for a multiparty contract.
// Returns nil, nil if no plan exists (caller should treat as "no plan yet").
func (s *SettlementService) GetPlanByContractID(ctx context.Context, contractID uuid.UUID) (*domain.SettlementPlan, error) {
	return s.plans.GetByMultiContractID(ctx, contractID)
}

// GetAllocations returns all allocations for a plan ordered by created_at ASC.
func (s *SettlementService) GetAllocations(ctx context.Context, planID uuid.UUID) ([]*domain.SettlementAllocation, error) {
	return s.allocs.ListByPlanID(ctx, planID)
}

// GetMilestoneDisbursements returns all disbursement records for a (plan, milestone) pair.
func (s *SettlementService) GetMilestoneDisbursements(ctx context.Context, planID, milestoneID uuid.UUID) ([]*domain.SettlementMilestoneDisbursement, error) {
	return s.disburseStore.ListByPlanMilestone(ctx, planID, milestoneID)
}

// ─── Validation helpers ───────────────────────────────────────────────────────

func validateCreatePlanInput(in *CreatePlanInput) error {
	if in.MultiContractID == uuid.Nil {
		return fmt.Errorf("%w: multi_contract_id is required", domain.ErrValidation)
	}

	if in.TenderID == uuid.Nil {
		return fmt.Errorf("%w: tender_id is required", domain.ErrValidation)
	}

	if in.Currency == "" {
		return fmt.Errorf("%w: currency is required", domain.ErrValidation)
	}

	if in.IdempotencyKey == "" {
		return fmt.Errorf("%w: idempotency_key is required", domain.ErrValidation)
	}

	return nil
}

func validateDisburseMilestoneInput(in *DisburseMilestoneInput) error {
	if in.PlanID == uuid.Nil {
		return fmt.Errorf("%w: plan_id is required", domain.ErrValidation)
	}

	if in.MilestoneID == uuid.Nil {
		return fmt.Errorf("%w: milestone_id is required", domain.ErrValidation)
	}

	if in.Amount.LessThanOrEqual(decimal.Zero) {
		return fmt.Errorf("%w: amount must be positive", domain.ErrValidation)
	}

	// Fix #1 (Major): mirror the create-path max-amount cap so that an untrusted
	// Redis event cannot drive unbounded money movement.
	if in.Amount.GreaterThan(maxAmount) {
		return fmt.Errorf("%w: amount must not exceed 100000000.00", domain.ErrValidation)
	}

	if in.Currency == "" {
		return fmt.Errorf("%w: currency is required", domain.ErrValidation)
	}

	// Fix #2 (Major): reject currencies not in the explicit allowlist — mirrors the
	// create-path check and prevents cross-currency disbursal from a forged Redis event.
	if !allowedCurrencies[in.Currency] {
		return fmt.Errorf("%w: currency must be one of TWD, USD, EUR", domain.ErrValidation)
	}

	return nil
}

// ─── Math helpers ─────────────────────────────────────────────────────────────

// validateRosterSum triple-checks Σ shareBps == 10000 and each entry is in [0, 10000].
// Three independent evaluations prevent a single arithmetic error from silently passing.
func validateRosterSum(roster []RosterEntry) error {
	sum1 := 0
	for _, p := range roster {
		if p.ShareBps < 0 || p.ShareBps > 10000 {
			return fmt.Errorf("%w: entry share_bps=%d is out of range [0, 10000]",
				domain.ErrSumInvariantViolation, p.ShareBps)
		}

		sum1 += p.ShareBps
	}

	sum2 := 0
	for i := len(roster) - 1; i >= 0; i-- {
		sum2 += roster[i].ShareBps
	}

	sum3 := decimal.Zero
	for _, p := range roster {
		sum3 = sum3.Add(decimal.NewFromInt(int64(p.ShareBps)))
	}

	sum3Int, _ := sum3.Float64()

	if sum1 != 10000 || sum2 != 10000 || int(sum3Int) != 10000 {
		return fmt.Errorf("%w: got sum1=%d sum2=%d sum3=%.0f, need all 10000",
			domain.ErrSumInvariantViolation, sum1, sum2, sum3Int)
	}

	return nil
}

// computeAllocatedAmounts computes per-allocation amounts from the milestone total.
// The last allocation (IsRoundingSink == true) absorbs the rounding residual so
// Σ allocated == total exactly (two-decimal-place precision).
func computeAllocatedAmounts(total decimal.Decimal, allocs []*domain.SettlementAllocation) []decimal.Decimal {
	tenThousand := decimal.NewFromInt(10000)
	amounts := make([]decimal.Decimal, len(allocs))
	runningSum := decimal.Zero

	for i, a := range allocs {
		if a.IsRoundingSink || i == len(allocs)-1 {
			amounts[i] = total.Sub(runningSum)
		} else {
			bps := decimal.NewFromInt(int64(a.ShareBps))
			amounts[i] = total.Mul(bps).Div(tenThousand).RoundBank(2)
			runningSum = runningSum.Add(amounts[i])
		}
	}

	return amounts
}

// verifySumEquals triple-checks that Σ parts == expected (guard against money drift).
func verifySumEquals(parts []decimal.Decimal, expected decimal.Decimal, phase string) error {
	sum1 := decimal.Zero
	for _, p := range parts {
		sum1 = sum1.Add(p)
	}

	sum2 := decimal.Zero
	for i := len(parts) - 1; i >= 0; i-- {
		sum2 = sum2.Add(parts[i])
	}

	sum3 := sum1.RoundBank(2)

	if !sum1.Equal(expected) || !sum2.Equal(expected) || !sum3.Equal(expected) {
		return fmt.Errorf("%w: %s sum1=%s sum2=%s sum3=%s expected=%s",
			domain.ErrSumInvariantViolation, phase,
			sum1.StringFixed(2), sum2.StringFixed(2), sum3.StringFixed(2),
			expected.StringFixed(2))
	}

	return nil
}
