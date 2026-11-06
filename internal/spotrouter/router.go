// Package spotrouter implements cross-provider CHEAPEST-SPOT routing.
//
// The platform aggregates live spot capacity across every registered provider
// that opts into provider.SpotCapability and
// places the customer on the cheapest offering that fits, failing over to the
// next-cheapest on preemption. This is price-taking, not a bidding auction:
// the customer sees one best spot price and never bids.
//
// The package depends on providers ONLY via the provider.Provider and
// provider.SpotCapability interfaces — never a concrete adapter — so adding a
// new spot-capable provider requires zero changes here.
package spotrouter

import (
	"context"
	"log/slog"
	"sort"

	"github.com/gpuai-dev/gpuai-oss/internal/availability"
	"github.com/gpuai-dev/gpuai-oss/internal/provider"
)

// Eligible reports whether a provider may be considered for a TierSpot
// request: it must implement the optional provider.SpotCapability interface
// AND currently report SupportsSpot(). This is the single source of truth for
// spot candidate-eligibility (the same axis as ProvisionCapability — no
// ranking semantics); eligibility is separate from live inventory, which is
// what ListAvailable answers.
func Eligible(p provider.Provider) bool {
	sc, ok := p.(provider.SpotCapability)
	return ok && sc.SupportsSpot()
}

// ProviderSource yields the providers to route across. *provider.Registry
// satisfies it; tests inject a static slice.
type ProviderSource interface {
	All() []provider.Provider
}

// MarkupResolver returns the retail markup percentage for a provider name.
// Production wires config.Config.MarkupPctFor so the router prices candidates
// with exactly the same markup the availability catalog applies — the markup
// is reused, never re-derived.
type MarkupResolver func(providerName string) float64

// SpotRequest describes the capacity a customer needs from the spot pool.
type SpotRequest struct {
	// GPUType is the exact GPU model required. When empty, any model with at
	// least MinVRAMGB per GPU qualifies (mirrors the engine's auto-select).
	GPUType provider.GPUType
	// GPUCount is the minimum number of GPUs the offering must supply.
	GPUCount int
	// MinVRAMGB is the per-GPU VRAM floor, used only when GPUType is empty.
	MinVRAMGB int
	// Regions optionally restricts candidates to these CANONICAL region codes
	// (offerings carry native regions; membership is checked after
	// availability.Normalize). Empty means any region.
	Regions []string
}

// Candidate is one spot offering a customer could be placed on, priced at
// retail (marked up per-provider like the catalog).
type Candidate struct {
	Prov     provider.Provider
	Offering provider.GPUOffering
	// RetailPricePerHour is the customer-facing whole-instance price:
	// availability.RetailPrice(Offering.PricePerHour, markup(provider),
	// minMarkup) — floor included, identical to the catalog's quote.
	RetailPricePerHour float64
}

// Router aggregates live spot capacity across all spot-eligible providers and
// ranks it cheapest-first by retail price.
type Router struct {
	providers ProviderSource
	markupPct MarkupResolver
	// minMarkupPct resolves the PER-PROVIDER retail floor margin percentage
	// (admin.Store.MinMarginPctFor in production): candidates are priced with
	// the same floored RetailPrice formula the catalog and engine apply, so a
	// spot candidate can never be quoted below cost × (1 + floor/100) — where
	// the floor may be a supplier's sanctioned negative loss floor. nil = the
	// at-cost floor everywhere (test routers).
	minMarkupPct MarkupResolver
	// costDiscountPct resolves the partner cost discount used to rebase the
	// retail floor onto true cost (config.CostDiscountPctFor). nil = none.
	costDiscountPct MarkupResolver
	logger          *slog.Logger
}

// New constructs a Router. markupPct must not be nil (wire
// config.Config.MarkupPctFor in production); minMarkupPct resolves the
// per-provider retail floor (wire admin.Store.MinMarginPctFor — nil floors at
// bare cost everywhere, see availability.RetailPrice); a nil logger falls back
// to slog.Default().
// costDiscountPct resolves a provider's partner cost discount (wire
// config.Config.CostDiscountPctFor); nil means no discount anywhere, which
// leaves the floor on the list basis — see availability.RetailPrice.
func New(providers ProviderSource, markupPct MarkupResolver, minMarkupPct MarkupResolver, costDiscountPct MarkupResolver, logger *slog.Logger) *Router {
	if logger == nil {
		logger = slog.Default()
	}
	return &Router{providers: providers, markupPct: markupPct, minMarkupPct: minMarkupPct, costDiscountPct: costDiscountPct, logger: logger}
}

// minMarkupFor resolves the retail-floor margin for a provider, tolerating a
// nil resolver (test routers) as the at-cost floor.
func (r *Router) minMarkupFor(providerName string) float64 {
	if r.minMarkupPct == nil {
		return 0
	}
	return r.minMarkupPct(providerName)
}

// costDiscountFor resolves the partner cost discount for a provider, tolerating
// a nil resolver (test routers) as "no discount".
func (r *Router) costDiscountFor(providerName string) float64 {
	if r.costDiscountPct == nil {
		return 0
	}
	return r.costDiscountPct(providerName)
}

// Candidates collects every live Tier==spot offering that fits req from every
// Eligible provider and returns them ordered by retail price ascending
// (stable, so registry order breaks ties). An empty slice with a nil error
// means no spot capacity currently fits; per-provider ListAvailable failures
// are logged and skipped so one flaky upstream never blanks the whole pool.
func (r *Router) Candidates(ctx context.Context, req SpotRequest) ([]Candidate, error) {
	var candidates []Candidate
	for _, prov := range r.providers.All() {
		if !Eligible(prov) {
			continue
		}
		offerings, err := prov.ListAvailable(ctx)
		if err != nil {
			r.logger.Warn("spotrouter: provider availability failed, skipping",
				slog.String("provider", prov.Name()),
				slog.String("error", err.Error()),
			)
			continue
		}
		for _, o := range offerings {
			if o.Tier != provider.TierSpot {
				continue
			}
			if req.GPUType == "" {
				// Normalized for the same reason as the provision engine's
				// auto-select filter: MinVRAMGB is a nameplate-scale floor,
				// a raw offering carries the upstream's measured figure.
				if provider.NormalizeVRAM(o.GPUType, o.VRAMPerGPUGB) < req.MinVRAMGB {
					continue
				}
			} else if o.GPUType != req.GPUType {
				continue
			}
			if o.GPUCount < req.GPUCount {
				continue
			}
			if o.AvailableCount <= 0 {
				continue
			}
			if len(req.Regions) > 0 {
				canonical := availability.Normalize(o.Region, o.DatacenterLocation, prov.Name())
				matched := false
				for _, want := range req.Regions {
					if want == canonical {
						matched = true
						break
					}
				}
				if !matched {
					continue
				}
			}
			candidates = append(candidates, Candidate{
				Prov:               prov,
				Offering:           o,
				RetailPricePerHour: availability.RetailPrice(availability.TrueCost(o.PricePerHour, r.costDiscountFor(prov.Name())), r.markupPct(prov.Name()), r.minMarkupFor(prov.Name())),
			})
		}
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		return candidates[i].RetailPricePerHour < candidates[j].RetailPricePerHour
	})
	return candidates, nil
}
