package provider

import "context"

// RawLister is an OPTIONAL capability a Provider may additionally implement: an
// UNFILTERED enumeration of every instance on the upstream account, for the leak
// sweep (tests/providers/sweep). It follows the same optional-interface pattern
// as BootRescueCapability — file-per-capability, type-asserted at the call site,
// with adapters that do not implement it keeping today's behavior byte-for-byte.
//
// WHY IT IS SEPARATE FROM ListProvisioned: ListProvisioned MUST keep its
// instanceid.IsOurs ownership filter, because it feeds the reconciler's
// DESTRUCTIVE orphan pass — dropping that filter is how demo and prod reaped
// each other on 2026-06-20 (WR-04). But that same filter makes smoke orphans
// invisible: a live-test box is named "smoke-gpu-<epoch>-<rand>", deliberately
// outside the ownership grammar, so the sweep built to catch leaks structurally
// cannot see one. On 2026-09-06 a leaked vultr smoke instance billed for 44h
// (~$41) while every sweep enumerated zero rows and reported green.
//
// CONTRACT: ListAllProvisioned is READ-ONLY and returns EVERY instance on the
// account, including manual VMs and other environments' boxes. A caller must
// never terminate straight from it — it applies its own selector first (the
// sweep's smoke-prefix + age match, plus its kill cap).
type RawLister interface {
	// ListAllProvisioned returns every instance on the upstream account,
	// without any ownership filtering.
	ListAllProvisioned(ctx context.Context) ([]ProvisionedInstance, error)
}
