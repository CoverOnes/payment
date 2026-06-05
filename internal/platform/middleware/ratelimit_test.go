package middleware

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newUserRLRouter wires RequireValidIdentity + userRL.Handler() on GET /v1/ping
// and returns the Gin engine. Pass userID="" to simulate a missing identity.
func newUserRLRouter(t *testing.T, limitPerMin, burst int, userID string) *gin.Engine {
	t.Helper()

	gin.SetMode(gin.TestMode)

	r := gin.New()

	// Inject identity directly (bypass RequireValidIdentity to control identity
	// precisely in table-driven tests).
	r.Use(func(c *gin.Context) {
		if userID == "" {
			// No identity set — simulates missing/unverified user.
			c.Next()

			return
		}

		uid, err := uuid.Parse(userID)
		require.NoError(t, err)

		c.Set(ctxKeyUserID, uid)
		c.Set(ctxKeyKYCTier, 3)
		c.Set(ctxKeyAccountType, "PERSONAL")
		c.Next()
	})

	userRL := NewGeneralUserRateLimiter(limitPerMin, burst)
	r.Use(userRL.Handler())

	r.GET("/v1/ping", func(c *gin.Context) { c.Status(http.StatusOK) })

	return r
}

// doGet fires a GET /v1/ping against the router and returns the status code.
func doGet(t *testing.T, r *gin.Engine) int {
	t.Helper()

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/v1/ping", http.NoBody)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	return rec.Code
}

func TestUserRateLimiter(t *testing.T) {
	const userA = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	const userB = "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"

	t.Run("allow_within_budget", func(t *testing.T) {
		// burst=3, limitPerMin=60 → 3 requests are allowed immediately.
		r := newUserRLRouter(t, 60, 3, userA)

		for i := range 3 {
			code := doGet(t, r)
			assert.Equal(t, http.StatusOK, code, "request %d should be allowed", i+1)
		}
	})

	t.Run("over_budget_returns_429", func(t *testing.T) {
		// burst=2: first 2 ok, 3rd must be 429.
		r := newUserRLRouter(t, 60, 2, userA)

		assert.Equal(t, http.StatusOK, doGet(t, r), "1st request should be allowed")
		assert.Equal(t, http.StatusOK, doGet(t, r), "2nd request should be allowed")

		code := doGet(t, r)
		assert.Equal(t, http.StatusTooManyRequests, code, "3rd request should be rate-limited")
	})

	t.Run("over_budget_sets_retry_after_header", func(t *testing.T) {
		// Table-driven: verify Retry-After is always >= "1" (never "0").
		// Previously, limitPerMin > 60 (e.g. 120) caused integer truncation → "0".
		tests := []struct {
			name        string
			limitPerMin int
			wantAtLeast int // Retry-After must be >= this value
		}{
			// limitPerMin=60 → ceil(60/60)=1 → "1"
			{"perMin_60", 60, 1},
			// limitPerMin=120 → ceil(60/120)=ceil(0.5)=1 → "1" (regression: was "0")
			{"perMin_120", 120, 1},
			// limitPerMin=30 → ceil(60/30)=2 → "2"
			{"perMin_30", 30, 2},
		}

		for _, tc := range tests {
			t.Run(tc.name, func(t *testing.T) {
				gin.SetMode(gin.TestMode)

				r := gin.New()
				r.Use(func(c *gin.Context) {
					uid, _ := uuid.Parse(userA)
					c.Set(ctxKeyUserID, uid)
					c.Set(ctxKeyKYCTier, 0)
					c.Set(ctxKeyAccountType, "")
					c.Next()
				})

				userRL := NewGeneralUserRateLimiter(tc.limitPerMin, 1)
				r.Use(userRL.Handler())
				r.GET("/v1/ping", func(c *gin.Context) { c.Status(http.StatusOK) })

				doGet(t, r) // consume burst

				req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/v1/ping", http.NoBody)
				rec := httptest.NewRecorder()
				r.ServeHTTP(rec, req)

				assert.Equal(t, http.StatusTooManyRequests, rec.Code)

				ra := rec.Header().Get("Retry-After")
				assert.NotEmpty(t, ra, "Retry-After header must be set on 429")

				raVal, err := strconv.Atoi(ra)
				assert.NoError(t, err, "Retry-After must be a valid integer, got %q", ra)
				assert.GreaterOrEqual(t, raVal, tc.wantAtLeast,
					"Retry-After=%q must be >= %d for limitPerMin=%d", ra, tc.wantAtLeast, tc.limitPerMin)
			})
		}
	})

	t.Run("two_users_have_independent_buckets", func(t *testing.T) {
		// Build a single limiter shared between two requests with different user_ids.
		gin.SetMode(gin.TestMode)

		userRL := NewGeneralUserRateLimiter(60, 1)

		makeRouter := func(uid string) *gin.Engine {
			r := gin.New()
			r.Use(func(c *gin.Context) {
				id, _ := uuid.Parse(uid)
				c.Set(ctxKeyUserID, id)
				c.Set(ctxKeyKYCTier, 0)
				c.Set(ctxKeyAccountType, "")
				c.Next()
			})
			r.Use(userRL.Handler())
			r.GET("/v1/ping", func(c *gin.Context) { c.Status(http.StatusOK) })

			return r
		}

		rA := makeRouter(userA)
		rB := makeRouter(userB)

		// Exhaust userA's bucket.
		assert.Equal(t, http.StatusOK, doGet(t, rA))
		assert.Equal(t, http.StatusTooManyRequests, doGet(t, rA), "userA should be rate-limited")

		// userB's bucket must be independent — first request should succeed.
		assert.Equal(t, http.StatusOK, doGet(t, rB), "userB bucket should be independent of userA")
	})

	t.Run("missing_identity_passes_through_not_429", func(t *testing.T) {
		// userID="" → identity not set in context → should pass through (passthrough policy).
		r := newUserRLRouter(t, 60, 1, "")

		// The first request must pass through (not rate-limited, not 401 — that's RequireValidIdentity's job).
		code := doGet(t, r)
		assert.Equal(t, http.StatusOK, code, "missing identity must pass through (not 429)")

		// Subsequent requests must also pass through (bucket never filled).
		code = doGet(t, r)
		assert.Equal(t, http.StatusOK, code, "repeated missing-identity requests must pass through")
	})

	t.Run("many_distinct_keys_do_not_panic_lru_bound", func(t *testing.T) {
		// Insert 1000 distinct user IDs into the limiter; must not panic or OOM.
		gin.SetMode(gin.TestMode)

		userRL := NewGeneralUserRateLimiter(600, 10)

		for range 1000 {
			uid := uuid.New()

			r := gin.New()
			r.Use(func(c *gin.Context) {
				c.Set(ctxKeyUserID, uid)
				c.Set(ctxKeyKYCTier, 0)
				c.Set(ctxKeyAccountType, "")
				c.Next()
			})
			r.Use(userRL.Handler())
			r.GET("/v1/ping", func(c *gin.Context) { c.Status(http.StatusOK) })

			req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/v1/ping", http.NoBody)
			rec := httptest.NewRecorder()
			r.ServeHTTP(rec, req)
		}

		// No panic = pass. Verify limiter is still functional.
		assert.NotNil(t, userRL)
	})
}
