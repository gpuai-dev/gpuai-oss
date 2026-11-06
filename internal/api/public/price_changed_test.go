package public

import (
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/gpuai-dev/gpuai-oss/internal/provider"
	"github.com/gpuai-dev/gpuai-oss/internal/provision"
)

// TestMapEngineError_PriceChanged — a price mismatch is the one launch failure
// a retry can never fix. Mapping it to operation_failed told SDKs the opposite:
// that handler's own contract calls operation_failed "retryable after a
// backoff", so an automated client would spin forever on a request that can
// only succeed after a human re-prices it. This became reachable for every
// CLI/SDK launch the moment each one carried an implicit price ceiling.
func TestMapEngineError_PriceChanged(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{
			name: "bare sentinel",
			err:  provision.ErrPriceExceeded,
			want: "price_changed",
		},
		{
			name: "priced detail carrying the sentinel",
			err:  &provision.PriceExceededError{CurrentBestPerHour: 0.34, MaxPricePerHour: 0.16},
			want: "price_changed",
		},
		{
			name: "wrapped by the walk's all-providers-failed error",
			err:  fmt.Errorf("provision: all providers failed (last: vultr): %w", &provision.PriceExceededError{CurrentBestPerHour: 0.34, MaxPricePerHour: 0.16}),
			want: "price_changed",
		},
		{
			// Genuinely transient — must stay retryable, or we would tell
			// clients to give up on capacity that comes back in a minute.
			name: "no capacity is still retryable",
			err:  provider.ErrNoCapacity,
			want: "operation_failed",
		},
		{
			// The engine's template-VRAM backstop (GPU-87): static floor vs
			// static VRAM, so a retry can never fix it — the async path must
			// carry the stable code, not "retryable" operation_failed.
			name: "template VRAM shortfall is not retryable",
			err:  fmt.Errorf("provision: %w", provision.TemplateVRAMShortfall("sglang", 24, "rtx_a4000")),
			want: "gpu_vram_too_small",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := mapEngineError(tc.err); got != tc.want {
				t.Errorf("mapEngineError() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestPriceChangedIsRegistered — the registry is the contract. An unregistered
// code fails the coherence test, and a wrong status would tell clients the
// request itself was malformed rather than that the market moved.
func TestPriceChangedIsRegistered(t *testing.T) {
	status, ok := CodeRegistry()["price_changed"]
	if !ok {
		t.Fatal("price_changed missing from codeRegistry")
	}
	if status != http.StatusConflict {
		t.Errorf("price_changed status = %d, want %d (conflict with market state, not a bad request)",
			status, http.StatusConflict)
	}
}

// TestPriceExceededSentinelUnwraps guards the seam the mapping depends on: the
// cost-ceiling refusal is translated into PriceExceededError inside the walk
// and then wrapped again, so both errors.Is hops must survive for the API to
// classify it correctly.
func TestPriceExceededSentinelUnwraps(t *testing.T) {
	ceil := &provider.CostCeilingError{CheapestCostPerHour: 0.3348, CeilingCostPerHour: 0.168}
	if !errors.Is(ceil, provider.ErrCostCeilingExceeded) {
		t.Error("CostCeilingError must match its own sentinel")
	}
	priced := &provision.PriceExceededError{CurrentBestPerHour: 0.34, MaxPricePerHour: 0.16}
	wrapped := fmt.Errorf("provision: all providers failed (last: vultr): %w", priced)
	if !errors.Is(wrapped, provision.ErrPriceExceeded) {
		t.Error("wrapped PriceExceededError must still match the sentinel")
	}
	var out *provision.PriceExceededError
	if !errors.As(wrapped, &out) || out.CurrentBestPerHour != 0.34 {
		t.Error("the re-quote price must survive wrapping — it is what the client displays")
	}
}
