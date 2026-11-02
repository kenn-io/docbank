package atomicfile

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"go.kenn.io/kit/fslink"
	"go.kenn.io/kit/safefileio"
)

// maxLinkHops bounds WithFollowLink resolution, matching the common Unix
// SYMLOOP_MAX.
const maxLinkHops = 40

// ErrPublished marks a failure that happened after the new content became
// visible at the target. Callers must not retry blindly: a retry of WriteNew
// fails with fs.ErrExist, and a retry of WriteFile rewrites content that is
// already visible. An error wrapping only ErrPublished, not ErrNotDurable,
// means the target is durable and only cleanup failed, such as removing
// WriteNew's leftover staging name. Errors wrapping ErrPublished also wrap
// the underlying cause.
var ErrPublished = errors.New("atomicfile: published")

// ErrNotDurable marks a post-publication directory sync failure: the target
// already holds the new content, but a crash may still lose it. It wraps
// ErrPublished.
var ErrNotDurable = fmt.Errorf("%w but not confirmed durable", ErrPublished)

var (
	errTooManyLinks = errors.New("too many levels of symlinks or junctions")
	errIsDir        = errors.New("target is a directory")
	errFinished     = errors.New("file already committed or aborted")
	errLinkChanged  = errors.New("link changed since Create")
)

// syncDir and removeFile are replaced by tests to inject failures after
// publication.
var (
	syncDir    = SyncDir
	removeFile = os.Remove
)

// WriteFile atomically replaces the file at path with data. It stages data in
// a temporary file, fsyncs and closes it, renames it over path, and fsyncs
// path's directory (and the staging directory when WithStagingDir names a
// different one). Readers see the old or the new content, never a partial
// file. Parent directories are not created.
//
// An error wrapping ErrPublished means the new content is already visible at
// path; see ErrPublished and ErrNotDurable. The link refusal is a check, not
// an atomic guarantee; see Create.
func WriteFile(path string, data []byte, opts ...Option) (err error) {
	file, err := Create(path, opts...)
	if err != nil {
		return err
	}
	defer func() {
		err = errors.Join(err, file.Abort())
	}()
	if _, err := file.Write(data); err != nil {
		return fmt.Errorf("atomicfile: write %s: %w", path, err)
	}
	return file.Commit()
}

// File is a staged replacement for a target file. Content written to it
// becomes visible at the target when Commit publishes it: on success, or on an
// error wrapping ErrPublished, which means the content is visible but a later
// step failed. Callers should defer Abort, which is a no-op after
// Commit.
type File struct {
	file   *os.File
	path   string
	target string
	cfg    config
	done   bool
}

// Create stages a replacement for the file at path. See WriteFile for the
// publication guarantees. The target is checked now and again at Commit: a
// link (unless WithFollowLink) or a directory at the target fails. The
// rename itself never follows a link, so a link swapped in after the Commit
// check is replaced rather than written through.
//
// atomicfile assumes the target and staging directories cannot be modified by
// untrusted users. The link refusal is a check at Create and just before
// publication, not an atomic guarantee against a link installed concurrently:
// the check and the rename are separate steps, and the staging file is handled
// by pathname. Anyone who can rename entries in those directories can replace
// the published file directly anyway.
func Create(path string, opts ...Option) (*File, error) {
	cfg, err := newConfig(opts)
	if err != nil {
		return nil, fmt.Errorf("atomicfile: create %s: %w", path, err)
	}
	target, err := resolveTarget(path, cfg.followLink)
	if err != nil {
		return nil, fmt.Errorf("atomicfile: create %s: %w", path, err)
	}
	info, err := checkTarget(target)
	if err != nil {
		return nil, fmt.Errorf("atomicfile: create %s: %w", path, err)
	}
	perm, umask := cfg.perm, cfg.createPerm
	if cfg.preserveMode && info != nil && info.Mode().IsRegular() {
		perm, umask = info.Mode().Perm(), false
	}
	staged, err := stage(cfg, target, perm, umask)
	if err != nil {
		return nil, fmt.Errorf("atomicfile: create %s: %w", path, err)
	}
	return &File{file: staged, path: path, target: target, cfg: cfg}, nil
}

// Name returns the target path passed to Create.
func (f *File) Name() string { return f.path }

// TempName returns the path of the staging file.
func (f *File) TempName() string { return f.file.Name() }

// Write writes to the staging file.
func (f *File) Write(p []byte) (int, error) { return f.file.Write(p) }

// WriteString writes s to the staging file.
func (f *File) WriteString(s string) (int, error) { return f.file.WriteString(s) }

// ReadFrom copies r into the staging file.
func (f *File) ReadFrom(r io.Reader) (int64, error) { return f.file.ReadFrom(r) }

// Commit fsyncs and closes the staging file, renames it over the target, and
// fsyncs the target's directory (and a different staging directory). On
// failure before the rename the staging file is removed and the target is
// untouched. With WithFollowLink the link chain is resolved again first, and
// Commit fails without writing if it now leads somewhere other than at
// Create. An error wrapping ErrPublished means the new content is already
// visible at the target; do not retry blindly. Commit may be called once;
// later calls, or a call after Abort, return an error.
func (f *File) Commit() error {
	if f.done {
		return fmt.Errorf("atomicfile: commit %s: %w", f.path, errFinished)
	}
	f.done = true
	if err := f.publish(); err != nil {
		return fmt.Errorf("atomicfile: commit %s: %w", f.path, err)
	}
	return nil
}

func (f *File) publish() error {
	err := finishStaged(f.file, f.cfg)
	if err == nil && f.cfg.followLink {
		err = f.checkLinkUnchanged()
	}
	if err == nil {
		_, err = checkTarget(f.target)
	}
	if err == nil {
		err = Replace(f.file.Name(), f.target)
	}
	if err != nil {
		return discardStaged(f.file, err)
	}
	return syncPublished(f.cfg, f.target, nil)
}

// checkLinkUnchanged resolves the original path's link chain again and fails
// when it no longer leads to the destination chosen at Create.
func (f *File) checkLinkUnchanged() error {
	target, err := resolveTarget(f.path, true)
	if err != nil {
		return err
	}
	if target != f.target {
		return fmt.Errorf("%w: resolved to %s at Create, now %s", errLinkChanged, f.target, target)
	}
	return nil
}

// syncPublished fsyncs the target's directory and, when it differs, the
// staging directory, both of which publication changed. A sync failure is
// reported as ErrNotDurable; a cleanup failure after a successful publish,
// passed in as cleanupErr, is reported as ErrPublished alone.
func syncPublished(cfg config, target string, cleanupErr error) error {
	var syncErrs []error
	if !cfg.noSync {
		dir := filepath.Dir(target)
		syncErrs = append(syncErrs, syncDir(dir))
		if cfg.stagingDir != "" && !sameDir(cfg.stagingDir, dir) {
			syncErrs = append(syncErrs, syncDir(cfg.stagingDir))
		}
	}
	if err := errors.Join(syncErrs...); err != nil {
		return fmt.Errorf("%w: %w", ErrNotDurable, errors.Join(err, cleanupErr))
	}
	if cleanupErr != nil {
		return fmt.Errorf("%w: %w", ErrPublished, cleanupErr)
	}
	return nil
}

// sameDir reports whether a and b are the same directory. It compares file
// identity, since the target's directory is resolved while a staging
// directory keeps the caller's spelling. When either cannot be inspected it
// reports false, so both get synced.
func sameDir(a, b string) bool {
	infoA, errA := os.Stat(a)
	infoB, errB := os.Stat(b)
	return errA == nil && errB == nil && os.SameFile(infoA, infoB)
}

// Abort closes and removes the staging file, leaving the target untouched.
// It is a no-op after Commit or a previous Abort, so callers can defer it.
func (f *File) Abort() error {
	if f.done {
		return nil
	}
	f.done = true
	if err := discardStaged(f.file, nil); err != nil {
		return fmt.Errorf("atomicfile: abort %s: %w", f.path, err)
	}
	return nil
}

// WriteNew atomically creates the file at path with data only when nothing
// exists there. The staged file is fsynced and published without replacing
// anything: with PublishNoReplace on Unix, and with RenameNoReplace on
// Windows, where a hard link is not written through. Any leftover staging
// name is removed, and path's directory (and a different staging directory)
// is fsynced. An existing entry at path, including a link, fails with an
// error wrapping fs.ErrExist and is left untouched. WithFollowLink is
// rejected.
//
// An error wrapping ErrPublished means path already holds data; a retry
// would fail with fs.ErrExist. atomicfile assumes path's directory and the
// staging directory cannot be modified by untrusted users; see Create.
func WriteNew(path string, data []byte, opts ...Option) error {
	if err := writeNew(path, data, opts); err != nil {
		return fmt.Errorf("atomicfile: write new %s: %w", path, err)
	}
	return nil
}

func writeNew(path string, data []byte, opts []Option) error {
	cfg, err := newConfig(opts)
	if err != nil {
		return err
	}
	if cfg.followLink {
		return errors.New("WithFollowLink cannot be used with WriteNew")
	}
	// Stage, publish and sync in the directory the kernel resolves path to,
	// as WriteFile does; see canonicalPath.
	path, err = canonicalPath(path)
	if err != nil {
		return err
	}
	staged, err := stage(cfg, path, cfg.perm, cfg.createPerm)
	if err != nil {
		return err
	}
	if _, err := staged.Write(data); err != nil {
		return discardStaged(staged, err)
	}
	if err := finishStaged(staged, cfg); err != nil {
		return discardStaged(staged, err)
	}
	if err := publishNew(staged.Name(), path); err != nil {
		return discardStaged(staged, err)
	}
	var removeErr error
	if err := removeFile(staged.Name()); err != nil && !errors.Is(err, fs.ErrNotExist) {
		removeErr = fmt.Errorf("remove staging file after publishing: %w", err)
	}
	return syncPublished(cfg, path, removeErr)
}

// PublishNoReplace publishes staging at final only when final does not
// exist. It first hard-links staging to final, which leaves staging in place
// for the caller to remove. Where hard links are unavailable it falls back to
// RenameNoReplace. Neither step replaces or copies over an existing final;
// when both fail the joined error wraps fs.ErrExist if final existed.
func PublishNoReplace(staging, final string) error {
	linkErr := linkFile(staging, final)
	if linkErr == nil {
		return nil
	}
	if renameErr := RenameNoReplace(staging, final); renameErr != nil {
		return errors.Join(
			fmt.Errorf("atomicfile: publish %s: hard link: %w", final, linkErr),
			fmt.Errorf("atomicfile: publish %s: no-replace rename: %w", final, renameErr),
		)
	}
	return nil
}

// linkFile is replaced by tests to force the PublishNoReplace fallback.
var linkFile = os.Link

// resolveTarget returns the path whose entry a replace should swap. A link at
// path's final component is refused unless follow is set, in which case the
// chain is resolved.
func resolveTarget(path string, follow bool) (string, error) {
	current, err := canonicalPath(path)
	if err != nil {
		return "", err
	}
	for range maxLinkHops + 1 {
		kind, err := fslink.Classify(current)
		if errors.Is(err, fs.ErrNotExist) {
			return current, nil
		}
		if err != nil {
			return "", err
		}
		if kind == fslink.NotLink {
			return current, nil
		}
		if !follow {
			return "", &fs.PathError{Op: "replace", Path: current, Err: fslink.ErrIsLink}
		}
		current, err = linkDest(current)
		if err != nil {
			return "", err
		}
	}
	return "", &fs.PathError{Op: "replace", Path: path, Err: errTooManyLinks}
}

// linkDest returns the path the link at link names, resolved the way the
// system resolves it; see resolveLinkDest.
func linkDest(link string) (string, error) {
	dest, err := fslink.Readlink(link)
	if err != nil {
		return "", err
	}
	return resolveLinkDest(filepath.Dir(link), dest)
}

// checkTarget fails when target's entry is a link or a directory and returns
// its Lstat info, or nil when it does not exist.
func checkTarget(target string) (fs.FileInfo, error) {
	kind, err := fslink.Classify(target)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if kind != fslink.NotLink {
		return nil, &fs.PathError{Op: "replace", Path: target, Err: fslink.ErrIsLink}
	}
	info, err := os.Lstat(target)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if info.IsDir() {
		return nil, &fs.PathError{Op: "replace", Path: target, Err: errIsDir}
	}
	return info, nil
}

// stage creates the staging file for target with perm, or a private file
// when cfg.private is set. With umask the process umask filters perm, as it
// does for os.WriteFile; otherwise perm is applied exactly.
func stage(cfg config, target string, perm fs.FileMode, umask bool) (*os.File, error) {
	dir, pattern := filepath.Dir(target), "."+filepath.Base(target)+".tmp-*"
	if cfg.stagingDir != "" {
		dir, pattern = cfg.stagingDir, filepath.Base(target)+".tmp-*"
	}
	if cfg.private {
		return safefileio.CreatePrivateTemp(dir, pattern)
	}
	if umask {
		return createTemp(dir, pattern, perm)
	}
	file, err := os.CreateTemp(dir, pattern)
	if err != nil {
		return nil, err
	}
	if err := file.Chmod(perm); err != nil {
		return nil, discardStaged(file, err)
	}
	return file, nil
}

// createTempAttempts matches the retry budget of os.CreateTemp.
const createTempAttempts = 10000

// createTemp is os.CreateTemp with a caller-chosen perm that the process
// umask filters, as os.OpenFile does. pattern holds one "*" and no separator.
func createTemp(dir, pattern string, perm fs.FileMode) (*os.File, error) {
	prefix, suffix, _ := strings.Cut(pattern, "*")
	for range createTempAttempts {
		name := filepath.Join(dir, prefix+strconv.FormatUint(uint64(rand.Uint32()), 10)+suffix)
		file, err := os.OpenFile(name, os.O_RDWR|os.O_CREATE|os.O_EXCL, perm)
		if errors.Is(err, fs.ErrExist) {
			continue
		}
		return file, err
	}
	return nil, &fs.PathError{Op: "createtemp", Path: filepath.Join(dir, pattern), Err: fs.ErrExist}
}

func finishStaged(file *os.File, cfg config) error {
	if !cfg.noSync {
		if err := file.Sync(); err != nil {
			return err
		}
	}
	return file.Close()
}

// discardStaged closes and removes a staging file, joining cleanup failures
// with cause. The file may already be closed.
func discardStaged(file *os.File, cause error) error {
	closeErr := file.Close()
	if errors.Is(closeErr, os.ErrClosed) {
		closeErr = nil
	}
	removeErr := os.Remove(file.Name())
	if errors.Is(removeErr, fs.ErrNotExist) {
		removeErr = nil
	}
	return errors.Join(cause, closeErr, removeErr)
}
