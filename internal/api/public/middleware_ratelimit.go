package public

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
)

// Default per-key rate limits (Q-4 lock from 18-CONTEXT.md / RESEARCH.md §9).
// 100 requests-per-second sustained, 200 burst — applied when an API key row
// has rate_limit_rps / rate_limit_burst set to zero.
const (
	DefaultRateLimitRPS   = 100
	DefaultRateLimitBurst = 200
)

// APIKey is the context value populated by Plan 18-08 ApiKeyAuthMiddleware.
// Defined here so the rate limiter compiles + tests independently; the auth
// middleware (Wave 4) will populate the same shape via WithAPIKey, after
// which Wave 5 handlers chain RateLimitMiddleware downstream of auth and
// inherit per-key rate limiting automatically.
type APIKey struct {
	APIKeyID string
	OrgID    string
	UserID   string
	// Role is the key owner's DB role ('owner'/'admin'/'member'), resolved
	// from users.role by the auth middlewares on every request (Phase 39,
	// SEC-01: the DB — not any JWT — is authoritative on the API-key
	// surface). Fail-closed: lookup errors yield "member". Plan 05's
	// member-scoped terminate builds its authorize() Principal from this
	// field without a second lookup.
	Role           string
	Scopes         json.RawMessage // see Plan 18-08 for shape
	RateLimitRPS   int
	RateLimitBurst int
}

// apiKeyCtxKey is the unexported context-key type used to attach an APIKey
// to a request context. Unexported so external callers cannot fabricate
// matching values without going through WithAPIKey.
type apiKeyCtxKey struct{}

// WithAPIKey returns a child context carrying the supplied API key.
// Used by both ApiKeyAuthMiddleware (Plan 18-08) and tests.
func WithAPIKey(ctx context.Context, k *APIKey) context.Context {
	return context.WithValue(ctx, apiKeyCtxKey{}, k)
}

// APIKeyFromContext returns the API key carried in ctx, or nil if absent.
func APIKeyFromContext(ctx context.Context) *APIKey {
	if v, ok := ctx.Value(apiKeyCtxKey{}).(*APIKey); ok {
		return v
	}
	return nil
}

// RateLimiter holds the Redis client used for sliding-window counters.
// Construct via NewRateLimiter; the middleware factory is the
// RateLimitMiddleware method.
type RateLimiter struct {
	rdb *redis.Client
}

// NewRateLimiter constructs a Redis-backed rate limiter. A nil rdb is
// permitted (silent fail-open) for dev/test convenience.
func NewRateLimiter(rdb *redis.Client) *RateLimiter {
	return &RateLimiter{rdb: rdb}
}

// effectiveLimits resolves the (rps, burst) to enforce for a request: the key's
// own limits, or the defaults when the key sets none.
func (rl *RateLimiter) effectiveLimits(ctx context.Context, key *APIKey) (int, int) {
	rps := key.RateLimitRPS
	if rps <= 0 {
		rps = DefaultRateLimitRPS
	}
	burst := key.RateLimitBurst
	if burst <= 0 {
		burst = DefaultRateLimitBurst
	}
	return rps, burst
}

// Allow consults the Redis sliding-window counter for the given API key id.
// Returns:
//   - allowed: true when the current-second count is within rps+burst.
//   - retryAfter: time until the current second rolls over (only meaningful
//     when allowed=false).
//   - err: non-nil on Redis pipeline failure. Callers MUST fail-open
//     (Pitfall 5: a Redis outage should not break customer traffic; matches
//     the Stripe pattern of degrading gracefully when the rate-limit cache
//     hiccups).
//
// Redis key shape: rl:apikey:{api_key_id}:{epoch_second}, EXPIRE 2s.
// The 2-second TTL exceeds the 1-second window so a counter that's hot at
// epoch N still has its rollover EXPIRE pending at epoch N+1.
func (rl *RateLimiter) Allow(ctx context.Context, keyID string, rps, burst int) (bool, time.Duration, error) {
	if rl == nil || rl.rdb == nil {
		// No limiter wired — fail-open silently. Dev/test convenience.
		return true, 0, nil
	}
	now := time.Now().Unix()
	redisKey := fmt.Sprintf("rl:apikey:%s:%d", keyID, now)
	pipe := rl.rdb.TxPipeline()
	incr := pipe.Incr(ctx, redisKey)
	pipe.Expire(ctx, redisKey, 2*time.Second)
	if _, err := pipe.Exec(ctx); err != nil {
		// Fail-open: surface the error to the caller, who will let the request through.
		return true, 0, err
	}
	threshold := int64(rps + burst)
	if incr.Val() > threshold {
		return false, time.Until(time.Unix(now+1, 0)), nil
	}
	return true, 0, nil
}

// RateLimitMiddleware wraps a handler with per-key rate limiting.
// Reads the APIKey from context (set by Plan 18-08 ApiKeyAuthMiddleware);
// when absent, passes through (the auth middleware would have rejected
// unauthenticated requests before reaching here).
//
// Per-key rate_limit_rps / rate_limit_burst from the apikey row override
// the defaults (Q-4 lock); zero or negative values fall back to
// DefaultRateLimitRPS / DefaultRateLimitBurst.
//
// On Redis outage, fails OPEN with a structured Warn log + an unincremented
// counter — see Allow doc-comment for rationale.
func (rl *RateLimiter) RateLimitMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := APIKeyFromContext(r.Context())
		if key == nil {
			// No API key on context — let downstream auth middleware deal with it.
			next.ServeHTTP(w, r)
			return
		}

		rps, burst := rl.effectiveLimits(r.Context(), key)

		allowed, retryAfter, err := rl.Allow(r.Context(), key.APIKeyID, rps, burst)
		if err != nil {
			// Fail-open + structured Warn (Pitfall 5).
			slog.WarnContext(r.Context(), "rate limit redis error — failing open",
				"api_key_id", key.APIKeyID,
				"error", err.Error(),
			)
			next.ServeHTTP(w, r)
			return
		}
		if !allowed {
			seconds := int(retryAfter.Seconds())
			if seconds < 1 {
				seconds = 1
			}
			w.Header().Set("Retry-After", strconv.Itoa(seconds))
			writeProblem(w, r, http.StatusTooManyRequests, "rate_limited",
				"Rate limit exceeded for this API key. Retry after the indicated interval.")
			return
		}
		next.ServeHTTP(w, r)
	})
}
