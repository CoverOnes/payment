package handler

import (
	"context"
	"io"
	"net/http"

	"github.com/CoverOnes/payment/internal/platform/httpx"
	"github.com/CoverOnes/payment/internal/platform/middleware"
	"github.com/CoverOnes/payment/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/shopspring/decimal"
)

// maxDisburseIdempotencyKeyLen caps the disburse idempotency key to prevent oversized inputs.
// The field is still accepted for forward-compat but is never forwarded to the service.
const maxDisburseIdempotencyKeyLen = 255

// SettlementDisburser is the minimal interface the Disburse handler needs.
// Defined here to allow handler-level unit tests to inject stubs without a DB.
type SettlementDisburser interface {
	DisburseMilestone(ctx context.Context, in *service.DisburseMilestoneInput) (*service.DisburseResult, error)
	CompletePlan(ctx context.Context, planID uuid.UUID) error
}

// SettlementHandler handles settlement-related HTTP endpoints.
type SettlementHandler struct {
	svc SettlementDisburser
}

// NewSettlementHandler returns a SettlementHandler backed by the concrete service.
func NewSettlementHandler(svc *service.SettlementService) *SettlementHandler {
	return &SettlementHandler{svc: svc}
}

// NewSettlementHandlerForTest returns a SettlementHandler with an injected stub.
// For use in handler unit tests only — not part of the production API.
func NewSettlementHandlerForTest(svc SettlementDisburser) *SettlementHandler {
	return &SettlementHandler{svc: svc}
}

// disburseRequest is the request body for POST /v1/settlement/plans/:id/disburse.
//
// IdempotencyKey notes:
//   - The field is accepted for forward-compat and to avoid breaking existing callers.
//   - It is NOT forwarded to the service: idempotency is content-addressed automatically
//     by (plan_id, milestone_id, vendor_user_id) via the UNIQUE index on
//     settlement_milestone_disbursements. Sending any value (or omitting the field)
//     produces identical outcomes.
//   - Validation (required / max-length) has been removed — validating a field that is
//     never used would mislead callers into believing the key affects behavior.
type disburseRequest struct {
	MilestoneID    string `json:"milestoneId"`
	Amount         string `json:"amount"`         // decimal string e.g. "3000.00"
	Currency       string `json:"currency"`       // ISO 4217; defaults to "TWD"
	IdempotencyKey string `json:"idempotencyKey"` // accepted but no-op — see struct doc
}

// Disburse handles POST /v1/settlement/plans/:id/disburse.
// This is a manual re-trigger for a milestone disburse — used when the event
// consumer failed or needs re-triggering.
//
// Auth: RequireValidIdentity + RequireTier(3) + VerifyGatewaySignature (from router group)
// AND RequireServiceIdentity (settlement S2S token — workspace or ops caller).
//
// Fix #3 (Critical): all-or-nothing model. The 207 partial_failure and 502 paths have
// been removed because they were structurally unreachable in production: a real DB error
// aborts the shared pgx transaction, making "some vendors paid, some failed" an illusion.
// DisburseMilestone now returns an error on any vendor failure, rolling back all vendors.
//
// Response discriminants:
//   - 200 {status:"disbursed"}  — all vendors paid (or idempotently skipped).
//   - 4xx/5xx via httpx.Err     — any error rolls back the entire disbursement.
//
// IdempotencyKey in the request body is accepted but has no effect — see disburseRequest doc.
func (h *SettlementHandler) Disburse(c *gin.Context) {
	planID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		httpx.ErrCode(c, http.StatusBadRequest, "VALIDATION_ERROR", "plan id must be a valid UUID")
		return
	}

	// Body limit — DoS prevention (backend-security-design).
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxBodyBytes)

	var req disburseRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		if err == io.EOF {
			httpx.ErrCode(c, http.StatusBadRequest, "VALIDATION_ERROR", "request body is required")
			return
		}

		httpx.ErrCode(c, http.StatusBadRequest, "VALIDATION_ERROR", "invalid request body")

		return
	}

	// IdempotencyKey length is still capped to prevent abuse (even though the value is unused).
	if len(req.IdempotencyKey) > maxDisburseIdempotencyKeyLen {
		httpx.ErrCode(c, http.StatusBadRequest, "VALIDATION_ERROR", "idempotencyKey too long")
		return
	}

	milestoneID, err := uuid.Parse(req.MilestoneID)
	if err != nil {
		httpx.ErrCode(c, http.StatusBadRequest, "VALIDATION_ERROR", "milestoneId must be a valid UUID")
		return
	}

	amount, err := decimal.NewFromString(req.Amount)
	if err != nil || amount.LessThanOrEqual(decimal.Zero) {
		httpx.ErrCode(c, http.StatusBadRequest, "VALIDATION_ERROR", "amount must be a positive decimal string")
		return
	}

	currency := req.Currency
	if currency == "" {
		currency = "TWD"
	}

	actorService := middleware.ServiceIDFromCtx(c)
	if actorService == "" {
		actorService = "manual-disburse"
	}

	result, err := h.svc.DisburseMilestone(c.Request.Context(), &service.DisburseMilestoneInput{
		PlanID:       planID,
		MilestoneID:  milestoneID,
		Amount:       amount,
		Currency:     currency,
		ActorService: actorService,
	})
	if err != nil {
		httpx.Err(c, err)
		return
	}

	// Fix #3 (Critical): all-or-nothing — on success all vendors are paid.
	// Any vendor error causes DisburseMilestone to return an error (handled above)
	// and rolls back the entire pgx transaction, so we only reach here on full success.
	c.JSON(http.StatusOK, gin.H{"data": gin.H{
		"planId":      planID,
		"milestoneId": milestoneID,
		"vendors":     result.Outcomes,
		"status":      "disbursed",
	}})
}

// CompletePlan handles POST /v1/settlement/plans/:id/complete.
// Transitions the plan from ACTIVE to COMPLETED and writes a PLAN_COMPLETED audit entry.
//
// Auth: RequireValidIdentity + RequireTier(3) + VerifyGatewaySignature (from router group)
// AND RequireServiceIdentity (settlement S2S token — workspace or ops caller).
//
// CompletePlan wiring: this endpoint must be called by the workspace service (or an ops
// operator) after confirming all milestones have been disbursed. The consumer-call approach
// was not chosen because the contract_completed event does not carry an "isLastMilestone"
// signal, so there is no reliable way for the payment service to detect completion autonomously.
//
// Response discriminants:
//   - 200 {status:"completed"} — plan transitioned to COMPLETED.
//   - 404 via httpx.Err        — plan not found.
//   - 409 via httpx.Err        — plan is not ACTIVE (ErrInvalidTransition).
func (h *SettlementHandler) CompletePlan(c *gin.Context) {
	planID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		httpx.ErrCode(c, http.StatusBadRequest, "VALIDATION_ERROR", "plan id must be a valid UUID")
		return
	}

	if err := h.svc.CompletePlan(c.Request.Context(), planID); err != nil {
		httpx.Err(c, err)
		return
	}

	c.JSON(http.StatusOK, gin.H{"data": gin.H{
		"planId": planID,
		"status": "completed",
	}})
}
