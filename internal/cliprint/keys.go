package cliprint

import (
	"encoding/json"
	"fmt"
	"io"
	"text/tabwriter"
	"time"

	"github.com/gpuai-dev/gpuai-oss/internal/apiclient"
)

// SSHKeys renders a list of SSH keys in the requested format. Table columns:
// ID NAME FINGERPRINT ON-LAUNCH CREATED.
func SSHKeys(format string, w io.Writer, items []apiclient.SSHKey) error {
	switch format {
	case FormatJSON, "":
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(items)
	case FormatTable:
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "ID\tNAME\tFINGERPRINT\tON-LAUNCH\tCREATED")
		for _, k := range items {
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n",
				k.ID, k.Name, k.Fingerprint, attachLabel(k.AttachOnProvision), k.CreatedAt.Format(time.RFC3339))
		}
		return tw.Flush()
	default:
		return fmt.Errorf("unknown output format: %s", format)
	}
}

// attachLabel renders the attach-on-provision flag for table output.
func attachLabel(on bool) string {
	if on {
		return "yes"
	}
	return "no"
}

// SSHKey renders a single SSH key as a vertical key/value table (or JSON).
// Used after `gpu keys create` to echo back what was uploaded.
func SSHKey(format string, w io.Writer, k apiclient.SSHKey) error {
	switch format {
	case FormatJSON, "":
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(k)
	case FormatTable:
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		fmt.Fprintf(tw, "ID:\t%s\n", k.ID)
		fmt.Fprintf(tw, "Name:\t%s\n", k.Name)
		fmt.Fprintf(tw, "Fingerprint:\t%s\n", k.Fingerprint)
		fmt.Fprintf(tw, "Attach on launch:\t%s\n", attachLabel(k.AttachOnProvision))
		fmt.Fprintf(tw, "Created:\t%s\n", k.CreatedAt.Format(time.RFC3339))
		return tw.Flush()
	default:
		return fmt.Errorf("unknown output format: %s", format)
	}
}
