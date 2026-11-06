package provider

// BootRescueCapability is an OPTIONAL capability interface a Provider may
// additionally implement to opt into the provision engine's boot-stall
// watchdog and rescue reprovision. It follows the same optional-interface
// pattern as SpotCapability (type-asserted at the call site).
//
// Declaring the capability asserts two adapter contracts:
//   - GetStatus fills InstanceStatus.BootDetail with live upstream boot
//     progress and InstanceStatus.UpstreamHostID with the backing host's
//     provider-native id, and
//   - Provision honors ProvisionRequest.ExcludeUpstreamHostIDs, so a rescue
//     attempt cannot land back on the host that just stalled.
//
// Adapters that do NOT implement this (or return false) keep today's
// behavior byte-for-byte: the engine never stall-judges their boots and the
// provisioning-budget timeout remains the only watchdog. The capability is
// meant for marketplace-style adapters, whose hosts show host-specific stall
// modes (dead bootstrap phone-home, dead image pull).
type BootRescueCapability interface {
	// SupportsBootRescue reports whether the adapter fulfills the boot
	// telemetry + host-exclusion contracts above.
	SupportsBootRescue() bool
}
