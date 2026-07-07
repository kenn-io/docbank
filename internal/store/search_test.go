package store

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSearchFindsLiveNodesOnly(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()

	docs, err := s.Mkdir(ctx, s.RootID(), "docs")
	require.NoError(t, err)
	_, err = s.CreateFile(ctx, docs.ID, "tax-return-2024.pdf", fakeHash("a1"), 1, "application/pdf")
	require.NoError(t, err)
	trashed, err := s.CreateFile(ctx, docs.ID, "tax-return-2019.pdf", fakeHash("b2"), 1, "application/pdf")
	require.NoError(t, err)
	require.NoError(t, s.Trash(ctx, trashed.ID))

	hits, err := s.Search(ctx, "tax", 0)
	require.NoError(t, err)
	require.Len(t, hits, 1)
	assert.Equal(t, "tax-return-2024.pdf", hits[0].Node.Name)
	assert.Equal(t, "/docs/tax-return-2024.pdf", hits[0].Path)
}

func TestSearchPrefixAndRename(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()

	f, err := s.CreateFile(ctx, s.RootID(), "insurance-policy.pdf", fakeHash("a1"), 1, "application/pdf")
	require.NoError(t, err)

	hits, err := s.Search(ctx, "insur", 0)
	require.NoError(t, err)
	require.Len(t, hits, 1)

	// Rename must update the index (FTS triggers).
	_, err = s.Move(ctx, f.ID, s.RootID(), "car-policy.pdf")
	require.NoError(t, err)
	hits, err = s.Search(ctx, "insur", 0)
	require.NoError(t, err)
	assert.Empty(t, hits)
	hits, err = s.Search(ctx, "car", 0)
	require.NoError(t, err)
	assert.Len(t, hits, 1)
}

func TestSearchSurvivesOperatorInput(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	_, err := s.CreateFile(ctx, s.RootID(), "a.txt", fakeHash("a1"), 1, "text/plain")
	require.NoError(t, err)

	// FTS operator syntax in user input must not error.
	for _, q := range []string{`"unbalanced`, `AND OR NOT`, `a*b(c)`} {
		_, err := s.Search(ctx, q, 0)
		assert.NoError(t, err, q)
	}
}

func TestSearchRanksMoreRelevantFirst(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()

	// Create two files: one with term frequency 3, one with frequency 1.
	// BM25 ranking should place the higher-frequency match first. The
	// less-relevant name is inserted FIRST so unordered rowid/scan order
	// disagrees with rank order — dropping the ORDER BY fails this test.
	_, err := s.CreateFile(ctx, s.RootID(), "tax report.pdf", fakeHash("b2"), 1, "application/pdf")
	require.NoError(t, err)
	_, err = s.CreateFile(ctx, s.RootID(), "tax tax tax.pdf", fakeHash("a1"), 1, "application/pdf")
	require.NoError(t, err)

	hits, err := s.Search(ctx, "tax", 0)
	require.NoError(t, err)
	require.Len(t, hits, 2)
	assert.Equal(t, "tax tax tax.pdf", hits[0].Node.Name)
	assert.Equal(t, "tax report.pdf", hits[1].Node.Name)
}

func TestSearchTieBreaksByName(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()

	// Same token count and term frequency → equal BM25 rank. Insert in
	// reverse name order so unordered scan order disagrees with the name
	// tie-break — dropping the secondary ORDER BY fails this test.
	_, err := s.CreateFile(ctx, s.RootID(), "tax c.pdf", fakeHash("c3"), 1, "application/pdf")
	require.NoError(t, err)
	_, err = s.CreateFile(ctx, s.RootID(), "tax b.pdf", fakeHash("b2"), 1, "application/pdf")
	require.NoError(t, err)
	_, err = s.CreateFile(ctx, s.RootID(), "tax a.pdf", fakeHash("a1"), 1, "application/pdf")
	require.NoError(t, err)

	hits, err := s.Search(ctx, "tax", 0)
	require.NoError(t, err)
	require.Len(t, hits, 3)
	assert.Equal(t, "tax a.pdf", hits[0].Node.Name)
	assert.Equal(t, "tax b.pdf", hits[1].Node.Name)
	assert.Equal(t, "tax c.pdf", hits[2].Node.Name)
}
