//go:build liveprobe

// Live state-machine probe. NOT part of any suite — run manually:
//
//	go test -tags liveprobe -run TestLiveProbe_MapStatus -v -timeout 15m ./internal/provider/vultr/
//
// Requires VULTR_API_KEY. Creates ONE 1×A16 instance ($0.471/hr) in whichever
// region currently has it in stock, observes the real (status, server_status,
// power_status) sequence through boot, and tears it down via the adapter's own
// Terminate. Costs well under $0.50.
package vultr

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/gpuai-dev/gpuai-oss/internal/provider"
)

func TestLiveProbe_MapStatus(t *testing.T) {
	key := os.Getenv("VULTR_API_KEY")
	if key == "" {
		t.Skip("VULTR_API_KEY unset")
	}
	a := NewAdapter(key)

	// Brand the probe with the smoke grammar ("smoke-gpu-<unix>-<suffix>") so a
	// probe VM that outlives its teardown — Ctrl-C mid-poll, or a Terminate that
	// fails — is still visible to the leak sweep and reapable. A bespoke label
	// would be owned by nothing and bill silently, which is the exact 2026-09-06
	// shape this package was just hardened against. The prefix is written out
	// rather than imported because internal/provider must not depend on
	// tests/providers; it is asserted against tests/providers.SmokePrefix there.
	label := fmt.Sprintf("smoke-gpu-%d-probe", time.Now().Unix())
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Minute)
	defer cancel()

	// Pick a region that actually has the plan RIGHT NOW. The first version of
	// this probe hardcoded "ewr" and red-alerted the moment ewr lost stock
	// (2026-09-16) — paging "the adapter's state model may have drifted" for
	// what was purely capacity. That is the same capacity-vs-correctness
	// confusion this probe exists to eliminate, just pointing the other way.
	//
	// The plan is the cheapest whole-GPU SKU. The fractional A16 slice this
	// used to run on (vcg-a16-2c-8g-2vram, $0.059/hr) left the catalog around
	// 2026-10-12, when Vultr dropped every vGPU slice and moved its whole-GPU
	// plans to plan type "vdm"; dead-SKU Fatalf below caught it daily.
	const probePlan = "vcg-a16-6c-60g-16vram"
	regions := probeRegionsFor(ctx, t, a, probePlan)
	if len(regions) == 0 {
		// Capacity, not correctness. A skip keeps the distinction honest: this
		// probe may only ever go red for a real state-machine problem.
		t.Skipf("no region currently has %s in stock — capacity, not a correctness signal", probePlan)
	}

	// Availability is a snapshot: a region can sell out between the read above
	// and the create, so walk the in-stock regions in turn. ONLY a capacity
	// rejection may advance to the next region. Two reasons, both load-bearing:
	//
	//   1. Anything else is ops-actionable and must page, not skip. The client
	//      maps our own account gates ("maximum monthly fee limit", "minimum
	//      deposit", "Unauthorized IP address" — all three hit live on this
	//      account 2026-08-14) to ErrQuotaExceeded precisely so they are not
	//      mistaken for capacity. Swallowing them here would leave the probe
	//      silently dead and green, which is worse than the false alarm this
	//      PR removes.
	//   2. createInstance is deliberately a SINGLE attempt because POST
	//      /instances is not idempotent: a 5xx or transport error may land
	//      AFTER Vultr created the VM. Retrying in another region would then
	//      bill a second instance that the defer below never sees. A capacity
	//      rejection is the one case where retrying is sound, because Vultr
	//      declined to place the instance at all. Note the narrowness of what
	//      errors.Is actually proves here: do() applies the capacity phrasing
	//      match to any status >= 400, so a 5xx whose body happened to contain
	//      "not available" would pass this gate. No such body is on record,
	//      and the workflow's always()-gated account-empty assertion is the
	//      backstop if one ever appears.
	var inst *instance
	var lastErr error
	var placedRegion string
	for _, region := range regions {
		created, err := a.client.createInstance(ctx, createInstanceRequest{
			Region: region, Plan: probePlan, OSID: defaultOSID,
			Label: label, Hostname: label,
			Backups: "disabled", ActivationEmail: false, DDOSProtection: false,
			Tags: []string{"gpuai", "probe"},
		})
		if err == nil {
			t.Logf("region %s accepted the create", region)
			inst, placedRegion = created, region
			break
		}
		lastErr = err
		if !errors.Is(err, provider.ErrNoCapacity) {
			// May have created a VM before failing — say so loudly. The
			// workflow's account-empty assertion runs on always() and will
			// red the job if one leaked.
			t.Fatalf("region %s rejected the create with a NON-capacity error: %v — "+
				"this is ops-actionable (account entitlement gate, or an adapter regression), "+
				"NOT stock churn. Check the Vultr account is empty.", region, err)
		}
		t.Logf("region %s is out of stock (%v) — trying the next", region, err)
	}
	if inst == nil {
		t.Skipf("every in-stock region sold out before the create — capacity, not a correctness signal (last error: %v)", lastErr)
	}
	id := inst.ID
	t.Logf("CREATED id=%s label=%s region=%s", id, label, placedRegion)

	// Teardown runs no matter how this test exits.
	defer func() {
		tctx, tcancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer tcancel()
		if err := a.Terminate(tctx, id, placedRegion); err != nil {
			t.Errorf("TERMINATE FAILED for %s: %v — CHECK THE ACCOUNT MANUALLY", id, err)
			return
		}
		t.Logf("TERMINATED %s via adapter.Terminate", id)
		st, err := a.GetStatus(context.Background(), id, placedRegion)
		t.Logf("post-terminate GetStatus: %+v err=%v", st, err)
	}()

	var sawStopping, sawRunning bool
	var timeline []string
	start := time.Now()
	for time.Since(start) < 8*time.Minute {
		raw, err := a.client.getInstance(ctx, id)
		if err != nil {
			t.Logf("t+%-4s getInstance err: %v", time.Since(start).Truncate(time.Second), err)
			time.Sleep(10 * time.Second)
			continue
		}
		mapped := mapStatus(raw.Status, raw.PowerStatus, raw.ServerStatus)
		line := fmt.Sprintf("t+%-5s status=%-8s server_status=%-16s power_status=%-8s => %s",
			time.Since(start).Truncate(time.Second), raw.Status, raw.ServerStatus, raw.PowerStatus, mapped)
		t.Log(line)
		timeline = append(timeline, line)

		if mapped == "stopping" {
			sawStopping = true
		}
		if mapped == "running" {
			sawRunning = true
			break
		}
		time.Sleep(10 * time.Second)
	}

	t.Log("---- OBSERVED TIMELINE ----")
	for _, l := range timeline {
		t.Log(l)
	}

	if sawStopping {
		t.Errorf("REGRESSION: adapter reported \"stopping\" during boot — the original bug")
	}
	if !sawRunning {
		t.Errorf("never reached \"running\" within 8m")
	}
	if sawRunning && !sawStopping {
		t.Logf("PASS: boot reached running with no spurious stopping")
	}
}

// probeRegionsFor returns the regions where plan is currently purchasable, in
// the order Vultr reports them. Read live rather than from a constant: Vultr's
// GPU stock moves between regions daily, and a hardcoded region turns every
// stock shift into a false correctness alert.
//
// The two empty outcomes are NOT the same thing and are deliberately split:
// a plan that is out of stock everywhere is capacity (skip), but a plan that
// has vanished from the catalog entirely means the probe is aimed at a SKU
// Vultr retired or renamed — coverage would silently drop to zero and the job
// would stay green forever. That fails.
func probeRegionsFor(ctx context.Context, t *testing.T, a *Adapter, plan string) []string {
	t.Helper()

	plans, err := a.client.listGPUPlans(ctx)
	if err != nil {
		// A read-only availability lookup failing is a Vultr-side blip, not
		// evidence about our state machine. Paging for it would re-create the
		// false-alarm class this probe was just fixed to avoid.
		t.Skipf("could not list Vultr plans (%v) — no capacity signal, skipping rather than crying wolf", err)
	}
	if len(plans) == 0 {
		// Vultr keeps out-of-stock plans listed with "locations": [], so an
		// EMPTY catalog is never a real answer — it is a glitch, or the
		// gpu_brand field changing under us. Skipping drains it; falling
		// through to the dead-SKU Fatalf below would page for neither
		// capacity nor drift.
		t.Skipf("Vultr returned an empty GPU catalog — not a capacity or correctness signal")
	}
	for _, p := range plans {
		if p.ID == plan {
			return p.Locations
		}
	}
	t.Fatalf("probe plan %s is no longer in Vultr's GPU catalog — the probe is aimed at a dead SKU "+
		"and would report green while covering nothing. Pick the cheapest currently-listed whole-GPU plan.", plan)
	return nil
}
