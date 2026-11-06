package vultr

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gpuai-dev/gpuai-oss/internal/provider"
)

// plansFixture mirrors the live GET /v2/plans?type=vcg shape (2026-08-11):
// one fractional A16 slice (skipped), a whole-GPU 2×A16 plan in two regions,
// a 1×L40S plan with no deployable region (skipped), and a not-on-demand
// plan (skipped).
const plansFixture = `{
  "plans": [
    {"id":"vcg-a16-2c-8g-2vram","vcpu_count":2,"ram":8192,"disk":50,
     "hourly_cost":0.059,"monthly_cost":43,"type":"vcg","locations":["ewr","fra"],
     "deploy_ondemand":true,"gpu_brand":"NVIDIA","gpu_vram_gb":2,
     "gpu_type":"NVIDIA_A16","gpu_count":"1/8"},
    {"id":"vcg-a16-12c-128g-32vram","vcpu_count":12,"ram":131072,"disk":700,
     "hourly_cost":0.942,"monthly_cost":688,"type":"vcg","locations":["atl","blr"],
     "deploy_ondemand":true,"gpu_brand":"NVIDIA","gpu_vram_gb":32,
     "gpu_type":"NVIDIA_A16","gpu_count":2},
    {"id":"vcg-l40s-16c-180g-48vram","vcpu_count":16,"ram":184320,"disk":1200,
     "hourly_cost":1.671,"monthly_cost":1220,"type":"vcg","locations":[],
     "deploy_ondemand":true,"gpu_brand":"NVIDIA","gpu_vram_gb":48,
     "gpu_type":"NVIDIA_L40S","gpu_count":1},
    {"id":"vcg-a16-24c-256g-64vram","vcpu_count":24,"ram":262144,"disk":1200,
     "hourly_cost":1.884,"monthly_cost":1376,"type":"vcg","locations":["blr"],
     "deploy_ondemand":false,"gpu_brand":"NVIDIA","gpu_vram_gb":64,
     "gpu_type":"NVIDIA_A16","gpu_count":4}
  ]
}`

// instancesFixture builds a GET /instances page holding n rows on the given
// plan id, labeled as foreign/manual VMs — the account-wide caps count those
// too, unlike ListProvisioned's label filter.
func instancesFixture(n int, planID string) string {
	rows := make([]string, n)
	for i := range rows {
		rows[i] = fmt.Sprintf(`{"id":"manual-%d","plan":%q,"label":"someone-elses-vm","region":"atl","status":"active","power_status":"running"}`, i, planID)
	}
	return `{"instances":[` + strings.Join(rows, ",") + `],"meta":{"total":` + strconv.Itoa(n) + `,"links":{"next":"","prev":""}}}`
}

// setupTestAdapter points an Adapter at a local httptest server via
// WithBaseURL. The server serves an authenticated /account and the plans
// fixture by default; overrides claim routes first (a route in overrides
// replaces the default of the same pattern).
func setupTestAdapter(t *testing.T, overrides map[string]http.HandlerFunc) (*Adapter, *httptest.Server) {
	t.Helper()
	routes := map[string]http.HandlerFunc{
		"/account": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"account":{"name":"test"}}`))
		},
		"/plans": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(plansFixture))
		},
		// ListAvailable's account-usage read; an empty account by default so
		// the supply caps never bite unless a test says so.
		"/instances": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(instancesFixture(0, "")))
		},
	}
	for pattern, h := range overrides {
		routes[pattern] = h
	}
	mux := http.NewServeMux()
	for pattern, h := range routes {
		mux.HandleFunc(pattern, h)
	}
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return NewAdapter("test-api-key", WithBaseURL(server.URL)), server
}

func TestListAvailable(t *testing.T) {
	adapter, _ := setupTestAdapter(t, nil)

	offerings, err := adapter.ListAvailable(context.Background())
	if err != nil {
		t.Fatalf("ListAvailable: %v", err)
	}
	// Only the whole-GPU 2×A16 plan survives (fractional slice, empty
	// locations, and not-on-demand are all skipped), one row per region.
	if len(offerings) != 2 {
		t.Fatalf("expected 2 offerings, got %d: %+v", len(offerings), offerings)
	}
	o := offerings[0]
	if o.Provider != "vultr" || o.GPUType != provider.GPUTypeA16 || o.GPUCount != 2 {
		t.Errorf("unexpected offering identity: %+v", o)
	}
	if o.VRAMPerGPUGB != 16 {
		t.Errorf("VRAMPerGPUGB = %d, want 16 (32 total / 2 GPUs)", o.VRAMPerGPUGB)
	}
	if o.PricePerHour != 0.942 {
		t.Errorf("PricePerHour = %v, want whole-instance 0.942", o.PricePerHour)
	}
	if o.RAMGB != 128 || o.CPUCores != 12 || o.StorageGB != 700 {
		t.Errorf("specs = ram %d cpu %d disk %d, want 128/12/700", o.RAMGB, o.CPUCores, o.StorageGB)
	}
	if o.Region != "atl" || o.DatacenterLocation != "atl" {
		t.Errorf("region = %q/%q, want native atl", o.Region, o.DatacenterLocation)
	}
	if o.UpstreamGPUID != "vcg-a16-12c-128g-32vram" {
		t.Errorf("UpstreamGPUID = %q, want the plan id", o.UpstreamGPUID)
	}
	// The engine drops offerings with AvailableCount <= 0 — a deployable
	// region must advertise at least one unit or vultr never places.
	if o.AvailableCount < 1 || o.StockStatus == "" {
		t.Errorf("stock = (%d, %q), want at least (1, non-empty)", o.AvailableCount, o.StockStatus)
	}
	if offerings[1].Region != "blr" {
		t.Errorf("second offering region = %q, want blr", offerings[1].Region)
	}
}

func TestListAvailable_AuthFailure(t *testing.T) {
	adapter, _ := setupTestAdapter(t, map[string]http.HandlerFunc{
		"/account": func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"Invalid API token.","status":401}`))
		},
	})

	if _, err := adapter.ListAvailable(context.Background()); err == nil {
		t.Fatal("expected an error on a 401 auth probe (the public /plans endpoint must not mask a dead key)")
	}
}

// TestListAvailable_InstanceCapSuppressesAll: at the account's instance-count
// limit, the adapter must emit NOTHING (nil, no error) — the cap stays
// invisible to customers instead of surfacing as a failed launch. Rows on
// unknown plans and foreign labels count too: the cap is account-wide.
func TestListAvailable_InstanceCapSuppressesAll(t *testing.T) {
	adapter, _ := setupTestAdapter(t, map[string]http.HandlerFunc{
		"/instances": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			// Track the constant, not a literal: a cap raise must not silently
			// turn this into a no-op that asserts nothing (it did, when the
			// account limit went 10 -> 15 on 2026-08-26).
			_, _ = w.Write([]byte(instancesFixture(defaultMaxInstances, "vps-plan-not-in-vcg-catalog")))
		},
	})

	offerings, err := adapter.ListAvailable(context.Background())
	if err != nil {
		t.Fatalf("ListAvailable: %v", err)
	}
	if len(offerings) != 0 {
		t.Fatalf("expected 0 offerings at the instance cap, got %d", len(offerings))
	}
}

// TestListAvailable_MonthlyCapSuppressesUnaffordablePlans: a plan whose next
// instance would exceed the monthly-fee limit must not be offered; one that
// still fits must be.
func TestListAvailable_MonthlyCapSuppressesUnaffordablePlans(t *testing.T) {
	// One live 2×A16 commits $688/mo of the cap.
	overrides := map[string]http.HandlerFunc{
		"/instances": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(instancesFixture(1, "vcg-a16-12c-128g-32vram")))
		},
	}

	// Headroom 2000-688=1312 ≥ 688 — another 2×A16 fits, both regions offered.
	adapter, _ := setupTestAdapter(t, overrides)
	adapter.SetAccountLimits(10, 2000)
	offerings, err := adapter.ListAvailable(context.Background())
	if err != nil {
		t.Fatalf("ListAvailable: %v", err)
	}
	if len(offerings) != 2 {
		t.Fatalf("expected 2 offerings with headroom, got %d: %+v", len(offerings), offerings)
	}

	// Headroom 1000-688=312 < 688 — no plan fits, vultr goes quietly dark.
	adapter2, _ := setupTestAdapter(t, overrides)
	adapter2.SetAccountLimits(10, 1000)
	offerings, err = adapter2.ListAvailable(context.Background())
	if err != nil {
		t.Fatalf("ListAvailable: %v", err)
	}
	if len(offerings) != 0 {
		t.Fatalf("expected 0 offerings without monthly headroom, got %d: %+v", len(offerings), offerings)
	}
}

// TestListAvailable_ZeroLimitsDisableGuards: 0/0 is the incident kill knob —
// it must not only ignore the caps but skip the /instances read entirely, so
// a flaky instances endpoint can't red a poll the operator un-guarded.
func TestListAvailable_ZeroLimitsDisableGuards(t *testing.T) {
	instancesCalls := 0
	adapter, _ := setupTestAdapter(t, map[string]http.HandlerFunc{
		"/instances": func(w http.ResponseWriter, r *http.Request) {
			instancesCalls++
			w.WriteHeader(http.StatusInternalServerError) // would red the poll if fetched
		},
	})
	adapter.SetAccountLimits(0, 0)

	offerings, err := adapter.ListAvailable(context.Background())
	if err != nil {
		t.Fatalf("ListAvailable: %v", err)
	}
	if len(offerings) != 2 {
		t.Fatalf("expected 2 offerings with guards disabled, got %d", len(offerings))
	}
	if instancesCalls != 0 {
		t.Fatalf("GET /instances called %d times, want 0 (disabled guards must skip the usage read)", instancesCalls)
	}
}

// TestListAvailable_UsageFetchFailureRedsThePoll: an /instances error must
// surface as a ListAvailable ERROR, never an empty catalog — the poller's
// breaker + last-known-good retention treat the two differently, and flying
// blind on usage must not silently take vultr dark as if that were truth.
func TestListAvailable_UsageFetchFailureRedsThePoll(t *testing.T) {
	adapter, _ := setupTestAdapter(t, map[string]http.HandlerFunc{
		"/instances": func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":"internal","status":500}`))
		},
	})

	if _, err := adapter.ListAvailable(context.Background()); err == nil {
		t.Fatal("expected an error when the account-usage read fails, got a successful (empty?) catalog")
	}
}

// TestListAvailable_MonthlyCapExactBoundaryFits: committed + plan == cap is
// allowed — the cap is a ceiling, not an open bound.
func TestListAvailable_MonthlyCapExactBoundaryFits(t *testing.T) {
	adapter, _ := setupTestAdapter(t, map[string]http.HandlerFunc{
		"/instances": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(instancesFixture(1, "vcg-a16-12c-128g-32vram")))
		},
	})
	adapter.SetAccountLimits(10, 1376) // 688 committed + 688 next == 1376 exactly

	offerings, err := adapter.ListAvailable(context.Background())
	if err != nil {
		t.Fatalf("ListAvailable: %v", err)
	}
	if len(offerings) != 2 {
		t.Fatalf("expected 2 offerings at the exact fee boundary, got %d", len(offerings))
	}
}

func TestProvision(t *testing.T) {
	var got createInstanceRequest
	instancesHandler := func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Fatalf("unexpected method %s", r.Method)
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatalf("decode create body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"instance":{"id":"inst-123","main_ip":"0.0.0.0","region":"blr","plan":"vcg-a16-12c-128g-32vram","status":"pending","power_status":"stopped","server_status":"none"}}`))
	}
	adapter, _ := setupTestAdapter(t, map[string]http.HandlerFunc{"/instances": instancesHandler})

	res, err := adapter.Provision(context.Background(), provider.ProvisionRequest{
		InstanceID:         "gpu-abc12345",
		GPUType:            provider.GPUTypeA16,
		GPUCount:           2,
		DatacenterLocation: "blr",
		UpstreamGPUID:      "vcg-a16-12c-128g-32vram",
		StartupScript:      "#!/bin/bash\necho hi\n",
	})
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	if res.UpstreamID != "inst-123" || res.Provider != "vultr" || res.Status != "creating" {
		t.Errorf("unexpected result: %+v", res)
	}
	if res.UpstreamIP != "" {
		t.Errorf("UpstreamIP = %q, want empty (0.0.0.0 placeholder must be filtered)", res.UpstreamIP)
	}
	if res.CostPerHour != 0.942 {
		t.Errorf("CostPerHour = %v, want 0.942", res.CostPerHour)
	}

	if got.Plan != "vcg-a16-12c-128g-32vram" || got.Region != "blr" || got.OSID != defaultOSID {
		t.Errorf("create body plan/region/os = %q/%q/%d", got.Plan, got.Region, got.OSID)
	}
	// Untagged (test) env: FormatUpstreamName returns the bare id, and the
	// hostname must carry the same correlation key as the label.
	if got.Label != "gpu-abc12345" || got.Hostname != got.Label {
		t.Errorf("label/hostname = %q/%q, want the upstream name on both", got.Label, got.Hostname)
	}
	decoded, err := base64.StdEncoding.DecodeString(got.UserData)
	if err != nil || string(decoded) != "#!/bin/bash\necho hi\n" {
		t.Errorf("user_data must be base64 of the byte-identical startup script; got %q (err %v)", got.UserData, err)
	}
	if got.Backups != "disabled" || got.ActivationEmail {
		t.Errorf("backups/activation_email = %q/%v, want disabled/false", got.Backups, got.ActivationEmail)
	}
}

// TestProvision_CreateIsNeverRetried locks the launchAttempts=1 contract:
// POST /instances is not idempotent, so a transient 5xx must NOT trigger a
// second create (a lost-response retry would orphan a billing VM the
// reconciler deliberately ignores).
func TestProvision_CreateIsNeverRetried(t *testing.T) {
	creates := 0
	adapter, _ := setupTestAdapter(t, map[string]http.HandlerFunc{
		"/instances": func(w http.ResponseWriter, r *http.Request) {
			creates++
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte(`{"error":"upstream hiccup","status":502}`))
		},
	})

	_, err := adapter.Provision(context.Background(), provider.ProvisionRequest{
		InstanceID:         "gpu-abc12345",
		GPUType:            provider.GPUTypeA16,
		GPUCount:           2,
		DatacenterLocation: "blr",
	})
	if err == nil {
		t.Fatal("expected the 502 create to fail")
	}
	if creates != 1 {
		t.Fatalf("POST /instances called %d times, want exactly 1 (non-idempotent create must never retry)", creates)
	}
}

// TestErrorStringsOmitUpstreamBody locks the privacy contract: provision
// failures reach customer-visible operation detail, so upstream response
// bodies must never appear in returned error strings.
func TestErrorStringsOmitUpstreamBody(t *testing.T) {
	adapter, _ := setupTestAdapter(t, map[string]http.HandlerFunc{
		"/instances": func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"SECRET-UPSTREAM-DETAIL account limit reached","status":400}`))
		},
	})

	_, err := adapter.Provision(context.Background(), provider.ProvisionRequest{
		InstanceID:         "gpu-abc12345",
		GPUType:            provider.GPUTypeA16,
		GPUCount:           2,
		DatacenterLocation: "blr",
	})
	if err == nil {
		t.Fatal("expected the 400 create to fail")
	}
	if strings.Contains(err.Error(), "SECRET-UPSTREAM-DETAIL") {
		t.Fatalf("error string leaks the upstream body: %q", err.Error())
	}
}

// TestProvision_EntitlementGatesMapToQuotaExceeded locks the account-gate
// mapping: the three Vultr account-side refusals hit live on 2026-08-14
// (minimum deposit, monthly fee limit, API-key IP ACL) must surface as
// provider.ErrQuotaExceeded — ops get a Sentry ping and the customer is
// steered away instead of invited to retry — and the upstream body must stay
// out of the error string (it reaches customer-visible operation detail).
func TestProvision_EntitlementGatesMapToQuotaExceeded(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
	}{
		{"min_deposit", http.StatusBadRequest,
			`{"error":"MARKER-BODY This product requires a minimum deposit of $50","status":400}`},
		{"monthly_fee_limit", http.StatusBadRequest,
			`{"error":"MARKER-BODY You have reached the maximum monthly fee limit for this account. To deploy additional services, please request a limit increase.","status":400}`},
		{"ip_acl", http.StatusUnauthorized,
			`{"error":"MARKER-BODY Unauthorized IP address: 203.0.113.7","status":401}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			creates := 0
			adapter, _ := setupTestAdapter(t, map[string]http.HandlerFunc{
				"/instances": func(w http.ResponseWriter, r *http.Request) {
					creates++
					w.WriteHeader(tc.status)
					_, _ = w.Write([]byte(tc.body))
				},
			})
			_, err := adapter.Provision(context.Background(), provider.ProvisionRequest{
				InstanceID:         "gpu-abc12345",
				GPUType:            provider.GPUTypeA16,
				GPUCount:           2,
				DatacenterLocation: "blr",
			})
			if !errors.Is(err, provider.ErrQuotaExceeded) {
				t.Fatalf("expected ErrQuotaExceeded, got %v", err)
			}
			if errors.Is(err, provider.ErrNoCapacity) {
				t.Fatalf("entitlement gate must not double as retryable ErrNoCapacity: %v", err)
			}
			if strings.Contains(err.Error(), "MARKER-BODY") {
				t.Fatalf("error string leaks the upstream body: %q", err.Error())
			}
			if creates != 1 {
				t.Fatalf("POST /instances called %d times, want 1 (entitlement gates are not retryable)", creates)
			}
		})
	}
}

// shrinkLockedRetry collapses the terminate lock-retry cadence for tests so the
// deadline-bounded loop can be exercised in milliseconds instead of minutes.
func shrinkLockedRetry(t *testing.T) {
	t.Helper()
	restore := terminateLockedRetryInterval
	terminateLockedRetryInterval = time.Millisecond
	t.Cleanup(func() { terminateLockedRetryInterval = restore })
}

// TestTerminate_RetriesWhileLocked: Vultr 400s a destroy during the first
// minutes of a deploy while the instance is "locked" — a self-clearing
// condition on an idempotent DELETE, so the adapter must back off and retry
// instead of bubbling a spurious terminate failure.
func TestTerminate_RetriesWhileLocked(t *testing.T) {
	shrinkLockedRetry(t)
	deletes := 0
	adapter, _ := setupTestAdapter(t, map[string]http.HandlerFunc{
		"/instances/vultr-inst-1": func(w http.ResponseWriter, r *http.Request) {
			deletes++
			if deletes == 1 {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"error":"Unable to destroy server: server is currently locked","status":400}`))
				return
			}
			w.WriteHeader(http.StatusNoContent)
		},
	})

	if err := adapter.Terminate(context.Background(), "vultr-inst-1", "atl"); err != nil {
		t.Fatalf("Terminate should succeed after the lock clears: %v", err)
	}
	if deletes != 2 {
		t.Fatalf("DELETE called %d times, want 2 (locked 400 must be retried)", deletes)
	}
}

// TestTerminate_RetriesOn409Conflict pins the OTHER shape of the same refusal.
// The body is verbatim what Vultr returned on 2026-09-06, when a 409 exhausted
// the old ~7s attempt ladder and stranded a smoke instance that then billed for
// 44h / ~$41.
func TestTerminate_RetriesOn409Conflict(t *testing.T) {
	shrinkLockedRetry(t)
	deletes := 0
	adapter, _ := setupTestAdapter(t, map[string]http.HandlerFunc{
		"/instances/vultr-inst-1": func(w http.ResponseWriter, r *http.Request) {
			deletes++
			if deletes == 1 {
				w.WriteHeader(http.StatusConflict)
				_, _ = w.Write([]byte(`{"error":"Server is currently locked","status":409}`))
				return
			}
			w.WriteHeader(http.StatusNoContent)
		},
	})

	if err := adapter.Terminate(context.Background(), "vultr-inst-1", "atl"); err != nil {
		t.Fatalf("Terminate should succeed after the 409 clears: %v", err)
	}
	if deletes != 2 {
		t.Fatalf("DELETE called %d times, want 2 (409 conflict must be retried)", deletes)
	}
}

// TestTerminate_LockOutlastsDeadline pins the exhaustion path. The loop is
// DEADLINE-bounded, not attempt-bounded, so the assertion is "kept trying until
// the context died" — and the surfaced error must still be a deadline error
// (so cost_open fires) whose string omits the upstream body.
func TestTerminate_LockOutlastsDeadline(t *testing.T) {
	shrinkLockedRetry(t)
	var mu sync.Mutex
	deletes := 0
	adapter, _ := setupTestAdapter(t, map[string]http.HandlerFunc{
		"/instances/vultr-inst-1": func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			deletes++
			mu.Unlock()
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"error":"MARKER-BODY Server is currently locked","status":409}`))
		},
	})

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()

	err := adapter.Terminate(ctx, "vultr-inst-1", "atl")
	if err == nil {
		t.Fatal("expected an error when the lock outlasts the deadline")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error must carry the deadline cause so cost_open fires, got %v", err)
	}
	if strings.Contains(err.Error(), "MARKER-BODY") {
		t.Fatalf("error string leaks the upstream body: %q", err.Error())
	}
	mu.Lock()
	got := deletes
	mu.Unlock()
	if got < 2 {
		t.Fatalf("DELETE called %d times, want >= 2 (the retry is deadline-bounded, not a fixed ladder)", got)
	}
}

// TestTerminate_NonLockErrorIsNotRetried: the adapter loop owns lock-retry
// policy only. A genuine client error must fast-fail on the first attempt, not
// get ground through a multi-minute loop.
func TestTerminate_NonLockErrorIsNotRetried(t *testing.T) {
	shrinkLockedRetry(t)
	deletes := 0
	adapter, _ := setupTestAdapter(t, map[string]http.HandlerFunc{
		"/instances/vultr-inst-1": func(w http.ResponseWriter, r *http.Request) {
			deletes++
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"Invalid instance ID","status":400}`))
		},
	})

	if err := adapter.Terminate(context.Background(), "vultr-inst-1", "atl"); err == nil {
		t.Fatal("expected a non-lock 400 to bubble")
	}
	if deletes != 1 {
		t.Fatalf("DELETE called %d times, want 1 (a non-lock client error is not retryable)", deletes)
	}
}

func TestProvision_RegionNotDeployable(t *testing.T) {
	adapter, _ := setupTestAdapter(t, nil)

	_, err := adapter.Provision(context.Background(), provider.ProvisionRequest{
		InstanceID:         "gpu-abc12345",
		GPUType:            provider.GPUTypeA16,
		GPUCount:           2,
		DatacenterLocation: "ewr", // 2×A16 lists atl+blr only
	})
	if !errors.Is(err, provider.ErrNoCapacity) {
		t.Fatalf("expected ErrNoCapacity for an undeployable region, got %v", err)
	}
}

func TestProvision_UnknownGPUType(t *testing.T) {
	adapter, _ := setupTestAdapter(t, nil)

	_, err := adapter.Provision(context.Background(), provider.ProvisionRequest{
		InstanceID: "gpu-abc12345",
		GPUType:    provider.GPUTypeH100SXM, // no Vultr plan carries it
		GPUCount:   1,
	})
	if !errors.Is(err, provider.ErrInvalidGPUType) {
		t.Fatalf("expected ErrInvalidGPUType, got %v", err)
	}
}

func TestGetStatus(t *testing.T) {
	adapter, _ := setupTestAdapter(t, map[string]http.HandlerFunc{
		"/instances/inst-123": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"instance":{"id":"inst-123","main_ip":"192.0.2.10","status":"active","power_status":"running","server_status":"ok"}}`))
		},
	})

	st, err := adapter.GetStatus(context.Background(), "inst-123", "atl")
	if err != nil {
		t.Fatalf("GetStatus: %v", err)
	}
	if st.Status != "running" || st.IP != "192.0.2.10" {
		t.Errorf("status/ip = %q/%q, want running/192.0.2.10", st.Status, st.IP)
	}
}

// TestGetStatus_InstallingIsCreating is the regression for the 2026-09-10
// lifecycle red: Vultr flips status to "active" and power_status to "stopped"
// while the install runs, and the old mapStatus read that pair as "stopping".
func TestGetStatus_InstallingIsCreating(t *testing.T) {
	adapter, _ := setupTestAdapter(t, map[string]http.HandlerFunc{
		"/instances/inst-boot": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"instance":{"id":"inst-boot","main_ip":"0.0.0.0","status":"active","power_status":"stopped","server_status":"installingbooting"}}`))
		},
	})

	st, err := adapter.GetStatus(context.Background(), "inst-boot", "atl")
	if err != nil {
		t.Fatalf("GetStatus: %v", err)
	}
	if st.Status != "creating" {
		t.Errorf("status = %q, want creating (a mid-install VM is booting, not stopping)", st.Status)
	}
	if st.IP != "" {
		t.Errorf("ip = %q, want empty (0.0.0.0 is Vultr's pre-assignment placeholder)", st.IP)
	}
}

func TestGetStatus_GoneIsTerminated(t *testing.T) {
	adapter, _ := setupTestAdapter(t, map[string]http.HandlerFunc{
		"/instances/": func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"invalid instance ID","status":404}`))
		},
	})

	st, err := adapter.GetStatus(context.Background(), "gone", "atl")
	if err != nil {
		t.Fatalf("GetStatus on 404 must converge, got error: %v", err)
	}
	if st.Status != "terminated" {
		t.Errorf("status = %q, want terminated", st.Status)
	}
}

// captureWarn runs fn with the default slog logger swapped for a WARN-level
// buffer and reports whether anything was logged. mapStatus signals contract
// drift by logging, not by returning — so "was it loud?" is part of the
// assertion, not decoration.
func captureWarn(t *testing.T, fn func()) bool {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	defer slog.SetDefault(prev)
	fn()
	return strings.Contains(buf.String(), "level=WARN")
}

// TestMapStatus pins the full documented state matrix. Vultr's enums are
// status: pending|active|suspended|closed (+resizing), server_status:
// none|locked|installingbooting|isomounting|ok, power_status: running|stopped.
// The matrix IS the bug: on 2026-09-10 a mid-install VM (active + stopped +
// installingbooting) mapped to "stopping", and the lifecycle harness read that
// as a dying instance — CI run 34484707350, verbatim
// `creating@+0s -> stopping@+1m4s`.
func TestMapStatus(t *testing.T) {
	cases := []struct {
		status, power, server, want string
		wantWarn                    bool
	}{
		// Pre-active.
		{status: "pending", power: "stopped", server: "none", want: "creating"},

		// Active + installing: server_status decides BEFORE power_status. This
		// row is the 2026-09-10 red.
		{status: "active", power: "stopped", server: "installingbooting", want: "creating"},
		{status: "active", power: "stopped", server: "locked", want: "creating"},
		{status: "active", power: "stopped", server: "isomounting", want: "creating"},
		{status: "active", power: "running", server: "installingbooting", want: "creating"},

		// Active + installed: power_status decides.
		{status: "active", power: "running", server: "ok", want: "running"},
		{status: "active", power: "stopped", server: "ok", want: "stopping"},

		// No server_status yet (pre-install rows, legacy fixtures): fall back
		// to power_status.
		{status: "active", power: "running", server: "none", want: "running"},
		{status: "active", power: "running", server: "", want: "running"},
		{status: "active", power: "stopped", server: "none", want: "creating"},
		{status: "active", power: "stopped", server: "", want: "creating"},

		// resizing behaves like active.
		{status: "resizing", power: "stopped", server: "ok", want: "stopping"},
		{status: "resizing", power: "running", server: "ok", want: "running"},

		// Terminal states.
		{status: "suspended", power: "running", server: "ok", want: "error"},
		{status: "closed", power: "stopped", server: "ok", want: "terminated"},

		// Drift must be LOUD. An unknown sub-state keeps the VM in "creating"
		// (never flip a possibly-running customer box to error on a sub-state
		// we do not recognise); an unknown TOP-LEVEL status is contract drift
		// and maps to "error" so it is noticed rather than looking healthy.
		{status: "active", power: "stopped", server: "some-new-value", want: "creating", wantWarn: true},
		{status: "active", power: "running", server: "some-new-value", want: "creating", wantWarn: true},
		{status: "active", power: "sideways", server: "ok", want: "creating", wantWarn: true},
		{status: "some-new-status", power: "running", server: "ok", want: "error", wantWarn: true},

		// Case-insensitive.
		{status: "ACTIVE", power: "RUNNING", server: "OK", want: "running"},
		{status: "ACTIVE", power: "STOPPED", server: "InstallingBooting", want: "creating"},
	}
	for _, c := range cases {
		var got string
		warned := captureWarn(t, func() { got = mapStatus(c.status, c.power, c.server) })
		if got != c.want {
			t.Errorf("mapStatus(%q,%q,%q) = %q, want %q", c.status, c.power, c.server, got, c.want)
		}
		if warned != c.wantWarn {
			t.Errorf("mapStatus(%q,%q,%q) warned=%v, want %v", c.status, c.power, c.server, warned, c.wantWarn)
		}
	}
}

func TestTerminate_Idempotent(t *testing.T) {
	adapter, _ := setupTestAdapter(t, map[string]http.HandlerFunc{
		"/instances/inst-123": func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodDelete {
				t.Fatalf("unexpected method %s", r.Method)
			}
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"invalid instance ID","status":404}`))
		},
	})

	if err := adapter.Terminate(context.Background(), "inst-123", "atl"); err != nil {
		t.Fatalf("Terminate on 404 must be idempotent success, got %v", err)
	}
}

func TestListProvisioned_FiltersForeignLabels(t *testing.T) {
	adapter, _ := setupTestAdapter(t, map[string]http.HandlerFunc{
		"/instances": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"instances":[
			{"id":"i-1","label":"gpu-abc12345","region":"atl","status":"active","power_status":"running"},
			{"id":"i-2","label":"my-personal-vm","region":"atl","status":"active","power_status":"running"},
			{"id":"i-3","label":"","region":"fra","status":"active","power_status":"running"}
		],"meta":{"links":{"next":""}}}`))
		},
	})

	rows, err := adapter.ListProvisioned(context.Background())
	if err != nil {
		t.Fatalf("ListProvisioned: %v", err)
	}
	// Only the instanceid-grammar label is ours (dev default tag accepts the
	// bare gpu-<8hex> form).
	if len(rows) != 1 || rows[0].UpstreamID != "i-1" || rows[0].Status != "running" {
		t.Fatalf("expected exactly the tagged instance, got %+v", rows)
	}
}

// TestListAllProvisioned_IncludesForeignLabels pins the RawLister contract: the
// UNFILTERED enumeration the leak sweep needs. A smoke orphan's "smoke-gpu-"
// prefix is outside the instanceid ownership grammar by design, so
// ListProvisioned can never see it — which is exactly why the 2026-09-06 leak
// read as a green sweep row for 44h.
func TestListAllProvisioned_IncludesForeignLabels(t *testing.T) {
	adapter, _ := setupTestAdapter(t, map[string]http.HandlerFunc{
		"/instances": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"instances":[
			{"id":"i-1","label":"gpu-abc12345","region":"atl","status":"active","power_status":"running"},
			{"id":"i-2","label":"my-personal-vm","region":"atl","status":"active","power_status":"running"},
			{"id":"i-3","label":"","region":"fra","status":"active","power_status":"running"},
			{"id":"i-4","label":"smoke-gpu-1788700018-7685","region":"ewr","status":"active","power_status":"running","server_status":"ok"}
		],"meta":{"links":{"next":""}}}`))
		},
	})

	rows, err := adapter.ListAllProvisioned(context.Background())
	if err != nil {
		t.Fatalf("ListAllProvisioned: %v", err)
	}
	if len(rows) != 4 {
		t.Fatalf("ListAllProvisioned returned %d rows, want all 4 (unfiltered): %+v", len(rows), rows)
	}
	byID := map[string]provider.ProvisionedInstance{}
	for _, r := range rows {
		byID[r.UpstreamID] = r
	}
	smoke, ok := byID["i-4"]
	if !ok {
		t.Fatal("the smoke orphan (i-4) is missing — the sweep would still be blind")
	}
	if smoke.Name != "smoke-gpu-1788700018-7685" || smoke.Region != "ewr" || smoke.Status != "running" {
		t.Errorf("smoke row = %+v, want name/region/status smoke-gpu-1788700018-7685/ewr/running", smoke)
	}
	if _, ok := byID["i-3"]; !ok {
		t.Error("the empty-label row (i-3) is missing — an unlabelled VM still bills")
	}

	// The ownership-filtered view must be unchanged by all of this.
	owned, err := adapter.ListProvisioned(context.Background())
	if err != nil {
		t.Fatalf("ListProvisioned: %v", err)
	}
	if len(owned) != 1 || owned[0].UpstreamID != "i-1" {
		t.Fatalf("ListProvisioned must stay ownership-filtered (the reconciler reaps from it), got %+v", owned)
	}
}

// TestListAllProvisioned_Paginates: the account walk must follow
// meta.links.next, or a leak on page 2 is invisible.
func TestListAllProvisioned_Paginates(t *testing.T) {
	adapter, _ := setupTestAdapter(t, map[string]http.HandlerFunc{
		"/instances": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			if r.URL.Query().Get("cursor") == "" {
				_, _ = w.Write([]byte(`{"instances":[
				{"id":"i-1","label":"smoke-gpu-1","region":"atl","status":"active","power_status":"running","server_status":"ok"}
			],"meta":{"links":{"next":"page2+cursor"}}}`))
				return
			}
			_, _ = w.Write([]byte(`{"instances":[
			{"id":"i-2","label":"smoke-gpu-2","region":"ewr","status":"active","power_status":"running","server_status":"ok"}
		],"meta":{"links":{"next":""}}}`))
		},
	})

	rows, err := adapter.ListAllProvisioned(context.Background())
	if err != nil {
		t.Fatalf("ListAllProvisioned: %v", err)
	}
	if len(rows) != 2 || rows[0].UpstreamID != "i-1" || rows[1].UpstreamID != "i-2" {
		t.Fatalf("expected both pages concatenated, got %+v", rows)
	}
}

func TestParseGPUCount(t *testing.T) {
	cases := []struct {
		raw  string
		want int
		ok   bool
	}{
		{`2`, 2, true},
		{`"1/8"`, 0, false},
		{`"4"`, 4, true},
		{`"1/24"`, 0, false},
		{`0`, 0, false},
		{`""`, 0, false},
	}
	for _, c := range cases {
		got, ok := parseGPUCount([]byte(c.raw))
		if got != c.want || ok != c.ok {
			t.Errorf("parseGPUCount(%s) = (%d,%v), want (%d,%v)", c.raw, got, ok, c.want, c.ok)
		}
	}
}

// liveCatalogFixture mirrors the UNFILTERED GET /v2/plans shape live on
// 2026-11-04, after Vultr moved its whole-GPU plans from type "vcg" to "vdm":
// a stocked-out vcg L40S (still carries gpu_type), two vdm NVIDIA plans with
// stock (no gpu_type/gpu_count/gpu_vram_gb, GPU only in the id), a stocked-out
// 16×A16 vdm plan, an in-stock AMD vdm plan (no canonical type, skipped), and
// a CPU plan whose gpu_brand is "none" (filtered before the adapter sees it).
const liveCatalogFixture = `{
  "plans": [
    {"id":"vc2-1c-1gb","vcpu_count":1,"ram":1024,"disk":25,"hourly_cost":0.007,
     "monthly_cost":5,"type":"vc2","locations":["ewr"],"deploy_ondemand":true,"gpu_brand":"none"},
    {"id":"vcg-l40s-16c-180g-48vram","vcpu_count":16,"ram":184320,"disk":1200,
     "hourly_cost":1.671,"monthly_cost":1122.91,"type":"vcg","locations":[],
     "deploy_ondemand":true,"gpu_brand":"NVIDIA","gpu_vram_gb":48,
     "gpu_type":"NVIDIA_L40S","gpu_count":1},
    {"id":"vcg-a16-6c-60g-16vram","vcpu_count":6,"ram":61440,"disk":350,
     "hourly_cost":0.471,"monthly_cost":344,"type":"vdm","locations":["ewr","ord"],
     "deploy_ondemand":true,"gpu_brand":"NVIDIA"},
    {"id":"vcg-a100-12c-120g-80vram","vcpu_count":12,"ram":122880,"disk":1400,
     "hourly_cost":2.397,"monthly_cost":1750,"type":"vdm","locations":["ewr"],
     "deploy_ondemand":true,"gpu_brand":"NVIDIA"},
    {"id":"vcg-a16-96c-960g-256vram","vcpu_count":96,"ram":983040,"disk":1400,
     "hourly_cost":7.534,"monthly_cost":5500,"type":"vdm","locations":[],
     "deploy_ondemand":true,"gpu_brand":"NVIDIA"},
    {"id":"vcg-mi325x-252c-2872g-1536vram","vcpu_count":252,"ram":2940928,"disk":1400,
     "hourly_cost":36.92,"monthly_cost":24810,"type":"vdm","locations":["ewr"],
     "deploy_ondemand":true,"gpu_brand":"AMD"}
  ],
  "meta":{"total":6,"links":{"next":"","prev":""}}
}`

func TestListAvailable_VDMPlans(t *testing.T) {
	adapter, _ := setupTestAdapter(t, map[string]http.HandlerFunc{
		"/plans": func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Query().Get("type") != "" {
				// The regression: a type filter hides every vdm plan.
				t.Errorf("plans requested with type=%q; the catalog must be read unfiltered", r.URL.Query().Get("type"))
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(liveCatalogFixture))
		},
	})

	offerings, err := adapter.ListAvailable(context.Background())
	if err != nil {
		t.Fatalf("ListAvailable: %v", err)
	}
	type key struct {
		plan, region string
		gt           provider.GPUType
		count, vram  int
	}
	var got []key
	for _, o := range offerings {
		got = append(got, key{o.UpstreamGPUID, o.Region, o.GPUType, o.GPUCount, o.VRAMPerGPUGB})
	}
	want := []key{
		{"vcg-a16-6c-60g-16vram", "ewr", provider.GPUTypeA16, 1, 16},
		{"vcg-a16-6c-60g-16vram", "ord", provider.GPUTypeA16, 1, 16},
		{"vcg-a100-12c-120g-80vram", "ewr", provider.GPUTypeA10080GB, 1, 80},
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("offerings =\n  %v\nwant\n  %v", got, want)
	}
	if o := offerings[2]; o.PricePerHour != 2.397 || o.RAMGB != 120 || o.CPUCores != 12 || o.StorageGB != 1400 {
		t.Errorf("a100 specs = $%v ram %d cpu %d disk %d, want $2.397/120/12/1400", o.PricePerHour, o.RAMGB, o.CPUCores, o.StorageGB)
	}
}

func TestListGPUPlans_FollowsCursor(t *testing.T) {
	var calls int
	adapter, _ := setupTestAdapter(t, map[string]http.HandlerFunc{
		"/plans": func(w http.ResponseWriter, r *http.Request) {
			calls++
			w.Header().Set("Content-Type", "application/json")
			if r.URL.Query().Get("cursor") == "" {
				_, _ = w.Write([]byte(`{"plans":[{"id":"vcg-a16-6c-60g-16vram","type":"vdm","gpu_brand":"NVIDIA"}],"meta":{"links":{"next":"p+2"}}}`))
				return
			}
			if r.URL.Query().Get("cursor") != "p+2" {
				t.Errorf("cursor = %q, want p+2 (escaped on the wire)", r.URL.Query().Get("cursor"))
			}
			_, _ = w.Write([]byte(`{"plans":[{"id":"vcg-a100-12c-120g-80vram","type":"vdm","gpu_brand":"NVIDIA"}],"meta":{"links":{"next":""}}}`))
		},
	})
	plans, err := adapter.client.listGPUPlans(context.Background())
	if err != nil {
		t.Fatalf("listGPUPlans: %v", err)
	}
	if calls != 2 || len(plans) != 2 {
		t.Fatalf("calls=%d plans=%d, want 2 and 2", calls, len(plans))
	}
}

func TestGPUFromPlanID(t *testing.T) {
	cases := []struct {
		id    string
		gt    provider.GPUType
		count int
		ok    bool
	}{
		{"vcg-a16-6c-60g-16vram", provider.GPUTypeA16, 1, true},
		{"vcg-a16-12c-120g-32vram", provider.GPUTypeA16, 2, true},
		{"vcg-a16-96c-960g-256vram", provider.GPUTypeA16, 16, true},
		{"vcg-a40-24c-120g-48vram", provider.GPUTypeA40, 1, true},
		{"vcg-a40-96c-480g-192vram", provider.GPUTypeA40, 4, true},
		{"vcg-a100-12c-120g-80vram", provider.GPUTypeA10080GB, 1, true},
		{"vcg-a100-96c-960g-640vram", provider.GPUTypeA10080GB, 8, true},
		{"vcg-h100-216c-1914gb-640vram", provider.GPUTypeH100SXM, 8, true},
		{"vcg-b200-248c-2826g-1536vram", provider.GPUTypeB200, 8, true},
		{"vcg-a16-2c-8g-2vram", "", 0, false},            // fractional slice
		{"vcg-mi325x-252c-2872g-1536vram", "", 0, false}, // no canonical AMD type
		{"vcg-a100-1c-85gb", "", 0, false},               // no vram suffix
		{"vc2-1c-1gb", "", 0, false},
	}
	for _, c := range cases {
		gt, count, ok := gpuFromPlanID(c.id)
		if gt != c.gt || count != c.count || ok != c.ok {
			t.Errorf("gpuFromPlanID(%q) = (%q, %d, %v), want (%q, %d, %v)", c.id, gt, count, ok, c.gt, c.count, c.ok)
		}
	}
}

func TestFindPlan_VDMPlan(t *testing.T) {
	var resp struct {
		Plans []plan `json:"plans"`
	}
	if err := json.Unmarshal([]byte(liveCatalogFixture), &resp); err != nil {
		t.Fatal(err)
	}
	id, region, hourly, err := findPlan(resp.Plans, provider.GPUTypeA10080GB, 1, "", "vcg-a100-12c-120g-80vram")
	if err != nil || id != "vcg-a100-12c-120g-80vram" || region != "ewr" || hourly != 2.397 {
		t.Fatalf("findPlan = (%q, %q, %v, %v), want the vdm A100 plan in ewr", id, region, hourly, err)
	}
	if _, _, _, err := findPlan(resp.Plans, provider.GPUTypeA16, 16, "", ""); !errors.Is(err, provider.ErrNoCapacity) {
		t.Fatalf("16×A16 with no locations: err = %v, want ErrNoCapacity", err)
	}
}
