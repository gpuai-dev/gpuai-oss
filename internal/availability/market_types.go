package availability

import (
	"context"
	"strconv"
	"strings"
	"time"
)

// market_types.go owns the phase-86 GPU Market contracts: the row shape of the
// 15-minute price/stock series (MarketSupply) and the narrow store seam the
// poller writes it through (MarketHistoryStore).
//
// PRIVACY, defense-by-omission (D-04) — the same discipline AvailableOffering
// documents in types.go, extended to durable storage: no field naming the
// provider / upstream / supplier a row came from exists here. PRIVACY_CI_ALLOW
// Identity therefore cannot leak through json.Marshal, through a future
// SELECT *, or through a stray debug log. Rows are aggregated across every
// source at WRITE time, so granular per-source data never lands in a
// customer-reachable table at all — a strip applied at the read boundary would
// be one forgotten endpoint away from failing.
//
// The repo-wide TestPrivacyCIGuard does NOT scan this package (it names
// internal/availability in its own EXCLUDED list), so the mechanical
// enforcement for this file is TestMarketPrivacy in internal/privacy
// (D-26). Adding a field here that names a source will fail that test.
//
// Deliberately NOT a widening of ModelSupply / HistoryStore in history.go
// (D-01): that hourly, model-only rollup is shared with the admin Key Metrics
// supply panel, and a customer-facing feature must not be able to change the
// primary key of an internal ops surface.

// MarketSupply is the market state for ONE (GPUModel, Region, CapacityClass)
// key at one 15-minute bucket: the floor per-GPU-hour price we can actually
// offer, and how much of it was obtainable.
//
// CapacityClass is the one source-derived attribute that intentionally
// survives the strip: it is a per-offering secure/community CLASS, never
// identity — the same precedent as AvailableOffering.InstantBoot / Community /
// CertifiedImage.
type MarketSupply struct {
	GPUModel      string
	Region        string
	CapacityClass string // "secure" | "community" — an attribute, never identity.

	// PricePerGPUHour is THE series value (D-05): the floor per-GPU-hour rate
	// for this bucket, computed at WRITE time from the whole-node catalog
	// price ÷ GPUCount. AvailableOffering.PricePerHour is a WHOLE-NODE price;
	// storing it raw would mix 1-GPU and 8-GPU rates into a single "floor" and
	// reproduce the bug #830 fixed on /pricing/comparison. Dividing on read is
	// exactly the shape that shipped −184% savings to production.
	PricePerGPUHour float64

	// ClusterSize is the node shape this row describes — how many GPUs are on
	// each node whose price it floors (v102). It is part of the row's KEY, not
	// context: a 1-GPU box and an 8-GPU node are different products at
	// different per-GPU rates, and the pre-v102 series merged them, which made
	// "what have 8-GPU nodes been costing?" unanswerable.
	//
	// 0 is a SENTINEL meaning "merged across sizes", carried by rows written
	// before v102. It is never produced by the aggregator — EffectiveGPUCount
	// maps a malformed 0 to 1 — so a 0 read back always means "legacy row",
	// and legacy rows are only ever served in the merged (no size filter) view
	// they honestly represent.
	ClusterSize int

	// NodePricePerHour and GPUCount are CONTEXT for how PricePerGPUHour was
	// derived (D-05 / D-09), populated only when GPUCount > 1. They are not a
	// parallel authoritative figure — nothing has to stay equal to anything.
	// Readers use them for the "(from an 8-GPU node)" qualifier so a derived
	// rate is never rendered as if it were single-GPU rentable.
	NodePricePerHour float64
	GPUCount         int

	// AvailableCount is the aggregate obtainable count across every source in
	// this bucket; OfferCount is how many distinct offerings contributed it.
	//
	// A zero AvailableCount means genuinely sold out. "We did not successfully
	// sample" is encoded as NO ROW — two states, two row shapes, never a flag
	// (D-06). Callers must not synthesize a zero row to fill a gap: that
	// records a phantom stockout and blames supply for our own outage.
	AvailableCount int64
	OfferCount     int
}

// MarketHistoryStore persists the 15-minute market series. Declared here with
// package availability's OWN row type so this package never imports
// internal/db (and internal/db never imports this one) — cmd/gpuctl owns the
// thin adapter that converts between the two, the same convention HistoryStore
// follows in history.go.
//
// nil is a supported state: a poller without a market store simply records
// nothing.
//
// PruneMarketHistory is DELETE-ONLY (D-03). There is no thin-to-hourly
// rewrite: a job that REWRITES history is a correctness liability on the exact
// data being sold as a trust asset, and downsampling belongs at query time.
type MarketHistoryStore interface {
	InsertMarketHistory(ctx context.Context, bucket time.Time, rows []MarketSupply) error
	PruneMarketHistory(ctx context.Context, before time.Time) (int64, error)
}

// ClusterSizeMerged is the cluster_size sentinel for a row that merges every
// node shape — every row written before v102 made the node size a dimension.
// It is not a size, and nothing writes it: it exists so readers can name what
// a 0 means instead of testing a bare literal.
const ClusterSizeMerged = 0

// SellableClusterSizes are the node shapes the catalog is actually offered in,
// and therefore the only values gpu_market_history can hold a series for.
//
// It is a fixed list rather than a range because node shapes are not arbitrary:
// hardware ships in these counts, upstreams price in them, and a customer
// picking "6" is asking about a product nobody sells. Answering that with an
// empty series would be indistinguishable from a genuine gap, which is the one
// confusion this whole surface exists to prevent — so the API refuses the value
// and names the set instead.
//
// 10 and 16 are here because customers ask for them even though they are rarer
// in our catalog than 1/2/4/8; a size we may legitimately have zero data for is
// still a size, and the chart's own gap band says so honestly.
var SellableClusterSizes = []int{1, 2, 4, 8, 10, 16}

// IsSellableClusterSize reports whether n names a node shape we sell.
func IsSellableClusterSize(n int) bool {
	for _, s := range SellableClusterSizes {
		if s == n {
			return true
		}
	}
	return false
}

// SellableClusterSizesLabel renders the set for an error detail, so a refusal
// names the way forward instead of only saying no.
func SellableClusterSizesLabel() string {
	parts := make([]string, len(SellableClusterSizes))
	for i, s := range SellableClusterSizes {
		parts[i] = strconv.Itoa(s)
	}
	return strings.Join(parts, ", ")
}
