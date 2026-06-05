package middleware

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/CoverOnes/payment/internal/platform/httpx"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	lru "github.com/hashicorp/golang-lru/v2"
	"github.com/redis/go-redis/v9"
	"golang.org/x/time/rate"
)

// fallbackBurst is the token-bucket burst for the in-process fallback limiter.
const fallbackBurst = 10

// fallbackLRUCap is the maximum number of unique keys tracked by the in-process
// fallback limiter. Bounded LRU prevents memory-DoS from IP rotation attacks.
const fallbackLRUCap = 100_000

// RateLimiter is a Redis-backed fixed-window rate limiter with an in-process
// token-bucket fallback that engages when Redis errors (fails safe, not open).
type RateLimiter struct {
	rdb      *redis.Client
	limit    int
	window   time.Duration
	keyFunc  func(c *gin.Context) string
	fallback *fallbackLimiter
}

// fallbackLimiter holds per-IP token buckets for the in-process safety net.
type fallbackLimiter struct {
	mu      sync.Mutex
	buckets *lru.Cache[string, *rate.Limiter]
	r       rate.Limit
	burst   int
}

func newFallbackLimiter(r rate.Limit, burst int) *fallbackLimiter {
	cache, err := lru.New[string, *rate.Limiter](fallbackLRUCap)
	if err != nil {
		// lru.New only errors when cap <= 0, which cannot happen here.
		panic(fmt.Sprintf("fallbackLimiter: unexpected lru.New error: %v", err))
	}

	return &fallbackLimiter{
		buckets: cache,
		r:       r,
		burst:   burst,
	}
}

func (f *fallbackLimiter) allow(key string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()

	lim, ok := f.buckets.Get(key)
	if !ok {
		lim = rate.NewLimiter(f.r, f.burst)
		f.buckets.Add(key, lim)
	}

	return lim.Allow()
}

// NewIPRateLimiter builds a limiter keyed by client IP.
func NewIPRateLimiter(rdb *redis.Client, limit int, window time.Duration) *RateLimiter {
	r := rate.Limit(float64(limit) / window.Seconds())

	return &RateLimiter{
		rdb:    rdb,
		limit:  limit,
		window: window,
		keyFunc: func(c *gin.Context) string {
			return fmt.Sprintf("payment:rl:ip:%s", c.ClientIP())
		},
		fallback: newFallbackLimiter(r, fallbackBurst),
	}
}

// Handler returns the Gin middleware function.
func (rl *RateLimiter) Handler() gin.HandlerFunc {
	return func(c *gin.Context) {
		if rl.rdb == nil {
			key := rl.keyFunc(c)
			if !rl.fallback.allow(key) {
				c.Abort()
				httpx.ErrCode(c, http.StatusTooManyRequests, "RATE_LIMITED", "too many requests, please try again later")

				return
			}

			c.Next()

			return
		}

		key := rl.keyFunc(c)
		ctx := c.Request.Context()

		count, err := rl.increment(ctx, key)
		if err != nil {
			slog.Warn("rate limiter redis error; applying in-process fallback limiter", "err", err)

			if !rl.fallback.allow(key) {
				c.Abort()
				httpx.ErrCode(c, http.StatusTooManyRequests, "RATE_LIMITED", "too many requests, please try again later")

				return
			}

			c.Next()

			return
		}

		if count > rl.limit {
			c.Abort()
			httpx.ErrCode(c, http.StatusTooManyRequests, "RATE_LIMITED", "too many requests, please try again later")

			return
		}

		c.Next()
	}
}

func (rl *RateLimiter) increment(ctx context.Context, key string) (int, error) {
	pipe := rl.rdb.Pipeline()
	incr := pipe.Incr(ctx, key)
	pipe.ExpireNX(ctx, key, rl.window)

	_, err := pipe.Exec(ctx)
	if err != nil {
		return 0, err
	}

	return int(incr.Val()), nil
}

// userFallbackLRUCap is the maximum number of unique user IDs tracked by the
// in-process per-user limiter. Bounding by LRU prevents memory exhaustion under
// high account-rotation attacks.
const userFallbackLRUCap = 100_000

// UserRateLimiter is a per-authenticated-user in-process token-bucket rate limiter.
// Key is derived from the verified user_id set in gin context by RequireValidIdentity.
// When user_id is absent — which should not happen on properly-wired routes since
// RequireValidIdentity always runs before this middleware — the request is passed
// through with a Warn log (belt-and-suspenders; the IP limiter still applies).
//
// Multi-pod caveat: this is an in-process limiter. Each pod maintains its own bucket,
// so the effective per-user limit across N pods is N×limitPerMin. A Redis sliding-window
// implementation is the recommended follow-up when accurate cross-pod enforcement is required.
type UserRateLimiter struct {
	mu          sync.Mutex
	buckets     *lru.Cache[string, *rate.Limiter]
	r           rate.Limit
	burst       int
	limitPerMin int
}

// NewGeneralUserRateLimiter builds a per-authenticated-user rate limiter keyed on
// the verified user_id from gin context (set by RequireValidIdentity).
// limitPerMin must be >= 0 (0 = disabled, caller checks); burst must be > 0.
// Caller is responsible for validating these at config load time.
func NewGeneralUserRateLimiter(limitPerMin, burst int) *UserRateLimiter {
	r := rate.Limit(float64(limitPerMin) / 60.0)

	cache, err := lru.New[string, *rate.Limiter](userFallbackLRUCap)
	if err != nil {
		// lru.New only errors when cap <= 0, which cannot happen here.
		panic(fmt.Sprintf("UserRateLimiter: unexpected lru.New error: %v", err))
	}

	return &UserRateLimiter{
		buckets:     cache,
		r:           r,
		burst:       burst,
		limitPerMin: limitPerMin,
	}
}

func (l *UserRateLimiter) allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	lim, ok := l.buckets.Get(key)
	if !ok {
		lim = rate.NewLimiter(l.r, l.burst)
		l.buckets.Add(key, lim)
	}

	return lim.Allow()
}

// Handler returns the Gin middleware function for per-user rate limiting.
//
// Key derivation: reads the verified identity from gin context via IdentityFromCtx
// (set by RequireValidIdentity). NEVER reads the raw X-User-Id header directly.
// If the identity is absent (misconfigured route — RequireValidIdentity not wired),
// the request is passed through with a Warn log; the IP-level limiter still applies.
//
// On deny: sets Retry-After header and returns 429 RATE_LIMITED.
func (l *UserRateLimiter) Handler() gin.HandlerFunc {
	retryAfter := strconv.Itoa(int(60.0 / float64(l.limitPerMin)))

	return func(c *gin.Context) {
		identity, ok := IdentityFromCtx(c)
		if !ok || identity.UserID == uuid.Nil {
			slog.Warn(
				"user rate limiter: no verified user_id in context; passing through",
				"path", c.Request.URL.Path,
			)
			c.Next()

			return
		}

		key := "payment:rl:user:" + identity.UserID.String()

		if !l.allow(key) {
			c.Header("Retry-After", retryAfter)
			c.Abort()
			httpx.ErrCode(c, http.StatusTooManyRequests, "RATE_LIMITED", "too many requests, please try again later")

			return
		}

		c.Next()
	}
}
