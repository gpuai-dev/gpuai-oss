package availability

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"math"
	"sync"

	"github.com/gpuai-dev/gpuai-oss/internal/manifest"
	"github.com/gpuai-dev/gpuai-oss/internal/provider"
)

// AvailableOffering is the customer-facing GPU offering representation.
// Uses defense-by-omission: no Provider field exists, so provider identity
// can never leak via json.Marshal.
//
// InstantBoot is the one provider-derived attribute that intentionally
// survives the strip: a boot-speed CLASS (fast container start vs slow
// full-VM boot), computed from provider.InstantBoot at cache-write time so
// readers never need the provider name. Cache entries written before the
// field existed unmarshal to false (slow) — self-heals within one poll
// cycle (~30s).
type AvailableOffering struct {
	GPUModel       string  `json:"gpu_model"`
	GPUCount       int     `json:"gpu_count"`
	VRAMGB         int     `json:"vram_gb"`
	CPUCores       int     `json:"cpu_cores"`
	RAMGB          int     `json:"ram_gb"`
	StorageGB      int     `json:"storage_gb"`
	PricePerHour   float64 `json:"price_per_hour"`
	Region         string  `json:"region"`
	Tier           string  `json:"tier"`
	AvailableCount int     `json:"available_count"`
	StockStatus    string  `json:"stock_status"`
	AvgUptimePct   float64 `json:"avg_uptime_pct"`
	InstantBoot    bool    `json:"instant_boot"`
	// Community is the second instance of the attribute-survives-the-strip
	// pattern (see InstantBoot above): a boolean CLASS marking
	// community-supplied capacity (supplier-owned hardware), computed at
	// cache-write time so readers never need the provider name. It is an
	// ATTRIBUTE, never provider identity. Since the per-OFFERING capacity
	// class landed it is exactly CapacityClass == "community" — kept as a bool
	// twin because every downstream dedupe/merge key and the public pricing
	// wire shape already speak it. Cache entries written before the field
	// existed unmarshal to false (datacenter) and self-heal within one poll
	// cycle (~30s) — the documented InstantBoot precedent for exactly this
	// additive field shape.
	Community bool `json:"community"`
	// CapacityClass is the offering's secure/community classification
	// ("secure" | "community") — the customer-visible signal the availability
	// UI splits its tabs on. Per-OFFERING, not per-provider: one provider can
	// sell both secure and community capacity, and a marketplace's hosts can
	// straddle datacenter vs individual, so a provider-level flag cannot
	// express it. Resolved at cache-write via
	// provider.CapacityClassFor (adapter stamp wins, provider-name rule as
	// fallback), so it is always one of the two valid classes for fresh rows.
	// Carries no provider identity — it survives the strip like InstantBoot.
	// Pre-field cache entries unmarshal to "" and self-heal within one poll
	// cycle (~30s); "" must be treated as unknown, never defaulted to a class.
	CapacityClass string `json:"capacity_class"`
	// ProvisioningEstimateMinutes is the typical create→ready wall-clock for
	// this offering, in minutes — surfaced to customers as a pre-launch
	// expectation (the launch warning on non-instant capacity reads it, e.g.
	// "up to ~15 min"). Carries no provider identity (just a number), so it is
	// safe past the defense-by-omission strip. Cache entries written before the
	// field existed unmarshal to 0; the marketplace simply omits the estimate
	// for those until the next ~30s poll refreshes them.
	ProvisioningEstimateMinutes int `json:"provisioning_estimate_minutes"`
	// CertifiedImage marks whether this offering's provider can run the GPU.ai
	// Certified Image (the pinned, digest-locked base environment). Like
	// InstantBoot/Community it is a name-derived boolean CLASS computed from
	// provider.CertifiedImageCapable at cache-write time — an ATTRIBUTE, never
	// provider identity, so it survives the defense-by-omission strip. Cache
	// entries written before the field existed unmarshal to false (not certified)
	// and self-heal within one poll cycle (~30s).
	CertifiedImage bool `json:"certified_image"`
	// DeploymentType is how this offering's provider hands off compute — "vm"
	// (we bootstrap the box: driver install + optional certified-image
	// docker-run) or "container" (provider runs our image natively; never nest
	// Docker). Computed from provider.DeploymentType at cache-write time; carries
	// no provider identity (just a class), so it is safe past the strip.
	//
	// CRITICAL: "" means an UNKNOWN, pre-field cache entry (written before this
	// field existed) — it is NOT a valid deployment class and must NOT silently
	// match either the "vm" or "container" filter value. Such entries self-heal
	// to a real class within one poll cycle (~30s); until then a deployment_type
	// filter excludes them rather than guessing.
	DeploymentType string `json:"deployment_type"`
	// VMCapable marks that at least one live host backing this offering could
	// hand off a full virtual machine (per-offering adapter stamp, e.g. a
	// marketplace's VM-enabled hosts). It is an ATTRIBUTE that survives the strip, like
	// InstantBoot. Distinct from DeploymentType on purpose: deployment_type
	// says how launches ACTUALLY hand off today (badge + engine bootstrap
	// mode), while vm_capable is upstream capability we ingest ahead of a VM
	// launch path existing — consumers must not treat it as launchable-as-VM.
	// Pre-field cache entries unmarshal to false and self-heal within one poll
	// cycle (~30s).
	VMCapable bool `json:"vm_capable"`
	// PartnerFeatured marks capacity the marketplace may headline in curated
	// "featured" rows (the multi-GPU node showcase). Another instance of the
	// attribute-survives-the-strip pattern (see InstantBoot): a name-derived
	// boolean CLASS computed from provider.FeaturedPartner at cache-write time —
	// an ATTRIBUTE, never provider identity. Pre-field cache entries unmarshal
	// to false (not featured, fail-closed) and self-heal within one poll
	// cycle (~30s).
	PartnerFeatured bool `json:"partner_featured"`
	// OfferingID is an opaque, deterministic identifier for pinning an EXACT
	// offering at launch (D-63-08: unavailable → explicit error, never a silent
	// pricier substitute). It is derived from stable offering attributes, carries
	// no provider identity, and survives the strip. Pre-field cache entries
	// unmarshal to "" and self-heal within one poll cycle (~30s).
	OfferingID string `json:"offering_id"`
	// OSOptions lists the raw-VM OS catalog ids this offering can boot with the
	// raw_vm:<os> grammar (the launch OS picker), e.g. ["ubuntu-22.04",
	// "ubuntu-24.04"]. Computed at cache-write from the provider name via the
	// embedded manifest's vm_images map — the ids are OURS; the per-provider
	// native image slugs stay server-side (D6 defense by omission, the
	// capacity_class/deployment_type pattern). VM offerings only: container
	// and unmapped-provider rows omit it (nil under omitempty). Pre-field cache
	// entries unmarshal to nil and self-heal within one poll cycle (~30s).
	OSOptions []string `json:"os_options,omitempty"`
	// InstanceDiskGB is the filesystem size, in GB, that a launch of THIS
	// offering actually receives — the number a customer can hold us to, not a
	// display assumption. Where the upstream honors the disk we ask for
	// (DiskConfigurable) it is the platform's effective default
	// (config.LaunchDefaultDiskGB, threaded in at cache-write), because that is
	// literally what the engine hands the adapter. Everywhere else the machine's
	// disk is the upstream's to decide, so it is that machine's REPORTED disk.
	//
	// 0 means UNKNOWN — an upstream that reported no disk — and must render as
	// unknown, never as a number. It deliberately does NOT inherit StorageGB's
	// 40 GB display fill: this field is a launch PROMISE, and a fabricated
	// promise is precisely the bug the configurable-disk work exists to close
	// (see the WR-03 note in ToAvailableOffering — the fill itself is removed
	// separately, after which the two agree).
	//
	// Carries no provider identity (just an integer), so it survives the
	// defense-by-omission strip like InstantBoot. Pre-field cache entries
	// unmarshal to 0 and self-heal within one poll cycle (~30s).
	InstanceDiskGB int `json:"instance_disk_gb"`
	// DiskConfigurable reports whether a launch of this offering may choose its
	// own disk size (the disk_gb request field). Another instance of the
	// attribute-survives-the-strip pattern (see InstantBoot): a name-derived
	// boolean CLASS computed from provider.ConfigurableDisk at cache-write time
	// — an ATTRIBUTE, never provider identity.
	//
	// The launch UI reads it to decide whether to render a disk control at all,
	// rather than offering a slider that fails on submit, and the create
	// handlers refuse an explicit disk_gb against a non-configurable offering
	// on the same signal. Pre-field cache entries unmarshal to false —
	// fail-CLOSED (no control offered, no launch broken), the safe direction —
	// and self-heal within one poll cycle (~30s).
	DiskConfigurable bool `json:"disk_configurable"`
	// DiskPricePerGBHour is the RETAIL price of one GB of instance disk beyond
	// InstanceDiskGB, per hour, for launches of THIS offering — the marginal
	// slope of the same fold the engine bills through (retailWithDisk): the
	// provider's wholesale disk rate carried through the effective markup
	// (max of the configured markup and the min-retail floor, the branch
	// RetailPrice's clamp picks for any positive basis). The launch UI uses it
	// to price the disk selector ("+$0.02/hr") and fold the uplift into the
	// quoted hourly price; the server's billed rate remains the trust boundary
	// (display can differ from the bill by at most the total's cent rounding).
	//
	// 0 on non-configurable capacity (there is nothing to buy) and while disk
	// metering is switched off (DISK_PRICE_PER_GB_HOUR and all per-provider
	// overrides unset/0) — the UI then renders larger sizes without a price,
	// exactly the pre-pricing behavior. Carries no provider identity (just a
	// rate), so it survives the defense-by-omission strip like InstantBoot.
	// Pre-field cache entries unmarshal to 0 and self-heal within one poll
	// cycle (~30s).
	DiskPricePerGBHour float64 `json:"disk_price_per_gb_hour"`
}

// StockStatusForCount derives a stock label ("High"/"Medium"/"Low"/"") from a
// FLEET-WIDE summed available count. Per-offering StockStatus labels are only
// stamped by some adapters and describe a single datacenter's stock, so any
// surface aggregating across offerings must derive its label from the summed
// count instead of propagating per-offering labels — otherwise plentiful
// capacity spread across many DCs reads as "Low". The availability dashboard
// derives its fleet-wide dots with these same thresholds
// (frontend/src/components/cloud/GPUAvailabilityTable.tsx).
func StockStatusForCount(count int) string {
	switch {
	case count >= 32:
		return "High"
	case count >= 10:
		return "Medium"
	case count >= 1:
		return "Low"
	default:
		return ""
	}
}

// DefaultMarkupPct is the default markup percentage applied to provider prices.
// GPU.ai retail price = provider price * (1 + markup/100).
const DefaultMarkupPct = 6.0

// DefaultMinRetailMarkupPct is a TEST fixture floor percentage, not the
// platform default — config's PRICING_MIN_RETAIL_MARKUP_PCT defaults to 0
// (sitewide passthrough) and both envs pin it explicitly. It exists so tests
// can exercise a non-zero floor: retail may never fall below cost ×
// (1 + this/100), rounded up to the cent. See RetailPrice.
const DefaultMinRetailMarkupPct = 5.0

// centEpsilon absorbs float64 representation noise when deciding whether a
// price in cents already sits exactly on a whole cent. Without it the floor's
// round-UP would tax exact boundaries an extra cent: 2.00 × 1.05 × 100
// evaluates to 210.00000000000003, which must ceil to 210 (=$2.10), not 211.
// One billionth of a cent is far below any real price granularity, so it can
// only ever cancel representation error, never a real sub-cent remainder.
const centEpsilon = 1e-9

// defaultAvgUptime returns a static uptime percentage based on tier.
// On-demand has higher reliability than spot.
const (
	avgUptimeOnDemand = 99.5
	avgUptimeSpot     = 95.0
)

// canonicalRegionMap maps native provider region labels to the canonical
// sub-continental set defined in CONTEXT.md D-06.
//
// Coverage:
//   - Canonical labels (us-east, us-west, eu-east, ...) self-map, so an
//     adapter that already emits canonical labels resolves unchanged.
//   - ISO-style country and country-subdivision codes (US-CA, EU-RO, US, CA,
//     AU, ...), the shape many suppliers use for a datacenter location.
//   - Generic datacenter labels (us-central-N, us-east-N) and Indian city
//     names (Delhi, Mumbai, Bangalore, in capitalized and lowercase variants).
//   - Vultr's native region ids (below).
//
// Pitfall 5 (RESEARCH.md): Bangalore is geographically south India; bucketed
// as india-south alongside Delhi/Noida for v1 since the canonical set is
// sub-continental.
//
// D-06 canonical set: us-east, us-central, us-west, eu-east, eu-west,
// eu-north, asia-pacific, india-south, india-west, ca-central, sa-east,
// me-central, unknown. (us-central split out of us-east 2026-07 for the
// Texas, Illinois, Kansas, Dallas and Chicago locations.)
var canonicalRegionMap = map[string]string{
	// Already-canonical labels self-map.
	"us-east":      "us-east",
	"us-west":      "us-west",
	"us-central":   "us-central", // terminal: central US is its own canonical bucket (split out of us-east per ops request, 2026-07)
	"eu-east":      "eu-east",
	"eu-west":      "eu-west",
	"eu-north":     "eu-west",
	"ca-central":   "ca-central", // terminal: Canada is its own canonical bucket (split out of us-east per ops request)
	"ap-northeast": "asia-pacific",
	"ap-southeast": "asia-pacific",
	"ap-south":     "india-south",
	"me-central":   "asia-pacific",
	"sa-east":      "us-east",
	"unknown":      "unknown",

	// ISO-style country-subdivision codes (exact match).
	"US-CA": "us-west",
	"US-OR": "us-west",
	"US-WA": "us-west",
	"US-TX": "us-central",
	"US-GA": "us-east",
	"US-NJ": "us-east",
	"US-VA": "us-east",
	"US-NY": "us-east",
	"US-IL": "us-central",
	"US-KS": "us-central",
	"US-DE": "us-east",
	"US-NC": "us-east",
	"EU-RO": "eu-east",
	"EU-CZ": "eu-east",
	"EU-BG": "eu-east",
	"EU-SE": "eu-west",
	"EU-NO": "eu-west",
	"EU-IS": "eu-west",
	"EU-NL": "eu-west",
	"EU-GB": "eu-west",
	"EU-DE": "eu-west",
	"EU-FR": "eu-west",
	"EU":    "eu-west",
	"AP-JP": "asia-pacific",
	"OC-AU": "asia-pacific",

	// ISO2 country codes, for suppliers that report no finer placement than
	// a country. CA is Canada (California is US-CA, above). IN buckets with
	// Delhi; PL buckets east with CZ/RO/BG; FI/NO/NL/FR bucket west
	// (EU-SE/EU-NO precedent); JP/AU go to asia-pacific (AP-JP/OC-AU
	// precedent).
	"US": "us-east",
	"CA": "ca-central",
	"AU": "asia-pacific",
	"FI": "eu-west",
	"FR": "eu-west",
	"IN": "india-south",
	"JP": "asia-pacific",
	"NL": "eu-west",
	"NO": "eu-west",
	"PL": "eu-east",

	// Generic numbered datacenter labels. Central US buckets into the
	// us-central terminal canonical (split out of us-east, 2026-07).
	"us-central-1": "us-central",
	"us-central-2": "us-central",
	"us-central-3": "us-central",
	"us-central-9": "us-central",
	"us-east-1":    "us-east",
	"us-east-5":    "us-east",

	// Indian city names.
	"Delhi":     "india-south", // NCR/Delhi -> india-south
	"Mumbai":    "india-west",
	"Bangalore": "india-south", // Pitfall 5: south India bucketed as india-south for v1
	"delhi":     "india-south",
	"mumbai":    "india-west",
	"bangalore": "india-south",
	"ncr":       "india-south",

	// Vultr — native region ids (3-letter airport-style codes, the value
	// POST /v2/instances takes, emitted verbatim as GPUOffering.Region).
	// Full region list live-verified 2026-08-11 from GET /v2/regions; GPU
	// (vcg) plans today list only ewr/ord/atl/fra/sjc/nrt/sgp/blr, but the
	// whole set is mapped so a region Vultr adds GPU stock to later lands in
	// a canonical bucket instead of passing through unmapped. Bucketing
	// follows the existing precedents: Chicago/Dallas → us-central
	// (US-IL/US-TX), LatAm incl. Mexico City → us-east (the
	// "sa-east"→"us-east" v1 precedent), Tel Aviv → asia-pacific
	// (the me-central→asia-pacific precedent), Johannesburg → eu-west (no
	// Africa bucket; closest longitude/timezone).
	"ewr": "us-east",      // New Jersey
	"atl": "us-east",      // Atlanta
	"mia": "us-east",      // Miami
	"ord": "us-central",   // Chicago
	"dfw": "us-central",   // Dallas
	"mex": "us-east",      // Mexico City — sa-east precedent
	"sjc": "us-west",      // Silicon Valley
	"lax": "us-west",      // Los Angeles
	"sea": "us-west",      // Seattle
	"hnl": "us-west",      // Honolulu
	"yto": "ca-central",   // Toronto
	"ams": "eu-west",      // Amsterdam
	"cdg": "eu-west",      // Paris
	"fra": "eu-west",      // Frankfurt
	"lhr": "eu-west",      // London
	"mad": "eu-west",      // Madrid
	"man": "eu-west",      // Manchester
	"mxp": "eu-west",      // Milan
	"sto": "eu-west",      // Stockholm (eu-north→eu-west precedent)
	"jnb": "eu-west",      // Johannesburg — no Africa bucket, closest timezone
	"waw": "eu-east",      // Warsaw
	"nrt": "asia-pacific", // Tokyo
	"itm": "asia-pacific", // Osaka
	"icn": "asia-pacific", // Seoul
	"sgp": "asia-pacific", // Singapore
	"syd": "asia-pacific", // Sydney
	"mel": "asia-pacific", // Melbourne
	"tlv": "asia-pacific", // Tel Aviv — me-central→asia-pacific precedent
	"blr": "india-south",  // Bangalore (Pitfall 5: south India → india-south)
	"del": "india-south",  // Delhi NCR
	"bom": "india-west",   // Mumbai
	"sao": "us-east",      // São Paulo — sa-east precedent
	"scl": "us-east",      // Santiago — sa-east precedent

	// Identity entries — EVERY terminal canonical bucket must also be a KEY
	// that self-maps (WR-04, Phase 58 review). Community suppliers declare
	// regions as free text and are expected to use canonical labels; a
	// declaration like "india-south" passes the IsCanonicalRegion gate (it is
	// a VALUE) but, without a self-mapping key, Normalize would miss the map
	// and take the passthrough+Warn path on EVERY evaluation — the adapter
	// gate each 30s poll, again at cache-write, again per provision request —
	// permanent Warn spam for a fully valid configuration, drowning the
	// signal this warning exists for (real map gaps). us-east/us-central/
	// us-west/eu-east/eu-west/ca-central/unknown already self-map above;
	// TestCanonicalRegionMapValuesSelfMap locks the invariant.
	"india-south":  "india-south",
	"india-west":   "india-west",
	"asia-pacific": "asia-pacific",
}

// canonicalRegionSet is the set of terminal canonical region buckets — the
// distinct VALUES of canonicalRegionMap minus RegionUnknown — computed once so it
// can never drift from the map that Normalize resolves into.
var canonicalRegionSet = func() map[string]bool {
	set := make(map[string]bool, len(canonicalRegionMap))
	for _, canonical := range canonicalRegionMap {
		if canonical != RegionUnknown {
			set[canonical] = true
		}
	}
	return set
}()

// IsCanonicalRegion reports whether name is one of the platform's terminal
// canonical region buckets (us-east, us-central, us-west, eu-east, eu-west,
// ca-central, asia-pacific, india-south, india-west). It is the membership
// test a caller
// applies to Normalize's OUTPUT to decide whether a region is a real,
// customer-routable bucket.
//
// This exists because Normalize passes UNMAPPED non-empty input through VERBATIM
// (D-07 passthrough+warn) rather than collapsing it to RegionUnknown. A supplier-
// declared junk region like "bobs-basement-austin" therefore normalizes to itself
// and would surface unchecked — the community adapter (T-58-02) uses this helper
// to fail closed on any region that is not a canonical bucket. RegionUnknown is
// deliberately NOT a member: an empty/missing source region is a data-quality
// signal, not a routable region.
//
// Pure read helper — it touches no poll/ranking/selection mechanism.
func IsCanonicalRegion(name string) bool {
	return canonicalRegionSet[name]
}

// RegionUnknown is the sentinel canonical region returned by Normalize when a
// provider emits an offering with an EMPTY source region. It is a data-quality
// signal (a missing provider-API field), not a real, customer-routable region —
// the poller drops these from the public catalog so customers never see an
// "unknown"-region price they cannot act on (M1).
const RegionUnknown = "unknown"

// Normalize maps a native region label to the canonical sub-continental set
// defined in CONTEXT.md D-06.
//
// Lookup order:
//  1. Empty input returns "unknown" (matches Phase 15-03 regionFromResult
//     fallback behavior).
//  2. Direct map hit — covers already-canonical labels (us-east, us-west, ...)
//     and exact-match natives (Delhi, EU-RO, ewr, ...).
//  3. Unmapped: emit slog.Warn (D-07 — surfaces gaps so ops can extend the
//     map) and return rawRegion unchanged so the catalog still lists it.
//
// Called from ToAvailableOffering (cache-write path) AND from
// provision.SelectProviderCandidates (filter path) — single source of truth
// so every provider flows through the same mapping (D-05).
func Normalize(rawRegion, datacenterLocation, providerName string) string {
	if rawRegion == "" {
		return RegionUnknown
	}
	// D-05/D-06: direct canonical map hit.
	if canonical, ok := canonicalRegionMap[rawRegion]; ok {
		return canonical
	}
	// D-07: passthrough + warn — surfaces gaps for ops to extend the map.
	slog.Warn("unmapped native region",
		"provider", providerName,
		"raw_region", rawRegion,
		"datacenter_location", datacenterLocation,
	)
	return rawRegion
}

// RetailPrice converts what an offering actually COSTS us into the customer
// retail price: the percentage markup applied and rounded to whole cents, then
// clamped to the retail floor — retail may never fall below
// costBasis × (1 + minMarkupPct/100), rounded UP to the next cent.
//
// This is THE pricing formula: the catalog (ToAvailableOffering), the staff twin
// (ToStaffOffering), the spot re-router, and the provision engine (candidate
// sort, price caps, instance/billing-session persist) all use it, so the price a
// customer sees in the catalog is byte-identical to the rate they are billed at
// — and the floor holds on every one of those surfaces at once.
//
// THE BASIS IS TRUE COST, NOT LIST. Callers pass
// TrueCost(wholesale, costDiscountPct) — what the provider actually invoices us
// — and the markup is applied to THAT. Two numbers per provider now fully
// describe its pricing:
//
//	markupPct        what we add on top of what we pay (0 = passthrough policy)
//	costDiscountPct  how far below list the provider invoices us (partner deal)
//
//	retail = list × (1 − costDiscountPct/100) × (1 + markupPct/100)
//
// Marking up LIST while flooring on true cost — the previous shape — meant a
// partner discount could not reach the customer on its own, so expressing "pass
// a provider's 10% partner discount straight through" took TWO settings that had
// to agree: a −10 markup to move the published price and a 10 discount to move
// the floor out of its way. Either one alone was silently wrong. Deriving retail
// from cost collapses that to one number per fact, and makes margin% equal
// markupPct exactly, for every provider — which is what makes the margin/loss
// surfaces readable without reconciling list against invoice.
//
// The floor exists because the 2026-07 billing audit found 23 prod instances
// launched with retail BELOW upstream cost through two gaps in the unfloored
// formula: (1) zero/negative effective markups let math.Round round sub-cent
// rates DOWN (0.792 → 0.79, 1.5048 → 1.50); (2) upstream list-price drift
// between poll and launch meant the marked-up stale price could undercut the
// provider's actual charge. max(retail, floor) closes the class: whatever the
// markup configuration or rounding does, the published price can never undercut
// the FLOOR it was computed against.
//
// minMarkupPct MAY BE NEGATIVE (since v101): it is the per-supplier LOSS FLOOR,
// resolved through admin.Store.MinMarginPctFor. −10 sanctions publishing and
// billing that supplier at up to a 10% loss — the floor clamps at
// costBasis × 0.90 instead of at costBasis. The sitewide env floor
// (PRICING_MIN_RETAIL_MARKUP_PCT) is still validated ≥ 0, so a below-cost
// floor only ever arrives via an explicit per-supplier admin override. The
// clamp's PROTECTIVE role is unchanged in shape: the price can undercut the
// cost basis by at most the sanctioned percentage, never more.
//
// The floor — not a range check — is also why markupPct is deliberately
// UNBOUNDED, including negative. A markup of −50 does not sell 50% below cost;
// it clamps at the configured floor (cost with the default floor of 0).
// Bounding the input at 0 (as the admin panel and the v50 table CHECK did
// until v85) blocked a real configuration while adding no protection the floor
// was not already providing.
//
// A non-positive costBasis is returned un-floored — reserved-tier rows
// deliberately carry a 0 rate (flat-fee, off-platform billing) and must stay 0.
//
// Callers folding in a cost we are NOT discounted on — the engine's disk term,
// our own configured rate, invoiced at face value — add it to the basis AFTER
// discounting, so a GPU-only partner discount never discounts our disk.
func RetailPrice(costBasis, markupPct, minMarkupPct float64) float64 {
	retail := markedUpRetail(costBasis, markupPct)
	if costBasis <= 0 {
		return retail
	}
	if floor := retailFloor(costBasis, minMarkupPct); floor > retail {
		return floor
	}
	return retail
}

// TrueCost converts a provider's LIST wholesale rate into what we are actually
// invoiced, applying a partner cost discount (config.CostDiscountPctFor). It is
// the costBasis every RetailPrice caller passes for a plain offering.
//
// Out-of-range discounts (negative, or ≥100% — which would claim free or
// paid-to-take capacity) are ignored rather than trusted: the safe degradation
// is the un-discounted list basis, which floors higher, never below cost.
// config.Load already range-checks these; this is defense in depth on a path
// where being wrong means selling below cost.
func TrueCost(wholesale, costDiscountPct float64) float64 {
	if costDiscountPct <= 0 || costDiscountPct >= 100 {
		return wholesale
	}
	return wholesale * (1.0 - costDiscountPct/100.0)
}

// DiskWholesalePerHour returns the WHOLESALE hourly cost of the instance disk
// beyond the included allowance: max(0, diskGB-includedGB) × pricePerGBHour.
// Disk at or under the allowance, a non-positive price, or a non-positive
// overage all return exactly 0.
//
// It returns a WHOLESALE addend, not a retail add-on, and that distinction is
// deliberate. Callers fold it into the wholesale basis and let the existing
// RetailPrice apply markup, rounding and the minimum-margin floor ONCE over
// (GPU + disk):
//
//	RetailPrice(TrueCost(offering.PricePerHour, disc)+DiskWholesalePerHour(...), markup, minMarkup)
//
// Marking up and rounding the disk separately would be materially wrong at the
// small end, because retailFloor rounds UP to the next whole cent: a customer
// 1 GB over the allowance would be charged $0.01/hr — about $7.30/month — for
// disk that costs us roughly $0.10/month. Folding into one basis makes disk
// cost behave exactly like GPU cost, bills the actual overage, and keeps the
// quoted rate cent-aligned like every other price on the platform.
//
// With pricePerGBHour = 0 this returns 0 and every quote is bit-identical to
// the GPU-only formula, which is what makes shipping the metering path inert
// a provable no-op rather than a hopeful one.
func DiskWholesalePerHour(diskGB, includedGB int, pricePerGBHour float64) float64 {
	if pricePerGBHour <= 0 {
		return 0
	}
	billable := diskGB - includedGB
	if billable <= 0 {
		return 0
	}
	return float64(billable) * pricePerGBHour
}

// markedUpRetail is the raw markup formula (round to NEAREST cent), before the
// floor clamp. Kept separate so floorEngaged can detect when the floor
// clamped. Never expose this as a pricing surface: publishing it un-floored is
// exactly the below-cost bug the floor closes.
func markedUpRetail(wholesale, markupPct float64) float64 {
	return math.Round(wholesale*(1.0+markupPct/100.0)*100) / 100
}

// floorEngaged reports whether the retail floor clamped the configured markup
// price for this wholesale rate — i.e. the operator's markup prices at or
// below the cost floor. The poller aggregates this per provider per cycle
// into a single ops warn (a 0%-markup provider with ~190 offers used to emit
// one warn per offer per 30s cycle — 12.8k lines/hour of the same fact).
func floorEngaged(wholesale, markupPct, minMarkupPct, costDiscountPct float64) bool {
	basis := TrueCost(wholesale, costDiscountPct)
	return RetailPrice(basis, markupPct, minMarkupPct) > markedUpRetail(basis, markupPct)
}

// retailFloor is the minimum permissible retail price for a wholesale price:
// wholesale × (1 + minMarkupPct/100), rounded UP to the next whole cent —
// never down (round-to-nearest is exactly how sub-cent wholesale rates leaked
// retail below cost). centEpsilon keeps exact-cent boundaries from being
// taxed an extra cent by float noise. With a negative minMarkupPct (a
// per-supplier loss floor) the round-UP still applies, so the realized loss is
// never more than the sanctioned percentage.
func retailFloor(wholesale, minMarkupPct float64) float64 {
	return math.Ceil(wholesale*(1.0+minMarkupPct/100.0)*100-centEpsilon) / 100
}

// ToAvailableOffering converts a provider-internal GPUOffering to a customer-safe
// AvailableOffering with markup pricing applied.
// markupPct is the percentage markup (e.g., 15.0 for 15% markup); minMarkupPct
// is the retail floor markup (config.MinRetailMarkupPct — see RetailPrice).
// defaultDiskGB is the platform's effective launch disk (config.LaunchDefaultDiskGB,
// wired through availability.WithDefaultInstanceDisk) — see instanceDiskGB, which
// documents the <= 0 degradation.
// diskPricePerGBHour is the provider's WHOLESALE disk rate in $/GB/hr
// (config.DiskPricePerGBHourFor, wired through availability.WithDiskPriceFor);
// see AvailableOffering.DiskPricePerGBHour for the retail derivation.
func ToAvailableOffering(o provider.GPUOffering, markupPct, minMarkupPct, costDiscountPct float64, defaultDiskGB int, diskPricePerGBHour float64) AvailableOffering {
	retailPrice := RetailPrice(TrueCost(o.PricePerHour, costDiscountPct), markupPct, minMarkupPct)

	// Floor engagement (customers only ever see the floored price) is NOT
	// warned here — this runs per offer per refresh, which flooded the logs.
	// The poller detects it via floorEngaged and emits one summary warn per
	// provider per cycle (see collectOfferings).

	// WR-03 (Phase 58 review), generalized by the configurable-disk work: an
	// unreported spec is UNKNOWN and renders as unknown — never a fabricated
	// number. This started as a community-row exemption (nothing about a
	// supplier machine's disk is declared or verified, and the tier's defining
	// property is a SOFTER SLA, so the old 40 GB storage fill and the 99.5/95
	// uptime constants were skipped for community rows). The 40 GB fill itself
	// is now GONE for every row: StorageGB passes through as reported, 0 means
	// unreported, and the launch-relevant number is InstanceDiskGB — a promise
	// keyed on what the platform actually controls. The uptime constants remain
	// community-exempt as before.
	//
	// VERIF-03 (Phase 60): AvgUptimePct for a community row is now the machine's
	// EARNED reliability_score when it has one (o.ReliabilityScore != nil), replacing
	// the deliberate hard-coded 0 placeholder. An unscored machine still renders
	// honestly as 0 — the frontend's em-dash "unknown" (the same treatment community
	// CPU/RAM already get), never a fabricated number. This is cache-write display
	// metadata only — no poll, ranking, or selection mechanism reads it, and the
	// price-only provision sort is untouched (D-60-04).
	communityTier := provider.CommunityTier(o.Provider)

	// The per-OFFERING secure/community class (adapter stamp wins, provider-
	// name rule as fallback). Community stays as its bool twin because every
	// downstream dedupe/merge key already speaks it.
	capacityClass := provider.CapacityClassFor(o.CapacityClass, o.Provider)

	avgUptime := avgUptimeOnDemand
	if o.Tier == provider.TierSpot {
		avgUptime = avgUptimeSpot
	}
	if communityTier {
		avgUptime = 0 // honest "unknown" default — renders em-dash
	}
	// A MEASURED reliability score beats any tier constant, on any provider:
	// the community adapter's earned reliability_score (VERIF-03) and any
	// adapter's per-host marketplace reliability both land here, replacing the
	// fabricated 99.5/95 defaults with the real number.
	if o.ReliabilityScore != nil {
		avgUptime = *o.ReliabilityScore
	}

	// D-05: canonical region is computed once here so every provider
	// emits the same canonical
	// labels into the cache. D-06 defines the v1 sub-continental set; D-07
	// makes unmapped natives passthrough with a slog.Warn.
	canonicalRegion := Normalize(o.Region, o.DatacenterLocation, o.Provider)

	// Advertise nameplate VRAM for the card, not whichever upstream's
	// measurement we sampled: NormalizeVRAM lifts a near-canonical report
	// (nvidia-smi MiB→GiB, net of ECC) back to the spec-sheet figure while
	// leaving a genuinely smaller variant reported as-is. It also absorbs the
	// old safety net — an offering must never surface with 0 VRAM (renders as
	// "0 GB" in /v1/gpu-types and the dashboard).
	vramGB := provider.NormalizeVRAM(o.GPUType, o.VRAMPerGPUGB)

	return AvailableOffering{
		GPUModel:                    string(o.GPUType),
		GPUCount:                    o.GPUCount,
		VRAMGB:                      vramGB,
		CPUCores:                    o.CPUCores,
		RAMGB:                       o.RAMGB,
		StorageGB:                   o.StorageGB,
		PricePerHour:                retailPrice,
		Region:                      canonicalRegion,
		Tier:                        string(o.Tier),
		AvailableCount:              o.AvailableCount,
		StockStatus:                 o.StockStatus,
		AvgUptimePct:                avgUptime,
		InstantBoot:                 provider.InstantBoot(o.Provider),
		Community:                   capacityClass == provider.CapacityClassCommunity,
		CapacityClass:               capacityClass,
		ProvisioningEstimateMinutes: provisioningEstimateMinutes(o),
		CertifiedImage:              provider.CertifiedImageCapable(o.Provider),
		DeploymentType:              provider.DeploymentType(o.Provider),
		VMCapable:                   o.VMCapable,
		PartnerFeatured:             provider.FeaturedPartner(o.Provider),
		OfferingID:                  offeringID(o, canonicalRegion),
		OSOptions:                   vmOSOptions(o.Provider),
		InstanceDiskGB:              instanceDiskGB(o, defaultDiskGB),
		DiskConfigurable:            provider.ConfigurableDisk(o.Provider),
		DiskPricePerGBHour:          retailDiskRate(o.Provider, diskPricePerGBHour, markupPct, minMarkupPct),
	}
}

// retailDiskRate derives the published RETAIL per-GB-hour disk rate for one
// offering from the provider's wholesale rate. Non-configurable capacity has
// nothing to sell, and a rate of 0 means disk metering is off — both publish 0.
//
// The retail slope applies max(markupPct, minMarkupPct): RetailPrice is a
// clamp between the marked-up price and the floored price, so for any
// positive wholesale basis the branch with the larger percentage wins and
// every extra wholesale dollar is scaled by exactly that factor. Publishing
// the same slope keeps the UI's "+$/hr" deltas equal to the billed uplift up
// to the total's single cent rounding. The rate itself is deliberately NOT
// cent-rounded — it is a coefficient (~$0.0001), not a price.
func retailDiskRate(providerName string, wholesalePerGBHour, markupPct, minMarkupPct float64) float64 {
	if !provider.ConfigurableDisk(providerName) || wholesalePerGBHour <= 0 {
		return 0
	}
	effectivePct := max(markupPct, minMarkupPct)
	if effectivePct < 0 {
		// A negative markup (deliberate below-list retail, e.g. a partner
		// discount passed through) never scales disk below cost: the floor
		// branch of RetailPrice would catch the total anyway.
		effectivePct = 0
	}
	return wholesalePerGBHour * (1 + effectivePct/100)
}

// instanceDiskGB resolves the disk a launch of this offering actually gets.
//
// The discriminator is provider.ConfigurableDisk — the SAME predicate the
// provision engine narrows placement on — not the deployment type, so the
// catalog's promise and the engine's behavior cannot disagree:
//
//   - Configurable upstreams honor ProvisionRequest.ContainerDiskGB, which the
//     engine fills from config.LaunchDefaultDiskGB on every launch. The disk we
//     ask for is the disk that arrives, so publish that number.
//   - Everyone else decides the machine's disk themselves. A full-VM upstream
//     hands us its flavor's root volume; a container upstream outside the
//     allowlist drops the field on the floor (generic omits it from the
//     provision request entirely). The only honest answer is what the upstream
//     REPORTED — 0 when it reported nothing, published as unknown rather than
//     invented.
//
// It reads the raw o.StorageGB — since the configurable-disk work removed the
// 40 GB display fill, StorageGB and this field agree on unreported = 0.
//
// defaultDiskGB <= 0 — a struct-literal test poller, or any caller that predates
// the knob — degrades to provider.FallbackDiskGB, the same backstop the adapters
// apply when a request specifies nothing. Degraded still means "the number a
// launch would actually receive", never 0.
func instanceDiskGB(o provider.GPUOffering, defaultDiskGB int) int {
	if !provider.ConfigurableDisk(o.Provider) {
		return o.StorageGB
	}
	if defaultDiskGB <= 0 {
		return provider.FallbackDiskGB
	}
	return defaultDiskGB
}

// availManifest holds the embedded pinned-environment contract for the
// os_options stamp, loaded lazily on first use — the loadedManifest pattern
// from both API packages: the poller carries no manifest seam, and a malformed
// embedded manifest degrades to nil (rows simply carry no os_options; the
// engine-side vm_images gates refuse rather than guess) instead of failing the
// cache write. Boot-time validation elsewhere is the fail-fast guard.
var (
	availManifestOnce sync.Once
	availManifest     *manifest.Manifest
)

// vmOSOptions computes the raw-VM OS catalog ids for one offering at
// cache-write time — the ONLY place provider identity meets the vm_images map,
// so what survives into the cache is just our os-id list (D6). Non-VM
// providers return nil: an OS pick on a container offering is meaningless, and
// nil serializes away under omitempty.
func vmOSOptions(providerName string) []string {
	if provider.DeploymentType(providerName) != "vm" {
		return nil
	}
	availManifestOnce.Do(func() {
		if m, err := manifest.LoadEmbedded(); err == nil {
			availManifest = m
		}
	})
	if availManifest == nil {
		return nil
	}
	return availManifest.VMOSOptionsFor(providerName)
}

// offeringID derives an opaque, deterministic identifier for pinning an EXACT
// offering at launch (D-63-08). It is the first 12 hex chars of a SHA-256 over
// the stable offering attributes (provider name, GPU type, region, count, tier,
// capacity class, provider-native product id). The provider name and the native
// product id are HASHED IN, never emitted — the digest is a one-way function, so
// OfferingID carries no reversible provider identity and is safe past the
// defense-by-omission strip (T-63-04, D-63-08). Determinism means the same
// offering re-hashes to the same id across polls, so a launch can pin it and
// later re-resolve the exact capacity (or fail explicitly if gone).
//
// The RESOLVED capacity class joins the seed because it is a distinguishing
// attribute WITHIN one provider (a provider's secure and community rows for
// the same SKU/region are different products with different prices — one pin
// id for both would let a launch silently resolve to the other class).
// Resolving via CapacityClassFor keeps the id stable for
// adapters that never stamp the field.
//
// UpstreamGPUID — the provider-NATIVE product the adapter stamped — joins the
// seed for the same reason, one level finer (GPU-55). Class attributes alone
// cannot tell two spec-different, PRICE-TIED products apart: one provider's
// 4x A6000 high-performance plan (54 vCPU / 256 GB) and its plain 4x A6000 plan
// (30 / 192) were both rtx_a6000/4/us-central/on_demand/secure at 188¢, so they
// hashed to ONE id. If
// the advertised variant drained between the catalog view and the launch, the
// re-polled dedup winner was the sibling — the pin still matched and the engine
// forwarded the SIBLING's UpstreamGPUID, handing the customer a same-price,
// lower-spec node. That is exactly the silent substitution D-63-08 exists to
// forbid. The same ambiguity arises wherever an adapter's GPU-name mapping is
// many-to-one (A100 PCIe vs A100-SXM4, the three RTX PRO 6000 editions, …) and
// in vultr (plan ids), where the colliding rows carry DIFFERENT prices. Adapters
// that never stamp the field contribute "" and are byte-for-byte unchanged in
// behavior.
//
// Note each seed change rotates every offering id once at deploy, and the
// rotation window is the WHOLE rolling deploy (several minutes), not one poll
// cycle. A rolling deploy runs old and new replicas CONCURRENTLY; every task runs its own availability
// poller (cmd/gpuctl/serve.go starts it unconditionally — no leader lock) and
// they all overwrite the SAME Redis offerings key. The published snapshot
// therefore alternates between the old-id and new-id sets for the duration of
// the rollout, and the ALB routes each launch to either binary independently.
// A pinned launch taken during the rollout has roughly even odds of missing —
// expect a burst of explicit 409s (ErrOfferingUnavailable) across the whole
// rollout, which is the deploy signature, not an incident.
//
// What does NOT change: the failure stays EXPLICIT. Catalog and engine always
// recompute with the same function, so a mismatched pin fails rather than
// resolving to the wrong capacity, the 409 carries live alternatives, and a
// retry after the rollout settles succeeds. Nothing persists an offering id —
// the pin is request-scoped — so there is no stored id to migrate.
func offeringID(o provider.GPUOffering, canonicalRegion string) string {
	seed := fmt.Sprintf("%s|%s|%s|%d|%s|%s|%s",
		o.Provider,
		o.GPUType,
		canonicalRegion,
		o.GPUCount,
		o.Tier,
		provider.CapacityClassFor(o.CapacityClass, o.Provider),
		o.UpstreamGPUID,
	)
	sum := sha256.Sum256([]byte(seed))
	return hex.EncodeToString(sum[:])[:12]
}

// OfferingID exposes the deterministic, provider-anonymized offering identifier
// (see offeringID) to the placement engine so a launch can pin an EXACT offering
// (D-63-08). The engine passes the offering plus the canonical region it already
// computes via Normalize, guaranteeing the id matches the one the catalog handed
// the client. The digest hashes provider identity IN but never emits it, so it is
// safe to compare past the defense-by-omission strip (T-63-04).
func OfferingID(o provider.GPUOffering, canonicalRegion string) string {
	return offeringID(o, canonicalRegion)
}

// provisioningEstimateMinutes resolves an offering's create→ready estimate in
// minutes, preferring the adapter's per-offering value (set only where boot
// time varies by offering, e.g. virtual vs bare-metal flavors of one provider) and falling back
// to the per-provider default (provider.ProvisioningEstimate) rounded up to
// the minute.
func provisioningEstimateMinutes(o provider.GPUOffering) int {
	if o.ProvisioningEstimateMinutes > 0 {
		return o.ProvisioningEstimateMinutes
	}
	return int(math.Ceil(provider.ProvisioningEstimate(o.Provider).Minutes()))
}
