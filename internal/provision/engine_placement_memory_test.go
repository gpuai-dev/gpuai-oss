package provision

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gpuai-dev/gpuai-oss/internal/availability"
	"github.com/gpuai-dev/gpuai-oss/internal/manifest"
	"github.com/gpuai-dev/gpuai-oss/internal/provider"
)

func placementEngine(t *testing.T, provs ...provider.Provider) (*Engine, *availability.PlacementMemory) {
	t.Helper()
	m := availability.NewPlacementMemory(availability.DefaultPlacementBackoff, availability.DefaultPlacementStreakWindow,
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	e := newFallbackEngine(fakeWithOneKey(), provs...)
	e.SetPlacementMemory(m)
	return e, m
}

// A provider that lists capacity it cannot place: each launch's walk records
// the refusal and places on the next candidate. From the second refusal the
// dead listing is hidden, and the next launch no longer tries it first.
func TestProvision_PlacementMemoryStopsResellingPhantomStock(t *testing.T) {
	phantom := fallbackStub("phantom", 1, false, "unused")
	phantom.provisionErr = provider.ErrNoCapacity
	real := fallbackStub("real", 2, false, "up-real")
	e, m := placementEngine(t, phantom, real)
	ctx := context.Background()
	deadID := candidateOfferingID(ProviderCandidate{Prov: phantom, Offering: phantom.offerings[0]})

	for launch := 1; launch <= 2; launch++ {
		if _, err := e.Provision(ctx, onDemandRequest()); err != nil {
			t.Fatalf("launch %d: %v", launch, err)
		}
		if len(phantom.provisionReqs) != launch {
			t.Fatalf("launch %d: phantom tried %d times; want %d", launch, len(phantom.provisionReqs), launch)
		}
	}
	if !m.Hidden(deadID) {
		t.Fatal("two refusals did not hide the phantom offering")
	}
	if _, err := e.Provision(ctx, onDemandRequest()); err != nil {
		t.Fatalf("third launch: %v", err)
	}
	if len(phantom.provisionReqs) != 2 {
		t.Errorf("the third launch re-tried the hidden phantom listing (%d attempts)", len(phantom.provisionReqs))
	}
	if len(real.provisionReqs) != 3 {
		t.Errorf("real placed %d of 3 launches", len(real.provisionReqs))
	}
}

// Never narrow to nothing: when every candidate is hidden, they are all still
// tried.
func TestSelectProviderCandidates_PlacementMemoryNeverEmpties(t *testing.T) {
	only := fallbackStub("only", 1, false, "up")
	e, m := placementEngine(t, only)
	id := candidateOfferingID(ProviderCandidate{Prov: only, Offering: only.offerings[0]})
	m.RecordFailure(id, "x")
	m.RecordFailure(id, "x")
	if !m.Hidden(id) {
		t.Fatal("setup: offering not hidden")
	}
	got, err := e.SelectProviderCandidates(context.Background(), onDemandRequest())
	if err != nil || len(got) != 1 {
		t.Errorf("candidates %d err %v; want the hidden offering kept as the last resort", len(got), err)
	}
}

// listInCatalog records an offering in the placement memory's catalog index,
// as the poller does for every row it lists to customers.
func listInCatalog(m *availability.PlacementMemory, o provider.GPUOffering) string {
	region := availability.Normalize(o.Region, o.DatacenterLocation, o.Provider)
	id := availability.OfferingID(o, region)
	m.ObserveListed(map[string]availability.ListedOffering{id: {
		Provider: o.Provider, GPUType: o.GPUType, GPUCount: o.GPUCount, Tier: o.Tier, Region: region,
	}})
	return id
}

// A pinned offering the catalog listed and its provider no longer has is
// counted, and hidden from the second time. An ID the catalog never listed is
// not counted, so callers cannot fill the memory with made-up IDs.
func TestProvision_VanishedPinIsRemembered(t *testing.T) {
	stub := fallbackStub("marketco", 1, false, "up")
	e, m := placementEngine(t, stub)
	gone := stub.offerings[0]
	gone.Region = "us-east" // listed on an earlier poll, absent from the live listing
	goneID := listInCatalog(m, gone)
	for _, pin := range []string{goneID, "gone0000000a"} {
		req := onDemandRequest()
		req.SelectedOfferingID = pin
		for i := 1; i <= 2; i++ {
			if _, err := e.Provision(context.Background(), req); !errors.Is(err, ErrOfferingUnavailable) {
				t.Fatalf("pin %s: want ErrOfferingUnavailable, got %v", pin, err)
			}
			if hidden, want := m.Hidden(pin), i == 2 && pin == goneID; hidden != want {
				t.Errorf("pin %s after %d vanished pin(s): hidden=%v want %v", pin, i, hidden, want)
			}
		}
	}
	if len(stub.provisionReqs) != 0 {
		t.Errorf("%d creates; a pin that matched nothing must never reach the provider", len(stub.provisionReqs))
	}
}

// A pin the request itself excludes is not the offering's fault: a gpu_count,
// tier, region or GPU type the listed row does not have. Proven before the
// fix: two requests with gpu_count=2 hid a healthy 1x offering from every
// customer without a single create call.
func TestProvision_PinTheRequestExcludesIsNotRemembered(t *testing.T) {
	cases := map[string]func(*ProvisionRequest){
		"gpu_count": func(r *ProvisionRequest) { r.GPUCount = 2 },
		"region":    func(r *ProvisionRequest) { r.Regions = []string{"eu-west"} },
		"gpu_type":  func(r *ProvisionRequest) { r.GPUType = provider.GPUTypeH100SXM },
		"tier":      func(r *ProvisionRequest) { r.Tier = provider.TierSpot },
	}
	for name, mismatch := range cases {
		t.Run(name, func(t *testing.T) {
			healthy := fallbackStub("marketco", 1, false, "up")
			e, m := placementEngine(t, healthy)
			id := listInCatalog(m, healthy.offerings[0])
			for i := 0; i < 2; i++ {
				req := onDemandRequest()
				req.SelectedOfferingID = id
				mismatch(&req)
				if _, err := e.Provision(context.Background(), req); err == nil {
					t.Fatal("a pin the request excludes must not place")
				}
			}
			if m.Hidden(id) {
				t.Errorf("a healthy offering was hidden by requests that excluded it (%d creates)", len(healthy.provisionReqs))
			}
		})
	}
}

// A pin on a provider this launch skipped or could not list says nothing about
// the offering: a listing error (expired key, rate limit, outage), a disabled
// provider, a provider gate the request fails (certified on a provider that
// cannot run the certified image), or the launch-outcome breaker. Proven
// before the fix: two pins during a 401 left the offering hidden for 15
// minutes after the outage ended.
func TestProvision_PinOnSkippedProviderIsNotRemembered(t *testing.T) {
	cases := map[string]struct {
		provider string
		setup    func(*Engine, *stubProvider, *ProvisionRequest)
	}{
		"listing error": {"marketco", func(_ *Engine, s *stubProvider, _ *ProvisionRequest) {
			s.listErr = errors.New("HTTP 401: invalid api key")
		}},
		"disabled provider": {"marketco", func(e *Engine, _ *stubProvider, _ *ProvisionRequest) {
			e.config.DisabledProviders = map[string]bool{"marketco": true}
		}},
		"provider gate": {"fixedimageco", func(_ *Engine, _ *stubProvider, r *ProvisionRequest) {
			r.Environment = "certified"
		}},
		"launch breaker": {"marketco", func(e *Engine, _ *stubProvider, _ *ProvisionRequest) {
			e.launchBreaker = availability.NewBreaker(3, 10*time.Minute, e.logger)
			tripLaunchKey(e.launchBreaker, "marketco", provider.GPUTypeA10080GB, 1, 3)
		}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			stub := fallbackStub(tc.provider, 1, false, "up")
			e, m := placementEngine(t, stub)
			id := listInCatalog(m, stub.offerings[0])
			req := onDemandRequest()
			req.SelectedOfferingID = id
			tc.setup(e, stub, &req)
			for i := 0; i < 2; i++ {
				if _, err := e.Provision(context.Background(), req); err == nil {
					t.Fatal("setup: the pinned launch must fail")
				}
			}
			stub.listErr = nil
			if m.Hidden(id) {
				t.Error("the offering was hidden for a failure that was not its own")
			}
		})
	}
}

// A capacity refusal on a request that narrows placement beyond the offering
// (explicit disk_gb, a template's CUDA floor, a custom image, a raw_vm OS) can
// mean only that no host fits that request, so it must not hide the offering
// from everyone else. The plain-request control is
// TestProvision_PlacementMemoryStopsResellingPhantomStock.
func TestProvision_NarrowedRequestRefusalIsNotRemembered(t *testing.T) {
	disk := 500
	cases := map[string]func(*Engine, *ProvisionRequest){
		"explicit disk_gb": func(_ *Engine, r *ProvisionRequest) { r.RequestedDiskGB = &disk },
		"template floors":  func(_ *Engine, r *ProvisionRequest) { r.MinCUDA = 12.8; r.ContainerDiskGB = 400 },
		"custom image":     func(_ *Engine, r *ProvisionRequest) { r.Image = "ghcr.io/acme/trainer:1" },
		"raw_vm OS": func(e *Engine, r *ProvisionRequest) {
			e.manifest = rawVMManifest(t, "marketco", "podco")
			r.Environment = "raw_vm:ubuntu-24.04"
		},
	}
	for name, narrow := range cases {
		t.Run(name, func(t *testing.T) {
			phantom := fallbackStub("marketco", 1, false, "unused")
			phantom.provisionErr = provider.ErrNoCapacity
			real := fallbackStub("podco", 2, false, "up-real")
			e, m := placementEngine(t, phantom, real)
			deadID := candidateOfferingID(ProviderCandidate{Prov: phantom, Offering: phantom.offerings[0]})
			for i := 0; i < 2; i++ {
				req := onDemandRequest()
				narrow(e, &req)
				if _, err := e.Provision(context.Background(), req); err != nil {
					t.Fatalf("launch %d: %v", i+1, err)
				}
			}
			if len(phantom.provisionReqs) != 2 {
				t.Fatalf("setup: the refusing offering was tried %d times; want 2", len(phantom.provisionReqs))
			}
			if m.Hidden(deadID) {
				t.Error("a refusal for a request-specific filter hid the offering from everyone")
			}
		})
	}
}

// rawVMManifest is a manifest whose vm_images maps ubuntu-24.04 to a native
// image on each of the given providers, so a raw_vm:ubuntu-24.04 launch may
// place on them.
func rawVMManifest(t *testing.T, providers ...string) *manifest.Manifest {
	t.Helper()
	slugs := make([]string, len(providers))
	for i, p := range providers {
		slugs[i] = fmt.Sprintf("%q: \"ubuntu-24.04-%s\"", p, p)
	}
	m, err := manifest.Load([]byte(`{
		"schema_version": 1,
		"gpus": {},
		"vm_images": {"ubuntu-24.04": {"label": "Ubuntu 24.04 LTS", "family": "ubuntu", "version": "24.04",
			"providers": {` + strings.Join(slugs, ", ") + `}}}
	}`))
	if err != nil {
		t.Fatalf("raw_vm manifest: %v", err)
	}
	return m
}

// A request for fewer GPUs than the offering has is asked of the adapter at
// the requested count (marketco searches exactly that many GPUs in the offering's
// datacenter), so a capacity refusal says nothing about the larger offering.
// Proven before the fix: two ordinary 1-GPU launches hid a healthy 2x offering
// from every customer.
func TestProvision_SmallerRequestRefusalIsNotRemembered(t *testing.T) {
	twoX := fallbackStub("marketco", 1, false, "unused")
	twoX.offerings[0].GPUCount = 2
	twoX.provisionErr = provider.ErrNoCapacity // no 1x host in the 2x row's datacenter
	real := fallbackStub("podco", 3, false, "up-real")
	e, m := placementEngine(t, twoX, real)
	twoXID := candidateOfferingID(ProviderCandidate{Prov: twoX, Offering: twoX.offerings[0]})

	for i := 0; i < 2; i++ {
		if _, err := e.Provision(context.Background(), onDemandRequest()); err != nil {
			t.Fatalf("launch %d: %v", i+1, err)
		}
	}
	if len(twoX.provisionReqs) != 2 || twoX.provisionReqs[0].GPUCount != 1 {
		t.Fatalf("setup: the 2x offering was tried %d times; want 2 asks for 1 GPU", len(twoX.provisionReqs))
	}
	if m.Hidden(twoXID) {
		t.Error("refusals of a 1-GPU ask hid the 2x offering from everyone")
	}
}

// Hidden candidates are demoted, never dropped: when every fresh candidate
// refuses, the launch still tries the hidden one, which may have recovered.
// Proven before the fix: the launch failed where main would have placed.
func TestProvision_HiddenCandidateIsDemotedNotDropped(t *testing.T) {
	cheap := fallbackStub("marketco", 1, false, "up-cheap") // held back, healthy again
	pricey := fallbackStub("podco", 2, false, "unused")
	pricey.provisionErr = provider.ErrNoCapacity
	e, m := placementEngine(t, cheap, pricey)
	id := candidateOfferingID(ProviderCandidate{Prov: cheap, Offering: cheap.offerings[0]})
	m.RecordFailure(id, "x")
	m.RecordFailure(id, "x")

	got, err := e.SelectProviderCandidates(context.Background(), onDemandRequest())
	if err != nil || len(got) != 2 || got[0].Prov.Name() != "podco" || got[1].Prov.Name() != "marketco" {
		t.Fatalf("candidates %v err %v; want the fresh offering first and the hidden one after it", candidateNames(got), err)
	}
	if _, err := e.Provision(context.Background(), onDemandRequest()); err != nil {
		t.Fatalf("the launch must fall back to the hidden offering: %v", err)
	}
	if len(pricey.provisionReqs) != 1 || len(cheap.provisionReqs) != 1 {
		t.Errorf("creates: fresh %d hidden %d; want the fresh one tried first, then the hidden one",
			len(pricey.provisionReqs), len(cheap.provisionReqs))
	}
	if m.Hidden(id) {
		t.Error("the placement on the hidden offering must clear it")
	}
}

// A cost-ceiling refusal on a fresh candidate goes on to a cheaper hidden one
// demoted behind it, skipping the pricier fresh one in between. Proven before
// the fix: the refusal ended the walk on a re-quote, so demotion turned a
// launch main placed on the cheap offering into a "price changed" error.
func TestProvision_CostCeilingRefusalStillTriesCheaperHiddenCandidate(t *testing.T) {
	cheap := fallbackStub("marketco", 1, false, "up-cheap") // held back, healthy again
	refuser := fallbackStub("podco", 2, false, "unused")
	refuser.provisionErr = &provider.CostCeilingError{CheapestCostPerHour: 3, CeilingCostPerHour: 2.1}
	pricier := fallbackStub("charlie", 3, false, "up-pricier")
	e, m := placementEngine(t, cheap, refuser, pricier)
	id := candidateOfferingID(ProviderCandidate{Prov: cheap, Offering: cheap.offerings[0]})
	m.RecordFailure(id, "x")
	m.RecordFailure(id, "x")

	got, err := e.SelectProviderCandidates(context.Background(), onDemandRequest())
	if err != nil || strings.Join(candidateNames(got), ",") != "podco,charlie,marketco" {
		t.Fatalf("candidates %v err %v; want the hidden offering demoted last", candidateNames(got), err)
	}
	if _, err := e.Provision(context.Background(), onDemandRequest()); err != nil {
		t.Fatalf("the launch must place on the cheaper hidden offering: %v", err)
	}
	if len(refuser.provisionReqs) != 1 || len(pricier.provisionReqs) != 0 || len(cheap.provisionReqs) != 1 {
		t.Errorf("creates: refuser %d pricier %d hidden %d; want 1, 0, 1",
			len(refuser.provisionReqs), len(pricier.provisionReqs), len(cheap.provisionReqs))
	}
}

// Demotion never moves a hidden candidate behind one the customer's price cap
// rejects: the walk stops at the first candidate over the cap, so the hidden
// one would never be tried and the launch would fail as "price changed" where
// main places it.
func TestSelectProviderCandidates_DemotionStaysWithinPriceCap(t *testing.T) {
	cheap := fallbackStub("marketco", 1, false, "up-cheap")
	pricey := fallbackStub("podco", 2, false, "up-pricey")
	e, m := placementEngine(t, cheap, pricey)
	id := candidateOfferingID(ProviderCandidate{Prov: cheap, Offering: cheap.offerings[0]})
	m.RecordFailure(id, "x")
	m.RecordFailure(id, "x")
	req := onDemandRequest()
	capPrice := (e.retailWithDisk(context.Background(), 1, "marketco", e.effectiveDiskGB(req)) +
		e.retailWithDisk(context.Background(), 2, "podco", e.effectiveDiskGB(req))) / 2
	req.MaxPricePerHour = &capPrice

	got, err := e.SelectProviderCandidates(context.Background(), req)
	if err != nil || len(got) != 2 || got[0].Prov.Name() != "marketco" {
		t.Fatalf("candidates %v err %v; want the hidden offering first, the only one within the cap", candidateNames(got), err)
	}
	if _, err := e.Provision(context.Background(), req); err != nil {
		t.Fatalf("the launch must place on the hidden offering the cap allows: %v", err)
	}
	if len(cheap.provisionReqs) != 1 || len(pricey.provisionReqs) != 0 {
		t.Errorf("creates: hidden %d over-cap %d", len(cheap.provisionReqs), len(pricey.provisionReqs))
	}
}

func candidateNames(cs []ProviderCandidate) []string {
	out := make([]string, len(cs))
	for i, c := range cs {
		out[i] = c.Prov.Name()
	}
	return out
}

// With placement memory off the engine computes no offering IDs at all: the
// ID runs the region through Normalize, which logs a warning for every
// unmapped region, and the walk used to do that on every placement for a
// memory that was not there.
func TestProvision_PlacementMemoryOffComputesNoOfferingIDs(t *testing.T) {
	logs := &lockedBuffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(logs, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	stub := fallbackStub("marketco", 1, false, "up")
	stub.offerings[0].Region = "atlantis-9" // no canonical mapping
	e := newFallbackEngine(fakeWithOneKey(), stub)
	// The engine's own logs (the background status poll keeps writing after
	// Provision returns) stay out of the capture; Normalize logs through the
	// default logger.
	e.logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	if _, err := e.Provision(context.Background(), onDemandRequest()); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(logs.String(), "atlantis-9") {
		t.Errorf("placement memory off, yet the region was normalized:\n%s", logs.String())
	}
}

// A successful placement clears the offering's streak.
func TestProvision_PlacementClearsTheStreak(t *testing.T) {
	stub := fallbackStub("marketco", 1, false, "up")
	e, m := placementEngine(t, stub)
	id := candidateOfferingID(ProviderCandidate{Prov: stub, Offering: stub.offerings[0]})
	m.RecordFailure(id, "x")
	if _, err := e.Provision(context.Background(), onDemandRequest()); err != nil {
		t.Fatal(err)
	}
	m.RecordFailure(id, "x")
	if m.Hidden(id) {
		t.Error("the placement must have reset the streak, so this failure is the first again")
	}
}

// lockedBuffer is a log sink safe to read while other goroutines write to it.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
