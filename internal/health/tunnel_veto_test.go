package health

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/gpuai-dev/gpuai-oss/internal/db"
	"github.com/gpuai-dev/gpuai-oss/internal/provider"
)

// count reports how many captured records have exactly this level and message.
func (h *recordingHandler) count(level slog.Level, msg string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := 0
	for _, r := range h.records {
		if r.Level == level && r.Message == msg {
			n++
		}
	}
	return n
}

// vetoTestMonitor builds a Monitor whose tunnel probe answers while *answers
// is true, and counts the probes it makes. No database: the veto decision
// never touches one.
func vetoTestMonitor(answers *bool) (*Monitor, *recordingHandler, *int) {
	logs := &recordingHandler{}
	probes := 0
	m := NewMonitor(MonitorDeps{Logger: slog.New(logs), TunnelProbeHost: "gw.test"})
	m.probeTunnel = func(string) error {
		probes++
		if *answers {
			return nil
		}
		return errors.New("connection refused")
	}
	return m, logs, &probes
}

func vetoInstance(status string) *db.Instance {
	port := 10042
	return &db.Instance{
		InstanceID: "gpu-veto0001", OrgID: "org_test", UpstreamProvider: "flapprov",
		Tier: "on_demand", Status: status, FRPRemotePort: &port,
	}
}

func TestTunnelVetoTracker(t *testing.T) {
	var tr tunnelVetoTracker // the zero value must be usable
	t0 := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)

	if since, first := tr.begin("a", t0); !first || !since.Equal(t0) {
		t.Fatalf("first veto: since %v first %v", since, first)
	}
	if since, first := tr.begin("a", t0.Add(time.Minute)); first || !since.Equal(t0) {
		t.Errorf("a later veto must keep the first start: since %v first %v", since, first)
	}
	tr.begin("b", t0)
	tr.retain(map[string]struct{}{"a": {}})
	if tr.size() != 1 {
		t.Errorf("retain kept %d vetoes; want only the live instance's", tr.size())
	}
	if since, ok := tr.end("a"); !ok || !since.Equal(t0) {
		t.Errorf("end: since %v ok %v", since, ok)
	}
	if _, ok := tr.end("a"); ok || tr.size() != 0 {
		t.Errorf("an ended veto must be gone (size %d)", tr.size())
	}
}

// A running instance that answers through the tunnel is kept for at most
// tunnelVetoMaxDuration from its FIRST veto. Then the veto stops (an ERROR
// line pages) and stays stopped, and a silent probe in between does not
// restart the clock.
func TestTunnelVeto_IsBoundedAndLogged(t *testing.T) {
	answers := true
	m, logs, _ := vetoTestMonitor(&answers)
	inst := vetoInstance("running")
	t0 := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)

	if !m.tunnelVeto(inst, t0) {
		t.Fatal("a running instance answering through the tunnel must be kept")
	}
	if n := logs.count(slog.LevelWarn, tunnelVetoStartMessage); n != 1 {
		t.Errorf("veto start logged %d times; want once", n)
	}

	answers = false
	if m.tunnelVeto(inst, t0.Add(10*time.Minute)) {
		t.Error("a silent tunnel is no evidence: the normal failure path must run")
	}
	answers = true

	if !m.tunnelVeto(inst, t0.Add(tunnelVetoMaxDuration)) {
		t.Error("kept at exactly the bound")
	}
	if n := logs.count(slog.LevelWarn, tunnelVetoStartMessage); n != 1 {
		t.Errorf("veto start logged %d times; want once per veto, not per tick", n)
	}
	if logs.hasError(tunnelVetoExpiredMessage) {
		t.Fatal("expiry logged before the bound")
	}

	if m.tunnelVeto(inst, t0.Add(tunnelVetoMaxDuration+time.Minute)) {
		t.Fatal("past the bound the provider must win, as before the veto existed")
	}
	if !logs.hasError(tunnelVetoExpiredMessage) {
		t.Error("expiry must log at ERROR so it pages")
	}
	if m.tunnelVeto(inst, t0.Add(tunnelVetoMaxDuration+2*time.Minute)) {
		t.Error("an expired veto must stay expired on the next tick")
	}
}

// The veto is for running instances only: a booting one is the engine's poll
// loop's to judge, so it is not even probed.
func TestTunnelVeto_OnlyRunningInstances(t *testing.T) {
	answers := true
	m, _, probes := vetoTestMonitor(&answers)
	if m.tunnelVeto(vetoInstance("booting"), time.Now()) {
		t.Error("a booting instance must not be vetoed")
	}
	if *probes != 0 || m.tunnelVetoes.size() != 0 {
		t.Errorf("booting: %d probes, %d vetoes; want none", *probes, m.tunnelVetoes.size())
	}
}

// No gateway, no allocated port, or a silent tunnel is no evidence: no veto,
// and no veto clock started.
func TestTunnelVeto_NoEvidenceNoVeto(t *testing.T) {
	answers := false
	m, _, _ := vetoTestMonitor(&answers)
	if m.tunnelVeto(vetoInstance("running"), time.Now()) {
		t.Error("silent tunnel vetoed")
	}
	answers = true
	noPort := vetoInstance("running")
	noPort.FRPRemotePort = nil
	if m.tunnelVeto(noPort, time.Now()) {
		t.Error("instance without a tunnel port vetoed")
	}
	m.tunnelProbeHost = ""
	if m.tunnelVeto(vetoInstance("running"), time.Now()) {
		t.Error("vetoed without a configured gateway")
	}
	if m.tunnelVetoes.size() != 0 {
		t.Errorf("%d vetoes started without evidence", m.tunnelVetoes.size())
	}
}

// When the provider reports the instance running again, the veto ends, so a
// later disagreement gets a fresh bound instead of inheriting an expired one.
func TestTunnelVeto_ProviderRunningAgainEndsIt(t *testing.T) {
	answers := true
	m, logs, _ := vetoTestMonitor(&answers)
	reg := provider.NewRegistry()
	reg.Register(&statusProvider{notFoundProvider: notFoundProvider{name: "flapprov"}, status: "running"})
	m.registry = reg
	inst := vetoInstance("running")

	m.tunnelVeto(inst, time.Now().UTC().Add(-2*tunnelVetoMaxDuration))
	if m.tunnelVeto(inst, time.Now().UTC()) {
		t.Fatal("setup: the veto should have expired")
	}

	// The reachability probe is not under test here; without a gateway it
	// skips, which keeps checkInstance off the network and the database.
	m.tunnelProbeHost = ""
	m.checkInstance(context.Background(), inst)
	m.tunnelProbeHost = "gw.test"

	if m.tunnelVetoes.size() != 0 {
		t.Fatal("provider reporting running must end the veto")
	}
	if n := logs.count(slog.LevelInfo, "health check: provider reports the instance running again; tunnel veto over"); n != 1 {
		t.Errorf("veto end logged %d times; want once", n)
	}
	if !m.tunnelVeto(inst, time.Now().UTC()) {
		t.Error("a new disagreement must get a fresh bound")
	}
}
