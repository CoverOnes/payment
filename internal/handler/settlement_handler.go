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
// consumer failed or a partial-failure allocation needs re-triggering.
//
// Auth: RequireValidIdentity + RequireTier(3) + VerifyGatewaySignature (from router group)
// AND RequireServiceIdentity (settlement S2S token — workspace or ops caller).
//
// Response discriminants:
//   - 200 {status:"disbursed"}          — all vendors paid (or idempotently skipped).
//   - 207 {status:"partial_failure"}    — some vendors paid, some failed; per-vendor outcomes included.
//   - 502 {status:"failed"}             — every vendor failed; no money moved this call.
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

	// Build an honest response based on per-vendor outcomes.
	// All succeeded (or idempotently skipped) → 200 disbursed.
	// Some failed → 207 partial_failure with per-vendor breakdown.
	// All failed → 502 failed (no money moved this call).
	base := gin.H{
		"planId":      planID,
		"milestoneId": milestoneID,
		"vendors":     result.Outcomes,
	}

	switch {
	case result.FailedCount == 0:
		base["status"] = "disbursed"
		c.JSON(http.StatusOK, gin.H{"data": base})

	case result.DisbursedCount > 0:
		base["status"] = "partial_failure"
		c.JSON(http.StatusMultiStatus, gin.H{"data": base})

	default:
		base["status"] = "failed"
		c.JSON(http.StatusBadGateway, gin.H{"data": base})
	}
}
