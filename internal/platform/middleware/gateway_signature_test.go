package middleware

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"
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

// signCanonical is an INDEPENDENT reimplementation of the §24.1 rev2-B canonical
// string and HMAC. It deliberately does NOT call the production
// computeGatewaySignature to keep the "valid → pass" test from being a tautology.
//
// Format (length-prefix framing):
//
//	{len(method)}\n{method}\n{len(path)}\n{path}\n{len(bodyHashHex)}\n{bodyHashHex}\n{identity|pipe|delimited|ts}
func signCanonical(t *testing.T, secret, method, path, accountType, ts string, body []byte) string {
	t.Helper()

	bodyHashRaw := sha256.Sum256(body)
	bodyHashHex := hex.EncodeToString(bodyHashRaw[:])

	canonical := fmt.Sprintf(
		"%d\n%s\n%d\n%s\n%d\n%s\n%s",
		len(method), method,
		len(path), path,
		len(bodyHashHex), bodyHashHex,
		strings.Join([]string{
			fixUserID, fixRealTier, accountType, fixEmailVerified, fixRequestID, ts,
		}, "|"),
	)

	mac := hmac.New(sha256.New, []byte(secret))
	_, err := mac.Write([]byte(canonical))
	require.NoError(t, err)

	return hex.EncodeToString(mac.Sum(nil))
}

// newSignedRequest builds a request with all identity + gateway headers set.
func newSignedRequest(
	t *testing.T,
	secret, accountType string,
	ts int64,
	method, path string,
	body []byte,
) *http.Request {
	t.Helper()

	tsStr := strconv.FormatInt(ts, 10)
	sig := signCanonical(t, secret, method, path, accountType, tsStr, body)

	var reqBody *bytes.Reader
	if len(body) > 0 {
		reqBody = bytes.NewReader(body)
	} else {
		reqBody = bytes.NewReader(nil)
	}

	req := httptest.NewRequestWithContext(t.Context(), method, path, reqBody)
	req.Header.Set(headerUserID, fixUserID)
	req.Header.Set(headerKYCTier, fixRealTier)
	req.Header.Set(headerAccountType, accountType)
	req.Header.Set(headerEmailVerified, fixEmailVerified)
	req.Header.Set(headerRequestID, fixRequestID)
	req.Header.Set(headerGatewayTs, tsStr)
	req.Header.Set(headerGatewaySignature, sig)

	return req
}

// runWithMiddleware wires VerifyGatewaySignature onto a catch-all route and returns the
// response recorder.
func runWithMiddleware(secret string, rdb *redis.Client, req *http.Request) *httptest.ResponseRecorder {
	gin.SetMode(gin.TestMode)

	r := gin.New()
	r.Use(VerifyGatewaySignature(secret, rdb))
	r.Any("/*path", func(c *gin.Context) { c.Status(http.StatusOK) })

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	return rec
}

func TestVerifyGatewaySignature(t *testing.T) {
	now := time.Now().Unix()

	t.Run("valid GET passes", func(t *testing.T) {
		req := newSignedRequest(t, testSecret, fixAccountType, now, http.MethodGet, "/v1/payments", nil)

		rec := runWithMiddleware(testSecret, nil, req)

		assert.Equal(t, http.StatusOK, rec.Code)
	})

	t.Run("valid POST with body passes", func(t *testing.T) {
		body := []byte(`{"amount":100,"currency":"USD"}`)
		req := newSignedRequest(t, testSecret, fixAccountType, now, http.MethodPost, "/v1/payments", body)

		rec := runWithMiddleware(testSecret, nil, req)

		assert.Equal(t, http.StatusOK, rec.Code)
	})

	t.Run("cross-endpoint replay: same body on different path rejected", func(t *testing.T) {
		body := []byte(`{"amount":100}`)
		reqA := newSignedRequest(t, testSecret, fixAccountType, now, http.MethodPost, "/v1/payments", body)
		sig := reqA.Header.Get(headerGatewaySignature)

		reqB := newSignedRequest(t, testSecret, fixAccountType, now, http.MethodPost, "/v1/payments/refund", body)
		reqB.Header.Set(headerGatewaySignature, sig)

		rec := runWithMiddleware(testSecret, nil, reqB)

		assert.Equal(t, http.StatusUnauthorized, rec.Code, "path bound in canonical prevents cross-endpoint replay")
	})

	t.Run("method-swap replay rejected", func(t *testing.T) {
		reqGet := newSignedRequest(t, testSecret, fixAccountType, now, http.MethodGet, "/v1/payments", nil)
		sig := reqGet.Header.Get(headerGatewaySignature)

		reqPost := newSignedRequest(t, testSecret, fixAccountType, now, http.MethodPost, "/v1/payments", nil)
		reqPost.Header.Set(headerGatewaySignature, sig)

		rec := runWithMiddleware(testSecret, nil, reqPost)

		assert.Equal(t, http.StatusUnauthorized, rec.Code, "method bound in canonical prevents method-swap replay")
	})

	t.Run("body tamper rejected", func(t *testing.T) {
		body := []byte(`{"amount":100}`)
		req := newSignedRequest(t, testSecret, fixAccountType, now, http.MethodPost, "/v1/payments", body)
		req.Body = io.NopCloser(bytes.NewReader([]byte(`{"amount":9999}`)))

		rec := runWithMiddleware(testSecret, nil, req)

		assert.Equal(t, http.StatusUnauthorized, rec.Code, "body hash bound in canonical prevents body tamper")
	})

	t.Run("missing signature rejected 401", func(t *testing.T) {
		req := newSignedRequest(t, testSecret, fixAccountType, now, http.MethodGet, "/v1/payments", nil)
		req.Header.Del(headerGatewaySignature)

		rec := runWithMiddleware(testSecret, nil, req)

		assert.Equal(t, http.StatusUnauthorized, rec.Code)
	})

	t.Run("missing timestamp rejected 401", func(t *testing.T) {
		req := newSignedRequest(t, testSecret, fixAccountType, now, http.MethodGet, "/v1/payments", nil)
		req.Header.Del(headerGatewayTs)

		rec := runWithMiddleware(testSecret, nil, req)

		assert.Equal(t, http.StatusUnauthorized, rec.Code)
	})

	t.Run("non-hex signature rejected 401", func(t *testing.T) {
		req := newSignedRequest(t, testSecret, fixAccountType, now, http.MethodGet, "/v1/payments", nil)
		req.Header.Set(headerGatewaySignature, "not-hex-zzzz")

		rec := runWithMiddleware(testSecret, nil, req)

		assert.Equal(t, http.StatusUnauthorized, rec.Code)
	})

	t.Run("stale timestamp rejected 401", func(t *testing.T) {
		stale := now - int64(maxGatewaySkew.Seconds()) - 5
		req := newSignedRequest(t, testSecret, fixAccountType, stale, http.MethodGet, "/v1/payments", nil)

		rec := runWithMiddleware(testSecret, nil, req)

		assert.Equal(t, http.StatusUnauthorized, rec.Code)
	})

	t.Run("future timestamp rejected 401", func(t *testing.T) {
		future := now + int64(maxGatewaySkew.Seconds()) + 5
		req := newSignedRequest(t, testSecret, fixAccountType, future, http.MethodGet, "/v1/payments", nil)

		rec := runWithMiddleware(testSecret, nil, req)

		assert.Equal(t, http.StatusUnauthorized, rec.Code)
	})

	t.Run("non-numeric timestamp rejected 401", func(t *testing.T) {
		req := newSignedRequest(t, testSecret, fixAccountType, now, http.MethodGet, "/v1/payments", nil)
		req.Header.Set(headerGatewayTs, "not-a-number")

		rec := runWithMiddleware(testSecret, nil, req)

		assert.Equal(t, http.StatusUnauthorized, rec.Code)
	})

	t.Run("forged kyc tier rejected 401", func(t *testing.T) {
		req := newSignedRequest(t, testSecret, fixAccountType, now, http.MethodGet, "/v1/payments", nil)
		req.Header.Set(headerKYCTier, "3") // forged: higher tier than what was signed

		rec := runWithMiddleware(testSecret, nil, req)

		assert.Equal(t, http.StatusUnauthorized, rec.Code)
	})

	t.Run("empty account type stable positions passes", func(t *testing.T) {
		req := newSignedRequest(t, testSecret, "", now, http.MethodGet, "/v1/payments", nil)

		rec := runWithMiddleware(testSecret, nil, req)

		assert.Equal(t, http.StatusOK, rec.Code)
	})

	t.Run("dev empty secret skips verification", func(t *testing.T) {
		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/v1/payments", http.NoBody)
		req.Header.Set(headerUserID, fixUserID)

		rec := runWithMiddleware("", nil, req)

		assert.Equal(t, http.StatusOK, rec.Code)
	})

	t.Run("wrong secret rejected 401", func(t *testing.T) {
		req := newSignedRequest(t, "ffffffffffffffffffffffffffffffff", fixAccountType, now, http.MethodGet, "/v1/payments", nil)

		rec := runWithMiddleware(testSecret, nil, req)

		assert.Equal(t, http.StatusUnauthorized, rec.Code)
	})
}

func TestVerifyGatewaySignature_NonceReplay(t *testing.T) {
	t.Run("first nonce stored returns true, second returns false", func(t *testing.T) {
		mr := miniredis.RunT(t)
		rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
		defer rdb.Close() //nolint:errcheck // test teardown

		ctx := context.Background()
		nonce := "pay-nonce-abc"

		first := storeNonce(ctx, rdb, nonce)
		require.True(t, first)

		second := storeNonce(ctx, rdb, nonce)
		assert.False(t, second, "replay of same nonce must be rejected")
	})

	t.Run("nil redis skips nonce check", func(t *testing.T) {
		now := time.Now().Unix()

		rec1 := runWithMiddleware(testSecret, nil,
			newSignedRequest(t, testSecret, fixAccountType, now, http.MethodGet, "/v1/payments", nil))
		rec2 := runWithMiddleware(testSecret, nil,
			newSignedRequest(t, testSecret, fixAccountType, now, http.MethodGet, "/v1/payments", nil))

		assert.Equal(t, http.StatusOK, rec1.Code)
		assert.Equal(t, http.StatusOK, rec2.Code)
	})

	t.Run("redis error is fail-closed", func(t *testing.T) {
		badRDB := redis.NewClient(&redis.Options{
			Addr:         "127.0.0.1:1",
			DialTimeout:  5 * time.Millisecond,
			ReadTimeout:  5 * time.Millisecond,
			WriteTimeout: 5 * time.Millisecond,
		})
		defer badRDB.Close() //nolint:errcheck // test teardown

		ok := storeNonce(context.Background(), badRDB, "pay-nonce-bad")
		assert.False(t, ok, "redis error must be fail-closed")
	})

	t.Run("replay same requestId with redis is rejected", func(t *testing.T) {
		mr := miniredis.RunT(t)
		rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
		defer rdb.Close() //nolint:errcheck // test teardown

		now := time.Now().Unix()
		req1 := newSignedRequest(t, testSecret, fixAccountType, now, http.MethodGet, "/v1/payments", nil)
		req2 := newSignedRequest(t, testSecret, fixAccountType, now, http.MethodGet, "/v1/payments", nil)

		rec1 := runWithMiddleware(testSecret, rdb, req1)
		rec2 := runWithMiddleware(testSecret, rdb, req2)

		assert.Equal(t, http.StatusOK, rec1.Code)
		assert.Equal(t, http.StatusUnauthorized, rec2.Code, "replay within skew window must be rejected")
	})
}
