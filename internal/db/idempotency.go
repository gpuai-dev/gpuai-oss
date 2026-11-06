package db

import (
	"errors"
)

// ErrIdempotencyKeyExists is returned when an idempotency key already exists
// for the given organization.
var ErrIdempotencyKeyExists = errors.New("idempotency key already exists")

// isDuplicateKeyError checks if the error is a PostgreSQL unique violation (SQLSTATE 23505).
func isDuplicateKeyError(err error) bool {
	// pgx wraps PostgreSQL errors with a Code() method.
	// We check for the unique_violation error code (23505).
	var pgErr interface{ SQLState() string }
	if errors.As(err, &pgErr) {
		return pgErr.SQLState() == "23505"
	}
	return false
}
