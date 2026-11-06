// Package api is the HTTP front door of gpuctl.
//
// It serves two trees from one listener:
//
//   - /v1/*: the public developer API (package internal/api/public), API-key
//     authenticated. Paths and JSON shapes match GPU.ai's hosted API.
//   - /internal/instances/{id}/*: the instance bootstrap callbacks (ready,
//     health, telemetry, staged-bootstrap fetch), authenticated by the
//     per-instance token minted at provision time. They are only exercised
//     when the optional FRP tunnel is enabled, because only then does the
//     engine render a bootstrap script for the instance to run.
//
// Plus GET /livez, an unauthenticated liveness probe for container
// orchestrators.
package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/gpuai-dev/gpuai-oss/internal/api/public"
	"github.com/gpuai-dev/gpuai-oss/internal/db"
	"github.com/redis/go-redis/v9"
)

// ServerDeps are the collaborators NewServer wires into the route tree.
type ServerDeps struct {
	DB     *db.Pool
	Redis  *redis.Client
	Public public.PublicDeps
	// OnStatusChange, when non-nil, is invoked after the ready callback flips
	// an instance to running (mirrors provision.Engine's status hook).
	OnStatusChange func(instanceID, status string)
}

// Server holds the route tree and the handler seams.
type Server struct {
	mux       *http.ServeMux
	publicMux *http.ServeMux
	db        *db.Pool
	redis     *redis.Client

	onStatusChange func(instanceID, status string)

	// Test seams for the internal callback handlers. nil in production, where
	// the handlers fall through to db.
	readyStore     readyCallbackStore
	telemetryStore telemetryStore
	bootstrapStore bootstrapFetchStore
}

// NewServer builds the route tree.
func NewServer(deps ServerDeps) *Server {
	s := &Server{
		mux:            http.NewServeMux(),
		publicMux:      http.NewServeMux(),
		db:             deps.DB,
		redis:          deps.Redis,
		onStatusChange: deps.OnStatusChange,
	}

	public.RegisterPublicRoutes(s.publicMux, deps.Public)

	s.mux.HandleFunc("GET /livez", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	s.mux.HandleFunc("GET /readyz", s.handleReadyz)

	// Instance bootstrap callbacks. Per-instance token auth: a foreign or
	// unknown token is rejected before the handler, so one instance can
	// neither flip nor read another.
	s.mux.Handle("POST /internal/instances/{id}/ready",
		InstanceTokenAuth(deps.DB, http.HandlerFunc(s.handleInstanceReady)))
	s.mux.Handle("POST /internal/instances/{id}/health",
		InstanceTokenAuth(deps.DB, http.HandlerFunc(s.handleInstanceHealth)))
	s.mux.Handle("POST /internal/instances/{id}/telemetry",
		InstanceTokenAuth(deps.DB, http.HandlerFunc(s.handleInstanceTelemetry)))
	s.mux.Handle("GET /internal/instances/{id}/bootstrap",
		InstanceTokenAuth(deps.DB, http.HandlerFunc(s.handleInstanceBootstrap)))

	return s
}

// Handler returns the root HTTP handler: RequestLog -> Recovery -> dispatch.
// /v1/* goes to the public mux, everything else to the internal mux.
func (s *Server) Handler() http.Handler {
	dispatch := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/v1/") {
			s.publicMux.ServeHTTP(w, r)
			return
		}
		s.mux.ServeHTTP(w, r)
	})
	return RequestLogMiddleware(RecoveryMiddleware(dispatch))
}

// readyzResponse is the JSON body returned by GET /readyz.
type readyzResponse struct {
	Status string `json:"status"`
	DB     string `json:"db"`
	Redis  string `json:"redis"`
}

// handleReadyz checks database and Redis connectivity: 200 when both answer,
// 503 otherwise. Unauthenticated; it reveals only connectivity.
func (s *Server) handleReadyz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	resp := readyzResponse{Status: "ok", DB: "connected", Redis: "connected"}
	if s.db == nil || s.db.Ping(ctx) != nil {
		resp.DB = "disconnected"
	}
	if s.redis == nil || s.redis.Ping(ctx).Err() != nil {
		resp.Redis = "disconnected"
	}
	code := http.StatusOK
	if resp.DB != "connected" || resp.Redis != "connected" {
		resp.Status = "degraded"
		code = http.StatusServiceUnavailable
		slog.Warn("readyz degraded", "db", resp.DB, "redis", resp.Redis)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(resp)
}
