package provision

import (
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/gpuai-dev/gpuai-oss/internal/availability"
	"github.com/gpuai-dev/gpuai-oss/internal/db"
	"github.com/gpuai-dev/gpuai-oss/internal/provider"
)

func silentQuarantine() *availability.HostQuarantine {
	return availability.NewHostQuarantine(availability.DefaultHostQuarantineHold, availability.DefaultHostQuarantineMax,
		slog.New(slog.NewTextHandler(io.Discard, nil)))
}

// Only a tunnel stall follows the machine: the container ran and the
// bootstrap never phoned home. A dead pull, a verdict without a host, a
// customer image, and an engine without a quarantine hold nothing.
func TestNoteStallVerdict(t *testing.T) {
	q := silentQuarantine()
	e := &Engine{}
	e.SetHostQuarantine(q)

	e.noteStallVerdict("marketco", "51178", verdictTunnelStall, false)
	if !q.Quarantined("marketco", "51178") {
		t.Error("a tunnel stall must quarantine the machine")
	}
	e.noteStallVerdict("marketco", "116630", verdictDeadPull, false)
	if q.Quarantined("marketco", "116630") {
		t.Error("a dead pull must not quarantine: most such hosts boot when rented again")
	}
	e.noteStallVerdict("marketco", "", verdictTunnelStall, false)
	if q.Quarantined("marketco", "") {
		t.Error("a verdict without a host identity holds nothing")
	}
	e.noteStallVerdict("marketco", "52470", verdictTunnelStall, true)
	if q.Quarantined("marketco", "52470") {
		t.Error("a customer image's tunnel stall must not quarantine: an image without bash, gunzip or base64 never phones home anywhere")
	}
	(&Engine{}).noteStallVerdict("marketco", "1", verdictTunnelStall, false) // nil quarantine: no panic
}

// The verdict site reads the image kind from the launch itself: the same
// tunnel stall holds the machine under a platform image and leaves it on sale
// under a customer image.
func TestProgressStatus_TunnelStallQuarantinesOnlyPlatformImages(t *testing.T) {
	for _, tc := range []struct {
		name          string
		customerImage bool
		wantHeld      bool
	}{
		{name: "platform image", customerImage: false, wantHeld: true},
		{name: "customer image", customerImage: true, wantHeld: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Container up on machine 51178, bootstrap silent.
			stub := &rescueCapableStub{&stubProvider{
				name:         "marketco",
				statusResult: &provider.InstanceStatus{Status: "running", UpstreamHostID: "51178"},
			}}
			row := &db.Instance{
				InstanceID:       "inst-q",
				UpstreamProvider: "marketco",
				UpstreamID:       "up-q",
				Region:           "us-east",
				Status:           StateProvisioning,
			}
			var errored []string
			fake := &fakeEngineDB{
				updateInstanceStatus: func(_, _, _ string) (bool, error) { return true, nil },
				getInstance:          func(string) (*db.Instance, error) { return row, nil },
				onSetInstanceError:   func(_, reason string) { errored = append(errored, reason) },
			}
			e := failoverEngine(fake, stub)
			e.tunnelStallAfter = time.Millisecond
			q := silentQuarantine()
			e.SetHostQuarantine(q)

			// No rescue left, so the verdict fails the launch at once.
			e.progressStatus("inst-q", e.provisionTimeout,
				&bootRescue{req: ProvisionRequest{CustomerImage: tc.customerImage}})

			if len(errored) != 1 || errored[0] != stallErrorReason(verdictTunnelStall, true) {
				t.Fatalf("want one tunnel-stall failure, got %v", errored)
			}
			if got := q.Quarantined("marketco", "51178"); got != tc.wantHeld {
				t.Errorf("machine quarantined = %v, want %v", got, tc.wantHeld)
			}
		})
	}
}
