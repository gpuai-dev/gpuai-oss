package cliprint

import (
	"encoding/json"
	"fmt"
	"io"
	"text/tabwriter"

	"github.com/gpuai-dev/gpuai-oss/internal/apiclient"
)

// RegistryCredentialDetail renders one stored registry login (no secret).
func RegistryCredentialDetail(format string, w io.Writer, c apiclient.RegistryCredential) error {
	switch format {
	case FormatJSON:
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(c)
	default:
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		fmt.Fprintf(tw, "ID:\t%s\n", c.ID)
		fmt.Fprintf(tw, "Name:\t%s\n", c.Name)
		fmt.Fprintf(tw, "Registry:\t%s\n", c.Registry)
		fmt.Fprintf(tw, "Username:\t%s\n", c.Username)
		fmt.Fprintf(tw, "Created:\t%s\n", c.CreatedAt.Format("2006-01-02 15:04:05 MST"))
		return tw.Flush()
	}
}

// RegistryCredentialList renders the login table (no secret material).
func RegistryCredentialList(format string, w io.Writer, creds []apiclient.RegistryCredential) error {
	switch format {
	case FormatJSON:
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(creds)
	default:
		if len(creds) == 0 {
			fmt.Fprintln(w, "No registry credentials. Add one with `gpu registry-credentials add`.")
			return nil
		}
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "ID\tNAME\tREGISTRY\tUSERNAME\tCREATED")
		for _, c := range creds {
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", c.ID, c.Name, c.Registry, c.Username, c.CreatedAt.Format("2006-01-02"))
		}
		return tw.Flush()
	}
}
