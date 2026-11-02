// Package local stores immutable objects in an existing directory.
//
// The caller owns the namespace directory and must keep it and its ancestors
// stable and prevent other writers from changing published bytes. Operations
// reject links in key paths; these checks do not protect against a writer who
// can swap directory entries concurrently. Local revisions are unsupported.
// Keys retain the filesystem's case sensitivity; callers must use one spelling
// per object. On Windows, nonportable names and 8.3 aliases are rejected.
// Key components beginning with ".objectstore-" (case-insensitive) are reserved
// for staging and rejected by point operations. Inventory preserves them.
// After a crash, stop all publishers sharing the root before removing leftover
// .objectstore- files directly from disk. Age alone does not prove abandonment.
//
// Publication uses atomicfile's platform durability semantics: file sync and
// directory sync on Unix, file sync and write-through rename on Windows.
// Windows cannot additionally sync directories. Atomic no-replace rename is
// supported on Linux, macOS, and Windows; other platforms fail explicitly.
// Cancellation is checked between synchronous file and caller-reader calls;
// it cannot interrupt an already blocked Read, Seek, Sync, or filesystem call.
package local

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"go.kenn.io/kit/atomicfile"
	"go.kenn.io/kit/fsname"
	"go.kenn.io/kit/internal/strictio"
	"go.kenn.io/kit/objectstore"
)

const stagingPrefix = ".objectstore-"

// Store is an immutable local namespace. Use New to construct it.
// Independent operations may run concurrently; callers must coordinate Delete
// with reads/publications because the local filesystem has no revision checks.
type Store struct {
	root    string
	syncDir func(string) error
	rename  func(string, string) error
}

// New opens an existing directory as a namespace. The caller is responsible
// for creating the root durably. New does not take ownership of any resource.
func New(root string) (*Store, error) {
	root, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("objectstore local: root: %w", err)
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return nil, fmt.Errorf("objectstore local: root: %w", err)
	}
	info, err := os.Stat(root)
	if err != nil {
		return nil, fmt.Errorf("objectstore local: root: %w", err)
	}
	if !info.IsDir() {
		return nil, errors.New("objectstore local: root is not a directory")
	}
	return &Store{root: root, syncDir: atomicfile.SyncDir, rename: atomicfile.RenameNoReplace}, nil
}

// Publish validates the entire source from offset zero, stages it on the
// destination filesystem, and publishes without replacing an existing entry.
// A matching retry verifies the stored bytes and repeats file/directory sync.
// The source stays open and its position is unspecified on return.
func (s *Store) Publish(ctx context.Context, key string, source io.ReadSeeker, size int64, digest [32]byte) (receipt objectstore.Receipt, resultErr error) {
	destination, err := s.path(ctx, key)
	if err != nil {
		return receipt, err
	}
	if source == nil || size < 0 {
		return receipt, fmt.Errorf("%w: invalid source or size", objectstore.ErrChecksumMismatch)
	}
	if _, err := source.Seek(0, io.SeekStart); err != nil {
		return receipt, fmt.Errorf("objectstore local: rewind source: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return receipt, err
	}
	parent := filepath.Dir(destination)
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return receipt, fmt.Errorf("objectstore local: create parent: %w", err)
	}
	staged, err := os.CreateTemp(parent, stagingPrefix+"*")
	if err != nil {
		return receipt, fmt.Errorf("objectstore local: stage: %w", err)
	}
	stagedPath := staged.Name()
	closed := false
	published := false
	defer func() {
		var closeErr, removeErr error
		if !closed {
			closeErr = staged.Close()
		}
		// After a rename, the staging name is free. Another publisher in the
		// same directory can receive it, so only an unpublished file is ours.
		if !published {
			if err := os.Remove(stagedPath); err != nil && !errors.Is(err, fs.ErrNotExist) {
				removeErr = err
			}
		}
		if cleanupErr := errors.Join(closeErr, removeErr); cleanupErr != nil {
			resultErr = errors.Join(resultErr, fmt.Errorf("objectstore local: clean staging: %w", cleanupErr))
			if published {
				resultErr = errors.Join(objectstore.ErrOutcomeUnknown, resultErr)
			}
			receipt = objectstore.Receipt{}
		}
	}()
	hash := sha256.New()
	checked := &contextReader{ctx: ctx, reader: source}
	n, err := io.Copy(io.MultiWriter(staged, hash), io.LimitReader(checked, size))
	if err != nil {
		return receipt, fmt.Errorf("objectstore local: stage source: %w", err)
	}
	var extra [1]byte
	extraN, extraErr := strictio.ReadFull(checked, extra[:])
	if err := strictio.IgnoreEOF(extraErr); err != nil {
		return receipt, fmt.Errorf("objectstore local: source length: %w", err)
	}
	if n != size || extraN != 0 || [32]byte(hash.Sum(nil)) != digest {
		return receipt, fmt.Errorf("%w: source size or SHA-256 differs", objectstore.ErrChecksumMismatch)
	}
	if err := ctx.Err(); err != nil {
		return receipt, err
	}
	if err := staged.Sync(); err != nil {
		return receipt, fmt.Errorf("objectstore local: sync staging: %w", err)
	}
	closed = true
	if err := staged.Close(); err != nil {
		return receipt, fmt.Errorf("objectstore local: close staging: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return receipt, err
	}
	if err := s.rename(stagedPath, destination); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return s.verify(ctx, key, destination, size, digest, true)
		}
		// A rename can take effect and still fail, as a write-through move
		// does when its final flush fails. Only a staging file that is still
		// present proves that nothing was published.
		if _, statErr := os.Lstat(stagedPath); statErr == nil {
			return receipt, fmt.Errorf("objectstore local: publish: %w", err)
		}
		published = true
		return receipt, fmt.Errorf("%w: publish %q: %w", objectstore.ErrOutcomeUnknown, key, err)
	}
	published = true
	return s.verify(ctx, key, destination, size, digest, false)
}

func (s *Store) verify(ctx context.Context, key, path string, size int64, digest [32]byte, existing bool) (objectstore.Receipt, error) {
	fail := func(err error) (objectstore.Receipt, error) {
		return objectstore.Receipt{}, fmt.Errorf("%w: verify %q: %w", objectstore.ErrOutcomeUnknown, key, err)
	}
	if err := ctx.Err(); err != nil {
		return fail(err)
	}
	flags := os.O_RDONLY
	// Windows FlushFileBuffers requires a writable file handle.
	if runtime.GOOS == "windows" {
		flags = os.O_RDWR
	}
	file, info, err := openRegular(path, flags)
	if err != nil {
		if existing && info != nil && !info.Mode().IsRegular() {
			return objectstore.Receipt{}, err
		}
		return fail(err)
	}
	hash := sha256.New()
	n, readErr := io.Copy(hash, &contextReader{ctx: ctx, reader: file})
	if readErr != nil {
		return fail(errors.Join(readErr, file.Close()))
	}
	if err := ctx.Err(); err != nil {
		return fail(errors.Join(err, file.Close()))
	}
	after, err := file.Stat()
	if err != nil {
		return fail(errors.Join(err, file.Close()))
	}
	current, err := os.Lstat(path)
	if err != nil {
		return fail(errors.Join(err, file.Close()))
	}
	if !os.SameFile(info, current) || info.Size() != after.Size() || !info.ModTime().Equal(after.ModTime()) || !os.SameFile(after, current) {
		return fail(errors.Join(objectstore.ErrRevisionChanged, file.Close()))
	}
	observed := [32]byte(hash.Sum(nil))
	if n != size || n != info.Size() || observed != digest {
		mismatch := objectstore.ErrChecksumMismatch
		if existing {
			mismatch = objectstore.ErrExists
		}
		return objectstore.Receipt{}, errors.Join(fmt.Errorf("%w: stored size or SHA-256 differs at %q", mismatch, key), file.Close())
	}
	syncErr := file.Sync()
	closeErr := file.Close()
	if err := errors.Join(syncErr, closeErr); err != nil {
		return fail(err)
	}
	// Sync every ancestor, including existing directories: a previous failed
	// publication may have left both the file and its parents undurable.
	for directory := filepath.Dir(path); ; directory = filepath.Dir(directory) {
		if err := ctx.Err(); err != nil {
			return fail(err)
		}
		if err := s.syncDir(directory); err != nil {
			return fail(err)
		}
		if directory == s.root {
			break
		}
	}
	if err := ctx.Err(); err != nil {
		return fail(err)
	}
	meta := metadata(key, info)
	meta.Checksums = []objectstore.Checksum{{Algorithm: "sha256", Value: base64.StdEncoding.EncodeToString(observed[:]), Scope: "full"}}
	return objectstore.Receipt{Object: meta, SHA256: observed, Verification: "readback-sha256"}, nil
}

// Inspect returns file metadata without computing a checksum. Missing keys
// wrap ErrNotFound; permission and I/O errors retain their original identity.
func (s *Store) Inspect(ctx context.Context, key string) (objectstore.Metadata, error) {
	path, err := s.path(ctx, key)
	if err != nil {
		return objectstore.Metadata{}, err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return objectstore.Metadata{}, objectError("inspect", key, err)
	}
	if !info.Mode().IsRegular() {
		return objectstore.Metadata{}, fmt.Errorf("objectstore local: %q is not a regular file", key)
	}
	if err := ctx.Err(); err != nil {
		return objectstore.Metadata{}, err
	}
	return metadata(key, info), nil
}

// OpenRange returns exactly length bytes starting at offset. Truncation during
// reading returns io.ErrUnexpectedEOF. The caller closes the reader; terminal
// reads also release its file. A nonempty revision fails with ErrUnsupported.
func (s *Store) OpenRange(ctx context.Context, key, revision string, offset, length int64) (io.ReadCloser, error) {
	path, err := s.path(ctx, key)
	if err != nil {
		return nil, err
	}
	if revision != "" {
		return nil, fmt.Errorf("objectstore local: revision precondition: %w", errors.ErrUnsupported)
	}
	if offset < 0 || length < 0 {
		return nil, errors.New("objectstore local: invalid range")
	}
	file, info, err := openRegular(path, os.O_RDONLY)
	if err != nil {
		return nil, objectError("open range", key, err)
	}
	if offset > info.Size() || length > info.Size()-offset {
		return nil, errors.Join(errors.New("objectstore local: range exceeds object"), file.Close())
	}
	if err := ctx.Err(); err != nil {
		return nil, errors.Join(err, file.Close())
	}
	return &rangeReader{ctx: ctx, file: file, reader: io.NewSectionReader(file, offset, length), remaining: length}, nil
}

// ListPage lists files (including foreign names and links) in lexical key
// order. Its opaque token is bound to the prefix; pages are not snapshots.
// Each page scans the namespace, retaining at most limit+1 object records.
// Listing never reads object contents or changes entries.
func (s *Store) ListPage(ctx context.Context, prefix, token string, limit int) (objectstore.Page, error) {
	var page objectstore.Page
	if err := ctx.Err(); err != nil {
		return page, err
	}
	if err := objectstore.ValidatePrefix(prefix); err != nil {
		return page, err
	}
	if limit <= 0 {
		return page, errors.New("objectstore local: page size must be positive")
	}
	after := ""
	if token != "" {
		decoded, err := base64.RawURLEncoding.DecodeString(token)
		if err != nil {
			return page, fmt.Errorf("objectstore local: invalid cursor: %w", err)
		}
		cursorPrefix, last, ok := strings.Cut(string(decoded), "\x00")
		if !ok || cursorPrefix != prefix || last == "" {
			return page, errors.New("objectstore local: cursor does not match prefix")
		}
		after = last
	}
	err := scanFiles(ctx, s.root, func(path string, entry fs.DirEntry) error {
		key, err := filepath.Rel(s.root, path)
		if err != nil {
			return err
		}
		key = filepath.ToSlash(key)
		if !strings.HasPrefix(key, prefix) || key <= after {
			return nil
		}
		position := sort.Search(len(page.Objects), func(i int) bool { return page.Objects[i].Key >= key })
		if position > limit {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		page.Objects = append(page.Objects, objectstore.Metadata{})
		copy(page.Objects[position+1:], page.Objects[position:])
		page.Objects[position] = metadata(key, info)
		if len(page.Objects)-1 > limit {
			page.Objects = page.Objects[:len(page.Objects)-1]
		}
		return nil
	})
	if err != nil {
		return objectstore.Page{}, fmt.Errorf("objectstore local: list: %w", err)
	}
	if len(page.Objects) > limit {
		page.Objects = page.Objects[:limit]
		page.NextToken = base64.RawURLEncoding.EncodeToString([]byte(prefix + "\x00" + page.Objects[limit-1].Key))
	}
	return page, nil
}

// Delete removes an exact key and syncs its parent. Missing keys succeed.
// A nonempty revision fails with errors.ErrUnsupported. It never removes
// directories or treats an inventory entry as cleanup authority.
func (s *Store) Delete(ctx context.Context, key, revision string) error {
	path, err := s.path(ctx, key)
	if err != nil {
		if errors.Is(err, objectstore.ErrNotFound) && revision == "" {
			return nil
		}
		return err
	}
	if revision != "" {
		return fmt.Errorf("objectstore local: revision precondition: %w", errors.ErrUnsupported)
	}
	info, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		// A previous removal may have succeeded while its directory sync failed.
		return s.syncDeletion(ctx, path)
	}
	if err != nil {
		return objectError("delete", key, err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("objectstore local: %q is not a regular file", key)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return objectError("delete", key, err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return s.syncDeletion(ctx, path)
}

func (s *Store) syncDeletion(ctx context.Context, path string) error {
	for parent := filepath.Dir(path); ; parent = filepath.Dir(parent) {
		if err := ctx.Err(); err != nil {
			return err
		}
		err := s.syncDir(parent)
		if !errors.Is(err, fs.ErrNotExist) || parent == s.root {
			return err
		}
	}
}

// Read directory entries in batches rather than materializing the inventory.
// Recursion retains one directory handle and one batch per namespace depth.
func scanFiles(ctx context.Context, directory string, visit func(string, fs.DirEntry) error) (resultErr error) {
	if err := ctx.Err(); err != nil {
		return err
	}
	dir, err := os.Open(directory)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, dir.Close()) }()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		entries, readErr := dir.ReadDir(128)
		for _, entry := range entries {
			if err := ctx.Err(); err != nil {
				return err
			}
			path := filepath.Join(directory, entry.Name())
			if entry.IsDir() {
				if err := scanFiles(ctx, path, visit); err != nil && !errors.Is(err, fs.ErrNotExist) {
					return err
				}
			} else if err := visit(path, entry); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return err
			}
		}
		if errors.Is(readErr, io.EOF) {
			return nil
		}
		if readErr != nil {
			return readErr
		}
	}
}

func (s *Store) path(ctx context.Context, key string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if err := objectstore.ValidateKey(key); err != nil {
		return "", err
	}
	if runtime.GOOS == "windows" {
		if err := fsname.CheckPath(key); err != nil {
			return "", err
		}
	}
	path := s.root
	parts := strings.Split(key, "/")
	for _, part := range parts {
		if strings.HasPrefix(strings.ToLower(part), stagingPrefix) {
			return "", fmt.Errorf("objectstore local: key %q uses a reserved staging name", key)
		}
	}
	for _, part := range parts[:len(parts)-1] {
		path = filepath.Join(path, part)
		info, err := os.Lstat(path)
		if errors.Is(err, fs.ErrNotExist) {
			break
		}
		if err != nil {
			return "", fmt.Errorf("objectstore local: inspect parent: %w", err)
		}
		if !info.IsDir() {
			if info.Mode().IsRegular() {
				return "", fmt.Errorf("%w: parent %q is a file", objectstore.ErrNotFound, part)
			}
			return "", fmt.Errorf("objectstore local: parent %q is not a directory", part)
		}
	}
	return filepath.Join(s.root, filepath.FromSlash(key)), nil
}

func openRegular(path string, flags int) (*os.File, fs.FileInfo, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, info, errors.New("objectstore local: not a regular file")
	}
	file, err := os.OpenFile(path, flags, 0)
	if err != nil {
		return nil, nil, err
	}
	info, err = file.Stat()
	if err != nil {
		return nil, nil, errors.Join(err, file.Close())
	}
	return file, info, nil
}

func objectError(operation, key string, err error) error {
	if errors.Is(err, fs.ErrNotExist) {
		err = errors.Join(objectstore.ErrNotFound, err)
	}
	return fmt.Errorf("objectstore local: %s %q: %w", operation, key, err)
}

func metadata(key string, info fs.FileInfo) objectstore.Metadata {
	return objectstore.Metadata{Key: key, Size: info.Size(), LastModified: info.ModTime().UTC()}
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r *contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	n, err := r.reader.Read(p)
	if contextErr := r.ctx.Err(); contextErr != nil {
		return n, errors.Join(contextErr, err)
	}
	return n, err
}

type rangeReader struct {
	ctx       context.Context
	file      *os.File
	reader    *io.SectionReader
	remaining int64
	closed    bool
	closeErr  error
}

func (r *rangeReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, errors.Join(err, r.Close())
	}
	if r.remaining == 0 {
		if err := r.Close(); err != nil {
			return 0, err
		}
		return 0, io.EOF
	}
	if r.closed {
		return 0, fs.ErrClosed
	}
	n, err := r.reader.Read(p)
	r.remaining -= int64(n)
	err = strictio.RangeEnd(err, r.remaining)
	if contextErr := r.ctx.Err(); contextErr != nil {
		err = errors.Join(contextErr, err)
	}
	if err != nil || r.remaining == 0 {
		err = errors.Join(err, r.Close())
	}
	return n, err
}

func (r *rangeReader) Close() error {
	if !r.closed {
		r.closed = true
		r.closeErr = r.file.Close()
	}
	return r.closeErr
}
