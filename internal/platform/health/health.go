// Package health provides liveness and readiness check helpers for the payment service.
package health

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Handler provides /healthz and /readyz gin handler functions.
type Handler struct {
	pool *pgxpool.Pool
}

// NewHandler returns a health handler with the given pool.
func NewHandler(pool *pgxpool.Pool) *Handler {
	return &Handler{pool: pool}
}

// Liveness always returns 200 if the process is serving.
func (h *Handler) Liveness(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}

// Readiness pings Postgres; returns 503 if any dependency is down.
func (h *Handler) Readiness(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 3*time.Second)
	defer cancel()

	checks := gin.H{}
	allOK := true

	if err := h.pool.Ping(ctx); err != nil {
		slog.Warn("readiness: postgres ping failed", "err", err)
		checks["postgres"] = "down"
		allOK = false
	} else {
		checks["postgres"] = "ok"
	}

	if allOK {
		c.JSON(http.StatusOK, gin.H{"status": "ready", "checks": checks})
		return
	}

	c.JSON(http.StatusServiceUnavailable, gin.H{"status": "not_ready", "checks": checks})
}
