package public

import (
	"testing"

	"github.com/gpuai-dev/gpuai-oss/internal/db"
)

// TestInstanceToPublic_StatusReason — the why behind an error status
// (db error_reason, written by SetInstanceError) must surface on the public
// wire as status_reason, and stay absent for healthy instances. Pre-fix the
// public shape had no reason field at all, so an errored launch was
// indistinguishable from a mystery ("status_reason: null" UX gap,
// 2026-08-04 boot-stall characterization).
func TestInstanceToPublic_StatusReason(t *testing.T) {
	reason := "provisioning stalled: the instance started but never became reachable"
	out := instanceToPublic(&db.Instance{InstanceID: "gpu-err00001", Status: "error", ErrorReason: &reason}, "", "", false)
	if out.StatusReason == nil || *out.StatusReason != reason {
		t.Fatalf("status_reason must carry error_reason, got %v", out.StatusReason)
	}

	healthy := instanceToPublic(&db.Instance{InstanceID: "gpu-ok000001", Status: "running"}, "", "", false)
	if healthy.StatusReason != nil {
		t.Fatalf("healthy instances must omit status_reason, got %q", *healthy.StatusReason)
	}
}
