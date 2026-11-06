// Wave 2 production test (Plan 31-08, TMPL-10 / Pitfall 5 two-writer race).
// The Wave 0 build tag was dropped by Plan 08, which landed the template-aware
// booting transition (progressStatusStep) + its engine DB seam
// (progressStatusStore + newEngineForTest).
//
// The two-writer race: for a template (service) instance, the poll loop must
// NOT flip booting→running on status.IP (writer #1). It must leave the
// instance booting for the bootstrap app_ready callback (writer #2) to flip,
// so the customer's deploy URL only goes live once the app is actually
// listening. A raw instance keeps today's behavior (IP flips to running).
//
// References (Wave 2 production symbols this file pins, by the names Plan
// 06/08 commit to):
//   - db.Instance.TemplateID *string                 — set for template deploys (Plan 06)
//   - provision.progressStatusStore interface         — the engine poll-loop DB seam (Plan 08)
//   - provision.newEngineForTest(progressStatusStore) — test constructor (Plan 08)
//   - (*Engine).progressStatusStep(ctx, instanceID, status) — one poll iteration (Plan 08)

package provision

import (
	"context"
	"sync"
	"testing"

	"github.com/gpuai-dev/gpuai-oss/internal/db"
	"github.com/gpuai-dev/gpuai-oss/internal/provider"
)

// spyProgressStore records whether SetInstanceRunning / SetInstanceUpstreamIP
// were called, satisfying the engine poll-loop DB seam Plan 08 declares.
// GetInstance returns the scripted instance; SetInstanceRunning flips a flag
// and returns updated=true.
type spyProgressStore struct {
	mu             sync.Mutex
	inst           *db.Instance
	setRunningCall int
	setIPCall      int
	lastIP         string
	sshPorts       []int
}

func (s *spyProgressStore) GetInstance(_ context.Context, _ string) (*db.Instance, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := *s.inst
	return &cp, nil
}

func (s *spyProgressStore) SetInstanceRunning(_ context.Context, _ string, _ string) (bool, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.setRunningCall++
	return true, StateBooting, nil
}

func (s *spyProgressStore) SetInstanceUpstreamIP(_ context.Context, _ string, ip string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.setIPCall++
	s.lastIP = ip
	return nil
}

func (s *spyProgressStore) SetInstanceSSHPort(_ context.Context, _ string, port int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sshPorts = append(s.sshPorts, port)
	return nil
}

func strPtr(s string) *string { return &s }

// bootingStatus is a provider status reporting a ready IP in the booting phase.
func bootingStatus() provider.InstanceStatus {
	return provider.InstanceStatus{Status: "running", IP: "10.1.2.3"}
}

// TestProgressStatusTemplateDefersRunning asserts a template instance is NOT
// flipped to running on status.IP — it stays booting for the app_ready
// callback. SetInstanceRunning must NOT be invoked on the template path.
func TestProgressStatusTemplateDefersRunning(t *testing.T) {
	store := &spyProgressStore{inst: &db.Instance{
		InstanceID: "gpu-tmpl1",
		Status:     string(StateBooting),
		TemplateID: strPtr("comfyui"),
		GPUType:    "rtx4090",
		GPUCount:   1,
	}}
	e := newEngineForTest(store)

	e.progressStatusStep(context.Background(), "gpu-tmpl1", bootingStatus())

	store.mu.Lock()
	defer store.mu.Unlock()
	if store.setRunningCall != 0 {
		t.Errorf("SetInstanceRunning called %d times on template path; want 0 (defer to app_ready)", store.setRunningCall)
	}
	if store.setIPCall != 0 {
		t.Errorf("SetInstanceUpstreamIP called %d times on template path; want 0 (upstream_ip intentionally unset for templates)", store.setIPCall)
	}
}

// TestProgressStatusRaw asserts a raw instance (TemplateID nil) is NOT flipped
// to running on status.IP either (RAWREADY: the tunnel_up ready callback is
// the sole authority — provider-ACTIVE+IP says nothing about the FRP tunnel).
// The step's only write is persisting the provider-reported IP, once.
func TestProgressStatusRaw(t *testing.T) {
	store := &spyProgressStore{inst: &db.Instance{
		InstanceID: "gpu-raw1",
		Status:     string(StateBooting),
		TemplateID: nil,
		GPUType:    "rtx4090",
		GPUCount:   1,
	}}
	e := newEngineForTest(store)

	e.progressStatusStep(context.Background(), "gpu-raw1", bootingStatus())

	store.mu.Lock()
	if store.setRunningCall != 0 {
		t.Errorf("SetInstanceRunning called %d times on raw path; want 0 (defer to tunnel_up callback)", store.setRunningCall)
	}
	if store.setIPCall != 1 {
		t.Errorf("SetInstanceUpstreamIP called %d times on raw path; want 1", store.setIPCall)
	}
	if store.lastIP != "10.1.2.3" {
		t.Errorf("SetInstanceUpstreamIP persisted %q; want %q", store.lastIP, "10.1.2.3")
	}
	// Script the row as already carrying the IP: a repeat step (poll tick)
	// must not re-write it.
	store.inst.UpstreamIP = strPtr("10.1.2.3")
	store.mu.Unlock()

	e.progressStatusStep(context.Background(), "gpu-raw1", bootingStatus())

	store.mu.Lock()
	defer store.mu.Unlock()
	if store.setIPCall != 1 {
		t.Errorf("SetInstanceUpstreamIP called %d times after IP already persisted; want 1 (idempotent)", store.setIPCall)
	}
	if store.setRunningCall != 0 {
		t.Errorf("SetInstanceRunning called %d times on raw path; want 0 (defer to tunnel_up callback)", store.setRunningCall)
	}
}
