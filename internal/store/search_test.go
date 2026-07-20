package store

import (
	"fmt"
	"testing"
	"time"

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
	_, _, err = s.Trash(ctx, trashed.ID, -1)
	require.NoError(t, err)

	hits, _, err := s.SearchPage(ctx, "tax", 0)
	require.NoError(t, err)
	require.Len(t, hits, 1)
	assert.Equal(t, "tax-return-2024.pdf", hits[0].Node.Name)
	assert.Equal(t, "/docs/tax-return-2024.pdf", hits[0].Path)
	assert.Equal(t, SearchMatchName, hits[0].Match)
}

func TestSearchPrefixAndRename(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()

	f, err := s.CreateFile(ctx, s.RootID(), "insurance-policy.pdf", fakeHash("a1"), 1, "application/pdf")
	require.NoError(t, err)

	hits, _, err := s.SearchPage(ctx, "insur", 0)
	require.NoError(t, err)
	require.Len(t, hits, 1)

	// Rename must update the index (FTS triggers).
	_, _, err = s.Move(ctx, f.ID, s.RootID(), "car-policy.pdf", -1)
	require.NoError(t, err)
	hits, _, err = s.SearchPage(ctx, "insur", 0)
	require.NoError(t, err)
	assert.Empty(t, hits)
	hits, _, err = s.SearchPage(ctx, "car", 0)
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
		_, _, err := s.SearchPage(ctx, q, 0)
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

	hits, _, err := s.SearchPage(ctx, "tax", 0)
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

	hits, _, err := s.SearchPage(ctx, "tax", 0)
	require.NoError(t, err)
	require.Len(t, hits, 3)
	assert.Equal(t, "tax a.pdf", hits[0].Node.Name)
	assert.Equal(t, "tax b.pdf", hits[1].Node.Name)
	assert.Equal(t, "tax c.pdf", hits[2].Node.Name)
}

func TestSearchPageReportsTruncation(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	for i, name := range []string{"report-a.pdf", "report-b.pdf", "report-c.pdf"} {
		_, err := s.CreateFile(ctx, s.RootID(), name, fakeHash(string(rune('a'+i))), 1, "application/pdf")
		require.NoError(t, err)
	}

	hits, truncated, err := s.SearchPage(ctx, "report", 2)
	require.NoError(t, err)
	assert.Len(t, hits, 2)
	assert.True(t, truncated)

	hits, truncated, err = s.SearchPage(ctx, "report", 3)
	require.NoError(t, err)
	assert.Len(t, hits, 3)
	assert.False(t, truncated)
}

func TestSearchContentFollowsStableNameMatches(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()

	nameMatch, err := s.CreateFile(
		ctx, s.RootID(), "quarterly-forecast.md", fakeHash("a1"), 5, "text/markdown",
	)
	require.NoError(t, err)
	bodyMatch, err := s.CreateFile(
		ctx, s.RootID(), "notes.md", fakeHash("b2"), 5, "text/markdown; charset=utf-8",
	)
	require.NoError(t, err)
	unsupported, err := s.CreateFile(
		ctx, s.RootID(), "scan.pdf", bodyMatch.BlobHash, 5, "application/pdf",
	)
	require.NoError(t, err)
	require.NoError(t, s.RecordExtraction(ctx, ExtractionResult{
		BlobHash: nameMatch.BlobHash, Extractor: "plain-text", ExtractorVersion: 1,
		Status: ExtractionOK, Text: "unrelated body",
	}))
	require.NoError(t, s.RecordExtraction(ctx, ExtractionResult{
		BlobHash: bodyMatch.BlobHash, Extractor: "plain-text", ExtractorVersion: 1,
		Status: ExtractionOK, Text: "quarterly forecast assumptions",
	}))

	hits, truncated, err := s.SearchPage(ctx, "quarterly", 10)
	require.NoError(t, err)
	require.Len(t, hits, 2)
	assert.False(t, truncated)
	assert.Equal(t, nameMatch.ID, hits[0].Node.ID)
	assert.Equal(t, SearchMatchName, hits[0].Match)
	assert.Equal(t, bodyMatch.ID, hits[1].Node.ID)
	assert.Equal(t, SearchMatchContent, hits[1].Match)
	assert.NotEqual(t, unsupported.ID, hits[1].Node.ID,
		"a shared blob does not make an unsupported current MIME searchable")

	// The same limit still returns the filename match first and truthfully
	// reports that a content match remains.
	hits, truncated, err = s.SearchPage(ctx, "quarterly", 1)
	require.NoError(t, err)
	require.Len(t, hits, 1)
	assert.Equal(t, nameMatch.ID, hits[0].Node.ID)
	assert.True(t, truncated)

	// Relabeling the current bytes with an unsupported MIME must revoke the
	// content match even though the immutable blob's derived text remains.
	_, _, err = s.ReplaceContent(
		ctx, bodyMatch.ID, bodyMatch.Revision, bodyMatch.BlobHash, bodyMatch.Size,
		"application/octet-stream",
	)
	require.NoError(t, err)
	hits, truncated, err = s.SearchPage(ctx, "quarterly", 10)
	require.NoError(t, err)
	require.Len(t, hits, 1)
	assert.False(t, truncated)
	assert.Equal(t, nameMatch.ID, hits[0].Node.ID)
}

func TestPendingAndFailedTextExtractions(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	textNode, err := s.CreateFile(
		ctx, s.RootID(), "notes.txt", fakeHash("a1"), 10, "text/plain; charset=utf-8",
	)
	require.NoError(t, err)
	jsonNode, err := s.CreateFile(
		ctx, s.RootID(), "session.jsonl", fakeHash("b2"), 20, "application/x-ndjson",
	)
	require.NoError(t, err)
	_, err = s.CreateFile(ctx, s.RootID(), "scan.pdf", fakeHash("c3"), 30, "application/pdf")
	require.NoError(t, err)
	oldTextHash := textNode.BlobHash
	textNode, _, err = s.ReplaceContent(
		ctx, textNode.ID, textNode.Revision, fakeHash("a0"), 12, "text/plain",
	)
	require.NoError(t, err)

	_, err = s.db.Exec(`DELETE FROM text_extraction_queue`)
	require.NoError(t, err)
	pending, err := s.PendingTextExtractions(ctx, 10)
	require.NoError(t, err)
	assert.Empty(t, pending)
	require.NoError(t, s.SeedTextExtractionQueue(ctx, "plain-text", 1))

	pending, err = s.PendingTextExtractions(ctx, 10)
	require.NoError(t, err)
	require.Len(t, pending, 2)
	assert.ElementsMatch(t, []string{textNode.BlobHash, jsonNode.BlobHash},
		[]string{pending[0].BlobHash, pending[1].BlobHash})
	assert.NotEqual(t, oldTextHash, textNode.BlobHash,
		"startup discovery should seed selected versions, not retained history")

	require.NoError(t, s.RecordExtraction(ctx, ExtractionResult{
		BlobHash: textNode.BlobHash, Extractor: "plain-text", ExtractorVersion: 1,
		Status: ExtractionFailed, Error: "not valid UTF-8",
	}))
	pending, err = s.PendingTextExtractions(ctx, 10)
	require.NoError(t, err)
	require.Len(t, pending, 1)
	assert.Equal(t, jsonNode.BlobHash, pending[0].BlobHash)

	// A future extractor implementation naturally retries the old result.
	require.NoError(t, s.SeedTextExtractionQueue(ctx, "plain-text", 2))
	pending, err = s.PendingTextExtractions(ctx, 10)
	require.NoError(t, err)
	assert.Len(t, pending, 2)
}

func TestTextExtractionQueueDefersFailuresBehindReadyWork(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	hashes := make([]string, 65)
	for i := range hashes {
		hashes[i] = fakeHash(fmt.Sprintf("%02x", i+1))
		_, err := s.CreateFile(
			ctx, s.RootID(), fmt.Sprintf("item-%02d.txt", i+1),
			hashes[i], 1, "text/plain",
		)
		require.NoError(t, err)
	}
	notBefore := time.Now().UTC().Add(time.Hour)
	for _, hash := range hashes[:64] {
		require.NoError(t, s.DeferTextExtraction(ctx, hash, notBefore))
	}

	pending, err := s.PendingTextExtractions(ctx, 64)
	require.NoError(t, err)
	require.Len(t, pending, 1)
	assert.Equal(t, hashes[64], pending[0].BlobHash,
		"deferred failures must not starve later ready work")
}
