package tunnel

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/fatedier/frp/pkg/config/types"
	v1 "github.com/fatedier/frp/pkg/config/v1"
	frpserver "github.com/fatedier/frp/server"
)

// Manager wraps the embedded frps (FRP server) lifecycle.
// It is created once at startup and runs for the lifetime of the process.
type Manager struct {
	frpSvc *frpserver.Service
	logger *slog.Logger
}

// NewManager creates a new FRP tunnel manager with the given configuration.
// bindPort is the port frps listens on for frpc client connections (e.g., 7000).
// token is the shared auth token for frpc authentication (can be empty for dev).
// allowPorts is the allowed remote port range string (e.g., "10000-10255").
// logger is optional; a default logger is used if nil.
func NewManager(bindPort int, token string, allowPorts string, logger *slog.Logger) (*Manager, error) {
	if bindPort <= 0 {
		return nil, fmt.Errorf("tunnel: invalid bind port %d (must be positive)", bindPort)
	}

	if logger == nil {
		logger = slog.Default()
	}

	cfg := &v1.ServerConfig{}
	cfg.BindPort = bindPort

	// Configure token auth if token is provided.
	if token != "" {
		cfg.Auth.Method = v1.AuthMethodToken
		cfg.Auth.Token = token
	}

	// Configure allowed port range.
	if allowPorts != "" {
		ports, err := parsePortsRange(allowPorts)
		if err != nil {
			return nil, fmt.Errorf("tunnel: parse allow ports %q: %w", allowPorts, err)
		}
		cfg.AllowPorts = ports
	}

	// Complete fills defaults and validates the config.
	if err := cfg.Complete(); err != nil {
		return nil, fmt.Errorf("tunnel: complete frps config: %w", err)
	}

	svc, err := frpserver.NewService(cfg)
	if err != nil {
		return nil, fmt.Errorf("tunnel: create frp service: %w", err)
	}

	logger.Info("FRP tunnel manager created",
		slog.Int("bind_port", bindPort),
		slog.String("allow_ports", allowPorts),
	)

	return &Manager{frpSvc: svc, logger: logger}, nil
}

// Start runs the FRP server in the current goroutine. The caller should wrap
// this in a goroutine: go mgr.Start(ctx).
// The server runs until the context is cancelled or Close() is called.
func (m *Manager) Start(ctx context.Context) {
	m.logger.Info("starting FRP tunnel server")
	m.frpSvc.Run(ctx)
}

// Close shuts down the FRP server gracefully.
func (m *Manager) Close() error {
	m.logger.Info("stopping FRP tunnel server")
	return m.frpSvc.Close()
}

// parsePortsRange parses a port range string like "10000-10255" into
// the frp types.PortsRange slice.
func parsePortsRange(s string) ([]types.PortsRange, error) {
	// Use the same type as frp config
	var start, end int
	_, err := fmt.Sscanf(s, "%d-%d", &start, &end)
	if err != nil {
		return nil, fmt.Errorf("invalid port range format %q (expected start-end)", s)
	}
	if start > end {
		return nil, fmt.Errorf("invalid port range: start %d > end %d", start, end)
	}
	return []types.PortsRange{{Start: start, End: end}}, nil
}
