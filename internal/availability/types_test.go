package availability

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"math"
	"reflect"
	"strings"
	"testing"

	"github.com/gpuai-dev/gpuai-oss/internal/manifest"
	"github.com/gpuai-dev/gpuai-oss/internal/provider"
)

// --- Phase 17 Wave 0: availability normalize + struct shape contracts ---
//
// These tests establish the contract Plan 17-03 (Wave 3) implements. Per the
// Wave 0 scaffolding pattern from Plan 15-01 (STATE.md decision [Phase 15-01]:
// "Wave 0 test scaffolding pattern: create test seams BEFORE implementation,
// intentional red tests become Wave N+1 contracts"), tests below COMPILE today
// against the existing package surface and run as RED until Wave 3 ships
// `availability.normalize(rawRegion, datacenter, providerName) string`.
//
// TestAvailableOfferingNoProviderField passes today and is a regression guard
// for D-11 (defense-by-omission — provider identity must never appear on
// AvailableOffering).

// TestNormalize_AllProviders covers the canonical region mapping for every
// supported provider source. Wave 3 of Plan 17 ships normalize() in this
// package; until then this test is RED (compile error: undefined: normalize).
func TestNormalize_AllProviders(t *testing.T) {
	tests := []struct {
		name            string
		rawRegion       string
		providerName    string
		expectCanonical string
	}{
		// Already-canonical labels self-map.
		{name: "us-central self -> us-central (terminal bucket, split out of us-east 2026-07)", rawRegion: "us-central", providerName: "generic", expectCanonical: "us-central"},
		{name: "ca-central self -> ca-central (terminal bucket, not folded into us-east)", rawRegion: "ca-central", providerName: "generic", expectCanonical: "ca-central"},

		// ISO-style country-subdivision and country codes.
		{name: "US-CA -> us-west", rawRegion: "US-CA", providerName: "generic", expectCanonical: "us-west"},
		{name: "EU-RO -> eu-east", rawRegion: "EU-RO", providerName: "generic", expectCanonical: "eu-east"},
		{name: "US-TX -> us-central", rawRegion: "US-TX", providerName: "generic", expectCanonical: "us-central"},
		{name: "CA (Canada) -> ca-central", rawRegion: "CA", providerName: "generic", expectCanonical: "ca-central"},
		{name: "JP -> asia-pacific", rawRegion: "JP", providerName: "generic", expectCanonical: "asia-pacific"},
		{name: "PL -> eu-east", rawRegion: "PL", providerName: "generic", expectCanonical: "eu-east"},

		// Generic numbered datacenter labels.
		{name: "us-central-1 -> us-central", rawRegion: "us-central-1", providerName: "generic", expectCanonical: "us-central"},
		{name: "us-central-3 -> us-central", rawRegion: "us-central-3", providerName: "generic", expectCanonical: "us-central"},

		// Indian city names.
		{name: "Delhi -> india-south", rawRegion: "Delhi", providerName: "generic", expectCanonical: "india-south"},
		{name: "Mumbai -> india-west", rawRegion: "Mumbai", providerName: "generic", expectCanonical: "india-west"},

		// Vultr native region ids.
		{name: "vultr ewr -> us-east", rawRegion: "ewr", providerName: "vultr", expectCanonical: "us-east"},
		{name: "vultr ord -> us-central (Chicago)", rawRegion: "ord", providerName: "vultr", expectCanonical: "us-central"},
		{name: "vultr fra -> eu-west", rawRegion: "fra", providerName: "vultr", expectCanonical: "eu-west"},
		{name: "vultr blr -> india-south", rawRegion: "blr", providerName: "vultr", expectCanonical: "india-south"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Normalize(tt.rawRegion, "", tt.providerName)
			if got != tt.expectCanonical {
				t.Errorf("Normalize(%q, \"\", %q) = %q, want %q",
					tt.rawRegion, tt.providerName, got, tt.expectCanonical)
			}
		})
	}
}

// TestNormalize_UnmappedPassthrough verifies that an unmapped raw region
// passes through unchanged AND emits a slog.Warn with provider + raw_region
// k/v pairs (D-07: "Unmapped passthrough + warn" — the contract is that
// unknown DCs do NOT crash; they bubble up unchanged so the catalog still
// lists them, while ops gets a structured warning to extend the mapping).
//
// RED until Wave 3 ships normalize() with the slog.Warn side effect.
func TestNormalize_UnmappedPassthrough(t *testing.T) {
	var buf bytes.Buffer
	originalDefault := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	defer slog.SetDefault(originalDefault)

	got := Normalize("totally-new-dc", "", "alpha")
	if got != "totally-new-dc" {
		t.Errorf("Normalize(\"totally-new-dc\", \"\", \"alpha\") = %q, want passthrough %q",
			got, "totally-new-dc")
	}

	logged := buf.String()
	if !strings.Contains(logged, "unmapped native region") {
		t.Errorf("expected slog.Warn message containing %q, got: %s",
			"unmapped native region", logged)
	}
	if !strings.Contains(logged, "provider=alpha") {
		t.Errorf("expected provider=alpha k/v in slog.Warn output, got: %s", logged)
	}
	if !strings.Contains(logged, "raw_region=totally-new-dc") {
		t.Errorf("expected raw_region=totally-new-dc k/v in slog.Warn output, got: %s", logged)
	}
}

// TestAvailableOfferingNoProviderField is a structural regression guard for
// D-11 (defense-by-omission): the customer-facing AvailableOffering must NOT
// contain a Provider field, so provider identity can never leak via
// json.Marshal even if a future planner forgets to filter it.
//
// PASSES today. The test is checked into Wave 0 so any future change that
// adds a Provider field is blocked at the test layer immediately.
func TestAvailableOfferingNoProviderField(t *testing.T) {
	typ := reflect.TypeOf(AvailableOffering{})
	for i := 0; i < typ.NumField(); i++ {
		if typ.Field(i).Name == "Provider" {
			t.Fatal("AvailableOffering must NOT contain Provider field (D-11 defense by omission)")
		}
	}
}

// TestToAvailableOffering_CommunityStamp verifies the Community badge is
// stamped at cache-write time from the provider name via the exact InstantBoot
// mechanism (provider.CommunityTier). An offering from "community" carries
// Community=true; any datacenter provider carries Community=false — a
// name-derived attribute CLASS, never provider identity.
func TestToAvailableOffering_CommunityStamp(t *testing.T) {
	community := ToAvailableOffering(provider.GPUOffering{
		Provider:     "community",
		GPUType:      "rtx_4090",
		GPUCount:     1,
		PricePerHour: 0.20,
		Region:       "us-west",
		Tier:         provider.TierOnDemand,
	}, DefaultMarkupPct, DefaultMinRetailMarkupPct, 0, 0, 0)
	if !community.Community {
		t.Errorf("community offering: Community = false, want true")
	}

	datacenter := ToAvailableOffering(provider.GPUOffering{
		Provider:     "generic",
		GPUType:      "h100_sxm",
		GPUCount:     1,
		PricePerHour: 2.50,
		Region:       "us-west",
		Tier:         provider.TierOnDemand,
	}, DefaultMarkupPct, DefaultMinRetailMarkupPct, 0, 0, 0)
	if datacenter.Community {
		t.Errorf("datacenter offering: Community = true, want false")
	}

	// The customer-facing JSON must carry the `community` key and still expose
	// NO provider identity (defense-by-omission stays intact).
	b, err := json.Marshal(community)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(b), `"community":true`) {
		t.Errorf("marshaled offering missing \"community\":true, got: %s", string(b))
	}
	if strings.Contains(strings.ToLower(string(b)), `"provider"`) {
		t.Errorf("marshaled offering leaked a provider key: %s", string(b))
	}
}

// TestStampCertified — Phase 63 Wave 0 (Plan 01, RED until the stamp is
// written). ToAvailableOffering must, at cache-write time, stamp the three new
// name-derived CLASS fields (CertifiedImage, DeploymentType, OfferingID)
// alongside the existing InstantBoot/Community stamps — the same
// attribute-survives-the-strip pattern. RED because ToAvailableOffering does not
// yet compute them (they read zero-value).
func TestStampCertified(t *testing.T) {
	// The VM fixture is a VM provider that can run the Certified Image.
	vm := ToAvailableOffering(provider.GPUOffering{
		Provider:     fixtureVM,
		GPUType:      "h100_sxm",
		GPUCount:     1,
		PricePerHour: 2.50,
		Region:       "us-west",
		Tier:         provider.TierOnDemand,
	}, DefaultMarkupPct, DefaultMinRetailMarkupPct, 0, 0, 0)
	if !vm.CertifiedImage {
		t.Errorf("vm provider: CertifiedImage = false, want true (provider.CertifiedImageCapable)")
	}
	if vm.DeploymentType != "vm" {
		t.Errorf("vm provider: DeploymentType = %q, want \"vm\"", vm.DeploymentType)
	}
	if vm.OfferingID == "" {
		t.Errorf("vm provider: OfferingID is empty, want a deterministic non-empty id")
	}

	// An unregistered fixture is a container-native provider.
	container := ToAvailableOffering(provider.GPUOffering{
		Provider:     "alpha",
		GPUType:      "rtx_4090",
		GPUCount:     1,
		PricePerHour: 0.50,
		Region:       "us-east",
		Tier:         provider.TierOnDemand,
	}, DefaultMarkupPct, DefaultMinRetailMarkupPct, 0, 0, 0)
	if container.DeploymentType != "container" {
		t.Errorf("container provider: DeploymentType = %q, want \"container\"", container.DeploymentType)
	}

	// The new fields must not leak provider identity in the customer-facing JSON.
	b, err := json.Marshal(vm)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(b), `"certified_image":true`) {
		t.Errorf("marshaled offering missing \"certified_image\":true, got: %s", string(b))
	}
	if strings.Contains(strings.ToLower(string(b)), `"provider"`) {
		t.Errorf("marshaled offering leaked a provider key: %s", string(b))
	}
}

// TestStampPartnerFeatured — ToAvailableOffering must stamp the name-derived
// PartnerFeatured CLASS at cache-write time (marketplace featured-row
// eligibility): true for providers registered with Traits.FeaturedPartner,
// false for everyone else (including the built-in generic and vultr), and the JSON must carry the boolean without any provider key.
func TestStampPartnerFeatured(t *testing.T) {
	{
		name := fixtureFeatured
		o := ToAvailableOffering(provider.GPUOffering{
			Provider:     name,
			GPUType:      "h100_sxm",
			GPUCount:     2,
			PricePerHour: 5.00,
			Region:       "us-east",
			Tier:         provider.TierOnDemand,
		}, DefaultMarkupPct, DefaultMinRetailMarkupPct, 0, 0, 0)
		if !o.PartnerFeatured {
			t.Errorf("%s: PartnerFeatured = false, want true (provider.FeaturedPartner)", name)
		}
	}

	for _, name := range []string{"generic", "vultr", "alpha"} {
		o := ToAvailableOffering(provider.GPUOffering{
			Provider:     name,
			GPUType:      "rtx_4090",
			GPUCount:     1,
			PricePerHour: 0.50,
			Region:       "us-east",
			Tier:         provider.TierOnDemand,
		}, DefaultMarkupPct, DefaultMinRetailMarkupPct, 0, 0, 0)
		if o.PartnerFeatured {
			t.Errorf("%s: PartnerFeatured = true, want false (opt-in trait)", name)
		}
	}

	other := ToAvailableOffering(provider.GPUOffering{
		Provider:     "alpha",
		GPUType:      "rtx_4090",
		GPUCount:     1,
		PricePerHour: 0.50,
		Region:       "us-east",
		Tier:         provider.TierOnDemand,
	}, DefaultMarkupPct, DefaultMinRetailMarkupPct, 0, 0, 0)

	b, err := json.Marshal(other)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(b), `"partner_featured":false`) {
		t.Errorf("marshaled offering missing \"partner_featured\":false, got: %s", string(b))
	}
	if strings.Contains(strings.ToLower(string(b)), `"provider"`) {
		t.Errorf("marshaled offering leaked a provider key: %s", string(b))
	}
}

// TestToAvailableOffering_VMCapablePassthrough — the per-offering VMCapable
// stamp (a marketplace's VM-enabled hosts) must survive cache-write verbatim, WITHOUT
// touching DeploymentType: a container-native marketplace row that is
// VM-capable upstream still hands off launches as a container until a VM
// launch path exists.
func TestToAvailableOffering_VMCapablePassthrough(t *testing.T) {
	capable := ToAvailableOffering(provider.GPUOffering{
		Provider:     "alpha",
		GPUType:      "rtx_4090",
		GPUCount:     1,
		PricePerHour: 0.50,
		Region:       "us-east",
		Tier:         provider.TierOnDemand,
		VMCapable:    true,
	}, DefaultMarkupPct, DefaultMinRetailMarkupPct, 0, 0, 0)
	if !capable.VMCapable {
		t.Error("VMCapable = false, want the adapter's per-offering stamp to pass through")
	}
	if capable.DeploymentType != "container" {
		t.Errorf("DeploymentType = %q, want \"container\" — VMCapable must not flip the launch class", capable.DeploymentType)
	}

	plain := ToAvailableOffering(provider.GPUOffering{
		Provider:     "alpha",
		GPUType:      "rtx_4090",
		GPUCount:     1,
		PricePerHour: 0.50,
		Region:       "us-east",
		Tier:         provider.TierOnDemand,
	}, DefaultMarkupPct, DefaultMinRetailMarkupPct, 0, 0, 0)
	if plain.VMCapable {
		t.Error("unstamped offering must stay VMCapable = false")
	}
}

// TestToAvailableOffering_CommunityNoFabricatedClaims — WR-03 (Phase 58
// review), generalized by the configurable-disk work: an unreported spec is an
// honest unknown, never a claim. For a community offering instant_boot must be
// false, the provisioning estimate must be the conservative community
// placeholder, and the unverifiable storage/uptime fills must stay 0. A
// datacenter offering keeps the uptime constant — but NOT a storage fill: the
// old 40 GB fabrication (the number behind two customer reports of
// 1250 GB machines listed as 40 GB) is deleted, so an upstream that reported
// no disk now publishes storage_gb 0 on every tier, and the launch-relevant
// number is instance_disk_gb.
func TestToAvailableOffering_CommunityNoFabricatedClaims(t *testing.T) {
	comm := ToAvailableOffering(provider.GPUOffering{
		Provider:     "community",
		GPUType:      "rtx_4090",
		GPUCount:     1,
		PricePerHour: 0.20,
		Region:       "us-west",
		Tier:         provider.TierOnDemand,
	}, DefaultMarkupPct, DefaultMinRetailMarkupPct, 0, 0, 0)

	if comm.InstantBoot {
		t.Errorf("community offering: InstantBoot = true, want false (WR-03 truth-in-catalog)")
	}
	if comm.StorageGB != 0 {
		t.Errorf("community offering: StorageGB = %d, want 0 (no verified disk spec — no 40 GB fill)", comm.StorageGB)
	}
	if comm.AvgUptimePct != 0 {
		t.Errorf("community offering: AvgUptimePct = %v, want 0 (softer-SLA tier must not claim 99.5%%)", comm.AvgUptimePct)
	}
	if comm.ProvisioningEstimateMinutes <= 1 {
		t.Errorf("community offering: ProvisioningEstimateMinutes = %d, want the conservative community estimate, not the 1-min container default", comm.ProvisioningEstimateMinutes)
	}

	dc := ToAvailableOffering(provider.GPUOffering{
		Provider:     "generic",
		GPUType:      "h100_sxm",
		GPUCount:     1,
		PricePerHour: 2.50,
		Region:       "us-west",
		Tier:         provider.TierOnDemand,
	}, DefaultMarkupPct, DefaultMinRetailMarkupPct, 0, 0, 0)

	if !dc.InstantBoot {
		t.Errorf("datacenter offering: InstantBoot = false, want true (defaults untouched)")
	}
	if dc.StorageGB != 0 {
		t.Errorf("datacenter offering: StorageGB = %d, want 0 — the 40 GB fill is deleted; an unreported disk is UNKNOWN on every tier, never a fabricated number", dc.StorageGB)
	}
	if dc.AvgUptimePct != avgUptimeOnDemand {
		t.Errorf("datacenter offering: AvgUptimePct = %v, want %v (constant untouched)", dc.AvgUptimePct, avgUptimeOnDemand)
	}
}

// TestToAvailableOffering_CommunityReliabilityScore — VERIF-03: a community
// row's AvgUptimePct is the machine's EARNED reliability_score when it has one
// (o.ReliabilityScore != nil), replacing the deliberate hard-coded 0. An
// unscored machine (nil) stays 0 — the honest "unknown" the frontend renders as
// an em-dash, never a fabricated number. A MEASURED score also beats the tier
// constants on non-community providers (a marketplace adapter can report
// per-host reliability); an unscored datacenter row keeps its constant.
func TestToAvailableOffering_CommunityReliabilityScore(t *testing.T) {
	f := func(v float64) *float64 { return &v }
	cases := []struct {
		name     string
		provider string
		tier     provider.InstanceTier
		score    *float64
		want     float64
	}{
		{"community scored → real score", "community", provider.TierOnDemand, f(92.5), 92.5},
		{"community scored zero → 0", "community", provider.TierOnDemand, f(0), 0},
		{"community unscored → 0 (unknown)", "community", provider.TierOnDemand, nil, 0},
		{"datacenter measured score wins", "generic", provider.TierOnDemand, f(99.9), 99.9},
		{"datacenter unscored keeps constant", "generic", provider.TierOnDemand, nil, avgUptimeOnDemand},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ToAvailableOffering(provider.GPUOffering{
				Provider:         tc.provider,
				GPUType:          "rtx_4090",
				GPUCount:         1,
				PricePerHour:     0.20,
				Region:           "us-west",
				Tier:             tc.tier,
				ReliabilityScore: tc.score,
			}, DefaultMarkupPct, DefaultMinRetailMarkupPct, 0, 0, 0)
			if got.AvgUptimePct != tc.want {
				t.Errorf("AvgUptimePct = %v, want %v", got.AvgUptimePct, tc.want)
			}
		})
	}
}

// TestToAvailableOffering_CapacityClass — the customer-facing capacity class
// is per OFFERING: an adapter stamp wins, an unstamped offering falls back to
// the provider-name rule (community adapter → community, everything else →
// secure), and the Community bool is exactly the class's bool twin so every
// downstream dedupe/merge key stays consistent with the tabs split.
func TestToAvailableOffering_CapacityClass(t *testing.T) {
	cases := []struct {
		name          string
		providerName  string
		stamp         string
		wantClass     string
		wantCommunity bool
	}{
		{"datacenter provider unstamped → secure", "generic", "", "secure", false},
		{"community adapter unstamped → community", "community", "", "community", true},
		{"community stamp wins over provider rule", "alpha", "community", "community", true},
		{"datacenter-host stamp → secure", "bravo", "secure", "secure", false},
		{"garbage stamp falls back to provider rule", "generic", "definitely-not-a-class", "secure", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ToAvailableOffering(provider.GPUOffering{
				Provider:      tc.providerName,
				GPUType:       "rtx_4090",
				GPUCount:      1,
				PricePerHour:  0.20,
				Region:        "us-west",
				Tier:          provider.TierOnDemand,
				CapacityClass: tc.stamp,
			}, DefaultMarkupPct, DefaultMinRetailMarkupPct, 0, 0, 0)
			if got.CapacityClass != tc.wantClass {
				t.Errorf("CapacityClass = %q, want %q", got.CapacityClass, tc.wantClass)
			}
			if got.Community != tc.wantCommunity {
				t.Errorf("Community = %v, want %v (must stay the class's bool twin)", got.Community, tc.wantCommunity)
			}
		})
	}
}

// TestOfferingID_CapacityClassDistinct — the pin id hashes the RESOLVED
// capacity class, so two same-SKU/region/tier rows differing only by class
// (a marketplace's datacenter host vs an individual host) get distinct ids — a pinned
// launch can never silently resolve to the other class. Resolution also keeps
// ids stable for adapters that never stamp: an explicit "secure" stamp and no
// stamp on a datacenter provider hash identically.
func TestOfferingID_CapacityClassDistinct(t *testing.T) {
	base := provider.GPUOffering{
		Provider:     "alpha",
		GPUType:      "rtx_4090",
		GPUCount:     1,
		PricePerHour: 0.20,
		Region:       "us-east",
		Tier:         provider.TierSpot,
	}
	secure, community := base, base
	secure.CapacityClass = provider.CapacityClassSecure
	community.CapacityClass = provider.CapacityClassCommunity
	if OfferingID(secure, "us-east") == OfferingID(community, "us-east") {
		t.Error("secure and community rows of the same SKU must have distinct offering ids")
	}

	stamped := provider.GPUOffering{Provider: "generic", GPUType: "h100_sxm", GPUCount: 1, Tier: provider.TierOnDemand, CapacityClass: provider.CapacityClassSecure}
	unstamped := stamped
	unstamped.CapacityClass = ""
	if OfferingID(stamped, "us-east") != OfferingID(unstamped, "us-east") {
		t.Error("an unstamped datacenter offering must hash identically to its resolved secure class")
	}
}

// TestOfferingID_UpstreamGPUIDDistinct — GPU-55. The pin id must name a
// PRODUCT, not just a class. The regression case is the live 2026-08-14 hit:
// one provider's 4x A6000 high-performance plan (54 vCPU / 256 GB) and its plain
// 4x A6000 plan (30 / 192) are the same provider/type/region/count/tier/class at the SAME
// 188¢ price, so a class-only seed gave them ONE id. When the advertised
// variant drained, the re-polled dedup winner was the sibling, the pin still
// matched, and the engine forwarded the sibling's UpstreamGPUID — a same-price,
// lower-spec node, which is the exact silent substitution D-63-08 forbids.
//
// The same collision arises wherever an adapter's GPU-name mapping is
// many-to-one (A100 PCIe and A100-SXM4 both normalize to a100_80gb) and the colliding rows carry
// DIFFERENT prices, so a pin could resolve to a pricier machine than displayed.
func TestOfferingID_UpstreamGPUIDDistinct(t *testing.T) {
	// The price-tied pair, identical in every class attribute.
	mcBase := provider.GPUOffering{
		Provider:      "alpha",
		GPUType:       "rtx_a6000",
		GPUCount:      4,
		PricePerHour:  1.88,
		Region:        "us-central-3",
		Tier:          provider.TierOnDemand,
		CapacityClass: provider.CapacityClassSecure,
	}
	highPerf, base := mcBase, mcBase
	highPerf.UpstreamGPUID = "plan_4x_a6000_highperf"
	highPerf.CPUCores, highPerf.RAMGB = 54, 256
	base.UpstreamGPUID = "plan_4x_a6000"
	base.CPUCores, base.RAMGB = 30, 192

	if OfferingID(highPerf, "us-central") == OfferingID(base, "us-central") {
		t.Error("price-tied, spec-different sibling products must have distinct offering ids — a drained variant must not resolve to its sibling")
	}
	// Product identity is the ONLY thing that separates them: the id must not
	// have picked up the specs (a spec change on the same product would rotate
	// the pin every time an upstream re-reports a host).
	respecced := highPerf
	respecced.CPUCores, respecced.RAMGB, respecced.PricePerHour = 60, 300, 2.10
	if OfferingID(respecced, "us-central") != OfferingID(highPerf, "us-central") {
		t.Error("the same product must keep its id — only class attributes and product identity may seed it")
	}

	// Two native GPU names that normalize to one canonical type.
	rpBase := provider.GPUOffering{
		Provider:      "bravo",
		GPUType:       "a100_80gb",
		GPUCount:      1,
		Region:        "US-TX",
		Tier:          provider.TierOnDemand,
		CapacityClass: provider.CapacityClassSecure,
	}
	pcie, sxm := rpBase, rpBase
	pcie.UpstreamGPUID = "NVIDIA A100 80GB PCIe"
	sxm.UpstreamGPUID = "NVIDIA A100-SXM4-80GB"
	if OfferingID(pcie, "us-central") == OfferingID(sxm, "us-central") {
		t.Error("native GPU variants that normalize to one canonical GPU type must have distinct offering ids")
	}
}

// TestOfferingID_EmptyUpstreamGPUIDStable — adapters that never stamp
// UpstreamGPUID (generic among them) contribute "" to the seed, so their ids stay a pure function
// of the same class attributes as before: deterministic across calls, stable
// across polls, and 12 hex chars wide. The digest VALUE rotates once at deploy
// (an accepted property of any seed change), but the behavior does not.
func TestOfferingID_EmptyUpstreamGPUIDStable(t *testing.T) {
	o := provider.GPUOffering{
		Provider:           "alpha",
		GPUType:            "rtx_4090",
		GPUCount:           1,
		PricePerHour:       0.20,
		Region:             "US-CA-2",
		DatacenterLocation: "California",
		Tier:               provider.TierOnDemand,
	}
	if o.UpstreamGPUID != "" {
		t.Fatalf("fixture must model a non-stamping adapter, got UpstreamGPUID = %q", o.UpstreamGPUID)
	}
	first := OfferingID(o, "us-west")
	if len(first) != 12 {
		t.Errorf("offering id = %q, want 12 hex chars", first)
	}
	for i := 0; i < 5; i++ {
		if got := OfferingID(o, "us-west"); got != first {
			t.Fatalf("call %d returned %q, want the stable %q — the id must be deterministic across polls", i, got, first)
		}
	}
	// A copy, and a copy whose stock/price moved, are the same offering.
	churned := o
	churned.PricePerHour, churned.AvailableCount, churned.StockStatus = 0.31, 7, "low_stock"
	if got := OfferingID(churned, "us-west"); got != first {
		t.Errorf("price/stock churn rotated the id (%q != %q) — an unstamped offering must keep a stable pin", got, first)
	}
}

// TestCanonicalRegionMapValuesSelfMap — WR-04 invariant: every terminal
// canonical VALUE of canonicalRegionMap must also be a KEY that maps to
// itself. Otherwise an already-canonical input (e.g. a community supplier
// correctly declaring "india-south") passes the IsCanonicalRegion gate but
// takes Normalize's passthrough+Warn path on every poll/cache-write/provision
// evaluation — permanent log spam for a valid configuration.
func TestCanonicalRegionMapValuesSelfMap(t *testing.T) {
	for key, canonical := range canonicalRegionMap {
		mapped, ok := canonicalRegionMap[canonical]
		if !ok {
			t.Errorf("canonical value %q (reached from key %q) is not itself a key — Normalize(%q) would passthrough+Warn every poll", canonical, key, canonical)
			continue
		}
		if mapped != canonical {
			t.Errorf("canonical value %q does not self-map: canonicalRegionMap[%q] = %q", canonical, canonical, mapped)
		}
	}
}

// TestNormalize_CanonicalCommunityRegionsNoWarn — WR-04: a community supplier
// declaring any terminal canonical bucket must resolve via a direct map hit
// (identity) with ZERO "unmapped native region" warnings — the Pitfall-4
// no-Warn-spam design for the poll path.
func TestNormalize_CanonicalCommunityRegionsNoWarn(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	defer slog.SetDefault(prev)

	for canonical := range canonicalRegionSet {
		if got := Normalize(canonical, "", "community"); got != canonical {
			t.Errorf("Normalize(%q, community) = %q; want identity", canonical, got)
		}
		if !IsCanonicalRegion(canonical) {
			t.Errorf("IsCanonicalRegion(%q) = false; want true", canonical)
		}
	}
	if strings.Contains(buf.String(), "unmapped native region") {
		t.Errorf("canonical inputs took the passthrough+Warn path; log: %s", buf.String())
	}
}

// TestRetailPrice_FloorRegressionCases — the five REAL below-cost launches the
// 2026-07 prod billing audit surfaced (23 of 492 instances priced under
// upstream cost). Each case reproduces the audited bad retail through the raw
// markup formula first — locking in that the fixture models the actual prod
// failure — then proves the floored RetailPrice clamps it to
// wholesale × (1 + 5%) rounded UP to the cent, i.e. ≥ upstream×1.05 always.
//
// In prod the bad retail arose two ways that are indistinguishable at this
// layer: a zero/negative effective markup (PRICING_MARKUP_PCT_<NAME> is not
// range-validated; at 0% math.Round rounds sub-cent wholesale DOWN — the
// h100_pcie and l40 rows), and upstream list-price drift between poll and
// launch (the marked-up stale list undercut the provider's actual charge —
// the rtx_pro_6000, rtx_3080, and a100_80gb rows). Both reduce to "configured
// retail below the current wholesale basis", which is exactly what the
// negative/zero markups below reproduce against the audited upstream price.
func TestRetailPrice_FloorRegressionCases(t *testing.T) {
	const minMarkup = DefaultMinRetailMarkupPct // 5.0 — the config default
	cases := []struct {
		name      string
		wholesale float64 // upstream $/hr at launch (audit)
		markupPct float64 // effective markup that reproduced the audited retail
		badRetail float64 // what actually shipped, pre-floor (audit)
		want      float64 // floored retail: ceil-to-cent(wholesale × 1.05)
	}{
		{"h100_pcie 1.50 vs 1.5048 (0% markup, sub-cent round-down)", 1.5048, 0, 1.50, 1.59},
		{"rtx_pro_6000 2.00 vs 2.09 (-4.3% margin)", 2.09, -4.3062, 2.00, 2.20},
		{"rtx_3080 0.13 vs 0.17 (-24% margin)", 0.17, -23.5294, 0.13, 0.18},
		{"a100_80gb 1.47 vs 1.49 (-1.3% margin)", 1.49, -1.3423, 1.47, 1.57},
		{"l40 0.79 vs 0.792 (0% markup, sub-cent round-down)", 0.792, 0, 0.79, 0.84},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// The fixture must reproduce the audited regression: the raw
			// (unfloored) formula yields the exact below-cost retail prod shipped.
			if raw := markedUpRetail(tc.wholesale, tc.markupPct); raw != tc.badRetail {
				t.Fatalf("markedUpRetail(%v, %v) = %v, want the audited %v — fixture no longer reproduces the regression",
					tc.wholesale, tc.markupPct, raw, tc.badRetail)
			}
			got := RetailPrice(tc.wholesale, tc.markupPct, minMarkup)
			if got != tc.want {
				t.Errorf("RetailPrice(%v, %v, %v) = %v, want floored %v",
					tc.wholesale, tc.markupPct, minMarkup, got, tc.want)
			}
			// Structural guarantees, independent of the exact expectation:
			// ≥ upstream × 1.05 (rounded up), and therefore never below cost.
			if floor := retailFloor(tc.wholesale, minMarkup); got < floor {
				t.Errorf("RetailPrice = %v is below the %v floor (wholesale %v × 1.05, ceil-to-cent)",
					got, floor, tc.wholesale)
			}
			if got < tc.wholesale*(1+minMarkup/100) {
				t.Errorf("RetailPrice = %v < wholesale×1.05 = %v — audit invariant violated",
					got, tc.wholesale*(1+minMarkup/100))
			}
		})
	}
}

// TestRetailPrice_HealthyMarginPassthrough — offerings already priced above
// the floor pass through byte-identical: the audit's healthy same-GPU
// siblings (6% default markup over live wholesale) must keep their exact
// pre-floor retail.
func TestRetailPrice_HealthyMarginPassthrough(t *testing.T) {
	cases := []struct {
		name      string
		wholesale float64
		markupPct float64
		want      float64
	}{
		{"rtx_pro_6000 healthy sibling 2.09 at 6% -> 2.22", 2.09, 6.0, 2.22},
		{"h100_pcie healthy sibling 1.67 at 6% -> 1.77", 1.67, 6.0, 1.77},
		{"heavy per-provider override untouched (1.50 at 20% -> 1.80)", 1.50, 20.0, 1.80},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := RetailPrice(tc.wholesale, tc.markupPct, DefaultMinRetailMarkupPct)
			if got != tc.want {
				t.Errorf("RetailPrice(%v, %v, %v) = %v, want unchanged %v",
					tc.wholesale, tc.markupPct, DefaultMinRetailMarkupPct, got, tc.want)
			}
			if got != markedUpRetail(tc.wholesale, tc.markupPct) {
				t.Errorf("healthy-margin price must be the raw markup result, got %v", got)
			}
		})
	}
}

// TestRetailPrice_ExactBoundary — a markup exactly AT the floor minimum on a
// clean-cent wholesale lands exactly on the boundary and must not be taxed an
// extra cent by float noise: 2.00 × 1.05 × 100 evaluates to
// 210.00000000000003, and without the centEpsilon guard the floor's round-UP
// would produce 2.11. The correct price is exactly 2.10, from either side of
// the max().
func TestRetailPrice_ExactBoundary(t *testing.T) {
	if got := RetailPrice(2.00, 5.0, 5.0); got != 2.10 {
		t.Errorf("RetailPrice(2.00, 5%%, min 5%%) = %v, want exactly 2.10 (no float-noise extra cent)", got)
	}
	if got := retailFloor(2.00, 5.0); got != 2.10 {
		t.Errorf("retailFloor(2.00, 5%%) = %v, want exactly 2.10", got)
	}
	// One sub-cent above the boundary must still round UP.
	if got := retailFloor(2.001, 5.0); got != 2.11 {
		t.Errorf("retailFloor(2.001, 5%%) = %v, want 2.11 (2.10105 rounds up)", got)
	}
}

// TestRetailPrice_FloorRoundsUpNeverDown — the floor rounds UP to the cent in
// every case: round-to-nearest is exactly how the audit's sub-cent wholesale
// rates (1.5048, 0.792) leaked retail below cost. Also locks the two edge
// behaviors: a zero min markup still floors at bare wholesale rounded up
// (degraded config means "never below cost", not "no floor"), and a
// non-positive wholesale — the reserved tier's deliberate 0 rate — is never
// floored.
func TestRetailPrice_FloorRoundsUpNeverDown(t *testing.T) {
	// 1.5048 × 1.05 = 1.58004 -> 1.59, never the round-to-nearest 1.58.
	if got := RetailPrice(1.5048, 0, 5.0); got != 1.59 {
		t.Errorf("RetailPrice(1.5048, 0%%, min 5%%) = %v, want 1.59 (floor rounds UP)", got)
	}
	// Min markup 0: floor = wholesale rounded UP -> 0.80, never 0.79 again.
	if got := RetailPrice(0.792, 0, 0); got != 0.80 {
		t.Errorf("RetailPrice(0.792, 0%%, min 0%%) = %v, want 0.80 (never below cost even unwired)", got)
	}
	// Reserved-tier rows carry 0 and must stay 0 (flat-fee, off-platform).
	if got := RetailPrice(0, 6.0, 5.0); got != 0 {
		t.Errorf("RetailPrice(0, 6%%, min 5%%) = %v, want 0 (reserved-tier rate untouched)", got)
	}
}

// TestToAvailableOffering_PriceFloor — the cache-write layer publishes the
// FLOORED price (the customer-facing row still strips provider identity), a
// healthy-margin row is untouched, and the staff twin mirrors the same
// floored price so the two snapshots cannot drift. floorEngaged fires exactly
// when the clamp does — its per-provider-per-cycle aggregation into the ops
// warn is covered by TestCollectOfferings_FloorWarnOncePerProvider.
func TestToAvailableOffering_PriceFloor(t *testing.T) {
	floored := provider.GPUOffering{
		Provider:     "alpha",
		GPUType:      provider.GPUTypeL40,
		GPUCount:     1,
		PricePerHour: 0.792, // the audited l40 wholesale
		Region:       "us-east",
		Tier:         provider.TierOnDemand,
	}
	got := ToAvailableOffering(floored, 0, DefaultMinRetailMarkupPct, 0, 0, 0)
	if got.PricePerHour != 0.84 {
		t.Errorf("floored catalog PricePerHour = %v, want 0.84 (0.792 × 1.05 ceil-to-cent)", got.PricePerHour)
	}
	if !floorEngaged(floored.PricePerHour, 0, DefaultMinRetailMarkupPct, 0) {
		t.Errorf("floorEngaged(0.792, 0%%, min %v%%, 0) = false, want true (clamp fired)", DefaultMinRetailMarkupPct)
	}

	// Staff twin mirrors the customer price exactly — floor included.
	if so := ToStaffOffering(floored, 0, DefaultMinRetailMarkupPct, 0); so.RetailPerHour != got.PricePerHour {
		t.Errorf("staff RetailPerHour = %v, want the customer catalog's %v", so.RetailPerHour, got.PricePerHour)
	}

	// The customer-facing row must still leak no provider identity.
	b, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(strings.ToLower(string(b)), `"provider"`) {
		t.Errorf("marshaled floored offering leaked a provider key: %s", string(b))
	}

	// Healthy margin: exact pre-floor price, and no engagement.
	healthy := provider.GPUOffering{
		Provider:     "alpha",
		GPUType:      provider.GPUTypeRTXPro6000,
		GPUCount:     1,
		PricePerHour: 2.09,
		Region:       "us-east",
		Tier:         provider.TierOnDemand,
	}
	if got := ToAvailableOffering(healthy, DefaultMarkupPct, DefaultMinRetailMarkupPct, 0, 0, 0); got.PricePerHour != 2.22 {
		t.Errorf("healthy catalog PricePerHour = %v, want unchanged 2.22", got.PricePerHour)
	}
	if floorEngaged(healthy.PricePerHour, DefaultMarkupPct, DefaultMinRetailMarkupPct, 0) {
		t.Errorf("floorEngaged(2.09, %v%%, min %v%%, 0) = true, want false (healthy margin)", DefaultMarkupPct, DefaultMinRetailMarkupPct)
	}
}

// TestToAvailableOffering_OSOptions — the launch OS picker's cache-write stamp:
// VM offerings carry the manifest's os-id list for their provider, container
// offerings and unmapped VM providers carry none, and the serialized row leaks
// only OUR os ids (never a provider-native image slug — D6).
func TestToAvailableOffering_OSOptions(t *testing.T) {
	m, err := manifest.LoadEmbedded()
	if err != nil {
		t.Fatalf("load embedded manifest: %v", err)
	}

	// A VM provider the embedded manifest maps carries exactly its sorted os ids.
	// The name is read from the manifest and classified VM here, so the test
	// pins the stamp against whatever vm_images ships, not a hardcoded adapter.
	mapped := ""
	for _, img := range m.VMImages {
		for name := range img.Providers {
			if mapped == "" || name < mapped {
				mapped = name
			}
		}
	}
	if mapped != "" {
		provider.RegisterTraits(mapped, provider.Traits{SlowBoot: true, VM: true})
		vm := ToAvailableOffering(provider.GPUOffering{
			Provider:     mapped,
			GPUType:      "h200_nvl",
			GPUCount:     1,
			PricePerHour: 2.50,
			Region:       "eu-west",
			Tier:         provider.TierOnDemand,
		}, DefaultMarkupPct, DefaultMinRetailMarkupPct, 0, 0, 0)
		want := m.VMOSOptionsFor(mapped)
		if len(want) == 0 || strings.Join(vm.OSOptions, ",") != strings.Join(want, ",") {
			t.Errorf("mapped VM OSOptions = %v; want %v", vm.OSOptions, want)
		}

		// Wire shape: os ids only, no native slugs, no provider key.
		b, err := json.Marshal(vm)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		wantJSON, _ := json.Marshal(want)
		if !strings.Contains(string(b), `"os_options":`+string(wantJSON)) {
			t.Errorf("marshaled offering missing os_options ids: %s", string(b))
		}
		for _, img := range m.VMImages {
			if slug := img.Providers[mapped]; slug != "" && strings.Contains(string(b), slug) {
				t.Errorf("marshaled offering leaked provider-native image slug %q: %s", slug, string(b))
			}
		}
		if strings.Contains(strings.ToLower(string(b)), `"provider"`) {
			t.Errorf("marshaled offering leaked a provider key: %s", string(b))
		}
	}

	// Container-native rows never carry OS options — an OS pick is meaningless
	// where the provider runs our image natively.
	container := ToAvailableOffering(provider.GPUOffering{
		Provider:     "alpha",
		GPUType:      "h100_sxm",
		GPUCount:     1,
		PricePerHour: 2.50,
		Region:       "us-west",
		Tier:         provider.TierOnDemand,
	}, DefaultMarkupPct, DefaultMinRetailMarkupPct, 0, 0, 0)
	if container.OSOptions != nil {
		t.Errorf("container OSOptions = %v; want nil", container.OSOptions)
	}

	// A VM provider with no vm_images mapping gets nil, and nil serializes
	// AWAY under omitempty.
	unmapped := ToAvailableOffering(provider.GPUOffering{
		Provider:     fixtureVM,
		GPUType:      "h100_sxm",
		GPUCount:     1,
		PricePerHour: 2.50,
		Region:       "eu-west",
		Tier:         provider.TierOnDemand,
	}, DefaultMarkupPct, DefaultMinRetailMarkupPct, 0, 0, 0)
	if unmapped.DeploymentType != "vm" {
		t.Fatalf("precondition: %s DeploymentType = %q, want vm", fixtureVM, unmapped.DeploymentType)
	}
	if unmapped.OSOptions != nil {
		t.Errorf("unmapped VM OSOptions = %v; want nil", unmapped.OSOptions)
	}
	if b2, _ := json.Marshal(unmapped); strings.Contains(string(b2), "os_options") {
		t.Errorf("empty OSOptions must serialize away under omitempty: %s", string(b2))
	}
}

// TestDiskWholesalePerHour pins the disk-cost formula. The critical case is the
// first one: a zero price must return exactly 0 so every quote on the platform
// stays bit-identical while metering ships inert.
func TestDiskWholesalePerHour(t *testing.T) {
	const containerDiskRate = 0.000137 // $0.10/GB/mo ÷ 730h, a typical published container-disk rate

	tests := []struct {
		name     string
		diskGB   int
		included int
		price    float64
		want     float64
		why      string
	}{
		{"zero price meters nothing", 500, 100, 0, 0,
			"DISK_PRICE_PER_GB_HOUR=0 must make the uplift identically zero at any size"},
		{"disk equal to allowance", 100, 100, containerDiskRate, 0,
			"a default launch must cost the customer nothing new"},
		{"disk under allowance", 40, 100, containerDiskRate, 0,
			"never credit for unused allowance"},
		{"100GB over allowance", 200, 100, containerDiskRate, 100 * containerDiskRate, ""},
		{"1GB over allowance", 101, 100, containerDiskRate, containerDiskRate,
			"the small-overage case: must bill 1 GB, not round up to a cent"},
		{"negative price is ignored", 200, 100, -0.5, 0,
			"config rejects this at boot; defense in depth against a credit"},
		{"zero allowance meters everything", 100, 0, containerDiskRate, 100 * containerDiskRate, ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := DiskWholesalePerHour(tt.diskGB, tt.included, tt.price)
			if math.Abs(got-tt.want) > 1e-12 {
				t.Errorf("DiskWholesalePerHour(%d, %d, %v) = %v, want %v — %s",
					tt.diskGB, tt.included, tt.price, got, tt.want, tt.why)
			}
		})
	}
}

// TestDiskWholesalePerHour_FoldsIntoOneRounding is the reason this returns a
// WHOLESALE addend rather than a retail add-on. RetailPrice's floor rounds UP
// to the next whole cent, so marking up and rounding the disk term separately
// would charge a customer 1 GB over the allowance a full cent per hour — about
// $7.30/month for disk that costs us about $0.10/month. Folding it into the
// wholesale basis bills the actual overage.
func TestDiskWholesalePerHour_FoldsIntoOneRounding(t *testing.T) {
	const (
		gpuWholesale = 2.50
		rate         = 0.000137
		markup       = 20.0
		minMarkup    = 5.0
	)

	// 1 GB over the allowance: folded into one basis, the quote must not jump a
	// full cent above the plain GPU-only price.
	folded := RetailPrice(gpuWholesale+DiskWholesalePerHour(101, 100, rate), markup, minMarkup)
	gpuOnly := RetailPrice(gpuWholesale, markup, minMarkup)
	if folded-gpuOnly > 0.005 {
		t.Errorf("1 GB over allowance moved the quote from %v to %v; separate rounding of a "+
			"sub-cent disk term is exactly what folding into the wholesale basis avoids", gpuOnly, folded)
	}

	// The separate-rounding shape, for contrast — this is what we are NOT doing.
	separate := gpuOnly + RetailPrice(DiskWholesalePerHour(101, 100, rate), markup, minMarkup)
	if separate <= folded {
		t.Fatalf("test premise broken: separate rounding (%v) should exceed folded (%v)", separate, folded)
	}
}

// TestDiskWholesalePerHour_ZeroPriceIsBitIdentical proves the ship-inert
// property at the level customers actually feel it: with the shipped config,
// the disk-inclusive quote equals the GPU-only quote exactly, at any disk size.
func TestDiskWholesalePerHour_ZeroPriceIsBitIdentical(t *testing.T) {
	for _, wholesale := range []float64{0.0, 0.35, 2.50, 2.799, 12.0} {
		for _, disk := range []int{0, 40, 100, 250, 1000} {
			gpuOnly := RetailPrice(wholesale, 20.0, 5.0)
			withDisk := RetailPrice(wholesale+DiskWholesalePerHour(disk, 100, 0), 20.0, 5.0)
			if gpuOnly != withDisk {
				t.Errorf("wholesale=%v disk=%d: GPU-only %v != disk-inclusive %v with price 0",
					wholesale, disk, gpuOnly, withDisk)
			}
		}
	}
}

// StockStatusForCount thresholds (32/10/1) match the availability dashboard's
// fleet-wide derivation — pin the boundaries so the two can't drift.
func TestStockStatusForCount(t *testing.T) {
	cases := map[int]string{
		0:  "",
		1:  "Low",
		9:  "Low",
		10: "Medium",
		31: "Medium",
		32: "High",
		99: "High",
	}
	for count, want := range cases {
		if got := StockStatusForCount(count); got != want {
			t.Errorf("StockStatusForCount(%d) = %q, want %q", count, got, want)
		}
	}
}

// A partner cost discount must reach the customer instead of being clamped
// back up to the provider's LIST price by the retail floor.
//
// Regression for the pricing shape prod shipped until 2026-08-12: a partner
// invoices 10% under the list price its API reports (a provider cost discount
// of 10) and is configured to pass that straight through (a provider markup of
// -10), but the floor was
// computed against LIST, so it outranked the discounted price on every
// surface. Real prod numbers: list $1.188, COGS $1.0692, customers billed
// $1.25 — a ~16.9% upcharge where the config asked for passthrough.
func TestRetailPrice_PartnerDiscountReachesCustomer(t *testing.T) {
	const list = 1.188 // the partner's a100_80gb list; we are invoiced 0.9 x this

	// The shipped defect: a list-basis floor at 5% republished at 1.05 x list.
	if got := retailFloor(list, 5); math.Abs(got-1.25) > 0.0001 {
		t.Fatalf("precondition: list-basis floor = %v, want 1.25 (the price prod billed)", got)
	}

	// Pricing off true cost passes the partner discount through with markup 0 —
	// the ONE value that now expresses "pass the partner's deal to the customer".
	// $1.188 list, 10% partner discount => $1.0692, published at $1.07.
	if got := RetailPrice(TrueCost(list, 10), 0, 0); math.Abs(got-1.07) > 0.0001 {
		t.Errorf("RetailPrice(TrueCost(list,10), markup=0, floor=0) = %v, want 1.07", got)
	}

	// Even with the 5% floor still configured, the floor is taken over TRUE
	// cost (ceil(0.9 x list x 1.05) = 1.13), never over list (which gave 1.25).
	if got := RetailPrice(TrueCost(list, 10), 0, 5); math.Abs(got-1.13) > 0.0001 {
		t.Errorf("RetailPrice(TrueCost(list,10), markup=0, floor=5) = %v, want 1.13", got)
	}

	// PRICE-NEUTRALITY OF THE v85 SWITCH. The old shape expressed this same
	// outcome as markup=-10 applied to LIST with the floor rebased onto true
	// cost. Both land on the same published cent, which is what makes deleting
	// the -10 provider markup a no-op for customers rather than a
	// repricing.
	oldShape := math.Ceil(TrueCost(list, 10)*100-1e-9) / 100 // old floor at true cost
	if newShape := RetailPrice(TrueCost(list, 10), 0, 0); math.Abs(newShape-oldShape) > 0.0001 {
		t.Errorf("v85 switch moved the price: old %v, new %v", oldShape, newShape)
	}
}

// The floor must still hold — rebasing it must never sanction selling below
// what we actually pay. This is the 2026-07 below-cost audit's guarantee.
func TestRetailPrice_DiscountedFloorStillNeverBelowCost(t *testing.T) {
	const list = 2.00 // true cost 1.80 at a 10% partner discount
	// A markup far below cost is still clamped up to true cost, not to list.
	got := RetailPrice(TrueCost(list, 10), -50, 0)
	if got < 1.80 {
		t.Errorf("RetailPrice(TrueCost(%v,10), -50, 0) = %v, below true cost 1.80", list, got)
	}
	if got > 1.80+0.0001 {
		t.Errorf("RetailPrice(TrueCost(%v,10), -50, 0) = %v, want exactly true cost 1.80", list, got)
	}
}

// No partner discount must be byte-identical to the pre-change behavior, so
// this cannot move prices for any provider without a discount configured.
func TestRetailPrice_NoDiscountUnchanged(t *testing.T) {
	for _, wholesale := range []float64{0, 0.075, 0.792, 1.69, 1.89, 48.0} {
		for _, markup := range []float64{-10, 0, 5, 6, 15} {
			for _, floor := range []float64{0, 5} {
				want := markedUpRetail(wholesale, markup)
				if wholesale > 0 {
					if f := retailFloor(wholesale, floor); f > want {
						want = f
					}
				}
				if got := RetailPrice(TrueCost(wholesale, 0), markup, floor); got != want {
					t.Errorf("RetailPrice(%v, %v, %v, no-discount) = %v, want %v", wholesale, markup, floor, got, want)
				}
			}
		}
	}
}

// An out-of-range discount must degrade to the un-discounted list basis
// (floors higher, never below cost) rather than being trusted.
func TestTrueCost_RejectsOutOfRangeDiscounts(t *testing.T) {
	for _, bad := range []float64{-1, 100, 150} {
		if got := TrueCost(2.00, bad); got != 2.00 {
			t.Errorf("TrueCost(2.00, %v) = %v, want 2.00 (ignored)", bad, got)
		}
	}
	if got := TrueCost(2.00, 10); math.Abs(got-1.80) > 0.0001 {
		t.Errorf("TrueCost(2.00, 10) = %v, want 1.80", got)
	}
}

// TestToAvailableOffering_VRAMNormalized — the snapshot must carry nameplate
// VRAM for a near-canonical upstream report (marketplace adapters pass
// through nvidia-smi MiB→GiB totals that land just under the spec sheet), while a
// materially smaller variant sharing one upstream name stays as reported.
// Both twins (customer + staff) must agree.
func TestToAvailableOffering_VRAMNormalized(t *testing.T) {
	cases := []struct {
		name     string
		gpuType  provider.GPUType
		reported int
		want     int
	}{
		{"measured b200 lifts to nameplate", "b200", 179, 192},
		{"measured l4 lifts to nameplate", "l4", 22, 24},
		{"unreported falls back to nameplate", "l40s", 0, 48},
		{"larger variant stays as reported", "v100", 32, 32},
		{"materially smaller stays as reported", "l40s", 24, 24},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			in := provider.GPUOffering{
				Provider:     "alpha",
				GPUType:      tt.gpuType,
				GPUCount:     1,
				VRAMPerGPUGB: tt.reported,
				PricePerHour: 1.0,
				Region:       "us-east",
				Tier:         provider.TierOnDemand,
			}
			if got := ToAvailableOffering(in, DefaultMarkupPct, DefaultMinRetailMarkupPct, 0, 0, 0).VRAMGB; got != tt.want {
				t.Errorf("customer VRAMGB = %d, want %d", got, tt.want)
			}
			if got := ToStaffOffering(in, DefaultMarkupPct, DefaultMinRetailMarkupPct, 0).VRAMGB; got != tt.want {
				t.Errorf("staff VRAMGB = %d, want %d", got, tt.want)
			}
		})
	}
}
