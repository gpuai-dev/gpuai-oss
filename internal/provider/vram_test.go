package provider

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strconv"
	"testing"
)

// gpuTypeConstValues parses types.go and returns the string value of every
// GPUType constant declared there, so the coverage test below can't drift when
// a new GPU type is added.
func gpuTypeConstValues(t *testing.T) []GPUType {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "types.go", nil, 0)
	if err != nil {
		t.Fatalf("parse types.go: %v", err)
	}
	var out []GPUType
	for _, decl := range f.Decls {
		gd, ok := decl.(*ast.GenDecl)
		if !ok || gd.Tok != token.CONST {
			continue
		}
		for _, spec := range gd.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			ident, ok := vs.Type.(*ast.Ident)
			if !ok || ident.Name != "GPUType" {
				continue
			}
			for _, v := range vs.Values {
				lit, ok := v.(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					continue
				}
				s, err := strconv.Unquote(lit.Value)
				if err != nil {
					t.Fatalf("unquote %s: %v", lit.Value, err)
				}
				out = append(out, GPUType(s))
			}
		}
	}
	if len(out) == 0 {
		t.Fatal("found no GPUType constants in types.go")
	}
	return out
}

// Every declared GPU type must have a nonzero canonical VRAM value — a missing
// row surfaces to customers as "vram_gb: 0" in /v1/gpu-types.
func TestVRAMByGPUTypeCoversAllGPUTypes(t *testing.T) {
	for _, gt := range gpuTypeConstValues(t) {
		if v := VRAMByGPUType(gt); v <= 0 {
			t.Errorf("VRAMByGPUType(%q) = %d, want > 0 — add it to vramByGPUType in vram.go", gt, v)
		}
	}
}

func TestVRAMByGPUTypeKnownValues(t *testing.T) {
	cases := map[GPUType]int{
		GPUTypeH100NVL:    94, // 94 GB per GPU (188 GB for the NVL pair)
		GPUTypeRTXPro6000: 96, // RTX PRO 6000 Blackwell
		GPUTypeH100SXM:    80,
		GPUTypeH200SXM:    141,
		GPUTypeB200:       192,
	}
	for gt, want := range cases {
		if got := VRAMByGPUType(gt); got != want {
			t.Errorf("VRAMByGPUType(%q) = %d, want %d", gt, got, want)
		}
	}
	if got := VRAMByGPUType(GPUType("not_a_gpu")); got != 0 {
		t.Errorf("VRAMByGPUType(unknown) = %d, want 0", got)
	}
}

func TestNormalizeVRAM(t *testing.T) {
	cases := []struct {
		name     string
		gpuType  GPUType
		reported int
		want     int
	}{
		// Measurement noise (nvidia-smi MiB→GiB, net of ECC) → nameplate.
		// These are the exact figures prod advertised on 2026-08-13.
		{"b200 measured", GPUTypeB200, 179, 192},
		{"h200 measured", GPUTypeH200SXM, 140, 141},
		{"l40s measured", GPUTypeL40S, 45, 48},
		{"l4 measured", GPUTypeL4, 22, 24},
		{"a4000 truncated", GPUTypeRTXA4000, 15, 16},

		// A materially smaller variant sharing one upstream name is what the
		// customer actually receives — report it, don't inflate to nameplate.
		{"v100 32gb variant", GPUTypeV100, 32, 32},
		{"a100 40gb under 80gb type", GPUTypeA10080GB, 40, 40},

		// Boundary, where 0.9*canonical is an integer so >= is actually
		// exercised: a100_40gb floor is exactly 36, rtx3080's exactly 9.
		{"exactly on floor lifts", GPUTypeA10040GB, 36, 40},
		{"one below floor does not", GPUTypeA10040GB, 35, 35},
		{"exactly on floor lifts (small)", GPUTypeRTX3080, 9, 10},
		{"one below floor does not (small)", GPUTypeRTX3080, 8, 8},

		// Over-delivery is never a mislabel; absent reports fall back.
		{"above canonical", GPUTypeL4, 26, 26},
		{"zero reported", GPUTypeL4, 0, 24},
		{"negative reported", GPUTypeL4, -1, 24},
		{"exact nameplate", GPUTypeH100SXM, 80, 80},

		// A type absent from the canonical table passes through untouched.
		{"unknown type", GPUType("not_a_gpu"), 37, 37},
		{"unknown type zero", GPUType("not_a_gpu"), 0, 0},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			if got := NormalizeVRAM(tt.gpuType, tt.reported); got != tt.want {
				t.Errorf("NormalizeVRAM(%q, %d) = %d, want %d", tt.gpuType, tt.reported, got, tt.want)
			}
		})
	}
}

// GPU-87: the suggestion list behind the "compatible GPU types" escape hatch in
// the template-floor rejection. A customer told only "rtx_a4000 is too small"
// has to guess the next type; naming the qualifying types ends the retry loop.
func TestGPUTypesWithVRAMAtLeast(t *testing.T) {
	got := GPUTypesWithVRAMAtLeast(24)
	inList := func(want GPUType) bool {
		for _, gt := range got {
			if gt == want {
				return true
			}
		}
		return false
	}
	for _, want := range []GPUType{GPUTypeL4, GPUTypeA30, GPUTypeRTX3090, GPUTypeRTX4090, GPUTypeRTXA5000} {
		if !inList(want) {
			t.Errorf("GPUTypesWithVRAMAtLeast(24) missing %q (canonical %d GB)", want, VRAMByGPUType(want))
		}
	}
	for _, unwanted := range []GPUType{GPUTypeRTXA4000, GPUTypeV100, GPUTypeT4} {
		if inList(unwanted) {
			t.Errorf("GPUTypesWithVRAMAtLeast(24) includes %q (canonical %d GB) — below the floor",
				unwanted, VRAMByGPUType(unwanted))
		}
	}

	// Ordering is ascending canonical VRAM, ties broken by name ascending, so
	// the suggestion list leads with the cheapest-class GPU that qualifies
	// rather than an arbitrary map-iteration order. The >= 141 GB tier is the
	// smallest slice that exercises BOTH rules: two 141 GB types (name tie) then
	// 192 then 288.
	top := GPUTypesWithVRAMAtLeast(141)
	wantTop := []GPUType{GPUTypeH200NVL, GPUTypeH200SXM, GPUTypeB200, GPUTypeB300}
	if len(top) != len(wantTop) {
		t.Fatalf("GPUTypesWithVRAMAtLeast(141) = %v, want %v", top, wantTop)
	}
	for i := range wantTop {
		if top[i] != wantTop[i] {
			t.Fatalf("GPUTypesWithVRAMAtLeast(141) = %v, want %v (VRAM asc, then name asc)", top, wantTop)
		}
	}

	if none := GPUTypesWithVRAMAtLeast(999); len(none) != 0 {
		t.Errorf("GPUTypesWithVRAMAtLeast(999) = %v, want empty — nothing we sell is that large", none)
	}
}

// TestNormalizeVRAMNeverInflatesAcrossCapacitySplits pins the property the
// noise floor exists to protect: for every pair of GPU types we sell that are
// the same card at different capacities, the smaller capacity must never be
// lifted to the larger one's nameplate.
func TestNormalizeVRAMNeverInflatesAcrossCapacitySplits(t *testing.T) {
	splits := []struct {
		larger  GPUType
		smaller int
	}{
		{GPUTypeA10080GB, 40}, // A100 40GB reported under the 80GB type
		{GPUTypeV100, 8},      // hypothetical half-capacity V100
		{GPUTypeH200SXM, 80},  // an H100-class report under the H200 type
	}
	for _, s := range splits {
		if got := NormalizeVRAM(s.larger, s.smaller); got != s.smaller {
			t.Errorf("NormalizeVRAM(%q, %d) = %d, want %d — a capacity split must never inflate",
				s.larger, s.smaller, got, s.smaller)
		}
	}
}
