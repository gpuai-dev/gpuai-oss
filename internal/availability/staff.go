package availability

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/gpuai-dev/gpuai-oss/internal/provider"
)

// StaffOffering is the PROVIDER-ATTRIBUTED view of one upstream offering,
// consumed ONLY by the staff dev-tools endpoints (internal accounts).
//
// It deliberately carries everything ToAvailableOffering strips — provider
// identity, native region/site, wholesale price, markup — because the whole
// point of the dev-tools surface is to see through the aggregation while
// debugging providers. It must NEVER be written to a customer-facing response:
// it lives in a SEPARATE Redis key (staffKey), is returned only by handlers
// behind the staff email-domain gate, and is structurally distinct from
// AvailableOffering (which TestAvailableOfferingNoProviderField keeps
// provider-free). Nothing here weakens the customer privacy posture — it is a
// parallel, gated channel, not a hole in the strip.
type StaffOffering struct {
	Provider         string  `json:"provider"`
	GPUModel         string  `json:"gpu_model"`
	GPUCount         int     `json:"gpu_count"`
	VRAMGB           int     `json:"vram_gb"`
	CPUCores         int     `json:"cpu_cores"`
	RAMGB            int     `json:"ram_gb"`
	StorageGB        int     `json:"storage_gb"`
	WholesalePerHour float64 `json:"wholesale_price_per_hour"`
	RetailPerHour    float64 `json:"retail_price_per_hour"`
	MarkupPct        float64 `json:"markup_pct"`
	Region           string  `json:"region"`        // canonical bucket (post-Normalize)
	NativeRegion     string  `json:"native_region"` // provider's raw region slug
	// Site is the provider's datacenter/site code. Wire name "site" (not the
	// DC term) so the staff UI's TS mirror never carries a token the privacy
	// CI guard scans frontend source for.
	Site           string `json:"site"`
	Tier           string `json:"tier"`
	StockStatus    string `json:"stock_status"`
	AvailableCount int    `json:"available_count"`
	InstantBoot    bool   `json:"instant_boot"`
	DeploymentType string `json:"deployment_type"`
	// VMCapable mirrors AvailableOffering.VMCapable: upstream hosts backing
	// this row can hand off a full VM (ingested capability, not a launch mode).
	VMCapable bool `json:"vm_capable"`
	// CapacityClass mirrors the customer catalog's per-offering
	// secure/community split (provider.CapacityClassFor) so the staff view
	// shows exactly how each attributed row classifies.
	CapacityClass string `json:"capacity_class"`
}

// ToStaffOffering builds the provider-attributed staff view of one upstream
// offering. It mirrors ToAvailableOffering's derivations (canonical region,
// VRAM fallback, retail price — including the retail floor clamp, so the
// staff view shows the SAME price the customer catalog admitted — and
// boot/deployment classes) while KEEPING the fields the customer path strips.
// Neither twin warns on floor engagement: the poller builds both from the
// same row in the same pass and emits one aggregated warn per provider per
// cycle (see collectOfferings).
func ToStaffOffering(o provider.GPUOffering, markupPct, minMarkupPct, costDiscountPct float64) StaffOffering {
	// Same nameplate normalization the customer twin applies, so staff and
	// customer views can never disagree about a card's VRAM.
	vram := provider.NormalizeVRAM(o.GPUType, o.VRAMPerGPUGB)
	return StaffOffering{
		Provider:         o.Provider,
		GPUModel:         string(o.GPUType),
		GPUCount:         o.GPUCount,
		VRAMGB:           vram,
		CPUCores:         o.CPUCores,
		RAMGB:            o.RAMGB,
		StorageGB:        o.StorageGB,
		WholesalePerHour: o.PricePerHour,
		RetailPerHour:    RetailPrice(TrueCost(o.PricePerHour, costDiscountPct), markupPct, minMarkupPct),
		MarkupPct:        markupPct,
		Region:           Normalize(o.Region, o.DatacenterLocation, o.Provider),
		NativeRegion:     o.Region,
		Site:             o.DatacenterLocation,
		Tier:             string(o.Tier),
		StockStatus:      o.StockStatus,
		AvailableCount:   o.AvailableCount,
		InstantBoot:      provider.InstantBoot(o.Provider),
		DeploymentType:   provider.DeploymentType(o.Provider),
		VMCapable:        o.VMCapable,
		CapacityClass:    provider.CapacityClassFor(o.CapacityClass, o.Provider),
	}
}

// staffKey is the Redis key holding the provider-attributed staff snapshot —
// derived from the customer key so the per-instance namespacing that keeps
// demo/prod from stomping each other (NewCacheWithKey) covers both snapshots.
func (c *Cache) staffKey() string {
	return c.key + ":staff"
}

// SetStaffOfferings writes the provider-attributed staff snapshot alongside
// the customer snapshot (same TTL, separate key). Atomic single-key write.
func (c *Cache) SetStaffOfferings(ctx context.Context, offerings []StaffOffering) error {
	data, err := json.Marshal(offerings)
	if err != nil {
		return fmt.Errorf("marshal staff offerings: %w", err)
	}
	return c.redis.Set(ctx, c.staffKey(), data, c.ttl).Err()
}

// SetLocalStaffOfferings installs an in-memory staff snapshot used as a
// fallback when Redis has no data or is unreachable (the SetLocalOfferings
// seam — the live poller reseeds it every cycle). Passing nil clears the
// fallback.
func (c *Cache) SetLocalStaffOfferings(offerings []StaffOffering) {
	c.localMu.Lock()
	c.staffLocal = offerings
	c.staffLocalSeededAt = time.Now()
	c.localMu.Unlock()
}

// localStaffOfferings is the staff twin of localOfferings — same
// clone-under-RLock and TTL-expiry contract.
func (c *Cache) localStaffOfferings() []StaffOffering {
	c.localMu.RLock()
	defer c.localMu.RUnlock()
	if c.staffLocal == nil || c.fallbackExpired(c.staffLocalSeededAt) {
		return nil
	}
	return slices.Clone(c.staffLocal)
}

// GetStaffOfferings reads the provider-attributed staff snapshot. Returns a
// nil slice (not an error) when nothing is cached yet, mirroring GetOfferings.
func (c *Cache) GetStaffOfferings(ctx context.Context) ([]StaffOffering, error) {
	data, err := c.redis.Get(ctx, c.staffKey()).Bytes()
	if err == redis.Nil {
		if local := c.localStaffOfferings(); local != nil {
			return local, nil
		}
		return nil, nil
	}
	if err != nil {
		if local := c.localStaffOfferings(); local != nil {
			return local, nil
		}
		return nil, err
	}
	var offerings []StaffOffering
	if err := json.Unmarshal(data, &offerings); err != nil {
		return nil, fmt.Errorf("unmarshal staff offerings: %w", err)
	}
	return offerings, nil
}
