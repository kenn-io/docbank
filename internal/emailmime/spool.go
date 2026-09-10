package emailmime

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/home"
)

const spoolPrefix = "docbank-email-"
const spoolMarkerName = ".docbank-email-spool"
const spoolMarker = "docbank-email-spool/v1\n"

type storedArtifact struct {
	artifact Artifact
	filename string
}

type spoolIOError struct{ err error }

func (e *spoolIOError) Error() string { return fmt.Sprintf("email MIME spool I/O: %v", e.err) }
func (e *spoolIOError) Unwrap() error { return e.err }

type ownedSpool struct {
	parent *os.Root
	root   *os.Root
	pin    *os.File
	name   string
}

func openStableSpoolRoot(path string) (*os.Root, error) {
	if !filepath.IsAbs(path) {
		return nil, errors.New("email spool parent must be absolute")
	}
	canonical, err := home.CanonicalRoot(path)
	if err != nil {
		return nil, fmt.Errorf("resolve email spool parent: %w", err)
	}
	if canonical != filepath.Clean(path) {
		return nil, errors.New("email spool parent contains a symlink or non-canonical component")
	}
	before, err := os.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("inspect email spool parent: %w", err)
	}
	if !before.IsDir() || before.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("email spool parent is not a rooted directory")
	}
	root, err := os.OpenRoot(path)
	if err != nil {
		return nil, fmt.Errorf("open rooted email spool parent: %w", err)
	}
	after, err := root.Stat(".")
	if err != nil || !after.IsDir() || !os.SameFile(before, after) {
		_ = root.Close()
		if err != nil {
			return nil, fmt.Errorf("verify rooted email spool parent: %w", err)
		}
		return nil, errors.New("email spool parent identity changed while opening")
	}
	return root, nil
}

func randomEmailSpoolName() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", fmt.Errorf("read email spool name entropy: %w", err)
	}
	return spoolPrefix + hex.EncodeToString(value[:]), nil
}

func createSpool(spoolParent string) (*ownedSpool, error) {
	parent, err := openStableSpoolRoot(spoolParent)
	if err != nil {
		return nil, err
	}
	name, err := randomEmailSpoolName()
	if err != nil {
		_ = parent.Close()
		return nil, err
	}
	pin, err := createPrivateSpoolDirectory(parent, name)
	if err != nil {
		_ = parent.Close()
		return nil, fmt.Errorf("create email spool: %w", err)
	}
	spool := &ownedSpool{parent: parent, pin: pin, name: name}
	clean := true
	defer func() {
		if clean {
			_ = spool.cleanup()
		}
	}()
	before, err := parent.Lstat(name)
	if err != nil || !before.IsDir() || before.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("created email spool is not a rooted directory")
	}
	spool.root, err = parent.OpenRoot(name)
	if err != nil {
		return nil, fmt.Errorf("open rooted email spool: %w", err)
	}
	after, err := spool.root.Stat(".")
	if err != nil || !after.IsDir() || !os.SameFile(before, after) {
		return nil, errors.New("email spool identity changed while opening")
	}
	if err = spool.root.WriteFile(spoolMarkerName, []byte(spoolMarker), 0o600); err != nil {
		return nil, fmt.Errorf("mark email spool: %w", err)
	}
	clean = false
	return spool, nil
}

func (s *ownedSpool) create(name string) (*os.File, error) {
	if s == nil || s.root == nil || filepath.Base(name) != name {
		return nil, errors.New("email spool is closed or artifact name is invalid")
	}
	return s.root.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
}

func (s *ownedSpool) openRegular(name string) (*os.File, error) {
	if s == nil || s.root == nil || filepath.Base(name) != name {
		return nil, errors.New("email spool is closed or artifact name is invalid")
	}
	return openRootRegular(s.root, name)
}

func openRootRegular(root *os.Root, name string) (*os.File, error) {
	before, err := root.Lstat(name)
	if err != nil {
		return nil, err
	}
	if !before.Mode().IsRegular() || before.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("email spool artifact is a symlink, reparse point, or non-regular file")
	}
	file, err := root.Open(name)
	if err != nil {
		return nil, err
	}
	after, err := file.Stat()
	if err != nil || !after.Mode().IsRegular() || !os.SameFile(before, after) {
		_ = file.Close()
		if err != nil {
			return nil, err
		}
		return nil, errors.New("email spool artifact identity changed while opening")
	}
	return file, nil
}

func ownershipMarkerMatches(reader io.Reader) (bool, error) {
	value, err := io.ReadAll(io.LimitReader(reader, int64(len(spoolMarker)+1)))
	if err != nil {
		return false, err
	}
	return string(value) == spoolMarker, nil
}

func (s *ownedSpool) remove(name string) error {
	if s == nil || s.root == nil || filepath.Base(name) != name {
		return errors.New("email spool is closed or artifact name is invalid")
	}
	return s.root.Remove(name)
}

func (s *ownedSpool) cleanup() error {
	if s == nil {
		return nil
	}
	var result error
	if s.root != nil {
		entries, err := fs.ReadDir(s.root.FS(), ".")
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			result = errors.Join(result, err)
		}
		for _, entry := range entries {
			if err := s.root.RemoveAll(entry.Name()); err != nil && !errors.Is(err, os.ErrNotExist) {
				result = errors.Join(result, err)
			}
		}
		result = errors.Join(result, s.root.Close())
		s.root = nil
	}
	if s.pin != nil {
		result = errors.Join(result, s.pin.Close())
		s.pin = nil
	}
	if s.parent != nil {
		if err := s.parent.RemoveAll(s.name); err != nil && !errors.Is(err, os.ErrNotExist) {
			result = errors.Join(result, err)
		}
		result = errors.Join(result, s.parent.Close())
		s.parent = nil
	}
	return result
}

func (d *decoder) storeBytes(partPath string, role document.EmailArtifactRole, value []byte) (*document.EmailArtifactRefV1, error) {
	return d.storeStream(partPath, role, bytes.NewReader(value), int64(len(value)), nil)
}

func (d *decoder) storeStream(partPath string, role document.EmailArtifactRole, reader io.Reader, limit int64, total *int64) (*document.EmailArtifactRefV1, error) {
	return d.storeStreamLimited(partPath, role, reader, limit, total, d.limits.DecodedBytes, document.EmailDiagnosticPartBytesLimit, document.EmailDiagnosticDecodedBytesLimit, document.EmailOperationTransfer)
}

func (d *decoder) storeStreamLimited(partPath string, role document.EmailArtifactRole, reader io.Reader, limit int64, total *int64, totalLimit int64, limitCode, totalCode document.EmailDiagnosticCode, operation document.EmailOperation) (*document.EmailArtifactRefV1, error) {
	initialTotal := int64(0)
	committed := false
	if total != nil {
		initialTotal = *total
		defer func() {
			if !committed {
				*total = initialTotal
			}
		}()
	}
	sequence := len(d.artifacts)
	name := fmt.Sprintf("artifact-%06d", sequence)
	file, err := d.spool.create(name)
	if err != nil {
		return nil, &spoolIOError{err: err}
	}
	discard := func(cause error) error {
		closeErr := file.Close()
		removeErr := d.spool.remove(name)
		if closeErr != nil || removeErr != nil {
			return errors.Join(cause, &spoolIOError{err: errors.Join(closeErr, removeErr)})
		}
		return cause
	}
	h := sha256.New()
	buffer := make([]byte, 32<<10)
	var written int64
	for {
		if err = d.ctx.Err(); err != nil {
			return nil, discard(err)
		}
		readSize := len(buffer)
		remaining := limit - written
		if remaining < int64(readSize) {
			readSize = int(remaining)
		}
		if total != nil {
			totalRemaining := totalLimit - *total
			if totalRemaining < int64(readSize) {
				readSize = int(totalRemaining)
			}
		}
		if readSize < 1 {
			readSize = 1
		}
		n, readErr := reader.Read(buffer[:readSize])
		if n > 0 {
			observed := written + int64(n)
			if observed > limit {
				return nil, discard(&policyLimitError{code: limitCode, operation: operation, path: partPath, limit: limit, observed: limit + 1})
			}
			if total != nil && *total+int64(n) > totalLimit {
				return nil, discard(&policyLimitError{code: totalCode, operation: operation, path: partPath, limit: totalLimit, observed: totalLimit + 1})
			}
			count, writeErr := file.Write(buffer[:n])
			if writeErr != nil || count != n {
				if writeErr == nil {
					writeErr = io.ErrShortWrite
				}
				return nil, discard(&spoolIOError{err: writeErr})
			}
			_, _ = h.Write(buffer[:n])
			written += int64(n)
			if total != nil {
				*total += int64(n)
			}
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				break
			}
			return nil, discard(readErr)
		}
		if n == 0 {
			return nil, discard(io.ErrNoProgress)
		}
	}
	if err = file.Close(); err != nil {
		removeErr := d.spool.remove(name)
		return nil, &spoolIOError{err: errors.Join(err, removeErr)}
	}
	ref := document.EmailArtifactRefV1{Role: role, SHA256: hex.EncodeToString(h.Sum(nil)), Size: written}
	d.artifacts = append(d.artifacts, storedArtifact{artifact: Artifact{PartPath: partPath, Reference: ref}, filename: name})
	committed = true
	return &ref, nil
}

type contextReadCloser struct {
	ctx  context.Context
	file *os.File
}

func (r *contextReadCloser) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.file.Read(p)
}
func (r *contextReadCloser) Close() error { return r.file.Close() }

func (r *Result) Artifacts() []Artifact {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Artifact, len(r.artifacts))
	for i, item := range r.artifacts {
		out[i] = item.artifact
	}
	slices.SortFunc(out, func(a, b Artifact) int {
		if c := comparePartPaths(a.PartPath, b.PartPath); c != 0 {
			return c
		}
		return strings.Compare(string(a.Reference.Role), string(b.Reference.Role))
	})
	return out
}

func (r *Result) OpenArtifact(ctx context.Context, partPath, role string) (io.ReadCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := document.ValidateEmailPartPath(partPath); err != nil {
		return nil, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil, errors.New("email MIME result is closed")
	}
	for _, item := range r.artifacts {
		if item.artifact.PartPath == partPath && string(item.artifact.Reference.Role) == role {
			file, err := r.spool.openRegular(item.filename)
			if err != nil {
				return nil, err
			}
			return &contextReadCloser{ctx: ctx, file: file}, nil
		}
	}
	return nil, errors.New("email MIME artifact is not available")
}

func (r *Result) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil
	}
	if r.spool == nil {
		r.closed = true
		return nil
	}
	if err := r.spool.cleanup(); err != nil {
		return err
	}
	r.spool = nil
	r.closed = true
	return nil
}

func RecoverStale(ctx context.Context, spoolParent string) (removed int, resultErr error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	root, err := openStableSpoolRoot(spoolParent)
	if err != nil {
		return 0, fmt.Errorf("open rooted email spool parent: %w", err)
	}
	defer func() {
		if closeErr := root.Close(); closeErr != nil {
			resultErr = errors.Join(resultErr, &spoolIOError{err: closeErr})
		}
	}()
	entries, err := fs.ReadDir(root.FS(), ".")
	if err != nil {
		return 0, fmt.Errorf("read rooted email spool parent: %w", err)
	}
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return removed, err
		}
		if !strings.HasPrefix(entry.Name(), spoolPrefix) || entry.Type()&os.ModeSymlink != 0 || !entry.IsDir() {
			continue
		}
		markerPath := filepath.Join(entry.Name(), spoolMarkerName)
		markerInfo, statErr := root.Lstat(markerPath)
		if statErr != nil || !markerInfo.Mode().IsRegular() || markerInfo.Mode()&os.ModeSymlink != 0 || markerInfo.Size() != int64(len(spoolMarker)) {
			continue
		}
		markerFile, openErr := openRootRegular(root, markerPath)
		if openErr != nil {
			continue
		}
		markerMatches, readErr := ownershipMarkerMatches(markerFile)
		closeErr := markerFile.Close()
		if readErr != nil || closeErr != nil {
			return removed, &spoolIOError{err: errors.Join(readErr, closeErr)}
		}
		if !markerMatches {
			continue
		}
		if err := root.RemoveAll(entry.Name()); err != nil {
			return removed, fmt.Errorf("remove stale email spool %q: %w", entry.Name(), err)
		}
		removed++
	}
	return removed, nil
}

func comparePartPaths(a, b string) int {
	as := strings.Split(a, ".")
	bs := strings.Split(b, ".")
	for i := range min(len(as), len(bs)) {
		if len(as[i]) != len(bs[i]) {
			if len(as[i]) < len(bs[i]) {
				return -1
			}
			return 1
		}
		if as[i] != bs[i] {
			return strings.Compare(as[i], bs[i])
		}
	}
	return len(as) - len(bs)
}
