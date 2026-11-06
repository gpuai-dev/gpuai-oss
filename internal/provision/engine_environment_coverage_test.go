// Certified-manifest coverage: every GPUType the platform can place must
// resolve in the embedded manifest via the engine's underscore→space key
// translation (resolveEnvironment). A miss is invisible at build time but
// fatal at launch time — every dashboard (certified-default) launch of that
// GPU 500s with ErrCertifiedUnavailable, which is exactly how the RTX PRO
// 6000/4500 "Blackwell"-suffixed manifest keys shipped broken.
package provision

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strconv"
	"strings"
	"testing"

	"github.com/gpuai-dev/gpuai-oss/internal/manifest"
)

// gpuTypeConstValues parses the provider package's types.go and returns the
// string value of every GPUType constant declared there (same approach as
// provider's TestVRAMByGPUTypeCoversAllGPUTypes), so this test can't drift
// when a new GPU type is added.
func gpuTypeConstValues(t *testing.T) []string {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "../provider/types.go", nil, 0)
	if err != nil {
		t.Fatalf("parse ../provider/types.go: %v", err)
	}
	var out []string
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
				out = append(out, s)
			}
		}
	}
	if len(out) == 0 {
		t.Fatal("found no GPUType constants in ../provider/types.go")
	}
	return out
}

func TestEmbeddedManifestCoversAllGPUTypes(t *testing.T) {
	m, err := manifest.LoadEmbedded()
	if err != nil {
		t.Fatalf("LoadEmbedded: %v", err)
	}
	for _, gt := range gpuTypeConstValues(t) {
		key := strings.ReplaceAll(gt, "_", " ")
		if _, ok := m.Lookup(key); !ok {
			t.Errorf("manifest.Lookup(%q) missed for GPUType %q — certified launches of this GPU will always fail; the manifest key must normalize to exactly %q", key, gt, key)
		}
	}
}
