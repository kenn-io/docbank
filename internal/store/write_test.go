package store

import (
	"database/sql"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeHash returns a deterministic 64-char hex-looking hash for tests.
func fakeHash(seed string) string {
	return strings.Repeat("0", 64-len(seed)) + seed
}

func TestCreateFile(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()

	f, err := s.CreateFile(ctx, s.RootID(), "report.pdf", fakeHash("a1"), 1234, "application/pdf")
	require.NoError(t, err)
	assert.Equal(t, "file", f.Kind)
	assert.Equal(t, fakeHash("a1"), f.BlobHash)
	assert.Equal(t, int64(1234), f.Size)
	assert.Equal(t, "application/pdf", f.MimeType)
	require.NoError(t, validateUUIDv4(f.CurrentVersionID))
	versions, total, err := s.ContentVersions(ctx, f.ID, 10, 0)
	require.NoError(t, err)
	assert.Equal(t, 1, total)
	require.Len(t, versions, 1)
	assert.Equal(t, f.CurrentVersionID, versions[0].ID)
	assert.Equal(t, f.BlobHash, versions[0].BlobHash)
	assert.Equal(t, int64(1), versions[0].NodeRevision)
	assert.Equal(t, "content_create", versions[0].TransitionKind)
	require.NoError(t, validateUUIDv4(versions[0].IntroducedOperationID))
	_, err = s.db.Exec(`UPDATE content_versions SET transition_kind='content_replace' WHERE version_id=?`,
		f.CurrentVersionID)
	require.Error(t, err, "revision one must remain the content_create transition")

	// Blob row exists.
	var size int64
	require.NoError(t, s.db.QueryRow(
		`SELECT size FROM blobs WHERE hash = ?`, fakeHash("a1")).Scan(&size))
	assert.Equal(t, int64(1234), size)

	// Collision is strict.
	_, err = s.CreateFile(ctx, s.RootID(), "report.pdf", fakeHash("b2"), 99, "application/pdf")
	require.ErrorIs(t, err, ErrExists)

	// Same blob twice under different names: one blob row, two nodes.
	copyNode, err := s.CreateFile(ctx, s.RootID(), "copy.pdf", fakeHash("a1"), 1234, "application/pdf")
	require.NoError(t, err)
	assert.NotEqual(t, f.CurrentVersionID, copyNode.CurrentVersionID,
		"deduplicated bytes still belong to distinct document versions")
	var blobCount int
	require.NoError(t, s.db.QueryRow(`SELECT COUNT(*) FROM blobs`).Scan(&blobCount))
	assert.Equal(t, 1, blobCount)
}

func TestCreateFileRejectsBlobSizeMismatch(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()

	_, err := s.CreateFile(ctx, s.RootID(), "a.txt", fakeHash("a1"), 10, "text/plain")
	require.NoError(t, err)

	_, err = s.CreateFile(ctx, s.RootID(), "b.txt", fakeHash("a1"), 11, "text/plain")
	require.ErrorContains(t, err, "does not match")
}

func TestCreateFileRejectsFileParent(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()

	f, err := s.CreateFile(ctx, s.RootID(), "a.txt", fakeHash("a1"), 1, "text/plain")
	require.NoError(t, err)
	_, err = s.CreateFile(ctx, f.ID, "b.txt", fakeHash("b2"), 1, "text/plain")
	assert.ErrorIs(t, err, ErrNotDir)
}

func TestCurrentVersionMustBelongToItsNode(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	first, err := s.CreateFile(ctx, s.RootID(), "first.txt", fakeHash("a1"), 1, "text/plain")
	require.NoError(t, err)
	second, err := s.CreateFile(ctx, s.RootID(), "second.txt", fakeHash("b2"), 1, "text/plain")
	require.NoError(t, err)

	err = s.withTx(ctx, func(tx *sql.Tx) error {
		_, updateErr := tx.Exec(`UPDATE nodes SET current_version_id = ? WHERE id = ?`,
			second.CurrentVersionID, first.ID)
		return updateErr
	})
	require.Error(t, err)

	unchanged, err := s.NodeByID(ctx, first.ID)
	require.NoError(t, err)
	assert.Equal(t, first.CurrentVersionID, unchanged.CurrentVersionID)
}

func TestReplaceContentCreatesImmutableHead(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	created, err := s.CreateFile(ctx, s.RootID(), "report.txt", fakeHash("a1"), 3, "text/plain")
	require.NoError(t, err)

	updated, replacement, err := s.ReplaceContent(
		ctx, created.ID, created.Revision, fakeHash("b2"), 4, "text/markdown",
	)
	require.NoError(t, err)
	assert.Equal(t, created.ID, updated.ID)
	assert.Equal(t, created.Revision+1, updated.Revision)
	assert.Equal(t, replacement.ID, updated.CurrentVersionID)
	assert.Equal(t, fakeHash("b2"), updated.BlobHash)
	assert.Equal(t, int64(4), updated.Size)
	assert.Equal(t, "text/markdown", updated.MimeType)
	assert.Equal(t, updated.Revision, replacement.NodeRevision)
	assert.Equal(t, "content_replace", replacement.TransitionKind)
	assert.Nil(t, replacement.SourceVersionID)
	require.NoError(t, validateUUIDv4(replacement.ID))
	require.NoError(t, validateUUIDv4(replacement.IntroducedOperationID))

	versions, total, err := s.ContentVersions(ctx, created.ID, 10, 0)
	require.NoError(t, err)
	assert.Equal(t, 2, total)
	require.Len(t, versions, 2)
	assert.Equal(t, replacement.ID, versions[0].ID)
	assert.Equal(t, created.CurrentVersionID, versions[1].ID)
	assert.Equal(t, fakeHash("a1"), versions[1].BlobHash,
		"replacement must retain the prior immutable version")

	// Replacing with the same bytes is still an explicit versioned mutation;
	// storage deduplicates the blob while history records the operation.
	sameBytes, sameVersion, err := s.ReplaceContent(
		ctx, updated.ID, updated.Revision, updated.BlobHash, updated.Size, updated.MimeType,
	)
	require.NoError(t, err)
	assert.NotEqual(t, replacement.ID, sameVersion.ID)
	assert.Equal(t, replacement.BlobHash, sameVersion.BlobHash)
	assert.Equal(t, updated.Revision+1, sameBytes.Revision)
	var blobCount int
	require.NoError(t, s.db.QueryRow(`SELECT COUNT(*) FROM blobs`).Scan(&blobCount))
	assert.Equal(t, 2, blobCount)
}

func TestReplaceContentRejectsInvalidTargetAndStaleRevision(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	file, err := s.CreateFile(ctx, s.RootID(), "report.txt", fakeHash("a1"), 3, "text/plain")
	require.NoError(t, err)
	dir, err := s.Mkdir(ctx, s.RootID(), "folder")
	require.NoError(t, err)

	_, _, err = s.ReplaceContent(ctx, file.ID, file.Revision+1, fakeHash("b2"), 4, "text/plain")
	require.ErrorIs(t, err, ErrStaleRevision)
	_, _, err = s.ReplaceContent(ctx, dir.ID, dir.Revision, fakeHash("b2"), 4, "text/plain")
	require.ErrorIs(t, err, ErrNotFile)
	trashed, _, err := s.Trash(ctx, file.ID, file.Revision)
	require.NoError(t, err)
	_, _, err = s.ReplaceContent(ctx, trashed.ID, trashed.Revision, fakeHash("b2"), 4, "text/plain")
	require.ErrorIs(t, err, ErrNotFound)

	versions, total, err := s.ContentVersions(ctx, file.ID, 10, 0)
	require.NoError(t, err)
	assert.Equal(t, 1, total)
	require.Len(t, versions, 1)
	assert.Equal(t, file.CurrentVersionID, versions[0].ID)
	var candidateBlobs int
	require.NoError(t, s.db.QueryRow(
		`SELECT COUNT(*) FROM blobs WHERE hash = ?`, fakeHash("b2")).Scan(&candidateBlobs))
	assert.Zero(t, candidateBlobs, "failed replacements must not grant blob authority")
}

func TestRevertContentCreatesNewHeadFromPriorAuthority(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	created, err := s.CreateFile(ctx, s.RootID(), "report.txt", fakeHash("a1"), 3, "text/plain")
	require.NoError(t, err)
	replaced, replacement, err := s.ReplaceContent(
		ctx, created.ID, created.Revision, fakeHash("b2"), 4, "text/markdown",
	)
	require.NoError(t, err)

	reverted, revertVersion, source, err := s.RevertContent(
		ctx, created.ID, replaced.Revision, created.CurrentVersionID,
	)
	require.NoError(t, err)
	assert.Equal(t, created.CurrentVersionID, source.ID)
	assert.Equal(t, created.BlobHash, revertVersion.BlobHash)
	assert.Equal(t, created.Size, revertVersion.Size)
	assert.Equal(t, created.MimeType, revertVersion.MimeType)
	assert.Equal(t, "content_revert", revertVersion.TransitionKind)
	require.NotNil(t, revertVersion.SourceVersionID)
	assert.Equal(t, source.ID, *revertVersion.SourceVersionID)
	assert.Equal(t, revertVersion.ID, reverted.CurrentVersionID)
	assert.Equal(t, replaced.Revision+1, reverted.Revision)

	// Repeating the same historical choice is another explicit operation. The
	// current reversion row is distinct from its immutable source identity.
	repeated, repeatedVersion, _, err := s.RevertContent(
		ctx, created.ID, reverted.Revision, created.CurrentVersionID,
	)
	require.NoError(t, err)
	assert.NotEqual(t, revertVersion.ID, repeatedVersion.ID)
	assert.Equal(t, created.BlobHash, repeatedVersion.BlobHash)
	assert.Equal(t, reverted.Revision+1, repeated.Revision)

	versions, total, err := s.ContentVersions(ctx, created.ID, 10, 0)
	require.NoError(t, err)
	assert.Equal(t, 4, total)
	require.Len(t, versions, 4)
	assert.Equal(t, repeatedVersion.ID, versions[0].ID)
	assert.Equal(t, revertVersion.ID, versions[1].ID)
	assert.Equal(t, replacement.ID, versions[2].ID)
	assert.Equal(t, created.CurrentVersionID, versions[3].ID)
	var blobCount int
	require.NoError(t, s.db.QueryRow(`SELECT COUNT(*) FROM blobs`).Scan(&blobCount))
	assert.Equal(t, 2, blobCount, "reversion must not create or copy a blob")
}

func TestRevertContentRejectsInvalidSourceAndTarget(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	first, err := s.CreateFile(ctx, s.RootID(), "first.txt", fakeHash("a1"), 3, "text/plain")
	require.NoError(t, err)
	second, err := s.CreateFile(ctx, s.RootID(), "second.txt", fakeHash("b2"), 4, "text/plain")
	require.NoError(t, err)
	dir, err := s.Mkdir(ctx, s.RootID(), "folder")
	require.NoError(t, err)

	rejectedNode, rejectedVersion, rejectedSource, err := s.RevertContent(
		ctx, first.ID, first.Revision, first.CurrentVersionID,
	)
	require.ErrorIs(t, err, ErrVersionAlreadyCurrent)
	assert.Equal(t, Node{}, rejectedNode)
	assert.Equal(t, ContentVersion{}, rejectedVersion)
	assert.Equal(t, ContentVersion{}, rejectedSource)
	wrongNode, wrongVersion, wrongSource, err := s.RevertContent(
		ctx, first.ID, first.Revision, second.CurrentVersionID,
	)
	require.ErrorIs(t, err, ErrVersionNodeMismatch)
	assert.Equal(t, Node{}, wrongNode)
	assert.Equal(t, ContentVersion{}, wrongVersion)
	assert.Equal(t, ContentVersion{}, wrongSource)
	staleNode, staleVersion, staleSource, err := s.RevertContent(
		ctx, first.ID, first.Revision+1, second.CurrentVersionID,
	)
	require.ErrorIs(t, err, ErrStaleRevision)
	assert.Equal(t, Node{}, staleNode)
	assert.Equal(t, ContentVersion{}, staleVersion)
	assert.Equal(t, ContentVersion{}, staleSource)
	dirNode, dirVersion, dirSource, err := s.RevertContent(
		ctx, dir.ID, dir.Revision, first.CurrentVersionID,
	)
	require.ErrorIs(t, err, ErrNotFile)
	assert.Equal(t, Node{}, dirNode)
	assert.Equal(t, ContentVersion{}, dirVersion)
	assert.Equal(t, ContentVersion{}, dirSource)
	missingNode, missingVersion, missingSource, err := s.RevertContent(ctx, first.ID, first.Revision,
		"11111111-1111-4111-8111-111111111111")
	require.ErrorIs(t, err, ErrNotFound)
	assert.Equal(t, Node{}, missingNode)
	assert.Equal(t, ContentVersion{}, missingVersion)
	assert.Equal(t, ContentVersion{}, missingSource)

	versions, total, err := s.ContentVersions(ctx, first.ID, 10, 0)
	require.NoError(t, err)
	assert.Equal(t, 1, total)
	require.Len(t, versions, 1)
}

func TestContentVersionsRequiresBoundedPage(t *testing.T) {
	s := newTestStore(t)
	file, err := s.CreateFile(t.Context(), s.RootID(), "bounded.txt", fakeHash("a1"), 1, "text/plain")
	require.NoError(t, err)

	_, _, err = s.ContentVersions(t.Context(), file.ID, 0, 0)
	require.ErrorContains(t, err, "between 1 and 1000")
	_, _, err = s.ContentVersions(t.Context(), file.ID, 1001, 0)
	require.ErrorContains(t, err, "between 1 and 1000")
	_, _, err = s.ContentVersions(t.Context(), file.ID, 1, -1)
	require.ErrorContains(t, err, "must not be negative")
	versions, total, err := s.ContentVersions(t.Context(), file.ID, 1, 1)
	require.NoError(t, err)
	assert.Empty(t, versions)
	assert.Equal(t, 1, total, "an exhausted page still reports its snapshot's total")
	_, _, err = s.ContentVersions(t.Context(), s.RootID(), 1, 0)
	require.ErrorIs(t, err, ErrNotFile)
	_, _, err = s.ContentVersions(t.Context(), file.ID+1000, 1, 0)
	require.ErrorIs(t, err, ErrNotFound)
}
