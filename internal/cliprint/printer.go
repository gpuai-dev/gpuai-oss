// Package cliprint implements per-resource output formatters for the gpu
// CLI. One method per type — no runtime introspection. JSON via
// encoding/json, table via text/tabwriter. See research §5 for the rationale.
package cliprint

import "io"

const (
	FormatJSON  = "json"
	FormatTable = "table"
)

// derefOr returns *s if non-nil and non-empty, else fallback.
func derefOr(s *string, fallback string) string {
	if s == nil || *s == "" {
		return fallback
	}
	return *s
}

// writerFlusher is a tiny helper interface for sites that want to capture
// writer output after tabwriter.Flush. Kept for symmetry with future
// per-resource formatters that may use it.
type writerFlusher interface {
	io.Writer
	Flush() error
}

// ensure unused interface stays referenced so unused-linter doesn't grumble
// — assigned to package-private blank var.
var _ writerFlusher = (*nopFlusher)(nil)

type nopFlusher struct{ io.Writer }

func (nopFlusher) Flush() error { return nil }
