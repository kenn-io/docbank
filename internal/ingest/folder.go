package ingest

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"go.kenn.io/docbank/internal/config"
	"go.kenn.io/docbank/internal/store"
)

// ErrRetryLater asks a folder scan to observe a file again on a later scan,
// after a fresh settle window. A one-shot scan stops with it instead.
var ErrRetryLater = errors.New("folder file was not processed")

// SourceAccessError marks a failure to open, read, or recheck the source file
// itself, as opposed to any other error from the file processor. A processor
// returns it for read failures, and the scanner applies one policy to all of
// them: watch mode retries a transient failure, such as a Windows sharing or
// lock violation, after a fresh settle window; any other failure, and any
// failure in a one-shot scan, is reported once and the scan continues.
type SourceAccessError struct{ Err error }

func (e *SourceAccessError) Error() string { return e.Err.Error() }
func (e *SourceAccessError) Unwrap() error { return e.Err }

// FolderFile is a confined regular source opened after its settle window.
// The callback may read and seek it, but must not modify it or retain it.
type FolderFile struct {
	Ref  string
	File *os.File
	Info fs.FileInfo
	// Scan numbers the pass that admitted the file, starting at 1. Files from
	// one pass share it, so a caller can group one pass's work.
	Scan   uint64
	check  func() error
	digest *string
}

// Digest hashes the opened source and records the bytes observed by its scanner.
// It leaves the file at EOF and rejects metadata changes during the read.
func (file FolderFile) Digest(ctx context.Context) (string, error) {
	hash := sha256.New()
	n, err := io.Copy(hash, progressReader{ctx: ctx, Reader: file.File})
	if err != nil {
		if ctx.Err() != nil {
			return "", err
		}
		return "", &SourceAccessError{Err: fmt.Errorf("hashing source: %w", err)}
	}
	if n != file.Info.Size() {
		return "", ErrSourceChanged
	}
	after, err := file.File.Stat()
	if err != nil {
		return "", &SourceAccessError{Err: fmt.Errorf("checking hashed source: %w", err)}
	}
	if after.Size() != file.Info.Size() || !after.ModTime().Equal(file.Info.ModTime()) {
		return "", ErrSourceChanged
	}
	digest := hex.EncodeToString(hash.Sum(nil))
	if file.digest != nil {
		*file.digest = digest
	}
	return digest, nil
}

// Unchanged returns ErrSourceChanged when the open file or the file now at its
// path differs from the observation that admitted it. Call it before treating
// a failure as caused by the file's admitted contents.
func (file FolderFile) Unchanged() error { return file.check() }

func (source watchSource) digest(ctx context.Context, expected watchFingerprint) (string, error) {
	f, err := source.open()
	if err != nil {
		return "", &SourceAccessError{Err: err}
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return "", &SourceAccessError{Err: err}
	}
	if !expected.matches(fingerprintFileInfo(info)) {
		return "", ErrSourceChanged
	}
	return (FolderFile{File: f, Info: info}).Digest(ctx)
}

// FolderScanner shares watched-inbox traversal, literal exclusions, mount
// boundaries and stability observations without opening a local vault.
type FolderScanner struct {
	watcher *Watcher
	scans   uint64
}

// NewFolderScanner creates a read-only client-side scanner. ScanOnce admits
// files immediately; Run requires a full settle window, including on restart.
// Run rechecks digests recorded by FolderFile.Digest and starts a fresh settle
// window when the bytes change without a metadata change.
// reject receives each file or subdirectory that cannot be read, once until it
// becomes readable or disappears, and the scan continues with the rest.
func NewFolderScanner(
	cfg config.WatchConfig, process func(context.Context, FolderFile) error,
	reject func(ctx context.Context, ref string, err error) error,
) (*FolderScanner, error) {
	if process == nil || reject == nil {
		return nil, errors.New("folder scanner requires a file processor and a rejection handler")
	}
	if err := store.ValidatePushName(cfg.Name); err != nil {
		return nil, err
	}
	if err := validateVirtualDestination(cfg.Destination); err != nil {
		return nil, err
	}
	if cfg.SettleTime.Std() <= 0 || cfg.ScanInterval.Std() <= 0 || cfg.MinimumAge.Std() < 0 {
		return nil, errors.New("settle time and scan interval must be positive; minimum age must not be negative")
	}
	excludes, err := compileExclusions(cfg.Exclude)
	if err != nil {
		return nil, err
	}
	w := &Watcher{config: cfg, excludes: excludes, now: time.Now, logger: slog.Default(), observations: make(map[string]watchObservation), reject: reject}
	scanner := &FolderScanner{watcher: w}
	w.process = func(ctx context.Context, ref string, observation *watchObservation, open localFileOpener) error {
		expected := observation.fingerprint
		f, err := open()
		if err != nil {
			return &SourceAccessError{Err: err}
		}
		defer func() { _ = f.Close() }()
		before, err := f.Stat()
		if err != nil {
			return &SourceAccessError{Err: fmt.Errorf("checking folder file: %w", err)}
		}
		if !expected.matches(fingerprintFileInfo(before)) {
			return ErrSourceChanged
		}
		file := FolderFile{Ref: ref, File: f, Info: before, Scan: scanner.scans, digest: &observation.digest, check: func() error {
			after, statErr := f.Stat()
			if statErr != nil {
				return &SourceAccessError{Err: fmt.Errorf("rechecking folder file: %w", statErr)}
			}
			if !expected.matches(fingerprintFileInfo(after)) {
				return ErrSourceChanged
			}
			return checkFolderSource(expected, open)
		}}
		if processErr := process(ctx, file); processErr != nil {
			if errors.Is(file.Unchanged(), ErrSourceChanged) {
				return ErrSourceChanged
			}
			return processErr
		}
		return file.Unchanged()
	}
	return scanner, nil
}

func (s *FolderScanner) openRoot() (*watchRoot, error) {
	resolved, err := filepath.EvalSymlinks(s.watcher.config.Source)
	if err != nil {
		return nil, fmt.Errorf("resolving push folder: %w", err)
	}
	resolved, err = filepath.Abs(resolved)
	if err != nil {
		return nil, fmt.Errorf("resolving push folder: %w", err)
	}
	before, err := os.Stat(resolved)
	if err != nil {
		return nil, fmt.Errorf("checking push folder: %w", err)
	}
	if !before.IsDir() {
		return nil, errors.New("push source must be a directory")
	}
	root, err := os.OpenRoot(resolved)
	if err != nil {
		return nil, fmt.Errorf("opening push folder: %w", err)
	}
	success := false
	defer func() {
		if !success {
			_ = root.Close()
		}
	}()
	opened, err := root.Stat(".")
	if err != nil {
		return nil, fmt.Errorf("checking pinned push folder: %w", err)
	}
	pinned := &watchRoot{path: resolved, root: root, identity: opened}
	if !os.SameFile(before, opened) {
		return nil, ErrSourceChanged
	}
	if err := pinned.verify(); err != nil {
		return nil, err
	}
	s.watcher.sourceMount, err = watchMountForRoot(root)
	if err != nil {
		return nil, fmt.Errorf("identifying push filesystem: %w", err)
	}
	success = true
	return pinned, nil
}

// ScanOnce processes each selected regular file once without a settle delay.
func (s *FolderScanner) ScanOnce(ctx context.Context) error {
	root, err := s.openRoot()
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	s.watcher.immediate = true
	s.watcher.observations = make(map[string]watchObservation)
	s.watcher.rejected = make(map[string]struct{})
	s.scans++
	if err := s.watcher.scan(ctx, root); err != nil {
		return err
	}
	return root.verify()
}

func checkFolderSource(expected watchFingerprint, open localFileOpener) error {
	f, err := open()
	if errors.Is(err, ErrSourceChanged) || errors.Is(err, fs.ErrNotExist) {
		return ErrSourceChanged
	}
	if err != nil {
		return &SourceAccessError{Err: fmt.Errorf("reopening folder file: %w", err)}
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if errors.Is(err, ErrSourceChanged) || errors.Is(err, fs.ErrNotExist) {
		return ErrSourceChanged
	}
	if err != nil {
		return &SourceAccessError{Err: fmt.Errorf("rechecking folder file: %w", err)}
	}
	if !expected.matches(fingerprintFileInfo(info)) {
		return ErrSourceChanged
	}
	return nil
}

// Run scans immediately and polls until cancellation or a terminal error.
func (s *FolderScanner) Run(ctx context.Context) error {
	root, err := s.openRoot()
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	s.watcher.immediate = false
	s.watcher.observations = make(map[string]watchObservation)
	s.watcher.rejected = make(map[string]struct{})
	s.scans++
	if err := s.watcher.scan(ctx, root); err != nil {
		return err
	}
	ticker := time.NewTicker(s.watcher.config.ScanInterval.Std())
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			s.scans++
			if err := s.watcher.scan(ctx, root); err != nil {
				return err
			}
		}
	}
}
