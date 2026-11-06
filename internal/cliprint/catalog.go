package cliprint

import (
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/gpuai-dev/gpuai-oss/internal/apiclient"
)

// specCell renders a hardware-spec count for a table cell: the number itself,
// or "-" when it is 0 — an unreported/unknown spec, which reads better than a
// misleading "0 vCPU". Shared by the gpu-types and pricing renderers.
func specCell(n int) string {
	if n <= 0 {
		return "-"
	}
	return strconv.Itoa(n)
}

// GPUTypes renders the GPU type catalog. Table columns: GPU TYPE, VRAM (GB),
// VCPU, RAM (GB), STORAGE (GB). VRAM is the card's nameplate figure, or the
// smallest currently-offered variant for a model that spans capacities (always
// the value every launch can honor); the CPU/RAM/storage
// columns are the model's REPRESENTATIVE host config (exact per-offering specs
// live in `gpu pricing`). An unreported spec (0) renders as "-".
func GPUTypes(format string, w io.Writer, items []apiclient.GPUType) error {
	switch format {
	case FormatJSON, "":
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(items)
	case FormatTable:
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "GPU TYPE\tVRAM (GB)\tVCPU\tRAM (GB)\tSTORAGE (GB)")
		for _, t := range items {
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n",
				t.GPUType, specCell(t.VRAMGB), specCell(t.CPUCores), specCell(t.RAMGB), specCell(t.StorageGB))
		}
		return tw.Flush()
	default:
		return fmt.Errorf("unknown output format: %s", format)
	}
}

// Pricing renders the (gpu_type, region, tier) price grid. Available is
// the integer count of units provisionable now — useful for capacity
// planning at a glance. VCPU and RAM (GB) are the exact host hardware for the
// offering, tied to the shown (cheapest) price; an unreported spec renders as
// "-". DISK (GB) is the filesystem a launch actually receives
// (instance_disk_gb) — a trailing "+" marks capacity that accepts a custom
// size via `gpu instances create --disk`; pre-field servers fall back to the
// host storage figure. BOOT distinguishes instant capacity (reachable in
// under a minute) from slow-boot capacity (~3-5 min to provision). SOURCE
// is the offering's capacity class — "secure" (datacenter-operated) vs
// "community" (supplier/peer-hosted, softer SLA) — a class label only,
// never supplier identity.
// ENV labels the image class (certified / raw-vm / provider-template) and TYPE
// the deployment class (vm / container) — both derived from provider ATTRIBUTES
// (CertifiedImage + DeploymentType), never provider identity. For certified rows
// the per-machine pinned-environment spec is printed as a footnote
// block after the table: image tag, digest (short), driver floor, CUDA.
func Pricing(format string, w io.Writer, items []apiclient.PricingRow) error {
	switch format {
	case FormatJSON, "":
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(items)
	case FormatTable:
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "GPU TYPE\tCOUNT\tVCPU\tRAM (GB)\tDISK (GB)\tREGION\tTIER\tCOST/HR\tAVAILABLE\tBOOT\tSOURCE\tENV\tTYPE")
		for _, p := range items {
			boot := "instant"
			if !p.InstantBoot {
				boot = "~3-5 min"
			}
			// Prefer the server's per-offering capacity class ("secure" /
			// "community"); fall back to the Community bool for older servers
			// (or pre-field cache rows) that don't send it.
			source := p.CapacityClass
			if source == "" {
				source = "secure"
				if p.Community {
					source = "community"
				}
			}
			// DISK is the filesystem a launch actually receives
			// (instance_disk_gb), suffixed "+" when the row accepts a custom
			// --disk. Pre-field servers send 0 → fall back to the displayed
			// host storage figure so the column never regresses to "-" during
			// a rollout window.
			disk := p.InstanceDiskGB
			if disk == 0 {
				disk = p.StorageGB
			}
			diskCell := specCell(disk)
			if p.DiskConfigurable && diskCell != "-" {
				diskCell += "+"
			}
			fmt.Fprintf(tw, "%s\t%d\t%s\t%s\t%s\t%s\t%s\t$%.2f\t%d\t%s\t%s\t%s\t%s\n",
				p.GPUType, p.GPUCount, specCell(p.CPUCores), specCell(p.RAMGB), diskCell,
				p.Region, p.Tier, p.PricePerHour, p.Available, boot, source,
				envLabel(p), deploymentLabel(p.DeploymentType))
		}
		if err := tw.Flush(); err != nil {
			return err
		}
		printEnvSpecs(w, items)
		return nil
	default:
		return fmt.Errorf("unknown output format: %s", format)
	}
}

// envLabel maps the (CertifiedImage, DeploymentType) attribute pair to an image
// class label: "certified" when the row can take the Certified Image,
// "raw-vm" for a VM we bootstrap that cannot, and "provider-template" for
// container-native (or otherwise non-normalizable) capacity where the runtime
// template comes from the provider. Never encodes provider identity.
func envLabel(p apiclient.PricingRow) string {
	if p.CertifiedImage {
		return "certified"
	}
	if p.DeploymentType == "vm" {
		return "raw-vm"
	}
	return "provider-template"
}

// deploymentLabel renders the deployment class; a pre-field ("") row shows "-".
func deploymentLabel(dt string) string {
	if dt == "" {
		return "-"
	}
	return dt
}

// shortDigest trims a digest-pinned image ref to the first 12 hex chars of its
// sha256. Falls back to the raw string when
// no "@sha256:" segment is present.
func shortDigest(digest string) string {
	if i := strings.Index(digest, "@sha256:"); i >= 0 {
		hex := digest[i+len("@sha256:"):]
		if len(hex) > 12 {
			hex = hex[:12]
		}
		return "sha256:" + hex
	}
	if len(digest) > 12 {
		return digest[:12]
	}
	return digest
}

// printEnvSpecs writes the per-machine pinned-environment spec for every row
// that carries a published environment contract (certified-capable rows). It is
// a footnote after the price grid so the columnar table stays aligned. Rows
// without a contract (nil Environment) contribute nothing.
func printEnvSpecs(w io.Writer, items []apiclient.PricingRow) {
	seen := make(map[string]struct{})
	var first bool
	for _, p := range items {
		if p.Environment == nil {
			continue
		}
		if _, ok := seen[p.GPUType]; ok {
			continue
		}
		seen[p.GPUType] = struct{}{}
		if !first {
			fmt.Fprintln(w, "\nCertified environment (pinned per machine):")
			first = true
		}
		e := p.Environment
		fmt.Fprintf(w, "  %s: image=%s digest=%s driver>=%s cuda=%s\n",
			p.GPUType, e.Image, shortDigest(e.ImageDigest), e.DriverMin, e.CUDA)
	}
}
