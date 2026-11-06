package api

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/gpuai-dev/gpuai-oss/internal/db"
	"github.com/gpuai-dev/gpuai-oss/internal/provision"
)

// readyCallbackStore is the narrow DB seam handleInstanceReady needs: look up
// the instance (to read its TemplateID + status) and atomically flip it to
// running. *db.Pool satisfies it in production; the unit test wires a fake so
// the app_ready gate can be asserted without a live Postgres. The handler's
// instance-event writes stay on the concrete *db.Pool and are nil-guarded (the
// fake leaves s.db nil), because they are side-effects orthogonal to the gate
// this seam exists to test.
type readyCallbackStore interface {
	GetInstance(ctx context.Context, instanceID string) (*db.Instance, error)
	SetInstanceRunning(ctx context.Context, instanceID, upstreamIP string) (updated bool, prevStatus string, err error)
}

// Compile-time guard: the production *db.Pool satisfies the seam, so the
// nil-fallback in readyCallbackBackend is type-safe.
var _ readyCallbackStore = (*db.Pool)(nil)

// readyStore returns the seam handleInstanceReady reads/writes through.
// Defaults to the concrete *db.Pool; the test constructor overrides it.
func (s *Server) readyCallbackBackend() readyCallbackStore {
	if s.readyStore != nil {
		return s.readyStore
	}
	return s.db
}

// readyCallbackBody is the JSON the pod bootstrap POSTs to the ready callback
// (Plan 04). Fields are string-valued ("true"/"false") because cloud-init emits
// them from shell. An empty/malformed body decodes to zero-values: the
// app_ready gate treats "" as "not ready" (conservative for templates), while
// the tunnel_up gate treats "" as "up" (only an explicit "false" defers —
// legacy bootstraps predate the probe and must keep flipping on callback).
type readyCallbackBody struct {
	GPUInfo  string `json:"gpu_info"`
	TunnelUp string `json:"tunnel_up"`
	AppReady string `json:"app_ready"`

	// Phase 63 env report (D-63-07). The certified-image bootstrap posts its
	// observed environment alongside the readiness signals. All string-valued
	// (cloud-init emits them from shell). Absent on legacy/raw bootstraps.
	DriverVersion        string `json:"driver_version"`
	CUDAVersion          string `json:"cuda_version"`
	CertifiedImageDigest string `json:"certified_image_digest"`
	ContainerID          string `json:"container_id"`
	DockerUp             string `json:"docker_up"`

	// Startup GPU compute check (bootstrap section 8b). Absent on bootstraps
	// rendered with the check off; in shadow mode they arrive after the ready
	// callback on a report-only POST. gpu_check is ok | fail | warn | skipped
	// (warn: a framework the environment did not promise cannot use the GPU,
	// reported but never blocking); gpu_check_mode is shadow | enforce.
	GPUCheck       string `json:"gpu_check"`
	GPUCheckDetail string `json:"gpu_check_detail"`
	GPUCheckMode   string `json:"gpu_check_mode"`
	GPUsSeen       string `json:"gpus_seen"`

	// ReportOnly ("true") marks a POST that only carries diagnostics for the
	// ready report: the shadow-mode GPU check runs after the ready callback
	// (so it adds no latency to readiness) and delivers its result this way.
	// It is recorded and acknowledged, and never records a readiness verdict
	// or touches status, whatever else it says.
	ReportOnly string `json:"report_only"`
}

// readinessFailureRecorder is the seam for an enforce-mode compute check
// failure: the handler records the verdict on the still-pre-running row and the
// provisioning poll loop (possibly on another gpuctl task) acts on it.
type readinessFailureRecorder interface {
	SetInstanceReadinessFailure(ctx context.Context, instanceID, reason string) (bool, error)
}

// The production pool must implement the seam. The ready-report hook shipped
// implemented only on a test fake; this assertion keeps that from recurring.
var _ readinessFailureRecorder = (*db.Pool)(nil)

// checkDetailMaxLen bounds the compute-check reason. It is longer than the env
// fields because it carries a sentence ("host: cuInit returned 999, ...").
const checkDetailMaxLen = 200

// sanitizeCheckDetail keeps the reason readable (spaces and punctuation the
// bootstrap emits) while dropping anything that could break a log line or a
// column. Like the env fields it is bootstrap-reported, so untrusted.
func sanitizeCheckDetail(v string) string {
	if len(v) > checkDetailMaxLen {
		v = v[:checkDetailMaxLen]
	}
	return strings.TrimSpace(strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			return r
		case strings.ContainsRune(" .,:;=/()+-_", r):
			return r
		default:
			return -1
		}
	}, v))
}

// readyReportRecorder is the optional Phase 63 seam a ready store may implement
// to persist the bootstrap env report (driver/cuda/digest/docker_up). It is
// deliberately separate from readyCallbackStore: the report is telemetry that
// must never gate or fail the callback, so handleInstanceReady only records it
// when the backing store opts in via a type assertion.
type readyReportRecorder interface {
	RecordReadyReport(ctx context.Context, instanceID string, report map[string]string)
}

// Keep the production hook wired; a fake-only implementation must not silently pass tests.
var _ readyReportRecorder = (*db.Pool)(nil)

// envReportMaxLen bounds each reported field before it is persisted/logged, so
// a forged bootstrap POST cannot blow up a log line or a column. Values are
// server-adjacent telemetry, not customer input, but the callback token is not
// a strong integrity guarantee, so treat them as untrusted at the boundary.
const envReportMaxLen = 128

// sanitizeEnvField trims a reported env value to a safe length + charset. It
// keeps dotted/alnum version and digest text (e.g. "570.86.10", "12.8",
// "sha256:0123…") and drops anything that could break a log/JSON line.
func sanitizeEnvField(v string) string {
	if len(v) > envReportMaxLen {
		v = v[:envReportMaxLen]
	}
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			return r
		case r == '.' || r == ':' || r == '-' || r == '_' || r == '/' || r == '@':
			return r
		default:
			return -1
		}
	}, v)
}

// newReadyHandlerForTest builds a minimal *Server whose ready handler reads/
// writes through the supplied seam. s.db is intentionally left nil so the
// event side-effects exercise their nil-guards.
func newReadyHandlerForTest(store readyCallbackStore) *Server {
	return &Server{readyStore: store}
}

// telemetryStore is the narrow DB seam handleInstanceTelemetry needs: append
// one nvidia-smi sample row keyed on the per-instance id. *db.Pool satisfies it
// in production; the unit test wires a fake so the ingest can be asserted
// without a live Postgres. Mirrors readyCallbackStore.
type telemetryStore interface {
	InsertGPUTelemetry(ctx context.Context, instanceID string, s db.GPUTelemetrySample) error
}

// Compile-time guard: the production *db.Pool satisfies the seam.
var _ telemetryStore = (*db.Pool)(nil)

// telemetryBackend returns the seam handleInstanceTelemetry writes through.
// Defaults to the concrete *db.Pool; the test constructor overrides it.
func (s *Server) telemetryBackend() telemetryStore {
	if s.telemetryStore != nil {
		return s.telemetryStore
	}
	return s.db
}

// telemetrySampleBody is the JSON the in-pod sampler POSTs to the telemetry
// callback (Plan 49-01). The single string field carries one nvidia-smi
// `--format=csv,noheader,nounits` line (e.g. "98, 1024, 81920, 250"), mirroring
// readyCallbackBody's string-valued, shell-emitted shape. An empty/malformed
// body decodes to Sample == "", which ParseTelemetrySample turns into an
// all-NULL row rather than failing the ingest.
type telemetrySampleBody struct {
	Sample string `json:"sample"`
}

// newTelemetryHandlerForTest builds a minimal *Server whose telemetry handler
// writes through the supplied seam (s.db left nil). Mirrors
// newReadyHandlerForTest.
func newTelemetryHandlerForTest(store telemetryStore) *Server {
	return &Server{telemetryStore: store}
}

// handleInstanceTelemetry handles POST /internal/instances/{id}/telemetry.
// Called on a recurring 1-5 min cadence by the in-pod nvidia-smi sampler baked
// into the FRP bootstrap (TELEM-01). Stores one append-only sample row for the
// instance and returns 204.
//
// The per-instance token is already verified by InstanceTokenAuth middleware,
// so a foreign/unknown token is rejected before this handler runs — one
// instance cannot write another's telemetry (T-49-01). Storage only: NO
// auto-termination or mutating action is wired off telemetry (RESEARCH Pitfall
// 6 — the cross-env reaper; any kill is the env-asserted RECON-03 path).
func (s *Server) handleInstanceTelemetry(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	store := s.telemetryBackend()

	// 1. Extract instance ID from path (token already verified by middleware).
	instanceID := r.PathValue("id")
	if instanceID == "" {
		writeProblem(w, http.StatusBadRequest, "missing-id", "Instance ID is required")
		return
	}

	// 2. Decode the body tolerantly — an empty/malformed body leaves Sample ""
	// (mirrors readyCallback). A bad body must not 500; ParseTelemetrySample
	// turns "" into an all-NULL row.
	var body telemetrySampleBody
	_ = json.NewDecoder(r.Body).Decode(&body)

	// 3. Parse the nvidia-smi CSV line and append the row.
	sample := db.ParseTelemetrySample(body.Sample)
	if err := store.InsertGPUTelemetry(ctx, instanceID, sample); err != nil {
		slog.Error("failed to insert gpu telemetry",
			slog.String("instance_id", instanceID),
			slog.String("error", err.Error()),
		)
		writeProblem(w, http.StatusInternalServerError, "internal-error",
			"Failed to store telemetry")
		return
	}

	// 4. 204 No Content — the sampler ignores the body.
	w.WriteHeader(http.StatusNoContent)
}

// handleInstanceReady handles POST /internal/instances/{id}/ready.
// Called by cloud-init when an instance boots successfully.
// Transitions booting -> running and publishes SSE event.
//
// This callback is the SOLE authority for the running flip (the engine poll
// loop only advances to booting), gated on two signals:
//
//   - RAWREADY (all instances): tunnel_up=="false" defers — "running" is the
//     customer's cue to SSH in, so it must not precede the FRP tunnel being
//     registered. The bootstrap derives tunnel_up by probing its own public
//     SSH port on the gateway and keeps re-POSTing in the background until it
//     opens. An absent tunnel_up (legacy bootstrap, malformed body) does NOT
//     defer.
//   - TMPL-10 (service templates, inst.TemplateID != nil): app_ready must be
//     "true" — flipping before the app listens 502s the deploy URL. A
//     false/unknown app_ready returns 200 WITHOUT transitioning so a later
//     callback flips it.
//
// For a raw instance left in booting by a tunnel_up=false defer, the engine
// poll loop's provisioning timeout is the backstop (error + upstream
// terminate); for templates the bootstrap's bounded re-POST loop is.
func (s *Server) handleInstanceReady(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	store := s.readyCallbackBackend()

	// 1. Extract instance ID from path.
	instanceID := r.PathValue("id")
	if instanceID == "" {
		writeProblem(w, http.StatusBadRequest, "missing-id", "Instance ID is required")
		return
	}

	// 1b. Decode the callback body. Tolerant: an empty/malformed body leaves
	// zero-values (AppReady == ""), which the gate below treats as "app not
	// ready". The error is intentionally ignored so a bootstrap that omits the
	// body (older image) still reaches the raw-instance transition path.
	var body readyCallbackBody
	_ = json.NewDecoder(r.Body).Decode(&body)

	// 1c. Env report (D-63-07). Persist the bootstrap's observed environment
	// (driver/cuda/certified-image digest/docker_up) as telemetry. This is
	// recorded BEFORE the readiness gates so it lands even on a deferred
	// (tunnel_up=false / app not ready) callback, and it NEVER fails the
	// callback — a bad/absent report is silently skipped. Sanitized at the
	// boundary (bounded length + safe charset). Only fields the bootstrap
	// actually set are recorded.
	report := map[string]string{}
	for k, v := range map[string]string{
		"driver_version":         body.DriverVersion,
		"cuda_version":           body.CUDAVersion,
		"certified_image_digest": body.CertifiedImageDigest,
		"container_id":           body.ContainerID,
		"docker_up":              body.DockerUp,
	} {
		if v == "" {
			continue
		}
		report[k] = sanitizeEnvField(v)
	}
	if body.GPUCheck != "" {
		report["gpu_check"] = sanitizeEnvField(body.GPUCheck)
		report["gpu_check_mode"] = sanitizeEnvField(body.GPUCheckMode)
		report["gpus_seen"] = sanitizeEnvField(body.GPUsSeen)
		if d := sanitizeCheckDetail(body.GPUCheckDetail); d != "" {
			report["gpu_check_detail"] = d
		}
	}
	if len(report) > 0 {
		slog.Info("instance env report",
			slog.String("instance_id", instanceID),
			slog.String("driver_version", report["driver_version"]),
			slog.String("cuda_version", report["cuda_version"]),
			slog.String("certified_image_digest", report["certified_image_digest"]),
			slog.String("docker_up", report["docker_up"]),
		)
		if rec, ok := store.(readyReportRecorder); ok {
			rec.RecordReadyReport(ctx, instanceID, report)
		}
	}

	// 1c'. A report-only POST (the shadow-mode GPU check's late result) ends
	// here: its report is recorded above, and it must never change status or
	// record a verdict, so it cannot alter readiness timing. The
	// bootstrap also sends tunnel_up=false with it, so a server that predates
	// this field defers at 2a instead of flipping the instance.
	if body.ReportOnly == "true" {
		writeJSON(w, http.StatusOK, map[string]string{"status": "recorded"})
		return
	}

	// 1d. Startup GPU compute check, enforce mode: the bootstrap refused to
	// publish this host (CUDA did not run, or the promised framework could not
	// use the GPU) and exited. Record the verdict for the provisioning poll
	// loop, which releases the host and rescues the launch before it ever
	// starts, and never flip to running here. A shadow-mode result arrives
	// after the ready callback on a report-only POST and only lands in the
	// report above.
	if body.GPUCheck == "fail" && body.GPUCheckMode == "enforce" {
		reason := sanitizeCheckDetail(body.GPUCheckDetail)
		if reason == "" {
			reason = "startup GPU compute check failed"
		}
		slog.Warn("instance failed its startup GPU compute check",
			slog.String("instance_id", instanceID),
			slog.String("reason", reason),
			slog.String("gpus_seen", sanitizeEnvField(body.GPUsSeen)),
		)
		if rec, ok := store.(readinessFailureRecorder); ok {
			if _, err := rec.SetInstanceReadinessFailure(ctx, instanceID, reason); err != nil {
				// Not recorded means the poll loop cannot rescue early; the
				// instance stays pre-running until the provisioning timeout,
				// which still never bills it.
				slog.Error("failed to record readiness failure",
					slog.String("instance_id", instanceID),
					slog.String("error", err.Error()),
				)
			}
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
		return
	}

	// 2. Look up instance (needed for status check in transition).
	// Token already verified by InstanceTokenAuth middleware.
	inst, err := store.GetInstance(ctx, instanceID)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			writeProblem(w, http.StatusNotFound, "not-found", "Instance not found")
			return
		}
		slog.Error("failed to get instance for ready callback",
			slog.String("instance_id", instanceID),
			slog.String("error", err.Error()),
		)
		writeProblem(w, http.StatusInternalServerError, "internal-error",
			"Failed to process callback")
		return
	}

	// 2a. Tunnel-readiness gate (RAWREADY) — applies to raw AND template
	// instances. An explicit tunnel_up=="false" means the bootstrap probed
	// this instance's public SSH port on the FRP gateway and it was not open
	// yet (frpc not registered). Defer with an idempotent 200: the bootstrap
	// re-probes in the background and re-POSTs once the port opens; if it
	// never does, the engine's provisioning timeout errors the instance and
	// terminates the upstream. Empty/missing tunnel_up is permissive so
	// legacy bootstraps (rendered before the probe existed) and malformed
	// bodies keep today's flip-on-callback behavior instead of stranding the
	// instance in booting.
	if body.TunnelUp == "false" {
		slog.Info("instance ready callback received, tunnel not registered yet",
			slog.String("instance_id", instanceID),
			slog.String("app_ready", body.AppReady),
		)
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
		return
	}

	// 2b. App-readiness gate (TMPL-10). For a service template the SSH-IP
	// signal is app-agnostic — flipping to running before the app is listening
	// 502s the deploy URL. Defer the transition until the bootstrap reports
	// app_ready=="true". A false/unknown app_ready on a template is an
	// idempotent no-op 200 (the bootstrap re-POSTs, or the readiness poll
	// re-fires). Raw instances (TemplateID nil) are NOT gated.
	isTemplate := inst.TemplateID != nil
	appReady := body.AppReady == "true"
	if isTemplate && !appReady {
		slog.Info("template instance ready callback received, app not ready yet",
			slog.String("instance_id", instanceID),
			slog.String("app_ready", body.AppReady),
		)
		// No transition happened — skip the status-change branch and
		// return 200 so the callback is safely retryable.
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
		return
	}

	// 3. Atomically transition {creating, provisioning, booting} -> running.
	// SetInstanceRunning returns updated=true for exactly one caller (the
	// atomic transition), so the side effects below run once even if the
	// bootstrap re-POSTs the callback.
	updated, prevStatus, err := store.SetInstanceRunning(ctx, instanceID, "")
	if err != nil {
		slog.Error("failed to set instance running",
			slog.String("instance_id", instanceID),
			slog.String("error", err.Error()),
		)
		writeProblem(w, http.StatusInternalServerError, "internal-error",
			"Failed to update instance status")
		return
	}

	// 4. If successfully transitioned, publish SSE event.
	if updated {
		slog.Info("instance ready",
			slog.String("instance_id", instanceID),
			slog.String("prev_status", prevStatus),
		)
		if s.onStatusChange != nil {
			s.onStatusChange(instanceID, provision.StateRunning)
		}

		// The staged bootstrap has served its purpose once the instance is
		// running — delete it so the fetch endpoint stops serving this
		// instance's tunnel/callback tokens. Best-effort: the 24h sweep in
		// StageInstanceBootstrap is the backstop. Guarded for the seam unit
		// test (s.db nil).
		if s.db != nil {
			if err := s.db.DeleteStagedBootstrap(ctx, instanceID); err != nil {
				slog.Warn("failed to delete staged bootstrap",
					slog.String("instance_id", instanceID),
					slog.String("error", err.Error()),
				)
			}
		}

		// Log ready event to instance_events table. Guarded for the seam unit
		// test (s.db nil); always present in production.
		if s.db != nil {
			metadata, _ := json.Marshal(map[string]string{
				"gpu_type": inst.GPUType,
				"region":   inst.Region,
			})
			readyEvent := &db.InstanceEvent{
				InstanceID: instanceID,
				OrgID:      inst.OrgID,
				EventType:  "ready",
				Metadata:   metadata,
			}
			if err := s.db.CreateInstanceEvent(ctx, readyEvent); err != nil {
				slog.Error("failed to log ready event",
					slog.String("instance_id", instanceID),
					slog.String("error", err.Error()),
				)
				// Non-fatal: instance is running, event logging failure doesn't block.
			}
		}
	} else {
		// Terminal/post-running state — idempotent, log warning.
		slog.Warn("instance ready callback ignored (instance not in startable state)",
			slog.String("instance_id", instanceID),
			slog.String("current_status", inst.Status),
		)
	}

	// 5. Return 200 OK regardless (idempotent).
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// handleInstanceHealth handles POST /internal/instances/{id}/health.
// Called by instance health pings. Logs the health ping for Phase 6 monitoring.
func (s *Server) handleInstanceHealth(w http.ResponseWriter, r *http.Request) {
	// 1. Extract instance ID from path.
	instanceID := r.PathValue("id")
	if instanceID == "" {
		writeProblem(w, http.StatusBadRequest, "missing-id", "Instance ID is required")
		return
	}

	// 2. Verify instance exists.
	// Token already verified by InstanceTokenAuth middleware.
	_, err := s.db.GetInstance(r.Context(), instanceID)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			writeProblem(w, http.StatusNotFound, "not-found", "Instance not found")
			return
		}
		slog.Error("failed to get instance for health ping",
			slog.String("instance_id", instanceID),
			slog.String("error", err.Error()),
		)
		writeProblem(w, http.StatusInternalServerError, "internal-error",
			"Failed to process health ping")
		return
	}

	// 3. Log the health ping (full last_seen update deferred to Phase 6).
	slog.Debug("health ping received",
		slog.String("instance_id", instanceID),
	)

	// 4. Return 200 OK.
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// bootstrapFetchStore is the narrow DB seam handleInstanceBootstrap needs:
// read the bootstrap staged for an instance at provision time. *db.Pool
// satisfies it in production; unit tests wire a fake. Mirrors telemetryStore.
type bootstrapFetchStore interface {
	GetStagedBootstrap(ctx context.Context, instanceID string) (string, error)
}

// Compile-time guard: the production *db.Pool satisfies the seam.
var _ bootstrapFetchStore = (*db.Pool)(nil)

// bootstrapBackend returns the seam handleInstanceBootstrap reads through.
// Defaults to the concrete *db.Pool; the test constructor overrides it.
func (s *Server) bootstrapBackend() bootstrapFetchStore {
	if s.bootstrapStore != nil {
		return s.bootstrapStore
	}
	return s.db
}

// handleInstanceBootstrap handles GET /internal/instances/{id}/bootstrap.
// Called at boot by a stage-0 loader on providers that cannot carry the full
// per-instance bootstrap in the launch itself (for example a provider with a
// small stored-script quota): such launches send only a static loader plus
// this URL and the instance's bearer token.
//
// The per-instance token is already verified by InstanceTokenAuth, so a
// foreign/unknown token is rejected before this handler runs — one instance
// cannot fetch another's bootstrap (which embeds its FRP + callback tokens).
// The staged row is deleted when the instance reaches running and swept after
// 24h, so this serves 404 once the instance is past boot.
func (s *Server) handleInstanceBootstrap(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	instanceID := r.PathValue("id")
	if instanceID == "" {
		writeProblem(w, http.StatusBadRequest, "missing-id", "Instance ID is required")
		return
	}

	script, err := s.bootstrapBackend().GetStagedBootstrap(ctx, instanceID)
	if errors.Is(err, db.ErrNotFound) {
		writeProblem(w, http.StatusNotFound, "not-found", "No bootstrap staged for this instance")
		return
	}
	if err != nil {
		slog.Error("failed to read staged bootstrap",
			slog.String("instance_id", instanceID),
			slog.String("error", err.Error()),
		)
		writeProblem(w, http.StatusInternalServerError, "internal-error",
			"Failed to read bootstrap")
		return
	}

	slog.Info("served staged bootstrap", slog.String("instance_id", instanceID))
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte(script))
}

// newBootstrapHandlerForTest builds a minimal *Server whose bootstrap handler
// reads through the supplied seam (s.db left nil). Mirrors
// newTelemetryHandlerForTest.
func newBootstrapHandlerForTest(store bootstrapFetchStore) *Server {
	return &Server{bootstrapStore: store}
}
