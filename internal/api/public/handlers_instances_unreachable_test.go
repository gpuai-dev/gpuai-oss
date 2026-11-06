package public

import (
	"testing"
	"time"

	"github.com/gpuai-dev/gpuai-oss/internal/db"
)

// GPU-77 status surface.
//
// A confirmed-dead tunnel used to read "running" on every customer-facing
// surface: the provider still reported the node up, so the dashboard was green
// while the customer's SSH was gone and the meter kept accruing. The DB's
// lifecycle status column deliberately STAYS "running" (every optimistic
// running->error flip, ListActiveInstances, and the probe's own skip depend on
// it), so the degraded state is applied at SERIALIZATION time, from the
// durable unreachable_since flag.
//
// Two rules make that safe, and both are pinned below:
//   - the override applies ONLY to internal status "running" — lifecycle truth
//     beats a stale flag, so a terminated or failed instance reads terminated
//     or error even if nothing ever cleared its unreachable_since;
//   - connection coordinates survive, because an unreachable instance is not a
//     dead one. It may recover inside the grace window, and stripping its SSH
//     details would break every client that reconnects on its own.

func TestInstanceToPublic_UnreachableStatus(t *testing.T) {
	t0 := time.Date(2026, 8, 20, 18, 45, 42, 0, time.UTC)

	out := instanceToPublic(&db.Instance{
		InstanceID:       "gpu-unre0001",
		Status:           "running",
		UnreachableSince: &t0,
	}, "", "", false)

	if out.Status != "unreachable" {
		t.Fatalf("status = %q, want %q (a running instance with a confirmed-dead tunnel is not healthy)", out.Status, "unreachable")
	}
	if out.UnreachableSince == nil || !out.UnreachableSince.Equal(t0) {
		t.Fatalf("unreachable_since = %v, want %v", out.UnreachableSince, t0)
	}
}

func TestInstanceToPublic_ReachableRunningIsUnchanged(t *testing.T) {
	out := instanceToPublic(&db.Instance{
		InstanceID: "gpu-ok000001",
		Status:     "running",
	}, "", "", false)

	if out.Status != "running" {
		t.Fatalf("status = %q, want %q", out.Status, "running")
	}
	if out.UnreachableSince != nil {
		t.Fatalf("unreachable_since = %v, want absent for a reachable instance", out.UnreachableSince)
	}
}

// A stale flag must never rewrite a lifecycle status. The escalation path
// leaves unreachable_since set on the row it fails (the value is forensic —
// it is the outage start billing was closed back to), so "error" instances
// routinely carry one.
func TestInstanceToPublic_StaleFlagNeverOverridesLifecycleStatus(t *testing.T) {
	t0 := time.Date(2026, 8, 20, 18, 45, 42, 0, time.UTC)

	for _, tc := range []struct{ internal, want string }{
		{"error", "error"},
		{"terminated", "terminated"},
		{"booting", "starting"},
		{"creating", "allocating"},
	} {
		t.Run(tc.internal, func(t *testing.T) {
			out := instanceToPublic(&db.Instance{
				InstanceID:       "gpu-stale001",
				Status:           tc.internal,
				UnreachableSince: &t0,
			}, "", "", false)

			if out.Status != tc.want {
				t.Fatalf("internal %q with a stale unreachable_since serialized as %q, want %q",
					tc.internal, out.Status, tc.want)
			}
		})
	}
}

// An unreachable instance may recover inside the grace window, so it keeps its
// SSH coordinates — the Connection block gates on the INTERNAL status, which
// this mapping never touches.
func TestInstanceToPublic_UnreachableKeepsConnection(t *testing.T) {
	t0 := time.Now().UTC().Add(-5 * time.Minute)
	port := 20022

	out := instanceToPublic(&db.Instance{
		InstanceID:       "gpu-unre0002",
		Status:           "running",
		FRPRemotePort:    &port,
		UnreachableSince: &t0,
	}, "tunnel.example.com", "", false)

	if out.Status != "unreachable" {
		t.Fatalf("status = %q, want unreachable", out.Status)
	}
	if out.Connection == nil {
		t.Fatal("unreachable instance lost its connection details — it may still recover, and clients reconnect on their own")
	}
	if out.Connection.Port != port {
		t.Errorf("connection port = %d, want %d", out.Connection.Port, port)
	}
}
