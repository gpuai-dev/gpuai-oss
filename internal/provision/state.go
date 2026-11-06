// Package provision orchestrates the instance provisioning flow.
package provision

// Instance state constants.
const (
	StateCreating     = "creating"
	StateProvisioning = "provisioning"
	StateBooting      = "booting"
	StateRunning      = "running"
	StateStopped      = "stopped"
	StateStopping     = "stopping"
	StateTerminated   = "terminated"
	StateError        = "error"
)

// validTransitions defines which state transitions are allowed.
// Key: current state, Value: list of valid next states.
//
// creating/provisioning -> running is allowed because fast-booting providers
// (fixed-template and VM-image suppliers) fire the ready callback before the engine's status-poll
// loop has had a chance to advance us through booting; we honor the
// callback as authoritative rather than ignoring it. See
// db.SetInstanceRunning for the matching SQL guard.
var validTransitions = map[string][]string{
	StateCreating:     {StateProvisioning, StateRunning, StateError, StateStopping},
	StateProvisioning: {StateBooting, StateRunning, StateError, StateStopping},
	StateBooting:      {StateRunning, StateError, StateStopping},
	StateRunning:      {StateStopped, StateStopping, StateError},
	StateStopped:      {StateRunning, StateStopping, StateTerminated},
	StateStopping:     {StateTerminated, StateError},
	StateTerminated:   {},
	StateError:        {StateStopping},
}

// CanTransition returns true if transitioning from the given state to the
// target state is valid according to the instance state machine.
func CanTransition(from, to string) bool {
	allowed, ok := validTransitions[from]
	if !ok {
		return false
	}
	for _, s := range allowed {
		if s == to {
			return true
		}
	}
	return false
}

// ExternalState maps an internal instance state to the customer-facing state.
// Internal states are collapsed to reduce API surface complexity.
//
// "allocating" vs "starting": creating/provisioning means we are still
// acquiring the node from the provider (a baremetal physical allocation can
// take ~13 min), whereas booting means the node exists and is coming up. The
// split lets the UI tag a freshly-launched slow-boot instance "Allocating" and
// set the right expectation instead of a single ambiguous "Starting".
func ExternalState(internal string) string {
	switch internal {
	case StateCreating, StateProvisioning:
		return "allocating"
	case StateBooting:
		return "starting"
	case StateRunning:
		return "running"
	case StateStopped:
		return "stopped"
	case StateStopping:
		return "stopping"
	case StateTerminated:
		return "terminated"
	case StateError:
		return "error"
	default:
		return "unknown"
	}
}

// IsTerminal returns true if the given state is a terminal state.
// Terminal states are states from which no forward progress is expected.
func IsTerminal(state string) bool {
	return state == StateTerminated || state == StateError
}

// shouldTransitionToStopping reports whether a terminate should park the row in
// `stopping` before tearing the upstream down.
//
// Everything live does. An `error` row does NOT, even though validTransitions
// permits error -> stopping, because the v7 partial unique index
// instances_frp_remote_port_active covers frp_remote_port WHERE status NOT IN
// ('terminated','error'). An errored row's port therefore reads as free and gets
// reallocated to a live instance; moving the errored row into `stopping` drags
// it back into that index against the live holder and Postgres rejects the
// UPDATE with SQLSTATE 23505, aborting the whole terminate. That happened 187
// times on prod, and an aborted terminate means the upstream VM is never torn
// down and keeps billing.
//
// Skipping the hop is safe: `error` is already terminal per IsTerminal(), and
// db.TerminateInstance is guarded on status NOT IN ('terminated') rather than on
// this state machine, so error -> terminated still lands.
func shouldTransitionToStopping(status string) bool {
	if status == StateError {
		return false
	}
	return CanTransition(status, StateStopping)
}
