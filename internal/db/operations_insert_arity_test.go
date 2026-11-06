package db

import (
	"regexp"
	"strings"
	"testing"
)

// TestOperationInsertSQLArity pins the column/expression parity of
// operationInsertSQL.
//
// This exists because of a real merge defect: phase 86 added the
// caller-supplied `operation_id` column (plan 86-20) and separately changed
// the api_key_id binding to allow NULL (plan 86-22). Merged, the column list
// grew to six while the VALUES list still held five expressions, so `kind`
// silently lost its placeholder and every CreateOperation raised
//
//	INSERT has more target columns than expressions (SQLSTATE 42601)
//
// CreateOperation is on the create path for EVERY instance, not just
// auto-launch, so that is a total outage of instance creation.
//
// It reached CI because the only tests that execute this statement are
// DATABASE_TEST_URL-gated and skip on a laptop: a full local `go test ./...`
// passed with 89 green packages while the statement was unrunnable. SQL arity
// is otherwise only checkable by a live Postgres. This test makes it checkable
// by anyone, anywhere, in milliseconds.
func TestOperationInsertSQLArity(t *testing.T) {
	cols, vals := parseInsertColumnsAndValues(t, operationInsertSQL)

	if len(cols) != len(vals) {
		t.Fatalf("operations INSERT arity mismatch: %d target columns %v but %d expressions %v",
			len(cols), cols, len(vals), vals)
	}

	// Every non-literal expression must be a distinct placeholder, numbered
	// contiguously from $1 — an unreferenced arg means a column is receiving a
	// value the caller never intended (which is exactly how `kind` came to be
	// fed the literal 'pending').
	seen := map[string]bool{}
	maxN := 0
	for _, v := range vals {
		for _, m := range regexp.MustCompile(`\$(\d+)`).FindAllStringSubmatch(v, -1) {
			seen["$"+m[1]] = true
			if n := len(m[1]); n > 0 {
				if x := atoi(m[1]); x > maxN {
					maxN = x
				}
			}
		}
	}
	for i := 1; i <= maxN; i++ {
		if !seen["$"+itoa(i)] {
			t.Errorf("placeholder $%d is never referenced in the VALUES list — an argument is being dropped", i)
		}
	}
}

func atoi(s string) int {
	n := 0
	for _, r := range s {
		n = n*10 + int(r-'0')
	}
	return n
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

// parseInsertColumnsAndValues splits an INSERT's column list and VALUES list,
// respecting nested parentheses so a function call such as
// COALESCE($1::uuid, gen_random_uuid()) counts as ONE expression rather than two.
func parseInsertColumnsAndValues(t *testing.T, sql string) (cols, vals []string) {
	t.Helper()

	colStart := strings.Index(sql, "(")
	colEnd := strings.Index(sql, ")")
	if colStart < 0 || colEnd < colStart {
		t.Fatalf("could not locate the column list in: %s", sql)
	}
	for _, c := range strings.Split(sql[colStart+1:colEnd], ",") {
		cols = append(cols, strings.TrimSpace(c))
	}

	vi := strings.Index(sql, "VALUES")
	if vi < 0 {
		t.Fatalf("could not locate VALUES in: %s", sql)
	}
	rest := sql[vi+len("VALUES"):]
	open := strings.Index(rest, "(")
	if open < 0 {
		t.Fatalf("could not locate the VALUES tuple in: %s", sql)
	}
	depth, cur := 0, strings.Builder{}
	for i := open; i < len(rest); i++ {
		ch := rest[i]
		switch ch {
		case '(':
			depth++
			if depth == 1 {
				continue // skip the tuple's own opening paren
			}
		case ')':
			depth--
			if depth == 0 {
				if s := strings.TrimSpace(cur.String()); s != "" {
					vals = append(vals, s)
				}
				return cols, vals
			}
		case ',':
			if depth == 1 {
				vals = append(vals, strings.TrimSpace(cur.String()))
				cur.Reset()
				continue
			}
		}
		cur.WriteByte(ch)
	}
	t.Fatalf("unterminated VALUES tuple in: %s", sql)
	return nil, nil
}
