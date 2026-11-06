// Package provider defines the interface all upstream GPU providers must implement.
package provider

import "context"

// Provider is the interface for upstream GPU cloud adapters.
// Each provider adapter (generic, vultr, etc.) implements this interface.
type Provider interface {
	// Name returns the provider identifier (e.g., "generic", "vultr").
	Name() string

	// ListAvailable polls the provider for current GPU inventory and pricing.
	ListAvailable(ctx context.Context) ([]GPUOffering, error)

	// Provision creates a new GPU instance with the given configuration.
	// Returns immediately with an upstream ID; status polling happens separately
	// via GetStatus. The adapter is responsible for configuring the instance with
	// the appropriate startup mechanism (Docker image, startup scripts, environment variables).
	Provision(ctx context.Context, req ProvisionRequest) (*ProvisionResult, error)

	// GetStatus returns the current status of an upstream instance.
	// region is the NATIVE upstream region (e.g. "india-noida-01"), passed
	// so providers with per-region backends can route the lookup to the
	// correct backend. Single-DC adapters ignore it.
	GetStatus(ctx context.Context, upstreamID, region string) (*InstanceStatus, error)

	// Terminate destroys an upstream instance. Returns nil on success.
	// region: see GetStatus — required for region-scoped provider backends.
	Terminate(ctx context.Context, upstreamID, region string) error

	// ListProvisioned returns every active upstream instance the adapter can
	// see across all of its regions / sub-accounts. Used by the reconciler
	// (internal/reconcile) to detect orphans — upstream instances we created
	// (Name = our InstanceID UUID) but no longer track in our DB.
	//
	// Best-effort: adapters that cannot enumerate safely return
	// (nil, ErrNotImplemented) and the reconciler skips orphan detection
	// for that provider. Returning a non-nil error other than
	// ErrNotImplemented is treated as a transient failure and the orphan
	// pass is skipped for this tick.
	//
	// The returned list MUST exclude already-terminated upstream rows; the
	// reconciler treats every entry as a live instance.
	ListProvisioned(ctx context.Context) ([]ProvisionedInstance, error)
}

// ProvisionCapability is an OPTIONAL capability interface a Provider may
// additionally implement to declare whether its Provision path is currently
// operational. Adapters that do NOT implement it are provisionable (the
// default — every datacenter adapter provisions).
//
// An AVAILABILITY-ONLY adapter — one that surfaces catalog inventory via
// ListAvailable but whose Provision deterministically fails (the community
// adapter pre-Phase-59, whose lifecycle methods are ErrNotImplemented stubs) —
// implements this returning false. The provision engine then excludes the
// adapter from the candidate set entirely, instead of burning one of its
// bounded fallback attempts per offering on a candidate that can never
// succeed (which, with ≥3 such offerings priced below datacenter rows, would
// exhaust every attempt and hard-fail provisions for SKUs that have real
// datacenter stock).
//
// This is a candidate-ELIGIBILITY attribute (the same axis as the
// PROVIDERS_DISABLED and custom-image gates); it carries no ranking, scoring,
// or ordering semantics.
type ProvisionCapability interface {
	// Provisionable reports whether Provision can currently be attempted.
	Provisionable() bool
}
