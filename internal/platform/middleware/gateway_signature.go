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
	"strconv"
	"strings"
	"time"

	"github.com/CoverOnes/payment/internal/platform/httpx"
	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"
)

const (
	headerEmailVerified    = "X-Email-Verified"
	headerGatewayTs        = "X-Gateway-Ts"
	headerGatewaySignature = "X-Gateway-Signature"

	// maxGatewaySkew bounds the replay window: a signed request is rejected when
	// |now - X-Gateway-Ts| exceeds this. Locked by conventions §24.1.
	maxGatewaySkew = 30 * time.Second

	// gatewayBodyLimit is the max bytes read from the request body for hashing.
	// Matches the per-handler MaxBytesReader cap (1 MB). Requests larger than this
	// are rejected before signature verification reaches the DB layer.
	gatewayBodyLimit = 1 << 20 // 1 MB

	// replayNoncePrefix is the Redis key prefix for nonce replay entries.
	// Key pattern: "gw:nonce:{requestId}" → value "1", TTL = maxGatewaySkew.
	replayNoncePrefix = "gw:nonce:"
)

// VerifyGatewaySignature returns a middleware that proves the gateway-injected
// identity headers actually originated from the API gateway, by verifying the
// HMAC-SHA256 signature the gateway emits over the identity + request tuple
// (conventions §24.1). It MUST be registered BEFORE RequireValidIdentity on the
// protected group so that no handler trusts X-User-Id / X-Kyc-Tier / X-Account-Type /
// X-Email-Verified until the signature is validated.
//
// The canonical string binds:
//   - HTTP method and full request path (length-prefix framing prevents
//     delimiter injection)
//   - SHA-256 of the raw request body (empty body → SHA-256(""))
//   - Identity headers: userId, kycTier, accountType, emailVerified, requestId
//   - Gateway timestamp (Unix seconds, string)
//
// Length-prefix format (prevents ambiguous delimiter attacks):
//
//	{len(method)}\n{method}\n{len(path)}\n{path}\n{len(bodyHash)}\n{bodyHash}\n{identity|pipe|delimited|ts}
//
// Nonce / replay cache: when rdb is non-nil, X-Request-ID is stored in Redis
// with TTL = maxGatewaySkew. A second request with the same requestId within
// the window is rejected with 401 even if the signature is valid. When rdb is
// nil (dev/test) the replay check is skipped.
//
// When secret == "" (development only — the gateway also disables signing in dev)
// verification is skipped and the request passes through unchanged.
func VerifyGatewaySignature(secret string, rdb *redis.Client) gin.HandlerFunc {
	if secret == "" {
		// Dev posture: signing disabled gateway-side, verification disabled here.
		return func(c *gin.Context) { c.Next() }
	}

	secretBytes := []byte(secret)

	return func(c *gin.Context) {
		sig := c.GetHeader(headerGatewaySignature)
		ts := c.GetHeader(headerGatewayTs)

		// Unsigned request → never trust identity headers on a protected route.
		if sig == "" || ts == "" {
			rejectUnauthorized(c)
			return
		}

		tsInt, err := strconv.ParseInt(ts, 10, 64)
		if err != nil || !withinSkew(tsInt) {
			rejectUnauthorized(c)
			return
		}

		// Read and buffer the body so it can be hashed AND forwarded to the handler.
		// LimitReader caps at gatewayBodyLimit (1 MB); requests beyond this are
		// rejected — an oversized body that wasn't signed is not a valid request.
		var bodyBuf []byte
		if c.Request.Body != nil && c.Request.Body != http.NoBody {
			bodyBuf, err = io.ReadAll(io.LimitReader(c.Request.Body, gatewayBodyLimit+1))
			if err != nil {
				rejectUnauthorized(c)
				return
			}

			if int64(len(bodyBuf)) > gatewayBodyLimit {
				c.Abort()
				httpx.ErrCode(c, http.StatusRequestEntityTooLarge, "BODY_TOO_LARGE", "request body exceeds limit")

				return
			}

			// Restore the body so downstream handlers can still read it.
			c.Request.Body = io.NopCloser(bytes.NewReader(bodyBuf))
		}

		expected := computeGatewaySignature(secretBytes, c, ts, bodyBuf)

		// hex-decode both sides and compare in constant time (hmac.Equal).
		// A non-hex incoming signature decodes with error → treated as mismatch.
		sigBytes, decodeErr := hex.DecodeString(sig)
		if decodeErr != nil || !hmac.Equal(sigBytes, expected) {
			rejectUnauthorized(c)
			return
		}

		// Nonce replay check — only when Redis is configured.
		// The signature is already verified at this point, so requestId is trusted.
		if rdb != nil {
			requestID := c.GetHeader(headerRequestID)
			if requestID == "" {
				// A signed request with no requestId cannot be replayed-checked safely:
				// reject to force the gateway to include the nonce.
				rejectUnauthorized(c)
				return
			}

			if !storeNonce(c.Request.Context(), rdb, requestID) {
				// Key already existed → replay detected.
				rejectUnauthorized(c)
				return
			}
		}

		c.Next()
	}
}

// computeGatewaySignature builds the §24.1 canonical string and returns the raw
// HMAC-SHA256 digest bytes (not hex-encoded).
//
// Canonical string format (length-prefix framing):
//
//	{len(method)}\n{method}\n{len(path)}\n{path}\n{len(bodyHashHex)}\n{bodyHashHex}\n{userId}|{kycTier}|{accountType}|{emailVerified}|{requestId}|{ts}
//
// Body hash: hex(SHA-256(body)); empty body → hex(SHA-256("")).
// Length prefix prevents delimiter injection: knowing the lengths, a parser can
// unambiguously reconstruct each field even if values contain '\n' or '|'.
func computeGatewaySignature(secret []byte, c *gin.Context, ts string, body []byte) []byte {
	// Body hash: SHA-256 of raw body bytes.
	bodyHashRaw := sha256.Sum256(body)
	bodyHashHex := hex.EncodeToString(bodyHashRaw[:])

	method := c.Request.Method
	path := c.Request.URL.RequestURI() // includes query string

	// Length-prefix framing: each field is prefixed with its byte length + '\n'.
	// The identity tuple at the end uses '|' delimiters (no length prefix needed
	// because all values are constrained: UUIDs, numeric tiers, enum account types,
	// "true"/"false", opaque request-IDs, Unix timestamps — none may contain '|').
	canonical := fmt.Sprintf(
		"%d\n%s\n%d\n%s\n%d\n%s\n%s",
		len(method), method,
		len(path), path,
		len(bodyHashHex), bodyHashHex,
		strings.Join([]string{
			c.GetHeader(headerUserID),
			c.GetHeader(headerKYCTier),
			c.GetHeader(headerAccountType),
			c.GetHeader(headerEmailVerified),
			c.GetHeader(headerRequestID),
			ts,
		}, "|"),
	)

	mac := hmac.New(sha256.New, secret)
	_, _ = mac.Write([]byte(canonical)) // hmac.Hash.Write never returns an error (documented in hash.Hash)

	return mac.Sum(nil)
}

// storeNonce attempts to set a Redis nonce key with TTL = maxGatewaySkew using
// SET NX EX semantics. Returns true if the key was newly set (nonce is fresh),
// false if the key already existed (replay detected).
// Redis errors are treated as a rejection (fail-closed security posture).
func storeNonce(ctx context.Context, rdb *redis.Client, requestID string) bool {
	key := replayNoncePrefix + requestID
	// SET key 1 NX EX <seconds>: only set if not exists.
	// Returns nil error + "OK" on success; redis.Nil when key existed.
	ok, err := rdb.SetNX(ctx, key, 1, maxGatewaySkew).Result()
	if err != nil {
		// Redis error: fail-closed (reject request) to avoid silently skipping
		// replay protection under Redis outage.
		return false
	}

	return ok
}

// withinSkew reports whether the gateway timestamp is within the allowed replay
// window of the current time.
func withinSkew(tsUnix int64) bool {
	delta := time.Since(time.Unix(tsUnix, 0))
	if delta < 0 {
		delta = -delta
	}

	return delta <= maxGatewaySkew
}

// rejectUnauthorized aborts with a generic 401 that does not leak which check
// failed (missing header vs skew vs signature mismatch all look identical).
func rejectUnauthorized(c *gin.Context) {
	c.Abort()
	httpx.ErrCode(c, http.StatusUnauthorized, "UNAUTHORIZED", "authentication required")
}
