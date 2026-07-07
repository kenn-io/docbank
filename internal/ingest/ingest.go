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
	Added   int
	Skipped int
	Failed  []FileError
}

// AddPaths ingests files and directory trees under the virtual destPath.
// Per-file failures are collected in the report; the run continues.
func (ing *Ingester) AddPaths(ctx context.Context, sources []string, destPath string) (Report, error) {
	var rep Report
	dest, err := ing.Store.MkdirAll(ctx, destPath)
	if err != nil {
		return rep, fmt.Errorf("resolving destination %q: %w", destPath, err)
	}
	ingestID, err := ing.Store.BeginIngest(ctx, "cli", fmt.Sprintf("%v", sources))
	if err != nil {
		return rep, err
	}

	for _, src := range sources {
		info, err := os.Lstat(src)
		if err != nil {
			return rep, fmt.Errorf("reading source %s: %w", src, err)
		}
		switch {
		case info.Mode().IsRegular():
			ing.addOne(ctx, &rep, ingestID, dest.ID, src, info)
		case info.IsDir():
			if err := ing.addTree(ctx, &rep, ingestID, dest.ID, src); err != nil {
				return rep, err
			}
		default:
			rep.Failed = append(rep.Failed, FileError{Path: src,
				Err: errors.New("not a regular file or directory (symlinks are skipped)")})
		}
	}
	return rep, nil
}

// addTree imports srcRoot recursively; its basename becomes a directory
// under destDirID and relative structure is preserved.
func (ing *Ingester) addTree(ctx context.Context, rep *Report, ingestID, destDirID int64, srcRoot string) error {
	// Absolutize first. WalkDir hands back the root spelled exactly as given
	// while children come from filepath.Join, which cleans — dirIDs keys must
	// use one spelling. And a source spelled "." or ".." has no usable
	// basename: a ".." topName would climb out of the destination when
	// joined into the virtual path below.
	srcRoot, err := filepath.Abs(srcRoot)
	if err != nil {
		return fmt.Errorf("resolving source %s: %w", srcRoot, err)
	}
	topName := filepath.Base(srcRoot)
	if topName == string(filepath.Separator) {
		return fmt.Errorf("cannot import filesystem root %q", srcRoot)
	}
	destPath, err := ing.Store.Path(ctx, destDirID)
	if err != nil {
		return err
	}

	dirIDs := map[string]int64{} // source dir path -> virtual dir node id
	walkErr := filepath.WalkDir(srcRoot, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			rep.Failed = append(rep.Failed, FileError{Path: p, Err: err})
			return nil //nolint:nilerr // intentional: record error and continue walk
		}
		rel, err := filepath.Rel(srcRoot, p)
		if err != nil {
			return fmt.Errorf("computing relative path of %s: %w", p, err)
		}
		switch {
		case d.IsDir():
			virtual := filepath.ToSlash(filepath.Join(destPath, topName, rel))
			dir, err := ing.Store.MkdirAll(ctx, virtual)
			if err != nil {
				rep.Failed = append(rep.Failed, FileError{Path: p,
					Err: fmt.Errorf("creating virtual dir %q: %w", virtual, err)})
				return fs.SkipDir
			}
			dirIDs[p] = dir.ID
		case d.Type().IsRegular():
			info, err := d.Info()
			if err != nil {
				rep.Failed = append(rep.Failed, FileError{Path: p, Err: err})
				return nil //nolint:nilerr // intentional: record error and continue walk
			}
			parentID, ok := dirIDs[filepath.Dir(p)]
			if !ok {
				return fmt.Errorf("internal: no virtual dir recorded for %s", filepath.Dir(p))
			}
			ing.addOne(ctx, rep, ingestID, parentID, p, info)
		default:
			rep.Failed = append(rep.Failed, FileError{Path: p,
				Err: errors.New("not a regular file (symlinks are skipped)")})
		}
		return nil
	})
	return walkErr
}

// addOne imports a single regular file; failures land in the report.
func (ing *Ingester) addOne(ctx context.Context, rep *Report, ingestID, parentID int64, src string, info os.FileInfo) {
	added, err := ing.importFile(ctx, ingestID, parentID, src, info)
	switch {
	case err != nil:
		rep.Failed = append(rep.Failed, FileError{Path: src, Err: err})
	case added:
		rep.Added++
	default:
		rep.Skipped++
	}
}

func (ing *Ingester) importFile(ctx context.Context, ingestID, parentID int64, src string, info os.FileInfo) (bool, error) {
	f, err := os.Open(src)
	if err != nil {
		return false, fmt.Errorf("opening %s: %w", src, err)
	}
	defer func() { _ = f.Close() }()

	head := make([]byte, 512)
	n, err := io.ReadFull(f, head)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return false, fmt.Errorf("reading %s: %w", src, err)
	}
	head = head[:n]
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return false, fmt.Errorf("rewinding %s: %w", src, err)
	}

	// Blob first: the node row must never commit before its bytes are
	// durable. A skip after this point can orphan the blob — harmless,
	// reclaimed by gc.
	hash, size, err := ing.Blobs.Write(f)
	if err != nil {
		return false, fmt.Errorf("storing content of %s: %w", src, err)
	}

	mtime := info.ModTime().UTC().Format(time.RFC3339Nano)
	_, added, err := ing.Store.IngestFile(ctx, ingestID, parentID,
		filepath.Base(src), hash, size, detectMime(src, head), src, mtime)
	if err != nil {
		return false, fmt.Errorf("recording %s: %w", src, err)
	}
	return added, nil
}
