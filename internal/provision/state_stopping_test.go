package provision

import "testing"

// TestShouldTransitionToStopping_SkipsErrorRows is the regression test for the
// 23505 that aborted 187 prod terminates. See shouldTransitionToStopping.
func TestShouldTransitionToStopping_SkipsErrorRows(t *testing.T) {
	if shouldTransitionToStopping(StateError) {
		t.Error("an error row must NOT be moved to stopping — it re-enters the v7 partial unique index on frp_remote_port against the live holder of the port it already released, and the 23505 aborts the whole terminate")
	}
	// The state machine still PERMITS the transition; the terminate path is what
	// declines it. If this ever stops being true, the comment above is stale.
	if !CanTransition(StateError, StateStopping) {
		t.Error("validTransitions no longer allows error -> stopping; shouldTransitionToStopping's rationale needs rewriting")
	}
}

func TestShouldTransitionToStopping_LiveRowsStillStop(t *testing.T) {
	for _, st := range []string{StateCreating, StateProvisioning, StateBooting, StateRunning, StateStopped} {
		if !shouldTransitionToStopping(st) {
			t.Errorf("shouldTransitionToStopping(%q) = false, want true — live rows must still be parked in stopping during teardown", st)
		}
	}
}

func TestShouldTransitionToStopping_TerminalRowsDoNot(t *testing.T) {
	for _, st := range []string{StateTerminated, StateStopping} {
		if shouldTransitionToStopping(st) {
			t.Errorf("shouldTransitionToStopping(%q) = true, want false", st)
		}
	}
}

// TestErrorIsTerminalButTerminateStillProceeds documents the asymmetry the fix
// relies on: `error` is terminal, yet terminate must still run steps 4-5 to tear
// the upstream VM down. Only the cosmetic `stopping` hop is skipped.
func TestErrorIsTerminalButTerminateStillProceeds(t *testing.T) {
	if !IsTerminal(StateError) {
		t.Error("IsTerminal(error) = false — shouldTransitionToStopping's justification assumes error is terminal")
	}
}
