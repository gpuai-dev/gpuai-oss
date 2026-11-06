package cliprint

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/gpuai-dev/gpuai-oss/internal/apiclient"
)

// Environments renders the launch environment catalog (GET /v1/environments):
// the certified framework roster with its version-picker options and the raw-VM
// OS catalog. Table mode prints two sections — FRAMEWORKS (with the exact
// `--environment` token to pass) and OPERATING SYSTEMS — so a user can read off
// what to hand `gpu instances create --environment`.
func Environments(format string, w io.Writer, env apiclient.Environments) error {
	switch format {
	case FormatJSON, "":
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(env)
	case FormatTable:
		printFrameworks(w, env)
		fmt.Fprintln(w)
		printVMImages(w, env)
		return nil
	default:
		return fmt.Errorf("unknown output format: %s", format)
	}
}

// printFrameworks renders the certified framework roster. Each framework lists
// its selectable versions (the `certified:<fw>@<ver>` picker) when it has more
// than one; single-version frameworks show the token without a version. The
// bare certified base image is always available as `certified`.
func printFrameworks(w io.Writer, env apiclient.Environments) {
	fmt.Fprintln(w, "FRAMEWORKS  — launch with:  gpu instances create ... --environment <token>")
	fmt.Fprintln(w, "  certified                       Certified base image (drivers + CUDA)")

	frameworks := append([]string(nil), env.Frameworks...)
	sort.Strings(frameworks)
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	for _, fw := range frameworks {
		versions := env.Versions[fw]
		if len(versions) == 0 {
			fmt.Fprintf(tw, "  certified:%s\t%s\n", fw, includesSummary(env.Includes[fw]))
			continue
		}
		// Multi-version: print the bare token as the default, then one row per
		// pinned version with the exact `@<ver>` token to pass.
		fmt.Fprintf(tw, "  certified:%s\t%s\n", fw, includesSummary(env.Includes[fw]))
		vs := append([]apiclient.EnvFrameworkVersion(nil), versions...)
		sort.Slice(vs, func(i, j int) bool { return vs[i].ID < vs[j].ID })
		for _, v := range vs {
			tok := fmt.Sprintf("  certified:%s@%s", fw, v.ID)
			note := v.Label + " · " + cudaLabel(v.Variant)
			if v.Default {
				note += " (default)"
			}
			fmt.Fprintf(tw, "%s\t%s\n", tok, note)
		}
	}
	_ = tw.Flush()
}

// printVMImages renders the raw-VM OS catalog (the `raw_vm:<os>` picker). An os
// only bootable on some providers still appears here — availability per machine
// is surfaced on the offering (os_options), not in this global catalog.
func printVMImages(w io.Writer, env apiclient.Environments) {
	if len(env.VMImages) == 0 {
		return
	}
	fmt.Fprintln(w, "OPERATING SYSTEMS (raw VM)  — launch with:  --environment raw_vm:<os-id>")
	ids := make([]string, 0, len(env.VMImages))
	for id := range env.VMImages {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "  OS ID\tLABEL\tDRIVER")
	for _, id := range ids {
		img := env.VMImages[id]
		driver := "install at boot"
		if img.DriverPreinstalled {
			driver = "preinstalled"
		}
		fmt.Fprintf(tw, "  %s\t%s\t%s\n", id, img.Label, driver)
	}
	_ = tw.Flush()
}

// includesSummary renders a compact one-line "what's included" hint from the
// framework's includes across CUDA variants — the headline components with a
// version, deduped and capped. Empty when no includes are published.
func includesSummary(byVariant map[string][]apiclient.EnvIncludeItem) string {
	if len(byVariant) == 0 {
		return ""
	}
	seen := map[string]bool{}
	var parts []string
	// Deterministic variant order so the summary is stable.
	variants := make([]string, 0, len(byVariant))
	for v := range byVariant {
		variants = append(variants, v)
	}
	sort.Strings(variants)
	for _, variant := range variants {
		for _, it := range byVariant[variant] {
			if it.Version == "" || seen[it.Name] {
				continue
			}
			seen[it.Name] = true
			parts = append(parts, it.Name+" "+it.Version)
			if len(parts) >= 4 {
				return strings.Join(parts, ", ")
			}
		}
	}
	return strings.Join(parts, ", ")
}

// cudaLabel turns a CUDA variant key into a display string: "cuda128" → "CUDA 12.8".
func cudaLabel(variant string) string {
	digits := strings.TrimPrefix(variant, "cuda")
	if len(digits) >= 3 {
		return "CUDA " + digits[:len(digits)-1] + "." + digits[len(digits)-1:]
	}
	return variant
}
