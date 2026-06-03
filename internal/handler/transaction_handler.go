package handler

import (
	"io"
	"net/http"

	"github.com/CoverOnes/payment/internal/domain"
	"github.com/CoverOnes/payment/internal/platform/httpx"
	"github.com/CoverOnes/payment/internal/platform/middleware"
	"github.com/CoverOnes/payment/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/shopspring/decimal"
)

const maxBodyBytes = 1 << 20 // 1 MiB body limit (DoS prevention)

// TransactionHandler handles transaction HTTP endpoints.
type TransactionHandler struct {
	svc *service.TransactionService
}

// NewTransactionHandler returns a TransactionHandler.
func NewTransactionHandler(svc *service.TransactionService) *TransactionHandler {
	return &TransactionHandler{svc: svc}
}

// createRequest is the request body for POST /v1/transactions.
type createRequest struct {
	PayeeUserID string  `json:"payeeUserId"`
	ContractID  *string `json:"contractId"`
	MilestoneID *string `json:"milestoneId"`
	Amount      string  `json:"amount"`   // decimal string e.g. "1500.00" — never float
	Currency    string  `json:"currency"` // ISO 4217, default "TWD"
}

// Create handles POST /v1/transactions.
// RequireTier(3) enforced in router. Idempotency-Key header is required.
// payer = X-User-Id (never from body — IDOR protection).
func (h *TransactionHandler) Create(c *gin.Context) {
	identity, ok := middleware.IdentityFromCtx(c)
	if !ok {
		httpx.ErrCode(c, http.StatusUnauthorized, "UNAUTHORIZED", "authentication required")
		return
	}

	idempotencyKey := c.GetHeader("Idempotency-Key")
	if idempotencyKey == "" {
		httpx.ErrCode(c, http.StatusBadRequest, "VALIDATION_ERROR", "Idempotency-Key header is required")
		return
	}

	// Body limit — DoS prevention (backend-security-design).
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxBodyBytes)

	var req createRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		if err == io.EOF {
			httpx.ErrCode(c, http.StatusBadRequest, "VALIDATION_ERROR", "request body is required")
			return
		}

		httpx.ErrCode(c, http.StatusBadRequest, "VALIDATION_ERROR", "invalid request body")
		return
	}

	// Parse payee UUID.
	payeeID, err := uuid.Parse(req.PayeeUserID)
	if err != nil {
		httpx.ErrCode(c, http.StatusBadRequest, "VALIDATION_ERROR", "payeeUserId must be a valid UUID")
		return
	}

	// Parse amount as decimal — never float.
	amount, err := decimal.NewFromString(req.Amount)
	if err != nil {
		httpx.ErrCode(c, http.StatusBadRequest, "VALIDATION_ERROR", "amount must be a valid decimal string")
		return
	}

	currency := req.Currency
	if currency == "" {
		currency = "TWD"
	}

	var contractID *uuid.UUID

	if req.ContractID != nil {
		parsed, parseErr := uuid.Parse(*req.ContractID)
		if parseErr != nil {
			httpx.ErrCode(c, http.StatusBadRequest, "VALIDATION_ERROR", "contractId must be a valid UUID")
			return
		}

		contractID = &parsed
	}

	var milestoneID *uuid.UUID

	if req.MilestoneID != nil {
		parsed, parseErr := uuid.Parse(*req.MilestoneID)
		if parseErr != nil {
			httpx.ErrCode(c, http.StatusBadRequest, "VALIDATION_ERROR", "milestoneId must be a valid UUID")
			return
		}

		milestoneID = &parsed
	}

	tx, err := h.svc.Create(c.Request.Context(), &service.CreateRequest{
		PayerUserID:    identity.UserID,
		PayeeUserID:    payeeID,
		ContractID:     contractID,
		MilestoneID:    milestoneID,
		Amount:         amount,
		Currency:       currency,
		IdempotencyKey: idempotencyKey,
	})
	if err != nil {
		httpx.Err(c, err)
		return
	}

	httpx.Created(c, toTransactionResponse(tx))
}

// Release handles POST /v1/transactions/:id/release.
// RequireTier(3) enforced in router.
func (h *TransactionHandler) Release(c *gin.Context) {
	identity, ok := middleware.IdentityFromCtx(c)
	if !ok {
		httpx.ErrCode(c, http.StatusUnauthorized, "UNAUTHORIZED", "authentication required")
		return
	}

	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		httpx.ErrCode(c, http.StatusBadRequest, "VALIDATION_ERROR", "id must be a valid UUID")
		return
	}

	tx, err := h.svc.Release(c.Request.Context(), id, identity.UserID)
	if err != nil {
		httpx.Err(c, err)
		return
	}

	httpx.OK(c, toTransactionResponse(tx))
}

// Refund handles POST /v1/transactions/:id/refund.
// RequireTier(3) enforced in router.
func (h *TransactionHandler) Refund(c *gin.Context) {
	identity, ok := middleware.IdentityFromCtx(c)
	if !ok {
		httpx.ErrCode(c, http.StatusUnauthorized, "UNAUTHORIZED", "authentication required")
		return
	}

	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		httpx.ErrCode(c, http.StatusBadRequest, "VALIDATION_ERROR", "id must be a valid UUID")
		return
	}

	tx, err := h.svc.Refund(c.Request.Context(), id, identity.UserID)
	if err != nil {
		httpx.Err(c, err)
		return
	}

	httpx.OK(c, toTransactionResponse(tx))
}

// GetByID handles GET /v1/transactions/:id.
// IDOR: returns 404 if the caller is neither payer nor payee.
// Never returns 403 on ownership mismatch (no existence leak).
func (h *TransactionHandler) GetByID(c *gin.Context) {
	identity, ok := middleware.IdentityFromCtx(c)
	if !ok {
		httpx.ErrCode(c, http.StatusUnauthorized, "UNAUTHORIZED", "authentication required")
		return
	}

	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		httpx.ErrCode(c, http.StatusBadRequest, "VALIDATION_ERROR", "id must be a valid UUID")
		return
	}

	tx, err := h.svc.GetByID(c.Request.Context(), id)
	if err != nil {
		httpx.Err(c, err)
		return
	}

	// IDOR: caller must be payer or payee — return 404 on ownership mismatch (no existence leak).
	if tx.PayerUserID != identity.UserID && tx.PayeeUserID != identity.UserID {
		httpx.ErrCode(c, http.StatusNotFound, "TRANSACTION_NOT_FOUND", "transaction not found")
		return
	}

	httpx.OK(c, toTransactionResponse(tx))
}

// ListMyTransactions handles GET /v1/me/transactions.
// Returns all transactions where X-User-Id is payer or payee.
func (h *TransactionHandler) ListMyTransactions(c *gin.Context) {
	identity, ok := middleware.IdentityFromCtx(c)
	if !ok {
		httpx.ErrCode(c, http.StatusUnauthorized, "UNAUTHORIZED", "authentication required")
		return
	}

	txs, err := h.svc.ListByUserID(c.Request.Context(), identity.UserID)
	if err != nil {
		httpx.Err(c, err)
		return
	}

	// Return empty array, never null.
	result := make([]transactionResponse, 0, len(txs))

	for _, tx := range txs {
		result = append(result, toTransactionResponse(tx))
	}

	httpx.OK(c, result)
}

// transactionResponse is the API response shape for transaction endpoints.
type transactionResponse struct {
	ID             uuid.UUID     `json:"id"`
	PayerUserID    uuid.UUID     `json:"payerUserId"`
	PayeeUserID    uuid.UUID     `json:"payeeUserId"`
	ContractID     *uuid.UUID    `json:"contractId,omitempty"`
	MilestoneID    *uuid.UUID    `json:"milestoneId,omitempty"`
	Amount         string        `json:"amount"` // decimal string — never float
	Currency       string        `json:"currency"`
	Status         domain.Status `json:"status"`
	IdempotencyKey string        `json:"idempotencyKey"`
	CreatedAt      string        `json:"createdAt"` // RFC3339
	UpdatedAt      string        `json:"updatedAt"` // RFC3339
}

func toTransactionResponse(tx *domain.Transaction) transactionResponse {
	return transactionResponse{
		ID:             tx.ID,
		PayerUserID:    tx.PayerUserID,
		PayeeUserID:    tx.PayeeUserID,
		ContractID:     tx.ContractID,
		MilestoneID:    tx.MilestoneID,
		Amount:         tx.Amount.StringFixed(2),
		Currency:       tx.Currency,
		Status:         tx.Status,
		IdempotencyKey: tx.IdempotencyKey,
		CreatedAt:      tx.CreatedAt.UTC().Format("2006-01-02T15:04:05Z07:00"),
		UpdatedAt:      tx.UpdatedAt.UTC().Format("2006-01-02T15:04:05Z07:00"),
	}
}
