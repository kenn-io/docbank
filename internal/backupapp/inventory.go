package backupapp

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"go.kenn.io/kit/backup"
	"go.kenn.io/kit/pack"
	"go.kenn.io/kit/packstore"
)

// ReadSnapshotExtraFile streams one verified extra file from a Kit snapshot
// into a caller-owned path. It never restores the snapshot or claims a
// repository lease.
func ReadSnapshotExtraFile(ctx context.Context, repository *backup.Repo, snapshotID, name, dst string) error {
	if repository == nil || snapshotID == "" || name == "" || filepath.IsAbs(name) || strings.Contains(name, "..") {
		return errors.New("invalid snapshot extra request")
	}
	manifest, err := repository.LoadManifest(snapshotID)
	if err != nil {
		return fmt.Errorf("load snapshot manifest: %w", err)
	}
	if manifest.Extras.Tree == "" {
		return fmt.Errorf("snapshot %s has no extras tree", snapshotID)
	}
	treeID, err := pack.ParseBlobID(manifest.Extras.Tree)
	if err != nil {
		return fmt.Errorf("parse extras tree: %w", err)
	}
	known, err := repository.LoadBlobIndex()
	if err != nil {
		return fmt.Errorf("load snapshot blob index: %w", err)
	}
	treeStream, err := repository.OpenBlob(ctx, known, treeID, nil, packstore.PackExt)
	if err != nil {
		return fmt.Errorf("open extras tree: %w", err)
	}
	treeBytes, readErr := io.ReadAll(treeStream)
	verifyErr := treeStream.Verify()
	closeErr := treeStream.Close()
	if err := errors.Join(readErr, verifyErr, closeErr); err != nil {
		return fmt.Errorf("read extras tree: %w", err)
	}
	var tree backup.ExtrasTree
	if err := json.Unmarshal(treeBytes, &tree); err != nil {
		return fmt.Errorf("decode extras tree: %w", err)
	}
	var entry *backup.ExtrasEntry
	for i := range tree.Entries {
		if tree.Entries[i].Path == name {
			entry = &tree.Entries[i]
			break
		}
	}
	if entry == nil {
		return fmt.Errorf("snapshot extra %q is missing", name)
	}
	blobID, err := pack.ParseBlobID(entry.Blob)
	if err != nil {
		return fmt.Errorf("parse extra %q blob: %w", name, err)
	}
	stream, err := repository.OpenBlob(ctx, known, blobID, nil, packstore.PackExt)
	if err != nil {
		return fmt.Errorf("open extra %q: %w", name, err)
	}
	if stream.Size() != entry.Size {
		_ = stream.Close()
		return fmt.Errorf("extra %q size is %d, expected %d", name, stream.Size(), entry.Size)
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		_ = stream.Close()
		return fmt.Errorf("create extra destination: %w", err)
	}
	file, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		_ = stream.Close()
		return fmt.Errorf("create extra destination: %w", err)
	}
	_, copyErr := io.Copy(file, stream)
	verifyErr = stream.Verify()
	closeErr = errors.Join(stream.Close(), file.Close())
	if err := errors.Join(copyErr, verifyErr, closeErr); err != nil {
		_ = os.Remove(dst)
		return fmt.Errorf("read extra %q: %w", name, err)
	}
	return nil
}
