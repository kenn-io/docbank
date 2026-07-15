package store

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestContentReferencesByHashIncludesCurrentHistoricalAndTrash(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	wanted := fakeHash("a1")
	replacement := fakeHash("b2")

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

	refs, total, err := s.ContentReferencesByHash(ctx, wanted, 10, 0)
	require.NoError(t, err)
	assert.Equal(t, 3, total)
	require.Len(t, refs, 3)

	assert.Equal(t, current.ID, refs[0].Node.ID, "live current references sort first")
	assert.Equal(t, current.CurrentVersionID, refs[0].Version.ID)
	assert.True(t, refs[0].IsCurrent)
	assert.Equal(t, "/current.txt", refs[0].Path)
	assert.Nil(t, refs[0].Node.TrashedAt)

	assert.Equal(t, historical.ID, refs[1].Node.ID, "live history follows live current references")
	assert.Equal(t, historicalVersion, refs[1].Version.ID)
	assert.False(t, refs[1].IsCurrent)
	assert.Equal(t, replacement, refs[1].Node.BlobHash,
		"the node projection describes its current authority")
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
	s := newTestStore(t)
	ctx := t.Context()
	orphan := fakeHash("c3")

	// A catalog row without a content version is physical authority only and
	// must not be presented as a document reference.
	require.NoError(t, s.withTx(ctx, func(tx *sql.Tx) error {
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
