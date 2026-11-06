package generic

import (
	"fmt"
	"strings"

	"github.com/gpuai-dev/gpuai-oss/internal/provider"
)

// --- Supplier API response types (what suppliers return) ---

type availableResponse struct {
	Offerings []offeringJSON `json:"offerings"`
}

type offeringJSON struct {
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

// provisionRequest is the JSON body sent to POST /v1/instances.
type provisionRequest struct {
	InstanceID    string   `json:"instance_id"`
	GPUType       string   `json:"gpu_type"`
	GPUCount      int      `json:"gpu_count"`
	Tier          string   `json:"tier"`
	Region        string   `json:"region,omitempty"`
	SSHPublicKeys []string `json:"ssh_public_keys,omitempty"`
	DockerImage   string   `json:"docker_image,omitempty"`
	StartupScript string   `json:"startup_script,omitempty"`
	// RegistryAuth authenticates a private docker_image pull (custom images
	// Phase 2) — an ADDITIVE field in the supplier API spec; suppliers that
	// predate it ignore it and the pull simply fails on a private image.
	// Secret: carried only on this outbound body, never logged.
	RegistryAuth *registryAuth `json:"registry_auth,omitempty"`
}

// registryAuth is the supplier-API shape of a private-registry login.
type registryAuth struct {
	Registry string `json:"registry"`
	Username string `json:"username"`
	Password string `json:"password"`
}

type provisionResponse struct {
	UpstreamID            string  `json:"upstream_id"`
	Status                string  `json:"status"`
	CostPerHour           float64 `json:"cost_per_hour"`
	EstimatedReadySeconds int     `json:"estimated_ready_seconds"`
	DatacenterLocation    string  `json:"datacenter_location"`
	Region                string  `json:"region"`
}

type statusResponse struct {
	UpstreamID    string  `json:"upstream_id"`
	Status        string  `json:"status"`
	IP            string  `json:"ip"`
	CostPerHour   float64 `json:"cost_per_hour"`
	UptimeSeconds int     `json:"uptime_seconds"`
	// SSHPort is the public TCP port that reaches the instance's sshd at IP.
	// Optional and additive: suppliers that omit it are reached on port 22.
	SSHPort int `json:"ssh_port,omitempty"`
}

// --- Mapping functions ---

// toGPUOfferings converts a supplier's available response into provider.GPUOffering slices.
// Provider is always "generic" -- never the supplier slug (privacy: Pitfall 6).
//
// PricePerHour is the WHOLE-NODE wholesale rate copied through unchanged (never
// multiplied by gpu_count), the invariant the spot-bid logic in
// internal/provision/engine.go relies on.
// Verified against a live supplier 2026-09-01 (GPU-64): it reports a flat
// per-GPU rate scaled by gpu_count, i.e. price_per_hour already covers the whole
// node, so multiplying here would overcharge an N-GPU node N-fold.
func toGPUOfferings(resp *availableResponse, _ string) []provider.GPUOffering {
	if resp == nil || len(resp.Offerings) == 0 {
		return nil
	}

	offerings := make([]provider.GPUOffering, 0, len(resp.Offerings))
	for _, o := range resp.Offerings {
		offerings = append(offerings, provider.GPUOffering{
			Provider:           "generic",
			GPUType:            provider.GPUType(o.GPUType),
			GPUCount:           o.GPUCount,
			VRAMPerGPUGB:       o.VRAMPerGPUGB,
			CPUCores:           o.CPUCores,
			RAMGB:              o.RAMGB,
			StorageGB:          o.StorageGB,
			PricePerHour:       o.PricePerHour,
			Tier:               provider.InstanceTier(o.Tier),
			Region:             o.Region,
			DatacenterLocation: o.DatacenterLocation,
			StockStatus:        o.StockStatus,
			AvailableCount:     o.AvailableCount,
		})
	}
	return offerings
}

// toProvisionResult converts a supplier's provision response into a provider.ProvisionResult.
// UpstreamID is encoded as "slug:id" for routing (Pitfall 2).
func toProvisionResult(resp *provisionResponse, supplierSlug string) *provider.ProvisionResult {
	return &provider.ProvisionResult{
		UpstreamID:            supplierSlug + ":" + resp.UpstreamID,
		Provider:              "generic",
		Status:                resp.Status,
		CostPerHour:           resp.CostPerHour,
		EstimatedReadySeconds: resp.EstimatedReadySeconds,
		DatacenterLocation:    resp.DatacenterLocation,
		Region:                resp.Region,
	}
}

// toInstanceStatus converts a supplier's status response into a provider.InstanceStatus.
// UpstreamID is encoded as "slug:id" for consistency.
// A reported ssh_port becomes the 22/tcp port mapping, which direct-connect
// mode (no FRP tunnel) uses as the customer's SSH endpoint.
func toInstanceStatus(resp *statusResponse, supplierSlug string) *provider.InstanceStatus {
	st := &provider.InstanceStatus{
		UpstreamID:    supplierSlug + ":" + resp.UpstreamID,
		Status:        resp.Status,
		IP:            resp.IP,
		CostPerHour:   resp.CostPerHour,
		UptimeSeconds: resp.UptimeSeconds,
	}
	if resp.SSHPort > 0 && resp.SSHPort <= 65535 {
		st.Ports = []provider.PortMapping{{IP: resp.IP, PrivatePort: 22, PublicPort: resp.SSHPort, Protocol: "tcp"}}
	}
	return st
}

// isCapacityError reports whether a supplier provision error is a
// capacity rejection ("pool full") rather than a genuine failure. The
// supplier spec never standardized an error code for this, so match the
// phrases suppliers actually return in their 4xx/409 bodies (the HTTP
// error path preserves the response body in the error string).
func isCapacityError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	for _, phrase := range []string{
		"no_capacity",
		"no capacity",
		"insufficient capacity",
		"out of capacity",
		"out of stock",
		"no availability",
	} {
		if strings.Contains(msg, phrase) {
			return true
		}
	}
	return false
}

// splitUpstreamID extracts the supplier slug and instance ID from
// a "slug:instanceID" encoded upstream_id.
func splitUpstreamID(upstreamID string) (slug string, instanceID string, err error) {
	idx := strings.Index(upstreamID, ":")
	if idx < 0 {
		return "", "", fmt.Errorf("invalid upstream_id %q: missing slug:id separator", upstreamID)
	}
	slug = upstreamID[:idx]
	instanceID = upstreamID[idx+1:]
	if slug == "" || instanceID == "" {
		return "", "", fmt.Errorf("invalid upstream_id %q: empty slug or instance ID", upstreamID)
	}
	return slug, instanceID, nil
}
