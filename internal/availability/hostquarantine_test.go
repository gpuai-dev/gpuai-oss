package availability

import (
	"fmt"
	"testing"
	"time"
)

func testQuarantine() (*HostQuarantine, *time.Time) {
	q := NewHostQuarantine(DefaultHostQuarantineHold, DefaultHostQuarantineMax, silentLogger())
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	q.now = func() time.Time { return now }
	return q, &now
}

func TestHostQuarantine_HoldsForAnHour(t *testing.T) {
	q, now := testQuarantine()
	if q.Quarantined("alpha", "51178") {
		t.Fatal("nothing is quarantined before a stall")
	}
	q.RecordTunnelStall("alpha", "51178")
	if !q.Quarantined("alpha", "51178") {
		t.Fatal("a tunnel stall must quarantine the machine")
	}
	if q.Quarantined("alpha", "52470") || q.Quarantined("bravo", "51178") {
		t.Error("only that provider's machine is held")
	}
	*now = now.Add(59 * time.Minute)
	q.RecordTunnelStall("alpha", "51178")
	*now = now.Add(59 * time.Minute)
	if !q.Quarantined("alpha", "51178") {
		t.Error("another stall holds the machine for another hour from then")
	}
	*now = now.Add(2 * time.Minute)
	if q.Quarantined("alpha", "51178") {
		t.Error("no hold lasts longer than an hour after the last stall")
	}
}

func TestHostQuarantine_CapsHeldMachines(t *testing.T) {
	q, now := testQuarantine()
	// Eight minutes apart: never three machines inside one burst window, so
	// every stall reads as the machine's fault and the cap is what binds.
	for i := 0; i < DefaultHostQuarantineMax; i++ {
		q.RecordTunnelStall("alpha", fmt.Sprint(1000+i))
		*now = now.Add(8 * time.Minute)
	}
	q.RecordTunnelStall("alpha", "2000")
	if !q.Quarantined("alpha", "2000") {
		t.Error("the newest stall must be held")
	}
	if q.Quarantined("alpha", "1000") {
		t.Error("at the cap, the hold closest to expiring makes room")
	}
	held := 0
	for i := 0; i < DefaultHostQuarantineMax; i++ {
		if q.Quarantined("alpha", fmt.Sprint(1000+i)) {
			held++
		}
	}
	if held+1 != DefaultHostQuarantineMax {
		t.Errorf("held %d machines; never more than %d", held+1, DefaultHostQuarantineMax)
	}
}

func TestHostQuarantine_NilAndEmpty(t *testing.T) {
	var nilQ *HostQuarantine
	nilQ.RecordTunnelStall("alpha", "1")
	if nilQ.Quarantined("alpha", "1") {
		t.Error("a nil quarantine holds nothing")
	}
	q := NewHostQuarantine(DefaultHostQuarantineHold, DefaultHostQuarantineMax, nil)
	q.RecordTunnelStall("alpha", "")
	if q.Quarantined("alpha", "") || len(q.until) != 0 {
		t.Error("an empty host id is never recorded")
	}
}

// Stalls on three different machines inside 15 minutes are GPU.ai's outage,
// not three bad machines: the burst's earlier holds are released, nothing new
// is held while it lasts, and holds resume once the window drains.
func TestHostQuarantine_PlatformBurstHoldsNothing(t *testing.T) {
	q, now := testQuarantine()
	q.RecordTunnelStall("alpha", "900") // a bad machine from before the outage
	*now = now.Add(20 * time.Minute)

	q.RecordTunnelStall("alpha", "1001")
	*now = now.Add(4 * time.Minute)
	q.RecordTunnelStall("alpha", "1002")
	if !q.Quarantined("alpha", "1001") || !q.Quarantined("alpha", "1002") {
		t.Fatal("two machines in a window are still held")
	}
	*now = now.Add(4 * time.Minute)
	q.RecordTunnelStall("alpha", "1003")
	for _, id := range []string{"1001", "1002", "1003"} {
		if q.Quarantined("alpha", id) {
			t.Errorf("machine %s held during a platform-side burst", id)
		}
	}
	if !q.Quarantined("alpha", "900") {
		t.Error("a hold from before the burst window must stand")
	}

	// The outage goes on: every new stall keeps the window full.
	*now = now.Add(5 * time.Minute)
	q.RecordTunnelStall("alpha", "1004")
	*now = now.Add(5 * time.Minute)
	q.RecordTunnelStall("alpha", "1005")
	if q.Quarantined("alpha", "1004") || q.Quarantined("alpha", "1005") {
		t.Error("nothing is held while stalls keep landing on different machines")
	}

	// Recovered: a quiet window later, one stall is one machine again.
	*now = now.Add(DefaultHostQuarantineBurstWindow)
	q.RecordTunnelStall("alpha", "1006")
	if !q.Quarantined("alpha", "1006") {
		t.Error("holds must resume once the burst window drains")
	}
}

// One machine stalling over and over is that machine, not the platform.
func TestHostQuarantine_RepeatStallsOnOneMachineAreNotABurst(t *testing.T) {
	q, now := testQuarantine()
	for i := 0; i < 2*DefaultHostQuarantineBurstMachines; i++ {
		q.RecordTunnelStall("alpha", "51178")
		*now = now.Add(time.Minute)
	}
	q.RecordTunnelStall("alpha", "52470")
	if !q.Quarantined("alpha", "51178") || !q.Quarantined("alpha", "52470") {
		t.Error("repeat stalls on one machine count once toward the burst check")
	}
}
