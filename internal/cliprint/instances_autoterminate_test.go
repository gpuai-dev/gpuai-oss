package cliprint

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/gpuai-dev/gpuai-oss/internal/apiclient"
)

// TestInstanceDetail_AutoTerminateRow covers the three states a runtime limit
// can be read in: armed (hours + resolved deadline), requested but not
// yet armed (still provisioning — the deadline only exists once the instance is
// running), and absent. The absent case is an assertion about NOISE: an
// instance with no limit must not grow a row saying so.
func TestInstanceDetail_AutoTerminateRow(t *testing.T) {
	deadline := time.Date(2026, 9, 2, 18, 0, 0, 0, time.UTC)
	hours := 8

	t.Run("armed_shows_deadline_and_limit", func(t *testing.T) {
		inst := sampleInstance()
		inst.AutoTerminateHours = &hours
		inst.AutoTerminateAt = &deadline
		buf := &bytes.Buffer{}
		if err := InstanceDetail(FormatTable, buf, inst); err != nil {
			t.Fatalf("InstanceDetail: %v", err)
		}
		out := buf.String()
		if !strings.Contains(out, "Auto-terminate:") {
			t.Fatalf("auto-terminate row missing: %s", out)
		}
		if !strings.Contains(out, "2026-09-02T18:00:00Z") {
			t.Errorf("deadline missing — the timestamp is the whole point, the hours alone are not "+
				"derivable into one: %s", out)
		}
		if !strings.Contains(out, "8h limit") {
			t.Errorf("requested limit missing: %s", out)
		}
	})

	t.Run("requested_but_not_running_shows_pending_limit", func(t *testing.T) {
		inst := sampleInstance()
		inst.Status = "starting"
		inst.AutoTerminateHours = &hours
		buf := &bytes.Buffer{}
		if err := InstanceDetail(FormatTable, buf, inst); err != nil {
			t.Fatalf("InstanceDetail: %v", err)
		}
		if out := buf.String(); !strings.Contains(out, "8h after it starts running") {
			t.Errorf("a limit with no deadline yet must say when the clock starts: %s", out)
		}
	})

	t.Run("no_limit_omits_the_row", func(t *testing.T) {
		buf := &bytes.Buffer{}
		if err := InstanceDetail(FormatTable, buf, sampleInstance()); err != nil {
			t.Fatalf("InstanceDetail: %v", err)
		}
		if out := buf.String(); strings.Contains(out, "Auto-terminate") {
			t.Errorf("unbounded instance grew an auto-terminate row: %s", out)
		}
	})
}

// TestInstances_AutoTerminateColumn pins the list table's conditional column:
// it appears only when some instance in the listing actually has a limit, so
// the common all-unbounded fleet listing is byte-for-byte what it was.
func TestInstances_AutoTerminateColumn(t *testing.T) {
	deadline := time.Date(2026, 9, 2, 18, 0, 0, 0, time.UTC)
	hours := 8

	t.Run("absent_when_nothing_is_bounded", func(t *testing.T) {
		buf := &bytes.Buffer{}
		if err := Instances(FormatTable, buf, []apiclient.Instance{sampleInstance()}); err != nil {
			t.Fatalf("Instances: %v", err)
		}
		if out := buf.String(); strings.Contains(out, "AUTO-TERM") {
			t.Errorf("AUTO-TERM column shown for a fleet with no limits: %s", out)
		}
	})

	t.Run("present_when_any_instance_is_bounded", func(t *testing.T) {
		bounded := sampleInstance()
		bounded.ID = "gpu-bound"
		bounded.AutoTerminateHours = &hours
		bounded.AutoTerminateAt = &deadline

		buf := &bytes.Buffer{}
		if err := Instances(FormatTable, buf, []apiclient.Instance{sampleInstance(), bounded}); err != nil {
			t.Fatalf("Instances: %v", err)
		}
		out := buf.String()
		if !strings.Contains(out, "AUTO-TERM") {
			t.Fatalf("AUTO-TERM column missing though an instance is bounded: %s", out)
		}
		if !strings.Contains(out, "2026-09-02T18:00:00Z") {
			t.Errorf("bounded row missing its deadline: %s", out)
		}
		// The unbounded sibling renders a dash, not a fabricated deadline.
		for _, line := range strings.Split(out, "\n") {
			if strings.HasPrefix(line, "gpu-abc") && !strings.HasSuffix(strings.TrimRight(line, " "), "-") {
				t.Errorf("unbounded row must end in a dash under AUTO-TERM: %q", line)
			}
		}
	})
}
