package db

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// instance_events_constraint_test.go is the drift guard for GPU-50.
//
// THE BUG IT EXISTS TO PREVENT: internal/spotrouter/watcher.go shipped writing
// event_type 'preemption_warned' / 'preemption_reclaimed' through
// db.CreateInstanceEvent while the instance_events_event_type_check value list
// had never admitted either. Every insert failed with SQLSTATE 23514, and
// because recordEvent only LOGS the error, the failure was invisible: no audit
// trail was persisted and the SSE callback published an event whose event_id /
// created_at were still zero (RETURNING never ran). Nothing in the build, the
// vet pass, or the unit tests noticed — the mismatch lived between a Go string
// literal and a SQL value list, and nothing cross-checked the two.
//
// This test IS that cross-check. It reads the constraint's value list out of
// the newest migration that defines it, extracts every event_type a
// non-test file under internal/ can hand to CreateInstanceEvent, and fails if
// any of them is not in the list. Adding a new event type therefore cannot ship
// without its migration.
//
// It runs with NO database — it is pure source analysis, so it is a fast unit
// test rather than a DATABASE_TEST_URL-gated one (see events_preemption_db_test.go
// for the real-Postgres half).

// checkConstraintName is the (v6 auto-assigned, v80/v87/v91 re-asserted) name of
// the constraint that pins instance_events.event_type.
const checkConstraintName = "instance_events_event_type_check"

// instanceEventTypeName is the Go struct whose EventType field lands in the
// constrained column.
const instanceEventTypeName = "InstanceEvent"

// addConstraintRe matches the forward `ADD CONSTRAINT ... CHECK (event_type IN
// (...))` block, tolerating the line breaks the migrations actually use.
//
// LIMITATION — only the `IN (...)` spelling is recognised, because that is the
// form every migration from v6 onward has hand-written. An equivalent constraint
// pasted out of pg_get_constraintdef, which renders it as
// `event_type = ANY (ARRAY['ready'::text, ...])`, would NOT match: the file would
// not even be counted as a candidate definition, so latestEventTypeConstraint
// would silently fall back to an older migration. Keep writing migrations in the
// IN (...) form, or teach this regexp the ARRAY spelling at the same time.
var addConstraintRe = regexp.MustCompile(
	`(?s)(?:ADD\s+)?CONSTRAINT\s+` + checkConstraintName +
		`\s+CHECK\s*\(\s*event_type\s+IN\s*\(([^)]*)\)`)

var sqlStringRe = regexp.MustCompile(`'([^']*)'`)

func TestInstanceEventTypesMatchCheckConstraint(t *testing.T) {
	root := repoRoot(t)

	allowed, srcFile := latestEventTypeConstraint(t, root)
	if len(allowed) == 0 {
		t.Fatalf("parsed an empty value list for %s out of %s", checkConstraintName, srcFile)
	}
	t.Logf("constraint value list from %s: %s", filepath.Base(srcFile), strings.Join(sortedKeys(allowed), ", "))

	used := collectEventTypeLiterals(t, filepath.Join(root, "internal"))
	if len(used) == 0 {
		t.Fatal("found no event_type literals under internal/ — the scanner is broken, not the code")
	}

	for _, lit := range sortedKeys(used) {
		if _, ok := allowed[lit]; !ok {
			t.Errorf("event_type %q is written by %s but is NOT in the %s value list (%s).\n"+
				"Add a migration widening the constraint — an unlisted type fails every INSERT "+
				"with SQLSTATE 23514, which CreateInstanceEvent's callers only log (GPU-50).",
				lit, used[lit], checkConstraintName, filepath.Base(srcFile))
		}
	}
}

// TestPreemptionEventTypesAdmitted pins the specific GPU-50 regression: if the
// v91 migration is ever reverted or rewritten without these two values, the
// spot-preemption audit trail silently disappears again.
func TestPreemptionEventTypesAdmitted(t *testing.T) {
	allowed, srcFile := latestEventTypeConstraint(t, repoRoot(t))
	for _, want := range []string{"preemption_warned", "preemption_reclaimed"} {
		if _, ok := allowed[want]; !ok {
			t.Errorf("%s must admit %q (GPU-50: spotrouter writes it on every spot preemption); "+
				"latest definition is %s", checkConstraintName, want, filepath.Base(srcFile))
		}
	}
}

// latestEventTypeConstraint returns the value list of the event_type CHECK as
// defined by the newest forward migration that (re)creates it, plus that file's
// path. "Newest" is by filename sort, which is how Pool.Migrate orders
// application, so this always reflects the constraint the database ends up with.
func latestEventTypeConstraint(t *testing.T, root string) (map[string]struct{}, string) {
	t.Helper()
	dir := filepath.Join(root, "internal", "db", "migrations")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read migrations dir: %v", err)
	}

	var candidates []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".sql") || strings.HasSuffix(name, ".down.sql") {
			continue
		}
		body, readErr := os.ReadFile(filepath.Join(dir, name))
		if readErr != nil {
			t.Fatalf("read %s: %v", name, readErr)
		}
		if addConstraintRe.Match(body) {
			candidates = append(candidates, name)
		}
	}
	if len(candidates) == 0 {
		t.Fatalf("no forward migration in %s defines %s", dir, checkConstraintName)
	}
	sort.Strings(candidates)
	newest := filepath.Join(dir, candidates[len(candidates)-1])

	body, err := os.ReadFile(newest)
	if err != nil {
		t.Fatalf("read %s: %v", newest, err)
	}
	// A migration may drop+recreate more than once; the LAST match wins,
	// mirroring the order Postgres applies the statements.
	matches := addConstraintRe.FindAllStringSubmatch(string(body), -1)
	last := matches[len(matches)-1]

	allowed := make(map[string]struct{})
	for _, m := range sqlStringRe.FindAllStringSubmatch(last[1], -1) {
		allowed[m[1]] = struct{}{}
	}
	return allowed, newest
}

// paramSink names a function parameter that flows into an InstanceEvent's
// EventType field — e.g. spotrouter's recordEvent(ctx, inst, eventType, reason).
// The literal lives at the CALL sites, so those are resolved in a second pass.
//
// pkg (the directory the sink was discovered in) scopes that second pass. Without
// it the sweep matches on bare function name across ALL of internal/, so an
// unrelated recordEvent(ctx, x, "some_reason") in another package would have its
// index-2 string mis-recorded as an event_type and fail the constraint check for
// no reason.
type paramSink struct {
	pkg string
	fn  string
	idx int
}

// collectEventTypeLiterals returns every event_type string a non-test Go file
// under dir can assign to an InstanceEvent, mapped to the position that wrote it.
//
// Test files are deliberately excluded: events_rescued_db_test.go inserts
// 'bogus' on purpose to prove the constraint still rejects unlisted values.
func collectEventTypeLiterals(t *testing.T, dir string) map[string]string {
	t.Helper()
	fset := token.NewFileSet()

	type parsedFile struct {
		path string
		pkg  string // directory, used to scope const resolution
		file *ast.File
	}
	var files []parsedFile

	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == "testdata" || d.Name() == "vendor" {
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, parseErr := parser.ParseFile(fset, path, nil, 0)
		if parseErr != nil {
			return parseErr
		}
		files = append(files, parsedFile{path: path, pkg: filepath.Dir(path), file: f})
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", dir, err)
	}

	// Pass 1 — package-level string consts, scoped by directory, so an
	// EventType written as a named constant (provision's rescueEventTypeRescued)
	// still resolves to its literal.
	consts := map[string]map[string]string{}
	for _, pf := range files {
		for _, decl := range pf.file.Decls {
			gd, ok := decl.(*ast.GenDecl)
			if !ok || gd.Tok != token.CONST {
				continue
			}
			for _, spec := range gd.Specs {
				vs, ok := spec.(*ast.ValueSpec)
				if !ok {
					continue
				}
				for i, name := range vs.Names {
					if i >= len(vs.Values) {
						continue
					}
					if s, ok := stringLit(vs.Values[i]); ok {
						if consts[pf.pkg] == nil {
							consts[pf.pkg] = map[string]string{}
						}
						consts[pf.pkg][name.Name] = s
					}
				}
			}
		}
	}

	used := map[string]string{}
	sinks := map[paramSink]bool{}

	// resolveWrite records the event_type an expression contributes: a string
	// literal directly, a named constant through the pass-1 table, or — when the
	// value is a parameter of the enclosing function — a sink for the pass-3
	// call-site sweep. Anything else is a hard failure rather than a silent skip,
	// because a value this scanner cannot follow is exactly the blind spot the
	// scanner exists to deny. Shared by every writer shape below so they all get
	// identical treatment.
	resolveWrite := func(expr ast.Expr, pkg string, enclosing *ast.FuncDecl, pos token.Position) {
		switch v := expr.(type) {
		case *ast.BasicLit:
			if s, ok := stringLit(v); ok {
				used[s] = pos.String()
				return
			}
			t.Errorf("%s: EventType set to a non-string literal", pos)
		case *ast.Ident:
			if s, ok := consts[pkg][v.Name]; ok {
				used[s] = pos.String()
				return
			}
			if enclosing != nil {
				if idx, ok := paramIndex(enclosing, v.Name); ok {
					sinks[paramSink{pkg: pkg, fn: enclosing.Name.Name, idx: idx}] = true
					return
				}
			}
			t.Errorf("%s: cannot resolve EventType identifier %q — teach this test how "+
				"to follow it, or the constraint cross-check has a blind spot", pos, v.Name)
		default:
			t.Errorf("%s: EventType set from an expression this test cannot resolve (%T) — "+
				"teach this test how to follow it, or the constraint cross-check has a blind spot",
				pos, expr)
		}
	}

	// Pass 2 — every shape that can put a value into InstanceEvent.EventType.
	// The first cut of this scanner only understood KEYED composite literals; a
	// probe proved two other shapes slipped through with no failure at all:
	//
	//	ev := &db.InstanceEvent{InstanceID: id}
	//	ev.EventType = "assigned_later"          // deferred field assignment
	//
	//	db.InstanceEvent{"", id, org, "positional", nil, ts}  // positional literal
	//
	// Both are handled below. A guard whose coverage depends on which syntax the
	// writer happened to pick is not a guard.
	for _, pf := range files {
		for _, decl := range pf.file.Decls {
			var enclosing *ast.FuncDecl
			if fd, isFunc := decl.(*ast.FuncDecl); isFunc {
				enclosing = fd
			}
			ast.Inspect(decl, func(n ast.Node) bool {
				switch node := n.(type) {
				case *ast.CompositeLit:
					if !isInstanceEventType(node.Type) {
						return true
					}
					// Positional literal. EventType is identified only by its
					// offset in the struct, so reordering the fields of
					// db.InstanceEvent would silently change which value this
					// scanner reads — and the compiler would not complain, since
					// four of the six fields are strings. Refuse the shape rather
					// than guess at the offset. Reported once for the literal, not
					// once per element.
					for _, elt := range node.Elts {
						if _, ok := elt.(*ast.KeyValueExpr); !ok {
							t.Errorf("%s: %s built with a POSITIONAL composite literal — use field names "+
								"(EventType: …) so the event_type drift scanner can see the value",
								fset.Position(node.Lbrace), instanceEventTypeName)
							return true
						}
					}
					for _, elt := range node.Elts {
						kv := elt.(*ast.KeyValueExpr)
						key, ok := kv.Key.(*ast.Ident)
						if !ok || key.Name != "EventType" {
							continue
						}
						resolveWrite(kv.Value, pf.pkg, enclosing, fset.Position(kv.Pos()))
					}
				case *ast.AssignStmt:
					// Deferred field assignment: ev.EventType = "…", which never
					// appears inside a composite literal.
					//
					// Matched on the field NAME alone — there is no type checker
					// here, so the receiver's type is unknown. If an unrelated
					// struct ever grows an EventType field written this way (the
					// webhook/SSE payloads have one, though today only as keyed
					// literals), its value lands here too and fails loudly. That
					// trade is deliberate: a false positive is one line to fix and
					// impossible to miss, whereas the miss it replaces was silent.
					for i, lhs := range node.Lhs {
						sel, ok := lhs.(*ast.SelectorExpr)
						if !ok || sel.Sel.Name != "EventType" || i >= len(node.Rhs) {
							continue
						}
						resolveWrite(node.Rhs[i], pf.pkg, enclosing, fset.Position(lhs.Pos()))
					}
				}
				return true
			})
		}
	}

	// Pass 3 — call sites feeding the parameter sinks found above.
	//
	// The sweep is SCOPED TO THE SINK'S OWN PACKAGE DIRECTORY. Matching is on the
	// bare function name (so both recordEvent(…) and w.recordEvent(…) hit), which
	// across all of internal/ would collide with any same-named helper elsewhere:
	// an unrelated recordEvent(ctx, x, "some_reason") would have its index-2
	// string recorded as an event_type and fail the constraint check for no
	// reason. Callers outside the package would have to go through an exported
	// name, and an exported EventType-feeding helper is a different (louder)
	// shape than the unexported one this resolves.
	for sink := range sinks {
		found := false
		for _, pf := range files {
			if pf.pkg != sink.pkg {
				continue
			}
			ast.Inspect(pf.file, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok || calleeName(call.Fun) != sink.fn || len(call.Args) <= sink.idx {
					return true
				}
				arg := call.Args[sink.idx]
				pos := fset.Position(arg.Pos())
				if s, ok := stringLit(arg); ok {
					used[s] = pos.String()
					found = true
					return true
				}
				if id, ok := arg.(*ast.Ident); ok {
					if s, ok := consts[pf.pkg][id.Name]; ok {
						used[s] = pos.String()
						found = true
						return true
					}
				}
				t.Errorf("%s: %s is called with an event_type this test cannot resolve statically — "+
					"teach this test how to follow it, or the constraint cross-check has a blind spot",
					pos, sink.fn)
				return true
			})
		}
		if !found {
			t.Errorf("%s (in %s) feeds an InstanceEvent.EventType but no resolvable call site was "+
				"found in its own package — the scanner is stale", sink.fn, sink.pkg)
		}
	}

	return used
}

// isInstanceEventType reports whether a composite-literal type is db.InstanceEvent
// (or bare InstanceEvent inside package db itself).
func isInstanceEventType(expr ast.Expr) bool {
	switch tv := expr.(type) {
	case *ast.Ident:
		return tv.Name == instanceEventTypeName
	case *ast.SelectorExpr:
		return tv.Sel.Name == instanceEventTypeName
	}
	return false
}

// calleeName returns the bare function name of a call target, so both
// recordEvent(...) and w.recordEvent(...) match.
func calleeName(fun ast.Expr) string {
	switch f := fun.(type) {
	case *ast.Ident:
		return f.Name
	case *ast.SelectorExpr:
		return f.Sel.Name
	}
	return ""
}

// paramIndex returns the positional index of a named parameter, flattening
// grouped declarations like (eventType, reason string).
func paramIndex(fd *ast.FuncDecl, name string) (int, bool) {
	if fd == nil || fd.Type.Params == nil {
		return 0, false
	}
	idx := 0
	for _, field := range fd.Type.Params.List {
		if len(field.Names) == 0 {
			idx++
			continue
		}
		for _, n := range field.Names {
			if n.Name == name {
				return idx, true
			}
			idx++
		}
	}
	return 0, false
}

func stringLit(expr ast.Expr) (string, bool) {
	bl, ok := expr.(*ast.BasicLit)
	if !ok || bl.Kind != token.STRING {
		return "", false
	}
	s, err := strconv.Unquote(bl.Value)
	if err != nil {
		return "", false
	}
	return s, true
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// repoRoot walks up from this source file to the directory holding go.mod.
// runtime.Caller (rather than os.Getwd) keeps it correct regardless of which
// package directory `go test` happens to run from.
func repoRoot(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed — cannot locate the repo root")
	}
	dir := filepath.Dir(thisFile)
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("could not locate go.mod walking up from %s", filepath.Dir(thisFile))
		}
		dir = parent
	}
}
