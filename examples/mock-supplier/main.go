// Command mock-supplier is a reference implementation of the Generic Supplier
// API (docs/suppliers.md): the four endpoints gpuctl's generic adapter calls
// to list offerings, create, inspect and delete instances.
//
// It advertises a small set of fake GPU offerings. There are no GPUs behind
// them: an "instance" is a container started on the local Docker daemon from a
// tiny sshd image, so the whole provision, SSH, terminate lifecycle is real and
// can be exercised on a laptop. Without Docker (MOCK_RUNTIME=simulated) the
// lifecycle states are still real, but nothing listens for SSH.
//
// Endpoints (all JSON):
//
//	GET    /v1/gpu/available      offerings
//	POST   /v1/instances          create; returns upstream_id
//	GET    /v1/instances/{id}     status: creating | running | terminated | error
//	DELETE /v1/instances/{id}     terminate (idempotent)
//	GET    /healthz               liveness
//	GET    /mock/instances        every instance this process knows about,
//	                              including terminated ones (for tests)
//
// Configuration (environment):
//
//	MOCK_LISTEN            listen address (default ":8080")
//	MOCK_API_KEY           when set, every /v1 call must send
//	                       "Authorization: Bearer <MOCK_API_KEY>"
//	MOCK_RUNTIME           "docker" (default when the Docker socket exists)
//	                       or "simulated"
//	MOCK_DOCKER_HOST       Docker Engine socket (default /var/run/docker.sock)
//	MOCK_INSTANCE_IMAGE    image run per instance (default
//	                       gpuai-oss-mock-instance:latest, built from
//	                       examples/mock-supplier/instance)
//	MOCK_DOCKER_NETWORK    network to attach instance containers to; the mock
//	                       probes sshd over it before reporting running
//	MOCK_PUBLIC_IP         IP address customers use to reach published SSH
//	                       ports (default 127.0.0.1)
//	MOCK_PUBLISH_IP        host interface SSH ports are published on
//	                       (default 127.0.0.1)
//	MOCK_OFFERINGS         JSON array overriding the advertised offerings
//	MOCK_READY_DELAY       extra delay before a simulated instance reports
//	                       running (default 3s)
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"
)

// offering is one entry of GET /v1/gpu/available.
type offering struct {
	GPUType            string  `json:"gpu_type"`
	GPUCount           int     `json:"gpu_count"`
	VRAMPerGPUGB       int     `json:"vram_per_gpu_gb"`
	CPUCores           int     `json:"cpu_cores"`
	RAMGB              int     `json:"ram_gb"`
	StorageGB          int     `json:"storage_gb"`
	PricePerHour       float64 `json:"price_per_hour"`
	Tier               string  `json:"tier"`
	Region             string  `json:"region"`
	DatacenterLocation string  `json:"datacenter_location"`
	StockStatus        string  `json:"stock_status"`
	AvailableCount     int     `json:"available_count"`
}

var defaultOfferings = []offering{
	{GPUType: "rtx_4090", GPUCount: 1, VRAMPerGPUGB: 24, CPUCores: 8, RAMGB: 32, StorageGB: 200,
		PricePerHour: 0.35, Tier: "on_demand", Region: "us-east", DatacenterLocation: "Mock DC 1",
		StockStatus: "available", AvailableCount: 4},
	{GPUType: "a100_80gb", GPUCount: 1, VRAMPerGPUGB: 80, CPUCores: 16, RAMGB: 120, StorageGB: 500,
		PricePerHour: 1.10, Tier: "on_demand", Region: "us-east", DatacenterLocation: "Mock DC 1",
		StockStatus: "available", AvailableCount: 2},
	{GPUType: "a100_80gb", GPUCount: 8, VRAMPerGPUGB: 80, CPUCores: 128, RAMGB: 960, StorageGB: 2000,
		PricePerHour: 8.80, Tier: "on_demand", Region: "us-west", DatacenterLocation: "Mock DC 2",
		StockStatus: "low", AvailableCount: 1},
}

// provisionRequest is the body of POST /v1/instances.
type provisionRequest struct {
	InstanceID    string   `json:"instance_id"`
	GPUType       string   `json:"gpu_type"`
	GPUCount      int      `json:"gpu_count"`
	Tier          string   `json:"tier"`
	Region        string   `json:"region,omitempty"`
	SSHPublicKeys []string `json:"ssh_public_keys,omitempty"`
	DockerImage   string   `json:"docker_image,omitempty"`
	StartupScript string   `json:"startup_script,omitempty"`
}

// instance is the mock's record of one created instance.
type instance struct {
	UpstreamID  string    `json:"upstream_id"`
	InstanceID  string    `json:"instance_id"`
	GPUType     string    `json:"gpu_type"`
	GPUCount    int       `json:"gpu_count"`
	Region      string    `json:"region"`
	CostPerHour float64   `json:"cost_per_hour"`
	Status      string    `json:"status"`
	IP          string    `json:"ip,omitempty"`
	SSHPort     int       `json:"ssh_port,omitempty"`
	ContainerID string    `json:"container_id,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
	RunningAt   time.Time `json:"running_at,omitempty"`
	DeletedAt   time.Time `json:"deleted_at,omitempty"`
	Error       string    `json:"error,omitempty"`
}

type server struct {
	apiKey     string
	offerings  []offering
	runtime    runtime
	readyDelay time.Duration
	logger     *slog.Logger

	mu        sync.Mutex
	instances map[string]*instance
}

// runtime starts and stops the thing behind an instance.
type runtime interface {
	// Start launches the instance and returns its container id (may be "").
	Start(ctx context.Context, inst *instance, req provisionRequest) (string, error)
	// Probe reports the instance's reachable address once it is ready.
	Probe(ctx context.Context, inst *instance) (ip string, sshPort int, ready bool, err error)
	// Stop tears the instance down. Must be idempotent.
	Stop(ctx context.Context, inst *instance) error
}

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	offs := defaultOfferings
	if raw := strings.TrimSpace(os.Getenv("MOCK_OFFERINGS")); raw != "" {
		if err := json.Unmarshal([]byte(raw), &offs); err != nil {
			logger.Error("MOCK_OFFERINGS is not a JSON array of offerings", "error", err)
			os.Exit(1)
		}
	}

	readyDelay := 3 * time.Second
	if raw := os.Getenv("MOCK_READY_DELAY"); raw != "" {
		d, err := time.ParseDuration(raw)
		if err != nil {
			logger.Error("invalid MOCK_READY_DELAY", "error", err)
			os.Exit(1)
		}
		readyDelay = d
	}

	rt, err := newRuntime(logger, readyDelay)
	if err != nil {
		logger.Error("runtime setup failed", "error", err)
		os.Exit(1)
	}

	s := &server{
		apiKey:     os.Getenv("MOCK_API_KEY"),
		offerings:  offs,
		runtime:    rt,
		readyDelay: readyDelay,
		logger:     logger,
		instances:  map[string]*instance{},
	}

	addr := os.Getenv("MOCK_LISTEN")
	if addr == "" {
		addr = ":8080"
	}
	httpSrv := &http.Server{Addr: addr, Handler: s.routes(), ReadHeaderTimeout: 10 * time.Second}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go func() {
		logger.Info("mock supplier listening", "addr", addr, "offerings", len(offs), "auth", s.apiKey != "")
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("listen failed", "error", err)
			os.Exit(1)
		}
	}()
	<-ctx.Done()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = httpSrv.Shutdown(shutdownCtx)
}

func newRuntime(logger *slog.Logger, readyDelay time.Duration) (runtime, error) {
	mode := strings.ToLower(strings.TrimSpace(os.Getenv("MOCK_RUNTIME")))
	sock := os.Getenv("MOCK_DOCKER_HOST")
	if sock == "" {
		sock = "/var/run/docker.sock"
	}
	sock = strings.TrimPrefix(sock, "unix://")
	if mode == "" {
		if _, err := os.Stat(sock); err == nil {
			mode = "docker"
		} else {
			mode = "simulated"
		}
	}
	publicIP := envOr("MOCK_PUBLIC_IP", "127.0.0.1")
	if net.ParseIP(publicIP) == nil {
		return nil, fmt.Errorf("MOCK_PUBLIC_IP must be an IP address, got %q", publicIP)
	}
	switch mode {
	case "docker":
		logger.Info("runtime: docker", "socket", sock)
		return &dockerRuntime{
			client:      newDockerClient(sock),
			image:       envOr("MOCK_INSTANCE_IMAGE", "gpuai-oss-mock-instance:latest"),
			network:     os.Getenv("MOCK_DOCKER_NETWORK"),
			publicIP:    publicIP,
			publishIP:   envOr("MOCK_PUBLISH_IP", "127.0.0.1"),
			logger:      logger,
			probeDialer: &net.Dialer{Timeout: 2 * time.Second},
		}, nil
	case "simulated":
		logger.Info("runtime: simulated (no containers; nothing listens for SSH)")
		return &simulatedRuntime{publicIP: publicIP, readyDelay: readyDelay}, nil
	default:
		return nil, fmt.Errorf("MOCK_RUNTIME must be docker or simulated, got %q", mode)
	}
}

func (s *server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("GET /mock/instances", s.handleDebugList)
	mux.Handle("GET /v1/gpu/available", s.auth(http.HandlerFunc(s.handleAvailable)))
	mux.Handle("POST /v1/instances", s.auth(http.HandlerFunc(s.handleCreate)))
	mux.Handle("GET /v1/instances/{id}", s.auth(http.HandlerFunc(s.handleStatus)))
	mux.Handle("DELETE /v1/instances/{id}", s.auth(http.HandlerFunc(s.handleDelete)))
	return logRequests(s.logger, mux)
}

func (s *server) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.apiKey != "" && r.Header.Get("Authorization") != "Bearer "+s.apiKey {
			writeError(w, http.StatusUnauthorized, "unauthorized", "missing or invalid bearer token")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *server) handleAvailable(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	inUse := map[string]int{}
	for _, inst := range s.instances {
		if inst.Status == "creating" || inst.Status == "running" {
			inUse[offeringKey(inst.GPUType, inst.GPUCount, inst.Region)]++
		}
	}
	s.mu.Unlock()

	out := make([]offering, 0, len(s.offerings))
	for _, o := range s.offerings {
		left := o.AvailableCount - inUse[offeringKey(o.GPUType, o.GPUCount, o.Region)]
		if left <= 0 {
			continue
		}
		o.AvailableCount = left
		out = append(out, o)
	}
	writeJSON(w, http.StatusOK, map[string]any{"offerings": out})
}

func (s *server) handleCreate(w http.ResponseWriter, r *http.Request) {
	var req provisionRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid JSON body: "+err.Error())
		return
	}
	if req.GPUType == "" || req.GPUCount <= 0 {
		writeError(w, http.StatusBadRequest, "bad_request", "gpu_type and gpu_count are required")
		return
	}
	off, ok := s.match(req)
	if !ok {
		// The generic adapter maps this phrase to "no capacity" and tries
		// the next candidate.
		writeError(w, http.StatusConflict, "no_capacity", "no capacity for the requested offering")
		return
	}

	inst := &instance{
		UpstreamID:  "mock-" + randomHex(6),
		InstanceID:  req.InstanceID,
		GPUType:     off.GPUType,
		GPUCount:    off.GPUCount,
		Region:      off.Region,
		CostPerHour: off.PricePerHour,
		Status:      "creating",
		CreatedAt:   time.Now().UTC(),
	}
	cid, err := s.runtime.Start(r.Context(), inst, req)
	if err != nil {
		s.logger.Error("instance start failed", "upstream_id", inst.UpstreamID, "error", err)
		writeError(w, http.StatusInternalServerError, "start_failed", "could not start the instance")
		return
	}
	inst.ContainerID = cid
	s.mu.Lock()
	s.instances[inst.UpstreamID] = inst
	s.mu.Unlock()
	s.logger.Info("instance created", "upstream_id", inst.UpstreamID, "instance_id", inst.InstanceID,
		"gpu_type", inst.GPUType, "gpu_count", inst.GPUCount, "container_id", shortID(cid))

	writeJSON(w, http.StatusCreated, map[string]any{
		"upstream_id":             inst.UpstreamID,
		"status":                  inst.Status,
		"cost_per_hour":           inst.CostPerHour,
		"estimated_ready_seconds": 15,
		"datacenter_location":     off.DatacenterLocation,
		"region":                  off.Region,
	})
}

// match finds the advertised offering a create request asks for, honouring
// remaining stock.
func (s *server) match(req provisionRequest) (offering, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, o := range s.offerings {
		if o.GPUType != req.GPUType || o.GPUCount != req.GPUCount {
			continue
		}
		if req.Tier != "" && o.Tier != req.Tier {
			continue
		}
		if req.Region != "" && o.Region != req.Region {
			continue
		}
		used := 0
		for _, inst := range s.instances {
			if (inst.Status == "creating" || inst.Status == "running") &&
				offeringKey(inst.GPUType, inst.GPUCount, inst.Region) == offeringKey(o.GPUType, o.GPUCount, o.Region) {
				used++
			}
		}
		if used < o.AvailableCount {
			return o, true
		}
	}
	return offering{}, false
}

func (s *server) handleStatus(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	s.mu.Lock()
	inst, ok := s.instances[id]
	s.mu.Unlock()
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "instance not found")
		return
	}

	if inst.Status == "creating" {
		ip, port, ready, err := s.runtime.Probe(r.Context(), inst)
		s.mu.Lock()
		switch {
		case err != nil:
			inst.Status = "error"
			inst.Error = err.Error()
			s.logger.Warn("instance failed", "upstream_id", id, "error", err)
		case ready:
			inst.Status = "running"
			inst.IP = ip
			inst.SSHPort = port
			inst.RunningAt = time.Now().UTC()
			s.logger.Info("instance running", "upstream_id", id, "ip", ip, "ssh_port", port)
		}
		s.mu.Unlock()
	}

	s.mu.Lock()
	resp := map[string]any{
		"upstream_id":   inst.UpstreamID,
		"status":        inst.Status,
		"cost_per_hour": inst.CostPerHour,
	}
	if inst.Status == "running" {
		resp["ip"] = inst.IP
		if inst.SSHPort > 0 {
			resp["ssh_port"] = inst.SSHPort
		}
		resp["uptime_seconds"] = int(time.Since(inst.RunningAt).Seconds())
	}
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, resp)
}

func (s *server) handleDelete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	s.mu.Lock()
	inst, ok := s.instances[id]
	s.mu.Unlock()
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "instance not found")
		return
	}
	if inst.Status != "terminated" {
		if err := s.runtime.Stop(r.Context(), inst); err != nil {
			s.logger.Error("instance teardown failed", "upstream_id", id, "error", err)
			writeError(w, http.StatusInternalServerError, "teardown_failed", "could not tear the instance down")
			return
		}
		s.mu.Lock()
		inst.Status = "terminated"
		inst.DeletedAt = time.Now().UTC()
		s.mu.Unlock()
		s.logger.Info("instance terminated: teardown confirmed", "upstream_id", id,
			"instance_id", inst.InstanceID, "container_id", shortID(inst.ContainerID))
	}
	writeJSON(w, http.StatusOK, map[string]string{"upstream_id": id, "status": "terminated"})
}

func (s *server) handleDebugList(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	out := make([]instance, 0, len(s.instances))
	for _, inst := range s.instances {
		out = append(out, *inst)
	}
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"instances": out})
}

// simulatedRuntime has no containers: an instance reports running after
// readyDelay at publicIP:22, where nothing listens.
type simulatedRuntime struct {
	publicIP   string
	readyDelay time.Duration
}

func (s *simulatedRuntime) Start(_ context.Context, _ *instance, _ provisionRequest) (string, error) {
	return "", nil
}

func (s *simulatedRuntime) Probe(_ context.Context, inst *instance) (string, int, bool, error) {
	if time.Since(inst.CreatedAt) < s.readyDelay {
		return "", 0, false, nil
	}
	return s.publicIP, 22, true, nil
}

func (s *simulatedRuntime) Stop(_ context.Context, _ *instance) error { return nil }

func offeringKey(gpuType string, count int, region string) string {
	return fmt.Sprintf("%s|%d|%s", gpuType, count, region)
}

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%x", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}

func shortID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]string{"error": code, "message": msg})
}

func logRequests(logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		if r.URL.Path != "/healthz" {
			logger.Debug("request", "method", r.Method, "path", r.URL.Path, "duration", time.Since(start))
		}
	})
}
