package httpx

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/CoverOnes/payment/internal/domain"
	"github.com/gin-gonic/gin"
)

// ErrorResponse is the machine-readable error envelope.
type ErrorResponse struct {
	Error ErrorBody `json:"error"`
}

// ErrorBody carries the stable code, human message, and optional details.
type ErrorBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Details any    `json:"details,omitempty"`
}

// Err sends a structured error response, translating domain errors to HTTP codes.
func Err(c *gin.Context, err error) {
	code, status, message, details := translate(err)
	c.JSON(status, ErrorResponse{Error: ErrorBody{
		Code:    code,
		Message: message,
		Details: details,
	}})
}

// ErrCode sends a raw code/status/message triple (for handler-generated errors
// that don't map cleanly to domain sentinels).
func ErrCode(c *gin.Context, status int, code, message string, details ...any) {
	var d any
	if len(details) > 0 {
		d = details[0]
	}

	c.JSON(status, ErrorResponse{Error: ErrorBody{
		Code:    code,
		Message: message,
		Details: d,
	}})
}

// translate maps domain / sentinel errors to HTTP status + machine code.
//
//nolint:unparam // details: reserved for structured error payload; always nil today but part of the stable contract
func translate(err error) (code string, status int, message string, details any) {
	switch {
	case errors.Is(err, domain.ErrTransactionNotFound):
		return "TRANSACTION_NOT_FOUND", http.StatusNotFound, "transaction not found", nil

	case errors.Is(err, domain.ErrPlanNotFound):
		return "PLAN_NOT_FOUND", http.StatusNotFound, "settlement plan not found", nil

	case errors.Is(err, domain.ErrAllocationNotFound):
		return "NOT_FOUND", http.StatusNotFound, "allocation not found", nil

	case errors.Is(err, domain.ErrMilestoneDisbursementNotFound):
		return "NOT_FOUND", http.StatusNotFound, "milestone disbursement not found", nil

	case errors.Is(err, domain.ErrInvalidTransition):
		return "INVALID_TRANSITION", http.StatusUnprocessableEntity, "invalid state transition", nil

	case errors.Is(err, domain.ErrDuplicateKey):
		return "DUPLICATE_IDEMPOTENCY_KEY", http.StatusConflict, "idempotency key already exists", nil

	case errors.Is(err, domain.ErrForbidden):
		return "FORBIDDEN", http.StatusForbidden, "forbidden", nil

	case errors.Is(err, domain.ErrUnauthorized):
		return "UNAUTHORIZED", http.StatusUnauthorized, "unauthorized", nil

	case errors.Is(err, domain.ErrValidation):
		// Fix #5 (Major): return a fixed client-safe message instead of err.Error().
		// The wrapped error string can contain internal IDs and invariants
		// (e.g. "party roster is empty for contract <uuid>", "frozen_party_count %d != %d").
		// The full detail is logged below via slog so it is still observable server-side.
		slog.Warn("validation error", "err", err)

		return "VALIDATION_ERROR", http.StatusBadRequest, "validation error", nil

	default:
		slog.Error("unhandled internal error", "err", err)
		return "INTERNAL_ERROR", http.StatusInternalServerError, "internal server error", nil
	}
}
