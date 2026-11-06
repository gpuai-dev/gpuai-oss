// GPU-87: unit tests for the ONE comparison that decides whether an explicit
// gpu_type clears a template's min_vram_gb floor. Both handlers and the engine
// route through TemplateVRAMShortfall, so everything pinned here is pinned for
// every layer at once (the cross-layer matrix in engine_template_floor_test.go
// closes the loop by asserting the engine agrees for every type × floor).
package provision

import (
	"strings"
	"testing"

	"github.com/gpuai-dev/gpuai-oss/internal/provider"
)

func TestTemplateVRAMShortfall(t *testing.T) {
	cases := []struct {
		name       string
		templateID string
		minVRAMGB  int
		gpuType    provider.GPUType
		wantErr    bool
	}{
		// The prod shape: org a054bcdb launched sglang (floor 24) onto an
		// rtx_a4000 (16 GB) five times, each dying on boot in 1-2 minutes.
		{"prod shape: sglang on rtx_a4000", "sglang", 24, provider.GPUTypeRTXA4000, true},
		// Exactly at the floor is allowed — and the L4 is the reason the
		// comparison must run through NormalizeVRAM: upstreams report 22 for a
		// 24 GB card, and template floors are written in nameplate terms.
		{"at floor: l4 meets 24", "sglang", 24, provider.GPUTypeL4, false},
		{"above floor: rtx_4090 meets 24", "sglang", 24, provider.GPUTypeRTX4090, false},
		{"one below floor", "sglang", 24, provider.GPUTypeRTX5080, true},
		// No floor declared => nothing to enforce (raw launches and floorless
		// templates stay byte-for-byte unchanged).
		{"no floor", "sglang", 0, provider.GPUTypeRTXA4000, false},
		{"negative floor", "sglang", -5, provider.GPUTypeRTXA4000, false},
		// Fail OPEN on a type the canonical table has never heard of: junk
		// gpu_types belong to the invalid_gpu_type validation, not here. A
		// wrong 422 from this gate would be unfixable by the customer.
		{"unknown type fails open", "sglang", 24, provider.GPUType("not_a_gpu"), false},
		{"empty type fails open", "sglang", 24, provider.GPUType(""), false},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			got := TemplateVRAMShortfall(tt.templateID, tt.minVRAMGB, tt.gpuType)
			if tt.wantErr && got == nil {
				t.Fatalf("TemplateVRAMShortfall(%q, %d, %q) = nil, want a shortfall error",
					tt.templateID, tt.minVRAMGB, tt.gpuType)
			}
			if !tt.wantErr && got != nil {
				t.Fatalf("TemplateVRAMShortfall(%q, %d, %q) = %v, want nil (allowed)",
					tt.templateID, tt.minVRAMGB, tt.gpuType, got)
			}
		})
	}
}

// The message is the whole point of the feature: it must carry both numbers,
// the offending type, the template, and the escape hatch (which types DO fit).
func TestTemplateVRAMErrorMessageIsActionable(t *testing.T) {
	err := TemplateVRAMShortfall("sglang", 24, provider.GPUTypeRTXA4000)
	if err == nil {
		t.Fatal("expected a shortfall error for sglang(24) on rtx_a4000(16)")
	}
	if err.MinVRAMGB != 24 {
		t.Errorf("MinVRAMGB = %d, want 24", err.MinVRAMGB)
	}
	if err.GPUVRAMGB != 16 {
		t.Errorf("GPUVRAMGB = %d, want 16 (rtx_a4000 nameplate)", err.GPUVRAMGB)
	}
	if err.TemplateID != "sglang" {
		t.Errorf("TemplateID = %q, want sglang", err.TemplateID)
	}
	if err.GPUType != provider.GPUTypeRTXA4000 {
		t.Errorf("GPUType = %q, want rtx_a4000", err.GPUType)
	}
	msg := err.Error()
	for _, want := range []string{"sglang", "24", "16", "rtx_a4000"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q missing %q", msg, want)
		}
	}
	// At least one compatible type must be named — otherwise the customer is
	// back to guessing, which is exactly the loop this ticket ends.
	named := false
	for _, gt := range provider.GPUTypesWithVRAMAtLeast(24) {
		if strings.Contains(msg, string(gt)) {
			named = true
			break
		}
	}
	if !named {
		t.Errorf("error %q names no compatible GPU type", msg)
	}

	// INFPRIV / T-lz9-01: the sentence is built from the canonical GPU table
	// and the template id only — no upstream supplier may ever appear in it.
	for _, prov := range []string{"podco", "vmco", "metalco", "marketco", "fixedimageco",
		"vmimageco", "cloudvmco", "baremetalco", "startupvmco", "communitycloud", "templateonlyco"} {
		if strings.Contains(strings.ToLower(msg), prov) {
			t.Errorf("error %q leaks provider name %q", msg, prov)
		}
	}
}
