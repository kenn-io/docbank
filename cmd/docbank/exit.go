package main

import (
	"errors"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"go.kenn.io/kit/backup"
	"go.kenn.io/kit/packstore"

	"go.kenn.io/docbank/internal/backupapp"
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
	exitClaim     = 7
)

const searchQueryHint = `pass words, or filter with --tag, --modified-since or --modified-before; see "docbank search --help"`

func missingBackupRepository(err error) bool {
	return errors.Is(err, daemonconn.ErrBackupRepositoryNotConfigured)
}

// Remove generated transport wrappers by following their error identities.
// Matching suffixes preserves user text (including a path containing the same
// words) and leaves the original error chain available for classification.
func cliErrorMessage(err error) string {
	message := err.Error()
	for cause := err; cause != nil; cause = errors.Unwrap(cause) {
		wrapped := errors.Unwrap(cause)
		if wrapped != nil && cause.Error() == "error executing request: "+wrapped.Error() {
			if before, ok := strings.CutSuffix(message, cause.Error()); ok {
				message = before + wrapped.Error()
			}
		}
	}
	if missingBackupRepository(err) {
		message = strings.Replace(message, "pass repo or set [backup]", "pass --repo <dir> or set [backup]", 1)
	}
	return message
}

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
		errors.Is(err, store.ErrAuditPreviewStale) || errors.Is(err, backupapp.ErrRepositoryClaimChanged) {
		return exitStale
	}
	if errors.Is(err, backupapp.ErrRepositoryClaimRequired) {
		return exitClaim
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
		errors.Is(err, store.ErrPhotoNodeNotEligible) ||
		errors.Is(err, backupapp.ErrRepositoryOwnerConflict) || missingBackupRepository(err) {
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
	if err == nil {
		return ""
	}
	if hinted, ok := errors.AsType[*hintedError](err); ok {
		return "hint: " + hinted.hint
	}
	if !started && strings.Contains(err.Error(), "\nDid you mean") {
		return ""
	}
	path := "docbank"
	if cmd != nil {
		path = cmd.CommandPath()
	}
	help := fmt.Sprintf("run %q for flags and examples", path+" --help")
	if problem, _ := daemonconn.ProblemCode(err); problem == "backup_repository" {
		return `hint: check --repo and repository permissions, or set [backup] repo in $DOCBANK_HOME/config.toml; initialize a new repository with "docbank backup init --repo <dir>"`
	}
	if errors.Is(err, daemonconn.ErrDaemonConfigInvalid) {
		return `hint: fix $DOCBANK_HOME/config.toml or its startup environment overrides, then retry; see "docbank daemon start --help"`
	}
	if errors.Is(err, store.ErrSearchQueryRequired) {
		return "hint: " + searchQueryHint
	}
	if errors.Is(err, daemonconn.ErrProcessingUnavailable) {
		return `hint: inspect "docbank processing profiles" and configure an available profile before planning again`
	}
	if (cmd == processingBuildCmd || cmd == searchCmd) &&
		(errors.Is(err, daemonconn.ErrProcessingConsent) || errors.Is(err, daemonconn.ErrProcessingPlanChanged)) {
		return `hint: review a fresh "docbank processing plan <path-or-id> --profile <profile>"; build only with its --plan-fingerprint and --consent`
	}
	if cmd == processingBuildCmd && code == exitGeneral {
		return `hint: if a job id was returned, inspect "docbank processing status <job-id> --json" before starting more work; otherwise inspect "docbank jobs" for an accepted job before retrying`
	}
	switch code {
	case exitUsage:
		if started {
			return "hint: " + help
		}
		if cmd == nil || cmd == rootCmd {
			return `hint: run "docbank --help"`
		}
		usage := cmd.UseLine()
		if len(usage) <= 120 && !strings.ContainsAny(usage, "\n\r") {
			return "hint: usage: " + usage + "; " + help
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
			return `hint: wait for the resource owner to finish; inspect "docbank daemon status" before retrying`
		}
	case exitNotFound:
		if cmd == rmCmd || cmd == mvCmd {
			return `hint: list paths with "docbank tree" or find by name with "docbank search <name>"; trashed nodes: "docbank trash list"`
		}
		if cmd == restoreCmd {
			return `hint: list restorable ids with "docbank trash list"; permanently removed nodes cannot be restored`
		}
		return "hint: check the requested identity; " + help
	case exitStale:
		if errors.Is(err, store.ErrAuditPreviewStale) {
			return `hint: review a fresh audit preview before confirming; see "docbank audit --help"`
		}
		return "hint: re-read the affected object and review any fresh preview before retrying with its current revision; " + help
	case exitIntegrity:
		return `hint: run "docbank verify" for a full report; do not trust affected bytes or retry writes until resolved`
	case exitClaim:
		return `hint: review the history with "docbank backup claim", then confirm it`
	default:
		return "hint: " + help + `; inspect "docbank daemon status" before deciding whether to retry`
	}
}

func exitCodeName(code int) string {
	switch code {
	case exitUsage:
		return "usage"
	case exitNotFound:
		return "not_found"
	case exitStale:
		return "stale"
	case exitBusy:
		return "busy"
	case exitIntegrity:
		return "integrity"
	case exitClaim:
		return "claim_required"
	default:
		return "error"
	}
}
