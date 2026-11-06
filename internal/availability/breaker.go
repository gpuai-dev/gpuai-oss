package availability

import (
	"log/slog"
	"sort"
	"sync"
	"time"
)

// ProviderHealth represents the breaker state for a single provider.
type ProviderHealth string

const (
	Healthy  ProviderHealth = "healthy"
	Tripped  ProviderHealth = "tripped"
	HalfOpen ProviderHealth = "half_open"
)

// Default knobs (overridable via env in config.Config; see
// docs/configuration.md).
const (
	DefaultBreakerFailureThreshold = 5
	DefaultBreakerCooldown         = 60 * time.Second
)

// breakerState holds the live state for a single provider. Mutated only under
// Breaker.mu (the field-level pointer is fine; the data behind the pointer is
// never accessed without the parent lock held).
type breakerState struct {
	name             string
	state            ProviderHealth
	consecutiveFails int
	// recent is the rolling outcome window, newest last, true = failure. Used
	// ONLY in rate mode (Breaker.rate != nil); nil otherwise.
	recent        []bool
	lastError     string
	lastErrorAt   time.Time
	lastSuccessAt time.Time
	trippedAt     time.Time
}

// ProviderHealthReport is the JSON-safe snapshot returned by Snapshot.
//
// All time fields are zero-value when never observed; the JSON encoder writes
// the RFC 3339 zero value ("0001-01-01T00:00:00Z") in that case. Admin
// consumers should treat IsZero() as "never observed".
type ProviderHealthReport struct {
	Name                string         `json:"name"`
	State               ProviderHealth `json:"state"`
	ConsecutiveFailures int            `json:"consecutive_failures"`
	LastError           string         `json:"last_error,omitempty"`
	LastErrorAt         time.Time      `json:"last_error_at,omitempty"`
	LastSuccessAt       time.Time      `json:"last_success_at,omitempty"`
	TrippedAt           time.Time      `json:"tripped_at,omitempty"`
}

// Breaker is a per-provider circuit breaker. It is safe for concurrent use.
//
// State transitions:
//
//	healthy   --(threshold consecutive failures)-->        tripped
//	tripped   --(cooldown elapsed, next Allow call)-->     half_open
//	half_open --(RecordSuccess)-->                         healthy
//	half_open --(RecordFailure)-->                         tripped (cooldown reset)
//
// The breaker keeps no persistent state; restarting the binary resets every
// provider to healthy. That is intentional for the single-binary deploy.
type Breaker struct {
	mu        sync.RWMutex
	states    map[string]*breakerState
	threshold int
	cooldown  time.Duration
	// rate, when non-nil, replaces the consecutive-failure trip rule with a
	// windowed failure RATE. nil keeps the original consecutive behaviour, which
	// is what the availability poller and the public request breaker still use.
	rate   *ratePolicy
	now    func() time.Time
	logger *slog.Logger
}

// ratePolicy is the windowed-rate trip rule. See NewRateBreaker.
type ratePolicy struct {
	window     int
	minSamples int
	failRate   float64
}

// push appends one outcome to the rolling window, trimming to size.
func (st *breakerState) push(fail bool, window int) {
	st.recent = append(st.recent, fail)
	if len(st.recent) > window {
		st.recent = st.recent[len(st.recent)-window:]
	}
}

// windowFailRate returns the failure rate over the rolling window and the
// number of samples it is computed from.
func (st *breakerState) windowFailRate() (float64, int) {
	n := len(st.recent)
	if n == 0 {
		return 0, 0
	}
	fails := 0
	for _, f := range st.recent {
		if f {
			fails++
		}
	}
	return float64(fails) / float64(n), n
}

// NewBreaker constructs a Breaker. threshold and cooldown are the failure
// count to trip and the time to wait before promoting to half-open. Pass
// non-positive values to fall back to defaults. logger may be nil.
func NewBreaker(threshold int, cooldown time.Duration, logger *slog.Logger) *Breaker {
	if threshold <= 0 {
		threshold = DefaultBreakerFailureThreshold
	}
	if cooldown <= 0 {
		cooldown = DefaultBreakerCooldown
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Breaker{
		states:    make(map[string]*breakerState),
		threshold: threshold,
		cooldown:  cooldown,
		now:       time.Now,
		logger:    logger,
	}
}

// NewRateBreaker constructs a Breaker that trips on a windowed failure RATE
// rather than a run of consecutive failures.
//
// Derived from 3,924 real prod launch outcomes over the 14 days to 2026-09-17,
// replayed per (provider, gpu_type, gpu_count) key. The consecutive rule this
// replaces was not too lax, as assumed — it was too AGGRESSIVE. It shut off
// capacity that mostly works: one marketplace SKU with 594 launches and a 72% success
// rate tripped after 29, and eleven other sellable shapes in the 20-46%
// failure band tripped too. A windowed rate caught nothing the consecutive rule
// missed; what it changes is that it stops suppressing sellable supply.
//
// Defaults (DefaultLaunchBreakerWindow / MinSamples / FailRate) trip exactly the
// six shapes measured at 79-100% failure and zero shape below the 16.8% fleet
// baseline. The window size matters as much as the rate, because it quantizes
// the achievable threshold — see the constants for why 20/0.75 separates the
// closest measured pair (79.2% vs 63.3%) and 10/0.70 does not.
//
// There is deliberately NO consecutive fast path alongside this. A 3-in-a-row
// escape hatch would reintroduce exactly the over-suppression above, and a
// 5-in-a-row one is already subsumed: five failures with no success is a 100%
// rate over minSamples, which this trips on anyway.
func NewRateBreaker(window, minSamples int, failRate float64, cooldown time.Duration, logger *slog.Logger) *Breaker {
	if window <= 0 {
		window = DefaultLaunchBreakerWindow
	}
	if minSamples <= 0 || minSamples > window {
		minSamples = DefaultLaunchBreakerMinSamples
	}
	if failRate <= 0 || failRate > 1 {
		failRate = DefaultLaunchBreakerFailRate
	}
	b := NewBreaker(0, cooldown, logger)
	b.rate = &ratePolicy{window: window, minSamples: minSamples, failRate: failRate}
	return b
}

// SetClock overrides the time source. Test-only.
func (b *Breaker) SetClock(now func() time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.now = now
}

// getOrInit returns the live state for name, creating it on first sight.
// Caller must hold b.mu (write lock).
func (b *Breaker) getOrInit(name string) *breakerState {
	st, ok := b.states[name]
	if !ok {
		st = &breakerState{name: name, state: Healthy}
		b.states[name] = st
	}
	return st
}

// Allow returns true if the provider should be polled this cycle. If the
// breaker is tripped and the cooldown has elapsed, Allow promotes it to
// half_open and returns true (one probe is allowed). Otherwise Allow returns
// false and the caller should skip the provider.
func (b *Breaker) Allow(name string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()

	st := b.getOrInit(name)
	switch st.state {
	case Healthy, HalfOpen:
		return true
	case Tripped:
		if b.now().Sub(st.trippedAt) >= b.cooldown {
			st.state = HalfOpen
			b.logger.Info("provider breaker promoted to half_open",
				"provider", name,
				"cooldown_elapsed", b.cooldown.String(),
			)
			return true
		}
		return false
	default:
		return true
	}
}

// RecordSuccess clears any failure state for name and returns the breaker to
// healthy. Idempotent.
func (b *Breaker) RecordSuccess(name string) {
	b.mu.Lock()
	defer b.mu.Unlock()

	st := b.getOrInit(name)
	wasTrippedOrHalfOpen := st.state != Healthy
	st.consecutiveFails = 0
	st.lastError = ""
	st.lastErrorAt = time.Time{}
	st.lastSuccessAt = b.now()

	if b.rate != nil {
		st.push(false, b.rate.window)
		// A probe that succeeds after suppression heals and starts a FRESH
		// window: the pre-trip failures describe capacity that has since been
		// re-tested, and carrying them forward would re-trip the key on its
		// own history the moment one more failure arrived.
		if wasTrippedOrHalfOpen {
			st.recent = nil
			st.trippedAt = time.Time{}
			st.state = Healthy
			b.logger.Info("provider breaker recovered to healthy", "provider", name)
		}
		// While already healthy the success is just another sample — state does
		// NOT reset. That reset is what made the old rule blind to a shape that
		// fails most of the time but not consecutively.
		//
		// The rate is evaluated here too, not only on failure. A success can
		// never raise the rate, but it CAN raise the sample count past
		// minSamples for the first time — four failures then one success is 80%
		// over five samples, which is a trip that evaluating only on failure
		// would miss forever.
		if st.state == Healthy {
			if rate, n := st.windowFailRate(); n >= b.rate.minSamples && rate >= b.rate.failRate {
				st.state = Tripped
				st.trippedAt = b.now()
				b.logger.Warn("provider breaker tripped on windowed failure rate",
					"provider", name,
					"fail_rate", rate,
					"samples", n,
					"threshold_rate", b.rate.failRate,
				)
			}
		}
		return
	}

	st.trippedAt = time.Time{}
	st.state = Healthy
	if wasTrippedOrHalfOpen {
		b.logger.Info("provider breaker recovered to healthy", "provider", name)
	}
}

// RecordFailure increments the failure counter and may trip the breaker. If
// the breaker is in half_open, any failure trips it again and resets the
// cooldown.
func (b *Breaker) RecordFailure(name string, err error) {
	if err == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()

	st := b.getOrInit(name)
	st.lastError = err.Error()
	st.lastErrorAt = b.now()

	if b.rate != nil {
		st.consecutiveFails++ // kept for the admin snapshot only
		st.push(true, b.rate.window)
		if st.state == HalfOpen {
			// A failed probe re-trips immediately and restarts the cooldown —
			// same contract as consecutive mode; one bad probe is enough.
			st.state = Tripped
			st.trippedAt = b.now()
			b.logger.Warn("provider breaker re-tripped from half_open",
				"provider", name,
				"error", err.Error(),
				"cooldown_seconds", int(b.cooldown.Seconds()),
			)
			return
		}
		if st.state == Tripped {
			return
		}
		if rate, n := st.windowFailRate(); n >= b.rate.minSamples && rate >= b.rate.failRate {
			st.state = Tripped
			st.trippedAt = b.now()
			b.logger.Warn("provider breaker tripped on windowed failure rate",
				"provider", name,
				"fail_rate", rate,
				"samples", n,
				"threshold_rate", b.rate.failRate,
				"error", err.Error(),
			)
		}
		return
	}

	switch st.state {
	case HalfOpen:
		st.consecutiveFails++
		st.state = Tripped
		st.trippedAt = b.now()
		b.logger.Warn("provider breaker re-tripped from half_open",
			"provider", name,
			"error", err.Error(),
			"cooldown_seconds", int(b.cooldown.Seconds()),
		)
	case Tripped:
		st.consecutiveFails++
	default:
		st.consecutiveFails++
		if st.consecutiveFails >= b.threshold {
			st.state = Tripped
			st.trippedAt = b.now()
			b.logger.Warn("provider breaker tripped",
				"provider", name,
				"consecutive_failures", st.consecutiveFails,
				"threshold", b.threshold,
				"error", err.Error(),
			)
		}
	}
}

// State returns the current state for name. Returns Healthy for unknown names.
// Useful for handlers that need to filter by provider state without taking a
// full snapshot.
func (b *Breaker) State(name string) ProviderHealth {
	b.mu.RLock()
	defer b.mu.RUnlock()
	if st, ok := b.states[name]; ok {
		return st.state
	}
	return Healthy
}

// Sheddable reports whether a request for name would be rejected by Allow
// right now: tripped AND the cooldown has not yet elapsed. Unlike State, it
// accounts for the lazy tripped->half_open promotion that only Allow performs —
// a tripped provider whose cooldown has elapsed is due a probe, so it is NOT
// sheddable even though State still reports Tripped. Read-only: it never
// drives the promotion itself. Returns false for unknown names.
//
// Fast-path pre-checks (e.g. the WR-06 fully-shed short-circuit in the public
// handlers) MUST use this instead of comparing State against Tripped:
// filtering on raw state starves Allow of the call that would half-open the
// breaker, wedging a single-endpoint model open forever.
func (b *Breaker) Sheddable(name string) bool {
	b.mu.RLock()
	defer b.mu.RUnlock()
	st, ok := b.states[name]
	if !ok || st.state != Tripped {
		return false
	}
	return b.now().Sub(st.trippedAt) < b.cooldown
}

// DegradedCount returns the number of providers currently in tripped state.
// Half-open providers are NOT counted (they're being probed, not excluded).
func (b *Breaker) DegradedCount() int {
	b.mu.RLock()
	defer b.mu.RUnlock()
	n := 0
	for _, st := range b.states {
		if st.state == Tripped {
			n++
		}
	}
	return n
}

// Snapshot returns a stable copy of every observed provider's state, sorted
// alphabetically by provider name for deterministic admin output.
func (b *Breaker) Snapshot() []ProviderHealthReport {
	b.mu.RLock()
	defer b.mu.RUnlock()

	out := make([]ProviderHealthReport, 0, len(b.states))
	for _, st := range b.states {
		out = append(out, ProviderHealthReport{
			Name:                st.name,
			State:               st.state,
			ConsecutiveFailures: st.consecutiveFails,
			LastError:           st.lastError,
			LastErrorAt:         st.lastErrorAt,
			LastSuccessAt:       st.lastSuccessAt,
			TrippedAt:           st.trippedAt,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}
