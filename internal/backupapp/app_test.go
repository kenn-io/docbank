package backupapp_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/backup"
	"go.kenn.io/kit/packstore"

	"go.kenn.io/docbank/internal/backupapp"
	"go.kenn.io/docbank/internal/blob"
	"go.kenn.io/docbank/internal/store"
)

type archiveFixture struct {
	root     string
	metadata *store.Store
	blobs    *blob.Store
	content  map[string]string
}

type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
}

func newArchiveFixture(t *testing.T) *archiveFixture {
	t.Helper()
	root := t.TempDir()
	metadata, err := store.Open(filepath.Join(root, "docbank.db"))
	require.NoError(t, err)
	blobsDir := filepath.Join(root, "blobs")
	require.NoError(t, os.MkdirAll(filepath.Join(blobsDir, "tmp"), 0o700))
	blobs, err := blob.New(store.NewPackCatalog(metadata), blobsDir)
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, blobs.Close())
		require.NoError(t, metadata.Close())
	})
	fixture := &archiveFixture{
		root: root, metadata: metadata, blobs: blobs,
		content: map[string]string{"alpha.txt": "alpha backup", "bravo.txt": "bravo backup"},
	}
	require.NoError(t, blobs.WithMutation(t.Context(), func() error {
		for name, content := range fixture.content {
			hash, size, writeErr := blobs.WriteContext(t.Context(), strings.NewReader(content))
			if writeErr != nil {
				return writeErr
			}
			if _, createErr := metadata.CreateFile(t.Context(), metadata.RootID(), name,
				hash, size, "text/plain"); createErr != nil {
				return createErr
			}
		}
		return nil
	}))
	return fixture
}

func TestLooseSnapshotVerifyAndRestore(t *testing.T) {
	fixture := newArchiveFixture(t)
	app := backupapp.New("test-version")
	repo, err := backup.Init(filepath.Join(t.TempDir(), "repo"))
	require.NoError(t, err)

	manifest, err := backup.Create(t.Context(), repo, app, backup.CreateOptions{
		DBPath:        filepath.Join(fixture.root, "docbank.db"),
		ContentSource: backupapp.NewContentSource(fixture.blobs),
		Jobs:          2,
	})
	require.NoError(t, err)
	assert.Equal(t, int64(2), manifest.Attachments.Rows)
	assert.Equal(t, int64(2), manifest.Attachments.Blobs)
	assert.Equal(t, int64(len("alpha backup")+len("bravo backup")), manifest.Attachments.BlobBytes)
	stats, err := backupapp.ParseStats(manifest.Stats)
	require.NoError(t, err)
	assert.Equal(t, int64(3), stats.Nodes, "root plus two files")
	assert.Equal(t, int64(2), stats.Files)
	assert.Equal(t, manifest.Attachments.BlobBytes, stats.BlobBytes)

	verified, err := backup.Verify(t.Context(), repo, app, backup.VerifyOptions{Jobs: 2})
	require.NoError(t, err)
	assert.Empty(t, verified.Problems)
	assert.Equal(t, []string{manifest.SnapshotID}, verified.Snapshots)

	target := filepath.Join(t.TempDir(), "restored")
	restored, err := backup.Restore(t.Context(), repo, app, backup.RestoreOptions{
		TargetDir: target, Jobs: 2,
	})
	require.NoError(t, err)
	assert.Equal(t, int64(2), restored.AttachmentBlobs)
	assert.Equal(t, int64(2), restored.LooseAttachmentBlobs)

	restoredStore, err := store.Open(filepath.Join(target, "docbank.db"))
	require.NoError(t, err)
	restoredBlobs, err := blob.New(store.NewPackCatalog(restoredStore), filepath.Join(target, "blobs"))
	require.NoError(t, err)
	for name, want := range fixture.content {
		node, nodeErr := restoredStore.NodeByPath(t.Context(), "/"+name)
		require.NoError(t, nodeErr)
		reader, openErr := restoredBlobs.OpenContext(t.Context(), node.BlobHash)
		require.NoError(t, openErr)
		got, readErr := io.ReadAll(reader)
		require.NoError(t, readErr)
		require.NoError(t, reader.Close())
		assert.Equal(t, want, string(got))
	}
	require.NoError(t, restoredBlobs.Close())
	require.NoError(t, restoredStore.Close())
}

func TestOversizedLegacyLooseSnapshotVerifyAndRestore(t *testing.T) {
	fixture := newArchiveFixture(t)
	size := blob.MaxBlobBytes + 1
	digest := sha256.New()
	_, err := io.CopyN(digest, zeroReader{}, size)
	require.NoError(t, err)
	hash := hex.EncodeToString(digest.Sum(nil))
	path := filepath.Join(fixture.root, "blobs", hash[:2], hash)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
	f, err := os.Create(path)
	require.NoError(t, err)
	_, err = io.CopyN(f, zeroReader{}, size)
	require.NoError(t, err)
	require.NoError(t, f.Close())
	_, err = fixture.metadata.CreateFile(t.Context(), fixture.metadata.RootID(), "legacy-large.bin",
		hash, size, "application/octet-stream")
	require.NoError(t, err)

	app := backupapp.New("test-version")
	repo, err := backup.Init(filepath.Join(t.TempDir(), "repo"))
	require.NoError(t, err)
	manifest, err := backup.Create(t.Context(), repo, app, backup.CreateOptions{
		DBPath: filepath.Join(fixture.root, "docbank.db"), ContentSource: backupapp.NewContentSource(fixture.blobs),
	})
	require.NoError(t, err)
	assert.Equal(t, int64(3), manifest.Attachments.Blobs)
	verified, err := backup.Verify(t.Context(), repo, app, backup.VerifyOptions{})
	require.NoError(t, err)
	assert.Empty(t, verified.Problems)

	target := filepath.Join(t.TempDir(), "restored")
	_, err = backup.Restore(t.Context(), repo, app, backup.RestoreOptions{TargetDir: target})
	require.NoError(t, err)
	restoredStore, err := store.Open(filepath.Join(target, "docbank.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, restoredStore.Close()) })
	restoredBlobs, err := blob.New(store.NewPackCatalog(restoredStore), filepath.Join(target, "blobs"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, restoredBlobs.Close()) })
	stream, restoredSize, err := restoredBlobs.OpenStreamContext(t.Context(), hash)
	require.NoError(t, err)
	assert.Equal(t, size, restoredSize)
	written, err := io.Copy(io.Discard, stream)
	require.NoError(t, err)
	assert.Equal(t, size, written)
	assert.True(t, stream.Verified())
	require.NoError(t, stream.Close())
}

func TestPackedSnapshotRequiresAndUsesPackedRestoreTarget(t *testing.T) {
	fixture := newArchiveFixture(t)
	packed, err := fixture.blobs.Maintainer().Pack(t.Context(), packstore.PackOptions{})
	require.NoError(t, err)
	require.Equal(t, 2, packed.BlobsPacked)
	require.Equal(t, 1, packed.PacksSealed)

	repo, err := backup.Init(filepath.Join(t.TempDir(), "repo"))
	require.NoError(t, err)
	app := backupapp.New("test-version")
	manifest, err := backup.Create(context.Background(), repo, app, backup.CreateOptions{
		DBPath:        filepath.Join(fixture.root, "docbank.db"),
		ContentSource: backupapp.NewContentSource(fixture.blobs),
		Jobs:          2,
	})
	require.NoError(t, err)
	assert.Equal(t, int64(2), manifest.Attachments.Blobs)

	verified, err := backup.Verify(t.Context(), repo, app, backup.VerifyOptions{Jobs: 2})
	require.NoError(t, err)
	assert.Empty(t, verified.Problems)

	unsafeTarget := filepath.Join(t.TempDir(), "unsafe-restored")
	_, err = backup.Restore(t.Context(), repo, app, backup.RestoreOptions{
		TargetDir: unsafeTarget, Jobs: 2,
	})
	require.ErrorContains(t, err, "snapshot contains packed blob authority")

	target := filepath.Join(t.TempDir(), "restored")
	restored, err := backupapp.Restore(t.Context(), repo, "test-version", backup.RestoreOptions{
		TargetDir: target, Jobs: 2,
	})
	require.NoError(t, err)
	assert.Zero(t, restored.LooseAttachmentBlobs)
	assert.Equal(t, int64(2), restored.PackedAttachmentBlobs)
	assert.Positive(t, restored.AttachmentPacks)

	restoredStore, err := store.Open(filepath.Join(target, "docbank.db"))
	require.NoError(t, err)
	restoredCatalog := store.NewPackCatalog(restoredStore)
	records, err := restoredCatalog.ListPackRecords(t.Context())
	require.NoError(t, err)
	assert.NotEmpty(t, records)
	entries, err := restoredCatalog.ListIndexed(t.Context())
	require.NoError(t, err)
	assert.Len(t, entries, 2)
	restoredBlobs, err := blob.New(restoredCatalog, filepath.Join(target, "blobs"))
	require.NoError(t, err)
	for name, want := range fixture.content {
		node, nodeErr := restoredStore.NodeByPath(t.Context(), "/"+name)
		require.NoError(t, nodeErr)
		reader, openErr := restoredBlobs.OpenContext(t.Context(), node.BlobHash)
		require.NoError(t, openErr)
		got, readErr := io.ReadAll(reader)
		require.NoError(t, readErr)
		require.NoError(t, reader.Close())
		assert.Equal(t, want, string(got))
	}
	require.NoError(t, restoredBlobs.Close())
	require.NoError(t, restoredStore.Close())
}
