package store

import (
	"database/sql"
	"errors"
)

// replayReceipt decides a request whose operation ID may already hold a
// receipt. lookupErr comes from reading that receipt: ErrNotFound or
// sql.ErrNoRows means none exists and the caller runs the operation. same
// reports whether the stored receipt belongs to this request.
func replayReceipt[T any](stored T, lookupErr error, same bool, conflict error) (T, bool, error) {
	var zero T
	switch {
	case errors.Is(lookupErr, ErrNotFound), errors.Is(lookupErr, sql.ErrNoRows):
		return zero, false, nil
	case lookupErr != nil:
		return zero, false, lookupErr
	case !same:
		return zero, false, conflict
	}
	return stored, true, nil
}
