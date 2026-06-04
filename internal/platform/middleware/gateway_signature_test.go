package middleware

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// testSecret is a 32-char placeholder HMAC secret used in tests only — not a real secret.
const testSecret = "0123456789abcdef0123456789abcdef"

// Fixed identity values shared across the table.
const (
	fixUserID        = "11111111-1111-1111-1111-111111111111"
	fixRealTier      = "2"
	fixAccountType   = "PERSONAL"
	fixEmailVerified = "true"
	fixRequestID     = "req-abc"
)

// signCanonical is an INDEPENDENT reimplementation of the §24.1 canonical string
// and HMAC, used by the tests to build a valid signature. It deliberately does
// NOT call the production computeGatewaySignature — using its OWN hmac.New keeps
// the "valid → pass" test from being a tautology (it proves the production code
// agrees with an independently-derived expected value).
func signCanonical(t *testing.T, secret, accountType, ts string) string {
	t.Helper()

	canonical := strings.Join([]string{
		fixUserID, fixRealTier, accountType, fixEmailVerified, fixRequestID, ts,
	}, "|")

	mac := hmac.New(sha256.New, []byte(secret))
	_, err := mac.Write([]byte(canonical))
	require.NoError(t, err)

	return hex.EncodeToString(mac.Sum(nil))
}

// newSignedRequest builds a request carrying the six identity headers plus a
// gateway timestamp and signature computed over them.
func newSignedRequest(t *testing.T, secret, accountType string, ts int64) *http.Request {
	t.Helper()

	tsStr := strconv.FormatInt(ts, 10)
	sig := signCanonical(t, secret, accountType, tsStr)

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/protected", http.NoBody)
	req.Header.Set(headerUserID, fixUserID)
	req.Header.Set(headerKYCTier, fixRealTier)
	req.Header.Set(headerAccountType, accountType)
	req.Header.Set(headerEmailVerified, fixEmailVerified)
	req.Header.Set(headerRequestID, fixRequestID)
	req.Header.Set(headerGatewayTs, tsStr)
	req.Header.Set(headerGatewaySignature, sig)

	return req
}

// runWithMiddleware wires VerifyGatewaySignature(secret) onto a single protected
// route whose handler returns 200, then serves req and returns the recorder.
func runWithMiddleware(secret string, req *http.Request) *httptest.ResponseRecorder {
	gin.SetMode(gin.TestMode)

	r := gin.New()
	r.Use(VerifyGatewaySignature(secret))
	r.GET("/protected", func(c *gin.Context) { c.Status(http.StatusOK) })

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	return rec
}

func TestVerifyGatewaySignature(t *testing.T) {
	now := time.Now().Unix()

	t.Run("valid signature passes", func(t *testing.T) {
		req := newSignedRequest(t, testSecret, fixAccountType, now)

		rec := runWithMiddleware(testSecret, req)

		assert.Equal(t, http.StatusOK, rec.Code, "a correctly-signed request must pass")
	})

	t.Run("missing signature is rejected 401", func(t *testing.T) {
		req := newSignedRequest(t, testSecret, fixAccountType, now)
		req.Header.Del(headerGatewaySignature)

		rec := runWithMiddleware(testSecret, req)

		assert.Equal(t, http.StatusUnauthorized, rec.Code)
	})

	t.Run("missing timestamp is rejected 401", func(t *testing.T) {
		req := newSignedRequest(t, testSecret, fixAccountType, now)
		req.Header.Del(headerGatewayTs)

		rec := runWithMiddleware(testSecret, req)

		assert.Equal(t, http.StatusUnauthorized, rec.Code)
	})

	t.Run("tampered signature is rejected 401", func(t *testing.T) {
		req := newSignedRequest(t, testSecret, fixAccountType, now)
		// Flip the signature to a valid-hex but wrong digest.
		req.Header.Set(headerGatewaySignature, strings.Repeat("a", 64))

		rec := runWithMiddleware(testSecret, req)

		assert.Equal(t, http.StatusUnauthorized, rec.Code)
	})

	t.Run("non-hex signature is rejected 401", func(t *testing.T) {
		req := newSignedRequest(t, testSecret, fixAccountType, now)
		req.Header.Set(headerGatewaySignature, "not-hex-zzzz")

		rec := runWithMiddleware(testSecret, req)

		assert.Equal(t, http.StatusUnauthorized, rec.Code)
	})

	t.Run("stale timestamp beyond skew is rejected 401", func(t *testing.T) {
		stale := now - int64(maxGatewaySkew.Seconds()) - 5
		req := newSignedRequest(t, testSecret, fixAccountType, stale)

		rec := runWithMiddleware(testSecret, req)

		assert.Equal(t, http.StatusUnauthorized, rec.Code)
	})

	t.Run("future timestamp beyond skew is rejected 401", func(t *testing.T) {
		future := now + int64(maxGatewaySkew.Seconds()) + 5
		req := newSignedRequest(t, testSecret, fixAccountType, future)

		rec := runWithMiddleware(testSecret, req)

		assert.Equal(t, http.StatusUnauthorized, rec.Code)
	})

	t.Run("non-numeric timestamp is rejected 401", func(t *testing.T) {
		req := newSignedRequest(t, testSecret, fixAccountType, now)
		req.Header.Set(headerGatewayTs, "not-a-number")

		rec := runWithMiddleware(testSecret, req)

		assert.Equal(t, http.StatusUnauthorized, rec.Code)
	})

	t.Run("forged kyc tier with signature over real tier is rejected 401", func(t *testing.T) {
		// Attacker signs over the REAL tier (2) but then forges X-Kyc-Tier=3 on
		// the wire. The recomputed canonical uses the forged header (3), so the
		// signature no longer matches → 401. This is the core threat §24.1 closes.
		req := newSignedRequest(t, testSecret, fixAccountType, now)
		req.Header.Set(headerKYCTier, "3") // forged: claims Tier-3 over a Tier-2 signature

		rec := runWithMiddleware(testSecret, req)

		assert.Equal(t, http.StatusUnauthorized, rec.Code, "forged tier must not pass")
	})

	t.Run("empty account type keeps stable pipe positions and still passes", func(t *testing.T) {
		// §24.1 empty-field rule: an empty value is an empty field. A request
		// signed with accountType="" must verify when accountType is sent empty.
		req := newSignedRequest(t, testSecret, "", now)

		rec := runWithMiddleware(testSecret, req)

		assert.Equal(t, http.StatusOK, rec.Code, "empty account type must still verify (stable | positions)")
	})

	t.Run("dev with empty secret skips verification", func(t *testing.T) {
		// Dev posture: no secret → middleware is a passthrough. An unsigned
		// request (no X-Gateway-* headers) must pass through to the handler.
		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/protected", http.NoBody)
		req.Header.Set(headerUserID, fixUserID)

		rec := runWithMiddleware("", req)

		assert.Equal(t, http.StatusOK, rec.Code, "dev-no-secret must skip verification")
	})

	t.Run("wrong secret is rejected 401", func(t *testing.T) {
		// Signed with a different secret than the verifier holds → mismatch.
		req := newSignedRequest(t, "ffffffffffffffffffffffffffffffff", fixAccountType, now)

		rec := runWithMiddleware(testSecret, req)

		assert.Equal(t, http.StatusUnauthorized, rec.Code)
	})
}
