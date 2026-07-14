// Package ingest implements the single import pipeline shared by all entry
// points: hash → durable blob write → one metadata transaction per file.
package ingest

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"go.kenn.io/docbank/internal/blob"
	"go.kenn.io/docbank/internal/store"
)

var (
	// ErrUploadDigestMismatch reports bytes that do not match the identity the
	// remote writer declared. The physical write may leave an authority-free
	// object for GC, but no blobs row or node is committed.
	ErrUploadDigestMismatch = errors.New("upload digest mismatch")
	// ErrUploadSizeMismatch is the corresponding declared-length failure.
	ErrUploadSizeMismatch = errors.New("upload size mismatch")
)

// Ingester wires the metadata store to the blob store.
type Ingester struct {
	Store *store.Store
	Blobs *blob.Store
}

// FileError records a per-file ingest failure.
type FileError struct {
	Path string
	Err  error
}

// Report summarizes an ingest run.
type Report struct {
	Added    int
	Skipped  int
	Excluded int
	Failed   []FileError
}

// ProgressEvent reports bytes read and file outcomes observed so far. A file
// contributes to FilesDone only after its blob and metadata operation returns;
// BytesRead may include an incomplete file when the operation is cancelled.
type ProgressEvent struct {
	FilesDone int64
	BytesRead int64
	Added     int
	Skipped   int
	Excluded  int
	Failed    int
	Final     bool
}

const (
	progressByteInterval = 4 << 20
	progressFileInterval = 64
	progressTimeInterval = time.Second
)

type progressTracker struct {
	notify               func(ProgressEvent)
	event                ProgressEvent
	lastNotifiedBytes    int64
	lastNotifiedFiles    int64
	lastNotifiedExcluded int
	lastNotifiedFailed   int
	lastNotifiedAt       time.Time
}

func newProgressTracker(notify func(ProgressEvent)) *progressTracker {
	if notify == nil {
		return nil
	}
	return &progressTracker{notify: notify}
}

func (p *progressTracker) addBytes(n int) {
	if p == nil || n <= 0 {
		return
	}
	p.event.BytesRead += int64(n)
	if p.event.BytesRead-p.lastNotifiedBytes >= progressByteInterval ||
		time.Since(p.lastNotifiedAt) >= progressTimeInterval {
		p.emit(false)
	}
}

func (p *progressTracker) report(rep Report, final bool) {
	if p == nil {
		return
	}
	p.event.FilesDone = int64(rep.Added + rep.Skipped + len(rep.Failed))
	p.event.Added = rep.Added
	p.event.Skipped = rep.Skipped
	p.event.Excluded = rep.Excluded
	p.event.Failed = len(rep.Failed)
	if p.lastNotifiedAt.IsZero() || final ||
		p.event.FilesDone-p.lastNotifiedFiles >= progressFileInterval ||
		p.event.Excluded-p.lastNotifiedExcluded >= progressFileInterval ||
		p.event.Failed > p.lastNotifiedFailed ||
		time.Since(p.lastNotifiedAt) >= progressTimeInterval {
		p.emit(final)
	}
}

func (p *progressTracker) emit(final bool) {
	if p == nil {
		return
	}
	p.event.Final = final
	p.lastNotifiedBytes = p.event.BytesRead
	p.lastNotifiedFiles = p.event.FilesDone
	p.lastNotifiedExcluded = p.event.Excluded
	p.lastNotifiedFailed = p.event.Failed
	p.lastNotifiedAt = time.Now()
	p.notify(p.event)
}

type progressReader struct {
	io.Reader

	ctx     context.Context
	tracker *progressTracker
}

func (r progressReader) Read(buf []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	n, err := r.Reader.Read(buf)
	r.tracker.addBytes(n)
	if err == nil {
		err = r.ctx.Err()
	}
	return n, err
}

// UploadResult is the server's independently computed receipt for one remote
// file. Node is populated for both a new import and an idempotent retry.
type UploadResult struct {
	Node         store.Node
	Added        bool
	ComputedHash string
	ComputedSize int64
}

// PreparedUpload is a verified, authority-free remote write. The caller may
// validate the remainder of its transport envelope before Commit grants blob
// and node authority.
type PreparedUpload struct {
	ing      *Ingester
	parentID int64
	name     string
	mimeType string
	result   UploadResult
}

// PrepareUpload streams one remote file through Kit's durable writer and
// checks the independently declared identity. It intentionally stops before
// inserting application metadata so callers can reject a malformed trailing
// multipart envelope without partially accepting the request.
func (ing *Ingester) PrepareUpload(
	ctx context.Context, parentID int64, name, mimeType string, r io.Reader,
	expectedHash string, expectedSize int64,
) (*PreparedUpload, error) {
	var result UploadResult
	name, err := store.NormalizeName(name)
	if err != nil {
		return nil, err
	}
	parent, err := ing.Store.NodeByID(ctx, parentID)
	if err != nil {
		return nil, err
	}
	if parent.TrashedAt != nil {
		return nil, store.ErrNotFound
	}
	if !parent.IsDir() {
		return nil, store.ErrNotDir
	}

	result.ComputedHash, result.ComputedSize, err = ing.Blobs.WriteContext(ctx, r)
	if err != nil {
		return nil, err
	}
	if result.ComputedSize != expectedSize {
		return nil, fmt.Errorf("declared %d bytes, received %d: %w",
			expectedSize, result.ComputedSize, ErrUploadSizeMismatch)
	}
	if result.ComputedHash != expectedHash {
		return nil, fmt.Errorf("declared SHA-256 %s, computed %s: %w",
			expectedHash, result.ComputedHash, ErrUploadDigestMismatch)
	}
	return &PreparedUpload{
		ing: ing, parentID: parentID, name: name, mimeType: mimeType, result: result,
	}, nil
}

// Commit grants application authority to a prepared upload and returns the
// stable node for either a new import or an idempotent retry.
func (p *PreparedUpload) Commit(ctx context.Context) (UploadResult, error) {
	result := p.result
	ingestID, err := p.ing.Store.BeginIngest(ctx, "upload", p.name)
	if err != nil {
		return result, err
	}
	result.Node, result.Added, err = p.ing.Store.IngestFile(ctx, ingestID, p.parentID,
		p.name, result.ComputedHash, result.ComputedSize, p.mimeType, p.name, "")
	if err != nil {
		return result, err
	}
	return result, nil
}

// AddPaths ingests files and directory trees under the virtual destPath.
// Per-file failures are collected in the report; the run continues.
func (ing *Ingester) AddPaths(ctx context.Context, sources []string, destPath string) (Report, error) {
	return ing.AddPathsWithOptions(ctx, sources, destPath, Options{})
}

// AddPathsWithOptions ingests the selected parts of files and directory trees
// under the virtual destPath. Exclusions have the same semantics as Preflight.
func (ing *Ingester) AddPathsWithOptions(
	ctx context.Context,
	sources []string,
	destPath string,
	opts Options,
) (rep Report, err error) {
	progress := newProgressTracker(opts.Progress)
	progress.report(rep, false)
	if err := ctx.Err(); err != nil {
		return rep, err
	}
	excludes, err := compileExclusions(opts.Exclude)
	if err != nil {
		return rep, err
	}
	dest, err := ing.Store.MkdirAll(ctx, destPath)
	if err != nil {
		return rep, fmt.Errorf("resolving destination %q: %w", destPath, err)
	}
	ingestID, err := ing.Store.BeginIngest(ctx, "cli", fmt.Sprintf("%v", sources))
	if err != nil {
		return rep, err
	}

	for _, rawSource := range sources {
		if err := ctx.Err(); err != nil {
			return rep, err
		}
		src := filepath.Clean(rawSource)
		info, err := os.Lstat(src)
		if err != nil {
			// Per-file failure like any other: aborting here would leave
			// already-imported files out of the report entirely.
			rep.Failed = append(rep.Failed, FileError{Path: src, Err: err})
			progress.report(rep, false)
			continue
		}
		if excludes.match(src, src) {
			rep.Excluded++
			progress.report(rep, false)
			continue
		}
		switch {
		case info.Mode().IsRegular():
			if err := ing.addOne(ctx, &rep, ingestID, dest.ID, src, src, progress); err != nil {
				return rep, err
			}
		case info.IsDir():
			if err := ing.addTree(ctx, &rep, ingestID, dest.ID, src, src, excludes, progress); err != nil {
				return rep, err
			}
		case info.Mode()&fs.ModeSymlink != 0:
			walkRoot, err := filepath.EvalSymlinks(src)
			if err != nil {
				rep.Failed = append(rep.Failed, FileError{Path: src,
					Err: fmt.Errorf("resolving explicitly named directory symlink: %w", err)})
				progress.report(rep, false)
				continue
			}
			target, err := os.Stat(walkRoot)
			if err != nil {
				rep.Failed = append(rep.Failed, FileError{Path: src,
					Err: fmt.Errorf("checking explicitly named directory symlink: %w", err)})
				progress.report(rep, false)
				continue
			}
			if !target.IsDir() {
				rep.Failed = append(rep.Failed, FileError{Path: src,
					Err: errors.New("explicit symlink source does not resolve to a directory")})
				progress.report(rep, false)
				continue
			}
			if err := ing.addTree(ctx, &rep, ingestID, dest.ID, src, walkRoot, excludes, progress); err != nil {
				return rep, err
			}
		default:
			rep.Failed = append(rep.Failed, FileError{Path: src,
				Err: errors.New("not a regular file or directory (symlinks are skipped)")})
			progress.report(rep, false)
		}
	}
	progress.report(rep, true)
	return rep, nil
}

// addTree imports walkRoot recursively; sourceRoot's basename becomes a
// directory under destDirID and relative structure is preserved. The roots
// differ only when the user explicitly supplied a symlink to a directory:
// traversal uses its resolved target while provenance retains the supplied
// spelling.
//
// Traversal is by path (WalkDir): swapping an already-classified directory
// for a symlink mid-walk can redirect descent outside walkRoot. That race is
// accepted — docbank is single-user and imports the user's own trees, so a
// process able to race the walk already runs as the user; importFile's
// no-follow open covers the accidental symlink case.
func (ing *Ingester) addTree(
	ctx context.Context,
	rep *Report,
	ingestID, destDirID int64,
	sourceRoot, walkRoot string,
	excludes exclusions,
	progress *progressTracker,
) error {
	// Absolutize first. WalkDir hands back the root spelled exactly as given
	// while children come from filepath.Join, which cleans — dirIDs keys must
	// use one spelling. And a source spelled "." or ".." has no usable
	// basename: a ".." topName would climb out of the destination when
	// joined into the virtual path below.
	sourceRoot, err := filepath.Abs(sourceRoot)
	if err != nil {
		return fmt.Errorf("resolving source %s: %w", sourceRoot, err)
	}
	walkRoot, err = filepath.Abs(walkRoot)
	if err != nil {
		return fmt.Errorf("resolving traversal root %s: %w", walkRoot, err)
	}
	topName := filepath.Base(sourceRoot)
	if topName == string(filepath.Separator) || filepath.Base(walkRoot) == string(filepath.Separator) {
		return fmt.Errorf("cannot import filesystem root %q", sourceRoot)
	}

	// Parentage stays ID-based throughout: every directory is created under
	// the resolved id of its parent, never by re-deriving a path from
	// destDirID — a concurrent move or trash of the destination would make
	// that path re-create (even resurrect) a tree somewhere else.
	dirIDs := map[string]int64{} // source dir path -> virtual dir node id
	walkErr := filepath.WalkDir(walkRoot, func(p string, d fs.DirEntry, err error) error {
		sourcePath := sourceTreePath(sourceRoot, walkRoot, p)
		if err != nil {
			rep.Failed = append(rep.Failed, FileError{Path: sourcePath, Err: err})
			progress.report(*rep, false)
			return nil //nolint:nilerr // intentional: record error and continue walk
		}
		if excludes.match(sourceRoot, sourcePath) {
			rep.Excluded++
			progress.report(*rep, false)
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		switch {
		case d.IsDir():
			parentID, name := destDirID, topName
			if p != walkRoot {
				pid, ok := dirIDs[filepath.Dir(p)]
				if !ok {
					return fmt.Errorf("internal: no virtual dir recorded for %s", filepath.Dir(p))
				}
				parentID, name = pid, d.Name()
			}
			dir, err := ing.Store.EnsureDir(ctx, parentID, name)
			if err != nil {
				rep.Failed = append(rep.Failed, FileError{Path: sourcePath,
					Err: fmt.Errorf("creating virtual dir %q under node %d: %w", name, parentID, err)})
				progress.report(*rep, false)
				return fs.SkipDir
			}
			dirIDs[p] = dir.ID
		case d.Type().IsRegular():
			parentID, ok := dirIDs[filepath.Dir(p)]
			if !ok {
				return fmt.Errorf("internal: no virtual dir recorded for %s", filepath.Dir(p))
			}
			if err := ing.addOne(ctx, rep, ingestID, parentID, p, sourcePath, progress); err != nil {
				return err
			}
		default:
			rep.Failed = append(rep.Failed, FileError{Path: sourcePath,
				Err: errors.New("not a regular file (symlinks are skipped)")})
			progress.report(*rep, false)
		}
		return nil
	})
	return walkErr
}

func sourceTreePath(sourceRoot, walkRoot, walkPath string) string {
	rel, err := filepath.Rel(walkRoot, walkPath)
	if err != nil || rel == "." {
		return sourceRoot
	}
	return filepath.Join(sourceRoot, rel)
}

// addOne imports a single regular file; failures land in the report.
func (ing *Ingester) addOne(
	ctx context.Context,
	rep *Report,
	ingestID, parentID int64,
	openPath, sourcePath string,
	progress *progressTracker,
) error {
	added, err := ing.importFile(ctx, ingestID, parentID, openPath, sourcePath, progress)
	switch {
	case err != nil:
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		rep.Failed = append(rep.Failed, FileError{Path: sourcePath, Err: err})
	case added:
		rep.Added++
	default:
		rep.Skipped++
	}
	progress.report(*rep, false)
	return nil
}

func (ing *Ingester) importFile(
	ctx context.Context,
	ingestID, parentID int64,
	openPath, sourcePath string,
	progress *progressTracker,
) (bool, error) {
	// No-follow plus fstat, not the earlier Lstat/WalkDir classification:
	// the file could have been swapped since, and "symlinks are skipped"
	// must hold for the file actually read, not the one classified.
	f, err := blob.OpenNoFollow(openPath)
	if err != nil {
		return false, fmt.Errorf("opening %s: %w", sourcePath, err)
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return false, fmt.Errorf("checking %s: %w", sourcePath, err)
	}
	if !info.Mode().IsRegular() {
		return false, fmt.Errorf("%s: not a regular file", sourcePath)
	}

	head := make([]byte, 512)
	n, err := io.ReadFull(f, head)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return false, fmt.Errorf("reading %s: %w", sourcePath, err)
	}
	head = head[:n]
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return false, fmt.Errorf("rewinding %s: %w", sourcePath, err)
	}

	// Blob first: the node row must never commit before its bytes are
	// durable. A skip, metadata failure, or crash after this point can
	// orphan the blob file — harmless, reclaimed by gc's untracked-file
	// scan.
	var content io.Reader = f
	if progress != nil {
		content = progressReader{Reader: f, ctx: ctx, tracker: progress}
	}
	hash, size, err := ing.Blobs.WriteContext(ctx, content)
	if err != nil {
		return false, fmt.Errorf("storing content of %s: %w", sourcePath, err)
	}

	mtime := info.ModTime().UTC().Format(time.RFC3339Nano)
	_, added, err := ing.Store.IngestFile(ctx, ingestID, parentID,
		filepath.Base(sourcePath), hash, size, detectMime(sourcePath, head), sourcePath, mtime)
	if err != nil {
		return false, fmt.Errorf("recording %s: %w", sourcePath, err)
	}
	return added, nil
}
