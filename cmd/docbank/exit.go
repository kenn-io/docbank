package main

import (
	"errors"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"go.kenn.io/kit/backup"
	"go.kenn.io/kit/packstore"

	"go.kenn.io/docbank/internal/daemonconn"
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
	if classified, ok := errors.AsType[*exitError](err); ok {
		return classified.code
	}
	if errors.Is(err, daemonconn.ErrIntegrity) {
		return exitIntegrity
	}
	if errors.Is(err, store.ErrNotFound) {
		return exitNotFound
	}
	if errors.Is(err, store.ErrStaleRevision) ||
		errors.Is(err, store.ErrAuditPreviewStale) {
		return exitStale
	}
	if errors.Is(err, home.ErrVaultLocked) || errors.Is(err, backup.ErrRepoLocked) ||
		errors.Is(err, packstore.ErrPackRetirementDeferred) ||
		errors.Is(err, daemonconn.ErrMaintenanceBusy) {
		return exitBusy
	}
	if errors.Is(err, store.ErrInvalidName) || errors.Is(err, store.ErrInvalidTag) ||
		errors.Is(err, store.ErrInvalidPerson) ||
		errors.Is(err, store.ErrSearchQueryRequired) ||
		errors.Is(err, store.ErrInvalidBatchMove) ||
		errors.Is(err, store.ErrInvalidVersionPrune) ||
		errors.Is(err, store.ErrInvalidAuditCursor) || errors.Is(err, store.ErrInvalidPhotoAsset) ||
		errors.Is(err, store.ErrPhotoNodeNotEligible) {
		return exitUsage
	}
	if code, ok := daemonconn.ProblemCode(err); ok &&
		(code == "validation" || code == "audit_acknowledgment_required") {
		return exitUsage
	}
	if !started {
		return exitUsage
	}
	return exitGeneral
}

// hintedError attaches a next step where the CLI understands the failure.
// runProcess prints it after the error, so the error text itself is unchanged.
type hintedError struct {
	err  error
	hint string
}

func (e *hintedError) Error() string { return e.err.Error() }
func (e *hintedError) Unwrap() error { return e.err }

func withHint(err error, hint string) error {
	return &hintedError{err: err, hint: hint}
}

// Hints belong to the CLI process boundary, not the daemon's problem contract.
func commandErrorHint(cmd *cobra.Command, err error, code int, started bool) string {
	if err == nil || strings.Contains(err.Error(), "Did you mean") {
		return ""
	}
	if hinted, ok := errors.AsType[*hintedError](err); ok {
		return "hint: " + hinted.hint
	}
	switch code {
	case exitUsage:
		if started {
			return ""
		}
		if cmd == nil || cmd == rootCmd {
			return `hint: run "docbank --help"`
		}
		usage := cmd.UseLine()
		if len(usage) <= 120 && !strings.ContainsAny(usage, "\n\r") {
			return "hint: usage: " + usage
		}
		return fmt.Sprintf("hint: run %q", cmd.CommandPath()+" --help")
	case exitBusy:
		// A vault lock can block the daemon itself, so "docbank jobs" would
		// fail the same way; that error already names the likely holder.
		switch {
		case errors.Is(err, backup.ErrRepoLocked):
			return `hint: wait for the backup repository owner; use --force-unlock only when its owner is known to be gone`
		case errors.Is(err, daemonconn.ErrMaintenanceBusy):
			return `hint: wait and retry; "docbank jobs" shows active work`
		default:
			return ""
		}
	default:
		return ""
	}
}
