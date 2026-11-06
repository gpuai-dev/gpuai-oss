package db

import (
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// The 22P02 mapping is what turns a malformed uuid path id into a 404 instead
// of a 500 (GPU-65). These tests pin the predicate's boundaries: ErrNoRows and
// invalid_text_representation are "no such row"; every other error — including
// other PgError codes — must keep propagating so real failures still 500.
func TestIsNoRowsOrInvalidID(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"pgx.ErrNoRows", pgx.ErrNoRows, true},
		{"wrapped ErrNoRows", fmt.Errorf("scan: %w", pgx.ErrNoRows), true},
		{"22P02 invalid text representation", &pgconn.PgError{Code: "22P02"}, true},
		{"wrapped 22P02", fmt.Errorf("query: %w", &pgconn.PgError{Code: "22P02"}), true},
		{"other pg error (23505 unique violation)", &pgconn.PgError{Code: "23505"}, false},
		{"plain error", errors.New("connection reset"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isNoRowsOrInvalidID(tc.err); got != tc.want {
				t.Errorf("isNoRowsOrInvalidID(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

func TestIsInvalidIDExec(t *testing.T) {
	if !isInvalidIDExec(&pgconn.PgError{Code: "22P02"}) {
		t.Error("isInvalidIDExec must be true for SQLSTATE 22P02")
	}
	// Exec has no ErrNoRows; the predicate must not treat it as invalid-id.
	if isInvalidIDExec(pgx.ErrNoRows) {
		t.Error("isInvalidIDExec must be false for pgx.ErrNoRows")
	}
	if isInvalidIDExec(nil) || isInvalidIDExec(errors.New("boom")) {
		t.Error("isInvalidIDExec must be false for nil / unrelated errors")
	}
}
