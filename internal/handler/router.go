package handler

import (
	"log/slog"
	"time"

	"github.com/CoverOnes/payment/internal/platform/health"
	"github.com/CoverOnes/payment/internal/platform/middleware"
	"github.com/CoverOnes/payment/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
)

// RouterConfig holds all handler-level dependencies.
type RouterConfig struct {
	TransactionSvc *service.TransactionService
	SettlementSvc  *service.SettlementService
	Pool           *pgxpool.Pool
	Redis          *redis.Client // may be nil in dev

	// GatewayHMACSecret is the §24.1 shared secret used to verify the
	// gateway-origin identity signature. Empty == dev posture (verification
	// disabled); config validation guarantees it is non-empty in non-dev.
	GatewayHMACSecret string

	// SettlementS2SToken is the shared secret for RequireServiceIdentity on the
	// settlement disburse endpoint (PAYMENT_SETTLEMENT_S2S_TOKEN).
	SettlementS2SToken string
}

// NewRouter builds and returns the configured Gin engine.
//
// CORS policy: CORS is intentionally NOT applied at this internal service layer.
// payment is reached only via the API gateway, which owns all browser-facing
// CORS handling. (CONVENTIONS §9)
func NewRouter(cfg RouterConfig) *gin.Engine {
	gin.SetMode(gin.ReleaseMode)

	r := gin.New()
	r.SetTrustedProxies(nil) //nolint:errcheck // nil proxy list disables proxy trust; gin docs confirm error is always nil for nil argument

	// Global middleware chain (order per CONVENTIONS §9).
	r.Use(middleware.Recover())
	r.Use(middleware.RequestID())
	r.Use(middleware.SecurityHeaders())
	r.Use(accessLogger())

	// Health endpoints — registered BEFORE the rate limiter so probes are never rate-limited.
	h := health.NewHandler(cfg.Pool)
	r.GET("/healthz", h.Liveness)
	r.GET("/readyz", h.Readiness)

	// Rate limiter — 120 req/min per IP for all API routes below.
	ipRL := middleware.NewIPRateLimiter(cfg.Redis, 120, time.Minute)
	r.Use(ipRL.Handler())

	txH := NewTransactionHandler(cfg.TransactionSvc)

	// All money operations require identity + tier 3 (spec §3.A).
	api := r.Group("/v1")
	// Defense-in-depth (§24.1): verify the gateway-origin HMAC signature BEFORE
	// RequireValidIdentity trusts any X-User-Id / X-Kyc-Tier / X-Account-Type /
	// X-Email-Verified header. When the secret is empty (dev) this is a no-op
	// passthrough, matching the gateway's dev signing-skip.
	api.Use(middleware.VerifyGatewaySignature(cfg.GatewayHMACSecret))
	api.Use(middleware.RequireValidIdentity())

	// Transaction creation and state transitions — tier 3 required (§3.A Tier3 金流).
	api.POST("/transactions", middleware.RequireTier(3), txH.Create)
	api.POST("/transactions/:id/release", middleware.RequireTier(3), txH.Release)
	api.POST("/transactions/:id/refund", middleware.RequireTier(3), txH.Refund)

	// Read endpoints — identity required, no tier gate (but IDOR enforced in handler).
	api.GET("/transactions/:id", txH.GetByID)
	api.GET("/me/transactions", txH.ListMyTransactions)

	// Settlement endpoints.
	// POST /v1/settlement/plans/:id/disburse — manual milestone re-trigger.
	// Gated by:
	//   1. VerifyGatewaySignature + RequireValidIdentity (inherited from api group)
	//   2. RequireTier(3) — caller must be KYC Tier 3
	//   3. RequireServiceIdentity — must present PAYMENT_SETTLEMENT_S2S_TOKEN
	if cfg.SettlementSvc != nil {
		settlementH := NewSettlementHandler(cfg.SettlementSvc)
		settlement := api.Group("/settlement")
		settlement.POST("/plans/:id/disburse",
			middleware.RequireTier(3),
			middleware.RequireServiceIdentity(cfg.SettlementS2SToken),
			settlementH.Disburse,
		)
	}

	return r
}

// accessLogger returns a minimal slog-based access-log middleware.
// Health probe paths (/healthz, /readyz) are excluded to keep logs noise-free.
func accessLogger() gin.HandlerFunc {
	return func(c *gin.Context) {
		path := c.Request.URL.Path
		if path == "/healthz" || path == "/readyz" {
			c.Next()
			return
		}

		start := time.Now()
		c.Next()
		slog.Info(
			"http",
			"method", c.Request.Method,
			"path", path,
			"status", c.Writer.Status(),
			"latency_ms", time.Since(start).Milliseconds(),
			"request_id", c.GetString("request_id"),
		)
	}
}
