package provider

import (
	"sync"
	"time"
)

// Traits are the per-adapter classifications the engine, the catalog and the
// API consult by Provider.Name(). They are provider ATTRIBUTES published as
// booleans/classes (instant_boot, deployment_type, disk_configurable, ...),
// never provider identity, so they survive the customer-facing privacy strip.
//
// Every field's zero value is the safe default for a container-native adapter
// that boots fast, takes the certified image, and does not honor a requested
// disk size. An adapter opts INTO anything else, so a forgotten registration
// can only cost capacity or a filter, never misdescribe what a customer gets.
type Traits struct {
	// SlowBoot marks capacity that takes materially longer to become
	// reachable (full-VM boot, floating-IP attach, first-boot image pull)
	// than a container start. Surfaced as instant_boot=false.
	SlowBoot bool
	// VM marks an adapter that hands off a full virtual or bare-metal machine
	// we bootstrap ourselves (driver install + optional certified-image
	// docker-run). Container-native adapters must NOT set it: a VM
	// classification triggers a nested docker-run inside the provider's
	// container.
	VM bool
	// NonNormalizable marks an adapter whose platform cannot run the GPU.ai
	// Certified Image (a fixed, provider-managed image we cannot replace).
	NonNormalizable bool
	// FeaturedPartner lets the catalog headline this adapter's capacity in
	// curated featured rows (published as partner_featured).
	FeaturedPartner bool
	// ConfigurableDisk marks an adapter whose create call actually honors
	// ProvisionRequest.ContainerDiskGB. Opt-in on purpose: a wrong entry hands
	// a customer a different disk than they asked for and paid for.
	ConfigurableDisk bool
	// ProvisioningEstimate is the typical create-to-ready wall clock, shown to
	// customers before launch and used to size the provisioning poll budget.
	// Zero means defaultProvisioningEstimate (a container start).
	ProvisioningEstimate time.Duration
}

var (
	traitsMu sync.RWMutex
	// traitsByName holds the built-in adapters' traits. "generic" is absent
	// on purpose: it is container-native, boots fast and does not forward a
	// disk size (its provisionRequest has no disk field), which is exactly
	// the zero value.
	traitsByName = map[string]Traits{
		// Cloud GPU VMs booted from a driver-preconfigured OS image; the
		// engine's VM bootstrap rides cloud-init user_data.
		"vultr": {SlowBoot: true, VM: true, ProvisioningEstimate: 5 * time.Minute},
	}
)

// RegisterTraits declares the traits of an adapter by its Provider.Name().
// Call it before the adapter is registered (typically from the same place
// that constructs it). Out-of-tree adapters use it to classify themselves;
// tests use it to declare fixture providers. Re-registering a name replaces
// its traits.
func RegisterTraits(providerName string, t Traits) {
	traitsMu.Lock()
	defer traitsMu.Unlock()
	traitsByName[providerName] = t
}

// TraitsFor returns the registered traits for providerName (the zero value for
// an unknown name).
func TraitsFor(providerName string) Traits {
	traitsMu.RLock()
	defer traitsMu.RUnlock()
	return traitsByName[providerName]
}

// InstantBoot reports whether the named provider's instances boot fast enough
// to be included when a customer filters the catalog to instant-boot capacity.
// Unclassified providers default to instant: slow is the exception, so a new
// adapter opts INTO slow and a forgotten entry never hides fast inventory.
func InstantBoot(providerName string) bool {
	return !TraitsFor(providerName).SlowBoot
}

// CommunityTier reports whether offerings from the named provider are
// community-supplied capacity (supplier-owned hardware, softer SLA). GPU.ai
// Open Source ships no community adapter, so only an adapter literally named
// "community" classifies as community tier; every datacenter provider (and any
// unknown/empty name) is false.
func CommunityTier(providerName string) bool { return providerName == "community" }

// CapacityClassFor resolves an offering's secure/community capacity class.
// A valid per-offering stamp (GPUOffering.CapacityClass) wins — that is how a
// marketplace adapter distinguishes its secure and community rows. Anything
// else — empty (an adapter that never stamps) or an unrecognized value (a
// bug) — falls back to the provider-level rule: CommunityTier providers
// classify community, every other provider secure. The result is always one of
// the two valid classes, so downstream consumers never see a third state.
func CapacityClassFor(offeringClass, providerName string) string {
	switch offeringClass {
	case CapacityClassSecure, CapacityClassCommunity:
		return offeringClass
	}
	if CommunityTier(providerName) {
		return CapacityClassCommunity
	}
	return CapacityClassSecure
}

// DeploymentType reports how the named provider hands off compute: "vm" (a full
// virtual/bare-metal machine we bootstrap ourselves) versus "container" (the
// provider runs our image natively; we NEVER nest Docker inside it). Default
// is "container".
func DeploymentType(providerName string) string {
	if TraitsFor(providerName).VM {
		return "vm"
	}
	return "container"
}

// CertifiedImageCapable reports whether the named provider can run the GPU.ai
// Certified Image (the pinned, digest-locked base environment). Default true;
// an adapter opts out with Traits.NonNormalizable.
func CertifiedImageCapable(providerName string) bool {
	return !TraitsFor(providerName).NonNormalizable
}

// FeaturedPartner reports whether the named provider's offerings are eligible
// for the marketplace's curated featured rows. Opt-in; computed at cache-write
// time so readers never need the provider name.
func FeaturedPartner(providerName string) bool {
	return TraitsFor(providerName).FeaturedPartner
}

// ConfigurableDisk reports whether the named provider's adapter honors a
// requested instance disk size. The provision engine uses it to narrow
// candidates when — and only when — the customer explicitly set disk_gb; the
// API handlers use it for the synchronous 422, and the availability layer
// publishes it per offering as disk_configurable.
func ConfigurableDisk(providerName string) bool {
	return TraitsFor(providerName).ConfigurableDisk
}

// defaultProvisioningEstimate is the create-to-ready time assumed for fast
// (container-start) providers and any provider not explicitly classified.
const defaultProvisioningEstimate = 60 * time.Second

// ProvisioningEstimate returns the typical create-to-ready wall clock for a
// provider. It is the per-provider fallback; an adapter that knows a finer-
// grained value stamps it per offering (GPUOffering.ProvisioningEstimate)
// instead, and availability.Normalize prefers the per-offering value.
func ProvisioningEstimate(providerName string) time.Duration {
	if d := TraitsFor(providerName).ProvisioningEstimate; d > 0 {
		return d
	}
	return defaultProvisioningEstimate
}
