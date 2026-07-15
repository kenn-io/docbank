// Package sqlite defines the database-driver boundary used by embedded and
// standalone Docbank vaults.
package sqlite

import (
	"database/sql"
	"errors"
	"time"
)

// AccessMode controls whether an open may create or mutate its database.
type AccessMode uint8

const (
	Create AccessMode = iota + 1
	ReadWriteExisting
	ReadOnlyImmutable
)

// TransactionMode controls how database/sql transactions acquire SQLite's
// writer lock. Docbank mutations use Immediate; pinned metadata snapshots use
// Deferred so they do not hold the writer lock for the full backup capture.
type TransactionMode string

const (
	Deferred  TransactionMode = "deferred"
	Immediate TransactionMode = "immediate"
)

// Driver opens SQLite with Docbank's required foreign-key, WAL, busy-timeout,
// and transaction-lock behavior, and classifies the two error families that
// affect Docbank's convergence and bootstrap logic.
//
// Implementations must return independent *sql.DB pools from every Open call.
type Driver interface {
	Name() string
	Open(path string, opts OpenOptions) (*sql.DB, error)
	IsBusy(err error) bool
	IsUniqueViolation(err error) bool
}

// OpenOptions carries the connection semantics required by a Docbank or Kit
// operation. BusyTimeout zero selects the adapter's five-second default.
type OpenOptions struct {
	Access          AccessMode
	TransactionMode TransactionMode
	BusyTimeout     time.Duration
}

// Validate rejects a missing or incomplete driver before any vault path is
// created or locked.
func Validate(driver Driver) error {
	if driver == nil {
		return errors.New("docbank sqlite driver is required")
	}
	if driver.Name() == "" {
		return errors.New("docbank sqlite driver name is required")
	}
	return nil
}
