package docbank

import (
	"context"
	"errors"
	"fmt"
	"io"

	"go.kenn.io/kit/backup"
	"go.kenn.io/kit/pack"

	"go.kenn.io/docbank/internal/backupapp"
	"go.kenn.io/docbank/internal/version"
)

// BackupReaderInfo identifies the exact recovery point held by a reader.
type BackupReaderInfo struct {
	RepositoryID, SnapshotID string
	MinReaderVersion         int
}

// BackupObject identifies a complete logical file listed in a snapshot.
type BackupObject struct {
	SHA256 string
	Bytes  int64
}

// BackupReader reads selected files without opening or restoring a vault.
// Calls must be sequential. Close releases its shared repository lease.
type BackupReader struct {
	reader *backup.SnapshotReader
}

// OpenSnapshot opens an exact snapshot; an empty ID is an error.
// It loads the repository index and snapshot metadata and writes lease
// heartbeats until Close. Contention wraps ErrBackupRepositoryLocked.
func (r *BackupRepository) OpenSnapshot(ctx context.Context, snapshotID string) (*BackupReader, error) {
	if r == nil || r.repo == nil {
		return nil, errors.New("docbank backup repository is required")
	}
	reader, err := r.repo.OpenSnapshot(ctx, snapshotID, backupapp.New(version.Version).PackFileExtension())
	if err != nil {
		return nil, fmt.Errorf("opening backup snapshot: %w", err)
	}
	return &BackupReader{reader: reader}, nil
}

// Info returns snapshot identity, including after Close.
func (r *BackupReader) Info() BackupReaderInfo {
	info := r.reader.Info()
	return BackupReaderInfo{RepositoryID: info.RepositoryID, SnapshotID: info.SnapshotID, MinReaderVersion: info.MinReaderVersion}
}

// Object looks up a snapshot member by whole-file SHA-256, also after Close.
// A missing member wraps fs.ErrNotExist, even if another snapshot contains it.
func (r *BackupReader) Object(sha256 string) (BackupObject, error) {
	id, err := pack.ParseBlobID(sha256)
	if err != nil {
		return BackupObject{}, fmt.Errorf("backup object SHA-256: %w", err)
	}
	object, err := r.reader.Object(id)
	if err != nil {
		return BackupObject{}, fmt.Errorf("looking up backup object: %w", err)
	}
	return BackupObject{SHA256: object.ID.String(), Bytes: object.Bytes}, nil
}

// Extra looks up a host file by its exact RecordAs name, also after Close.
// A missing name wraps fs.ErrNotExist.
func (r *BackupReader) Extra(name string) (BackupObject, error) {
	object, err := r.reader.Extra(name)
	if err != nil {
		return BackupObject{}, fmt.Errorf("looking up backup extra: %w", err)
	}
	return BackupObject{SHA256: object.ID.String(), Bytes: object.Bytes}, nil
}

// CopyObject streams and verifies a snapshot member's complete hash and size.
// Discard all output on any error, including cancellation or a failed write.
// Copying a valid snapshot member after Close wraps fs.ErrClosed.
func (r *BackupReader) CopyObject(ctx context.Context, sha256 string, dst io.Writer) (int64, error) {
	id, err := pack.ParseBlobID(sha256)
	if err != nil {
		return 0, fmt.Errorf("backup object SHA-256: %w", err)
	}
	n, err := r.reader.CopyObject(ctx, id, dst)
	if err != nil {
		return n, fmt.Errorf("copying backup object: %w", err)
	}
	return n, nil
}

// CopyExtra copies a host file with the same verification and failure
// contract as CopyObject. The caller owns the destination and its cleanup.
// A missing name wraps fs.ErrNotExist, including after Close.
func (r *BackupReader) CopyExtra(ctx context.Context, name string, dst io.Writer) (int64, error) {
	n, err := r.reader.CopyExtra(ctx, name, dst)
	if err != nil {
		return n, fmt.Errorf("copying backup extra: %w", err)
	}
	return n, nil
}

// Close closes pack files before releasing the repository lease. Repeated
// calls return the first close result. Metadata lookups remain available.
// Closing a nil or uninitialized reader does nothing.
func (r *BackupReader) Close() error {
	if r == nil || r.reader == nil {
		return nil
	}
	if err := r.reader.Close(); err != nil {
		return fmt.Errorf("closing backup reader: %w", err)
	}
	return nil
}
