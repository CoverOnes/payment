package middleware

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
)

// testS2SToken is a fixed shared token used in all service-identity tests.
// Not a real secret — test fixture only.
const testS2SToken = "test-s2s-token-fixture-32charabc" //nolint:gosec // G101 false positive: test fixture constant, not a real credential

// runWithServiceIdentity wires RequireServiceIdentity(token) onto a single
// protected route whose handler returns 200, serves req, and returns the recorder.
func runWithServiceIdentity(token string, req *http.Request) *httptest.ResponseRecorder {
	gin.SetMode(gin.TestMode)

	r := gin.New()
	r.Use(RequireServiceIdentity(token))
	r.GET("/internal", func(c *gin.Context) { c.Status(http.StatusOK) })

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	return rec
}

// newS2SRequest builds a GET /internal request with the given X-Service-Id and
// X-Service-Token headers (empty string means the header is omitted).
func newS2SRequest(t *testing.T, serviceID, token string) *http.Request {
	t.Helper()

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/internal", http.NoBody)

	if serviceID != "" {
		req.Header.Set(headerServiceID, serviceID)
	}

	if token != "" {
		req.Header.Set(headerServiceToken, token)
	}

	return req
}

func TestRequireServiceIdentity(t *testing.T) {
	t.Run("valid token and valid service-id returns 200", func(t *testing.T) {
		req := newS2SRequest(t, "payment-worker", testS2SToken)

		rec := runWithServiceIdentity(testS2SToken, req)

		assert.Equal(t, http.StatusOK, rec.Code, "valid credentials must pass through to handler")
	})

	t.Run("empty configured token fails closed 401", func(t *testing.T) {
		// Even with correct headers, an empty expected token must always reject.
		req := newS2SRequest(t, "payment-worker", testS2SToken)

		rec := runWithServiceIdentity("", req)

		assert.Equal(t, http.StatusUnauthorized, rec.Code, "empty configured token must fail closed")
	})

	t.Run("missing X-Service-Token header returns 401", func(t *testing.T) {
		req := newS2SRequest(t, "payment-worker", "") // token header omitted

		rec := runWithServiceIdentity(testS2SToken, req)

		assert.Equal(t, http.StatusUnauthorized, rec.Code)
	})

	t.Run("wrong X-Service-Token returns 401", func(t *testing.T) {
		req := newS2SRequest(t, "payment-worker", "wrong-token-value")

		rec := runWithServiceIdentity(testS2SToken, req)

		assert.Equal(t, http.StatusUnauthorized, rec.Code)
	})

	t.Run("missing X-Service-Id returns 401", func(t *testing.T) {
		req := newS2SRequest(t, "", testS2SToken) // service-id header omitted

		rec := runWithServiceIdentity(testS2SToken, req)

		assert.Equal(t, http.StatusUnauthorized, rec.Code)
	})

	t.Run("oversized X-Service-Id (>64 chars) returns 401", func(t *testing.T) {
		oversized := strings.Repeat("a", maxServiceIDLen+1) // 65 chars
		req := newS2SRequest(t, oversized, testS2SToken)

		rec := runWithServiceIdentity(testS2SToken, req)

		assert.Equal(t, http.StatusUnauthorized, rec.Code, "service-id exceeding 64 chars must be rejected")
	})

	t.Run("X-Service-Id with null byte returns 401", func(t *testing.T) {
		req := newS2SRequest(t, "payment\x00worker", testS2SToken)

		rec := runWithServiceIdentity(testS2SToken, req)

		assert.Equal(t, http.StatusUnauthorized, rec.Code, "null byte in service-id must be rejected")
	})

	t.Run("X-Service-Id with newline returns 401", func(t *testing.T) {
		req := newS2SRequest(t, "payment\nworker", testS2SToken)

		rec := runWithServiceIdentity(testS2SToken, req)

		assert.Equal(t, http.StatusUnauthorized, rec.Code, "newline in service-id must be rejected")
	})

	t.Run("X-Service-Id with carriage return returns 401", func(t *testing.T) {
		req := newS2SRequest(t, "payment\rworker", testS2SToken)

		rec := runWithServiceIdentity(testS2SToken, req)

		assert.Equal(t, http.StatusUnauthorized, rec.Code, "carriage return in service-id must be rejected")
	})

	t.Run("X-Service-Id with tab returns 401", func(t *testing.T) {
		req := newS2SRequest(t, "payment\tworker", testS2SToken)

		rec := runWithServiceIdentity(testS2SToken, req)

		assert.Equal(t, http.StatusUnauthorized, rec.Code, "tab in service-id must be rejected")
	})

	t.Run("exactly 64-char X-Service-Id is accepted", func(t *testing.T) {
		exactly64 := strings.Repeat("a", maxServiceIDLen)
		req := newS2SRequest(t, exactly64, testS2SToken)

		rec := runWithServiceIdentity(testS2SToken, req)

		assert.Equal(t, http.StatusOK, rec.Code, "exactly-64-char service-id must be accepted")
	})

	t.Run("service-id is stored in gin context", func(t *testing.T) {
		const svcID = "workspace-service"

		gin.SetMode(gin.TestMode)

		r := gin.New()
		r.Use(RequireServiceIdentity(testS2SToken))

		var captured string

		r.GET("/internal", func(c *gin.Context) {
			captured = ServiceIDFromCtx(c)
			c.Status(http.StatusOK)
		})

		req := newS2SRequest(t, svcID, testS2SToken)
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)

		assert.Equal(t, http.StatusOK, rec.Code)
		assert.Equal(t, svcID, captured, "service-id must be available via ServiceIDFromCtx after auth")
	})
}
