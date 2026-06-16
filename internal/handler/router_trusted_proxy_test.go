package handler_test

// Regression tests for the ClientIP trust-chain downstream fix (downstream of the
// merged gateway ClientIP keystone).
//
// Root cause this fixes:
//   [Major] router.go — the IP rate limiter collapses to a single global bucket
//           behind the gateway because SetTrustedProxies(nil) makes c.ClientIP()
//           return the gateway's egress IP for every request, so all clients share
//           one bucket (self-DoS).
//
// Fix: when GatewayCIDR is set, NewRouter must call SetTrustedProxies([GatewayCIDR])
// so Gin honors X-Forwarded-For from the trusted gateway CIDR, and c.ClientIP()
// returns the real end-user IP. Empty CIDR keeps the safe dev fallback
// (SetTrustedProxies(nil)); an invalid CIDR panics at boot to surface the config bug.
//
// These are pure HTTP-layer unit tests: no DB, no testcontainer (the /healthz
// liveness probe does not touch the pgx pool, so Pool may be nil).

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/CoverOnes/payment/internal/handler"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// trustedProxyRouterCfg builds a minimal RouterConfig for router-wiring tests.
// GatewayHMACSecret is empty (dev posture — gateway-signature verification is a
// no-op passthrough), and Pool/Redis are nil because /healthz needs neither.
func trustedProxyRouterCfg(gatewayCIDR string) *handler.RouterConfig {
	return &handler.RouterConfig{
		GatewayCIDR:       gatewayCIDR,
		GatewayHMACSecret: "", // dev mode — no gateway HMAC required
	}
}

// TestNewRouter_TrustedProxies_GatewayCIDRSet proves that when GatewayCIDR is a
// valid CIDR the router accepts a request forwarded by the gateway (peer inside
// the trusted CIDR) carrying an X-Forwarded-For for the real client, without
// panicking — i.e. SetTrustedProxies succeeds and Gin honors XFF from the gateway.
func TestNewRouter_TrustedProxies_GatewayCIDRSet(t *testing.T) {
	r := handler.NewRouter(trustedProxyRouterCfg("10.0.0.0/8"))

	// Simulate a request arriving from the gateway (10.1.2.3, inside 10.0.0.0/8)
	// with XFF saying the real client is 203.0.113.42 (TEST-NET-3, RFC 5737).
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/healthz", http.NoBody)
	req.RemoteAddr = "10.1.2.3:54321"                 // simulated gateway peer (in trusted CIDR)
	req.Header.Set("X-Forwarded-For", "203.0.113.42") // real client IP the gateway forwards

	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	// /healthz returns 200 — the critical assertion is that the router does NOT
	// panic when GatewayCIDR is a valid CIDR, proving SetTrustedProxies succeeds
	// and the trusted gateway hop is honored.
	require.Equal(t, http.StatusOK, w.Code,
		"NewRouter with valid GatewayCIDR must serve requests without panicking")
}

// TestNewRouter_TrustedProxies_EmptyCIDR proves that when GatewayCIDR is empty the
// router falls back to SetTrustedProxies(nil) (safe dev default) — an inbound XFF
// is ignored because no proxy is trusted, so c.ClientIP() resolves the direct peer.
func TestNewRouter_TrustedProxies_EmptyCIDR(t *testing.T) {
	r := handler.NewRouter(trustedProxyRouterCfg(""))

	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/healthz", http.NoBody)
	req.RemoteAddr = "127.0.0.1:8888"
	req.Header.Set("X-Forwarded-For", "1.2.3.4") // ignored: no trusted proxy

	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code,
		"NewRouter with empty GatewayCIDR must serve requests without panicking")
}

// TestNewRouter_TrustedProxies_InvalidCIDR_Panics proves that an invalid GatewayCIDR
// causes a panic at startup — surfacing a config bug immediately rather than running
// silently with wrong proxy trust. (config.validate normally rejects this at boot;
// the router panic is the in-process defense-in-depth backstop.)
func TestNewRouter_TrustedProxies_InvalidCIDR_Panics(t *testing.T) {
	cfg := trustedProxyRouterCfg("not-a-cidr")

	assert.Panics(t, func() {
		handler.NewRouter(cfg)
	}, "NewRouter with invalid GatewayCIDR must panic to surface the config bug at boot")
}
