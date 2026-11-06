package provider

import "context"

// SpotCapability is an OPTIONAL capability interface a Provider may additionally
// implement to declare that it can supply interruptible ("spot") capacity and to
// expose how the platform detects preemption. It mirrors the ProvisionCapability
// optional-interface pattern (a Provider is type-asserted to it at wiring time).
//
// Adapters that do NOT implement SpotCapability supply on-demand capacity only —
// the cheapest-spot router (internal/spotrouter) simply never considers them for
// a TierSpot request. This keeps spot support additive and per-provider: each
// adapter opts in independently without a flag-day change to the Provider
// interface.
//
// The contract is deliberately POLL-BASED, not push-based: the platform already
// runs a health/status cadence (internal/health) that calls GetStatus per
// instance, and PreemptionState is queried on that same cadence. A provider with
// a native webhook may still back PreemptionState with its own cache — the
// router only depends on the polled view, so no provider is required to expose a
// webhook to participate.
type SpotCapability interface {
	// SupportsSpot reports whether this provider can currently fulfill a spot
	// ProvisionRequest (req.Tier == TierSpot). It is the spot analog of
	// ProvisionCapability.Provisionable: a candidate-ELIGIBILITY attribute with
	// no ranking/ordering semantics. A provider may return true here yet still
	// have no spot offerings in ListAvailable at a given moment — eligibility is
	// separate from live inventory.
	SupportsSpot() bool

	// PreemptionState reports where an active spot instance sits in the
	// preemption lifecycle, so the router can fail the customer over to the
	// next-cheapest source BEFORE (PreemptionWarned) or AFTER (PreemptionReclaimed)
	// the upstream reclaims the box. It is called on the health cadence with the
	// same identifiers as Provider.GetStatus:
	//   upstreamID — the provider-native instance id.
	//   region     — the NATIVE upstream region (region-scoped backends route on
	//                it; single-DC adapters ignore it).
	// A provider whose upstream gives no preemption signal returns
	// (PreemptionNone, ErrNotImplemented); the router then treats an unexpected
	// GetStatus disappearance as an implicit reclamation (fail-safe re-route).
	PreemptionState(ctx context.Context, upstreamID, region string) (PreemptionStatus, error)
}

// PreemptionStatus is the preemption lifecycle of a spot instance. It is
// deliberately coarse — the router only needs to know "keep it", "drain it now,
// a reclaim is imminent", or "it's gone, re-route" — so every provider can map
// its native signal onto these three states without lossy precision games.
type PreemptionStatus string

const (
	// PreemptionNone: the instance is running normally, no reclamation signalled.
	PreemptionNone PreemptionStatus = "none"
	// PreemptionWarned: the upstream (or, for community, the supplier's reclaim
	// request) has signalled that reclamation is imminent. The router should
	// begin draining the customer workload and pre-stage a re-route within the
	// grace window, but the box is still usable until then. Reuses the same drain
	// grace semantics community machines already have (Phase 59 draining state).
	PreemptionWarned PreemptionStatus = "warned"
	// PreemptionReclaimed: the instance is gone (or unrecoverably reclaimed). The
	// router must re-route the customer to the next-cheapest candidate and stop
	// billing the reclaimed instance immediately.
	PreemptionReclaimed PreemptionStatus = "reclaimed"
)
