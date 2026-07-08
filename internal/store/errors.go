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
	// ErrNotTrashed is returned when restoring a node that is not a trash root.
	ErrNotTrashed = errors.New("node is not trashed")
	// ErrIsRoot is returned when an operation targets the root node.
	ErrIsRoot = errors.New("operation not allowed on root")
	// ErrStaleRevision means a mutation's expected revision no longer
	// matches the node (lost-update guard for If-Match).
	ErrStaleRevision = errors.New("revision mismatch")
)
