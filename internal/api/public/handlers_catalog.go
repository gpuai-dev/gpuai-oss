package public

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gpuai-dev/gpuai-oss/internal/availability"
	"github.com/gpuai-dev/gpuai-oss/internal/manifest"
)

// catalogManifest holds the embedded pinned-environment contract (D-63-07),
// loaded lazily on first use. PublicDeps carries no manifest seam and the
// catalog tests inject none, so the handler self-loads the binary-embedded
// manifest here. A malformed embedded manifest degrades to nil (certified rows
// simply omit the environment block) rather than failing the catalog — boot-time
// validation elsewhere is the fail-fast guard for a broken manifest.
var (
	catalogManifestOnce sync.Once
	catalogManifest     *manifest.Manifest
)

func loadedManifest() *manifest.Manifest {
	catalogManifestOnce.Do(func() {
		if m, err := manifest.LoadEmbedded(); err == nil {
			catalogManifest = m
		}
	})
	return catalogManifest
}

// PricingEnvironment is the published pinned-environment contract for a catalog
// row (D-63-07): the digest-pinned certified image plus the driver/CUDA floors
// the server guarantees for that GPU model. Every field is server-derived from
// the manifest — never customer input, and NEVER a provider identity or an
// internal-only manifest field (T-63-04 defense by omission).
type PricingEnvironment struct {
	Image       string `json:"image"`        // human-readable tag-form ref (digest stripped)
	ImageDigest string `json:"image_digest"` // the digest-pinned ref the launch path actually pins
	DriverMin   string `json:"driver_min"`   // minimum NVIDIA driver, e.g. "570.26"
	CUDA        string `json:"cuda"`         // CUDA toolkit generation, e.g. "12.8"
}

// environmentFor resolves the published environment contract for a certified
// offering's GPU model, or nil when the model has no manifest entry (or the
// embedded manifest failed to load). The cache stores gpu_type in snake_case
// ("h100_sxm") while the manifest keys are display strings ("H100 SXM"); the
// manifest's Lookup fallback is case/whitespace-insensitive but does NOT map
// underscores to spaces, so we bridge that here before the lookup. Only the
// four customer-safe fields cross the wire — never provider identity or any
// other internal manifest field (T-63-04 defense by omission).
func environmentFor(gpuModel string) *PricingEnvironment {
	m := loadedManifest()
	if m == nil {
		return nil
	}
	env, ok := m.Lookup(strings.ReplaceAll(gpuModel, "_", " "))
	if !ok {
		return nil
	}
	// ImageRef is digest-pinned ("...:TAG@sha256:..."); the tag-form image drops
	// the "@sha256:..." suffix while image_digest keeps the full pinned ref.
	image := env.ImageRef
	if at := strings.Index(image, "@"); at >= 0 {
		image = image[:at]
	}
	return &PricingEnvironment{
		Image:       image,
		ImageDigest: env.ImageRef,
		DriverMin:   env.DriverFloor,
		CUDA:        env.CUDAMin,
	}
}

// GPUType is the public catalog's per-gpu_type response shape (one row per
// distinct GPU model). PRIV-08 contract: this struct intentionally omits
// every upstream-leaking field — provider identity, datacenter, internal  // PRIVACY_CI_ALLOW: PRIV-08 doc enumerates omitted fields
// tunnel keys — so json.Marshal cannot emit them by accident. The privacy
// regression test (TestNoUpstreamLeakage_Catalog) enforces this at CI time.
//
// Note on Architecture: the source AvailableOffering struct does not carry
// an architecture string today, so the field is omitted from the wire shape
// rather than serialized as an empty string. A future plan may add it once
// the upstream cache record is extended; the omitempty tag-pattern is not
// used here because struct absence is stronger than `,omitempty`.
type GPUType struct {
	GPUType string `json:"gpu_type"`
	VRAMGB  int    `json:"vram_gb"`
	// CPUCores, RAMGB and StorageGB are the host hardware that ships alongside
	// this GPU model. Unlike VRAM (an invariant property of the silicon) these
	// vary per offering/host, so the value here is REPRESENTATIVE — the
	// first-sighting config for the model (see handleListGPUTypes). GET
	// /v1/pricing carries the exact per-offering specs. All three are plain
	// integers carrying no provider identity, so they survive the PRIV-08 strip;
	// 0 marks a spec the upstream did not report (the CLI renders it as "-").
	CPUCores  int `json:"cpu_cores"`
	RAMGB     int `json:"ram_gb"`
	StorageGB int `json:"storage_gb"`
	// InstanceDiskGB is the filesystem size a launch of this model receives,
	// REPRESENTATIVE in the same sense as CPUCores/RAMGB above (first-sighting
	// for the model); /v1/pricing carries the exact per-offering value. 0 marks
	// an upstream that reported no disk.
	InstanceDiskGB int `json:"instance_disk_gb"`
	// DiskConfigurable reports whether ANY offering of this model accepts a
	// custom disk_gb — an OR across sightings, deliberately NOT the
	// first-sighting rule the specs above use. A model is served by several
	// upstreams with different capabilities, and requesting disk_gb narrows
	// placement to the capable ones, so "at least one" is exactly the question a
	// caller is asking before it sends the field. First-sighting would answer a
	// coin flip instead, hiding a real capability half the time.
	DiskConfigurable bool `json:"disk_configurable"`
}

// Pricing is the public catalog's per-offer response shape. Same privacy
// contract as GPUType. Offers indistinguishable to the customer
// (same gpu_type, gpu_count, region, tier, boot class) are aggregated into one
// row — the cheapest price wins and availability is summed (issue #102) — so a
// single SKU never returns multiple rows that differ only by price with no way
// to tell them apart. instant_boot remains a distinguishing field: a fast-start
// and a slow-start offer for the same SKU are still separate rows.
//
// GPUCount is the number of GPUs in the listed configuration; PricePerHour
// covers the WHOLE configuration (an 8×A100 node lists ~8× the 1×A100
// price), so rows must be compared per (gpu_count, price) pair — not by
// price alone.
//
// InstantBoot classifies boot speed (fast container start vs ~3-5 min
// full-VM boot) without naming the upstream — it is a provider ATTRIBUTE,
// not provider identity, and is the field CLI/SDK consumers filter on to
// skip slow-start capacity.
type Pricing struct {
	GPUType  string `json:"gpu_type"`
	GPUCount int    `json:"gpu_count"`
	// CPUCores, RAMGB and StorageGB are the host hardware for THIS offering —
	// the exact spec of the machine a launch of this row provisions. They track
	// the offer whose price is shown (cheapest-wins): when offers merge, the
	// winning offer's specs are surfaced so specs and price always describe the
	// same machine. Plain integers carrying no provider identity, so they
	// survive the PRIV-08 strip; 0 marks a spec the upstream did not report.
	CPUCores     int     `json:"cpu_cores"`
	RAMGB        int     `json:"ram_gb"`
	StorageGB    int     `json:"storage_gb"`
	Region       string  `json:"region"`
	Tier         string  `json:"tier"`
	PricePerHour float64 `json:"price_per_hour"`
	Available    int     `json:"available"`
	InstantBoot  bool    `json:"instant_boot"`
	// Community marks community-supplied capacity (supplier-owned hardware) —
	// a provider ATTRIBUTE, not provider identity, like InstantBoot. The field
	// is named `community` (never `supplier_*`, which trips bannedKeysRe); it is
	// a distinguishing field, so community and hosted-provider offerings for the
	// same SKU stay separate, distinguishable rows.
	Community bool `json:"community"`
	// CapacityClass is the per-offering secure/community classification
	// ("secure" | "community"), the string twin of Community — per OFFERING,
	// not per provider, because some upstreams price both secure and
	// peer-host capacity for the same SKU. "" marks a pre-field cache entry
	// (self-heals within one poll cycle). Like Community it is an attribute,
	// never provider identity, and it is a distinguishing field in the merge.
	CapacityClass string `json:"capacity_class"`
	// CertifiedImage reports whether this offering can run the GPU.ai Certified
	// Image (a provider CLASS, not identity — survives the privacy strip). It is
	// a distinguishing field: a certified and a non-certified offering for the
	// same SKU stay separate rows (Pitfall 1 — silent collapse).
	CertifiedImage bool `json:"certified_image"`
	// DeploymentType is "vm" (we bootstrap the machine ourselves) or "container"
	// (the provider runs our image natively); "" marks a pre-field cache entry.
	// Also a distinguishing field for the same reason as CertifiedImage.
	//
	// The classification is a customer-visible CAPABILITY, not trivia: a
	// container offering runs the workload AS an unprivileged container, so
	// Docker cannot run inside it (no Docker-in-Docker, no --privileged);
	// vm offerings give root on the machine and can.
	DeploymentType string `json:"deployment_type"`
	// InstanceDiskGB is the filesystem size a launch of THIS row receives — the
	// platform's effective default where the upstream honors the disk we ask for
	// (see DiskConfigurable), otherwise the machine's own reported disk, and 0
	// when the upstream reported none. Like the specs above it tracks the offer
	// whose price is shown (cheapest-wins), so disk and price always describe the
	// same machine.
	InstanceDiskGB int `json:"instance_disk_gb"`
	// DiskConfigurable reports whether a launch of this row may choose its own
	// disk size (a provider CLASS, not identity — survives the privacy strip).
	// It is a distinguishing field for the same reason as CertifiedImage: the
	// container providers split on it (some honor a requested disk, some drop it
	// on the floor), so merging across it would stamp "you may pick your disk"
	// onto capacity that cannot, and the UI would render a control that fails on
	// submit (Pitfall 1 — silent collapse).
	DiskConfigurable bool `json:"disk_configurable"`
	// DiskPricePerGBHour is the retail price of one GB of instance disk beyond
	// InstanceDiskGB, per hour, folded into the launch's single hourly rate
	// (see the create request's disk_gb). Like the specs above it tracks the
	// offer whose price is shown. 0 = larger sizes are currently free, or the
	// row is not disk-configurable. A rate, not provider identity — survives
	// the privacy strip like InstantBoot.
	DiskPricePerGBHour float64 `json:"disk_price_per_gb_hour"`
	// OfferingID is the opaque, deterministic pin target for an EXACT offering at
	// launch (D-63-08). It is a one-way hash — it carries no provider identity.
	OfferingID string `json:"offering_id"`
	// Environment is the published pinned-environment contract (D-63-07), present
	// only for certified-capable rows; nil otherwise (raw-VM / provider-template
	// paths omit the block entirely rather than emitting empty strings).
	Environment *PricingEnvironment `json:"environment,omitempty"`
}

// RegisterCatalogRoutes wires the catalog endpoints onto mux. router.go
// calls this once. Per the Wave 5 router-serialization convention, each
// handler module owns its own Register*Routes helper so router.go remains
// a thin aggregator and parallel plans never collide on its body.
//
// Both routes are wrapped in RequestIDMiddleware so X-Request-Id is always
// present on the response, matching the rest of the public API.
func RegisterCatalogRoutes(mux *http.ServeMux, deps PublicDeps) {
	mux.Handle("GET /v1/gpu-types", RequestIDMiddleware(http.HandlerFunc(deps.handleListGPUTypes)))
	mux.Handle("GET /v1/pricing", RequestIDMiddleware(http.HandlerFunc(deps.handleListPricing)))
	mux.Handle("GET /v1/environments", RequestIDMiddleware(http.HandlerFunc(deps.handleListEnvironments)))
}

// handleListGPUTypes serves GET /v1/gpu-types (no auth — public catalog).
//
// Reads the aggregated AvailableOffering snapshot from the availability
// cache, deduplicates to one row per gpu_model (VRAM = nameplate, or the
// smallest currently-offered variant; see the dedupe comment below), sorts
// ascending by gpu_type, and applies cursor pagination on the gpu_type sort
// key.
//
// On cache read error → 503 internal_error. The endpoint deliberately does
// NOT inspect the Authorization header; the must_haves contract pins it
// open so SDK consumers can introspect available GPUs before authenticating.
func (deps PublicDeps) handleListGPUTypes(w http.ResponseWriter, r *http.Request) {
	params := ParsePageParams(r)

	offerings, err := deps.AvailCache.Read(r.Context())
	if err != nil {
		slog.ErrorContext(r.Context(), "gpu-types: cache read failed", "error", err.Error())
		WriteProblem(w, r, http.StatusServiceUnavailable, "internal_error", "GPU catalog unavailable")
		return
	}

	// Dedupe to one row per gpu_model, first-sighting wins. CPU/RAM/storage
	// vary per offering/host, so the first-sighting value is REPRESENTATIVE of
	// the model rather than exact; /v1/pricing carries the precise
	// per-offering specs. instance_disk_gb joins them on the same terms.
	//
	// VRAM is NOT first-sighting: it is the MINIMUM across every sighting of
	// the model. VRAM should be invariant per model, but upstreams do drift —
	// v100 covers 16GB and 32GB silicon, and supplier feeds self-report — and
	// a first-sighting pick both flaps with snapshot order and can advertise
	// more VRAM than the offering a launch actually lands on delivers (the
	// 2026-08-12 "reported 32GB, got 16GB" support ticket). The minimum is the
	// only value every launch of the model can honor.
	//
	// disk_configurable is ORed across every sighting of the model, because it
	// answers a capability question ("may I send disk_gb for this model?")
	// that a coin-flip sighting would get wrong half the time. See the field
	// comment on GPUType.
	seen := map[string]GPUType{}
	for _, o := range offerings {
		t, ok := seen[o.GPUModel]
		if !ok {
			t = GPUType{
				GPUType:        o.GPUModel,
				VRAMGB:         o.VRAMGB,
				CPUCores:       o.CPUCores,
				RAMGB:          o.RAMGB,
				StorageGB:      o.StorageGB,
				InstanceDiskGB: o.InstanceDiskGB,
			}
		}
		if o.VRAMGB > 0 && (t.VRAMGB == 0 || o.VRAMGB < t.VRAMGB) {
			t.VRAMGB = o.VRAMGB
		}
		t.DiskConfigurable = t.DiskConfigurable || o.DiskConfigurable
		seen[o.GPUModel] = t
	}
	rows := make([]GPUType, 0, len(seen))
	for _, t := range seen {
		rows = append(rows, t)
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].GPUType < rows[j].GPUType })

	// Cursor: GPU types have no meaningful chronology, so the timestamp is
	// synthetic (time.Now). Only the id portion (gpu_type string) carries
	// real ordering signal — handler skips rows whose id <= cursor.id.
	if params.Cursor != "" {
		_, after, derr := DecodeCursor(params.Cursor)
		if derr != nil {
			WriteProblem(w, r, http.StatusBadRequest, "validation_failed", "Invalid cursor")
			return
		}
		filtered := rows[:0]
		for _, t := range rows {
			if t.GPUType > after {
				filtered = append(filtered, t)
			}
		}
		rows = filtered
	}
	page := BuildPage(rows, params.Limit, func(t GPUType) (time.Time, string) {
		return time.Now().UTC(), t.GPUType
	})
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(page)
}

// pricingCursorKey returns the row's unique identity string, used both as
// the sort key and the cursor id. Numeric fields are zero-padded to fixed
// width so lexicographic comparison matches numeric ordering — this is what
// makes `key > after` correct at page boundaries.
//
// The old 3-part key (gpu_type|region|tier) was NOT unique per row: every
// row sharing the last-returned row's key was silently dropped from the
// next page (ties filtered by the strict `>`). Including gpu_count, price
// and boot class makes the key unique post-merge, so pagination is lossless.
// Old 3-part (and 6-part pre-community) cursors still decode; they compare as a
// strict prefix of the current key, so the worst case for an in-flight stale
// cursor is a repeated row, never a lost one. The trailing segments are boot
// (instant_boot), comm (community), then the capacity class string — each
// appended in turn so older cursors remain prefix-compatible. The class is
// needed beyond comm only for the pre-field cache window, where a "" class
// row and a resolved-class row share the same comm bit but are distinct rows.
func pricingCursorKey(p Pricing) string {
	boot, comm := "0", "0"
	if p.InstantBoot {
		boot = "1"
	}
	if p.Community {
		comm = "1"
	}
	return fmt.Sprintf("%s|%s|%s|%04d|%013.2f|%s|%s|%s",
		p.GPUType, p.Region, p.Tier, p.GPUCount, p.PricePerHour, boot, comm, p.CapacityClass)
}

// handleListPricing serves GET /v1/pricing (no auth).
//
// Offerings identical from the customer's point of view (same gpu_type,
// gpu_count, region, tier, boot class) are aggregated into one row — cheapest
// price wins, availability summed (issue #102) — so the same SKU never returns
// duplicate rows differing only by price. Different node configs of the same
// GPU stay separate (gpu_count differs), as do fast- vs slow-boot offers
// (instant_boot differs). Rows with zero availability are dropped unless
// ?include_unavailable=true, so the default response only lists offers a
// customer can actually provision right now.
//
// Optional exact-match filters: ?gpu_type= ?region= ?tier= ?deployment_type=
// ?certified_image= (the gpu CLI has always sent these; they are honored
// server-side as of this handler). Sort + cursor use pricingCursorKey (above).
//
// deployment_type and certified_image are strict enums (T-q82-01): an
// unrecognized value is a 400 rather than a silently-empty page, so a typo'd
// `--type vmm` cannot read as "no such capacity". Both are validated BEFORE
// the cache read.
func (deps PublicDeps) handleListPricing(w http.ResponseWriter, r *http.Request) {
	params := ParsePageParams(r)
	q := r.URL.Query()
	fGPUType := q.Get("gpu_type")
	fRegion := q.Get("region")
	fTier := q.Get("tier")
	includeUnavailable := q.Get("include_unavailable") == "true" || q.Get("include_unavailable") == "1"

	// fDeploymentType is "" (no filter), "vm" or "container". Customers pick on
	// this field because it decides whether their workload can run its own
	// Docker, so an invalid value must fail loudly.
	fDeploymentType := q.Get("deployment_type")
	if fDeploymentType != "" && fDeploymentType != "vm" && fDeploymentType != "container" {
		WriteProblem(w, r, http.StatusBadRequest, "validation_failed", "Invalid deployment_type: must be vm or container")
		return
	}

	// fCertifiedImage is nil when unset; otherwise the required value. Parsed
	// strictly ("true"/"false" only) rather than via the loose ==\"true\"
	// convention used for include_unavailable, because a mistyped value here
	// silently widens the result set instead of narrowing it.
	var fCertifiedImage *bool
	if v := q.Get("certified_image"); v != "" {
		switch v {
		case "true":
			t := true
			fCertifiedImage = &t
		case "false":
			f := false
			fCertifiedImage = &f
		default:
			WriteProblem(w, r, http.StatusBadRequest, "validation_failed", "Invalid certified_image: must be true or false")
			return
		}
	}

	offerings, err := deps.AvailCache.Read(r.Context())
	if err != nil {
		slog.ErrorContext(r.Context(), "pricing: cache read failed", "error", err.Error())
		WriteProblem(w, r, http.StatusServiceUnavailable, "internal_error", "Pricing catalog unavailable")
		return
	}

	// Query filters run BEFORE the merge so an aggregate never mixes rows the
	// caller excluded. A deployment_type filter deliberately excludes
	// ""-classified pre-field cache rows: the internal dashboard endpoint drops
	// UNKNOWN rows under the same filter, and surfacing an unclassified row
	// under `--type vm` would answer "yes, this can run Docker" without knowing.
	selected := make([]availability.AvailableOffering, 0, len(offerings))
	for _, o := range offerings {
		if fGPUType != "" && o.GPUModel != fGPUType {
			continue
		}
		if fRegion != "" && o.Region != fRegion {
			continue
		}
		if fTier != "" && o.Tier != fTier {
			continue
		}
		if fDeploymentType != "" && o.DeploymentType != fDeploymentType {
			continue
		}
		if fCertifiedImage != nil && o.CertifiedImage != *fCertifiedImage {
			continue
		}
		selected = append(selected, o)
	}

	rows := mergePricingOffers(selected, includeUnavailable)
	sort.Slice(rows, func(i, j int) bool {
		return pricingCursorKey(rows[i]) < pricingCursorKey(rows[j])
	})
	if params.Cursor != "" {
		_, after, derr := DecodeCursor(params.Cursor)
		if derr != nil {
			WriteProblem(w, r, http.StatusBadRequest, "validation_failed", "Invalid cursor")
			return
		}
		filtered := rows[:0]
		for _, p := range rows {
			if pricingCursorKey(p) > after {
				filtered = append(filtered, p)
			}
		}
		rows = filtered
	}
	page := BuildPage(rows, params.Limit, func(p Pricing) (time.Time, string) {
		return time.Now().UTC(), pricingCursorKey(p)
	})
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(page)
}

// pricingOfferKey is the customer-visible identity of a /v1/pricing row.
//
// Offers that are indistinguishable to the customer aggregate into ONE row per
// (gpu_type, gpu_count, region, tier, instant_boot, community): the CHEAPEST
// price wins and availability is summed (issue #102). price is deliberately NOT
// in the key — two offers differing only by price used to surface as duplicate
// rows the customer couldn't tell apart. instant_boot and community STAY in the
// key: both are real, customer-visible attributes, so a fast-start vs slow-start
// — or a community vs hosted-provider — offer for the same SKU remain separate,
// distinguishable rows (badge integrity, Pitfall 1: community must also join
// pricingCursorKey).
//
// certified_image and deployment_type ALSO join the key: both are real,
// customer-visible attributes, so a certified vs non-certified — or a vm vs
// container — offer for the same SKU must stay separate rows. Omitting them
// would silently collapse two genuinely different products onto one row and
// stamp one row's descriptors (and environment contract) onto the other
// (Pitfall 1 — the silent-collapse hazard PATTERNS.md flags).
//
// disk_configurable joins the key on exactly the same grounds, and the collapse
// it prevents is live rather than hypothetical: the container providers split on
// this capability, so without it in the key a row whose price comes from
// capacity that ignores a requested disk could still advertise a disk control —
// which then fails on submit.
//
// offering_id deliberately does NOT join the key: it is the pin token, not a
// customer-visible attribute, and grouping by it would split one card into a row
// per underlying product. Row cardinality is a function of this key ALONE, which
// is why folding UpstreamGPUID into the id (GPU-55) cannot introduce duplicate
// rows.
type pricingOfferKey struct {
	gpuType     string
	gpuCount    int
	region      string
	tier        string
	instantBoot bool
	community   bool
	// capacityClass joins the key alongside its bool twin `community` so
	// a pre-field cache entry ("") never merges with a fresh row of the
	// same community value and silently stamps "" onto it.
	capacityClass    string
	certifiedImage   bool
	deploymentType   string
	diskConfigurable bool
}

// pricingOfferAgg accumulates the offers merged under one pricingOfferKey.
type pricingOfferAgg struct {
	price       float64 // lowest POSITIVE price; 0 only if no positive offer exists
	available   int     // summed across every merged offer
	offeringID  string  // pin target (D-63-08) for the offer whose price/specs this row publishes (GPU-55)
	environment *PricingEnvironment
	// cpuCores/ramGB/storageGB/instanceDiskGB describe the offer whose price
	// is shown: seeded on first sighting, overwritten whenever a cheaper
	// positive price wins, so the surfaced specs always match the machine at
	// `price`. instanceDiskGB rides with them rather than joining the key
	// because it is a per-machine NUMBER, not a capability class — two offers
	// that differ only in reported disk are still the same product, and the one
	// we price is the one whose disk we publish.
	cpuCores       int
	ramGB          int
	storageGB      int
	instanceDiskGB int
	diskPriceRate  float64
	// winnerAvailable is the PRICED offer's own AvailableCount, kept apart from
	// the summed `available` the row publishes: the exact-price tie-break
	// compares one candidate machine against the current winner, and comparing
	// against a running total would make the outcome depend on how many offers
	// happened to merge before it.
	winnerAvailable int
}

// pricingTieBreak reports whether candidate o should displace the current winner
// when the two are at the EXACT same price. Something must break the tie
// deterministically — the poller appends per-provider results in
// goroutine-completion order (availability.Poller.collectOfferings), so
// "first seen" was never stable across cycles and the published row could flip
// for no reason.
//
// The order is CUSTOMER-FAVORABLE first, arbitrary-but-total last. At an
// identical price the same silicon on a richer host is strictly better, so more
// RAM wins, then more vCPU, then the deeper stock (a launch is likelier to
// succeed and the pin likelier to survive to the next poll). Only when the
// machines are indistinguishable on all three does the offering id decide, and
// then purely to guarantee a TOTAL order.
//
// This is deliberately the same ladder adapters use to rank products
// with (productChoice.preferredOver: cheapest, then more RAM, then more vCPU,
// then a stable final key). Advertise-side and launch-side must not rank
// differently — the moment they do, the catalog card and the launched product
// diverge again, which is the class of bug GPU-55 and PR #634 both closed.
//
// Without this the motivating case ranked backwards: two price-tied A6000 nodes,
// 54 vCPU / 256 GB vs 30 / 192, would publish and pin whichever hash sorted
// lower — potentially advertising the LOWER-spec machine at the same price.
func pricingTieBreak(o availability.AvailableOffering, a *pricingOfferAgg) bool {
	if o.RAMGB != a.ramGB {
		return o.RAMGB > a.ramGB
	}
	if o.CPUCores != a.cpuCores {
		return o.CPUCores > a.cpuCores
	}
	if o.AvailableCount != a.winnerAvailable {
		return o.AvailableCount > a.winnerAvailable
	}
	return o.OfferingID < a.offeringID
}

// mergePricingOffers projects already-filtered cache rows onto the /v1/pricing
// wire shape: one row per pricingOfferKey, cheapest positive price wins, counts
// summed, and — since GPU-55 — the published price, specs AND pin all taken from
// the SAME offer. Rows with no live capacity are dropped unless the caller asked
// to include them. Order is unspecified; the handler sorts.
//
// Split out of the handler so the projection is directly testable without a
// Redis-backed cache: the aggregation is the piece with the invariants.
func mergePricingOffers(offerings []availability.AvailableOffering, includeUnavailable bool) []Pricing {
	merged := map[pricingOfferKey]*pricingOfferAgg{}
	for _, o := range offerings {
		k := pricingOfferKey{o.GPUModel, o.GPUCount, o.Region, o.Tier, o.InstantBoot, o.Community, o.CapacityClass, o.CertifiedImage, o.DeploymentType, o.DiskConfigurable}
		a := merged[k]
		if a == nil {
			a = &pricingOfferAgg{}
			// First sighting SEEDS the pin and (for certified-capable offerings
			// only) the published environment contract. The pin is replaced
			// below by whichever offer's price the row ends up publishing —
			// first-seen is only a starting value, never the answer (GPU-55).
			a.offeringID = o.OfferingID
			if o.CertifiedImage {
				a.environment = environmentFor(o.GPUModel)
			}
			// Seed the hardware specs so a group with only $0 offers still
			// carries one; the cheapest-positive branch below overwrites them so
			// specs stay tied to the offer whose price we surface.
			a.cpuCores, a.ramGB, a.storageGB, a.instanceDiskGB = o.CPUCores, o.RAMGB, o.StorageGB, o.InstanceDiskGB
			a.diskPriceRate = o.DiskPricePerGBHour
			a.winnerAvailable = o.AvailableCount
			merged[k] = a
		}
		a.available += o.AvailableCount
		// Cheapest-wins, but never let a $0/non-positive sibling (a dedicated-only
		// or reserved-style listing) shadow a real price — prefer the lowest
		// positive price, leaving 0 only when no offer in the group has one.
		//
		// The PIN moves with the price (GPU-55). Offers merged under one key are
		// customer-INDISTINGUISHABLE, not identical: they can be different
		// upstream products at different prices — two hardware variants that
		// normalize to one canonical GPU type in one region, or two node plans of
		// the same size from one supplier. Publishing a first-seen pin next to a
		// cheapest-wins price meant the token could name a machine that is not the
		// one whose price and specs the row advertises — and once the offering id
		// folds in the native product (UpstreamGPUID) that mismatch stops being
		// masked by a shared id and becomes a real "pinned a pricier machine than
		// displayed" bug. Binding price, specs and pin to one offer is the only
		// coherent contract.
		//
		// An EXACT price tie is broken by pricingTieBreak — customer-favorable
		// first, deterministic last (see its doc).
		promote := false
		switch {
		case a.price <= 0 && o.PricePerHour > 0:
			promote = true // first real price in the group
		case a.price <= 0 && o.PricePerHour <= 0:
			promote = pricingTieBreak(o, a) // no real price anywhere yet
		case o.PricePerHour > 0 && o.PricePerHour < a.price:
			promote = true
		case o.PricePerHour > 0 && o.PricePerHour == a.price:
			promote = pricingTieBreak(o, a)
		}
		if promote {
			a.price = o.PricePerHour
			a.offeringID = o.OfferingID
			a.cpuCores, a.ramGB, a.storageGB, a.instanceDiskGB = o.CPUCores, o.RAMGB, o.StorageGB, o.InstanceDiskGB
			a.diskPriceRate = o.DiskPricePerGBHour
			a.winnerAvailable = o.AvailableCount
		}
	}

	rows := make([]Pricing, 0, len(merged))
	for k, a := range merged {
		if a.available == 0 && !includeUnavailable {
			continue
		}
		rows = append(rows, Pricing{
			GPUType:            k.gpuType,
			GPUCount:           k.gpuCount,
			CPUCores:           a.cpuCores,
			RAMGB:              a.ramGB,
			StorageGB:          a.storageGB,
			Region:             k.region,
			Tier:               k.tier,
			PricePerHour:       a.price,
			Available:          a.available,
			InstantBoot:        k.instantBoot,
			Community:          k.community,
			CapacityClass:      k.capacityClass,
			CertifiedImage:     k.certifiedImage,
			DeploymentType:     k.deploymentType,
			InstanceDiskGB:     a.instanceDiskGB,
			DiskConfigurable:   k.diskConfigurable,
			DiskPricePerGBHour: a.diskPriceRate,
			OfferingID:         a.offeringID,
			Environment:        a.environment,
		})
	}
	return rows
}
