package backupapp

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"math"
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

// SnapshotUniqueBlobBytes reads verified logical blob metadata without
// restoring the snapshot or opening a destination store.
func SnapshotUniqueBlobBytes(ctx context.Context, repository *backup.Repo, manifest *backup.Manifest) (total int64, retErr error) {
	if repository == nil || manifest == nil || manifest.Metadata == nil || manifest.Metadata.Format != MetadataFormat {
		return 0, errors.New("snapshot has unsupported Docbank metadata")
	}
	metadataID, err := pack.ParseBlobID(manifest.Metadata.Blob)
	if err != nil {
		return 0, fmt.Errorf("parse metadata blob: %w", err)
	}
	known, err := repository.LoadBlobIndex()
	if err != nil {
		return 0, fmt.Errorf("load metadata blob index: %w", err)
	}
	stream, err := repository.OpenBlob(ctx, known, metadataID, nil, packstore.PackExt)
	if err != nil {
		return 0, fmt.Errorf("open metadata blob: %w", err)
	}
	defer func() { retErr = errors.Join(retErr, stream.Close()) }()
	if stream.Size() != manifest.Metadata.Bytes {
		return 0, fmt.Errorf("metadata blob size is %d, expected %d", stream.Size(), manifest.Metadata.Bytes)
	}
	decoder := jsontext.NewDecoder(stream)
	seen := make(map[string]struct{})
	var headerSeen bool
	for row := 0; ; row++ {
		var record struct {
			Type    string `json:"type"`
			Format  string `json:"format"`
			Version int    `json:"version"`
			Hash    string `json:"hash"`
			Size    int64  `json:"size"`
		}
		raw, err := decoder.ReadValue()
		if errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			return 0, fmt.Errorf("decode metadata record %d: %w", row, err)
		}
		if err := json.Unmarshal(raw, &record); err != nil {
			return 0, fmt.Errorf("decode metadata record %d: %w", row, err)
		}
		if row == 0 {
			if record.Type != "meta" || record.Format != "docbank-metadata" || record.Version != 1 {
				return 0, errors.New("metadata header is unsupported")
			}
			headerSeen = true
			continue
		}
		if record.Type != "blob" {
			continue
		}
		if record.Hash == "" || record.Size < 0 {
			return 0, errors.New("metadata has an invalid blob record")
		}
		if _, ok := seen[record.Hash]; ok {
			return 0, fmt.Errorf("metadata repeats blob %q", record.Hash)
		}
		seen[record.Hash] = struct{}{}
		if record.Size > math.MaxInt64-total {
			return 0, errors.New("metadata blob sizes exceed int64")
		}
		total += record.Size
	}
	if !headerSeen {
		return 0, errors.New("metadata header is missing")
	}
	if err := stream.Verify(); err != nil {
		return 0, fmt.Errorf("verify metadata blob: %w", err)
	}
	return total, nil
}
