package store

import (
	"bytes"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPruneContentVersionsPreviewRunAndMetadataRoundTrip(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	created, err := s.CreateFile(ctx, s.RootID(), "history.txt", fakeHash("a1"), 10, "text/plain")
	require.NoError(t, err)
	replaced, second, err := s.ReplaceContent(
		ctx, created.ID, created.Revision, fakeHash("b2"), 20, "text/plain",
	)
	require.NoError(t, err)
	replaced, current, err := s.ReplaceContent(
		ctx, created.ID, replaced.Revision, fakeHash("c3"), 30, "text/plain",
	)
	require.NoError(t, err)

	preview, err := s.PruneContentVersions(ctx, created.ID, replaced.Revision,
		VersionPruneSelector{KeepNewest: 2}, false)
	require.NoError(t, err)
	assert.False(t, preview.Run)
	assert.False(t, preview.Changed)
	assert.Equal(t, int64(10), preview.LogicalBytes)
	assert.Equal(t, 1, preview.UniqueBlobs)
	assert.Equal(t, 1, preview.ReleasableBlobs)
	assert.Equal(t, int64(10), preview.ReleasableBytes)
	assert.Equal(t, 1, preview.LooseBlobsPendingGC)
	require.Len(t, preview.Candidates, 1)
	assert.Equal(t, created.CurrentVersionID, preview.Candidates[0].ID)

	_, err = s.PruneContentVersions(ctx, created.ID, created.Revision,
		VersionPruneSelector{KeepNewest: 2}, true)
	require.ErrorIs(t, err, ErrStaleRevision)

	receipt, err := s.PruneContentVersions(ctx, created.ID, replaced.Revision,
		VersionPruneSelector{KeepNewest: 2}, true)
	require.NoError(t, err)
	assert.True(t, receipt.Run)
	assert.True(t, receipt.Changed)
	assert.Equal(t, 1, receipt.DeletedVersions)
	assert.Equal(t, replaced.Revision+1, receipt.Node.Revision)
	assert.Equal(t, current.ID, receipt.Node.CurrentVersionID)
	_, err = s.ContentVersionByID(ctx, created.CurrentVersionID)
	require.ErrorIs(t, err, ErrNotFound)
	versions, total, err := s.ContentVersions(ctx, created.ID, 10, 0)
	require.NoError(t, err)
	assert.Equal(t, 2, total)
	require.Len(t, versions, 2)
	assert.Equal(t, current.ID, versions[0].ID)
	assert.Equal(t, second.ID, versions[1].ID)

	unreachable, err := s.UnreachableBlobs(ctx)
	require.NoError(t, err)
	require.Len(t, unreachable, 1)
	assert.Equal(t, fakeHash("a1"), unreachable[0].Hash)

	var exported bytes.Buffer
	require.NoError(t, s.ExportMetadata(ctx, &exported))
	restored := newTestStore(t)
	require.NoError(t, restored.ImportMetadata(ctx, bytes.NewReader(exported.Bytes())))
	restoredNode, err := restored.NodeByPath(ctx, "/history.txt")
	require.NoError(t, err)
	assert.Equal(t, receipt.Node.Revision, restoredNode.Revision)
	restoredVersions, restoredTotal, err := restored.ContentVersions(ctx, restoredNode.ID, 10, 0)
	require.NoError(t, err)
	assert.Equal(t, 2, restoredTotal)
	assert.Equal(t, []string{current.ID, second.ID},
		[]string{restoredVersions[0].ID, restoredVersions[1].ID})
}

func TestPruneContentVersionsRetainsDependenciesAndCheckpointsAllPrior(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	created, err := s.CreateFile(ctx, s.RootID(), "reverted.txt", fakeHash("d4"), 40, "text/plain")
	require.NoError(t, err)
	replaced, replacement, err := s.ReplaceContent(
		ctx, created.ID, created.Revision, fakeHash("e5"), 50, "text/markdown",
	)
	require.NoError(t, err)
	reverted, revertVersion, _, err := s.RevertContent(
		ctx, created.ID, replaced.Revision, created.CurrentVersionID,
	)
	require.NoError(t, err)

	protected, err := s.PruneContentVersions(ctx, created.ID, reverted.Revision,
		VersionPruneSelector{VersionIDs: []string{created.CurrentVersionID}}, false)
	require.NoError(t, err)
	assert.Empty(t, protected.Candidates)
	require.Len(t, protected.DependencyRetained, 1)
	assert.Equal(t, created.CurrentVersionID, protected.DependencyRetained[0].ID)
	unchanged, err := s.PruneContentVersions(ctx, created.ID, reverted.Revision,
		VersionPruneSelector{VersionIDs: []string{created.CurrentVersionID}}, true)
	require.NoError(t, err)
	assert.False(t, unchanged.Changed)
	assert.Equal(t, reverted.Revision, unchanged.Node.Revision)

	preview, err := s.PruneContentVersions(ctx, created.ID, reverted.Revision,
		VersionPruneSelector{AllPrior: true}, false)
	require.NoError(t, err)
	assert.True(t, preview.CheckpointRequired)
	assert.Len(t, preview.Candidates, 3)
	assert.Equal(t, int64(130), preview.LogicalBytes)
	assert.Equal(t, 2, preview.UniqueBlobs)
	assert.Equal(t, 1, preview.SharedBlobs,
		"the checkpoint keeps the current/reverted blob reachable")
	assert.Equal(t, 1, preview.ReleasableBlobs)
	assert.Equal(t, int64(50), preview.LooseBytesPendingGC)

	receipt, err := s.PruneContentVersions(ctx, created.ID, reverted.Revision,
		VersionPruneSelector{AllPrior: true}, true)
	require.NoError(t, err)
	assert.True(t, receipt.Changed)
	assert.Equal(t, 3, receipt.DeletedVersions)
	require.NotNil(t, receipt.Checkpoint)
	assert.Equal(t, "content_replace", receipt.Checkpoint.TransitionKind)
	assert.Nil(t, receipt.Checkpoint.SourceVersionID)
	assert.Equal(t, created.BlobHash, receipt.Checkpoint.BlobHash)
	assert.Equal(t, reverted.Revision+1, receipt.Node.Revision)
	assert.Equal(t, receipt.Checkpoint.ID, receipt.Node.CurrentVersionID)

	versions, total, err := s.ContentVersions(ctx, created.ID, 10, 0)
	require.NoError(t, err)
	assert.Equal(t, 1, total)
	require.Len(t, versions, 1)
	assert.Equal(t, receipt.Checkpoint.ID, versions[0].ID)
	for _, id := range []string{created.CurrentVersionID, replacement.ID, revertVersion.ID} {
		_, err = s.ContentVersionByID(ctx, id)
		require.ErrorIs(t, err, ErrNotFound)
	}
	unreachable, err := s.UnreachableBlobs(ctx)
	require.NoError(t, err)
	require.Len(t, unreachable, 1)
	assert.Equal(t, replacement.BlobHash, unreachable[0].Hash)
}

func TestPruneContentVersionsReportsPackedAndSharedConsequences(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	created, err := s.CreateFile(ctx, s.RootID(), "packed.txt", fakeHash("f6"), 60, "text/plain")
	require.NoError(t, err)
	replaced, packedVersion, err := s.ReplaceContent(
		ctx, created.ID, created.Revision, fakeHash("a7"), 70, "text/plain",
	)
	require.NoError(t, err)
	replaced, _, err = s.ReplaceContent(
		ctx, created.ID, replaced.Revision, fakeHash("b8"), 80, "text/plain",
	)
	require.NoError(t, err)
	_, err = s.CreateFile(ctx, s.RootID(), "shared.txt", created.BlobHash, created.Size, "text/plain")
	require.NoError(t, err)
	_, err = s.db.Exec(`INSERT INTO blob_packs(pack_id,entry_count,stored_bytes,created_at)
		VALUES('pack-test',1,17,?)`, nowRFC3339())
	require.NoError(t, err)
	_, err = s.db.Exec(`INSERT INTO blob_pack_index(
		blob_hash,pack_id,pack_offset,stored_len,raw_len,flags,crc32c
	) VALUES(?, 'pack-test', 0, 17, ?, 0, 0)`, packedVersion.BlobHash, packedVersion.Size)
	require.NoError(t, err)

	preview, err := s.PruneContentVersions(ctx, created.ID, replaced.Revision,
		VersionPruneSelector{KeepNewest: 1}, false)
	require.NoError(t, err)
	assert.Len(t, preview.Candidates, 2)
	assert.Equal(t, 2, preview.UniqueBlobs)
	assert.Equal(t, 1, preview.SharedBlobs)
	assert.Equal(t, 1, preview.ReleasableBlobs)
	assert.Equal(t, 1, preview.PackedBlobsPendingRepack)
	assert.Equal(t, int64(17), preview.PackedBytesPendingRepack)
	assert.Zero(t, preview.LooseBytesPendingGC)
}

func TestPruneContentVersionsValidatesSelectorsAndTargets(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	first, err := s.CreateFile(ctx, s.RootID(), "first.txt", fakeHash("c9"), 90, "text/plain")
	require.NoError(t, err)
	second, err := s.CreateFile(ctx, s.RootID(), "second.txt", fakeHash("da"), 100, "text/plain")
	require.NoError(t, err)

	for name, selector := range map[string]VersionPruneSelector{
		"none":       {},
		"several":    {KeepNewest: 1, AllPrior: true},
		"zero age":   {OlderThan: 0},
		"negative":   {KeepNewest: -1},
		"duplicate":  {VersionIDs: []string{second.CurrentVersionID, second.CurrentVersionID}},
		"invalid id": {VersionIDs: []string{"not-a-uuid"}},
	} {
		t.Run(name, func(t *testing.T) {
			_, pruneErr := s.PruneContentVersions(ctx, second.ID, second.Revision, selector, false)
			require.Error(t, pruneErr)
		})
	}
	_, err = s.PruneContentVersions(ctx, second.ID, second.Revision,
		VersionPruneSelector{VersionIDs: []string{second.CurrentVersionID}}, false)
	require.ErrorIs(t, err, ErrVersionAlreadyCurrent)
	_, err = s.PruneContentVersions(ctx, second.ID, second.Revision,
		VersionPruneSelector{VersionIDs: []string{first.CurrentVersionID}}, false)
	require.ErrorIs(t, err, ErrVersionNodeMismatch)
	_, err = s.PruneContentVersions(ctx, second.ID, second.Revision,
		VersionPruneSelector{OlderThan: time.Hour}, false)
	require.NoError(t, err)

	replaced, _, err := s.ReplaceContent(
		ctx, second.ID, second.Revision, fakeHash("eb"), 110, "text/plain",
	)
	require.NoError(t, err)
	old := time.Now().UTC().Add(-2 * time.Hour).Format(timestampLayout)
	_, err = s.db.Exec(`UPDATE content_versions SET recorded_at = ? WHERE version_id = ?`,
		old, second.CurrentVersionID)
	require.NoError(t, err)
	preview, err := s.PruneContentVersions(ctx, second.ID, replaced.Revision,
		VersionPruneSelector{OlderThan: time.Hour}, false)
	require.NoError(t, err)
	require.Len(t, preview.Candidates, 1)
	assert.Equal(t, second.CurrentVersionID, preview.Candidates[0].ID)
	assert.NotEmpty(t, preview.Cutoff)
}
