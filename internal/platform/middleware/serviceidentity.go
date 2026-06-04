package middleware

import (
	"crypto/subtle"
	"net/http"
	"strings"

	"github.com/CoverOnes/payment/internal/platform/httpx"
	"github.com/gin-gonic/gin"
)

const (
	headerServiceID    = "X-Service-Id"
	headerServiceToken = "X-Service-Token" // header NAME (not a credential value)
	ctxKeyServiceID    = "service_id"
)

// RequireServiceIdentity is a deny-by-default service-to-service (S2S) guard for
// internal-only endpoints (e.g. settlement disburse triggers). A caller MUST present:
//
//	X-Service-Id    — a non-empty caller identifier (recorded for audit)
//	X-Service-Token — the shared PAYMENT_SETTLEMENT_S2S_TOKEN, compared in constant time
//
// This middleware guards endpoints that are reachable only over the internal network
// by trusted services that hold the shared token (backend-security-design §5.5).
// The API gateway MUST NOT proxy these endpoints from the public edge.
//
// If expectedToken is empty the guard FAILS CLOSED (every request is rejected) —
// a missing token must never accidentally open the endpoint. Config validation
// enforces a non-empty token in non-dev when the settlement endpoint is wired;
// this is the runtime backstop.
func RequireServiceIdentity(expectedToken string) gin.HandlerFunc {
	expected := []byte(expectedToken)

	return func(c *gin.Context) {
		if len(expected) == 0 {
			c.Abort()
			httpx.ErrCode(c, http.StatusUnauthorized, "UNAUTHORIZED", "service authentication unavailable")

			return
		}

		serviceID := strings.TrimSpace(c.GetHeader(headerServiceID))
		token := c.GetHeader(headerServiceToken)

		// Constant-time compare; only accept when the service id is also present so
		// an audit trail always has a caller identifier.
		tokenOK := subtle.ConstantTimeCompare([]byte(token), expected) == 1
		if serviceID == "" || !tokenOK {
			c.Abort()
			httpx.ErrCode(c, http.StatusUnauthorized, "UNAUTHORIZED", "service authentication required")

			return
		}

		c.Set(ctxKeyServiceID, serviceID)
		c.Next()
	}
}

// ServiceIDFromCtx returns the authenticated caller service id set by
// RequireServiceIdentity, or "" if absent.
func ServiceIDFromCtx(c *gin.Context) string {
	if v, ok := c.Get(ctxKeyServiceID); ok {
		if id, ok2 := v.(string); ok2 {
			return id
		}
	}

	return ""
}
