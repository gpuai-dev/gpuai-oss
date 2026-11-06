package public

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gpuai-dev/gpuai-oss/internal/availability"
)

// TestHandleListPricing_DiskConfigurableSeparatesRows is the silent-collapse
// guard (Pitfall 1) for the new attribute, and the reason disk_configurable
// joins offerKey rather than riding on the aggregate.
//
// The container providers SPLIT on this capability — some honor a requested
// disk, some drop it on the floor — so two same-SKU offerings can differ in it
// alone. If they merged, the cheapest price would decide the row and stamp its
// capability onto capacity that does not have it: the launch UI would render a
// disk control that fails on submit, which is precisely what publishing this
// attribute is supposed to prevent.
func TestHandleListPricing_DiskConfigurableSeparatesRows(t *testing.T) {
	cache := catalogTestCache(t, []availability.AvailableOffering{
		{GPUModel: "h100_sxm", GPUCount: 1, VRAMGB: 80, PricePerHour: 2.40, Region: "us-west", Tier: "on_demand",
			AvailableCount: 3, DeploymentType: "container", DiskConfigurable: true, InstanceDiskGB: 100},
		// Same SKU, same deployment type, CHEAPER — would win the merge and
		// carry its non-configurable capability onto the row above.
		{GPUModel: "h100_sxm", GPUCount: 1, VRAMGB: 80, PricePerHour: 2.10, Region: "us-west", Tier: "on_demand",
			AvailableCount: 7, DeploymentType: "container", DiskConfigurable: false, InstanceDiskGB: 0},
	})

	page := listPricing(t, PublicDeps{AvailCache: cache}, "/v1/pricing")
	if len(page.Data) != 2 {
		t.Fatalf("same SKU differing only in disk_configurable: got %d rows, want 2 (silent collapse); rows=%+v",
			len(page.Data), page.Data)
	}

	var configurable, fixed *Pricing
	for i := range page.Data {
		if page.Data[i].DiskConfigurable {
			configurable = &page.Data[i]
		} else {
			fixed = &page.Data[i]
		}
	}
	if configurable == nil || fixed == nil {
		t.Fatalf("expected one configurable + one fixed row, got %+v", page.Data)
	}
	if configurable.Available != 3 || configurable.PricePerHour != 2.40 {
		t.Errorf("configurable row: got avail=%d price=%v, want avail=3 price=2.40 (never merged)",
			configurable.Available, configurable.PricePerHour)
	}
	if fixed.Available != 7 || fixed.PricePerHour != 2.10 {
		t.Errorf("fixed row: got avail=%d price=%v, want avail=7 price=2.10 (capability never transferred)",
			fixed.Available, fixed.PricePerHour)
	}

	b, err := json.Marshal(*configurable)
	if err != nil {
		t.Fatalf("marshal configurable row: %v", err)
	}
	if !strings.Contains(string(b), `"disk_configurable":true`) {
		t.Errorf("configurable row missing \"disk_configurable\":true, got: %s", string(b))
	}
	if !strings.Contains(string(b), `"instance_disk_gb":100`) {
		t.Errorf("configurable row missing \"instance_disk_gb\":100, got: %s", string(b))
	}
}

// TestHandleListPricing_InstanceDiskFollowsPricedOffer pins instance_disk_gb to
// the offer whose price the row shows, the same rule cpu_cores/ram_gb/storage_gb
// follow. Disk is a per-machine NUMBER, not a capability class, so it belongs on
// the aggregate rather than in the key — but it must still describe the machine
// the customer would actually get at the advertised price.
func TestHandleListPricing_InstanceDiskFollowsPricedOffer(t *testing.T) {
	cache := catalogTestCache(t, []availability.AvailableOffering{
		// First sighting: pricier, big disk. Seeds the aggregate.
		{GPUModel: "a100_pcie", GPUCount: 1, VRAMGB: 40, PricePerHour: 1.80, Region: "us-east", Tier: "on_demand",
			AvailableCount: 2, DeploymentType: "vm", InstanceDiskGB: 1000, StorageGB: 1000},
		// Cheaper sibling with a smaller disk — this is the machine we advertise.
		{GPUModel: "a100_pcie", GPUCount: 1, VRAMGB: 40, PricePerHour: 1.20, Region: "us-east", Tier: "on_demand",
			AvailableCount: 5, DeploymentType: "vm", InstanceDiskGB: 250, StorageGB: 250},
	})

	page := listPricing(t, PublicDeps{AvailCache: cache}, "/v1/pricing")
	if len(page.Data) != 1 {
		t.Fatalf("identical offers must merge: got %d rows, want 1; rows=%+v", len(page.Data), page.Data)
	}
	row := page.Data[0]
	if row.PricePerHour != 1.20 {
		t.Fatalf("price = %v, want 1.20 (cheapest wins)", row.PricePerHour)
	}
	if row.InstanceDiskGB != 250 {
		t.Errorf("instance_disk_gb = %d, want 250: disk must describe the machine at the advertised price, "+
			"not the first offer sighted (1000 would mean specs and price describe different machines)",
			row.InstanceDiskGB)
	}
	if row.Available != 7 {
		t.Errorf("available = %d, want 7 (summed across merged offers)", row.Available)
	}
}

// TestHandleListGPUTypes_DiskConfigurableIsOredAcrossSightings covers the one
// deliberate departure from the model-level endpoint's first-sighting rule.
//
// /v1/gpu-types answers per MODEL, and a model is served by several upstreams
// with different capabilities. disk_configurable there answers "may I send
// disk_gb for this model?" — requesting it narrows placement to the capable
// upstreams, so "at least one" is the true answer. First-sighting would return a
// coin flip and hide a real capability half the time. The specs, which are
// merely representative, keep first-sighting.
func TestHandleListGPUTypes_DiskConfigurableIsOredAcrossSightings(t *testing.T) {
	cache := catalogTestCache(t, []availability.AvailableOffering{
		// First sighting is NOT configurable — the case that would answer false.
		{GPUModel: "h100_sxm", GPUCount: 1, VRAMGB: 80, PricePerHour: 2.50, Region: "us-east", Tier: "on_demand",
			AvailableCount: 4, CPUCores: 16, RAMGB: 128, StorageGB: 500, InstanceDiskGB: 500, DiskConfigurable: false},
		{GPUModel: "h100_sxm", GPUCount: 1, VRAMGB: 80, PricePerHour: 2.20, Region: "us-west", Tier: "on_demand",
			AvailableCount: 6, CPUCores: 32, RAMGB: 256, StorageGB: 0, InstanceDiskGB: 100, DiskConfigurable: true},
		// A model with no configurable capacity anywhere must stay false.
		{GPUModel: "a100_pcie", GPUCount: 1, VRAMGB: 40, PricePerHour: 1.20, Region: "us-east", Tier: "on_demand",
			AvailableCount: 8, CPUCores: 8, RAMGB: 64, StorageGB: 200, InstanceDiskGB: 200, DiskConfigurable: false},
	})

	mux := http.NewServeMux()
	RegisterCatalogRoutes(mux, PublicDeps{AvailCache: cache})
	req := httptest.NewRequest(http.MethodGet, "/v1/gpu-types", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("GET /v1/gpu-types: got %d, want 200; body=%s", w.Code, w.Body.String())
	}

	var page Page[GPUType]
	if err := json.NewDecoder(w.Body).Decode(&page); err != nil {
		t.Fatalf("decode page: %v", err)
	}
	rows := map[string]GPUType{}
	for _, r := range page.Data {
		rows[r.GPUType] = r
	}

	h100, ok := rows["h100_sxm"]
	if !ok {
		t.Fatalf("h100_sxm missing from %+v", page.Data)
	}
	if !h100.DiskConfigurable {
		t.Error("h100_sxm disk_configurable = false, want true: one of its offerings accepts a custom disk, " +
			"and first-sighting (which is false here) would hide that capability from every caller")
	}
	if h100.InstanceDiskGB != 500 {
		t.Errorf("h100_sxm instance_disk_gb = %d, want 500 (first-sighting, like cpu_cores/ram_gb)", h100.InstanceDiskGB)
	}
	if h100.CPUCores != 16 || h100.RAMGB != 128 {
		t.Errorf("h100_sxm specs = %d cores / %d GB, want 16/128 — the OR must not disturb first-sighting specs",
			h100.CPUCores, h100.RAMGB)
	}

	a100, ok := rows["a100_pcie"]
	if !ok {
		t.Fatalf("a100_pcie missing from %+v", page.Data)
	}
	if a100.DiskConfigurable {
		t.Error("a100_pcie disk_configurable = true, want false: no offering of this model accepts a custom disk, " +
			"so the OR must not leak another model's capability")
	}
}
