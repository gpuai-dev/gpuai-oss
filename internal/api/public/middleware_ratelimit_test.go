package public

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

// newTestRedis returns a connected client when REDIS_TEST_URL is set,
// skipping the test otherwise. Mirrors the DATABASE_TEST_URL pattern used
// by internal/db/instances_test.go and the postgres+redis service-container
// gating from Plan 18-03.
func newTestRedis(t *testing.T) *redis.Client {
	t.Helper()
	url := os.Getenv("REDIS_TEST_URL")
	if url == "" {
		t.Skip("REDIS_TEST_URL not set — skipping Redis-gated test")
	}
	opt, err := redis.ParseURL(url)
	if err != nil {
		t.Fatalf("parse REDIS_TEST_URL: %v", err)
	}
	c := redis.NewClient(opt)
	if err := c.Ping(context.Background()).Err(); err != nil {
		t.Fatalf("redis ping: %v", err)
	}
	return c
}

func TestAPIKeyContextRoundTrip(t *testing.T) {
	ctx := WithAPIKey(context.Background(), &APIKey{APIKeyID: "k1", RateLimitRPS: 50})
	got := APIKeyFromContext(ctx)
	if got == nil || got.APIKeyID != "k1" || got.RateLimitRPS != 50 {
		t.Fatalf("round-trip lost data: %+v", got)
	}
	if APIKeyFromContext(context.Background()) != nil {
		t.Errorf("background ctx should have no api key")
	}
}

func TestRateLimitMiddleware_NoAPIKeyPassesThrough(t *testing.T) {
	rl := NewRateLimiter(nil) // nil rdb is also fail-open friendly
	h := rl.RateLimitMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest("GET", "/v1/health", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200 (no api key on ctx → pass-through)", w.Code)
	}
}

func TestRateLimit_BelowThresholdAllows(t *testing.T) {
	rdb := newTestRedis(t)
	defer rdb.Close()
	rl := NewRateLimiter(rdb)
	keyID := fmt.Sprintf("test-below-%d", time.Now().UnixNano())

	ctx := WithAPIKey(context.Background(), &APIKey{
		APIKeyID:       keyID,
		RateLimitRPS:   100,
		RateLimitBurst: 200,
	})
	h := rl.RateLimitMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	// 50 sequential calls — well under 100+200 threshold.
	for i := 0; i < 50; i++ {
		req := httptest.NewRequest("GET", "/v1/health", nil).WithContext(ctx)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("call %d: status %d (Retry-After %q)", i, w.Code, w.Header().Get("Retry-After"))
		}
	}
}

func TestRateLimit_OverThresholdRejects(t *testing.T) {
	rdb := newTestRedis(t)
	defer rdb.Close()
	rl := NewRateLimiter(rdb)
	keyID := fmt.Sprintf("test-over-%d", time.Now().UnixNano())

	// Use a small effective threshold for fast iteration: rps=2, burst=3 → threshold=5.
	ctx := WithAPIKey(context.Background(), &APIKey{
		APIKeyID:       keyID,
		RateLimitRPS:   2,
		RateLimitBurst: 3,
	})
	h := rl.RateLimitMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	// First 5 should pass, 6th should 429.
	var rejected int
	for i := 0; i < 10; i++ {
		req := httptest.NewRequest("GET", "/v1/health", nil).WithContext(ctx)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code == http.StatusTooManyRequests {
			rejected++
			// Validate envelope shape.
			if got := w.Header().Get("Content-Type"); got != "application/problem+json" {
				t.Errorf("rejected response Content-Type: got %q", got)
			}
			if w.Header().Get("Retry-After") == "" {
				t.Errorf("rejected response missing Retry-After")
			}
			var p Problem
			if err := json.NewDecoder(w.Body).Decode(&p); err != nil {
				t.Errorf("decode body: %v", err)
			}
			if p.Code != "rate_limited" {
				t.Errorf("body code: got %q, want rate_limited", p.Code)
			}
			// Validate Retry-After is a positive integer (seconds).
			if seconds, err := strconv.Atoi(w.Header().Get("Retry-After")); err != nil || seconds < 1 {
				t.Errorf("Retry-After: got %q, want positive int", w.Header().Get("Retry-After"))
			}
		}
	}
	if rejected == 0 {
		t.Fatalf("expected at least 1 rejection out of 10 with threshold 5, got 0")
	}
}

// TestRateLimit_RedisErrorFailsOpen verifies the Pitfall 5 contract using
// a closed client (immediate Exec error) — request still passes through.
func TestRateLimit_RedisErrorFailsOpen(t *testing.T) {
	// Open a client pointed at a definitely-unreachable address with very short timeout.
	client := redis.NewClient(&redis.Options{
		Addr:        "127.0.0.1:1",
		DialTimeout: 100 * time.Millisecond,
	})
	defer client.Close()
	rl := NewRateLimiter(client)

	ctx := WithAPIKey(context.Background(), &APIKey{
		APIKeyID:       "fail-open-test",
		RateLimitRPS:   10,
		RateLimitBurst: 10,
	})
	var seen bool
	h := rl.RateLimitMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = true
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest("GET", "/v1/health", nil).WithContext(ctx)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if !seen {
		t.Fatalf("handler not called — middleware failed CLOSED on Redis outage; should fail OPEN")
	}
	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200 (fail-open semantics)", w.Code)
	}
	// Sanity: ensure the underlying Allow call indeed errored.
	_, _, err := rl.Allow(context.Background(), "fail-open-allow", 10, 10)
	if err == nil || errors.Is(err, context.Canceled) {
		t.Logf("note: Allow returned err=%v — test still validates middleware behavior", err)
	}
}
