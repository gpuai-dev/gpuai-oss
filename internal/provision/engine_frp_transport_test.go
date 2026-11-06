package provision

import (
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/gpuai-dev/gpuai-oss/internal/config"
	"github.com/gpuai-dev/gpuai-oss/internal/provider"
	"github.com/gpuai-dev/gpuai-oss/internal/tunnel"
)

// transportTestBootstrapData is a minimally-valid BootstrapData so
// RenderBootstrap succeeds inside the walk.
func transportTestBootstrapData() *tunnel.BootstrapData {
	return &tunnel.BootstrapData{
		InstanceID:        "gpu-transport",
		ProxyHost:         "frp.example.com",
		FRPServerPort:     7000,
		FRPToken:          "tok",
		RemotePort:        10042,
		SSHAuthorizedKeys: "ssh-ed25519 AAAAC3Nz test@host",
		InternalToken:     "itok",
		Hostname:          "gpu-transport.example.com",
		CallbackURL:       "https://api.example.com/internal/instances/gpu-transport/ready",
	}
}

func transportTestEngine(db *fakeEngineDB, cfg *config.Config) *Engine {
	return &Engine{logger: slog.Default(), db: db, config: cfg}
}

// TestTransportOverrideIsRestaged is the regression guard for the bug that
// made the first live experiment a FALSE NEGATIVE.
//
// The bootstrap is rendered and staged BEFORE the candidate walk. The walk
// re-renders for a provider with a transport override, but re-assigning
// provReq.StartupScript alone only reaches providers that carry the whole
// script in their launch call. Container / stage-0-loader providers boot a
// tiny loader that FETCHES the staged copy from
// GET /internal/instances/{id}/bootstrap — so without a re-stage they download
// and run the DEFAULT-transport script while every server-side signal claims
// the override was applied.
//
// That is exactly what happened on demo (gpu-052746bc, 2026-08-13): the
// tunnel registered, SSH still died on the first client->container byte, and
// the "websocket doesn't help" conclusion was wrong — the box was running the
// raw-TCP config it had fetched.
func TestTransportOverrideIsRestaged(t *testing.T) {
	fdb := &fakeEngineDB{}
	cfg := &config.Config{ProviderFRPTransport: map[string]string{"stub": "websocket"}}

	stub := &stubProvider{
		name:            "stub",
		provisionResult: &provider.ProvisionResult{UpstreamID: "up-1", Provider: "stub"},
	}
	o := baseFloorOffering()
	provReq := &provider.ProvisionRequest{
		InstanceID: "gpu-transport", GPUType: o.GPUType, GPUCount: 1, Tier: o.Tier,
	}

	_, _, _, err := transportTestEngine(fdb, cfg).walkProvisionCandidates(
		context.Background(), provReq,
		[]ProviderCandidate{floorTestCandidate(stub, o)},
		provisionWalkParams{
			instanceID:    "gpu-transport",
			bootstrapData: transportTestBootstrapData(),
		},
	)
	if err != nil {
		t.Fatalf("walk: %v", err)
	}

	// The script handed to the adapter must carry the override...
	if len(stub.provisionReqs) != 1 {
		t.Fatalf("want 1 Provision call, got %d", len(stub.provisionReqs))
	}
	if !strings.Contains(stub.provisionReqs[0].StartupScript, `transport.protocol = "websocket"`) {
		t.Error("ProvisionRequest.StartupScript lost the transport override")
	}

	// ...AND the staged copy must too, or stage-0-loader providers fetch the
	// wrong config. This is the assertion that would have caught the bug.
	if len(fdb.stagedBootstraps) == 0 {
		t.Fatal("walk never re-staged the bootstrap; a stage-0-loader provider would fetch the default-transport script")
	}
	last := fdb.stagedBootstraps[len(fdb.stagedBootstraps)-1]
	if !strings.Contains(last, `transport.protocol = "websocket"`) {
		t.Error("staged bootstrap lost the transport override — the instance would boot the default transport")
	}
}

// TestNoOverrideNeverRestages pins the blast radius: with no provider opted
// in, the walk must not re-render OR re-stage, so the pre-walk staged script
// stands and behavior is identical to before this knob existed.
func TestNoOverrideNeverRestages(t *testing.T) {
	fdb := &fakeEngineDB{}
	cfg := &config.Config{} // no overrides

	stub := &stubProvider{
		name:            "stub",
		provisionResult: &provider.ProvisionResult{UpstreamID: "up-1", Provider: "stub"},
	}
	o := baseFloorOffering()
	provReq := &provider.ProvisionRequest{
		InstanceID: "gpu-plain", GPUType: o.GPUType, GPUCount: 1, Tier: o.Tier,
		StartupScript: "ORIGINAL-SCRIPT",
	}

	_, _, _, err := transportTestEngine(fdb, cfg).walkProvisionCandidates(
		context.Background(), provReq,
		[]ProviderCandidate{floorTestCandidate(stub, o)},
		provisionWalkParams{
			instanceID:    "gpu-plain",
			bootstrapData: transportTestBootstrapData(),
		},
	)
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	if len(fdb.stagedBootstraps) != 0 {
		t.Errorf("walk re-staged %d time(s) with no override set; want 0", len(fdb.stagedBootstraps))
	}
	if got := stub.provisionReqs[0].StartupScript; got != "ORIGINAL-SCRIPT" {
		t.Errorf("StartupScript was rewritten with no override set: %q", got)
	}
}
