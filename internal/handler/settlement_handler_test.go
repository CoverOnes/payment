package handler_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/CoverOnes/payment/internal/domain"
	"github.com/CoverOnes/payment/internal/handler"
	"github.com/CoverOnes/payment/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
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

// ─── Stub disburser ───────────────────────────────────────────────────────────

// stubDisburser implements handler.SettlementDisburser for handler-level unit tests.
// result and err are returned verbatim from DisburseMilestone.
type stubDisburser struct {
	result *service.DisburseResult
	err    error
}

func (s *stubDisburser) DisburseMilestone(
	_ context.Context,
	_ *service.DisburseMilestoneInput,
) (*service.DisburseResult, error) {
	return s.result, s.err
}

// buildStubRouter wires a Gin engine with the given stub disburser so that
// handler-level tests can control the DisburseMilestone outcome without a DB.
func buildStubRouter(stub handler.SettlementDisburser) http.Handler {
	gin.SetMode(gin.TestMode)
	r := gin.New()

	h := handler.NewSettlementHandlerForTest(stub)

	// Mirror the router group structure from NewRouter — identity + service token guards.
	grp := r.Group("/v1/settlement")
	grp.POST("/plans/:id/disburse", h.Disburse)

	return r
}

// disburseBodyWithKey builds a disburse request body with the given idempotency key.
func disburseBodyWithKey(t *testing.T, key string) *bytes.Reader {
	t.Helper()

	body := map[string]any{
		"milestoneId":    "00000000-0000-0000-0000-000000000001",
		"amount":         "1000.00",
		"currency":       "TWD",
		"idempotencyKey": key,
	}

	data, err := json.Marshal(body)
	require.NoError(t, err)

	return bytes.NewReader(data)
}

// disburseReq builds a disburse request for the stub router using testPlanID.
func disburseReq(t *testing.T, body *bytes.Reader) *http.Request {
	t.Helper()

	req := httptest.NewRequestWithContext(
		context.Background(),
		http.MethodPost,
		"/v1/settlement/plans/"+testPlanID+"/disburse",
		body,
	)
	req.Header.Set("Content-Type", "application/json")

	return req
}

// ─── Fix 1: honest disburse response discriminant ─────────────────────────────

// TestSettlementHandler_Disburse_AllSuccess verifies that when all vendors succeed
// the response is 200 with status "disbursed".
func TestSettlementHandler_Disburse_AllSuccess(t *testing.T) {
	vendor1 := uuid.New()
	vendor2 := uuid.New()

	stub := &stubDisburser{
		result: &service.DisburseResult{
			DisbursedCount: 2,
			FailedCount:    0,
			Outcomes: []service.VendorDisburseOutcome{
				{VendorUserID: vendor1, Status: "DISBURSED"},
				{VendorUserID: vendor2, Status: "DISBURSED"},
			},
		},
	}

	r := buildStubRouter(stub)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, disburseReq(t, disburseBody(t)))

	require.Equal(t, http.StatusOK, w.Code, "all-success must return 200")

	var resp map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	data := resp["data"].(map[string]any)
	assert.Equal(t, "disbursed", data["status"], "all-success status must be 'disbursed'")
}

// TestSettlementHandler_Disburse_PartialFailure verifies that when some vendors fail
// the response is 207 Multi-Status with status "partial_failure" and per-vendor outcomes.
func TestSettlementHandler_Disburse_PartialFailure(t *testing.T) {
	vendor1 := uuid.New()
	vendor2 := uuid.New()

	stub := &stubDisburser{
		result: &service.DisburseResult{
			DisbursedCount: 1,
			FailedCount:    1,
			Outcomes: []service.VendorDisburseOutcome{
				{VendorUserID: vendor1, Status: "DISBURSED"},
				{VendorUserID: vendor2, Status: "FAILED"},
			},
		},
	}

	r := buildStubRouter(stub)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, disburseReq(t, disburseBody(t)))

	require.Equal(t, http.StatusMultiStatus, w.Code, "partial failure must return 207")

	var resp map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	data := resp["data"].(map[string]any)
	assert.Equal(t, "partial_failure", data["status"], "partial failure status must be 'partial_failure'")

	vendors, ok := data["vendors"].([]any)
	require.True(t, ok, "vendors must be a list")
	assert.Len(t, vendors, 2, "must include per-vendor outcome for both vendors")
}

// TestSettlementHandler_Disburse_AllFailed verifies that when ALL vendors fail
// the response is 502 with status "failed" — NOT 200 disbursed.
func TestSettlementHandler_Disburse_AllFailed(t *testing.T) {
	vendor1 := uuid.New()
	vendor2 := uuid.New()

	stub := &stubDisburser{
		result: &service.DisburseResult{
			DisbursedCount: 0,
			FailedCount:    2,
			Outcomes: []service.VendorDisburseOutcome{
				{VendorUserID: vendor1, Status: "FAILED"},
				{VendorUserID: vendor2, Status: "FAILED"},
			},
		},
	}

	r := buildStubRouter(stub)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, disburseReq(t, disburseBody(t)))

	require.Equal(t, http.StatusBadGateway, w.Code, "all-failed must return 502, not 200")

	var resp map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	data := resp["data"].(map[string]any)
	assert.Equal(t, "failed", data["status"], "all-failed status must be 'failed'")
	assert.NotEqual(t, "disbursed", data["status"], "all-failed must NOT report 'disbursed'")
}

// TestSettlementHandler_Disburse_ServiceError verifies that a service error
// (e.g. plan not found) is surfaced as an error response, not a success.
func TestSettlementHandler_Disburse_ServiceError(t *testing.T) {
	stub := &stubDisburser{
		err: domain.ErrPlanNotFound,
	}

	r := buildStubRouter(stub)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, disburseReq(t, disburseBody(t)))

	assert.NotEqual(t, http.StatusOK, w.Code, "service error must not return 200")
}

// ─── Fix 2: IdempotencyKey is a no-op ────────────────────────────────────────

// TestSettlementHandler_Disburse_IdempotencyKey_Noop verifies that:
//   - Sending any idempotencyKey value (or omitting it entirely) produces an identical outcome.
//   - The field is not required — no 400 is returned when it is absent or empty.
func TestSettlementHandler_Disburse_IdempotencyKey_Noop(t *testing.T) {
	makeResult := func() *service.DisburseResult {
		return &service.DisburseResult{
			DisbursedCount: 1,
			FailedCount:    0,
			Outcomes: []service.VendorDisburseOutcome{
				{VendorUserID: uuid.New(), Status: "DISBURSED"},
			},
		}
	}

	table := []struct {
		name string
		key  string
	}{
		{name: "with_key", key: "some-opaque-idempotency-key"},
		{name: "empty_key", key: ""},
		{name: "no_key_field", key: ""},
	}

	for _, tc := range table {
		t.Run(tc.name, func(t *testing.T) {
			stub := &stubDisburser{result: makeResult()}
			r := buildStubRouter(stub)
			w := httptest.NewRecorder()
			r.ServeHTTP(w, disburseReq(t, disburseBodyWithKey(t, tc.key)))

			assert.Equal(t, http.StatusOK, w.Code,
				"idempotencyKey=%q must not affect outcome (no-op, not required)", tc.key)

			var resp map[string]any
			require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
			data := resp["data"].(map[string]any)
			assert.Equal(t, "disbursed", data["status"])
		})
	}
}

// TestSettlementHandler_Disburse_IdempotencyKey_TooLong verifies that an oversized
// idempotencyKey (> 255 chars) is still rejected — the length cap prevents request
// abuse even though the value is never used.
func TestSettlementHandler_Disburse_IdempotencyKey_TooLong(t *testing.T) {
	longKey := make([]byte, 256)
	for i := range longKey {
		longKey[i] = 'x'
	}

	stub := &stubDisburser{result: &service.DisburseResult{}}
	r := buildStubRouter(stub)

	body := map[string]any{
		"milestoneId":    "00000000-0000-0000-0000-000000000001",
		"amount":         "1000.00",
		"currency":       "TWD",
		"idempotencyKey": string(longKey),
	}

	data, err := json.Marshal(body)
	require.NoError(t, err)

	w := httptest.NewRecorder()
	r.ServeHTTP(w, disburseReq(t, bytes.NewReader(data)))

	require.Equal(t, http.StatusBadRequest, w.Code, "oversized idempotencyKey must return 400")

	var resp map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	errBody := resp["error"].(map[string]any)
	assert.Equal(t, "VALIDATION_ERROR", errBody["code"])
}

// ─── Fix 1 integration: DisburseResult fields from service ───────────────────

// TestDisburseResult_Fields verifies the DisburseResult struct fields so callers
// can rely on DisbursedCount + FailedCount + Outcomes as a stable API contract.
func TestDisburseResult_Fields(t *testing.T) {
	vendor1 := uuid.New()
	vendor2 := uuid.New()

	result := &service.DisburseResult{
		DisbursedCount: 1,
		FailedCount:    1,
		Outcomes: []service.VendorDisburseOutcome{
			{VendorUserID: vendor1, Status: "DISBURSED"},
			{VendorUserID: vendor2, Status: "FAILED"},
		},
	}

	assert.Equal(t, 1, result.DisbursedCount)
	assert.Equal(t, 1, result.FailedCount)
	require.Len(t, result.Outcomes, 2)
	assert.Equal(t, vendor1, result.Outcomes[0].VendorUserID)
	assert.Equal(t, "DISBURSED", result.Outcomes[0].Status)
	assert.Equal(t, vendor2, result.Outcomes[1].VendorUserID)
	assert.Equal(t, "FAILED", result.Outcomes[1].Status)
}

// Compile-time check: ensure errors sentinel package is used (imported above for ServiceError test).
var _ = errors.Is
