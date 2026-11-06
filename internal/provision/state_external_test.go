package provision

import "testing"

// TestExternalState_AllocatingSplit locks the customer-facing split introduced
// for slow-boot capacity: creating/provisioning collapse to "allocating" (still
// acquiring the node from the provider — baremetal can take ~13 min), while
// booting maps to "starting" (the node exists and is coming up).
func TestExternalState_AllocatingSplit(t *testing.T) {
	cases := map[string]string{
		StateCreating:     "allocating",
		StateProvisioning: "allocating",
		StateBooting:      "starting",
		StateRunning:      "running",
		StateStopping:     "stopping",
		StateStopped:      "stopped",
		StateTerminated:   "terminated",
		StateError:        "error",
	}
	for in, want := range cases {
		if got := ExternalState(in); got != want {
			t.Errorf("ExternalState(%q) = %q, want %q", in, got, want)
		}
	}
}
