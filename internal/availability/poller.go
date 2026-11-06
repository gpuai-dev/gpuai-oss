package availability

import (
	"context"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gpuai-dev/gpuai-oss/internal/manifest"
	"github.com/gpuai-dev/gpuai-oss/internal/provider"
)

// DefaultProbeTimeout caps the time any single ListAvailable call may take,
// inclusive of provider-internal retries. Matches the BREAKER_PROBE_TIMEOUT
// knob default (10s).
const DefaultProbeTimeout = 10 * time.Second

// Poller queries all registered providers for GPU availability on a fixed interval
// and writes aggregated results to the Redis cache.
type Poller struct {
	registry     *provider.Registry
	cache        *Cache
	interval     time.Duration
	probeTimeout time.Duration
	markupFor    func(providerName string) float64
	// costDiscountFor resolves a provider's partner cost discount percentage
	// (wired via WithCostDiscountFor; serve.go resolves it DB-over-env through
	// the admin.Store seam, so a Suppliers-panel edit moves this within ~30s):
	// the amount a partner invoices us BELOW the list price their API reports.
	//
	// Since v85 it is the BASIS retail is computed from, not merely a rebased
	// floor — retail = true cost x (1 + markup) — so a discount reaches the
	// customer on its own rather than needing a matching negative markup. nil
	// (the zero value, struct-literal test pollers) means no discount anywhere.
	costDiscountFor func(providerName string) float64
	// minRetailMarkupPct is the retail floor markup percentage
	// (config.MinRetailMarkupPct, wired via WithMinRetailMarkup): no offering
	// is published with retail < wholesale × (1 + this/100), rounded up to
	// the cent. The zero value (struct-literal test pollers) still floors at
	// bare wholesale — degraded means "never below cost", not "no floor".
	minRetailMarkupPct float64
	// minMarkupFor resolves the PER-PROVIDER retail-floor margin percentage
	// (admin.Store.MinMarginPctFor, wired via WithMinRetailMarkupFor; serve.go
	// always wires it). It may return NEGATIVE values — the per-supplier LOSS
	// FLOOR, an operator's explicit sanction to publish that supplier's rows
	// below cost by at most that much. nil (struct-literal test pollers) falls
	// back to the scalar minRetailMarkupPct above, which can never be negative.
	minMarkupFor func(providerName string) float64
	// defaultInstanceDiskGB is the platform's effective launch disk
	// (config.LaunchDefaultDiskGB, wired via WithDefaultInstanceDisk) — what
	// AvailableOffering.InstanceDiskGB publishes for offerings whose upstream
	// honors the disk we ask for, so the catalog's promised disk is the same
	// number the provision engine hands the adapter. The zero value
	// (struct-literal test pollers) degrades to provider.FallbackDiskGB rather
	// than publishing 0 (see instanceDiskGB).
	defaultInstanceDiskGB int
	// diskPriceFor resolves a provider's WHOLESALE disk rate in $/GB/hr
	// (config.DiskPricePerGBHourFor, wired via WithDiskPriceFor) — the input
	// to the published retail disk rate (AvailableOffering.DiskPricePerGBHour).
	// nil (struct-literal test pollers) resolves to 0: no rate published,
	// matching disk metering switched off.
	diskPriceFor func(providerName string) float64
	isDisabled   func(providerName string) bool
	breaker      *Breaker
	// launchBreaker is the launch-outcome breaker (launchbreaker.go), shared
	// with the provision engine. Non-Healthy (provider, gpu_type) keys are
	// dropped from the customer catalog at conversion time — the row
	// vanishes, exactly like zero stock. Read-only here (State): probe
	// admission is the ENGINE's job at placement; the catalog must never
	// mutate breaker state.
	launchBreaker *Breaker
	// fundingGate is the provider-level "account unfunded" circuit
	// (fundinggate.go), shared with the provision engine and the supplier
	// balance poller. A suspended provider is skipped like a disabled one, so
	// an empty supplier account stops being sold within one poll. nil
	// suspends nothing.
	fundingGate *FundingGate
	// verdicts is the fleet prober's per-offering verdict source
	// (internal/fleetprobe). nil hides nothing. In shadow mode delisted rows
	// are only counted and logged; in enforce mode they leave the customer
	// catalog like zero stock (the staff twin keeps them visible).
	verdicts        OfferingVerdicts
	enforceVerdicts bool
	// placement hides offerings that keep failing to place
	// (PlacementMemory). nil hides nothing.
	placement *PlacementMemory
	logger    *slog.Logger
	// retention bounds how long a provider's last successful (converted)
	// snapshot may be served in place of a failed or breaker-skipped poll,
	// so one slow upstream response doesn't blank that provider out of both
	// catalogs for a cycle (the customer cache and the staff twin are
	// rewritten wholesale every poll). Zero (struct-literal test pollers)
	// disables retention entirely: failures clear the provider's rows within
	// one cycle, the pre-retention behavior. serve.go always wires
	// WithRetention from AVAILABILITY_RETENTION_SECONDS.
	retention time.Duration
	// lastGood holds each provider's most recent successful snapshot,
	// guarded by lastGoodMu (collectOfferings polls providers concurrently).
	// Entries are dropped on expiry and on operator disable — the
	// PROVIDERS_DISABLED kill-switch must clear a supplier within one poll,
	// retention or not.
	lastGoodMu sync.Mutex
	lastGood   map[string]lastGoodSnapshot
	// now is the retention clock; nil means time.Now (tests inject a fake).
	now func() time.Time
	// manifest is the pinned-base-environment source (Phase 63). Optional and
	// currently a reachability seam only — the cache-write stamp is derived from
	// provider classification, not the manifest — but threading it here lets a
	// later wave resolve per-GPU driver floors / certified image refs during the
	// poll without another constructor change. nil is safe.
	manifest *manifest.Manifest
}

// lastGoodSnapshot is one provider's most recent successfully-converted rows
// (customer and staff twins from the same pass, so retention can never make
// the two views drift) plus when they were taken.
type lastGoodSnapshot struct {
	offerings []AvailableOffering
	// held is the customer rows the placement memory held back that pass.
	// Kept so a retained cycle can re-check them against the memory and
	// restoreLastOfShape still sees them; without them a GPU shape whose only
	// row is held vanished from the catalog while the provider's poll failed.
	held    []AvailableOffering
	staff   []StaffOffering
	takenAt time.Time
}

// PollerOption configures optional Poller behavior.
type PollerOption func(*Poller)

// WithRetention sets how long a provider's last successful snapshot may be
// served in place of a failed or breaker-skipped poll. Non-positive values
// are ignored, leaving retention disabled — so AVAILABILITY_RETENTION_SECONDS=0
// is the operator off-switch, restoring strict clear-on-failure. Defaults to
// disabled so the zero-value Poller keeps the behavior tests assert; serve.go
// always wires this from AVAILABILITY_RETENTION_SECONDS (default 300).
func WithRetention(d time.Duration) PollerOption {
	return func(p *Poller) {
		if d > 0 {
			p.retention = d
		}
	}
}

// WithBreaker injects a circuit breaker. When set, providers that the breaker
// reports as Tripped are skipped this cycle; failures are recorded so the
// breaker can promote/demote state.
func WithBreaker(b *Breaker) PollerOption {
	return func(p *Poller) { p.breaker = b }
}

// WithLaunchBreaker injects the launch-outcome breaker (launchbreaker.go).
// When set, offerings whose LaunchKey(provider, gpu_type, gpu_count) is not Healthy are
// dropped from the customer catalog: our own launch outcomes say the SKU
// does not boot, so the provider's advertised stock is not trusted.
func WithLaunchBreaker(b *Breaker) PollerOption {
	return func(p *Poller) { p.launchBreaker = b }
}

// OfferingVerdicts answers whether continuous fleet measurement has delisted
// an offering (by its catalog offering ID). Implemented by
// fleetprobe.VerdictCache, refreshed from Postgres so every gpuctl task agrees.
type OfferingVerdicts interface {
	Delisted(offeringID string) bool
}

// WithOfferingVerdicts injects the fleet prober's verdicts. enforce=false is
// shadow mode: delisted rows stay in the catalog and are only counted, so an
// operator can see what enforcement would hide before turning it on.
func WithOfferingVerdicts(v OfferingVerdicts, enforce bool) PollerOption {
	return func(p *Poller) { p.verdicts, p.enforceVerdicts = v, enforce }
}

// WithPlacementMemory hides offerings that keep failing to place from the
// customer catalog (the staff twin keeps them).
func WithPlacementMemory(m *PlacementMemory) PollerOption {
	return func(p *Poller) { p.placement = m }
}

// WithFundingGate injects the provider-level "account unfunded" circuit
// (fundinggate.go). While a provider is suspended its offerings are left out of
// the catalog: we cannot place anything there, so advertising it only converts
// customers into refused launches (the 2026-09-21→23 incident). Read-only here
// — tripping is the engine's and the balance poller's job.
func WithFundingGate(g *FundingGate) PollerOption {
	return func(p *Poller) { p.fundingGate = g }
}

// WithProbeTimeout overrides the per-call deadline on ListAvailable. Defaults
// to DefaultProbeTimeout (10s).
func WithProbeTimeout(d time.Duration) PollerOption {
	return func(p *Poller) {
		if d > 0 {
			p.probeTimeout = d
		}
	}
}

// WithDisabledCheck injects a predicate reporting whether a provider has been
// switched off sitewide (config.Config.IsProviderDisabled). Disabled providers
// are skipped before any API call each cycle — the same shape as a tripped
// breaker, but operator-driven and sticky — so their offerings leave the cache
// (and every customer-facing surface that reads it) within one poll. nil (the
// zero value) disables nothing, keeping struct-literal test pollers usable.
func WithDisabledCheck(fn func(providerName string) bool) PollerOption {
	return func(p *Poller) { p.isDisabled = fn }
}

// WithManifest injects the pinned-base-environment manifest (Phase 63). nil (the
// zero value) leaves the poller manifest-less, keeping struct-literal test
// pollers usable.
func WithManifest(m *manifest.Manifest) PollerOption {
	return func(p *Poller) { p.manifest = m }
}

// WithMinRetailMarkup sets the retail floor markup percentage applied at
// cache-write (config.MinRetailMarkupPct; serve.go always wires it). Negative
// values are ignored — config.Load already rejects them, and a negative floor
// would sanction publishing below cost. The zero value keeps struct-literal
// test pollers usable: the floor then degrades to bare wholesale rounded up
// to the cent (see RetailPrice), never below cost.
func WithMinRetailMarkup(pct float64) PollerOption {
	return func(p *Poller) {
		if pct > 0 {
			p.minRetailMarkupPct = pct
		}
	}
}

// WithMinRetailMarkupFor sets the per-provider retail-floor resolver
// (admin.Store.MinMarginPctFor; serve.go always wires it, DB-over-env). Unlike
// WithMinRetailMarkup this resolver's values are trusted INCLUDING negatives —
// a negative per-supplier floor is the operator's deliberate loss sanction
// (the Suppliers panel's min_margin_pct knob), not a misconfiguration. When
// set it takes precedence over the scalar floor for every provider; nil keeps
// struct-literal test pollers on the scalar/zero-value behavior.
func WithMinRetailMarkupFor(fn func(providerName string) float64) PollerOption {
	return func(p *Poller) {
		if fn != nil {
			p.minMarkupFor = fn
		}
	}
}

// minMarkupPctFor resolves the retail-floor margin for one provider: the
// per-provider resolver when wired, else the sitewide scalar.
func (p *Poller) minMarkupPctFor(name string) float64 {
	if p.minMarkupFor != nil {
		return p.minMarkupFor(name)
	}
	return p.minRetailMarkupPct
}

// WithCostDiscountFor sets the per-provider partner cost-discount resolver
// (serve.go always wires it, DB-over-env via admin.Store). Leaving it unset
// keeps struct-literal test pollers usable and degrades to "no discount", i.e.
// pricing off the provider's list basis — the safe direction, since a list
// basis is always >= a true-cost one, so an unwired resolver over-prices rather
// than selling below cost.
func WithCostDiscountFor(fn func(providerName string) float64) PollerOption {
	return func(p *Poller) {
		if fn != nil {
			p.costDiscountFor = fn
		}
	}
}

// WithDefaultInstanceDisk sets the platform's effective launch disk in GB
// (config.LaunchDefaultDiskGB; serve.go always wires it), published per offering
// as instance_disk_gb. Non-positive values are ignored — config.Load already
// rejects them — leaving the zero value, which instanceDiskGB degrades to
// provider.FallbackDiskGB so the catalog still promises a real number. Keeping
// this a Poller option rather than a hard constructor argument keeps
// struct-literal test pollers usable, matching WithMinRetailMarkup.
func WithDefaultInstanceDisk(gb int) PollerOption {
	return func(p *Poller) {
		if gb > 0 {
			p.defaultInstanceDiskGB = gb
		}
	}
}

// WithDiskPriceFor injects the per-provider wholesale disk rate resolver
// (config.DiskPricePerGBHourFor; serve.go always wires it), consumed at
// cache-write to publish each offering's retail disk rate. nil (the zero
// value) publishes no rate — the same shape as disk metering switched off —
// keeping struct-literal test pollers usable, matching WithMinRetailMarkup.
func WithDiskPriceFor(fn func(providerName string) float64) PollerOption {
	return func(p *Poller) { p.diskPriceFor = fn }
}

// NewPoller creates a new availability poller.
// interval is the polling frequency (typically 30s).
// markupFor resolves the pricing markup percentage applied to a provider's
// wholesale prices, by provider registry name (config.Config.MarkupPctFor).
func NewPoller(registry *provider.Registry, cache *Cache, interval time.Duration, markupFor func(providerName string) float64, logger *slog.Logger, opts ...PollerOption) *Poller {
	p := &Poller{
		registry:     registry,
		cache:        cache,
		interval:     interval,
		probeTimeout: DefaultProbeTimeout,
		markupFor:    markupFor,
		logger:       logger,
	}
	for _, opt := range opts {
		opt(p)
	}
	return p
}

// timeNow resolves the retention clock (nil → time.Now).
func (p *Poller) timeNow() time.Time {
	if p.now != nil {
		return p.now()
	}
	return time.Now()
}

// storeLastGood records a provider's successful snapshot for retention. An
// empty successful listing is stored too — "legitimately out of stock"
// replaces whatever was retained, it is not preserved by it. No-op when
// retention is disabled.
func (p *Poller) storeLastGood(name string, offerings, held []AvailableOffering, staff []StaffOffering) {
	if p.retention <= 0 {
		return
	}
	p.lastGoodMu.Lock()
	defer p.lastGoodMu.Unlock()
	if p.lastGood == nil {
		p.lastGood = make(map[string]lastGoodSnapshot)
	}
	p.lastGood[name] = lastGoodSnapshot{offerings: offerings, held: held, staff: staff, takenAt: p.timeNow()}
}

// retainedLastGood returns a provider's retained snapshot and its age when one
// exists and is still within the retention window. Expired entries are dropped
// on read so a provider that stays down goes dark once, permanently, instead
// of resurfacing stale rows later.
func (p *Poller) retainedLastGood(name string) (lastGoodSnapshot, time.Duration, bool) {
	if p.retention <= 0 {
		return lastGoodSnapshot{}, 0, false
	}
	p.lastGoodMu.Lock()
	defer p.lastGoodMu.Unlock()
	snap, ok := p.lastGood[name]
	if !ok {
		return lastGoodSnapshot{}, 0, false
	}
	age := p.timeNow().Sub(snap.takenAt)
	if age > p.retention {
		delete(p.lastGood, name)
		return lastGoodSnapshot{}, 0, false
	}
	return snap, age, true
}

// dropLastGood discards a provider's retained snapshot (operator disable).
func (p *Poller) dropLastGood(name string) {
	p.lastGoodMu.Lock()
	defer p.lastGoodMu.Unlock()
	delete(p.lastGood, name)
}

// Start runs the poller loop. It polls immediately on startup, then on each tick.
// Blocks until ctx is cancelled. Intended to be run as a goroutine.
func (p *Poller) Start(ctx context.Context) {
	// Poll immediately on startup -- don't wait 30s for first data.
	p.poll(ctx)

	ticker := time.NewTicker(p.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			p.logger.Info("availability poller stopped")
			return
		case <-ticker.C:
			p.poll(ctx)
		}
	}
}

// poll queries all providers concurrently and writes aggregated offerings to cache.
func (p *Poller) poll(ctx context.Context) {
	allOfferings, staffOfferings := p.collectOfferings(ctx)
	if allOfferings == nil {
		// Registry was empty -- skip cache write entirely so we don't clobber
		// a still-fresh snapshot from a previous cycle.
		return
	}

	// Seed the in-process fallback BEFORE the Redis write, so the read path
	// serves this cycle's snapshot even when the write fails or lands after
	// the previous snapshot's TTL (the slowest provider gates the write; a
	// timed-out cycle used to open a window where GET returned an empty
	// catalog with a 200). Neither slice is mutated after this point, so the
	// cache may alias them (SetLocalOfferings' retain-by-reference contract).
	p.cache.SetLocalOfferings(allOfferings)
	p.cache.SetLocalStaffOfferings(staffOfferings)

	// Write to cache even if empty (clears stale data from removed providers).
	if err := p.cache.SetOfferings(ctx, allOfferings); err != nil {
		p.logger.Error("failed to cache offerings", slog.String("error", err.Error()))
		return
	}

	// Staff snapshot (dev tools): the provider-attributed twin, written to a
	// SEPARATE key only the staff-gated dev-tools endpoints read. Same cycle,
	// same TTL — a write failure degrades only the staff view, never the
	// customer path.
	if err := p.cache.SetStaffOfferings(ctx, staffOfferings); err != nil {
		p.logger.Error("failed to cache staff offerings", slog.String("error", err.Error()))
	}

	p.logger.Debug("availability poll complete",
		slog.Int("total_offerings", len(allOfferings)),
	)
}

// collectOfferings runs one poll cycle and returns the merged customer-facing
// offerings. Per-provider errors are logged and isolated -- one failing
// provider does not block others.
//
// When a Breaker is wired, tripped providers are skipped before any API call.
// Half-open providers receive exactly one probe (the breaker handles promotion
// internally). Failures past the provider's own retry layer (HTTP backoff,
// error mapping, etc.) flow into Breaker.RecordFailure; successes flow into
// RecordSuccess. Empty offerings are NOT a failure (legitimate state for an
// inventory-empty provider).
//
// With retention wired (WithRetention), a failed or breaker-skipped provider
// contributes its last successful snapshot instead of nothing, for at most
// the retention window — so a slow upstream (e.g. an inventory endpoint that
// flirts with the probe timeout) degrades to minutes-stale rows rather than
// flapping in and out of the catalog every cycle. Disabled providers and
// expired snapshots still clear within one cycle.
//
// Returns nil when no providers are registered (signal to skip cache write).
// Returns a non-nil empty slice when all providers either failed or were
// skipped — the caller still writes that to clear stale data.
//
// The second return is the PROVIDER-ATTRIBUTED staff snapshot (dev tools) —
// the same rows before the privacy strip, built in the same pass so the two
// snapshots can never drift within a cycle. It is written to a separate
// staff-only cache key by poll(); nothing on the customer path touches it.
func (p *Poller) collectOfferings(ctx context.Context) ([]AvailableOffering, []StaffOffering) {
	providers := p.registry.All()
	if len(providers) == 0 {
		p.logger.Debug("no providers registered, skipping poll")
		return nil, nil
	}

	var mu sync.Mutex
	allOfferings := make([]AvailableOffering, 0)
	staffOfferings := make([]StaffOffering, 0)
	// Rows the placement memory held back this cycle; restoreLastOfShape puts
	// one back for any GPU shape that would otherwise vanish.
	heldBack := make([]AvailableOffering, 0)
	var wg sync.WaitGroup

	for _, prov := range providers {
		wg.Add(1)
		go func(prov provider.Provider) {
			defer wg.Done()
			name := prov.Name()

			// serveRetained substitutes the provider's last successful
			// snapshot for this cycle's missing rows, when retention is on
			// and the snapshot is still within the window. Catalog-only: the
			// provision engine calls the provider live at launch, so a
			// retained row over a dead upstream still fails into the normal
			// offering-unavailable redirect, never a silent success.
			serveRetained := func(reason string) {
				snap, age, ok := p.retainedLastGood(name)
				if !ok || len(snap.offerings)+len(snap.staff) == 0 {
					// Nothing retained, or the last success was legitimately
					// empty — nothing to serve, and logging "0 offerings"
					// every failed cycle would only be noise.
					return
				}
				// Re-split the retained customer rows against the placement
				// memory as it stands now, as a fresh poll would: a hold that
				// lapsed during the outage lists its row again, one recorded
				// since holds it back. Held rows join heldBack so
				// restoreLastOfShape still keeps a GPU shape whose only rows
				// are held from vanishing while this provider's poll fails.
				listed := make([]AvailableOffering, 0, len(snap.offerings)+len(snap.held))
				var held []AvailableOffering
				for _, rows := range [][]AvailableOffering{snap.offerings, snap.held} {
					for _, o := range rows {
						if p.placement.Hidden(o.OfferingID) {
							held = append(held, o)
						} else {
							listed = append(listed, o)
						}
					}
				}
				mu.Lock()
				allOfferings = append(allOfferings, listed...)
				staffOfferings = append(staffOfferings, snap.staff...)
				heldBack = append(heldBack, held...)
				mu.Unlock()
				p.logger.Info("serving last-known-good offerings",
					slog.String("provider", name),
					slog.String("reason", reason),
					slog.Int("offerings", len(listed)),
					slog.Duration("age", age),
				)
			}

			// Operator kill-switch (PROVIDERS_DISABLED): skip before any API
			// call so a disabled supplier contributes zero offerings and drops
			// out of the cache this cycle. Checked ahead of the breaker so a
			// disabled provider is never even probed. Retention must not
			// soften the kill-switch, so the retained snapshot goes too.
			if p.isDisabled != nil && p.isDisabled(name) {
				p.dropLastGood(name)
				p.logger.Debug("provider skipped (disabled via PROVIDERS_DISABLED)",
					slog.String("provider", name),
				)
				return
			}

			// Account-unfunded suspension (fundinggate.go): our supplier
			// account cannot pay for a launch, so nothing listed there is
			// sellable. Dropped like the kill-switch — retention must not
			// re-serve rows we already know every launch of will be refused
			// — but transient: the gate re-opens by itself after its
			// cooldown and the provider returns on the next poll.
			if p.fundingGate.Suspended(name) {
				p.dropLastGood(name)
				p.logger.Debug("provider skipped (supplier account unfunded)",
					slog.String("provider", name),
				)
				return
			}

			if p.breaker != nil && !p.breaker.Allow(name) {
				p.logger.Debug("provider skipped (breaker tripped)",
					slog.String("provider", name),
				)
				serveRetained("breaker_tripped")
				return
			}

			callCtx, cancel := context.WithTimeout(ctx, p.probeTimeout)
			defer cancel()

			offerings, err := prov.ListAvailable(callCtx)
			if err != nil {
				if p.breaker != nil {
					p.breaker.RecordFailure(name, err)
				}
				p.logger.Error("availability poll failed",
					slog.String("provider", name),
					slog.String("error", err.Error()),
				)
				serveRetained("poll_failed")
				return
			}
			if p.breaker != nil {
				p.breaker.RecordSuccess(name)
			}

			// nil markupFor = 0% markup — keeps the zero-value Poller
			// usable (tests build the struct directly); serve.go always
			// wires config.MarkupPctFor.
			markupPct := 0.0
			if p.markupFor != nil {
				markupPct = p.markupFor(name)
			}
			// nil diskPriceFor = no published disk rate (see WithDiskPriceFor).
			diskRate := 0.0
			if p.diskPriceFor != nil {
				diskRate = p.diskPriceFor(name)
			}
			// nil costDiscountFor = no partner discount, so the floor stays on
			// the list basis — the pre-discount behavior (see RetailPrice).
			costDiscountPct := 0.0
			if p.costDiscountFor != nil {
				costDiscountPct = p.costDiscountFor(name)
			}
			// Per-provider retail floor — may be NEGATIVE (a sanctioned loss
			// floor set in the Suppliers panel). Resolved once per provider per
			// cycle, like the markup and discount above.
			minMarkupPct := p.minMarkupPctFor(name)
			converted := make([]AvailableOffering, 0, len(offerings))
			staffConverted := make([]StaffOffering, 0, len(offerings))
			// Floor-engagement tally for this provider's cycle: ops needs to
			// know a provider's configured markup prices at or below cost, but
			// once per provider per cycle — the old per-offer warn inside
			// ToAvailableOffering emitted 12.8k lines/hour for one 0%-markup
			// provider with ~190 offers. Deliberate scope: only rows the
			// catalog admits are tallied (spot/RegionUnknown drops don't
			// count), and retained last-known-good rows never re-warn — their
			// last live cycle already did.
			flooredCount := 0
			flooredTypes := make(map[string]struct{})
			// Launch-outcome suppression tally: logged once per provider per
			// cycle (same rationale as the floor tally — a suppressed SKU
			// with many configs would otherwise log per row every 30s).
			suppressedCount := 0
			suppressedTypes := make(map[string]struct{})
			verdictHidden, verdictShadow := 0, 0
			unplaceable := 0
			var held []AvailableOffering
			// The rows this poll lists to customers (held ones included),
			// for the placement memory's catalog index (ListedOffering).
			var listed map[string]ListedOffering
			if p.placement != nil {
				listed = make(map[string]ListedOffering, len(offerings))
			}
			for _, o := range offerings {
				// TEMPORARY (2026-07): spot/interruptible capacity disabled
				// sitewide — too unreliable. Drop every spot offer before it
				// reaches the cache, so it never hits the API or the frontend
				// (the spot UI self-hides when no spot rows exist). Provider-
				// agnostic. Revert by deleting this guard.
				if o.Tier == provider.TierSpot {
					p.logger.Debug("dropping spot catalog offering (spot disabled sitewide)",
						slog.String("provider", name),
						slog.String("gpu_type", string(o.GPUType)),
					)
					continue
				}
				// Launch-outcome breaker (launchbreaker.go): recent launches
				// of this (provider, gpu_type) kept dying at boot, so the
				// provider's advertised stock is not trusted — the row
				// vanishes from the CUSTOMER catalog like zero stock
				// (GPU-108: a provider listed A100 capacity its scheduler
				// could not place). Sheddable, not State != Healthy, and the
				// distinction is load-bearing: hiding is bounded by the
				// cooldown. Once it elapses the row RETURNS even before any
				// success, because hiding chokes off the very traffic that
				// could heal the circuit — a non-cheapest or sole-source SKU
				// would otherwise never win an auto-select probe and stay
				// dark until a restart. Post-cooldown customer launches are
				// the probes; a failure re-trips (another cooldown of
				// hiding), a success heals, and the #846 failover shields
				// whoever probed. Read-only on purpose — promotion to
				// half-open belongs to the engine's Allow at placement.
				//
				// The STAFF twin is still built below: ops diagnosing a
				// suppression need to see the rows the customer catalog is
				// hiding, so this is a deliberate deviation from the
				// staff-mirrors-customer invariant, visible via the Warn.
				if p.launchBreaker != nil && p.launchBreaker.Sheddable(LaunchKey(name, string(o.GPUType), o.GPUCount)) {
					suppressedCount++
					suppressedTypes[string(o.GPUType)] = struct{}{}
					staffConverted = append(staffConverted, ToStaffOffering(o, markupPct, minMarkupPct, costDiscountPct))
					continue
				}
				ao := ToAvailableOffering(o, markupPct, minMarkupPct, costDiscountPct, p.defaultInstanceDiskGB, diskRate)
				// M1: an offering whose source region was empty normalizes to
				// the RegionUnknown sentinel. Region is the routing key, so such
				// a row is not customer-provisionable, and surfacing an
				// "unknown"-region price is a data-quality defect. Drop it from
				// the public catalog. Provider-agnostic — catches whichever
				// adapter emitted the empty region (e.g. a supplier
				// row with no `region`).
				if ao.Region == RegionUnknown {
					p.logger.Debug("dropping catalog offering with unresolved region",
						slog.String("provider", name),
						slog.String("gpu_type", ao.GPUModel),
					)
					continue
				}
				// Fleet prober verdict: a probe launched this exact offering
				// and the machine did not work as listed (dead CUDA, missing
				// GPUs, wrong VRAM, never ran). The prober re-probes delisted
				// offerings on a short cycle, so a repaired host comes back on
				// its own; unlike the launch breaker, hiding does not starve
				// the signal that heals it.
				if p.verdicts != nil && p.verdicts.Delisted(ao.OfferingID) {
					if p.enforceVerdicts {
						verdictHidden++
						staffConverted = append(staffConverted, ToStaffOffering(o, markupPct, minMarkupPct, costDiscountPct))
						continue
					}
					verdictShadow++
				}
				if listed != nil {
					listed[ao.OfferingID] = ListedOffering{Provider: name, GPUType: o.GPUType,
						GPUCount: o.GPUCount, Tier: o.Tier, Region: ao.Region}
				}
				// Placement memory: this exact offering keeps failing to place
				// (gone at launch, or the provider refused it for capacity).
				// Hidden with a backoff that clears on the next successful
				// placement, so phantom stock stops being resold to the next
				// customer.
				if p.placement.Hidden(ao.OfferingID) {
					unplaceable++
					held = append(held, ao)
					staffConverted = append(staffConverted, ToStaffOffering(o, markupPct, minMarkupPct, costDiscountPct))
					continue
				}
				converted = append(converted, ao)
				// Staff twin of the SAME row (dev tools) — built only for
				// rows that survived the region gate above, so the staff
				// view mirrors exactly what the customer catalog admitted.
				staffConverted = append(staffConverted, ToStaffOffering(o, markupPct, minMarkupPct, costDiscountPct))
				if floorEngaged(o.PricePerHour, markupPct, minMarkupPct, costDiscountPct) {
					flooredCount++
					flooredTypes[string(o.GPUType)] = struct{}{}
				}
			}
			if verdictHidden > 0 || verdictShadow > 0 {
				p.logger.Warn("fleet probe verdicts on catalog offerings",
					slog.String("provider", name),
					slog.Int("hidden", verdictHidden),
					slog.Int("would_hide_shadow", verdictShadow),
				)
			}
			p.placement.ObserveListed(listed)
			if unplaceable > 0 {
				p.logger.Info("placement memory hiding recently unplaceable offerings",
					slog.String("provider", name),
					slog.Int("offerings", unplaceable),
				)
			}
			if suppressedCount > 0 {
				types := make([]string, 0, len(suppressedTypes))
				for t := range suppressedTypes {
					types = append(types, t)
				}
				sort.Strings(types)
				// Warn, not Debug: a suppressed SKU means real customer
				// launches died on capacity the provider still advertises —
				// this line is the ops alert GPU-108 found missing.
				p.logger.Warn("launch-outcome breaker suppressing catalog offerings",
					slog.String("provider", name),
					slog.Int("offerings", suppressedCount),
					slog.String("gpu_types", strings.Join(types, ",")),
				)
			}
			if flooredCount > 0 {
				// Provider identity in an INTERNAL log line is fine (the
				// Normalize warn has carried it for years); it must never
				// reach a customer-visible surface.
				types := make([]string, 0, len(flooredTypes))
				for t := range flooredTypes {
					types = append(types, t)
				}
				sort.Strings(types)
				p.logger.Warn("retail price floor engaged: configured markup prices below upstream cost floor",
					slog.String("provider", name),
					slog.Int("offerings", flooredCount),
					slog.String("gpu_types", strings.Join(types, ",")),
					slog.Float64("markup_pct", markupPct),
					slog.Float64("min_markup_pct", minMarkupPct),
				)
			}

			// Snapshot BEFORE the merge: converted/held/staffConverted are never
			// mutated after this point, so retention can alias them safely.
			p.storeLastGood(name, converted, held, staffConverted)

			mu.Lock()
			allOfferings = append(allOfferings, converted...)
			staffOfferings = append(staffOfferings, staffConverted...)
			heldBack = append(heldBack, held...)
			mu.Unlock()

			p.logger.Debug("polled provider",
				slog.String("provider", name),
				slog.Int("offerings", len(offerings)),
			)
		}(prov)
	}
	wg.Wait()
	return restoreLastOfShape(allOfferings, heldBack), staffOfferings
}

// restoreLastOfShape keeps the placement memory from emptying a GPU shape:
// when every row of a (GPU model, GPU count) was held back, the cheapest held
// row is listed anyway. The memory is meant to steer customers from a listing
// that keeps refusing to one that can place, never to make a GPU look
// unavailable; with nothing else to offer, the refusing row is still the best
// lead (exact pins are honored at placement either way).
func restoreLastOfShape(listed, held []AvailableOffering) []AvailableOffering {
	if len(held) == 0 {
		return listed
	}
	type shape struct {
		model string
		count int
	}
	present := make(map[shape]bool, len(listed))
	for _, o := range listed {
		present[shape{o.GPUModel, o.GPUCount}] = true
	}
	best := map[shape]AvailableOffering{}
	var order []shape
	for _, o := range held {
		k := shape{o.GPUModel, o.GPUCount}
		if present[k] {
			continue
		}
		cur, seen := best[k]
		if !seen {
			order = append(order, k)
		}
		if !seen || o.PricePerHour < cur.PricePerHour {
			best[k] = o
		}
	}
	for _, k := range order {
		listed = append(listed, best[k])
	}
	return listed
}
