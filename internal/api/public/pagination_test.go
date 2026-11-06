package public

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestParsePageParams_Defaults(t *testing.T) {
	req := httptest.NewRequest("GET", "/v1/instances", nil)
	p := ParsePageParams(req)
	if p.Limit != 50 {
		t.Errorf("default limit: got %d, want 50", p.Limit)
	}
	if p.Cursor != "" {
		t.Errorf("default cursor: got %q, want empty", p.Cursor)
	}
}

func TestParsePageParams_ClampMax(t *testing.T) {
	req := httptest.NewRequest("GET", "/v1/instances?limit=500", nil)
	p := ParsePageParams(req)
	if p.Limit != 200 {
		t.Errorf("clamp max: got %d, want 200", p.Limit)
	}
}

func TestParsePageParams_ClampMin(t *testing.T) {
	req := httptest.NewRequest("GET", "/v1/instances?limit=0", nil)
	p := ParsePageParams(req)
	if p.Limit != 1 {
		t.Errorf("clamp min: got %d, want 1", p.Limit)
	}
}

func TestParsePageParams_CursorPassthrough(t *testing.T) {
	req := httptest.NewRequest("GET", "/v1/instances?cursor=abc&limit=75", nil)
	p := ParsePageParams(req)
	if p.Cursor != "abc" {
		t.Errorf("cursor: got %q, want abc", p.Cursor)
	}
	if p.Limit != 75 {
		t.Errorf("limit: got %d, want 75", p.Limit)
	}
}

func TestEncodeDecodeCursor_RoundTrip(t *testing.T) {
	original := time.Date(2026, 5, 7, 12, 34, 56, 0, time.UTC)
	c := EncodeCursor(original, "inst-abc123")
	ts, id, err := DecodeCursor(c)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !ts.Equal(original) {
		t.Errorf("ts: got %v, want %v", ts, original)
	}
	if id != "inst-abc123" {
		t.Errorf("id: got %q", id)
	}
}

func TestDecodeCursor_Invalid(t *testing.T) {
	// Build payloads that exercise each parse-failure branch.
	cases := []string{
		"",                                       // empty cursor
		"not-base64!!!",                          // invalid base64url
		base64URLEncode("malformed"),             // valid base64url, no | separator
		base64URLEncode("nopipe-here"),           // valid base64url, no | separator (variant)
		base64URLEncode("not-rfc3339|id"),        // valid base64url + pipe but bad timestamp
		base64URLEncode("2026-05-07T12:34:56Z|"), // valid timestamp, empty id
	}
	for _, c := range cases {
		_, _, err := DecodeCursor(c)
		if err == nil {
			t.Errorf("DecodeCursor(%q): want error, got nil", c)
			continue
		}
		if !errors.Is(err, ErrInvalidCursor) {
			t.Errorf("DecodeCursor(%q): error must wrap ErrInvalidCursor, got %v", c, err)
		}
	}
}

func TestBuildPage_NoCursorWhenUnderLimit(t *testing.T) {
	rows := []row{{"id-1", time.Now()}, {"id-2", time.Now()}}
	page := BuildPage(rows, 10, func(r row) (time.Time, string) { return r.t, r.id })
	if page.NextCursor != nil {
		t.Errorf("NextCursor: got %v, want nil", page.NextCursor)
	}
	if len(page.Data) != 2 {
		t.Errorf("Data len: got %d, want 2", len(page.Data))
	}
}

func TestBuildPage_NoCursorWhenExactlyAtLimit(t *testing.T) {
	// limit=2, rows=2 → exactly at limit, no cursor. Callers fetch limit+1
	// to detect more pages; rows == limit means no next page.
	rows := []row{{"id-1", time.Now()}, {"id-2", time.Now()}}
	page := BuildPage(rows, 2, func(r row) (time.Time, string) { return r.t, r.id })
	if page.NextCursor != nil {
		t.Errorf("NextCursor: got %v, want nil", page.NextCursor)
	}
	if len(page.Data) != 2 {
		t.Errorf("Data len: got %d, want 2", len(page.Data))
	}
}

func TestBuildPage_CursorWhenOverLimit(t *testing.T) {
	// limit=2, rows=3 → keep first 2, emit cursor for row at index 1 (the cut-off row).
	rows := []row{
		{"id-1", time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)},
		{"id-2", time.Date(2026, 5, 2, 0, 0, 0, 0, time.UTC)},
		{"id-3", time.Date(2026, 5, 3, 0, 0, 0, 0, time.UTC)},
	}
	page := BuildPage(rows, 2, func(r row) (time.Time, string) { return r.t, r.id })
	if page.NextCursor == nil {
		t.Fatal("NextCursor: got nil, want non-nil")
	}
	ts, id, err := DecodeCursor(*page.NextCursor)
	if err != nil {
		t.Fatalf("decode emitted cursor: %v", err)
	}
	if id != "id-2" {
		t.Errorf("cut-off id: got %q, want id-2", id)
	}
	if !ts.Equal(rows[1].t) {
		t.Errorf("cut-off ts: got %v, want %v", ts, rows[1].t)
	}
	if len(page.Data) != 2 {
		t.Errorf("Data len: got %d, want 2", len(page.Data))
	}
}

func TestPageEnvelope_JSONNullsCursor(t *testing.T) {
	p := Page[row]{Data: []row{}, NextCursor: nil}
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	s := string(b)
	if !strings.Contains(s, `"next_cursor":null`) {
		t.Errorf("expected explicit next_cursor:null, got %s", s)
	}
	if !strings.Contains(s, `"data":[]`) {
		t.Errorf("expected data:[], got %s", s)
	}
}

func TestPageEnvelope_JSONNonNullCursor(t *testing.T) {
	cursor := "some-cursor-string"
	p := Page[row]{Data: []row{}, NextCursor: &cursor}
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	s := string(b)
	if !strings.Contains(s, `"next_cursor":"some-cursor-string"`) {
		t.Errorf("expected next_cursor:string, got %s", s)
	}
}

// --- test helpers ---

type row struct {
	id string
	t  time.Time
}

// base64URLEncode wraps stdlib base64.RawURLEncoding for negative-test
// payload construction. Test-file imports do not affect the production
// banned-imports test (which scans only non-_test.go files).
func base64URLEncode(s string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(s))
}
