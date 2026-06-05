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

// WorkspaceRosterClient fetches the frozen ACTIVE-party roster from the workspace service.
// Abstracted so tests can inject a stub without a real HTTP server.
type WorkspaceRosterClient interface {
	// GetPartyRoster calls GET <workspace>/internal/v1/contracts/:id/parties
	// and returns the frozen [{vendorUserId, shareBps}] roster.
	GetPartyRoster(ctx context.Context, contractID uuid.UUID) ([]RosterEntry, error)
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
		TotalAmount:      decimal.Zero,
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

// persistPlan atomically writes the plan, allocations, and PLAN_CREATED audit entry.
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

		payload, _ := json.Marshal(map[string]any{
			"multi_contract_id":  in.MultiContractID,
			"tender_id":          in.TenderID,
			"frozen_party_count": len(allocationRows),
		})

		return audit.Append(ctx, &domain.SettlementAuditEntry{
			ID:           uuid.New(),
			PlanID:       plan.ID,
			EventType:    "PLAN_CREATED",
			ActorService: "payment",
			Payload:      payload,
			OccurredAt:   time.Now().UTC(),
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

// DisburseMilestone disburses a milestone payout to all frozen allocations of a plan.
//
// Algorithm (per-milestone model — fixes multi-milestone bug):
//  1. SELECT FOR UPDATE on the plan (TOCTOU serialization).
//  2. SELECT FOR UPDATE on all allocations (frozen roster).
//  3. For each allocation: compute amount × shareBps / 10000 (last absorbs rounding).
//  4. Idempotency: check settlement_milestone_disbursements for (plan, milestone, vendor);
//     if already DISBURSED → genuine skip (return success WITHOUT re-creating).
//  5. Create one settlement_milestone_disbursements row + one transactions row per vendor
//     in the SAME transaction (atomicity — Critical fix).
//  6. Partial-failure: a failed vendor gets status FAILED; others still disburse.
//  7. Triple sum-check pre-tx and post-tx.
func (s *SettlementService) DisburseMilestone(ctx context.Context, in *DisburseMilestoneInput) error {
	if err := validateDisburseMilestoneInput(in); err != nil {
		return err
	}

	var disbursedAmounts []decimal.Decimal

	var anyFailed bool

	txErr := s.txMgr.WithSettlementTx(ctx, func(
		ctx context.Context,
		plans store.TxSettlementPlanStore,
		allocs store.TxSettlementAllocationStore,
		disbursements store.SettlementMilestoneDisbursementStore,
		txTxStore store.TransactionStore,
		audit store.SettlementAuditStore,
	) error {
		return s.disburseMilestoneTx(ctx, in, plans, allocs, disbursements, txTxStore, audit, &disbursedAmounts, &anyFailed)
	})

	if txErr != nil {
		return txErr
	}

	return s.postDisburseCheck(in, disbursedAmounts, anyFailed)
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
	disbursedAmounts *[]decimal.Decimal,
	anyFailed *bool,
) error {
	plan, lockErr := plans.GetByIDForUpdate(ctx, in.PlanID)
	if lockErr != nil {
		return lockErr
	}

	if plan.Status == domain.PlanStatusCanceled {
		return fmt.Errorf("%w: plan is CANCELED", domain.ErrInvalidTransition)
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

	for i, alloc := range lockedAllocs {
		ok, disbursed := s.disburseAllocation(ctx, in, disbursements, txTxStore, audit, plan, alloc, amounts[i])
		if ok {
			*disbursedAmounts = append(*disbursedAmounts, disbursed)
		} else {
			*anyFailed = true
		}
	}

	return nil
}

// disburseAllocation disburses a single allocation using the per-milestone disbursement model.
// Returns (true, amount) on success or idempotent skip, (false, zero) on failure.
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
) (bool, decimal.Decimal) {
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
		slog.Error("create milestone disbursement row failed; marking FAILED",
			"plan_id", in.PlanID,
			"milestone_id", in.MilestoneID,
			"vendor_user_id", alloc.VendorUserID,
			"err", createErr)

		return false, decimal.Zero
	}

	if !inserted {
		// Row already existed — idempotent skip (rev Critical 2 fix).
		// Return success without re-creating to avoid orphan PENDING rows.
		slog.Info("per-milestone disbursement already exists; skipping (idempotent)",
			"plan_id", in.PlanID,
			"milestone_id", in.MilestoneID,
			"vendor_user_id", alloc.VendorUserID)

		return true, allocAmt
	}

	// Self-transfer guard (Critical fix): PayerUserID = platform system account,
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
	if createErr := txTxStore.Create(ctx, txRow); createErr != nil {
		if errors.Is(createErr, domain.ErrDuplicateKey) {
			// transactions row already exists — update disbursement to DISBURSED and skip.
			// This handles a rare race where the disbursement PENDING was created but the
			// tx create was retried.
			slog.Info("transactions row already exists; marking disbursement DISBURSED (idempotent)",
				"plan_id", in.PlanID,
				"milestone_id", in.MilestoneID,
				"vendor_user_id", alloc.VendorUserID)

			return true, allocAmt
		}

		slog.Error("create transaction row failed; marking disbursement FAILED",
			"plan_id", in.PlanID,
			"milestone_id", in.MilestoneID,
			"vendor_user_id", alloc.VendorUserID,
			"err", createErr)

		if updErr := disbursements.UpdateStatus(ctx, disburseRow.ID, domain.MilestoneDisbursementStatusFailed, nil); updErr != nil {
			slog.Error("failed to mark disbursement FAILED", "disbursement_id", disburseRow.ID, "err", updErr)
		}

		return false, decimal.Zero
	}

	txID := txRow.ID

	// Update disbursement record to DISBURSED with the tx_id.
	if updErr := disbursements.UpdateStatus(ctx, disburseRow.ID, domain.MilestoneDisbursementStatusDisbursed, &txID); updErr != nil {
		slog.Error("update disbursement to DISBURSED failed",
			"disbursement_id", disburseRow.ID, "err", updErr)

		return false, decimal.Zero
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

	return true, allocAmt
}

// postDisburseCheck runs post-tx sum verification and logs final outcome.
func (s *SettlementService) postDisburseCheck(
	in *DisburseMilestoneInput,
	disbursedAmounts []decimal.Decimal,
	anyFailed bool,
) error {
	if !anyFailed && len(disbursedAmounts) > 0 {
		if err := verifySumEquals(disbursedAmounts, in.Amount, "post-disburse"); err != nil {
			slog.Error("POST-DISBURSE SUM INVARIANT VIOLATED — possible money-drift",
				"plan_id", in.PlanID,
				"milestone_id", in.MilestoneID,
				"err", err)

			return err
		}
	}

	if anyFailed {
		slog.Warn("milestone disburse completed with partial failures; plan remains ACTIVE for re-trigger",
			"plan_id", in.PlanID,
			"milestone_id", in.MilestoneID)
	} else {
		slog.Info("milestone disburse completed successfully",
			"plan_id", in.PlanID,
			"milestone_id", in.MilestoneID,
			"amount", in.Amount.StringFixed(2))
	}

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

	if in.Currency == "" {
		return fmt.Errorf("%w: currency is required", domain.ErrValidation)
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
