// Package blob implements loose content-addressed storage: one immutable
// file per unique content at blobs/<aa>/<sha256>, written durably.
package blob

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"go.kenn.io/kit/pack"
)

// ErrInvalidHash is returned when a caller-supplied hash is not a 64-char
// lowercase SHA-256 hex string.
var ErrInvalidHash = errors.New("invalid blob hash")

// Store is a content-addressed blob directory.
type Store struct {
	dir string // .../blobs
}

// New returns a Store rooted at blobsDir (which must contain a tmp/ subdir;
// home.Layout.Ensure creates both).
func New(blobsDir string) *Store {
	return &Store{dir: blobsDir}
}

func (s *Store) tmpDir() string { return filepath.Join(s.dir, "tmp") }

// Path returns the final on-disk path for a hash. Its result is only
// meaningful when hash is a valid 64-char lowercase SHA-256 hex string;
// callers that accept hashes from outside this package (e.g. DB rows) should
// validate via Open, Exists, or Remove instead of calling Path directly.
func (s *Store) Path(hash string) string {
	return filepath.Join(s.dir, hash[:2], hash)
}

func validHash(hash string) bool {
	if len(hash) != 64 {
		return false
	}
	for _, c := range hash {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// Write streams r into the store, returning the content's SHA-256 (lowercase
// hex) and size. The blob is durable — file fsynced, directory entries
// fsynced — before Write returns. Writing content that already exists is a
// no-op success.
func (s *Store) Write(r io.Reader) (string, int64, error) {
	tmp, err := os.CreateTemp(s.tmpDir(), "blob-*")
	if err != nil {
		return "", 0, fmt.Errorf("creating temp blob file: %w", err)
	}
	tmpName := tmp.Name()
	defer func() {
		_ = tmp.Close()
		_ = os.Remove(tmpName) // no-op after successful rename
	}()

	hasher := sha256.New()
	size, err := io.Copy(io.MultiWriter(tmp, hasher), r)
	if err != nil {
		return "", 0, fmt.Errorf("writing blob content: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		return "", 0, fmt.Errorf("syncing blob content: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return "", 0, fmt.Errorf("closing temp blob file: %w", err)
	}

	hash := hex.EncodeToString(hasher.Sum(nil))
	final := s.Path(hash)
	if _, err := os.Stat(final); err == nil {
		return hash, size, nil // dedup: existing blob wins
	} else if !os.IsNotExist(err) {
		return "", 0, fmt.Errorf("checking existing blob %s: %w", hash, err)
	}

	shard := filepath.Dir(final)
	if err := pack.MkdirAllSynced(shard); err != nil {
		return "", 0, fmt.Errorf("creating blob shard dir: %w", err)
	}
	if err := os.Rename(tmpName, final); err != nil {
		return "", 0, fmt.Errorf("finalizing blob %s: %w", hash, err)
	}
	if err := pack.SyncDir(shard); err != nil {
		return "", 0, fmt.Errorf("syncing blob shard dir: %w", err)
	}
	return hash, size, nil
}

// Open opens a blob for reading.
func (s *Store) Open(hash string) (*os.File, error) {
	if !validHash(hash) {
		return nil, fmt.Errorf("blob hash %q: %w", hash, ErrInvalidHash)
	}
	f, err := os.Open(s.Path(hash))
	if err != nil {
		return nil, fmt.Errorf("opening blob %s: %w", hash, err)
	}
	return f, nil
}

// Exists reports whether the blob file is present.
func (s *Store) Exists(hash string) (bool, error) {
	if !validHash(hash) {
		return false, fmt.Errorf("blob hash %q: %w", hash, ErrInvalidHash)
	}
	_, err := os.Stat(s.Path(hash))
	if err == nil {
		return true, nil
	}
	if os.IsNotExist(err) {
		return false, nil
	}
	return false, fmt.Errorf("checking blob %s: %w", hash, err)
}

// Remove deletes a blob file; a missing file is success (GC re-runs
// reconcile crash leftovers).
func (s *Store) Remove(hash string) error {
	if !validHash(hash) {
		return fmt.Errorf("blob hash %q: %w", hash, ErrInvalidHash)
	}
	if err := os.Remove(s.Path(hash)); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("removing blob %s: %w", hash, err)
	}
	return nil
}

// CleanTmp removes leftover temp files from interrupted writes.
func (s *Store) CleanTmp() error {
	entries, err := os.ReadDir(s.tmpDir())
	if err != nil {
		return fmt.Errorf("reading blob tmp dir: %w", err)
	}
	for _, e := range entries {
		if err := os.Remove(filepath.Join(s.tmpDir(), e.Name())); err != nil {
			return fmt.Errorf("removing stale temp file %s: %w", e.Name(), err)
		}
	}
	return nil
}
