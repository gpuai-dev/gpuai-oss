package vultr

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gpuai-dev/gpuai-oss/internal/instanceid"
	"github.com/gpuai-dev/gpuai-oss/internal/provider"
)

// Compile-time checks: Adapter implements provider.Provider, and the optional
// provider.RawLister capability the leak sweep uses to see smoke orphans that
// ListProvisioned's ownership filter hides.
var (
	_ provider.Provider  = (*Adapter)(nil)
	_ provider.RawLister = (*Adapter)(nil)
)

const (
	// estimatedReadySeconds — full-VM boot + cloud-init bootstrap (driver
	// stack is preinstalled by Vultr's Cloud GPU images).
	estimatedReadySeconds = 300

	// defaultOSID is Vultr's numeric os_id for "Ubuntu 22.04 LTS x64"
	// (live-verified in GET /v2/os, 2026-08-11). Vultr Cloud GPU deploys
	// preconfigure the NVIDIA vGPU driver + licensing on their stock OS
	// images, so a plain Ubuntu boot hands over a working GPU. Overridable
	// per launch via ProvisionRequest.OSImage (numeric id or OS name).
	defaultOSID = 1743
)

// Account-side supply caps (portal figures, 2026-08-18): Vultr enforces a
// per-account instance-count limit and a monthly-fee limit consumed by the
// DEPLOYED monthly LIST price of every instance on the account (a 2×A16 alone
// eats $688/mo of it). Neither is queryable via the API — the portal is the
// only place they show — so the adapter mirrors them and suppresses offerings
// that could not actually deploy, keeping the cap invisible to customers
// instead of surfacing as a failed launch. Overridable via config when Vultr
// support raises the account's limits (VULTR_MAX_INSTANCES /
// VULTR_MAX_MONTHLY_COST_USD).
const (
	// Defaults match the limits of the account the adapter was developed
	// against. Set the env vars to your own account's limits; Vultr support
	// raises them on request after a period of normal use.
	defaultMaxInstances      = 15
	defaultMaxMonthlyCostUSD = 4000
)

// Adapter implements provider.Provider for Vultr Cloud GPU.
type Adapter struct {
	client *Client

	// maxInstances caps offerings on the account's instance count; 0 disables.
	maxInstances int
	// maxMonthlyCostUSD caps offerings on the account's committed monthly list
	// price (deployed instances' plan monthly_cost summed); 0 disables.
	maxMonthlyCostUSD float64
}

// NewAdapter creates a Vultr adapter. The key gate (refuse to register when
// VULTR_API_KEY is empty) lives in cmd/gpuctl/deps.go; this constructor never
// inspects apiKey.
func NewAdapter(apiKey string, opts ...ClientOption) *Adapter {
	return &Adapter{
		client:            NewClient(apiKey, opts...),
		maxInstances:      defaultMaxInstances,
		maxMonthlyCostUSD: defaultMaxMonthlyCostUSD,
	}
}

// SetAccountLimits overrides the account-side supply caps (chainable). Zero disables the corresponding guard.
func (a *Adapter) SetAccountLimits(maxInstances int, maxMonthlyCostUSD float64) *Adapter {
	a.maxInstances = maxInstances
	a.maxMonthlyCostUSD = maxMonthlyCostUSD
	return a
}

// Name returns the provider identifier.
func (a *Adapter) Name() string { return "vultr" }

// ListAvailable polls the Cloud GPU plan catalog and emits one WHOLESALE-USD
// offering per whole-GPU plan × listed region. Fractional vGPU plans (A16
// "1/8", A40 "1/24", …) don't fit the platform's whole-GPU model and are
// skipped silently, as are plans with no deployable region. Because the plan
// catalog is public, credentials are probed against GET /account first — a
// revoked key must red the poll, not keep advertising capacity every
// provision would 401 on. Offerings the account could not deploy under its
// portal-side limits (instance count, monthly fee) are suppressed here so the
// caps stay invisible to customers — see accountUsage.
func (a *Adapter) ListAvailable(ctx context.Context) ([]provider.GPUOffering, error) {
	ctx, cancel := contextWithDefaultTimeout(ctx, listTimeout)
	defer cancel()

	if err := a.client.checkAuth(ctx); err != nil {
		return nil, fmt.Errorf("list available: auth probe: %w", err)
	}

	plans, err := a.client.listGPUPlans(ctx)
	if err != nil {
		return nil, fmt.Errorf("list available: %w", err)
	}

	// Account-side supply caps: count the account's live instances and their
	// committed monthly list price, and emit only offerings the account could
	// actually deploy RIGHT NOW. ALL instances count — both envs share the
	// account and manual VMs consume the same caps — so this deliberately does
	// not filter on our label the way ListProvisioned does. An offering that
	// would blow a cap never reaches the catalog, so the limit stays invisible
	// to customers instead of surfacing as a failed launch. With both guards
	// disabled (0/0 — the incident kill knob) the usage read is skipped
	// entirely, restoring pre-guard behavior: a flaky /instances endpoint must
	// not keep redding polls the operator has explicitly un-guarded.
	var instanceCount int
	var committedMonthly float64
	if a.maxInstances > 0 || a.maxMonthlyCostUSD > 0 {
		instanceCount, committedMonthly, err = a.accountUsage(ctx, plans)
		if err != nil {
			// Error, not an empty catalog: the poller's breaker/LKG retention
			// treats these differently, and flying blind on usage must red.
			return nil, fmt.Errorf("list available: account usage: %w", err)
		}
	}
	if a.maxInstances > 0 && instanceCount >= a.maxInstances {
		// Info, not Warn: at-cap is a legitimate steady state that can hold
		// for days (the #548 per-cycle-warn lesson); the fields below keep it
		// diagnosable without paging anyone.
		slog.Info("vultr at account instance cap — suppressing all offerings",
			slog.Int("account_instances", instanceCount),
			slog.Int("max_instances", a.maxInstances),
			slog.Float64("committed_monthly_usd", committedMonthly),
		)
		return nil, nil
	}

	var offerings []provider.GPUOffering
	suppressedPlans := 0
	for _, p := range plans {
		gpuType, count, vram, ok := planGPU(p)
		if !ok {
			continue // fractional vGPU slice, unmapped model or unparsable — expected skip
		}
		if !p.DeployOnDemand || len(p.Locations) == 0 {
			continue // not deployable, or out of stock in every region
		}
		// Monthly-fee-limit headroom: one more instance of this plan must fit
		// under the cap, which Vultr consumes by deployed monthly LIST price.
		if a.maxMonthlyCostUSD > 0 && committedMonthly+p.MonthlyCost > a.maxMonthlyCostUSD {
			suppressedPlans++
			continue
		}
		for _, region := range p.Locations {
			if region == "" {
				continue
			}
			offerings = append(offerings, provider.GPUOffering{
				Provider:           "vultr",
				GPUType:            gpuType,
				GPUCount:           count,
				VRAMPerGPUGB:       vram,
				CPUCores:           p.VCPUCount,
				RAMGB:              provider.MiBToGB(p.RAMMB),
				StorageGB:          p.DiskGB,
				PricePerHour:       p.HourlyCost, // WHOLE-INSTANCE hourly USD as listed
				Tier:               provider.TierOnDemand,
				Region:             region, // NATIVE code — availability.Normalize maps it
				DatacenterLocation: region,
				// Vultr exposes no numeric stock — a region in the plan's
				// locations list is deployable NOW, so emit the display-grade
				// "at least one".
				// A zero here is not neutral: the engine's candidate loop
				// drops offerings with AvailableCount <= 0, which took vultr
				// out of every placement (live-hit 2026-08-12).
				StockStatus:    "in_stock",
				AvailableCount: 1,
				UpstreamGPUID:  p.ID, // pin the exact plan the row advertised
			})
		}
	}

	slog.Info("listed provider GPU offerings",
		slog.String("provider", "vultr"),
		slog.Int("total_offerings", len(offerings)),
		slog.Int("raw_plans", len(plans)),
		// Cap telemetry: "vultr dark" must be diagnosable as our own supply
		// guard, not upstream stock-out — these three fields say which.
		slog.Int("account_instances", instanceCount),
		slog.Float64("committed_monthly_usd", committedMonthly),
		slog.Int("plans_over_monthly_cap", suppressedPlans),
	)
	return offerings, nil
}

// accountUsage returns the account's live instance count and the committed
// monthly list price (each instance's plan monthly_cost summed), the two
// figures Vultr's portal-only account limits are consumed by. Costs are
// resolved against the already-fetched GPU catalog; an instance on a plan
// outside it (a manual non-GPU VM) still counts toward the instance cap but
// contributes $0 — GPU plans dominate the fee cap by an order of magnitude,
// and understating is the safe direction only because Vultr itself still
// hard-refuses at the true cap (mapped to ErrQuotaExceeded).
func (a *Adapter) accountUsage(ctx context.Context, plans []plan) (int, float64, error) {
	monthlyByPlan := make(map[string]float64, len(plans))
	for _, p := range plans {
		monthlyByPlan[p.ID] = p.MonthlyCost
	}

	count := 0
	committed := 0.0
	unknownPlans := 0
	cursor := ""
	for {
		rows, next, err := a.client.listInstancesPage(ctx, cursor)
		if err != nil {
			return 0, 0, err
		}
		count += len(rows)
		for _, r := range rows {
			cost, ok := monthlyByPlan[r.Plan]
			if !ok {
				unknownPlans++
				continue
			}
			committed += cost
		}
		if next == "" {
			break
		}
		cursor = next
	}
	if unknownPlans > 0 {
		// Info, not Warn: a manual non-GPU VM parked on the account makes this
		// a permanent condition — per-poll Warns would spam forever (#548).
		slog.Info("vultr instances on plans outside the GPU catalog — counted toward the instance cap at $0 monthly",
			slog.Int("unknown_plan_instances", unknownPlans),
		)
	}
	return count, committed, nil
}

// resolvePlan resolves the (plan id, region) to launch for a requested
// (GPU type, count, region preference) from the LIVE plan catalog — never
// hardcoded. pinned (ProvisionRequest.UpstreamGPUID = the plan id the matched
// offering advertised) is validated against the live catalog and the request
// before it is trusted: it is placement routing, never an override to a
// different GPU. An unsatisfiable request returns provider.ErrInvalidGPUType
// (no matching plan) or provider.ErrNoCapacity (plan matched, region not
// deployable).
func (a *Adapter) resolvePlan(ctx context.Context, gpuType provider.GPUType, gpuCount int, regionPref, pinned string) (planID, region string, hourly float64, err error) {
	plans, err := a.client.listGPUPlans(ctx)
	if err != nil {
		return "", "", 0, fmt.Errorf("resolve plan: %w", err)
	}
	return findPlan(plans, gpuType, gpuCount, regionPref, pinned)
}

// findPlan searches an already-fetched catalog (split from resolvePlan so the
// pin-miss fallback re-searches the same slice instead of re-fetching inside
// the provision window).
func findPlan(plans []plan, gpuType provider.GPUType, gpuCount int, regionPref, pinned string) (planID, region string, hourly float64, err error) {
	matched := false
	for _, p := range plans {
		rowType, count, _, ok := planGPU(p)
		if !ok || !p.DeployOnDemand || rowType != gpuType {
			continue
		}
		// Plans carry a FIXED GPU count — a count mismatch is a different
		// SKU. gpuCount 0 means "unspecified" → any count matches.
		if gpuCount > 0 && count != gpuCount {
			continue
		}
		// A pinned plan id narrows the match to the exact advertised SKU;
		// a pin that no longer exists in the live catalog falls through to
		// the regular type+count match.
		if pinned != "" && p.ID != pinned {
			continue
		}
		matched = true
		if regionPref == "" {
			if len(p.Locations) > 0 {
				return p.ID, p.Locations[0], p.HourlyCost, nil
			}
			continue
		}
		for _, loc := range p.Locations {
			if strings.EqualFold(loc, regionPref) {
				return p.ID, loc, p.HourlyCost, nil
			}
		}
	}
	if pinned != "" {
		// Retry without the pin: the advertised plan may have rotated out
		// between poll and launch while an equivalent SKU still exists.
		return findPlan(plans, gpuType, gpuCount, regionPref, "")
	}
	if matched {
		return "", "", 0, fmt.Errorf("no vultr plan for GPU %s count %d deployable in region %q: %w",
			gpuType, gpuCount, regionPref, provider.ErrNoCapacity)
	}
	return "", "", 0, fmt.Errorf("no vultr plan for GPU %s count %d: %w",
		gpuType, gpuCount, provider.ErrInvalidGPUType)
}

// sshKeysCloudConfig is the cloud-init user data that installs keys for root. Each
// key is quoted as a JSON string, which YAML reads as-is, so a key's contents
// cannot add YAML of their own.
func sshKeysCloudConfig(keys []string) string {
	var b strings.Builder
	b.WriteString("#cloud-config\nusers:\n  - name: root\n    ssh_authorized_keys:\n")
	for _, k := range keys {
		q, _ := json.Marshal(strings.TrimSpace(k))
		b.WriteString("      - ")
		b.Write(q)
		b.WriteString("\n")
	}
	return b.String()
}

// resolveOSID resolves ProvisionRequest.OSImage — the server-resolved boot
// image (numeric Vultr os_id, or an OS display name matched case-insensitively
// against GET /os) — falling back to the Ubuntu 22.04 default. OSImage is
// never customer input (V5): the engine resolves it from the manifest.
func (a *Adapter) resolveOSID(ctx context.Context, osImage string) (int, error) {
	osImage = strings.TrimSpace(osImage)
	if osImage == "" {
		return defaultOSID, nil
	}
	if id, err := strconv.Atoi(osImage); err == nil && id > 0 {
		return id, nil
	}
	images, err := a.client.listOS(ctx)
	if err != nil {
		return 0, fmt.Errorf("resolve os %q: %w", osImage, err)
	}
	for _, img := range images {
		if strings.EqualFold(img.Name, osImage) {
			return img.ID, nil
		}
	}
	return 0, fmt.Errorf("resolve os: no vultr os image named %q", osImage)
}

// Provision creates a new Cloud GPU VM.
//
// The plan + region are resolved from the LIVE catalog (pin-first via
// req.UpstreamGPUID); region preference is req.DatacenterLocation falling
// back to req.Region (both carry the native code for offerings this adapter
// emitted). req.StartupScript — the engine-rendered VM bootstrap, which also
// installs the customer's authorized_keys — rides base64 in user_data
// byte-identical; cloud-init runs it on first boot. The upstream label and
// hostname carry the env-tagged instance name (the ListProvisioned
// correlation key). Vultr VMs boot a provider OS image, so DockerImage /
// PinnedImageRef are not consumed here — the certified docker-run happens
// inside the engine's bootstrap, exactly like the other VM adapters.
func (a *Adapter) Provision(ctx context.Context, req provider.ProvisionRequest) (*provider.ProvisionResult, error) {
	ctx, cancel := contextWithDefaultTimeout(ctx, provisionTimeout)
	defer cancel()

	if req.GPUType == "" {
		return nil, fmt.Errorf("provision: gpu type required: %w", provider.ErrInvalidGPUType)
	}

	regionPref := req.DatacenterLocation
	if regionPref == "" {
		regionPref = req.Region
	}
	// A canonical bucket ("us-east") can reach this adapter when the engine
	// forwards a request without a native placement; it matches no Vultr
	// region id, so treat it as "any region" rather than an impossible pin.
	if regionPref != "" && !KnownRegion(strings.ToLower(regionPref)) {
		regionPref = ""
	}

	planID, region, hourly, err := a.resolvePlan(ctx, req.GPUType, req.GPUCount, regionPref, req.UpstreamGPUID)
	if err != nil {
		return nil, fmt.Errorf("provision: %w", err)
	}

	osID, err := a.resolveOSID(ctx, req.OSImage)
	if err != nil {
		return nil, fmt.Errorf("provision: %w", err)
	}

	// Env-tagged upstream name (e.g. "prod-gpu-<8hex>") so a reconciler in
	// another env sharing this account doesn't reap it as an orphan.
	// req.InstanceID (bare) stays the callback/DB identity. The name doubles
	// as the hostname — the instanceid grammar is hostname-safe.
	upstreamName := instanceid.FormatUpstreamName(req.InstanceID)

	create := createInstanceRequest{
		Region:          region,
		Plan:            planID,
		OSID:            osID,
		Label:           upstreamName,
		Hostname:        upstreamName,
		Backups:         "disabled",
		ActivationEmail: false,
		DDOSProtection:  false,
		Tags:            []string{"gpuai"},
	}
	if req.StartupScript != "" {
		// Byte-identical bootstrap (D-04 — never re-render): Vultr decodes
		// the base64 and hands it to cloud-init as-is.
		create.UserData = base64.StdEncoding.EncodeToString([]byte(req.StartupScript))
	} else if len(req.SSHPublicKeys) > 0 {
		// Direct connect renders no bootstrap, so nothing else would install the
		// customer's keys on this VM.
		create.UserData = base64.StdEncoding.EncodeToString([]byte(sshKeysCloudConfig(req.SSHPublicKeys)))
	}

	inst, err := a.client.createInstance(ctx, create)
	if err != nil {
		return nil, fmt.Errorf("provision: %w", err)
	}

	slog.Info("provisioned provider instance",
		slog.String("provider", "vultr"),
		slog.String("upstream_id", inst.ID),
		slog.String("plan", planID),
		slog.String("region", region),
	)

	return &provider.ProvisionResult{
		UpstreamID:            inst.ID,
		UpstreamIP:            realIP(inst.MainIP),
		Provider:              "vultr",
		Status:                "creating",
		Region:                region,
		DatacenterLocation:    region,
		CostPerHour:           hourly,
		EstimatedReadySeconds: estimatedReadySeconds,
	}, nil
}

// realIP filters Vultr's pre-assignment placeholder ("0.0.0.0") so callers
// never dial a bogus address.
func realIP(ip string) string {
	if ip == "0.0.0.0" {
		return ""
	}
	return ip
}

// mapStatus maps Vultr's (status, power_status, server_status) triple onto the
// canonical lifecycle set (creating, running, stopping, terminated, error).
//
// The three upstream enums, verbatim:
//
//	status:        pending | active | suspended | closed   (+ resizing)
//	server_status: none | locked | installingbooting | isomounting | ok
//	power_status:  running | stopped
//
// server_status is only meaningful while status == active: Vultr flips status
// to "active" as soon as the VM record exists, then runs the install with
// power_status still "stopped". Reading power_status alone therefore calls a
// mid-install VM "stopping" — which is exactly the 2026-09-10 lifecycle red
// (`creating@+0s -> stopping@+1m4s`, CI run 34484707350). So under active/
// resizing we consult server_status FIRST and only fall through to
// power_status once the install is done ("ok") or was never reported ("none"/"").
//
// Unrecognised values are LOUD but asymmetric, on purpose. An unknown TOP-LEVEL
// status is contract drift on a closed enum, so it maps to "error": it charges
// the launch breaker and gets noticed, rather than looking like a healthy boot
// forever (the old `default: creating` swallowed "closed" that way). An unknown
// SUB-state maps to "creating": we will not flip a customer VM that may well be
// running into a terminal state over a sub-state we simply do not recognise.
// Both log at Warn. The Warn is per-call and GetStatus polls ~every 30s, so a
// genuinely new enum value would be noisy — that noise IS the signal (#548's
// lesson is about PERMANENT conditions; an unknown state is expected to be
// rare-to-never).
//
// KNOWN LIMITATION — this does NOT make the launch breaker fire for Vultr's
// most common failure mode. Vultr has no failure status: a deploy that never
// boots stays "pending", or sits "active"+"installingbooting" indefinitely, or
// 404s. This function correctly reports that as "creating", and the launch
// breaker (internal/provision/engine.go, charged only on "terminated"/"error")
// is therefore still blind to a boot-stall. Closing that needs a generic
// timeout→breaker feed in the engine, which is out of scope here — see
// 260911-kzm-deferred-items.md item 1.
func mapStatus(status, powerStatus, serverStatus string) string {
	status = strings.ToLower(status)
	powerStatus = strings.ToLower(powerStatus)
	serverStatus = strings.ToLower(serverStatus)

	switch status {
	case "pending":
		return "creating"
	case "suspended":
		return "error"
	case "closed":
		return "terminated"
	case "active", "resizing":
		// server_status first: it is the only field that distinguishes
		// "installing, not yet powered on" from "deliberately powered off".
		switch serverStatus {
		case "locked", "installingbooting", "isomounting":
			return "creating"
		case "ok":
			switch powerStatus {
			case "running":
				return "running"
			case "stopped":
				return "stopping"
			default:
				warnUnknownState("vultr: unrecognised instance power_status", status, powerStatus, serverStatus)
				return "creating"
			}
		case "none", "":
			// Pre-install or a row that never carried server_status: the only
			// evidence is the power state.
			if powerStatus == "running" {
				return "running"
			}
			return "creating"
		default:
			warnUnknownState("vultr: unrecognised instance server_status", status, powerStatus, serverStatus)
			return "creating"
		}
	default:
		warnUnknownState("vultr: unrecognised instance status — adapter state model may have drifted", status, powerStatus, serverStatus)
		return "error"
	}
}

// warnUnknownState makes state-model drift visible. Vultr's enums are closed
// and documented; a value outside them means the upstream contract moved under
// us, and silence there is how a stuck instance bills forever.
func warnUnknownState(msg, status, powerStatus, serverStatus string) {
	slog.Warn(msg,
		slog.String("provider", "vultr"),
		slog.String("status", status),
		slog.String("power_status", powerStatus),
		slog.String("server_status", serverStatus),
	)
}

// GetStatus returns the current status of an instance by its Vultr id. The
// region param is threaded but does not vary behavior (instances are
// addressed globally). A 404 → "terminated" so the engine converges
// idempotently.
func (a *Adapter) GetStatus(ctx context.Context, upstreamID, region string) (*provider.InstanceStatus, error) {
	_ = region
	ctx, cancel := contextWithDefaultTimeout(ctx, statusTimeout)
	defer cancel()

	inst, err := a.client.getInstance(ctx, upstreamID)
	if err != nil {
		var hse *httpStatusError
		if errors.As(err, &hse) && hse.StatusCode == http.StatusNotFound {
			return &provider.InstanceStatus{UpstreamID: upstreamID, Status: "terminated"}, nil
		}
		return nil, fmt.Errorf("get status: %w", err)
	}

	return &provider.InstanceStatus{
		UpstreamID: upstreamID,
		Status:     mapStatus(inst.Status, inst.PowerStatus, inst.ServerStatus),
		IP:         realIP(inst.MainIP),
	}, nil
}

// terminateLockedRetryInterval is the steady delay between DELETE retries while
// Vultr refuses the destroy because the instance is locked. A var, not a const,
// so tests can shrink it; production keeps the 15s cadence (the client's own
// doWithRetry treats every non-429 4xx as non-retryable, so this adapter-level
// loop owns the terminate-specific policy — steady, not exponential, over
// minutes).
var terminateLockedRetryInterval = 15 * time.Second

// deleteInstanceRetryingLocked deletes an instance, RETRYING while Vultr refuses
// because the instance is locked. Vultr rejects a destroy with "server is
// currently locked" — as a 400, or as a 409 during an upstream incident — while
// a freshly created instance is still settling. The old client-side ladder gave
// up after ~7s, which is how the 2026-09-06 smoke instance survived its own
// teardown and billed for 44h / ~$41. We retry on a steady interval until the
// delete is accepted or the context deadline hits.
//
// Any other error returns immediately for the caller's handling (404 /
// already-gone keeps feeding Terminate's idempotency branch). A lock that never
// clears surfaces as a ctx-deadline error so the cost-open alert still fires —
// never a silent success. The upstream body is consulted only for the "locked"
// substring and never enters the returned error (privacy contract on
// httpStatusError).
func (a *Adapter) deleteInstanceRetryingLocked(ctx context.Context, upstreamID string) error {
	for attempt := 0; ; attempt++ {
		err := a.client.deleteInstance(ctx, upstreamID)
		if err == nil {
			return nil
		}
		var hse *httpStatusError
		if !errors.As(err, &hse) {
			return err
		}
		locked := hse.StatusCode == http.StatusConflict ||
			(hse.StatusCode == http.StatusBadRequest && strings.Contains(strings.ToLower(hse.Body), "locked"))
		if !locked {
			return err // genuine client error (incl. 404) — surface it
		}
		slog.Warn("vultr terminate refused — instance locked, retrying",
			slog.String("provider", "vultr"),
			slog.String("upstream_id", upstreamID),
			slog.Int("http_status", hse.StatusCode),
			slog.Int("attempt", attempt+1),
			slog.Duration("retry_in", terminateLockedRetryInterval),
		)
		select {
		case <-ctx.Done():
			return fmt.Errorf("delete instance: upstream lock did not clear before deadline: %w", ctx.Err())
		case <-time.After(terminateLockedRetryInterval):
		}
	}
}

// Terminate destroys an instance by its Vultr id. The region param is
// threaded but does not vary behavior. Idempotent: a 404 is success. Vultr's
// transient "server is locked" refusal is retried across the lock window by
// deleteInstanceRetryingLocked — that retry, not the reconciler's next pass, is
// the primary defence against a stranded billing VM; anything else bubbles.
func (a *Adapter) Terminate(ctx context.Context, upstreamID, region string) error {
	_ = region
	ctx, cancel := contextWithDefaultTimeout(ctx, terminateTimeout)
	defer cancel()

	if err := a.deleteInstanceRetryingLocked(ctx, upstreamID); err != nil {
		var hse *httpStatusError
		if errors.As(err, &hse) && hse.StatusCode == http.StatusNotFound {
			slog.Info("provider instance already gone; terminate is idempotent",
				slog.String("provider", "vultr"),
				slog.String("upstream_id", upstreamID),
			)
			return nil
		}
		return fmt.Errorf("terminate: %w", err)
	}

	slog.Info("terminated provider instance",
		slog.String("provider", "vultr"),
		slog.String("upstream_id", upstreamID),
	)
	return nil
}

// ListProvisioned lists the account's instances for the reconciler's orphan
// detection. Rows are correlated on label (== the env-tagged upstream name
// set at Provision); rows whose label is not ours are skipped (manual VMs,
// other envs). Vultr has no terminated state on live rows — destroyed
// instances vanish from the list — so every correlated row is reported.
func (a *Adapter) ListProvisioned(ctx context.Context) ([]provider.ProvisionedInstance, error) {
	ctx, cancel := contextWithDefaultTimeout(ctx, listTimeout)
	defer cancel()

	var out []provider.ProvisionedInstance
	cursor := ""
	for {
		rows, next, err := a.client.listInstancesPage(ctx, cursor)
		if err != nil {
			return nil, fmt.Errorf("list provisioned: %w", err)
		}
		for _, r := range rows {
			if r.Label == "" || !instanceid.IsOurs(r.Label) {
				continue // manual VM or another env's instance — never ours to reap
			}
			out = append(out, provider.ProvisionedInstance{
				UpstreamID: r.ID,
				Name:       r.Label,
				Region:     r.Region,
				Status:     mapStatus(r.Status, r.PowerStatus, r.ServerStatus),
			})
		}
		if next == "" {
			return out, nil
		}
		cursor = next
	}
}

// ListAllProvisioned returns EVERY instance on the account, unfiltered —
// provider.RawLister. This is what lets the leak sweep see a smoke orphan
// ("smoke-gpu-<epoch>-<rand>"), which ListProvisioned's instanceid.IsOurs filter
// is designed to exclude and which therefore stayed invisible for 44h during the
// 2026-09-06 leak. Read-only: callers apply their own selector before acting.
// The filtered ListProvisioned above is unchanged and remains the reconciler's
// destructive-path input.
func (a *Adapter) ListAllProvisioned(ctx context.Context) ([]provider.ProvisionedInstance, error) {
	ctx, cancel := contextWithDefaultTimeout(ctx, listTimeout)
	defer cancel()

	var out []provider.ProvisionedInstance
	cursor := ""
	for {
		rows, next, err := a.client.listInstancesPage(ctx, cursor)
		if err != nil {
			return nil, fmt.Errorf("list all provisioned: %w", err)
		}
		for _, r := range rows {
			out = append(out, provider.ProvisionedInstance{
				UpstreamID: r.ID,
				Name:       r.Label,
				Region:     r.Region,
				Status:     mapStatus(r.Status, r.PowerStatus, r.ServerStatus),
			})
		}
		if next == "" {
			return out, nil
		}
		cursor = next
	}
}
