package api

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/kit/packstore"

	"go.kenn.io/docbank/internal/store"
)

// Error is the wire error envelope: RFC 7807 fields plus a machine-readable
// "code" extension member. Code is the contract clients branch on; Detail is
// for humans and may change freely.
type Error struct {
	Title  string   `json:"title"`
	Status int      `json:"status"`
	Detail string   `json:"detail,omitempty"`
	Code   string   `json:"code,omitempty"`
	Errors []string `json:"errors,omitempty"`
}

func (e *Error) Error() string  { return e.Detail }
func (e *Error) GetStatus() int { return e.Status }

// ContentType keeps huma emitting problem+json for our envelope.
func (e *Error) ContentType(ct string) string {
	if ct == "application/json" {
		return "application/problem+json"
	}
	return ct
}

func NewError(status int, code, detail string) *Error {
	return &Error{Title: http.StatusText(status), Status: status, Code: code, Detail: detail}
}

// installErrorFormatter routes huma's own errors (request validation,
// parsing) through the same envelope. Called once from NewServer.
func installErrorFormatter() {
	huma.NewError = func(status int, msg string, errs ...error) huma.StatusError {
		code := "validation"
		if status >= http.StatusInternalServerError {
			code = "internal"
		}
		e := NewError(status, code, msg)
		for _, err := range errs {
			if err != nil {
				e.Errors = append(e.Errors, err.Error())
			}
		}
		return e
	}
}

var storeErrCodes = []struct {
	target error
	status int
	code   string
}{
	{store.ErrNotFound, http.StatusNotFound, "not_found"},
	{store.ErrExists, http.StatusConflict, "exists"},
	{store.ErrCycle, http.StatusConflict, "cycle"},
	{store.ErrStaleRevision, http.StatusPreconditionFailed, "stale_revision"},
	{store.ErrNotDir, http.StatusUnprocessableEntity, "not_dir"},
	{store.ErrNotFile, http.StatusUnprocessableEntity, "not_file"},
	{store.ErrInvalidName, http.StatusUnprocessableEntity, "invalid_name"},
	{store.ErrNotTrashed, http.StatusUnprocessableEntity, "not_trashed"},
	{store.ErrIsRoot, http.StatusUnprocessableEntity, "is_root"},
}

// FromStoreError maps the store's typed errors onto the wire envelope; an
// unrecognized error becomes an opaque 500 (message still surfaced — this
// is a single-user local daemon, not a hardened multi-tenant service).
func FromStoreError(err error) error {
	if err == nil {
		return nil
	}
	for _, m := range storeErrCodes {
		if errors.Is(err, m.target) {
			return NewError(m.status, m.code, err.Error())
		}
	}
	return NewError(http.StatusInternalServerError, "internal", err.Error())
}

// FromMaintenanceError preserves the commit boundary of a deferred physical
// pack retirement. Repack catalog changes are already authoritative; a later
// pack pass reconciles the now-orphaned source file after external locks clear.
func FromMaintenanceError(err error) error {
	if errors.Is(err, packstore.ErrPackRetirementDeferred) {
		return NewError(http.StatusServiceUnavailable, "pack_retirement_deferred", fmt.Sprintf(
			"pack replacement committed, but source-file cleanup was deferred; release external file locks, "+
				"then run docbank storage pack to reconcile orphan packs: %v", err))
	}
	return FromStoreError(err)
}
