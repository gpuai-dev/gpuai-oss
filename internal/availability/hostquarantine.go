package availability

import (
	"log/slog"
	"sync"
	"time"
)

// DefaultHostQuarantineHold keeps a machine out of the catalog and placement
// for an hour after its bootstrap never phoned home (a tunnel stall). Another
// stall on the same machine holds it for another hour; nothing is held longer.
//
// A tunnel stall follows the machine. On 2026-10-07 one marketplace RTX 4090
// in Vietnam, the cheapest 4090 in the catalog, took five customer-path launches
// in two hours; each reached "starting" within two minutes, never became
// reachable, and failed about nine minutes in after its rescue found no
// replacement. A container rented directly on that machine ran fine, so
// whatever breaks is between the host and GPU.ai's bootstrap, and it repeats.
// A dead image pull does not follow the machine the same way: of four hosts
// that never finished pulling, three booted when rented again, so a dead pull
// never quarantines.
const DefaultHostQuarantineHold = time.Hour

// DefaultHostQuarantineMax caps how many machines are held at once, so the
// quarantine can never take a meaningful share of the pool: about 2% of the
// roughly 300 marketplace offers that pass GPU.ai's search filters. Production saw
// about 0.7 marketplace tunnel-stall failures a day from 9/24 to 10/6 and never more
// than two in an hour, so the cap is a backstop, not a working limit. When it
// is reached, the hold closest to expiring makes room for the new one.
const DefaultHostQuarantineMax = 5

// DefaultHostQuarantineBurstMachines and DefaultHostQuarantineBurstWindow tell
// a bad machine from a bad platform. A tunnel stall can be GPU.ai's own fault:
// with the FRP gateway down, its ports exhausted, or the callback API
// unreachable, every marketplace boot stalls on whatever machine it lands on.
// Quarantining those machines would churn the hold set through healthy hosts
// and leave up to DefaultHostQuarantineMax of them off sale for an hour after
// the outage ends. Production never saw more than two marketplace tunnel stalls in an
// hour, so stalls on three different machines within 15 minutes are read as
// platform-side: the holds those stalls placed are released, no new hold is
// recorded, and an error is logged, until fewer than three machines have
// stalled in the last 15 minutes. One machine stalling again and again is
// still that machine and counts once.
const (
	DefaultHostQuarantineBurstMachines = 3
	DefaultHostQuarantineBurstWindow   = 15 * time.Minute
)

// HostQuarantine keeps machines whose bootstrap never phoned home out of the
// catalog and placement, for adapters that can exclude a machine (by its
// supplier-native machine id). State lives in process, like the launch-outcome breaker:
// gpuctl runs as one task, so the engine that records a stall and the adapter
// that lists and places share it. A restart forgets it.
type HostQuarantine struct {
	hold          time.Duration
	max           int
	burstMachines int // see DefaultHostQuarantineBurstMachines; <= 0 disables
	burstWindow   time.Duration
	logger        *slog.Logger
	now           func() time.Time

	mu     sync.Mutex
	until  map[string]time.Time // provider|host -> end of hold
	stalls map[string]time.Time // provider|host -> last tunnel stall, within burstWindow
}

// NewHostQuarantine builds an empty quarantine with the default burst check.
func NewHostQuarantine(hold time.Duration, max int, logger *slog.Logger) *HostQuarantine {
	if logger == nil {
		logger = slog.Default()
	}
	return &HostQuarantine{
		hold:          hold,
		max:           max,
		burstMachines: DefaultHostQuarantineBurstMachines,
		burstWindow:   DefaultHostQuarantineBurstWindow,
		logger:        logger,
		now:           time.Now,
		until:         map[string]time.Time{},
		stalls:        map[string]time.Time{},
	}
}

func hostKey(provider, hostID string) string { return provider + "|" + hostID }

// Quarantined reports whether the provider's machine is held out right now.
func (q *HostQuarantine) Quarantined(provider, hostID string) bool {
	if q == nil || hostID == "" {
		return false
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	end, ok := q.until[hostKey(provider, hostID)]
	return ok && q.now().Before(end)
}

// RecordTunnelStall holds the machine out for the hold period from now, unless
// stalls are landing on too many machines at once to be the machines' fault
// (see DefaultHostQuarantineBurstMachines).
func (q *HostQuarantine) RecordTunnelStall(provider, hostID string) {
	if q == nil || hostID == "" || q.hold <= 0 || q.max <= 0 {
		return
	}
	now := q.now()
	k := hostKey(provider, hostID)
	q.mu.Lock()
	defer q.mu.Unlock()
	for key, end := range q.until {
		if !now.Before(end) {
			delete(q.until, key)
		}
	}
	// Every stall counts toward the burst check, held or not, so the rate is
	// measured for as long as an outage lasts and holds resume only once it
	// has drained.
	for key, at := range q.stalls {
		if now.Sub(at) >= q.burstWindow {
			delete(q.stalls, key)
		}
	}
	q.stalls[k] = now
	if q.burstMachines > 0 && len(q.stalls) >= q.burstMachines {
		// The holds placed earlier in this burst are as suspect as the one
		// being refused, so they are released too; holds from before the
		// window stand.
		for key := range q.stalls {
			delete(q.until, key)
		}
		q.logger.Error("tunnel stalls on several machines at once look platform-side (FRP gateway or callback API); not quarantining",
			slog.String("provider", provider),
			slog.String("host", hostID),
			slog.Int("machines", len(q.stalls)),
			slog.Duration("window", q.burstWindow),
		)
		return
	}
	if _, held := q.until[k]; !held && len(q.until) >= q.max {
		soonest := ""
		for key, end := range q.until {
			if soonest == "" || end.Before(q.until[soonest]) {
				soonest = key
			}
		}
		delete(q.until, soonest)
	}
	q.until[k] = now.Add(q.hold)
	q.logger.Warn("host quarantined after a tunnel stall",
		slog.String("provider", provider),
		slog.String("host", hostID),
		slog.Duration("for", q.hold),
		slog.Int("held", len(q.until)),
	)
}
