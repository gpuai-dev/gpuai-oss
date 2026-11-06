package provision

import (
	"context"
	"testing"
	"time"

	"github.com/gpuai-dev/gpuai-oss/internal/availability"
	"github.com/gpuai-dev/gpuai-oss/internal/db"
	"github.com/gpuai-dev/gpuai-oss/internal/provider"
)

// Fleet-probe launches (internal/fleetprobe) run in shadow mode before anyone
// enforces, so they must not change what customers see, and the probe's boot
// time must not turn a working host into a failed launch.

// A probe launch never trips or heals the customer launch-outcome breaker:
// the same upstream death that trips it for a customer launch leaves it
// untouched for a probe launch.
func TestProvision_FleetProbeLaunchNeverFeedsTheLaunchBreaker(t *testing.T) {
	for _, tc := range []struct {
		name  string
		probe bool
		want  availability.ProviderHealth
	}{
		{"customer launch charges it", false, availability.Tripped},
		{"probe launch does not", true, availability.Healthy},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stub := onDemandStub("up-1", "us-west")
			stub.statusResult = &provider.InstanceStatus{Status: "error"} // dies before running
			fake := fakeWithOneKey()
			fake.updateInstanceStatus = func(_, from, to string) (bool, error) {
				return from == StateCreating && to == StateProvisioning, nil
			}
			fake.getInstance = func(id string) (*db.Instance, error) {
				return &db.Instance{InstanceID: id, UpstreamProvider: "stub", UpstreamID: "up-1",
					GPUType: string(provider.GPUTypeA10080GB), GPUCount: 1, Region: "us-west", Status: StateProvisioning}, nil
			}
			failed := make(chan struct{}, 1)
			fake.onSetInstanceError = func(string, string) { failed <- struct{}{} }
			e := newProvisionUnitEngine(stub, fake)
			e.pollInterval = time.Millisecond
			e.provisionTimeout = time.Minute
			lb := availability.NewBreaker(1, 10*time.Minute, e.logger)
			e.launchBreaker = lb

			req := onDemandRequest()
			req.FleetProbe = tc.probe
			if _, err := e.Provision(context.Background(), req); err != nil {
				t.Fatalf("Provision: %v", err)
			}
			select {
			case <-failed: // progressStatus reached the upstream-terminal fail path
			case <-time.After(10 * time.Second):
				t.Fatal("the launch never failed")
			}

			key := availability.LaunchKey("stub", string(provider.GPUTypeA10080GB), 1)
			if got := lb.State(key); got != tc.want {
				t.Errorf("breaker %s; want %s", got, tc.want)
			}
			e.fleetProbeLaunches.Range(func(id, _ any) bool {
				t.Errorf("probe marker for %v left behind after its poll loop ended", id)
				return true
			})
		})
	}
}

// The success and stall-timeout paths are gated the same way: a probe launch's
// first upstream-running does not heal a tripped circuit, and its timeout does
// not charge a healthy one.
func TestProgressStatus_FleetProbeLaunchLeavesTheBreakerAlone(t *testing.T) {
	key := availability.LaunchKey("dying", string(provider.GPUTypeA10080GB), 1)

	t.Run("first upstream running", func(t *testing.T) {
		prov := &stubProvider{name: "dying", statusResult: &provider.InstanceStatus{Status: "running"}}
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
					return &db.Instance{InstanceID: id, Status: StateTerminated}, nil
				}
				return &db.Instance{InstanceID: id, UpstreamProvider: "dying", UpstreamID: "up",
					GPUType: string(provider.GPUTypeA10080GB), GPUCount: 1, Status: StateProvisioning}, nil
			},
		}
		e := failoverEngine(fake, prov)
		lb := availability.NewBreaker(3, 10*time.Minute, e.logger)
		e.launchBreaker = lb
		tripLaunchKey(lb, "dying", provider.GPUTypeA10080GB, 1, 3)
		e.fleetProbeLaunches.Store("inst-probe", struct{}{})

		e.progressStatus("inst-probe", e.provisionTimeout, nil)

		if lb.State(key) != availability.Tripped {
			t.Errorf("a probe launch healed the circuit: %s", lb.State(key))
		}
	})

	t.Run("provisioning timeout", func(t *testing.T) {
		prov := &stubProvider{name: "dying", statusResult: &provider.InstanceStatus{Status: "creating"}}
		fake := &fakeEngineDB{
			updateInstanceStatus: func(_, _, _ string) (bool, error) { return true, nil },
			getInstance: func(id string) (*db.Instance, error) {
				return &db.Instance{InstanceID: id, UpstreamProvider: "dying", UpstreamID: "up",
					GPUType: string(provider.GPUTypeA10080GB), GPUCount: 1, Status: StateProvisioning}, nil
			},
		}
		e := failoverEngine(fake, prov)
		lb := availability.NewBreaker(1, 10*time.Minute, e.logger)
		e.launchBreaker = lb
		e.fleetProbeLaunches.Store("inst-probe", struct{}{})

		e.progressStatus("inst-probe", time.Millisecond, nil)

		if lb.State(key) != availability.Healthy {
			t.Errorf("a probe launch's timeout charged the circuit: %s", lb.State(key))
		}
	})
}

// A probe launch gets the probe's boot time on top of the provider's budget,
// and Provision hands that budget back so the prober waits at least as long.
func TestProvision_FleetProbeBudgetIncludesTheProbe(t *testing.T) {
	e := &Engine{provisionTimeout: 10 * time.Minute}
	customer, probe := onDemandRequest(), onDemandRequest()
	probe.FleetProbe = true
	if got := e.launchBudget(customer, 600); got != 20*time.Minute {
		t.Errorf("customer budget %s; want the provider's 20m", got)
	}
	if got := e.launchBudget(probe, 600); got != 20*time.Minute+FleetProbeBootAllowance {
		t.Errorf("probe budget %s; want 20m plus the probe allowance", got)
	}

	stub := onDemandStub("up-1", "us-west")
	stub.provisionResult.EstimatedReadySeconds = 600
	pe := newProvisionUnitEngine(stub, fakeWithOneKey())
	pe.provisionTimeout = 10 * time.Minute
	resp, err := pe.Provision(context.Background(), probe)
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	if resp.ProvisioningBudget != 20*time.Minute+FleetProbeBootAllowance {
		t.Errorf("response budget %s; want the probe launch's budget", resp.ProvisioningBudget)
	}
}

// The boot-stall watchdog's "running but no ready callback" leg gives a probe
// launch the probe's time too.
func TestBootStallDetector_FleetProbeGetsTheProbeAllowance(t *testing.T) {
	e := &Engine{tunnelStallAfter: 8 * time.Minute, pullStallAfter: 5 * time.Minute}
	capable := &rescueCapableStub{&stubProvider{name: "marketco"}}
	raw := &db.Instance{InstanceID: "inst"}
	customer := &bootRescue{req: onDemandRequest()}
	probe := &bootRescue{req: onDemandRequest()}
	probe.req.FleetProbe = true

	if d := e.bootStallDetectorFor(capable, raw, customer); d.tunnelStallAfter != 8*time.Minute || d.pullStallAfter != 5*time.Minute {
		t.Errorf("customer detector %+v", d)
	}
	if d := e.bootStallDetectorFor(capable, raw, probe); d.tunnelStallAfter != 8*time.Minute+FleetProbeBootAllowance || d.pullStallAfter != 5*time.Minute {
		t.Errorf("probe detector tunnel %s pull %s; want the tunnel leg extended by the probe allowance", d.tunnelStallAfter, d.pullStallAfter)
	}
}

// IsBootTimeout separates time-based boot verdicts (the prober flags them)
// from a provider reporting the VM dead (the prober delists). It keys on the
// customer-visible reason prefixes, so every reason the engine writes is
// pinned here.
func TestIsBootTimeout(t *testing.T) {
	timeouts := []string{
		"provisioning timeout: instance did not become SSH-reachable within 20m0s",
		`provisioning timeout (reaper): stuck in "booting" for over 45m0s`,
		stallErrorReason(verdictTunnelStall, false),
		stallErrorReason(verdictDeadPull, true),
		stallErrorReason("unknown-verdict", false),
	}
	failures := []string{
		"upstream instance failed: error",
		stallErrorReason(verdictUpstreamTerminal, false),
		stallErrorReason(verdictUpstreamTerminal, true),
		stallErrorReason(verdictGPUCheckFailed, false),
	}
	for _, r := range timeouts {
		if !IsBootTimeout(r) {
			t.Errorf("%q not read as a boot timeout", r)
		}
	}
	for _, r := range failures {
		if IsBootTimeout(r) {
			t.Errorf("%q read as a boot timeout; it is a reported failure", r)
		}
	}
}
