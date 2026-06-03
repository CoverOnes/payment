package middleware

import (
	"net/http"
	"strconv"

	"github.com/CoverOnes/payment/internal/platform/httpx"
	"github.com/CoverOnes/payment/internal/platform/logger"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

const (
	ctxKeyUserID      = "user_id"
	ctxKeyKYCTier     = "kyc_tier"
	ctxKeyAccountType = "account_type"

	headerUserID      = "X-User-Id"
	headerKYCTier     = "X-Kyc-Tier"
	headerAccountType = "X-Account-Type"
)

// Identity holds the parsed gateway-injected identity for a request.
type Identity struct {
	UserID      uuid.UUID
	KYCTier     int
	AccountType string
}

// RequireValidIdentity parses the gateway-injected X-User-Id header as a UUID.
// Returns 401 UNAUTHORIZED if the header is missing or not a valid UUID.
// This is the defense-in-depth guard: requests without a valid X-User-Id were not
// routed via the gateway (which verifies the JWT and injects identity headers).
func RequireValidIdentity() gin.HandlerFunc {
	return func(c *gin.Context) {
		rawID := c.GetHeader(headerUserID)
		if rawID == "" {
			c.Abort()
			httpx.ErrCode(c, http.StatusUnauthorized, "UNAUTHORIZED", "authentication required")

			return
		}

		userID, err := uuid.Parse(rawID)
		if err != nil {
			c.Abort()
			httpx.ErrCode(c, http.StatusUnauthorized, "UNAUTHORIZED", "invalid identity header")

			return
		}

		// Parse optional tier (default 0 if missing/invalid).
		tier := 0

		if rawTier := c.GetHeader(headerKYCTier); rawTier != "" {
			if t, parseErr := strconv.Atoi(rawTier); parseErr == nil {
				tier = t
			}
		}

		accountType := c.GetHeader(headerAccountType)

		c.Set(ctxKeyUserID, userID)
		c.Set(ctxKeyKYCTier, tier)
		c.Set(ctxKeyAccountType, accountType)

		c.Request = c.Request.WithContext(
			logger.WithUserID(c.Request.Context(), userID.String()),
		)

		c.Next()
	}
}

// RequireTier returns a middleware that enforces a minimum KYC tier.
// MUST be used after RequireValidIdentity.
func RequireTier(minTier int) gin.HandlerFunc {
	return func(c *gin.Context) {
		raw, ok := c.Get(ctxKeyKYCTier)
		if !ok {
			c.Abort()
			httpx.ErrCode(c, http.StatusUnauthorized, "UNAUTHORIZED", "authentication required")

			return
		}

		tier, ok := raw.(int)
		if !ok {
			c.Abort()
			httpx.ErrCode(c, http.StatusUnauthorized, "UNAUTHORIZED", "authentication required")

			return
		}

		if tier < minTier {
			c.Abort()
			httpx.ErrCode(
				c, http.StatusForbidden, "KYC_TIER_REQUIRED", "kyc verification required",
				gin.H{
					"requiredTier": minTier,
					"currentTier":  tier,
				},
			)

			return
		}

		c.Next()
	}
}

// IdentityFromCtx extracts the parsed Identity set by RequireValidIdentity.
func IdentityFromCtx(c *gin.Context) (Identity, bool) {
	rawUID, ok := c.Get(ctxKeyUserID)
	if !ok {
		return Identity{}, false
	}

	userID, ok := rawUID.(uuid.UUID)
	if !ok {
		return Identity{}, false
	}

	tier := 0

	if rawTier, exists := c.Get(ctxKeyKYCTier); exists {
		if t, ok2 := rawTier.(int); ok2 {
			tier = t
		}
	}

	accountType := ""

	if rawAT, exists := c.Get(ctxKeyAccountType); exists {
		if at, ok2 := rawAT.(string); ok2 {
			accountType = at
		}
	}

	return Identity{
		UserID:      userID,
		KYCTier:     tier,
		AccountType: accountType,
	}, true
}
