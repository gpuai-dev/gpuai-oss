package public

import (
	"strings"
	"testing"
)

// TestCreateInstanceRequest_Validate_CustomImageSpotRefused pins the GPU-92
// request-shape gate: a custom image on the spot tier is refused up front (a
// preemption re-route cannot yet carry the image), while the identical request
// on on_demand — and a plain spot launch without an image — still validate.
func TestCreateInstanceRequest_Validate_CustomImageSpotRefused(t *testing.T) {
	img := "ghcr.io/acme/trainer:v1"
	cases := []struct {
		name    string
		req     CreateInstanceRequest
		wantErr string // substring; "" means Validate must pass
	}{
		{
			name:    "custom image on spot is refused",
			req:     CreateInstanceRequest{GPUType: "h100_sxm", GPUCount: 1, Tier: "spot", Image: &img},
			wantErr: "spot tier",
		},
		{
			name: "custom image on on_demand validates",
			req:  CreateInstanceRequest{GPUType: "h100_sxm", GPUCount: 1, Tier: "on_demand", Image: &img},
		},
		{
			name: "plain spot launch without an image validates",
			req:  CreateInstanceRequest{GPUType: "h100_sxm", GPUCount: 1, Tier: "spot"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := c.req.Validate()
			if c.wantErr == "" {
				if err != nil {
					t.Fatalf("Validate() = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), c.wantErr) {
				t.Fatalf("Validate() = %v, want error containing %q", err, c.wantErr)
			}
			if strings.Contains(strings.ToLower(err.Error()), "vultr") || strings.Contains(strings.ToLower(err.Error()), "generic") {
				t.Fatalf("validation error must not name an upstream provider: %q", err.Error())
			}
		})
	}
}
