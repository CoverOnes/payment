package httpx

import (
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/CoverOnes/payment/internal/domain"
	"github.com/stretchr/testify/assert"
)

// TestTranslate_ErrValidation_ReturnsSafeMessage verifies Fix #5 (Major):
// translate() must return a fixed client-safe string "validation error" for
// domain.ErrValidation, not err.Error(). The wrapped error string can contain
// internal IDs and invariants that must not leak to clients.
func TestTranslate_ErrValidation_ReturnsSafeMessage(t *testing.T) {
	table := []struct {
		name    string
		err     error
		wantMsg string
	}{
		{
			name:    "bare ErrValidation",
			err:     domain.ErrValidation,
			wantMsg: "validation error",
		},
		{
			name:    "wrapped ErrValidation with internal detail",
			err:     fmt.Errorf("%w: party roster is empty for contract %s", domain.ErrValidation, "some-internal-uuid"),
			wantMsg: "validation error",
		},
		{
			name:    "double-wrapped ErrValidation",
			err:     fmt.Errorf("outer: %w", fmt.Errorf("%w: frozen_party_count %d != %d", domain.ErrValidation, 3, 2)),
			wantMsg: "validation error",
		},
	}

	for _, tc := range table {
		t.Run(tc.name, func(t *testing.T) {
			code, status, message, details := translate(tc.err)
			assert.Equal(t, "VALIDATION_ERROR", code)
			assert.Equal(t, http.StatusBadRequest, status)
			assert.Equal(t, "validation error", message, "must NOT contain err.Error() content")
			assert.Nil(t, details)

			// Ensure the raw error content is NOT present in the message.
			if tc.err != domain.ErrValidation {
				assert.NotContains(t, message, tc.err.Error(),
					"client-facing message must not contain internal error details")
			}
		})
	}
}

// TestTranslate_OtherErrors verifies that other domain errors map to their correct
// HTTP codes and that the internal server error case returns a fixed message too.
func TestTranslate_OtherErrors(t *testing.T) {
	table := []struct {
		name       string
		err        error
		wantCode   string
		wantStatus int
		wantMsg    string
	}{
		{
			name:       "ErrTransactionNotFound",
			err:        domain.ErrTransactionNotFound,
			wantCode:   "TRANSACTION_NOT_FOUND",
			wantStatus: http.StatusNotFound,
			wantMsg:    "transaction not found",
		},
		{
			name:       "ErrInvalidTransition",
			err:        domain.ErrInvalidTransition,
			wantCode:   "INVALID_TRANSITION",
			wantStatus: http.StatusUnprocessableEntity,
			wantMsg:    "invalid state transition",
		},
		{
			name:       "ErrDuplicateKey",
			err:        domain.ErrDuplicateKey,
			wantCode:   "DUPLICATE_IDEMPOTENCY_KEY",
			wantStatus: http.StatusConflict,
			wantMsg:    "idempotency key already exists",
		},
		{
			name:       "ErrForbidden",
			err:        domain.ErrForbidden,
			wantCode:   "FORBIDDEN",
			wantStatus: http.StatusForbidden,
			wantMsg:    "forbidden",
		},
		{
			name:       "ErrUnauthorized",
			err:        domain.ErrUnauthorized,
			wantCode:   "UNAUTHORIZED",
			wantStatus: http.StatusUnauthorized,
			wantMsg:    "unauthorized",
		},
		{
			name:       "unknown error — internal server error",
			err:        errors.New("db connection lost"),
			wantCode:   "INTERNAL_ERROR",
			wantStatus: http.StatusInternalServerError,
			wantMsg:    "internal server error",
		},
		{
			name:       "unknown error — message must not leak raw error",
			err:        errors.New("password=supersecret conn refused"),
			wantCode:   "INTERNAL_ERROR",
			wantStatus: http.StatusInternalServerError,
			wantMsg:    "internal server error",
		},
	}

	for _, tc := range table {
		t.Run(tc.name, func(t *testing.T) {
			code, status, message, details := translate(tc.err)
			assert.Equal(t, tc.wantCode, code)
			assert.Equal(t, tc.wantStatus, status)
			assert.Equal(t, tc.wantMsg, message)
			assert.Nil(t, details)
		})
	}
}
