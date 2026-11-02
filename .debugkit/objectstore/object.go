// Package objectstore defines metadata, errors, and backend-neutral readers
// for immutable byte storage. Concrete stores expose byte operations; Open and
// Reader read any store as an io.Reader, io.ReaderAt, or io.ReadSeeker.
// Applications own their store interfaces and decide which keys to retain or
// delete.
package objectstore

import (
	"errors"
	"time"
)

var (
	// ErrNotFound means the requested key does not exist.
	ErrNotFound = errors.New("objectstore: object not found")
	// ErrExists means verified bytes at the key differ from the supplied source.
	ErrExists = errors.New("objectstore: different object exists")
	// ErrRevisionChanged means an operation's revision precondition failed.
	ErrRevisionChanged = errors.New("objectstore: revision changed")
	// ErrChecksumMismatch means the supplied or newly stored bytes failed validation.
	ErrChecksumMismatch = errors.New("objectstore: checksum mismatch")
	// ErrOutcomeUnknown means publication or verification could not be resolved.
	// The error also wraps the underlying cause; callers may retry the same intent.
	ErrOutcomeUnknown = errors.New("objectstore: publication outcome unknown")
)

// Checksum describes observed object integrity. Algorithm uses lower-case
// names such as "sha256" and
// "crc32c". Scope is "full" or "composite". Full-object Value uses standard
// base64; composite Value preserves the provider representation, including
// any part-count suffix. A composite checksum does not establish a full
// object's digest.
type Checksum struct{ Algorithm, Value, Scope string }

// Metadata describes an observed object. Revision is opaque and empty when
// unsupported. LastModified is UTC. Checksums may be absent.
type Metadata struct {
	Key          string
	Size         int64
	Revision     string
	LastModified time.Time
	Checksums    []Checksum
}

// Receipt proves the size and contents at an immutable key, regardless of
// which caller created it. SHA256 identifies the validated source.
// Verification is "provider-sha256", "provider-crc32c", or "readback-sha256".
// Failed publications return a zero Receipt, even if the key may be visible.
type Receipt struct {
	Object       Metadata
	SHA256       [32]byte
	Verification string
}

// Page is an inventory page, not permission to delete its objects.
// An empty NextToken marks the end of the inventory.
type Page struct {
	Objects   []Metadata
	NextToken string
}
