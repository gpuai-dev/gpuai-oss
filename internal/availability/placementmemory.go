package availability

import (
	"log/slog"
	"sync"
	"time"

	"github.com/gpuai-dev/gpuai-oss/internal/provider"
)

// PlacementBackoff is how long an offering stays out of the customer catalog
// and unpinned placement after consecutive failed placements: entry i applies
// to the (i+1)th failure in a streak, and the last entry to every failure after
// that. A zero entry counts the failure but keeps selling the offering.
type PlacementBackoff []time.Duration

// DefaultPlacementBackoff keeps selling an offering after its first failed
// placement, hides it for 15 minutes after the second, and for an hour from
// the third.
//
// One refusal is weak evidence. On 2026-10-06, 12 of 45 fresh pinned launches
// were refused and 7 of those rows placed two minutes later. A second refusal
// in the same streak is strong evidence: over two days of customer-path
// launches (384 pinned, 2026-10-06 and 07), hiding from the second refusal
// would have avoided 46 later refusals on the same offerings at the cost of 2
// later placements, where hiding from the first would have avoided 59 at the
// cost of 5.
var DefaultPlacementBackoff = PlacementBackoff{0, 15 * time.Minute, time.Hour}

// DefaultPlacementStreakWindow ends a streak: a failure more than this long
// after the previous one counts as the first again.
const DefaultPlacementStreakWindow = time.Hour

// maxPlacementEntries bounds the failure map. Entries only come from offerings
// a provider returned (the walk records candidates it tried, and a pin counts
// only when the catalog listed it, see ListedOffering), so the map normally
// holds just the offerings that failed within the last streak window; the cap
// keeps a burst of failures from growing it without limit.
const maxPlacementEntries = 4096

// placementPruneEvery rate-limits the full-map prune, so a burst of refused
// launches does not scan the maps on every one.
const placementPruneEvery = time.Minute

type placementEntry struct {
	failures    int
	lastFailure time.Time
	hiddenUntil time.Time
}

// ListedOffering is what the customer catalog showed for one offering ID:
// the provider it came from and the shape a launch has to ask for to get it.
//
// The engine needs it to judge a pinned launch that matched nothing. An
// offering ID is a hash, so on its own a pin that drops out of selection says
// nothing about why: the offering may have vanished from its provider, or the
// request itself may have excluded it (a different GPU count, tier or region,
// a provider gate the launch failed, a provider whose listing errored). Only
// the first is the offering's fault, and counting the rest would let any
// caller hide any listing by pinning it with a mismatched request.
type ListedOffering struct {
	Provider string
	GPUType  provider.GPUType
	GPUCount int
	Tier     provider.InstanceTier
	// Region is the canonical region (Normalize), as the catalog showed it.
	Region string
}

type listedEntry struct {
	ListedOffering
	seen time.Time
}

// PlacementMemory remembers offerings that keep failing to place (the pinned
// offering was gone at placement, or the provider refused the create for
// capacity), so the catalog stops advertising them and unpinned placement
// stops picking them until the backoff lapses or one of them places.
//
// The state lives in process, like the launch-outcome breaker and the funding
// gate: gpuctl runs as one task (infra: desired_count 1, the FRP DNS
// constraint), so the poller that builds the catalog and the engine that places
// launches share this instance. A restart forgets it, which only means a dead
// listing has to fail twice again before it is hidden.
type PlacementMemory struct {
	backoff PlacementBackoff
	window  time.Duration
	logger  *slog.Logger
	now     func() time.Time

	mu        sync.Mutex
	entries   map[string]*placementEntry
	listed    map[string]listedEntry
	lastPrune time.Time
}

// NewPlacementMemory builds an empty memory.
func NewPlacementMemory(backoff PlacementBackoff, window time.Duration, logger *slog.Logger) *PlacementMemory {
	if logger == nil {
		logger = slog.Default()
	}
	return &PlacementMemory{backoff: backoff, window: window, logger: logger, now: time.Now,
		entries: map[string]*placementEntry{}, listed: map[string]listedEntry{}}
}

// Hidden reports whether the offering is currently held out of the catalog.
func (m *PlacementMemory) Hidden(offeringID string) bool {
	if m == nil || offeringID == "" {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.entries[offeringID]
	return ok && m.now().Before(e.hiddenUntil)
}

// RecordFailure counts a failed placement of the offering and hides it for the
// backoff its streak has reached.
func (m *PlacementMemory) RecordFailure(offeringID, reason string) {
	if m == nil || offeringID == "" || len(m.backoff) == 0 {
		return
	}
	now := m.now()
	m.mu.Lock()
	defer m.mu.Unlock()
	m.pruneLocked(now)
	e := m.entries[offeringID]
	if e == nil || now.Sub(e.lastFailure) > m.window {
		if e == nil && len(m.entries) >= maxPlacementEntries {
			m.evictOldestLocked()
		}
		e = &placementEntry{}
		m.entries[offeringID] = e
	}
	e.failures++
	e.lastFailure = now
	hide := m.backoff[min(e.failures, len(m.backoff))-1]
	if hide <= 0 {
		return
	}
	if until := now.Add(hide); until.After(e.hiddenUntil) {
		e.hiddenUntil = until
	}
	m.logger.Info("placement memory: hiding an offering that keeps failing to place",
		slog.String("offering_id", offeringID),
		slog.Int("failures", e.failures),
		slog.Duration("hidden_for", hide),
		slog.String("reason", reason),
	)
}

// RecordSuccess forgets the offering's failures once it placed.
func (m *PlacementMemory) RecordSuccess(offeringID string) {
	if m == nil || offeringID == "" {
		return
	}
	m.mu.Lock()
	delete(m.entries, offeringID)
	m.mu.Unlock()
}

// ObserveListed records the rows one provider's poll put in the customer
// catalog (held-back rows included), keyed by offering ID. The poller calls it
// for every provider it polled successfully; Listed reads it back for the
// engine. An ID stays known for the streak window after it was last listed,
// so a listing that flickers out between the catalog read and the launch is
// still recognized. Entries come only from our own polls, so the map is
// bounded by the catalog, never by what callers send.
func (m *PlacementMemory) ObserveListed(rows map[string]ListedOffering) {
	if m == nil || len(rows) == 0 {
		return
	}
	now := m.now()
	m.mu.Lock()
	defer m.mu.Unlock()
	m.pruneLocked(now)
	for id, l := range rows {
		m.listed[id] = listedEntry{ListedOffering: l, seen: now}
	}
}

// Listed returns what the catalog last showed for the offering, if it listed
// it within the streak window.
func (m *PlacementMemory) Listed(offeringID string) (ListedOffering, bool) {
	if m == nil || offeringID == "" {
		return ListedOffering{}, false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	l, ok := m.listed[offeringID]
	if !ok || m.now().Sub(l.seen) > m.window {
		return ListedOffering{}, false
	}
	return l.ListedOffering, true
}

// pruneLocked drops failure entries that are neither hidden nor inside a
// streak, and catalog entries not listed within the window, so the maps hold
// only recent offerings. At most once per placementPruneEvery (or when the
// failure map is full): a full scan on every refused launch would make a
// burst of refusals quadratic.
func (m *PlacementMemory) pruneLocked(now time.Time) {
	if now.Sub(m.lastPrune) < placementPruneEvery && len(m.entries) < maxPlacementEntries {
		return
	}
	m.lastPrune = now
	for id, e := range m.entries {
		if !now.Before(e.hiddenUntil) && now.Sub(e.lastFailure) > m.window {
			delete(m.entries, id)
		}
	}
	for id, l := range m.listed {
		if now.Sub(l.seen) > m.window {
			delete(m.listed, id)
		}
	}
}

// evictOldestLocked makes room in a full failure map by dropping the entry
// whose last failure is oldest. Only reached when every entry is recent (the
// prune that runs first at the cap found nothing to drop), which takes far
// more distinct failing offerings than any catalog lists.
func (m *PlacementMemory) evictOldestLocked() {
	var oldestID string
	var oldest time.Time
	for id, e := range m.entries {
		if oldestID == "" || e.lastFailure.Before(oldest) {
			oldestID, oldest = id, e.lastFailure
		}
	}
	delete(m.entries, oldestID)
}
