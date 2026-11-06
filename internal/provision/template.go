package provision

import (
	"encoding/base64"
	"strings"

	"github.com/gpuai-dev/gpuai-oss/internal/provider"
)

// CustomerImageVMCapable reports whether a VM-class provider can host a
// CUSTOMER image via the bootstrap docker-run path (custom images Phase 3):
// we boot the provider's VM, install docker + the GPU toolkit, and run the
// digest-pinned customer ref beside the host's sshd/tunnel — the exact
// machinery the certified-VM path hardened (GPU-57/76). Derived from the
// existing classifiers rather than a third hand-kept roster: vm-class AND
// certified-capable (a fixed-template provider fails the latter). Container-native providers
// return false here — they run the image natively via SupportsCustomerImage.
func CustomerImageVMCapable(providerName string) bool {
	return provider.DeploymentType(providerName) == "vm" && provider.CertifiedImageCapable(providerName)
}

// argvB64 encodes an argv as NUL-TERMINATED elements (element + NUL, each),
// base64-enveloped, for the bootstrap's `read -d ”` loop — the only
// representation that survives arbitrary argument bytes (spaces, quotes,
// newlines) without ever entering shell source. Terminated rather than
// delimited so a single EMPTY element ([""], docker's clear-entrypoint idiom)
// encodes to "\x00" and stays distinguishable from an absent override
// (review 759). "" only when the argv is empty.
func argvB64(argv []string) string {
	if len(argv) == 0 {
		return ""
	}
	var b strings.Builder
	for _, a := range argv {
		b.WriteString(a)
		b.WriteByte(0)
	}
	return base64.StdEncoding.EncodeToString([]byte(b.String()))
}

// envFileB64 renders env vars as a docker --env-file (KEY=VALUE per line),
// base64-enveloped. Values are newline-free by construction (the API refuses
// control bytes in custom-image env values), so the line format cannot be
// broken from the outside. "" when empty.
func envFileB64(env []provider.EnvVar) string {
	if len(env) == 0 {
		return ""
	}
	var b strings.Builder
	for _, e := range env {
		b.WriteString(e.Key)
		b.WriteByte('=')
		b.WriteString(e.Value)
		b.WriteByte('\n')
	}
	return base64.StdEncoding.EncodeToString([]byte(b.String()))
}

// customStartB64 renders the launch script for a customer image's own process
// (custom-images Phase 1): a #!/bin/sh exec line over the image config's
// Entrypoint+Cmd argv, single-quoted per argument, then base64-encoded so the
// bootstrap template never interpolates a customer byte into shell source.
// Empty argv (image with neither Entrypoint nor Cmd) returns "" — the
// bootstrap renders no launch block and the instance is SSH-only.
func customStartB64(entrypoint, cmd []string) string {
	argv := make([]string, 0, len(entrypoint)+len(cmd))
	argv = append(argv, entrypoint...)
	argv = append(argv, cmd...)
	if len(argv) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("#!/bin/sh\nexec")
	for _, a := range argv {
		b.WriteString(" '")
		b.WriteString(strings.ReplaceAll(a, "'", `'\''`))
		b.WriteString("'")
	}
	b.WriteString("\n")
	return base64.StdEncoding.EncodeToString([]byte(b.String()))
}

// Image-capability allow-sets. Each is deny-by-default: an adapter not listed
// here is treated as unable to take the corresponding image path, so a
// forgotten entry can only cost capacity, never boot a customer the wrong
// image. Of the built-in adapters only "generic" is container-native ("vultr"
// is a VM adapter and hosts customer images through CustomerImageVMCapable).
// Tests add fixture provider names to these sets.
var (
	// customImageProviders backs SupportsCustomImage.
	customImageProviders = map[string]bool{"generic": true}
	// customerImageProviders backs SupportsCustomerImage. Must stay a subset
	// of customImageProviders.
	customerImageProviders = map[string]bool{"generic": true}
	// entrypointArgProviders backs BootstrapRidesImageEntrypoint. No built-in
	// adapter starts the bootstrap as ENTRYPOINT arguments.
	entrypointArgProviders = map[string]bool{}
	// rawVMProviders backs SupportsRawVM. No built-in adapter exposes the
	// raw-vm environment class.
	rawVMProviders = map[string]bool{}
)

// SupportsCustomImage reports whether a registered provider honors a custom
// Docker image + command (TMPL-05 / D2). Container-native providers do;
// VM-image and fixed-template providers do not.
//
// This is the capability gate the deploy entrypoint (Plan 07) consults before
// routing a template deploy to a provider: a template ships a custom container
// image, so landing it on a provider that ignores the image would silently boot
// the wrong/default image (T-31-05a Elevation). Providers not in the allow-list
// are rejected by default — adding a new container-native provider is an
// explicit opt-in here.
func SupportsCustomImage(providerName string) bool {
	return customImageProviders[providerName]
}

// SupportsCustomerImage reports whether a provider may run a CUSTOMER-supplied
// image (custom-images Phase 1). Deliberately a strict subset of
// SupportsCustomImage: the template path trusts the catalog's server-controlled
// refs, while this path carries arbitrary (validated) customer refs, so a
// container-native provider with operational rough edges (prepaid billing on
// failed deploys, a generic Failed status, a single-HTTP-port model) can take
// templates while staying out of this set. VM and fixed-template providers are
// out of it (VM providers host customer images via CustomerImageVMCapable).
// Deny-by-default like every other capability gate here.
func SupportsCustomerImage(providerName string) bool {
	return customerImageProviders[providerName]
}

// BootstrapRidesImageEntrypoint reports whether the provider starts our
// container bootstrap as ARGUMENTS to the customer image's own ENTRYPOINT
// rather than as the container's full command. On such a provider PID 1 is
// `docker-init -- <image-entrypoint> bash -c <bootstrap>`, so the bootstrap
// only runs when the entrypoint follows the exec-"$@" convention — a
// fixed-argv or shell-form ENTRYPOINT never starts it and the instance never
// becomes ready. Providers whose own init owns PID 1, and generic (the
// partner contract executes startup_script itself), run the bootstrap outside
// the image's process and are safe. Placement steers ENTRYPOINT-bearing
// customer images away from providers listed here.
func BootstrapRidesImageEntrypoint(providerName string) bool {
	return entrypointArgProviders[providerName]
}

// SupportsRawVM reports whether a provider exposes a raw VM we bootstrap
// ourselves (cloud-init / startup script) and can therefore expose the
// "raw-vm" environment class (Phase 63): explicit root VM, no Certified
// Image. Container-native providers (generic) return false because they run
// our image natively rather than a VM, and are gated to the certified/container
// path instead. The class label is server-derived — never provider identity
// leaked to the client.
func SupportsRawVM(providerName string) bool {
	return rawVMProviders[providerName]
}

// autoSelectsGPU reports whether this request is a template deploy that omitted
// gpu_type and therefore wants the engine to auto-select the cheapest eligible
// GPU (TMPL-04). When true, SelectProviderCandidates filters offerings by the
// template's VRAM floor (req.MinVRAMGB) — with the GPU-count floor folded into
// req.GPUCount — instead of an exact gpu_type match, and the matched offering's
// GPUType becomes the selection. Raw instances and explicit-gpu_type template
// deploys return false and keep the exact-match path byte-for-byte.
//
// GPU-87 note: the exact-match offering loop is still byte-for-byte, but an
// explicit-gpu_type TEMPLATE request now passes a type-level floor pre-check
// (TemplateVRAMShortfall) before that loop runs — the floor applies to both
// template paths, just by different mechanisms (filter vs. outright refusal).
//
// The eligibility predicate runs inline in SelectProviderCandidates against the
// per-provider provider.GPUOffering set (which carries VRAMPerGPUGB AND provider
// identity), so it composes naturally with the SupportsCustomImage gate — unlike
// a filter over the provider-anonymized availability set, which cannot see
// provider identity. (An earlier filterEligibleOfferings helper operated on the
// anonymized availability.AvailableOffering set and so was never wireable into
// the per-provider candidate loop; it has been replaced by this inline path.)
func (r ProvisionRequest) autoSelectsGPU() bool {
	return r.TemplateID != nil && r.GPUType == ""
}
