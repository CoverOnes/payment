package handler_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/CoverOnes/payment/internal/handler"
	"github.com/CoverOnes/payment/internal/service"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	// testS2SToken is a valid 32-char token for tests.
	testS2SToken = "test-settlement-s2s-token-32chars"
	// testHMACSecret is empty in dev (verification disabled).
	testHMACSecret = ""
	// testPlanID is a fixed UUID used across handler tests.
	testPlanID = "00000000-0000-0000-0000-000000000002"
)

// buildTestRouter returns a test Gin engine wired with a nil-store-backed SettlementService
// for auth-gating tests (no DB calls reach the DB).
func buildTestRouter() http.Handler {
	return handler.NewRouter(handler.RouterConfig{
		TransactionSvc:     nil,
		SettlementSvc:      &service.SettlementService{},
		Pool:               nil,
		Redis:              nil,
		GatewayHMACSecret:  testHMACSecret,
		SettlementS2SToken: testS2SToken,
	})
}

func disburseBody(t *testing.T) *bytes.Reader {
	t.Helper()

	body := map[string]any{
		"milestoneId":    "00000000-0000-0000-0000-000000000001",
		"amount":         "1000.00",
		"currency":       "TWD",
		"idempotencyKey": "test-key-001",
	}

	data, err := json.Marshal(body)
	require.NoError(t, err)

	return bytes.NewReader(data)
}

// TestSettlementHandler_Disburse_ForgedIdentity verifies that a request without
// a valid X-User-Id (forged identity) is rejected with 401 before reaching the handler.
func TestSettlementHandler_Disburse_ForgedIdentity(t *testing.T) {
	r := buildTestRouter()

	req := httptest.NewRequestWithContext(
		context.Background(),
		http.MethodPost,
		"/v1/settlement/plans/"+testPlanID+"/disburse",
		disburseBody(t),
	)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Service-Id", "workspace")
	req.Header.Set("X-Service-Token", testS2SToken)
	// NO X-User-Id — forged identity attempt.

	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	assert.Equal(t, http.StatusUnauthorized, w.Code, "forged identity (no X-User-Id) must return 401")
}

// TestSettlementHandler_Disburse_MissingServiceToken verifies that a caller without
// X-Service-Token is rejected with 401 even with a valid identity.
func TestSettlementHandler_Disburse_MissingServiceToken(t *testing.T) {
	r := buildTestRouter()

	req := httptest.NewRequestWithContext(
		context.Background(),
		http.MethodPost,
		"/v1/settlement/plans/"+testPlanID+"/disburse",
		disburseBody(t),
	)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-User-Id", "00000000-0000-0000-0000-000000000003")
	req.Header.Set("X-Kyc-Tier", "3")
	// X-Service-Token intentionally omitted.

	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	assert.Equal(t, http.StatusUnauthorized, w.Code, "missing service token must return 401")
}

// TestSettlementHandler_Disburse_WrongServiceToken verifies that an incorrect token returns 401.
func TestSettlementHandler_Disburse_WrongServiceToken(t *testing.T) {
	r := buildTestRouter()

	req := httptest.NewRequestWithContext(
		context.Background(),
		http.MethodPost,
		"/v1/settlement/plans/"+testPlanID+"/disburse",
		disburseBody(t),
	)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-User-Id", "00000000-0000-0000-0000-000000000003")
	req.Header.Set("X-Kyc-Tier", "3")
	req.Header.Set("X-Service-Id", "workspace")
	req.Header.Set("X-Service-Token", "wrong-token-that-does-not-match")

	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	assert.Equal(t, http.StatusUnauthorized, w.Code, "wrong service token must return 401")
}

// TestSettlementHandler_Disburse_InsufficientTier verifies that KYC tier < 3 returns 403.
func TestSettlementHandler_Disburse_InsufficientTier(t *testing.T) {
	r := buildTestRouter()

	req := httptest.NewRequestWithContext(
		context.Background(),
		http.MethodPost,
		"/v1/settlement/plans/"+testPlanID+"/disburse",
		disburseBody(t),
	)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-User-Id", "00000000-0000-0000-0000-000000000003")
	req.Header.Set("X-Kyc-Tier", "1") // tier 1, not 3
	req.Header.Set("X-Service-Id", "workspace")
	req.Header.Set("X-Service-Token", testS2SToken)

	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	assert.Equal(t, http.StatusForbidden, w.Code, "tier 1 must return 403")
}

// TestSettlementHandler_Disburse_InvalidPlanID verifies that a non-UUID plan ID returns 400.
func TestSettlementHandler_Disburse_InvalidPlanID(t *testing.T) {
	r := buildTestRouter()

	req := httptest.NewRequestWithContext(
		context.Background(),
		http.MethodPost,
		"/v1/settlement/plans/not-a-uuid/disburse",
		disburseBody(t),
	)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-User-Id", "00000000-0000-0000-0000-000000000003")
	req.Header.Set("X-Kyc-Tier", "3")
	req.Header.Set("X-Service-Id", "workspace")
	req.Header.Set("X-Service-Token", testS2SToken)

	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	require.Equal(t, http.StatusBadRequest, w.Code)

	var resp map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	errBody, _ := resp["error"].(map[string]any)
	assert.Equal(t, "VALIDATION_ERROR", errBody["code"])
}
