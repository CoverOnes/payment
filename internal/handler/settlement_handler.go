package handler

import (
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
const maxDisburseIdempotencyKeyLen = 255

// SettlementHandler handles settlement-related HTTP endpoints.
type SettlementHandler struct {
	svc *service.SettlementService
}

// NewSettlementHandler returns a SettlementHandler.
func NewSettlementHandler(svc *service.SettlementService) *SettlementHandler {
	return &SettlementHandler{svc: svc}
}

// disburseRequest is the request body for POST /v1/settlement/plans/:id/disburse.
type disburseRequest struct {
	MilestoneID    string `json:"milestoneId"`
	Amount         string `json:"amount"`   // decimal string e.g. "3000.00"
	Currency       string `json:"currency"` // ISO 4217; defaults to "TWD"
	IdempotencyKey string `json:"idempotencyKey"`
}

// Disburse handles POST /v1/settlement/plans/:id/disburse.
// This is a manual re-trigger for a milestone disburse — used when the event
// consumer failed or a partial-failure allocation needs re-triggering.
//
// Auth: RequireValidIdentity + RequireTier(3) + VerifyGatewaySignature (from router group)
// AND RequireServiceIdentity (settlement S2S token — workspace or ops caller).
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

	milestoneID, err := uuid.Parse(req.MilestoneID)
	if err != nil {
		httpx.ErrCode(c, http.StatusBadRequest, "VALIDATION_ERROR", "milestoneId must be a valid UUID")
		return
	}

	if req.IdempotencyKey == "" {
		httpx.ErrCode(c, http.StatusBadRequest, "VALIDATION_ERROR", "idempotencyKey is required")
		return
	}

	if len(req.IdempotencyKey) > maxDisburseIdempotencyKeyLen {
		httpx.ErrCode(c, http.StatusBadRequest, "VALIDATION_ERROR", "idempotencyKey too long")
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

	if err := h.svc.DisburseMilestone(c.Request.Context(), &service.DisburseMilestoneInput{
		PlanID:               planID,
		MilestoneID:          milestoneID,
		Amount:               amount,
		Currency:             currency,
		IdempotencyKeySuffix: req.IdempotencyKey,
		ActorService:         actorService,
	}); err != nil {
		httpx.Err(c, err)
		return
	}

	httpx.OK(c, gin.H{"planId": planID, "milestoneId": milestoneID, "status": "disbursed"})
}
