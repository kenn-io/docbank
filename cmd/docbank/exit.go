package main

import (
	"errors"

	"go.kenn.io/kit/backup"
	"go.kenn.io/kit/packstore"

	"go.kenn.io/docbank/internal/client"
	"go.kenn.io/docbank/internal/home"
	"go.kenn.io/docbank/internal/store"
)

const (
	exitSuccess   = 0
	exitGeneral   = 1
	exitUsage     = 2
	exitNotFound  = 3
	exitStale     = 4
	exitBusy      = 5
	exitIntegrity = 6
)

type exitError struct {
	code int
	err  error
}

func (e *exitError) Error() string { return e.err.Error() }
func (e *exitError) Unwrap() error { return e.err }

func usageError(err error) error {
	if err == nil {
		return nil
	}
	return &exitError{code: exitUsage, err: err}
}

func integrityError(err error) error {
	if err == nil {
		return nil
	}
	return &exitError{code: exitIntegrity, err: err}
}

func commandExitCode(err error, started bool) int {
	if err == nil {
		return exitSuccess
	}
	var classified *exitError
	if errors.As(err, &classified) {
		return classified.code
	}
	if errors.Is(err, client.ErrIntegrity) {
		return exitIntegrity
	}
	if errors.Is(err, store.ErrNotFound) {
		return exitNotFound
	}
	if errors.Is(err, store.ErrStaleRevision) || errors.Is(err, store.ErrAuditPreviewStale) {
		return exitStale
	}
	if errors.Is(err, home.ErrVaultLocked) || errors.Is(err, backup.ErrRepoLocked) ||
		errors.Is(err, packstore.ErrPackRetirementDeferred) {
		return exitBusy
	}
	if errors.Is(err, store.ErrInvalidName) || errors.Is(err, store.ErrInvalidTag) ||
		errors.Is(err, store.ErrInvalidBatchMove) ||
		errors.Is(err, store.ErrInvalidVersionPrune) ||
		errors.Is(err, store.ErrInvalidAuditCursor) {
		return exitUsage
	}
	if code, ok := client.ProblemCode(err); ok &&
		(code == "validation" || code == "audit_acknowledgment_required") {
		return exitUsage
	}
	if !started {
		return exitUsage
	}
	return exitGeneral
}
