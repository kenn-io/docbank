package store

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestContentReferencesByHashIncludesCurrentHistoricalAndTrash(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	wanted := fakeHash("a1")
	replacement := fakeHash("b2")
	const wantedMD5 = "f6fdffe48c908deb0f4c3bd36c032e72"
	const replacementMD5 = "8d777f385d3dfec8815d20f7496026dc"

	historical, err := s.CreateFile(ctx, s.RootID(), "historical.txt", wanted, 10, "text/plain")
	require.NoError(t, err)
	historicalVersion := historical.CurrentVersionID
	historical, _, err = s.ReplaceContent(
		ctx, historical.ID, historical.Revision, replacement, 11, "text/plain",
	)
	require.NoError(t, err)

	current, err := s.CreateFile(ctx, s.RootID(), "current.txt", wanted, 10, "text/plain")
	require.NoError(t, err)
	trashed, err := s.CreateFile(ctx, s.RootID(), "trashed.txt", wanted, 10, "text/plain")
	require.NoError(t, err)
	trashed, _, err = s.Trash(ctx, trashed.ID, trashed.Revision)
	require.NoError(t, err)
	require.NoError(t, s.RecordVerifiedBlobChecksum(ctx,
		BlobChecksumRecord{BlobSHA256: wanted, MD5: wantedMD5}))
	require.NoError(t, s.RecordVerifiedBlobChecksum(ctx,
		BlobChecksumRecord{BlobSHA256: replacement, MD5: replacementMD5}))

	refs, total, err := s.ContentReferencesByHash(ctx, wanted, 10, 0)
	require.NoError(t, err)
	assert.Equal(t, 3, total)
	require.Len(t, refs, 3)

	assert.Equal(t, current.ID, refs[0].Node.ID, "live current references sort first")
	assert.Equal(t, current.CurrentVersionID, refs[0].Version.ID)
	assert.Equal(t, wantedMD5, refs[0].Version.MD5)
	assert.Equal(t, wantedMD5, refs[0].Node.MD5)
	assert.True(t, refs[0].IsCurrent)
	assert.Equal(t, "/current.txt", refs[0].Path)
	assert.Nil(t, refs[0].Node.TrashedAt)

	assert.Equal(t, historical.ID, refs[1].Node.ID, "live history follows live current references")
	assert.Equal(t, historicalVersion, refs[1].Version.ID)
	assert.False(t, refs[1].IsCurrent)
	assert.Equal(t, replacement, refs[1].Node.BlobHash,
		"the node projection describes its current authority")
	assert.Equal(t, wantedMD5, refs[1].Version.MD5)
	assert.Equal(t, replacementMD5, refs[1].Node.MD5)
	assert.Equal(t, "/historical.txt", refs[1].Path)

	assert.Equal(t, trashed.ID, refs[2].Node.ID, "trashed references sort last")
	assert.True(t, refs[2].IsCurrent)
	assert.NotNil(t, refs[2].Node.TrashedAt)
	assert.Empty(t, refs[2].Path, "trashed nodes have no resolvable current path")

	page, pageTotal, err := s.ContentReferencesByHash(ctx, wanted, 1, 1)
	require.NoError(t, err)
	assert.Equal(t, 3, pageTotal)
	require.Len(t, page, 1)
	assert.Equal(t, historicalVersion, page[0].Version.ID)

	exhausted, exhaustedTotal, err := s.ContentReferencesByHash(ctx, wanted, 1, 10)
	require.NoError(t, err)
	assert.Equal(t, 3, exhaustedTotal)
	assert.Empty(t, exhausted)
}

func TestContentReferencesByHashRequiresLogicalAuthorityAndBoundedInput(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	orphan := fakeHash("c3")

	// A catalog row without a content version is physical authority only and
	// must not be presented as a document reference.
	require.NoError(t, s.withStorageTx(ctx, func(tx *sql.Tx) error {
		return s.EnsureBlobTx(tx, orphan, 7)
	}))
	refs, total, err := s.ContentReferencesByHash(ctx, orphan, 10, 0)
	require.NoError(t, err)
	assert.Zero(t, total)
	assert.Empty(t, refs)

	_, _, err = s.ContentReferencesByHash(ctx, "ABC", 10, 0)
	require.ErrorContains(t, err, "canonical lowercase SHA-256")
	_, _, err = s.ContentReferencesByHash(ctx, orphan, 0, 0)
	require.ErrorContains(t, err, "between 1 and 1000")
	_, _, err = s.ContentReferencesByHash(ctx, orphan, 1001, 0)
	require.ErrorContains(t, err, "between 1 and 1000")
	_, _, err = s.ContentReferencesByHash(ctx, orphan, 1, -1)
	require.ErrorContains(t, err, "must not be negative")
}

func TestContentReferencesByHashPagesRepeatedVersions(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	parent, err := s.MkdirAll(ctx, "/nested/folder")
	require.NoError(t, err)
	wanted := fakeHash("a1")
	file, err := s.CreateFile(ctx, parent.ID, "repeated.txt", wanted, 10, "text/plain")
	require.NoError(t, err)
	versions := []string{file.CurrentVersionID}
	for _, hash := range []string{fakeHash("b2"), wanted, fakeHash("b2"), wanted} {
		file, _, err = s.ReplaceContent(ctx, file.ID, file.Revision, hash, 10, "text/plain")
		require.NoError(t, err)
		if hash == wanted {
			versions = append(versions, file.CurrentVersionID)
		}
	}
	other, err := s.CreateFile(ctx, s.RootID(), "other.txt", wanted, 10, "text/plain")
	require.NoError(t, err)

	first, total, err := s.ContentReferencesByHash(ctx, wanted, 2, 0)
	require.NoError(t, err)
	require.Equal(t, 4, total)
	require.Len(t, first, 2)
	assert.Equal(t, versions[2], first[0].Version.ID)
	assert.Equal(t, "/nested/folder/repeated.txt", first[0].Path)
	assert.True(t, first[0].IsCurrent)
	assert.Equal(t, other.CurrentVersionID, first[1].Version.ID)
	assert.Equal(t, "/other.txt", first[1].Path)
	assert.True(t, first[1].IsCurrent)

	history, total, err := s.ContentReferencesByHash(ctx, wanted, 2, 2)
	require.NoError(t, err)
	require.Equal(t, 4, total)
	require.Len(t, history, 2)
	assert.Equal(t, versions[1], history[0].Version.ID)
	assert.Equal(t, versions[0], history[1].Version.ID)
	for _, ref := range history {
		assert.Equal(t, file.ID, ref.Node.ID)
		assert.Equal(t, versions[2], ref.Node.CurrentVersionID)
		assert.Equal(t, "/nested/folder/repeated.txt", ref.Path)
		assert.False(t, ref.IsCurrent)
	}

	empty, total, err := s.ContentReferencesByHash(ctx, wanted, 2, 4)
	require.NoError(t, err)
	assert.Equal(t, 4, total)
	assert.Empty(t, empty)
}
