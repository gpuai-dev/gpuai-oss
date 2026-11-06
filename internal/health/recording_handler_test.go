package health

import (
	"context"
	"log/slog"
	"sync"
)

// recordingHandler captures slog records so the tests can assert on the ALERT
// LINES, which are the alert (ALRT-02): a log-based alert matches the exact
// message string, so an assertion on the string is an assertion on the pager.
type recordingHandler struct {
	mu      sync.Mutex
	records []slog.Record
}

func (h *recordingHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *recordingHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.records = append(h.records, r.Clone())
	return nil
}

func (h *recordingHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *recordingHandler) WithGroup(string) slog.Handler      { return h }

// errorMessages returns every ERROR-level message captured so far.
func (h *recordingHandler) errorMessages() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []string
	for _, r := range h.records {
		if r.Level >= slog.LevelError {
			out = append(out, r.Message)
		}
	}
	return out
}

func (h *recordingHandler) hasError(msg string) bool {
	for _, m := range h.errorMessages() {
		if m == msg {
			return true
		}
	}
	return false
}
