package availability

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/gpuai-dev/gpuai-oss/internal/provider"
)

// errorLines returns the level=ERROR records a JSON slog handler wrote to buf.
func errorLines(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("bad log line %q: %v", line, err)
		}
		if rec["level"] == "ERROR" {
			out = append(out, rec)
		}
	}
	return out
}

func newTestGate(cooldown time.Duration) (*FundingGate, *fixedClock, *bytes.Buffer) {
	buf := &bytes.Buffer{}
	g := NewFundingGate(cooldown, slog.New(slog.NewJSONHandler(buf, nil)))
	clk := newClock(time.Now())
	g.SetClock(clk.Now)
	return g, clk, buf
}

func TestFundingGate_NilIsInert(t *testing.T) {
	var g *FundingGate
	if g.Suspended("bravo") {
		t.Error("nil gate must suspend nothing")
	}
	if g.Trip("bravo", "create_rejected", "x", true) {
		t.Error("nil gate Trip must be a no-op")
	}
	if g.Snapshot() != nil {
		t.Error("nil gate snapshot must be nil")
	}
}

func TestFundingGate_TripSuspendsForCooldownThenReopens(t *testing.T) {
	g, clk, _ := newTestGate(10 * time.Minute)
	if g.Suspended("bravo") {
		t.Fatal("untripped provider must be placeable")
	}
	if !g.Trip("bravo", "create_rejected", "insufficient_credit", false) {
		t.Error("first trip must report it opened a suspension")
	}
	if !g.Suspended("bravo") || !g.Suspended("BRAVO") {
		t.Error("tripped provider must be suspended (case-insensitively)")
	}
	if g.Suspended("alpha") {
		t.Error("suspension must be per provider")
	}
	if g.Trip("bravo", "create_rejected", "again", false) {
		t.Error("re-trip while suspended must not report a new suspension")
	}

	clk.Advance(9 * time.Minute)
	if !g.Suspended("bravo") {
		t.Error("must stay suspended inside the cooldown")
	}
	clk.Advance(2 * time.Minute)
	// The re-trip at t=0 extended nothing past 10m, so at 11m it is open —
	// the next launch is the re-probe.
	if g.Suspended("bravo") {
		t.Error("must re-open once the cooldown lapses")
	}
}

func TestFundingGate_ReTripExtendsSuspension(t *testing.T) {
	g, clk, _ := newTestGate(10 * time.Minute)
	g.Trip("bravo", "balance_poller", "empty", false)
	clk.Advance(5 * time.Minute)
	g.Trip("bravo", "balance_poller", "empty", false) // the next 5-min poll
	clk.Advance(9 * time.Minute)
	if !g.Suspended("bravo") {
		t.Error("a re-trip must extend the suspension from the re-trip time")
	}
}

func TestFundingGate_LoudAlertOncePerHour(t *testing.T) {
	g, clk, buf := newTestGate(10 * time.Minute)

	g.Trip("bravo", "create_rejected", "raw upstream body", true)
	g.Trip("bravo", "create_rejected", "raw upstream body", true)
	alerts := errorLines(t, buf)
	if len(alerts) != 1 {
		t.Fatalf("want 1 alert inside the repeat window, got %d", len(alerts))
	}
	a := alerts[0]
	if a["msg"] != FundingAlertMessage || a["reason"] != FundingAlertReason || a["provider"] != "bravo" {
		t.Errorf("unexpected alert record: %v", a)
	}
	if _, leaked := a["detail"]; leaked {
		t.Error("the raw upstream error must never ride the alert line")
	}

	// A different provider pages independently.
	g.Trip("alpha", "create_rejected", "x", true)
	if n := len(errorLines(t, buf)); n != 2 {
		t.Fatalf("want a second provider's alert, got %d total", n)
	}

	clk.Advance(FundingAlertRepeat)
	g.Trip("bravo", "create_rejected", "x", true)
	if n := len(errorLines(t, buf)); n != 3 {
		t.Fatalf("want a re-page after the repeat window, got %d total", n)
	}
}

func TestFundingGate_QuietTripNeverPages(t *testing.T) {
	g, _, buf := newTestGate(0)
	g.Trip("bravo", "balance_poller", "prepaid balance at or below zero", false)
	if n := len(errorLines(t, buf)); n != 0 {
		t.Errorf("quiet trip must not page (the balance guardrail already does), got %d alerts", n)
	}
	if !g.Suspended("bravo") {
		t.Error("quiet trip must still suspend")
	}
}

func TestFundingGate_Snapshot(t *testing.T) {
	g, clk, _ := newTestGate(10 * time.Minute)
	g.Trip("bravo", "create_rejected", "", false)
	g.Trip("alpha", "balance_poller", "", false)
	snap := g.Snapshot()
	if len(snap) != 2 || snap[0].Provider != "alpha" || snap[1].Provider != "bravo" || snap[1].Source != "create_rejected" {
		t.Fatalf("unexpected snapshot: %+v", snap)
	}
	clk.Advance(11 * time.Minute)
	if snap := g.Snapshot(); len(snap) != 0 {
		t.Errorf("lapsed suspensions must leave the snapshot, got %+v", snap)
	}
}

// TestPoller_FundingGateHidesSuspendedProvider — while a supplier is
// suspended for an empty account its rows leave the catalog (every launch on
// them would be refused); other suppliers keep flowing, and the rows return
// once the gate re-opens.
func TestPoller_FundingGateHidesSuspendedProvider(t *testing.T) {
	registry := provider.NewRegistry()
	broke := newFakeProvider("broke", sampleOfferings("broke"))
	funded := newFakeProvider("funded", sampleOfferings("funded"))
	registry.Register(broke)
	registry.Register(funded)

	g, clk, _ := newTestGate(10 * time.Minute)
	poller := &Poller{
		registry:     registry,
		interval:     30 * time.Second,
		probeTimeout: time.Second,
		fundingGate:  g,
		logger:       silentLogger(),
	}
	ctx := context.Background()

	all, _ := poller.collectOfferings(ctx)
	if len(all) != 2 {
		t.Fatalf("healthy cycle: want 2 offerings, got %d", len(all))
	}

	g.Trip("broke", "create_rejected", "", false)
	got, staff := poller.collectOfferings(ctx)
	if len(got) != 1 || len(staff) != 1 {
		t.Fatalf("suspended cycle: want 1 customer + 1 staff row, got %d / %d", len(got), len(staff))
	}
	for _, o := range staff {
		if o.Provider == "broke" {
			t.Error("suspended supplier must not appear in the catalog")
		}
	}

	clk.Advance(11 * time.Minute)
	if got, _ := poller.collectOfferings(ctx); len(got) != 2 {
		t.Fatalf("after the cooldown the supplier must return, got %d offerings", len(got))
	}
}
