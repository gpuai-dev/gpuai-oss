package provider

import (
	"errors"
	"fmt"
)

// Sentinel errors for provider failure modes.
// Callers use errors.Is() to distinguish failure types.
var (
	// ErrNoCapacity indicates the provider has no GPU capacity available.
	// The provisioning engine uses this to try the next provider.
	ErrNoCapacity = errors.New("no GPU capacity available")

	// ErrProviderUnavailable indicates the provider API is unreachable or returning errors.
	ErrProviderUnavailable = errors.New("provider API unavailable")

	// ErrQuotaExceeded indicates OUR account's quota/entitlement at the
	// provider blocks the launch — an operator-side misconfiguration, not
	// capacity churn. Unlike ErrNoCapacity a retry can NEVER succeed, so the
	// API layer must not invite one: it alerts ops and steers the customer to
	// a different machine or region instead.
	ErrQuotaExceeded = errors.New("provider account quota exceeded")

	// ErrAccountUnfunded indicates the provider refused a create because OUR
	// account with it is out of money (e.g. an "insufficient credit" or
	// "account balance is too low" refusal). It is a property of
	// the whole supplier account, not of the offering or the request: every
	// other launch on that provider will fail identically until someone tops
	// the account up. The engine uses it to suspend the provider for placement
	// (availability.FundingGate) instead of walking thousands of doomed
	// launches into it — the 2026-09-21→23 incident, ~3,200 failed launches
	// across 36 orgs over ~30-37h.
	ErrAccountUnfunded = errors.New("provider account unfunded")

	// ErrInvalidGPUType indicates the requested GPU type is not supported by the provider.
	ErrInvalidGPUType = errors.New("unsupported GPU type")

	// ErrNotImplemented indicates an optional Provider operation is not
	// supported by this adapter. Returned by ListProvisioned for providers
	// where a safe upstream-listing endpoint is not available; the
	// reconciler treats it as "skip orphan detection for this provider"
	// rather than a failure.
	ErrNotImplemented = errors.New("operation not implemented for this provider")

	// ErrCostCeilingExceeded indicates every offer the adapter could place on
	// costs MORE than ProvisionRequest.MaxCostPerHour — the wholesale ceiling
	// the engine derived from the price the customer was actually quoted.
	// Returned INSTEAD of accepting the offer, because on a per-machine
	// marketplace accepting IS renting: there is no hold primitive, so
	// the only moment we can decline a repriced node is before the accept.
	ErrCostCeilingExceeded = errors.New("cheapest available offer exceeds cost ceiling")
)

// CostCeilingError carries the cheapest wholesale $/hr the adapter could
// actually place on alongside the ErrCostCeilingExceeded sentinel, so the
// engine can re-quote the customer at the real new price instead of failing
// opaquely. errors.Is(err, ErrCostCeilingExceeded) still matches (see Is).
//
// Both fields are WHOLESALE whole-node rates (the same basis as
// GPUOffering.PricePerHour) — the engine marks them up before they reach a
// customer.
type CostCeilingError struct {
	// CheapestCostPerHour is the lowest-cost placeable offer right now.
	CheapestCostPerHour float64
	// CeilingCostPerHour is the ceiling it exceeded.
	CeilingCostPerHour float64
}

func (e *CostCeilingError) Error() string {
	return fmt.Sprintf("cheapest available offer %.4f/hr exceeds cost ceiling %.4f/hr",
		e.CheapestCostPerHour, e.CeilingCostPerHour)
}

// Is reports true for ErrCostCeilingExceeded so callers can errors.Is on the
// sentinel while errors.As on *CostCeilingError recovers the priced detail.
func (e *CostCeilingError) Is(target error) bool { return target == ErrCostCeilingExceeded }
