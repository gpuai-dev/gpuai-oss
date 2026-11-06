package public

import (
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Pagination defaults are higher than the dashboard's 20/100 because the
// public API targets programmatic / SDK consumers who page in larger
// batches. Per D-13 (CONTEXT.md): default 50, max 200.
const (
	// DefaultPageSize is returned when ?limit is absent.
	DefaultPageSize = 50

	// MaxPageSize is the upper bound; ?limit values above this are clamped.
	MaxPageSize = 200
)

// ErrInvalidCursor is returned when DecodeCursor cannot parse the input.
// Handlers map this to writeProblem(w, r, 400, "validation_failed", ...).
var ErrInvalidCursor = errors.New("invalid cursor")

// PageParams holds parsed pagination parameters extracted from a request.
type PageParams struct {
	Cursor string
	Limit  int
}

// Page is the public-API list response envelope. NextCursor is *string so
// the field can JSON-emit `null` (no more pages) rather than be omitted.
// Per D-13: response shape `{data: [...], next_cursor: "..."|null}`.
type Page[T any] struct {
	Data       []T     `json:"data"`
	NextCursor *string `json:"next_cursor"`
}

// ParsePageParams extracts cursor + limit from the URL query string.
// Limit clamps to [1, MaxPageSize] with default DefaultPageSize.
// Parse failures on `limit` are silently coerced to default — matches
// the dashboard's permissive behavior so a junk `?limit=abc` does not
// 400 the request.
func ParsePageParams(r *http.Request) PageParams {
	cursor := r.URL.Query().Get("cursor")
	limit := DefaultPageSize
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			limit = n
		}
	}
	if limit < 1 {
		limit = 1
	}
	if limit > MaxPageSize {
		limit = MaxPageSize
	}
	return PageParams{Cursor: cursor, Limit: limit}
}

// EncodeCursor encodes a (createdAt, id) tuple into a URL-safe base64 cursor.
// Format: base64url(RFC3339 + "|" + id), no padding. Time is normalized to
// UTC so cursors are byte-identical regardless of caller-side timezone.
func EncodeCursor(createdAt time.Time, id string) string {
	raw := createdAt.UTC().Format(time.RFC3339) + "|" + id
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

// DecodeCursor parses a cursor produced by EncodeCursor. Every error path
// wraps ErrInvalidCursor so handlers can branch on errors.Is(err, ErrInvalidCursor)
// to emit a 400 validation_failed RFC-7807 problem.
func DecodeCursor(cursor string) (time.Time, string, error) {
	if cursor == "" {
		return time.Time{}, "", fmt.Errorf("%w: empty", ErrInvalidCursor)
	}
	decoded, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return time.Time{}, "", fmt.Errorf("%w: %v", ErrInvalidCursor, err)
	}
	parts := strings.SplitN(string(decoded), "|", 2)
	if len(parts) != 2 || parts[1] == "" {
		return time.Time{}, "", fmt.Errorf("%w: malformed", ErrInvalidCursor)
	}
	ts, err := time.Parse(time.RFC3339, parts[0])
	if err != nil {
		return time.Time{}, "", fmt.Errorf("%w: timestamp: %v", ErrInvalidCursor, err)
	}
	return ts, parts[1], nil
}

// BuildPage assembles a Page envelope. The caller is expected to fetch
// limit+1 rows from the database; if the result is exactly limit+1 or
// more, BuildPage trims to limit and emits a NextCursor pointing at
// the cut-off row's (createdAt, id). The encoder closure abstracts
// type-specific id/created accessors so handlers can use this with any
// concrete row type.
//
// Usage:
//
//	rows := db.ListXFor(ctx, orgID, params.Limit+1, ...)
//	page := BuildPage(rows, params.Limit, func(r MyRow) (time.Time, string) {
//	    return r.CreatedAt, r.ID
//	})
func BuildPage[T any](rows []T, limit int, encoder func(T) (time.Time, string)) Page[T] {
	if len(rows) <= limit {
		return Page[T]{Data: rows, NextCursor: nil}
	}
	cut := rows[limit-1]
	ts, id := encoder(cut)
	next := EncodeCursor(ts, id)
	return Page[T]{Data: rows[:limit], NextCursor: &next}
}
