package provider

import (
	"sync"
	"testing"
	"time"
)

// registerFixtureTraits declares traits for a fictional fixture provider and
// restores the previous entry when the test ends, so fixture names never leak
// into other tests. A restored zero value is indistinguishable from an
// unregistered name (TraitsFor returns the zero value for both).
func registerFixtureTraits(t *testing.T, name string, tr Traits) {
	t.Helper()
	prev := TraitsFor(name)
	RegisterTraits(name, tr)
	t.Cleanup(func() { RegisterTraits(name, prev) })
}

// TestRegisterTraitsRoundTrip pins the registry contract: TraitsFor returns
// exactly what RegisterTraits stored, and re-registering a name replaces its
// traits rather than merging them.
func TestRegisterTraitsRoundTrip(t *testing.T) {
	want := Traits{
		SlowBoot:             true,
		VM:                   true,
		NonNormalizable:      true,
		FeaturedPartner:      true,
		ConfigurableDisk:     true,
		ProvisioningEstimate: 7 * time.Minute,
	}
	registerFixtureTraits(t, "alpha", want)
	if got := TraitsFor("alpha"); got != want {
		t.Fatalf("TraitsFor(alpha) = %+v, want %+v", got, want)
	}

	replacement := Traits{ConfigurableDisk: true}
	RegisterTraits("alpha", replacement)
	if got := TraitsFor("alpha"); got != replacement {
		t.Fatalf("after re-registration TraitsFor(alpha) = %+v, want %+v (replace, not merge)", got, replacement)
	}
}

// TestTraitsForUnknownIsZero pins the fail-safe default: an unknown or empty
// name yields the zero value, which every accessor maps to the safe default.
func TestTraitsForUnknownIsZero(t *testing.T) {
	for _, name := range []string{"some-future-provider", ""} {
		if got := TraitsFor(name); got != (Traits{}) {
			t.Errorf("TraitsFor(%q) = %+v, want zero value", name, got)
		}
	}
}

// TestBuiltinTraits pins the built-in classifications shipped in the open
// source release: vultr is a slow-booting VM adapter with a 5 minute estimate,
// and generic is the zero value (container-native, fast, no disk forwarding,
// certified-image capable).
func TestBuiltinTraits(t *testing.T) {
	if got, want := TraitsFor("vultr"), (Traits{SlowBoot: true, VM: true, ProvisioningEstimate: 5 * time.Minute}); got != want {
		t.Errorf("TraitsFor(vultr) = %+v, want %+v", got, want)
	}
	if got := TraitsFor("generic"); got != (Traits{}) {
		t.Errorf("TraitsFor(generic) = %+v, want zero value", got)
	}
}

// TestRegisterTraitsConcurrent exercises the registry lock: concurrent
// registrations and reads must not race (run with -race to make it bite).
func TestRegisterTraitsConcurrent(t *testing.T) {
	names := []string{"alpha", "bravo", "charlie"}
	for _, n := range names {
		registerFixtureTraits(t, n, Traits{})
	}
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(2)
		go func(i int) {
			defer wg.Done()
			RegisterTraits(names[i%len(names)], Traits{SlowBoot: i%2 == 0})
		}(i)
		go func(i int) {
			defer wg.Done()
			_ = InstantBoot(names[i%len(names)])
		}(i)
	}
	wg.Wait()
}

// TestInstantBoot pins the boot-profile contract: slow is opt-in by
// Provider.Name(), and every provider NOT opted in, including brand new or
// unknown names, defaults to instant. A typo'd name must come back instant
// (fail-open for inventory visibility), never hidden.
func TestInstantBoot(t *testing.T) {
	registerFixtureTraits(t, "vmco", Traits{SlowBoot: true, VM: true})
	registerFixtureTraits(t, "podco", Traits{})
	tests := []struct {
		provider string
		want     bool
	}{
		{"vmco", false}, // full-VM boot, minutes to reachable
		{"vultr", false},
		{"podco", true},
		{"generic", true},
		{"some-future-provider", true}, // unknown defaults to instant
		{"", true},
	}
	for _, tt := range tests {
		if got := InstantBoot(tt.provider); got != tt.want {
			t.Errorf("InstantBoot(%q) = %v, want %v", tt.provider, got, tt.want)
		}
	}
}

// TestConfigurableDisk pins the disk-capability contract: opt-IN, keyed by
// Provider.Name(), so an unknown or typo'd name comes back false. That polarity
// is the whole point: false costs a placement opportunity, true would silently
// hand a customer a different disk than they requested and were quoted for.
func TestConfigurableDisk(t *testing.T) {
	registerFixtureTraits(t, "diskco", Traits{ConfigurableDisk: true})
	registerFixtureTraits(t, "flavorco", Traits{VM: true})
	tests := []struct {
		provider string
		want     bool
		why      string
	}{
		{"diskco", true, "disk size on the create call"},
		{"flavorco", false, "VM flavor fixes the root volume, immutable after create"},
		{"vultr", false, "plan fixes the disk"},
		{"some-future-provider", false, "unknown names must not claim the capability"},
		{"", false, "empty name must not claim the capability"},
	}
	for _, tt := range tests {
		if got := ConfigurableDisk(tt.provider); got != tt.want {
			t.Errorf("ConfigurableDisk(%q) = %v, want %v (%s)", tt.provider, got, tt.want, tt.why)
		}
	}
}

// TestConfigurableDiskExcludesGeneric guards the one entry most likely to be
// "fixed" by someone reconciling this list against custom-image support.
// generic is genuinely custom-image capable, but its provisionRequest literal
// (internal/provider/generic/adapter.go) omits ContainerDiskGB, along with
// Ports and Env, so a disk request routed there is dropped silently, with no
// error and no upstream rejection. Opting "generic" in without first mapping
// the field in that adapter would make us bill for disk we never asked the
// supplier for.
func TestConfigurableDiskExcludesGeneric(t *testing.T) {
	if ConfigurableDisk("generic") {
		t.Error("ConfigurableDisk(\"generic\") = true; generic drops ContainerDiskGB on the floor " +
			"(see generic/adapter.go provisionRequest), so map the field there before opting it in")
	}
}

// TestCommunityTier pins the community-tier classification contract: like
// InstantBoot, it is a name-keyed provider ATTRIBUTE (opt-in per D-03), true
// ONLY for the "community" provider and false for every datacenter provider
// (and the empty name). It must never key off anything but Provider.Name().
func TestCommunityTier(t *testing.T) {
	tests := []struct {
		provider string
		want     bool
	}{
		{"community", true},
		{"generic", false},
		{"vultr", false},
		{"alpha", false},
		{"some-future-provider", false},
		{"", false},
	}
	for _, tt := range tests {
		if got := CommunityTier(tt.provider); got != tt.want {
			t.Errorf("CommunityTier(%q) = %v, want %v", tt.provider, got, tt.want)
		}
	}
}

// TestDeploymentType pins the Phase 63 deployment-class contract: VM providers
// we bootstrap ourselves classify "vm"; container-native providers (and
// unknown/empty names, which default container so a forgotten entry never
// triggers a bogus nested docker-run, Pitfall 1) classify "container".
func TestDeploymentType(t *testing.T) {
	registerFixtureTraits(t, "vmco", Traits{SlowBoot: true, VM: true})
	registerFixtureTraits(t, "podco", Traits{})
	tests := []struct {
		provider string
		want     string
	}{
		{"vmco", "vm"},
		{"vultr", "vm"},
		{"podco", "container"},
		{"generic", "container"},
		{"community", "container"},
		{"some-future-provider", "container"}, // unknown defaults to container
		{"", "container"},
	}
	for _, tt := range tests {
		if got := DeploymentType(tt.provider); got != tt.want {
			t.Errorf("DeploymentType(%q) = %q, want %q", tt.provider, got, tt.want)
		}
	}
}

// TestCertifiedImage pins the Phase 63 certified-image capability contract: an
// adapter that opts out with NonNormalizable classifies false (D-63-02); every
// other provider, including unknown names, classifies true.
func TestCertifiedImage(t *testing.T) {
	registerFixtureTraits(t, "fixedimageco", Traits{NonNormalizable: true})
	tests := []struct {
		provider string
		want     bool
	}{
		{"fixedimageco", false}, // D-63-02: cannot take the GPU.ai Certified Image
		{"vultr", true},
		{"generic", true},
		{"some-future-provider", true},
	}
	for _, tt := range tests {
		if got := CertifiedImageCapable(tt.provider); got != tt.want {
			t.Errorf("CertifiedImageCapable(%q) = %v, want %v", tt.provider, got, tt.want)
		}
	}
}

// TestFeaturedPartner pins the opt-in polarity of the featured-row flag.
func TestFeaturedPartner(t *testing.T) {
	registerFixtureTraits(t, "partnerco", Traits{FeaturedPartner: true})
	if !FeaturedPartner("partnerco") {
		t.Error("FeaturedPartner(partnerco) = false, want true for an opted-in adapter")
	}
	for _, name := range []string{"generic", "vultr", "some-future-provider", ""} {
		if FeaturedPartner(name) {
			t.Errorf("FeaturedPartner(%q) = true, want false (opt-in only)", name)
		}
	}
}

// TestProvisioningEstimate pins the estimate fallback: a registered non-zero
// estimate wins, and a zero or unknown entry falls back to the container-start
// default.
func TestProvisioningEstimate(t *testing.T) {
	registerFixtureTraits(t, "vmco", Traits{SlowBoot: true, VM: true, ProvisioningEstimate: 12 * time.Minute})
	registerFixtureTraits(t, "podco", Traits{})
	tests := []struct {
		provider string
		want     time.Duration
	}{
		{"vmco", 12 * time.Minute},
		{"vultr", 5 * time.Minute},
		{"podco", defaultProvisioningEstimate},
		{"generic", defaultProvisioningEstimate},
		{"some-future-provider", defaultProvisioningEstimate},
	}
	for _, tt := range tests {
		if got := ProvisioningEstimate(tt.provider); got != tt.want {
			t.Errorf("ProvisioningEstimate(%q) = %v, want %v", tt.provider, got, tt.want)
		}
	}
}
