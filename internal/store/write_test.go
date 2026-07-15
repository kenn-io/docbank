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
