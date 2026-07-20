package store

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestVectorDocumentsContainsOnlyCurrentLiveExtractedText(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	live, err := s.CreateFile(ctx, s.RootID(), "live.txt", fakeHash("a1"), 5, "text/plain")
	require.NoError(t, err)
	_, err = s.CreateFile(ctx, s.RootID(), "duplicate.txt", fakeHash("a1"), 5, "text/plain")
	require.NoError(t, err)
	trashed, err := s.CreateFile(ctx, s.RootID(), "trashed.txt", fakeHash("b2"), 5, "text/plain")
	require.NoError(t, err)
	unsupported, err := s.CreateFile(ctx, s.RootID(), "scan.pdf", fakeHash("c3"), 5, "application/pdf")
	require.NoError(t, err)
	require.NoError(t, s.RecordExtraction(ctx, ExtractionResult{
		BlobHash: live.BlobHash, Extractor: "plain-text", ExtractorVersion: 1,
		Status: ExtractionOK, Text: "current live text",
	}))
	require.NoError(t, s.RecordExtraction(ctx, ExtractionResult{
		BlobHash: trashed.BlobHash, Extractor: "plain-text", ExtractorVersion: 1,
		Status: ExtractionOK, Text: "trashed text",
	}))
	require.NoError(t, s.RecordExtraction(ctx, ExtractionResult{
		BlobHash: unsupported.BlobHash, Extractor: "plain-text", ExtractorVersion: 1,
		Status: ExtractionOK, Text: "unsupported text",
	}))
	_, _, err = s.Trash(ctx, trashed.ID, trashed.Revision)
	require.NoError(t, err)

	items, err := s.VectorDocuments(ctx, "", 100)
	require.NoError(t, err)
	require.Len(t, items, 1)
	assert.Equal(t, live.BlobHash, items[0].BlobHash)
	assert.Equal(t, 1, items[0].ExtractorVersion)
	assert.Equal(t, "current live text", items[0].Text)

	items, err = s.VectorDocuments(ctx, live.BlobHash, 100)
	require.NoError(t, err)
	assert.Empty(t, items)
}
