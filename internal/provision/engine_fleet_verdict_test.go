package provision

import (
	"context"
	"testing"

	"github.com/gpuai-dev/gpuai-oss/internal/availability"
)

type delistAll bool

func (d delistAll) Delisted(string) bool { return bool(d) }

// In enforce mode a delisted offering is skipped at placement; shadow mode,
// probe launches and staff provider pins still reach it.
func TestSelectProviderCandidates_FleetProbeVerdicts(t *testing.T) {
	stub := fallbackStub("marketco", 1, false, "up")
	for _, tc := range []struct {
		name    string
		enforce bool
		mutate  func(r *ProvisionRequest)
		want    int
	}{
		{"enforce skips", true, nil, 0},
		{"shadow keeps", false, nil, 1},
		{"probe launches still reach it", true, func(r *ProvisionRequest) { r.FleetProbe = true }, 1},
		{"staff pin still reaches it", true, func(r *ProvisionRequest) { r.PinnedProvider = "marketco" }, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newFallbackEngine(fakeWithOneKey(), stub)
			var v availability.OfferingVerdicts = delistAll(true)
			e.SetOfferingVerdicts(v, tc.enforce)
			req := onDemandRequest()
			if tc.mutate != nil {
				tc.mutate(&req)
			}
			got, err := e.SelectProviderCandidates(context.Background(), req)
			if len(got) != tc.want {
				t.Errorf("candidates %d (err %v); want %d", len(got), err, tc.want)
			}
		})
	}
}
