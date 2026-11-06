package availability

import (
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"
)

// FundingGate is the provider-level "account unfunded" circuit.
//
// WHY IT EXISTS: on 2026-09-21→23 two prepaid upstream accounts hit $0 and for
// ~30-37h we kept selling them. Every launch was refused at CREATE time (an
// "insufficient credit" or "balance too low" error from the upstream) — ~3,200
// failed launch requests across 36 orgs. The launch-outcome breaker
// (launchbreaker.go) never fired because it only scores launches the provider
// ACCEPTED; a create-time refusal never reaches it. And an unfunded account is
// not a (provider, gpu_type) problem anyway: it is the whole supplier.
//
// Semantics: Trip suspends a provider for placement for the cooldown (default
// 10 min). While suspended the provision engine drops it from the candidate
// ladder and the availability poller drops its offerings from the catalog.
// When the cooldown lapses the gate simply re-opens — the next real launch IS
// the re-probe: if the account is still empty it is refused at create time
// again (no instance, no charge) and re-trips the gate; if it was topped up the
// launch succeeds and nothing else needs to happen. The supplier balance poller
// also re-trips it every cycle while a prepaid pot reads <= 0, so a provider
// with a readable balance stays suspended for as long as the pot is empty.
//
// In-memory and per process by design, like the other breakers: each replica
// learns the condition from its own first refused launch (or its own balance
// poll). A restart re-opens the gate, which costs at most one more refused
// launch per replica.
//
// All methods are nil-receiver safe: a nil *FundingGate suspends nothing.
type FundingGate struct {
	cooldown time.Duration
	logger   *slog.Logger
	now      func() time.Time

	mu sync.Mutex
	// until is when each suspended provider's suspension lapses.
	until map[string]time.Time
	// reason is the latest trip reason, for the log and the snapshot.
	reason map[string]string
	// lastAlert rate-limits the loud alert per provider (FundingAlertRepeat).
	lastAlert map[string]time.Time
}

const (
	// DefaultFundingCooldown is how long one trip suspends a provider.
	// Short on purpose: the only cost of re-opening early is one refused
	// create (which bills nothing), while the cost of staying shut after a
	// top-up is lost sales. The balance poller's 5-minute cycle keeps a
	// readable empty pot suspended continuously.
	DefaultFundingCooldown = 10 * time.Minute

	// FundingAlertRepeat is the per-provider page suppression window — the
	// same hourly cadence the supplier balance guardrail uses, so a supplier
	// that stays empty pages once an hour rather than every re-trip.
	FundingAlertRepeat = time.Hour

	// FundingAlertMessage is the slog message of the loud alert. It is
	// DELIBERATELY the supplier balance guardrail's message
	// (supplierbalance.AlertMessage — a test there pins the equality): one
	// log-based alert for "our supplier account is out of money", whichever
	// detector saw it first.
	FundingAlertMessage = "supplier balance guardrail"

	// FundingAlertReason is carried as reason= on the alert line.
	FundingAlertReason = "account_unfunded"
)

// NewFundingGate builds a gate. cooldown <= 0 selects DefaultFundingCooldown.
func NewFundingGate(cooldown time.Duration, logger *slog.Logger) *FundingGate {
	if cooldown <= 0 {
		cooldown = DefaultFundingCooldown
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &FundingGate{
		cooldown:  cooldown,
		logger:    logger,
		now:       time.Now,
		until:     make(map[string]time.Time),
		reason:    make(map[string]string),
		lastAlert: make(map[string]time.Time),
	}
}

// SetClock injects the time source (tests).
func (g *FundingGate) SetClock(now func() time.Time) {
	if g == nil || now == nil {
		return
	}
	g.mu.Lock()
	g.now = now
	g.mu.Unlock()
}

// Trip suspends providerName for one cooldown from now (extending any running
// suspension). source names the detector ("create_rejected",
// "balance_poller"); detail is a short operator-facing explanation.
//
// loud controls the page: when true, a level=ERROR FundingAlertMessage line is
// emitted, at most once per FundingAlertRepeat per provider. The balance poller
// passes false because it already pages through its own guardrail line for the
// same condition — two lines for one empty pot would only teach operators to
// skim.
//
// Returns true when this call opened a suspension (the provider was placeable
// immediately before).
func (g *FundingGate) Trip(providerName, source, detail string, loud bool) bool {
	if g == nil || providerName == "" {
		return false
	}
	name := strings.ToLower(providerName)
	g.mu.Lock()
	now := g.now()
	wasOpen := !now.Before(g.until[name]) // lapsed or never tripped
	g.until[name] = now.Add(g.cooldown)
	g.reason[name] = source
	alert := false
	if loud {
		if last, seen := g.lastAlert[name]; !seen || now.Sub(last) >= FundingAlertRepeat {
			g.lastAlert[name] = now
			alert = true
		}
	}
	g.mu.Unlock()

	if wasOpen {
		g.logger.Warn("supplier account unfunded: provider suspended for placement",
			slog.String("provider", name),
			slog.String("source", source),
			slog.Duration("cooldown", g.cooldown),
			slog.String("detail", detail),
		)
	}
	if alert {
		// The alert IS this line (ALRT-02): route it with log-based alerting
		// on the message. Deliberately WITHOUT detail: that is the raw
		// upstream error string, which stays on the Warn line in the log group
		// and never rides the alert route (same rule as upstreamalert).
		g.logger.Error(FundingAlertMessage,
			slog.String("provider", name),
			slog.String("reason", FundingAlertReason),
			slog.String("source", source),
		)
	}
	return wasOpen
}

// Suspended reports whether providerName is currently suspended for placement.
func (g *FundingGate) Suspended(providerName string) bool {
	if g == nil || providerName == "" {
		return false
	}
	name := strings.ToLower(providerName)
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.now().Before(g.until[name])
}

// FundingSuspension is one suspended provider, for diagnostics.
type FundingSuspension struct {
	Provider string    `json:"provider"`
	Source   string    `json:"source"`
	Until    time.Time `json:"until"`
}

// Snapshot returns the currently suspended providers, sorted by name.
func (g *FundingGate) Snapshot() []FundingSuspension {
	if g == nil {
		return nil
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	now := g.now()
	out := make([]FundingSuspension, 0, len(g.until))
	for name, until := range g.until {
		if now.Before(until) {
			out = append(out, FundingSuspension{Provider: name, Source: g.reason[name], Until: until})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Provider < out[j].Provider })
	return out
}
