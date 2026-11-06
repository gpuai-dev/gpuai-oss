package cliprint

// operations.go — formatter for `gpu operations get`. Renders the async
// operation's lifecycle state and, on failure, its structured error payload.
//
// The operation's Result (*apiclient.OperationResult) is deliberately NOT
// rendered in table mode: app coordinates belong to `gpu instances get` and
// `gpu instances open`, which fetch them with the explicit credential opt-in.

import (
	"encoding/json"
	"fmt"
	"io"
	"text/tabwriter"
	"time"

	"github.com/gpuai-dev/gpuai-oss/internal/apiclient"
)

// Operation renders a single async operation as a vertical key/value table
// (or JSON). Optional fields — resource id, error, completion time — are
// printed only when the server populated them.
func Operation(format string, w io.Writer, op apiclient.Operation) error {
	switch format {
	case FormatJSON, "":
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(op)
	case FormatTable:
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		fmt.Fprintf(tw, "Operation ID:\t%s\n", op.OperationID)
		fmt.Fprintf(tw, "Kind:\t%s\n", op.Kind)
		fmt.Fprintf(tw, "State:\t%s\n", op.State)
		if op.ResourceID != nil {
			fmt.Fprintf(tw, "Resource ID:\t%s\n", *op.ResourceID)
		}
		if op.Error != nil {
			fmt.Fprintf(tw, "Error Code:\t%s\n", op.Error.Code)
			fmt.Fprintf(tw, "Error Detail:\t%s\n", op.Error.Detail)
		}
		fmt.Fprintf(tw, "Created:\t%s\n", op.CreatedAt.Format(time.RFC3339))
		fmt.Fprintf(tw, "Updated:\t%s\n", op.UpdatedAt.Format(time.RFC3339))
		if op.CompletedAt != nil {
			fmt.Fprintf(tw, "Completed:\t%s\n", op.CompletedAt.Format(time.RFC3339))
		}
		return tw.Flush()
	default:
		return fmt.Errorf("unknown output format: %s", format)
	}
}
