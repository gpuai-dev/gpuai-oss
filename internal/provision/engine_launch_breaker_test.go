package provision

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/gpuai-dev/gpuai-oss/internal/availability"
	"github.com/gpuai-dev/gpuai-oss/internal/db"
	"github.com/gpuai-dev/gpuai-oss/internal/provider"
)

// --- launch-outcome breaker (GPU-108 follow-up) ---

// tripLaunchKey drives a breaker to Tripped for (providerName, gpuType, gpuCount).
func tripLaunchKey(b *availability.Breaker, providerName string, gpuType provider.GPUType, gpuCount, times int) {
	for i := 0; i < times; i++ {
		b.RecordFailure(availability.LaunchKey(providerName, string(gpuType), gpuCount),
			errors.New("upstream reported error before running"))
	}
}

// TestSelectProviderCandidates_LaunchBreakerSkipsTrippedSKU — a (provider,
// gpu_type) whose recent launches died at boot is skipped at placement even
// though the provider still advertises stock; after the cooldown the SKU is
// admitted again as a half-open probe.
func TestSelectProviderCandidates_LaunchBreakerSkipsTrippedSKU(t *testing.T) {
	dying, backup, rescue, _ := failoverFixture()
	e := failoverEngine(&fakeEngineDB{}, dying, backup)
	lb := availability.NewBreaker(3, 10*time.Minute, e.logger)
	now := time.Now()
	clock := now
	lb.SetClock(func() time.Time { return clock })
	e.launchBreaker = lb

	req := rescue.req
	req.SelectedOfferingID = ""

	tripLaunchKey(lb, "dying", provider.GPUTypeA10080GB, 1, 3)
	candidates, err := e.SelectProviderCandidates(context.Background(), req)
	if err != nil {
		t.Fatalf("SelectProviderCandidates: %v", err)
	}
	for _, c := range candidates {
		if c.Prov.Name() == "dying" {
			t.Errorf("tripped SKU must be skipped at placement")
		}
	}
	if len(candidates) == 0 {
		t.Fatal("healthy provider must survive")
	}

	// Cooldown elapsed: Allow promotes to half-open and the SKU re-enters
	// the candidate set as a probe.
	clock = now.Add(11 * time.Minute)
	candidates, err = e.SelectProviderCandidates(context.Background(), req)
	if err != nil {
		t.Fatalf("SelectProviderCandidates after cooldown: %v", err)
	}
	found := false
	for _, c := range candidates {
		if c.Prov.Name() == "dying" {
			found = true
		}
	}
	if !found {
		t.Error("cooled-down SKU must be admitted as a half-open probe")
	}
}

// TestSelectProviderCandidates_LaunchBreakerPinnedProviderBypasses — the
// staff dev-tools provider pin exists precisely to test a provider the
// operator knows is suspect, so it bypasses the breaker.
func TestSelectProviderCandidates_LaunchBreakerPinnedProviderBypasses(t *testing.T) {
	dying, backup, rescue, _ := failoverFixture()
	e := failoverEngine(&fakeEngineDB{}, dying, backup)
	lb := availability.NewBreaker(3, 10*time.Minute, e.logger)
	e.launchBreaker = lb

	tripLaunchKey(lb, "dying", provider.GPUTypeA10080GB, 1, 3)
	req := rescue.req
	req.SelectedOfferingID = ""
	req.PinnedProvider = "dying"

	candidates, err := e.SelectProviderCandidates(context.Background(), req)
	if err != nil {
		t.Fatalf("SelectProviderCandidates: %v", err)
	}
	if len(candidates) == 0 {
		t.Fatal("staff pin must bypass the launch breaker")
	}
	for _, c := range candidates {
		if c.Prov.Name() != "dying" {
			t.Errorf("pin must narrow to the pinned provider, got %s", c.Prov.Name())
		}
	}
}

// TestProgressStatus_LaunchBreakerRecordsFailure — an upstream-terminal read
// in the poll loop charges the breaker circuit for the failing (provider,
// gpu_type).
func TestProgressStatus_LaunchBreakerRecordsFailure(t *testing.T) {
	dying, _, _, inst := failoverFixture()
	inst.GPUType = string(provider.GPUTypeA10080GB)
	inst.GPUCount = 1
	fake := &fakeEngineDB{
		updateInstanceStatus: func(_, from, to string) (bool, error) {
			return from == StateCreating && to == StateProvisioning, nil
		},
		getInstance: func(id string) (*db.Instance, error) { return inst, nil },
	}
	e := failoverEngine(fake, dying)
	lb := availability.NewBreaker(1, 10*time.Minute, e.logger)
	e.launchBreaker = lb

	e.progressStatus("inst-f", e.provisionTimeout, nil) // no rescue: legacy fail path

	key := availability.LaunchKey("dying", string(provider.GPUTypeA10080GB), 1)
	if lb.State(key) != availability.Tripped {
		t.Errorf("terminal read must charge the launch breaker, state=%s", lb.State(key))
	}
}

// TestProgressStatus_LaunchBreakerRecordsSuccess — the first upstream-running
// transition heals the circuit.
func TestProgressStatus_LaunchBreakerRecordsSuccess(t *testing.T) {
	prov := &stubProvider{
		name:         "dying",
		statusResult: &provider.InstanceStatus{Status: "running"},
	}
	inst := &db.Instance{
		InstanceID:       "inst-f",
		UpstreamProvider: "dying",
		UpstreamID:       "up-dying",
		GPUType:          string(provider.GPUTypeA10080GB),
		GPUCount:         1,
		Region:           "us-west",
		Status:           StateProvisioning,
	}
	booted := false
	fake := &fakeEngineDB{
		updateInstanceStatus: func(_, from, to string) (bool, error) {
			if from == StateProvisioning && to == StateBooting {
				booted = true
			}
			return true, nil
		},
		getInstance: func(id string) (*db.Instance, error) {
			if booted {
				// Exit the loop via the concurrent-status-change branch.
				return &db.Instance{InstanceID: "inst-f", Status: StateTerminated}, nil
			}
			return inst, nil
		},
	}
	e := failoverEngine(fake, prov)
	lb := availability.NewBreaker(3, 10*time.Minute, e.logger)
	e.launchBreaker = lb
	// Seed prior failures so the success visibly heals the circuit.
	tripLaunchKey(lb, "dying", provider.GPUTypeA10080GB, 1, 3)

	e.progressStatus("inst-f", e.provisionTimeout, nil)

	key := availability.LaunchKey("dying", string(provider.GPUTypeA10080GB), 1)
	if lb.State(key) != availability.Healthy {
		t.Errorf("upstream-running must heal the launch breaker, state=%s", lb.State(key))
	}
}

// TestProgressStatus_Timeout_ChargesBreakerWhenNeverRunning — the stall path.
// A provider that accepts a create and then never reports running (Vultr has no
// failure status at all: a dead create sits in "creating" until the budget
// expires) must charge the circuit, or the phantom SKU is never shed and every
// subsequent customer waits out the full timeout.
func TestProgressStatus_Timeout_ChargesBreakerWhenNeverRunning(t *testing.T) {
	prov := &stubProvider{
		name: "staller",
		// Never leaves creating — the exact Vultr failed-install shape.
		statusResult: &provider.InstanceStatus{Status: "creating"},
	}
	inst := &db.Instance{
		InstanceID:       "inst-stall",
		UpstreamProvider: "staller",
		UpstreamID:       "up-stall",
		GPUType:          string(provider.GPUTypeA10080GB),
		GPUCount:         1,
		Region:           "us-west",
		Status:           StateProvisioning,
	}
	fake := &fakeEngineDB{
		updateInstanceStatus: func(_, from, to string) (bool, error) { return true, nil },
		getInstance:          func(id string) (*db.Instance, error) { return inst, nil },
	}
	e := failoverEngine(fake, prov)
	lb := availability.NewBreaker(1, 10*time.Minute, e.logger)
	e.launchBreaker = lb

	// A budget short enough that the timeout fires immediately.
	e.progressStatus("inst-stall", time.Millisecond, nil)

	key := availability.LaunchKey("staller", string(provider.GPUTypeA10080GB), 1)
	if lb.State(key) != availability.Tripped {
		t.Errorf("a provider that never reported running must charge the breaker on timeout, state=%s", lb.State(key))
	}
}

// TestProgressStatus_Timeout_DoesNotChargeBreakerAfterRunning — the guard that
// keeps this change from being a cross-provider footgun. If the provider DID
// report running, the VM existed and the timeout is our side (cloud-init,
// bootstrap, sshd, tunnel). Charging there would shed a healthy SKU on our own
// bug, for every provider, so the breaker must stay quiet however symmetric it
// looks.
func TestProgressStatus_Timeout_DoesNotChargeBreakerAfterRunning(t *testing.T) {
	prov := &stubProvider{
		name: "healthy",
		// Reports running: the box is real. Whatever times out afterwards is ours.
		statusResult: &provider.InstanceStatus{Status: "running"},
	}
	inst := &db.Instance{
		InstanceID:       "inst-ours",
		UpstreamProvider: "healthy",
		UpstreamID:       "up-ours",
		GPUType:          string(provider.GPUTypeA10080GB),
		GPUCount:         1,
		Region:           "us-west",
		// Booting, not Provisioning: the provider already said running, so the
		// loop keeps polling until the budget expires rather than exiting via
		// the status-changed branch.
		Status: StateBooting,
	}
	fake := &fakeEngineDB{
		updateInstanceStatus: func(_, from, to string) (bool, error) { return true, nil },
		getInstance:          func(id string) (*db.Instance, error) { return inst, nil },
	}
	e := failoverEngine(fake, prov)
	lb := availability.NewBreaker(1, 10*time.Minute, e.logger)
	e.launchBreaker = lb

	e.progressStatus("inst-ours", 40*time.Millisecond, nil)

	key := availability.LaunchKey("healthy", string(provider.GPUTypeA10080GB), 1)
	if lb.State(key) == availability.Tripped {
		t.Errorf("a timeout AFTER the provider reported running is our fault, not phantom stock — the breaker must not trip (state=%s)", lb.State(key))
	}
}
