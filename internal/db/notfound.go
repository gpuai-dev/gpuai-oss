package db

import (
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// isNoRowsOrInvalidID reports whether err means "this id matches no row":
// either pgx.ErrNoRows, or Postgres SQLSTATE 22P02 (invalid_text_representation)
// — the cast failure produced when a malformed id is bound against a uuid-typed
// column. A malformed id can never match a row, so the two cases are the same
// answer to the caller, and mapping both to the method's not-found contract
// keeps existence-hiding intact. Without this, every uuid-keyed public route
// surfaced a malformed path id as HTTP 500 (GPU-65).
func isNoRowsOrInvalidID(err error) bool {
	if errors.Is(err, pgx.ErrNoRows) {
		return true
	}
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "22P02"
}

// isInvalidIDExec is the Exec-shaped sibling of isNoRowsOrInvalidID: Exec never
// returns pgx.ErrNoRows, so a malformed uuid surfaces only as SQLSTATE 22P02.
// Callers treat it exactly like RowsAffected()==0 — the id matched nothing.
func isInvalidIDExec(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "22P02"
}
