// Package blob implements loose content-addressed storage: one immutable
// file per unique content at blobs/<aa>/<sha256>, written durably.
package blob

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
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

// path returns the final on-disk path for a hash. Its result is only
// meaningful when hash is a valid 64-char lowercase SHA-256 hex string;
// callers that accept hashes from outside this package (e.g. DB rows) should
// validate via Open, Exists, or Remove instead of calling path directly.
func (s *Store) path(hash string) string {
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

// isStoredBlob reports whether final already stores this blob: a regular
// file (Lstat, so symlinks don't impersonate one) of the expected size.
// Anything else at that path is not the content its name promises; the
// caller replaces it with its own verified temp file. Size plus type catches
// every structural corruption at Lstat cost — same-size bit rot is verify's
// contract, and re-reading the full blob on every dedup would double the
// I/O of duplicate imports.
func isStoredBlob(final string, size int64) (bool, error) {
	fi, err := os.Lstat(final)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return fi.Mode().IsRegular() && fi.Size() == size, nil
}

// Write streams r into the store, returning the content's SHA-256 (lowercase
// hex) and size. The blob is durable — file fsynced, directory entries
// fsynced — before Write returns. Writing content that already exists is a
// no-op success; a stale invalid object at the blob's path (wrong size,
// symlink) is replaced with the verified bytes.
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
	final := s.path(hash)
	ok, err := isStoredBlob(final, size)
	if err != nil {
		return "", 0, fmt.Errorf("checking existing blob %s: %w", hash, err)
	}
	if ok {
		// Dedup: existing blob wins. Still sync the shard dir in case a
		// prior writer crashed after rename but before its own dir sync,
		// so we don't report durable success for a non-durable entry.
		if err := pack.SyncDir(filepath.Dir(final)); err != nil {
			return "", 0, fmt.Errorf("syncing blob shard dir: %w", err)
		}
		return hash, size, nil
	}

	shard := filepath.Dir(final)
	if err := pack.MkdirAllSynced(shard); err != nil {
		return "", 0, fmt.Errorf("creating blob shard dir: %w", err)
	}
	if err := os.Rename(tmpName, final); err != nil {
		// Some filesystems refuse to rename onto an existing destination
		// (or a concurrent writer's rename can interleave with ours). If
		// the destination now holds a valid blob, a concurrent Write for
		// the same content already finalized it: that's dedup success, not
		// our failure, so fall through and sync/return as if we'd won.
		// Anything else there — e.g. a directory, which rename cannot
		// replace — stays a hard error.
		//
		// The reverse interleaving — our rename replacing a concurrent
		// writer's already-synced file — is equally benign: same hash
		// means same bytes, and every writer syncs its temp file before
		// renaming, so the directory entry swaps between two durable
		// identical inodes.
		ok, verr := isStoredBlob(final, size)
		if verr != nil || !ok {
			return "", 0, fmt.Errorf("finalizing blob %s: %w", hash, err)
		}
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
	f, err := os.Open(s.path(hash))
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
	_, err := os.Stat(s.path(hash))
	if err == nil {
		return true, nil
	}
	if os.IsNotExist(err) {
		return false, nil
	}
	return false, fmt.Errorf("checking blob %s: %w", hash, err)
}

// Remove deletes a blob file; a missing file is success (GC re-runs
// reconcile crash leftovers). The unlink is durable — shard directory
// fsynced — before Remove returns, because callers delete the blob's
// metadata row next: a crash resurfacing the file after the row commit
// would leave an untracked blob no later gc or verify could see.
func (s *Store) Remove(hash string) error {
	if !validHash(hash) {
		return fmt.Errorf("blob hash %q: %w", hash, ErrInvalidHash)
	}
	if err := os.Remove(s.path(hash)); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("removing blob %s: %w", hash, err)
	}
	// Sync even when the file was already missing: a prior Remove may have
	// unlinked it and then failed this same sync, and returning success
	// here without one would let gc delete the row above a still-volatile
	// unlink. A shard dir that never existed has no entry to resurface.
	if err := pack.SyncDir(filepath.Dir(s.path(hash))); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("syncing blob shard dir: %w", err)
	}
	return nil
}

// List returns the hash and size of every blob file present on disk,
// walking the shard directories directly. gc uses it to find files that
// never gained (or lost) their metadata row — a blob written durably whose
// metadata transaction then failed is invisible to every row-based query.
func (s *Store) List() (map[string]int64, error) {
	shards, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, fmt.Errorf("reading blob dir: %w", err)
	}
	out := map[string]int64{}
	for _, shard := range shards {
		if !shard.IsDir() || len(shard.Name()) != 2 {
			continue // tmp/ and anything else that is not a shard
		}
		entries, err := os.ReadDir(filepath.Join(s.dir, shard.Name()))
		if err != nil {
			return nil, fmt.Errorf("reading blob shard %s: %w", shard.Name(), err)
		}
		for _, e := range entries {
			name := e.Name()
			if !validHash(name) || name[:2] != shard.Name() || !e.Type().IsRegular() {
				continue
			}
			info, err := e.Info()
			if err != nil {
				return nil, fmt.Errorf("reading blob %s: %w", name, err)
			}
			out[name] = info.Size()
		}
	}
	return out, nil
}

// CleanTmp removes leftover temp files from interrupted writes.
func (s *Store) CleanTmp() error {
	// Cleanup deletes everything it finds, so refuse to follow a symlinked
	// tmp dir: it would delete files outside the vault (and a tmp on another
	// filesystem would break rename-based finalization anyway).
	fi, err := os.Lstat(s.tmpDir())
	if err != nil {
		return fmt.Errorf("checking blob tmp dir: %w", err)
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("blob tmp dir %s is a symlink; refusing to clean it", s.tmpDir())
	}
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
