package availability

import (
	"testing"
	"time"
)

// TestNewCacheWithKey_FallsBackOnEmpty exercises the empty-string fallback
// without requiring a live Redis connection.
func TestNewCacheWithKey_FallsBackOnEmpty(t *testing.T) {
	c := NewCacheWithKey(nil, time.Second, "")
	if c.key != DefaultOfferingsKey {
		t.Errorf("empty key should fall back to %q, got %q", DefaultOfferingsKey, c.key)
	}
	c2 := NewCacheWithKey(nil, time.Second, "demo:gpu:offerings:all")
	if c2.key != "demo:gpu:offerings:all" {
		t.Errorf("explicit key not honored: got %q", c2.key)
	}
}

// TestNewCache_PreservesLegacyKey confirms the no-arg constructor still
// uses the default key (back-compat for callers that haven't migrated).
func TestNewCache_PreservesLegacyKey(t *testing.T) {
	c := NewCache(nil, time.Second)
	if c.key != DefaultOfferingsKey {
		t.Errorf("NewCache should use %q, got %q", DefaultOfferingsKey, c.key)
	}
}
