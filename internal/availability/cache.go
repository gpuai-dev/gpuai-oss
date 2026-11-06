package availability

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

// DefaultOfferingsKey is the Redis key used when no override is configured.
// Multi-tenant droplets that run multiple binaries against the same Redis
// instance MUST set a per-instance key (see Cache.key) to avoid cross-env
// stomping on every poll cycle.
const DefaultOfferingsKey = "gpu:offerings:all"

// Cache provides Redis-backed storage for GPU offerings.
// Uses a single key with JSON array for atomic reads (no SCAN needed).
type Cache struct {
	redis *redis.Client
	ttl   time.Duration
	key   string
	// localMu guards local and staffLocal: the live poller reseeds both every
	// cycle while API handlers read them on any Redis miss, from separate
	// goroutines.
	localMu sync.RWMutex
	// local is an in-memory offerings snapshot served as a fallback when Redis
	// is unreachable or the key has expired. The live poller reseeds it on
	// every cycle (Poller.poll), so a Redis-write that lands after the previous
	// snapshot's TTL — the empty-catalog race a slow provider used to open —
	// degrades to a one-cycle-stale catalog instead of an empty 200. Tests
	// exercising the read-path filters seed it directly via SetLocalOfferings.
	local []AvailableOffering
	// localSeededAt is when local was last seeded. The fallback expires after
	// c.ttl just like the Redis key: without a bound, a poller that dies or
	// wedges (its goroutine panic-recovers without restart; a ctx-ignoring
	// adapter can hang collectOfferings forever) would freeze the catalog
	// silently — prices, stock and all — where the pre-fallback behavior
	// failed loud with an empty catalog within one TTL.
	localSeededAt time.Time
	// staffLocal is the staff-snapshot twin of local (SetLocalStaffOfferings —
	// see staff.go), reseeded the same way in the same cycle and expiring on
	// the same clock.
	staffLocal         []StaffOffering
	staffLocalSeededAt time.Time
}

// NewCache creates a new Cache with the specified TTL using the default key.
func NewCache(redisClient *redis.Client, ttl time.Duration) *Cache {
	return NewCacheWithKey(redisClient, ttl, DefaultOfferingsKey)
}

// NewCacheWithKey creates a Cache that reads/writes the supplied Redis key.
// Empty key falls back to DefaultOfferingsKey so an unset env var preserves
// the legacy behavior.
func NewCacheWithKey(redisClient *redis.Client, ttl time.Duration, key string) *Cache {
	if key == "" {
		key = DefaultOfferingsKey
	}
	return &Cache{
		redis: redisClient,
		ttl:   ttl,
		key:   key,
	}
}

// SetOfferings writes all offerings as a single JSON array to Redis with TTL.
// Atomic: readers always see a complete snapshot, never partial data.
func (c *Cache) SetOfferings(ctx context.Context, offerings []AvailableOffering) error {
	data, err := json.Marshal(offerings)
	if err != nil {
		return fmt.Errorf("marshal offerings: %w", err)
	}
	return c.redis.Set(ctx, c.key, data, c.ttl).Err()
}

// SetLocalOfferings installs an in-memory offerings snapshot used as a
// fallback when Redis has no data or is unreachable (see Cache.local). Passing
// nil clears the fallback. The slice is retained by reference — callers must
// not mutate it after handing it over.
func (c *Cache) SetLocalOfferings(offerings []AvailableOffering) {
	c.localMu.Lock()
	c.local = offerings
	c.localSeededAt = time.Now()
	c.localMu.Unlock()
}

// fallbackExpired reports whether a fallback seed has outlived the cache TTL —
// the same freshness contract the Redis key gets, so a dead poller fails loud
// (empty catalog) instead of freezing the last snapshot forever. A
// non-positive TTL disables expiry, keeping struct-literal/zero-TTL test
// caches usable.
func (c *Cache) fallbackExpired(seededAt time.Time) bool {
	return c.ttl > 0 && time.Since(seededAt) > c.ttl
}

// localOfferings returns a clone of the fallback snapshot (nil if none is
// seeded, or the seed has outlived the TTL). Cloned because the Redis path
// hands every reader a fresh slice and the shared fallback must be as safe
// under concurrent reads. The clone is SHALLOW: readers may sort/filter the
// outer slice and reassign scalar/string fields freely, but element writes
// into a row's OSOptions would still hit the shared backing array — don't.
func (c *Cache) localOfferings() []AvailableOffering {
	c.localMu.RLock()
	defer c.localMu.RUnlock()
	if c.local == nil || c.fallbackExpired(c.localSeededAt) {
		return nil
	}
	return slices.Clone(c.local)
}

// GetOfferings reads all cached offerings from Redis.
// Returns nil slice (not error) if no data is cached yet. When a local
// fallback snapshot has been installed (SetLocalOfferings — the live poller
// reseeds it every cycle), it is returned whenever Redis holds no data or is
// unreachable — so the read path stays serviceable without a live Redis.
func (c *Cache) GetOfferings(ctx context.Context) ([]AvailableOffering, error) {
	data, err := c.redis.Get(ctx, c.key).Bytes()
	if err == redis.Nil {
		if local := c.localOfferings(); local != nil {
			return local, nil // Redis empty — serve the seeded fallback.
		}
		return nil, nil // No cached data yet
	}
	if err != nil {
		if local := c.localOfferings(); local != nil {
			return local, nil // Redis unreachable — serve the seeded fallback.
		}
		return nil, err
	}
	var offerings []AvailableOffering
	if err := json.Unmarshal(data, &offerings); err != nil {
		return nil, fmt.Errorf("unmarshal offerings: %w", err)
	}
	return offerings, nil
}

// Read is the canonical read entry-point used by public-API handlers
// (Plan 18-12 onward). Delegates to GetOfferings — kept as an alias because
// callers in internal/api/public refer to this method name in their plan
// contracts. Returning nil offerings without an error preserves the
// "no data cached yet" semantics; callers treat that as an empty catalog
// rather than a 503.
func (c *Cache) Read(ctx context.Context) ([]AvailableOffering, error) {
	return c.GetOfferings(ctx)
}

// KnownGPUModels returns the set of gpu_type values currently present in the
// offerings cache — identical to the set GET /v1/gpu-types surfaces (both read
// the same snapshot via Read). The instance-create handlers use it to reject an
// unknown gpu_type synchronously (issue #99) instead of accepting it with a 202
// that fails ~7s later in the async provision path with the misleading
// "no suitable provider available".
//
// A genuinely-offered gpu_type that is merely sold out stays in the set — the
// poller lists zero-availability offerings without filtering — so it still
// flows to the async path and the existing "no capacity" behaviour is
// preserved. Only never-offered values (typos like "not_a_real_gpu") are
// absent. Read's (nil, nil) "no data cached yet" result yields an empty set,
// which callers treat as "can't validate" and fail open.
func (c *Cache) KnownGPUModels(ctx context.Context) (map[string]struct{}, error) {
	offerings, err := c.Read(ctx)
	if err != nil {
		return nil, err
	}
	set := make(map[string]struct{}, len(offerings))
	for _, o := range offerings {
		set[o.GPUModel] = struct{}{}
	}
	return set, nil
}
