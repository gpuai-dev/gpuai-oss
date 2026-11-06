package availability

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/gpuai-dev/gpuai-oss/internal/provider"
)

func testPlacementMemory() (*PlacementMemory, *time.Time) {
	m := NewPlacementMemory(DefaultPlacementBackoff, DefaultPlacementStreakWindow, silentLogger())
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	m.now = func() time.Time { return now }
	return m, &now
}

func TestPlacementMemory_HidesFromTheSecondFailure(t *testing.T) {
	m, now := testPlacementMemory()

	m.RecordFailure("off-a", "gone at placement")
	if m.Hidden("off-a") {
		t.Fatal("one failure must not hide an offering; most refusals are momentary")
	}
	*now = now.Add(30 * time.Second)
	m.RecordFailure("off-a", "gone at placement")
	if !m.Hidden("off-a") {
		t.Fatal("a second failure in the streak must hide the offering")
	}
	*now = now.Add(14 * time.Minute)
	if !m.Hidden("off-a") {
		t.Error("the second failure hides for 15 minutes")
	}
	*now = now.Add(2 * time.Minute)
	if m.Hidden("off-a") {
		t.Error("the 15-minute hide must lapse")
	}
	m.RecordFailure("off-a", "no capacity at create")
	*now = now.Add(59 * time.Minute)
	if !m.Hidden("off-a") {
		t.Error("a third failure hides for an hour")
	}
	*now = now.Add(2 * time.Minute)
	if m.Hidden("off-a") {
		t.Error("the hour must lapse")
	}
}

func TestPlacementMemory_SuccessClearsAndStreaksExpire(t *testing.T) {
	m, now := testPlacementMemory()

	m.RecordFailure("off-a", "x")
	m.RecordFailure("off-a", "x")
	m.RecordSuccess("off-a")
	if m.Hidden("off-a") {
		t.Fatal("a successful placement must clear the offering at once")
	}
	m.RecordFailure("off-a", "x")
	if m.Hidden("off-a") {
		t.Error("after a success the next failure is the first of a new streak")
	}

	m.RecordFailure("off-b", "x")
	*now = now.Add(DefaultPlacementStreakWindow + time.Minute)
	m.RecordFailure("off-b", "x")
	if m.Hidden("off-b") {
		t.Error("a failure more than the streak window after the last one starts a new streak")
	}
	if len(m.entries) != 1 {
		t.Errorf("entries outside the streak window and not hidden must be pruned; have %d", len(m.entries))
	}
}

func TestPlacementMemory_NilAndEmpty(t *testing.T) {
	var nilMemory *PlacementMemory
	if nilMemory.Hidden("x") {
		t.Error("a nil memory hides nothing")
	}
	nilMemory.RecordFailure("x", "")
	nilMemory.RecordSuccess("x")

	m, _ := testPlacementMemory()
	m.RecordFailure("", "x")
	m.RecordFailure("", "x")
	if m.Hidden("") || len(m.entries) != 0 {
		t.Error("an empty offering ID is never recorded")
	}
	off := NewPlacementMemory(nil, time.Hour, nil)
	off.RecordFailure("x", "")
	off.RecordFailure("x", "")
	if off.Hidden("x") {
		t.Error("an empty backoff hides nothing")
	}
}

func TestPoller_PlacementMemoryHidesUnplaceableOffering(t *testing.T) {
	dead := provider.GPUOffering{Provider: "alpha", GPUType: provider.GPUTypeRTX5090, GPUCount: 1, PricePerHour: 0.30,
		Tier: provider.TierOnDemand, Region: "us-east", AvailableCount: 3}
	fine := provider.GPUOffering{Provider: "alpha", GPUType: provider.GPUTypeRTX5090, GPUCount: 1, PricePerHour: 0.45,
		Tier: provider.TierOnDemand, Region: "us-east", AvailableCount: 3, UpstreamGPUID: "other"}
	registry := provider.NewRegistry()
	registry.Register(newFakeProvider("alpha", []provider.GPUOffering{dead, fine}))
	m, _ := testPlacementMemory()
	p := &Poller{registry: registry, interval: 30 * time.Second, probeTimeout: time.Second, logger: silentLogger()}
	WithPlacementMemory(m)(p)

	m.RecordFailure(OfferingID(dead, "us-east"), "no capacity at create")
	if got, _ := p.collectOfferings(context.Background()); len(got) != 2 {
		t.Fatalf("after one failure the catalog has %d rows; want both still listed", len(got))
	}
	m.RecordFailure(OfferingID(dead, "us-east"), "no capacity at create")
	got, staff := p.collectOfferings(context.Background())
	if len(got) != 1 || got[0].OfferingID == OfferingID(dead, "us-east") {
		t.Fatalf("customer catalog %+v; want only the placeable offering", got)
	}
	if len(staff) != 2 {
		t.Errorf("staff twin has %d rows; want both", len(staff))
	}
}

// The memory never empties a GPU shape: when every row of a (model, count) is
// held back, the cheapest one stays listed; a shape with another row loses
// only the held one.
func TestPoller_PlacementMemoryKeepsLastRowOfShape(t *testing.T) {
	only := provider.GPUOffering{Provider: "alpha", GPUType: provider.GPUTypeRTX4090, GPUCount: 1, PricePerHour: 0.33,
		Tier: provider.TierOnDemand, Region: "asia-pacific", AvailableCount: 2}
	dead := provider.GPUOffering{Provider: "alpha", GPUType: provider.GPUTypeRTX5090, GPUCount: 1, PricePerHour: 0.30,
		Tier: provider.TierOnDemand, Region: "us-east", AvailableCount: 3}
	fine := provider.GPUOffering{Provider: "alpha", GPUType: provider.GPUTypeRTX5090, GPUCount: 1, PricePerHour: 0.45,
		Tier: provider.TierOnDemand, Region: "us-west", AvailableCount: 3}
	registry := provider.NewRegistry()
	registry.Register(newFakeProvider("alpha", []provider.GPUOffering{only, dead, fine}))
	m, _ := testPlacementMemory()
	p := &Poller{registry: registry, interval: 30 * time.Second, probeTimeout: time.Second, logger: silentLogger()}
	WithPlacementMemory(m)(p)
	for _, o := range []provider.GPUOffering{only, dead} {
		id := OfferingID(o, Normalize(o.Region, o.DatacenterLocation, o.Provider))
		m.RecordFailure(id, "gone at placement")
		m.RecordFailure(id, "gone at placement")
	}

	got, _ := p.collectOfferings(context.Background())
	ids := map[string]bool{}
	for _, o := range got {
		ids[o.OfferingID] = true
	}
	if !ids[OfferingID(only, Normalize(only.Region, "", "alpha"))] {
		t.Error("the only RTX 4090 row was hidden; a shape must never vanish from the catalog")
	}
	if ids[OfferingID(dead, Normalize(dead.Region, "", "alpha"))] || !ids[OfferingID(fine, Normalize(fine.Region, "", "alpha"))] {
		t.Errorf("RTX 5090: want only the placeable row listed, got %v", ids)
	}
}

// The catalog index answers what the catalog last showed for an offering, for
// the streak window after it was last listed.
func TestPlacementMemory_ListedIndex(t *testing.T) {
	m, now := testPlacementMemory()
	want := ListedOffering{Provider: "bravo", GPUType: provider.GPUTypeRTX4090, GPUCount: 1,
		Tier: provider.TierOnDemand, Region: "eu-west"}
	m.ObserveListed(map[string]ListedOffering{"off-a": want})
	if got, ok := m.Listed("off-a"); !ok || got != want {
		t.Fatalf("Listed = %+v, %v; want %+v", got, ok, want)
	}
	if _, ok := m.Listed("never-listed"); ok {
		t.Error("an ID the catalog never listed must be unknown")
	}
	*now = now.Add(DefaultPlacementStreakWindow + time.Minute)
	if _, ok := m.Listed("off-a"); ok {
		t.Error("an ID not listed within the streak window must be forgotten")
	}
	m.ObserveListed(map[string]ListedOffering{"off-b": want})
	if len(m.listed) != 1 {
		t.Errorf("stale catalog entries must be pruned; have %d", len(m.listed))
	}

	var nilMemory *PlacementMemory
	nilMemory.ObserveListed(map[string]ListedOffering{"x": want})
	if _, ok := nilMemory.Listed("x"); ok {
		t.Error("a nil memory knows nothing")
	}
}

// The failure map is bounded: a burst of distinct failing offerings evicts the
// oldest instead of growing the map without limit.
func TestPlacementMemory_BoundedEntries(t *testing.T) {
	m, now := testPlacementMemory()
	for i := 0; i < maxPlacementEntries+50; i++ {
		*now = now.Add(time.Millisecond)
		m.RecordFailure(fmt.Sprintf("off-%d", i), "x")
	}
	if len(m.entries) > maxPlacementEntries {
		t.Fatalf("entries = %d; want at most %d", len(m.entries), maxPlacementEntries)
	}
	if _, ok := m.entries["off-0"]; ok {
		t.Error("the oldest entry must be the one evicted")
	}
	newest := fmt.Sprintf("off-%d", maxPlacementEntries+49)
	m.RecordFailure(newest, "x")
	if !m.Hidden(newest) {
		t.Error("eviction must not cost the streak of a recent entry")
	}
}

// The poller indexes every row it lists to customers, held-back rows included,
// with the provider and the shape a launch must ask for.
func TestPoller_PlacementMemoryIndexesListedRows(t *testing.T) {
	dead := provider.GPUOffering{Provider: "alpha", GPUType: provider.GPUTypeRTX5090, GPUCount: 1, PricePerHour: 0.30,
		Tier: provider.TierOnDemand, Region: "us-east", AvailableCount: 3}
	fine := provider.GPUOffering{Provider: "alpha", GPUType: provider.GPUTypeRTX5090, GPUCount: 2, PricePerHour: 0.90,
		Tier: provider.TierOnDemand, Region: "us-west", AvailableCount: 3}
	registry := provider.NewRegistry()
	registry.Register(newFakeProvider("alpha", []provider.GPUOffering{dead, fine}))
	m, _ := testPlacementMemory()
	p := &Poller{registry: registry, interval: 30 * time.Second, probeTimeout: time.Second, logger: silentLogger()}
	WithPlacementMemory(m)(p)
	deadID := OfferingID(dead, Normalize(dead.Region, "", "alpha"))
	m.RecordFailure(deadID, "x")
	m.RecordFailure(deadID, "x")

	p.collectOfferings(context.Background())
	for _, o := range []provider.GPUOffering{dead, fine} {
		region := Normalize(o.Region, "", "alpha")
		want := ListedOffering{Provider: "alpha", GPUType: o.GPUType, GPUCount: o.GPUCount, Tier: o.Tier, Region: region}
		if got, ok := m.Listed(OfferingID(o, region)); !ok || got != want {
			t.Errorf("Listed(%s) = %+v, %v; want %+v", region, got, ok, want)
		}
	}
}

// The never-empty-a-shape guard holds through last-known-good retention: the
// snapshot keeps the held rows, so a GPU shape whose only row is held stays
// listed while the provider's poll fails, and a hold that lapses during the
// outage lists its row again. Proven before the fix: the RTX 4090 x1 shape
// vanished on the retained cycle.
func TestPoller_RetentionKeepsLastRowOfShape(t *testing.T) {
	only := provider.GPUOffering{Provider: "alpha", GPUType: provider.GPUTypeRTX4090, GPUCount: 1, PricePerHour: 0.33,
		Tier: provider.TierOnDemand, Region: "us-east", AvailableCount: 2}
	dead := provider.GPUOffering{Provider: "alpha", GPUType: provider.GPUTypeRTX5090, GPUCount: 1, PricePerHour: 0.30,
		Tier: provider.TierOnDemand, Region: "us-east", AvailableCount: 3}
	fine := provider.GPUOffering{Provider: "alpha", GPUType: provider.GPUTypeRTX5090, GPUCount: 1, PricePerHour: 0.45,
		Tier: provider.TierOnDemand, Region: "us-west", AvailableCount: 3}
	fp := newFakeProvider("alpha", []provider.GPUOffering{only, dead, fine})
	registry := provider.NewRegistry()
	registry.Register(fp)
	m, now := testPlacementMemory()
	p := &Poller{registry: registry, interval: 30 * time.Second, probeTimeout: time.Second, logger: silentLogger(),
		retention: 5 * time.Minute}
	WithPlacementMemory(m)(p)
	ids := map[string]string{}
	for name, o := range map[string]provider.GPUOffering{"only": only, "dead": dead, "fine": fine} {
		ids[name] = OfferingID(o, Normalize(o.Region, "", "alpha"))
	}
	m.RecordFailure(ids["only"], "x")
	m.RecordFailure(ids["only"], "x")
	m.RecordFailure(ids["dead"], "x")
	m.RecordFailure(ids["dead"], "x")

	listed := func() map[string]bool {
		got, _ := p.collectOfferings(context.Background())
		out := map[string]bool{}
		for _, o := range got {
			out[o.OfferingID] = true
		}
		return out
	}
	if got := listed(); len(got) != 2 || !got[ids["only"]] || !got[ids["fine"]] {
		t.Fatalf("fresh poll listed %v; want the only RTX 4090 row and the placeable RTX 5090 row", got)
	}
	fp.failNext.Store(1) // retention serves the snapshot
	if got := listed(); len(got) != 2 || !got[ids["only"]] || !got[ids["fine"]] {
		t.Fatalf("retained poll listed %v; want the same two rows, the RTX 4090 shape kept", got)
	}
	*now = now.Add(16 * time.Minute) // both 15-minute holds lapse during the outage
	fp.failNext.Store(1)
	if got := listed(); len(got) != 3 {
		t.Fatalf("retained poll after the holds lapsed listed %v; want all three rows", got)
	}
}
