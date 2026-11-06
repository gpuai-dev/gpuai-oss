package public_test

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestNoInternalAPIImports walks every .go file under internal/api/public/
// (including subdirectories) and asserts that none of them import the
// dashboard package github.com/gpuai-dev/gpuai-oss/internal/api. The boundary
// protects against Pitfall 1 in 18-RESEARCH.md: leaking Clerk-session-
// shaped types into the public-API surface.
//
// The test is package `public_test` (external) so it itself cannot
// accidentally pull internal/api into the public package's import graph.
func TestNoInternalAPIImports(t *testing.T) {
	const banned = "github.com/gpuai-dev/gpuai-oss/internal/api"
	// Resolve the package directory relative to this test file.
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	var offenders []string
	err = filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		if strings.HasSuffix(path, "_test.go") {
			// Tests can import internal/api freely (none currently do; just be permissive).
			return nil
		}
		fset := token.NewFileSet()
		f, perr := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if perr != nil {
			t.Fatalf("parse %s: %v", path, perr)
		}
		for _, imp := range f.Imports {
			v, err := strconv.Unquote(imp.Path.Value)
			if err != nil {
				continue
			}
			// EXACT match — the banned package, not subpackages like internal/api/public itself.
			if v == banned {
				offenders = append(offenders, path+" imports "+v)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	if len(offenders) > 0 {
		t.Fatalf("internal/api/public/ files MUST NOT import %q. Offenders:\n  %s",
			banned, strings.Join(offenders, "\n  "))
	}
	// Sanity-check the walker actually saw files (so an empty pass doesn't
	// mask a misconfigured test).
	var fileCount int
	filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() && strings.HasSuffix(path, ".go") && !strings.HasSuffix(path, "_test.go") {
			fileCount++
		}
		return nil
	})
	if fileCount == 0 {
		t.Fatalf("walker saw 0 production .go files in %s — test setup is broken", dir)
	}
}
