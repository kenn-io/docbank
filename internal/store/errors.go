package store

import "errors"

var (
	// ErrNotFound is returned when a node, path, or blob does not exist.
	ErrNotFound = errors.New("not found")
	// ErrExists is returned when a live sibling with the same name exists.
	ErrExists = errors.New("name already exists")
	// ErrNotDir is returned when a directory operation targets a file.
	ErrNotDir = errors.New("not a directory")
	// ErrNotFile is returned when a file operation targets a directory.
	ErrNotFile = errors.New("not a file")
	// ErrCycle is returned when a move would place a node under its own descendant.
	ErrCycle = errors.New("move would create a cycle")
	// ErrInvalidName is returned for empty, ".", "..", or names containing '/' or NUL.
	ErrInvalidName = errors.New("invalid name")
	// ErrInvalidTag is returned for an empty, non-UTF-8, or control-containing tag name.
	ErrInvalidTag = errors.New("invalid tag name")
	// ErrNotTrashed is returned when restoring a node that is not a trash root.
	ErrNotTrashed = errors.New("node is not trashed")
	// ErrIsRoot is returned when an operation targets the root node.
	ErrIsRoot = errors.New("operation not allowed on root")
	// ErrStaleRevision means a mutation's expected revision no longer
	// matches the node (lost-update guard for If-Match).
	ErrStaleRevision = errors.New("revision mismatch")
	// ErrVersionNodeMismatch means a requested source version belongs to a
	// different stable file node.
	ErrVersionNodeMismatch = errors.New("content version belongs to another node")
	// ErrVersionAlreadyCurrent means a revert selected the node's current head,
	// which is not a historical transition.
	ErrVersionAlreadyCurrent = errors.New("content version is already current")
	// ErrInvalidVersionPrune means a history-pruning selector is absent,
	// contradictory, or otherwise unsafe to execute.
	ErrInvalidVersionPrune = errors.New("invalid version-prune selector")
	// ErrInvalidBatchMove means a batch is empty, oversized, repeats a source,
	// or does not identify each source in exactly one supported way.
	ErrInvalidBatchMove = errors.New("invalid batch move")
	// ErrAuditMutationUnsupported means an audited vault cannot perform a
	// logical mutation until that mutation records its audit transition.
	ErrAuditMutationUnsupported = errors.New("mutation is not supported for an audited vault")
	// ErrAuditAlreadyEnabled means a plan reviewed against dormant authority
	// cannot run because another operation enabled audit first.
	ErrAuditAlreadyEnabled = errors.New("audit is already enabled for this vault")
	// ErrAuditScopeOverlap means an additional scope would share at least one
	// sticky member with an existing scope. Disjoint scopes are supported first;
	// overlapping and nested scopes remain fail-closed.
	ErrAuditScopeOverlap = errors.New("audit scope overlaps existing permanent protection")
	// ErrAuditScopeLimit means permanent enrollment has reached the maximum
	// number of scope terminals one evidence bundle can represent.
	ErrAuditScopeLimit = errors.New("audit scope limit reached")
	// ErrAuditPreviewStale means enrollment no longer matches the exact
	// metadata state reviewed by the caller.
	ErrAuditPreviewStale = errors.New("audit enrollment preview is stale")
	// ErrAuditNotEnrolled means a node exists but has no sticky audit membership.
	ErrAuditNotEnrolled = errors.New("node is not enrolled in an audit scope")
	// ErrInvalidAuditCursor means an audit-history cursor is malformed or belongs
	// to a different stable node or scope.
	ErrInvalidAuditCursor = errors.New("invalid audit history cursor")
)

// UnconditionalRev is the only ifRev value that skips the revision
// precondition on Move, Trash, and Restore. Every other value — including
// other negatives, which can never match a real revision — must satisfy
// the check, so an accidentally propagated bad revision fails stale
// instead of silently mutating.
const UnconditionalRev int64 = -1
