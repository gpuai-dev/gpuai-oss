package cliprint

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/gpuai-dev/gpuai-oss/internal/apiclient"
)

// Instances renders a list of instances in the requested format. Table layout:
// ID NAME STATUS GPU COUNT REGION COST/HR CREATED. Empty list still emits
// the header row in table mode so the user sees the schema.
func Instances(format string, w io.Writer, items []apiclient.Instance) error {
	switch format {
	case FormatJSON, "":
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(items)
	case FormatTable:
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		// AUTO-TERM appears only when at least one listed instance actually has
		// a runtime limit. A permanent column would be a dash on
		// essentially every row for every customer, and the point of the
		// listing is the fleet at a glance; when a limit IS in play, when it
		// fires is exactly what you want to see next to the instance.
		showAutoTerm := false
		for _, it := range items {
			if it.AutoTerminateHours != nil {
				showAutoTerm = true
				break
			}
		}
		header := "ID\tNAME\tSTATUS\tGPU\tCOUNT\tREGION\tCOST/HR\tCREATED"
		if showAutoTerm {
			header += "\tAUTO-TERM"
		}
		fmt.Fprintln(tw, header)
		for _, it := range items {
			name := derefOr(it.Name, "-")
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%d\t%s\t$%.2f\t%s",
				it.ID, name, it.Status, it.GPUType, it.GPUCount, it.Region,
				it.PricePerHour, it.CreatedAt.Format(time.RFC3339))
			if showAutoTerm {
				fmt.Fprintf(tw, "\t%s", autoTerminateCell(it))
			}
			fmt.Fprintln(tw)
		}
		return tw.Flush()
	default:
		return fmt.Errorf("unknown output format: %s", format)
	}
}

// autoTerminateCell renders one instance's runtime limit for the list table
// : the resolved deadline once the server has one, the requested hours
// while the instance is still provisioning, and "-" for an instance with no
// limit (which only appears when some OTHER row in the same listing has one).
func autoTerminateCell(it apiclient.Instance) string {
	if it.AutoTerminateHours == nil {
		return "-"
	}
	if it.AutoTerminateAt != nil {
		return it.AutoTerminateAt.Format(time.RFC3339)
	}
	return fmt.Sprintf("in %dh", *it.AutoTerminateHours)
}

// environmentBrand maps a server-resolved environment class to its customer-
// facing branding string. A certified:<framework> sub-choice
// renders the certified brand with the framework's display name.
// Unknown or empty classes render nothing so the caller omits the row. Strings
// are verbatim product copy.
func environmentBrand(env string) string {
	switch env {
	case "certified":
		return "Certified Image"
	case "raw_vm":
		return "VM · root access"
	case "provider_template":
		return "Provider template"
	}
	if fw, ok := strings.CutPrefix(env, "certified:"); ok && fw != "" {
		return "Certified Image · " + frameworkBrand(fw)
	}
	return ""
}

// frameworkBrand maps a framework id to its display name; an id this build
// predates falls back to the raw id rather than rendering nothing.
func frameworkBrand(fw string) string {
	switch fw {
	case "pytorch":
		return "PyTorch"
	case "cuda-devel":
		return "CUDA devel"
	case "jupyter":
		return "JupyterLab"
	default:
		return fw
	}
}

// InstanceDetail renders a single instance as a vertical key/value table
// (or pretty JSON). Includes Connection details when present.
func InstanceDetail(format string, w io.Writer, it apiclient.Instance) error {
	switch format {
	case FormatJSON, "":
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(it)
	case FormatTable:
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		fmt.Fprintf(tw, "ID:\t%s\n", it.ID)
		fmt.Fprintf(tw, "Name:\t%s\n", derefOr(it.Name, "-"))
		fmt.Fprintf(tw, "Status:\t%s\n", it.Status)
		// The why behind an error status — present only when the server set one.
		if it.StatusReason != nil && *it.StatusReason != "" {
			fmt.Fprintf(tw, "Status Reason:\t%s\n", *it.StatusReason)
		}
		fmt.Fprintf(tw, "GPU Type:\t%s\n", it.GPUType)
		fmt.Fprintf(tw, "GPU Count:\t%d\n", it.GPUCount)
		fmt.Fprintf(tw, "Tier:\t%s\n", it.Tier)
		fmt.Fprintf(tw, "Region:\t%s\n", it.Region)
		fmt.Fprintf(tw, "Cost/hour:\t$%.2f\n", it.PricePerHour)
		// Disk renders only when the platform configured it (nil = the
		// machine's own provider-determined disk — omit rather than guess).
		if it.DiskGB != nil {
			fmt.Fprintf(tw, "Disk:\t%d GB\n", *it.DiskGB)
		}
		// The customer's own image ref (custom-image launches only).
		if it.Image != nil && *it.Image != "" {
			fmt.Fprintf(tw, "Image:\t%s\n", *it.Image)
		}
		if brand := environmentBrand(it.Environment); brand != "" {
			fmt.Fprintf(tw, "Environment:\t%s\n", brand)
		}
		if it.DriverVersion != "" {
			fmt.Fprintf(tw, "Driver:\t%s\n", it.DriverVersion)
		}
		if it.CUDAVersion != "" {
			fmt.Fprintf(tw, "CUDA:\t%s\n", it.CUDAVersion)
		}
		if it.ImageDigest != "" {
			fmt.Fprintf(tw, "Image Digest:\t%s\n", it.ImageDigest)
		}
		fmt.Fprintf(tw, "Created:\t%s\n", it.CreatedAt.Format(time.RFC3339))
		if it.ReadyAt != nil {
			fmt.Fprintf(tw, "Ready:\t%s\n", it.ReadyAt.Format(time.RFC3339))
		}
		if it.TerminatedAt != nil {
			fmt.Fprintf(tw, "Terminated:\t%s\n", it.TerminatedAt.Format(time.RFC3339))
		}
		// Last verified SSH reachability. Omitted entirely when nil: the server
		// sends nothing for an instance it has not probed yet, and printing
		// "never" would read as "unreachable" for a healthy fresh launch.
		if it.LastReachableAt != nil {
			fmt.Fprintf(tw, "Last reachable:\t%s\n", it.LastReachableAt.Format(time.RFC3339))
		}
		// The confirmed outage start, present only while Status is
		// "unreachable". Worth its own row rather than folding into
		// the status: it says how long the instance has been down.
		if it.UnreachableSince != nil {
			fmt.Fprintf(tw, "Unreachable since:\t%s\n", it.UnreachableSince.Format(time.RFC3339))
		}
		// The per-instance runtime limit. Printed as one row with the
		// deadline where the server has resolved one, because the two numbers
		// only mean something as a pair: the hours say what was asked for, the
		// timestamp says when it actually happens (the clock starts at running,
		// so it is not derivable from the launch time). Omitted entirely for an
		// instance with no limit — the overwhelmingly common case.
		if it.AutoTerminateHours != nil {
			if it.AutoTerminateAt != nil {
				fmt.Fprintf(tw, "Auto-terminate:\t%s (%dh limit)\n",
					it.AutoTerminateAt.Format(time.RFC3339), *it.AutoTerminateHours)
			} else {
				fmt.Fprintf(tw, "Auto-terminate:\t%dh after it starts running\n", *it.AutoTerminateHours)
			}
		}
		if it.Connection != nil {
			fmt.Fprintf(tw, "Hostname:\t%s\n", it.Connection.Hostname)
			fmt.Fprintf(tw, "Port:\t%d\n", it.Connection.Port)
			fmt.Fprintf(tw, "SSH Command:\t%s\n", it.Connection.SSHCommand)
			if it.Connection.AppURL != "" {
				fmt.Fprintf(tw, "App URL:\t%s\n", it.Connection.AppURL)
			}
			if it.Connection.TerminalURL != "" {
				fmt.Fprintf(tw, "Console:\t%s\n", it.Connection.TerminalURL)
			}
			if it.Connection.AppUser != "" {
				fmt.Fprintf(tw, "App Login:\t%s / %s\n", it.Connection.AppUser, it.Connection.AppPassword)
			}
		}
		return tw.Flush()
	default:
		return fmt.Errorf("unknown output format: %s", format)
	}
}
