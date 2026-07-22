package backupapp_test

import (
	"bytes"
	"context"
	"encoding/json"
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
	docsqlite "go.kenn.io/docbank/pkg/sqlite"
)

type archiveFixture struct {
	root     string
	metadata *store.Store
	blobs    *blob.Store
	content  map[string]string
}

type zeroReader struct{}

type freezeCoordinator struct {
	end func(context.Context) error
}

func (freezeCoordinator) Begin(context.Context) error { return nil }

func (f freezeCoordinator) End(ctx context.Context) error { return f.end(ctx) }

type rawMetadataSource struct{ metadata []byte }

func (rawMetadataSource) Format() string { return backupapp.MetadataFormat }

func (source rawMetadataSource) OpenSnapshot(context.Context) (backup.MetadataSnapshot, error) {
	return rawMetadataSnapshot(source), nil
}

type rawMetadataSnapshot struct{ metadata []byte }

func (snapshot rawMetadataSnapshot) OpenMetadata(context.Context) (io.ReadCloser, int64, error) {
	return io.NopCloser(bytes.NewReader(snapshot.metadata)), int64(len(snapshot.metadata)), nil
}

func (rawMetadataSnapshot) ContentInfo(context.Context) (*backup.ContentInfo, error) {
	return &backup.ContentInfo{}, nil
}

func (rawMetadataSnapshot) Stats(context.Context) (json.RawMessage, error) {
	return json.RawMessage(`{}`), nil
}

func (rawMetadataSnapshot) Close() error { return nil }

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

func exportMetadata(t *testing.T, metadata *store.Store) []byte {
	t.Helper()
	var dst bytes.Buffer
	require.NoError(t, metadata.ExportMetadata(t.Context(), &dst))
	return dst.Bytes()
}

func TestJSONLLooseSnapshotVerifyAndRestore(t *testing.T) {
	fixture := newArchiveFixture(t)
	wantMetadata := exportMetadata(t, fixture.metadata)
	app := backupapp.New("test-version")
	repo, err := backup.Init(filepath.Join(t.TempDir(), "repo"))
	require.NoError(t, err)

	manifest, err := backupapp.Create(
		t.Context(), repo, "test-version", fixture.metadata, fixture.blobs, backup.CreateOptions{
			Jobs: 2,
		})
	require.NoError(t, err)
	require.NotNil(t, manifest.Metadata)
	assert.Equal(t, backupapp.MetadataFormat, manifest.Metadata.Format)
	assert.Empty(t, manifest.DB.Engine)
	assert.Equal(t, int64(2), manifest.Attachments.Rows)
	assert.Equal(t, int64(2), manifest.Attachments.Blobs)
	assert.Equal(t, int64(len("alpha backup")+len("bravo backup")), manifest.Attachments.BlobBytes)
	stats, err := backupapp.ParseStats(manifest.Stats)
	require.NoError(t, err)
	assert.Equal(t, int64(3), stats.Nodes, "root plus two files")
	assert.Equal(t, int64(2), stats.Files)
	assert.Equal(t, int64(2), stats.ContentVersions)
	assert.Equal(t, manifest.Attachments.BlobBytes, stats.BlobBytes)

	verified, err := backup.Verify(t.Context(), repo, app, backup.VerifyOptions{Jobs: 2})
	require.NoError(t, err)
	assert.Empty(t, verified.Problems)
	assert.Equal(t, []string{manifest.SnapshotID}, verified.Snapshots)

	target := filepath.Join(t.TempDir(), "restored")
	_, err = backup.Restore(t.Context(), repo, app, backup.RestoreOptions{
		TargetDir: filepath.Join(t.TempDir(), "missing-metadata-restorer"), Jobs: 2,
	})
	require.ErrorContains(t, err, "requires a MetadataRestorer")

	restored, err := backupapp.Restore(t.Context(), repo, "test-version", backup.RestoreOptions{
		TargetDir: target, Jobs: 2,
	})
	require.NoError(t, err)
	assert.Equal(t, int64(2), restored.AttachmentBlobs)
	assert.Equal(t, int64(2), restored.PackedAttachmentBlobs)
	assert.Zero(t, restored.LooseAttachmentBlobs)

	restoredStore, err := store.Open(filepath.Join(target, "docbank.db"))
	require.NoError(t, err)
	assert.Equal(t, string(wantMetadata), string(exportMetadata(t, restoredStore)))
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
		version, versionErr := restoredStore.ContentVersionByID(t.Context(), node.CurrentVersionID)
		require.NoError(t, versionErr)
		assert.Equal(t, node.ID, version.NodeID)
		assert.Equal(t, node.BlobHash, version.BlobHash)
	}
	require.NoError(t, restoredBlobs.Close())
	require.NoError(t, restoredStore.Close())
}

func TestCompressedLooseSnapshotRestoresIndexedAuthority(t *testing.T) {
	root := t.TempDir()
	metadata, err := store.Open(filepath.Join(root, "docbank.db"))
	require.NoError(t, err)
	blobsDir := filepath.Join(root, "blobs")
	require.NoError(t, os.MkdirAll(filepath.Join(blobsDir, "tmp"), 0o700))
	blobs, err := blob.NewWithOptions(store.NewPackCatalog(metadata), blobsDir, blob.Options{
		LooseCompression: blob.LooseCompressionOptions{
			Enabled: true, MinBytes: 1024, MinSavingsPercent: 10,
		},
	})
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, blobs.Close())
		require.NoError(t, metadata.Close())
	})

	content := strings.Repeat("compressible snapshot content\n", 1024)
	var written blob.WriteReceipt
	require.NoError(t, blobs.WithMutation(t.Context(), func() error {
		var writeErr error
		written, writeErr = blobs.WriteDetailedContext(t.Context(), strings.NewReader(content))
		if writeErr != nil {
			return writeErr
		}
		_, writeErr = metadata.CreateFile(t.Context(), metadata.RootID(), "compressed.txt",
			written.Hash, written.Size, "text/plain", store.BlobPhysical{
				Encoding: "zstd", StoredBytes: written.StoredSize, PackEligible: written.PackEligible,
			})
		return writeErr
	}))
	require.Equal(t, packstore.LooseEncodingZstd, written.Encoding)

	repo, err := backup.Init(filepath.Join(t.TempDir(), "repo"))
	require.NoError(t, err)
	_, err = backupapp.Create(t.Context(), repo, "test-version", metadata, blobs, backup.CreateOptions{})
	require.NoError(t, err)
	target := filepath.Join(t.TempDir(), "restored")
	_, err = backupapp.Restore(t.Context(), repo, "test-version", backup.RestoreOptions{TargetDir: target})
	require.NoError(t, err)

	restoredStore, err := store.Open(filepath.Join(target, "docbank.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, restoredStore.Close()) })
	physical, err := restoredStore.PhysicalContent(t.Context(), written.Hash)
	require.NoError(t, err)
	assert.Equal(t, "packed", physical.Kind)
	assert.Equal(t, "zstd", physical.Encoding)
	assert.Equal(t, written.Size, physical.LogicalBytes)
	assert.Less(t, physical.StoredBytes, physical.LogicalBytes)
	assert.True(t, physical.PackEligible)
	backlog, err := restoredStore.LooseBacklog(t.Context())
	require.NoError(t, err)
	assert.Equal(t, store.LooseBacklog{}, backlog)

	restoredBlobs, err := blob.New(store.NewPackCatalog(restoredStore), filepath.Join(target, "blobs"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, restoredBlobs.Close()) })
	reader, size, err := restoredBlobs.OpenStreamContext(t.Context(), written.Hash)
	require.NoError(t, err)
	got, err := io.ReadAll(reader)
	require.NoError(t, err)
	assert.Equal(t, written.Size, size)
	assert.Equal(t, content, string(got))
	assert.True(t, reader.Verified())
	require.NoError(t, reader.Close())
}

func TestAuditedIncrementalSnapshotRestoresCompleteProtectedHistory(t *testing.T) {
	fixture := newArchiveFixture(t)
	writeFile := func(parentID int64, name, content string) store.Node {
		t.Helper()
		var node store.Node
		require.NoError(t, fixture.blobs.WithMutation(t.Context(), func() error {
			hash, size, err := fixture.blobs.WriteContext(
				t.Context(), strings.NewReader(content),
			)
			if err != nil {
				return err
			}
			node, err = fixture.metadata.CreateFile(
				t.Context(), parentID, name, hash, size, "text/plain",
			)
			return err
		}))
		return node
	}

	records, err := fixture.metadata.Mkdir(t.Context(), fixture.metadata.RootID(), "Records")
	require.NoError(t, err)
	contracts, err := fixture.metadata.Mkdir(t.Context(), fixture.metadata.RootID(), "Contracts")
	require.NoError(t, err)
	document := writeFile(records.ID, "return.txt", "original tax return")
	initialVersionID := document.CurrentVersionID
	plan, err := fixture.metadata.PreviewInitialAudit(
		t.Context(), records.ID, "cli", nil,
	)
	require.NoError(t, err)
	initialStatus, err := fixture.metadata.EnableInitialAudit(t.Context(), plan)
	require.NoError(t, err)
	require.Len(t, initialStatus.Scopes, 1)

	packed, err := fixture.blobs.Maintainer().Pack(t.Context(), packstore.PackOptions{})
	require.NoError(t, err)
	require.Equal(t, 3, packed.BlobsPacked)
	require.Equal(t, 1, packed.PacksSealed)

	repo, err := backup.Init(filepath.Join(t.TempDir(), "repo"))
	require.NoError(t, err)
	baseline, err := backupapp.Create(
		t.Context(), repo, "test-version", fixture.metadata, fixture.blobs,
		backup.CreateOptions{Jobs: 2},
	)
	require.NoError(t, err)
	assert.Empty(t, baseline.ParentID)
	secondPlan, err := fixture.metadata.PreviewInitialAudit(
		t.Context(), contracts.ID, "cli", nil,
	)
	require.NoError(t, err)
	secondStatus, err := fixture.metadata.EnableInitialAudit(t.Context(), secondPlan)
	require.NoError(t, err)
	require.Len(t, secondStatus.Scopes, 2)
	contract := writeFile(contracts.ID, "agreement.txt", "signed agreement")
	contractVersionID := contract.CurrentVersionID

	const replacementContent = "corrected tax return"
	var replacement store.ContentVersion
	require.NoError(t, fixture.blobs.WithMutation(t.Context(), func() error {
		hash, size, writeErr := fixture.blobs.WriteContext(
			t.Context(), strings.NewReader(replacementContent),
		)
		if writeErr != nil {
			return writeErr
		}
		document, replacement, writeErr = fixture.metadata.ReplaceContent(
			t.Context(), document.ID, document.Revision, hash, size, "text/plain",
		)
		return writeErr
	}))
	archive, err := fixture.metadata.Mkdir(t.Context(), records.ID, "Archive")
	require.NoError(t, err)
	receipt := writeFile(records.ID, "receipt.txt", "supporting receipt")
	receiptVersionID := receipt.CurrentVersionID
	receipt, _, err = fixture.metadata.Move(
		t.Context(), receipt.ID, archive.ID, receipt.Name, receipt.Revision,
	)
	require.NoError(t, err)
	trashedReceipt, _, err := fixture.metadata.Trash(
		t.Context(), receipt.ID, receipt.Revision,
	)
	require.NoError(t, err)
	tag, err := fixture.metadata.CreateTag(t.Context(), "filed")
	require.NoError(t, err)
	assignment, err := fixture.metadata.AssignTag(
		t.Context(), tag.ID, document.ID, document.Revision,
	)
	require.NoError(t, err)
	document = assignment.Node

	sourceStorage, err := fixture.blobs.Stats(t.Context())
	require.NoError(t, err)
	assert.Equal(t, 3, int(sourceStorage.PackedBlobs))
	assert.Equal(t, 3, sourceStorage.LooseBlobs)
	wantMetadata := exportMetadata(t, fixture.metadata)
	sourceAudit, err := fixture.metadata.VerifyAudit(t.Context(), nil)
	require.NoError(t, err)
	require.True(t, sourceAudit.Evidence.Enabled)
	require.Len(t, sourceAudit.Evidence.Scopes, 2)
	assert.ElementsMatch(t,
		[]string{initialStatus.Scopes[0].ID, secondPlan.Preview().ScopeID},
		[]string{sourceAudit.Evidence.Scopes[0].ID, sourceAudit.Evidence.Scopes[1].ID},
	)
	require.Len(t, sourceAudit.ProtectedBlobs, 4)

	manifest, err := backupapp.Create(
		t.Context(), repo, "test-version", fixture.metadata, fixture.blobs,
		backup.CreateOptions{Jobs: 2},
	)
	require.NoError(t, err)
	assert.Equal(t, baseline.SnapshotID, manifest.ParentID)
	assert.Equal(t, int64(6), manifest.Attachments.Blobs)
	verified, err := backup.Verify(
		t.Context(), repo, backupapp.New("test-version"),
		backup.VerifyOptions{All: true, Jobs: 2},
	)
	require.NoError(t, err)
	assert.Empty(t, verified.Problems)
	assert.ElementsMatch(t, []string{baseline.SnapshotID, manifest.SnapshotID}, verified.Snapshots)

	target := filepath.Join(t.TempDir(), "restored")
	result, err := backupapp.Restore(
		t.Context(), repo, "test-version",
		backup.RestoreOptions{TargetDir: target, Jobs: 2},
	)
	require.NoError(t, err)
	assert.Equal(t, manifest.Attachments.Blobs, result.AttachmentBlobs)
	restoredStore, err := store.Open(filepath.Join(target, "docbank.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, restoredStore.Close()) })
	assert.Equal(t, wantMetadata, exportMetadata(t, restoredStore))

	restoredAudit, err := restoredStore.VerifyAudit(t.Context(), &sourceAudit.Evidence)
	require.NoError(t, err)
	assert.Equal(t, sourceAudit.Evidence, restoredAudit.Evidence)
	require.NotNil(t, restoredAudit.EvidenceCheck)
	assert.True(t, restoredAudit.EvidenceCheck.Extends)
	assert.Empty(t, restoredAudit.EvidenceCheck.Problems)
	assert.Equal(t, sourceAudit.ProtectedBlobs, restoredAudit.ProtectedBlobs)
	assert.Equal(t, sourceAudit.ProtectedBytes, restoredAudit.ProtectedBytes)

	restoredBlobs, err := blob.New(
		store.NewPackCatalog(restoredStore), filepath.Join(target, "blobs"),
	)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, restoredBlobs.Close()) })
	for versionID, want := range map[string]string{
		initialVersionID:  "original tax return",
		replacement.ID:    replacementContent,
		receiptVersionID:  "supporting receipt",
		contractVersionID: "signed agreement",
	} {
		version, versionErr := restoredStore.ContentVersionByID(t.Context(), versionID)
		require.NoError(t, versionErr)
		stream, _, openErr := restoredBlobs.OpenStreamContext(t.Context(), version.BlobHash)
		require.NoError(t, openErr)
		got, readErr := io.ReadAll(stream)
		require.NoError(t, readErr)
		assert.True(t, stream.Verified())
		require.NoError(t, stream.Close())
		assert.Equal(t, want, string(got))
	}

	restoredReceipt, err := restoredStore.NodeByID(t.Context(), trashedReceipt.ID)
	require.NoError(t, err)
	assert.NotNil(t, restoredReceipt.TrashedAt)
	restoredStatus, err := restoredStore.AuditStatus(t.Context(), &restoredReceipt.ID)
	require.NoError(t, err)
	require.NotNil(t, restoredStatus.Membership)
	assert.True(t, restoredStatus.Membership.Protected)
	assert.Equal(t, []string{initialStatus.Scopes[0].ID}, restoredStatus.Membership.ScopeIDs)
	restoredContractStatus, err := restoredStore.AuditStatus(t.Context(), &contract.ID)
	require.NoError(t, err)
	require.NotNil(t, restoredContractStatus.Membership)
	assert.True(t, restoredContractStatus.Membership.Protected)
	assert.Equal(t, []string{secondPlan.Preview().ScopeID},
		restoredContractStatus.Membership.ScopeIDs)
	history, err := restoredStore.AuditHistory(t.Context(), restoredReceipt.ID, 100, "")
	require.NoError(t, err)
	assert.GreaterOrEqual(t, history.Total, 4)

	_, err = restoredStore.PruneContentVersions(
		t.Context(), document.ID, document.Revision,
		store.VersionPruneSelector{AllPrior: true}, true,
	)
	require.ErrorIs(t, err, store.ErrAuditMutationUnsupported)
	_, err = restoredStore.TrashEmpty(t.Context(), 0, true)
	require.ErrorIs(t, err, store.ErrAuditMutationUnsupported)
}

func TestTruncatedAuditJSONLRestoreLeavesNoPublishedDatabase(t *testing.T) {
	fixture := newArchiveFixture(t)
	records, err := fixture.metadata.Mkdir(t.Context(), fixture.metadata.RootID(), "Records")
	require.NoError(t, err)
	plan, err := fixture.metadata.PreviewInitialAudit(
		t.Context(), records.ID, "cli", nil,
	)
	require.NoError(t, err)
	_, err = fixture.metadata.EnableInitialAudit(t.Context(), plan)
	require.NoError(t, err)

	metadata := bytes.TrimSuffix(exportMetadata(t, fixture.metadata), []byte("\n"))
	lastBreak := bytes.LastIndexByte(metadata, '\n')
	require.Positive(t, lastBreak)
	require.Contains(t, string(metadata[lastBreak+1:]), `"type":"audit_record"`)
	truncated := bytes.Clone(metadata[:lastBreak+1])

	repo, err := backup.Init(filepath.Join(t.TempDir(), "repo"))
	require.NoError(t, err)
	_, err = backup.Create(t.Context(), repo, backupapp.New("test-version"), backup.CreateOptions{
		MetadataSource: rawMetadataSource{metadata: truncated},
	})
	require.NoError(t, err)
	target := filepath.Join(t.TempDir(), "restored")
	_, err = backupapp.Restore(
		t.Context(), repo, "test-version", backup.RestoreOptions{TargetDir: target},
	)
	require.ErrorContains(t, err, "audit")
	_, statErr := os.Stat(filepath.Join(target, "docbank.db"))
	require.ErrorIs(t, statErr, os.ErrNotExist)
}

func TestRestoreSupportsLegacySQLitePageSnapshots(t *testing.T) {
	fixture := newArchiveFixture(t)
	repo, err := backup.Init(filepath.Join(t.TempDir(), "repo"))
	require.NoError(t, err)
	app := backupapp.New("legacy-version")
	manifest, err := backup.Create(t.Context(), repo, app, backup.CreateOptions{
		DBPath:        filepath.Join(fixture.root, "docbank.db"),
		ContentSource: backupapp.NewContentSource(fixture.blobs),
		SQLiteOpener:  backupapp.SQLiteOpener(fixture.metadata.SQLiteDriver()),
		Jobs:          2,
	})
	require.NoError(t, err)
	assert.Nil(t, manifest.Metadata)
	assert.NotEmpty(t, manifest.DB.Engine)

	target := filepath.Join(t.TempDir(), "restored")
	result, err := backupapp.Restore(t.Context(), repo, "current-version", backup.RestoreOptions{
		TargetDir: target,
		Jobs:      2,
	})
	require.NoError(t, err)
	assert.Equal(t, int64(2), result.AttachmentBlobs)

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
		_, versionErr := restoredStore.ContentVersionByID(t.Context(), node.CurrentVersionID)
		require.NoError(t, versionErr)
	}
	require.NoError(t, restoredBlobs.Close())
	require.NoError(t, restoredStore.Close())
}

func TestJSONLSnapshotRemainsStableAfterFreezeEnds(t *testing.T) {
	fixture := newArchiveFixture(t)
	repo, err := backup.Init(filepath.Join(t.TempDir(), "repo"))
	require.NoError(t, err)
	freezer := freezeCoordinator{end: func(ctx context.Context) error {
		_, mkdirErr := fixture.metadata.Mkdir(ctx, fixture.metadata.RootID(), "created-after-snapshot")
		return mkdirErr
	}}

	manifest, err := backupapp.Create(
		t.Context(), repo, "test-version", fixture.metadata, fixture.blobs,
		backup.CreateOptions{Freezer: freezer, Jobs: 2})
	require.NoError(t, err)
	stats, err := backupapp.ParseStats(manifest.Stats)
	require.NoError(t, err)
	assert.Equal(t, int64(3), stats.Nodes)
	_, err = fixture.metadata.NodeByPath(t.Context(), "/created-after-snapshot")
	require.NoError(t, err)

	target := filepath.Join(t.TempDir(), "restored")
	_, err = backupapp.Restore(
		t.Context(), repo, "test-version", backup.RestoreOptions{TargetDir: target, Jobs: 2})
	require.NoError(t, err)
	restoredStore, err := store.Open(filepath.Join(target, "docbank.db"))
	require.NoError(t, err)
	_, err = restoredStore.NodeByPath(t.Context(), "/created-after-snapshot")
	require.Error(t, err)
	require.NoError(t, restoredStore.Close())
}

func TestJSONLSnapshotRejectsMalformedLiveMetadata(t *testing.T) {
	tests := []struct {
		name      string
		statement string
		want      string
	}{
		{
			name:      "invalid operation ID",
			statement: `UPDATE content_versions SET introduced_operation_id='not-a-uuid'`,
			want:      "invalid content version operation ID",
		},
		{
			name: "dangling blob reference",
			statement: `UPDATE content_versions SET blob_hash='` + strings.Repeat("d", 64) + `'
				WHERE rowid=(SELECT rowid FROM content_versions LIMIT 1)`,
			want: "metadata violates foreign key",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fixture := newArchiveFixture(t)
			rawDB, err := fixture.metadata.SQLiteDriver().Open(
				filepath.Join(fixture.root, "docbank.db"), docsqlite.OpenOptions{
					Access: docsqlite.ReadWriteExisting, TransactionMode: docsqlite.Immediate,
				})
			require.NoError(t, err)
			rawDB.SetMaxOpenConns(1)
			_, err = rawDB.Exec(`PRAGMA foreign_keys=OFF`)
			require.NoError(t, err)
			_, err = rawDB.Exec(tt.statement)
			require.NoError(t, err)
			require.NoError(t, rawDB.Close())

			repo, err := backup.Init(filepath.Join(t.TempDir(), "repo"))
			require.NoError(t, err)
			manifest, err := backupapp.Create(
				t.Context(), repo, "test-version", fixture.metadata, fixture.blobs, backup.CreateOptions{})
			require.ErrorContains(t, err, tt.want)
			assert.Nil(t, manifest)
			snapshots, err := repo.ListSnapshots()
			require.NoError(t, err)
			assert.Empty(t, snapshots)
		})
	}
}

func TestMalformedJSONLRestoreLeavesNoPublishedDatabase(t *testing.T) {
	repo, err := backup.Init(filepath.Join(t.TempDir(), "repo"))
	require.NoError(t, err)
	_, err = backup.Create(t.Context(), repo, backupapp.New("test-version"), backup.CreateOptions{
		MetadataSource: rawMetadataSource{metadata: []byte("{malformed\n")},
	})
	require.NoError(t, err)

	target := filepath.Join(t.TempDir(), "restored")
	_, err = backupapp.Restore(
		t.Context(), repo, "test-version", backup.RestoreOptions{TargetDir: target})
	require.ErrorContains(t, err, "importing metadata JSONL")
	_, statErr := os.Stat(filepath.Join(target, "docbank.db"))
	require.ErrorIs(t, statErr, os.ErrNotExist)
}

func TestLooseAbovePackingLimitSnapshotVerifyAndRestore(t *testing.T) {
	fixture := newArchiveFixture(t)
	size := blob.MaxPackedBlobBytes + 1
	var hash string
	require.NoError(t, fixture.blobs.WithMutation(t.Context(), func() error {
		var err error
		hash, _, err = fixture.blobs.WriteContext(t.Context(), io.LimitReader(zeroReader{}, size))
		if err != nil {
			return err
		}
		_, err = fixture.metadata.CreateFile(t.Context(), fixture.metadata.RootID(), "large-loose.bin",
			hash, size, "application/octet-stream")
		return err
	}))

	app := backupapp.New("test-version")
	repo, err := backup.Init(filepath.Join(t.TempDir(), "repo"))
	require.NoError(t, err)
	manifest, err := backupapp.Create(
		t.Context(), repo, "test-version", fixture.metadata, fixture.blobs, backup.CreateOptions{})
	require.NoError(t, err)
	assert.Equal(t, int64(3), manifest.Attachments.Blobs)
	verified, err := backup.Verify(t.Context(), repo, app, backup.VerifyOptions{})
	require.NoError(t, err)
	assert.Empty(t, verified.Problems)

	target := filepath.Join(t.TempDir(), "restored")
	_, err = backupapp.Restore(
		t.Context(), repo, "test-version", backup.RestoreOptions{TargetDir: target})
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
	physical, err := restoredStore.PhysicalContent(t.Context(), hash)
	require.NoError(t, err)
	assert.Equal(t, store.PhysicalContent{
		Kind: "loose", Encoding: "raw", LogicalBytes: size,
		StoredBytes: size, PackEligible: false,
	}, physical)
	backlog, err := restoredStore.LooseBacklog(t.Context())
	require.NoError(t, err)
	assert.Equal(t, store.LooseBacklog{}, backlog)
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
	manifest, err := backupapp.Create(
		context.Background(), repo, "test-version", fixture.metadata, fixture.blobs, backup.CreateOptions{
			Jobs: 2,
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
	require.ErrorContains(t, err, "requires a MetadataRestorer")

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
		version, versionErr := restoredStore.ContentVersionByID(t.Context(), node.CurrentVersionID)
		require.NoError(t, versionErr)
		assert.Equal(t, node.BlobHash, version.BlobHash)
	}
	require.NoError(t, restoredBlobs.Close())
	require.NoError(t, restoredStore.Close())
}

func TestVersionedEditingRoundTripsPackedRevertSource(t *testing.T) {
	fixture := newArchiveFixture(t)
	alpha, err := fixture.metadata.NodeByPath(t.Context(), "/alpha.txt")
	require.NoError(t, err)
	priorVersionID := alpha.CurrentVersionID
	packed, err := fixture.blobs.Maintainer().Pack(t.Context(), packstore.PackOptions{})
	require.NoError(t, err)
	require.Equal(t, 2, packed.BlobsPacked)

	const replacement = "alpha replacement"
	var replaced store.Node
	require.NoError(t, fixture.blobs.WithMutation(t.Context(), func() error {
		hash, size, writeErr := fixture.blobs.WriteContext(t.Context(), strings.NewReader(replacement))
		if writeErr != nil {
			return writeErr
		}
		replaced, _, writeErr = fixture.metadata.ReplaceContent(
			t.Context(), alpha.ID, alpha.Revision, hash, size, "text/plain",
		)
		return writeErr
	}))
	reverted, revertVersion, _, err := fixture.metadata.RevertContent(
		t.Context(), alpha.ID, replaced.Revision, priorVersionID,
	)
	require.NoError(t, err)
	wantMetadata := exportMetadata(t, fixture.metadata)

	repo, err := backup.Init(filepath.Join(t.TempDir(), "repo"))
	require.NoError(t, err)
	manifest, err := backupapp.Create(
		t.Context(), repo, "test-version", fixture.metadata, fixture.blobs,
		backup.CreateOptions{Jobs: 2})
	require.NoError(t, err)
	assert.Equal(t, int64(3), manifest.Attachments.Blobs,
		"backup must include both heads plus the other file")
	stats, err := backupapp.ParseStats(manifest.Stats)
	require.NoError(t, err)
	assert.Equal(t, int64(4), stats.ContentVersions)

	target := filepath.Join(t.TempDir(), "restored")
	_, err = backupapp.Restore(t.Context(), repo, "test-version", backup.RestoreOptions{
		TargetDir: target, Jobs: 2,
	})
	require.NoError(t, err)
	restoredStore, err := store.Open(filepath.Join(target, "docbank.db"))
	require.NoError(t, err)
	assert.Equal(t, string(wantMetadata), string(exportMetadata(t, restoredStore)),
		"JSONL restore must preserve the complete replacement history byte-for-byte")
	restoredBlobs, err := blob.New(store.NewPackCatalog(restoredStore), filepath.Join(target, "blobs"))
	require.NoError(t, err)

	restoredNode, err := restoredStore.NodeByID(t.Context(), alpha.ID)
	require.NoError(t, err)
	assert.Equal(t, reverted.CurrentVersionID, restoredNode.CurrentVersionID)
	assert.Equal(t, int64(3), restoredNode.Revision)
	for versionID, want := range map[string]string{
		priorVersionID:                "alpha backup",
		replaced.CurrentVersionID:     replacement,
		restoredNode.CurrentVersionID: "alpha backup",
	} {
		version, versionErr := restoredStore.ContentVersionByID(t.Context(), versionID)
		require.NoError(t, versionErr)
		stream, _, openErr := restoredBlobs.OpenStreamContext(t.Context(), version.BlobHash)
		require.NoError(t, openErr)
		got, readErr := io.ReadAll(stream)
		require.NoError(t, readErr)
		assert.True(t, stream.Verified())
		require.NoError(t, stream.Close())
		assert.Equal(t, want, string(got))
	}
	restoredRevert, err := restoredStore.ContentVersionByID(t.Context(), revertVersion.ID)
	require.NoError(t, err)
	require.NotNil(t, restoredRevert.SourceVersionID)
	assert.Equal(t, priorVersionID, *restoredRevert.SourceVersionID)
	require.NoError(t, restoredBlobs.Close())
	require.NoError(t, restoredStore.Close())
}

func TestPrunedVersionHistoryRoundTripsWithoutResurrection(t *testing.T) {
	fixture := newArchiveFixture(t)
	alpha, err := fixture.metadata.NodeByPath(t.Context(), "/alpha.txt")
	require.NoError(t, err)
	prunedVersionID := alpha.CurrentVersionID
	const replacement = "retained replacement"
	var replaced store.Node
	require.NoError(t, fixture.blobs.WithMutation(t.Context(), func() error {
		hash, size, writeErr := fixture.blobs.WriteContext(t.Context(), strings.NewReader(replacement))
		if writeErr != nil {
			return writeErr
		}
		replaced, _, writeErr = fixture.metadata.ReplaceContent(
			t.Context(), alpha.ID, alpha.Revision, hash, size, "text/plain",
		)
		return writeErr
	}))
	pruned, err := fixture.metadata.PruneContentVersions(t.Context(), alpha.ID, replaced.Revision,
		store.VersionPruneSelector{AllPrior: true}, true)
	require.NoError(t, err)
	require.Equal(t, 1, pruned.DeletedVersions)
	wantMetadata := exportMetadata(t, fixture.metadata)

	repo, err := backup.Init(filepath.Join(t.TempDir(), "repo"))
	require.NoError(t, err)
	manifest, err := backupapp.Create(
		t.Context(), repo, "test-version", fixture.metadata, fixture.blobs,
		backup.CreateOptions{Jobs: 2})
	require.NoError(t, err)
	stats, err := backupapp.ParseStats(manifest.Stats)
	require.NoError(t, err)
	assert.Equal(t, int64(2), stats.ContentVersions,
		"backup metadata must contain only the retained heads")

	target := filepath.Join(t.TempDir(), "restored")
	_, err = backupapp.Restore(t.Context(), repo, "test-version", backup.RestoreOptions{
		TargetDir: target, Jobs: 2,
	})
	require.NoError(t, err)
	restoredStore, err := store.Open(filepath.Join(target, "docbank.db"))
	require.NoError(t, err)
	defer func() { require.NoError(t, restoredStore.Close()) }()
	assert.Equal(t, string(wantMetadata), string(exportMetadata(t, restoredStore)))
	_, err = restoredStore.ContentVersionByID(t.Context(), prunedVersionID)
	require.ErrorIs(t, err, store.ErrNotFound,
		"backup and restore must not resurrect released history")
	restoredAlpha, err := restoredStore.NodeByPath(t.Context(), "/alpha.txt")
	require.NoError(t, err)
	versions, total, err := restoredStore.ContentVersions(t.Context(), restoredAlpha.ID, 10, 0)
	require.NoError(t, err)
	require.Equal(t, 1, total)
	require.Len(t, versions, 1)
	assert.Equal(t, replaced.CurrentVersionID, versions[0].ID)
}
