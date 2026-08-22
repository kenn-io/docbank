package document_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
)

func TestPublicSourceEvidenceAndPolicy(t *testing.T) {
	policy, err := document.NewNormalizePolicy(25_000_000)
	require.NoError(t, err)
	assert.Equal(t, 3, policy.Identity().Version)
	assert.Equal(t, 25_000_000, policy.Identity().MaxDocumentChars)

	source := document.SourceDocument{
		Family:   "pdf",
		UnitKind: "page",
		Units: []document.SourceUnit{{
			Index:    0,
			Markdown: "# Synthetic report",
			Header:   "Example header",
			Footer:   "Page 1",
			Dimensions: document.UnitDimensions{
				DPI: 200, Height: 2200, Width: 1700,
			},
		}},
	}
	require.Len(t, source.Units, 1)
	assert.Equal(t, "pdf", source.Family)
	assert.Equal(t, "page", source.UnitKind)
	assert.Equal(t, 0, source.Units[0].Index)
	assert.Equal(t, "# Synthetic report", source.Units[0].Markdown)
	assert.Equal(t, "Example header", source.Units[0].Header)
	assert.Equal(t, "Page 1", source.Units[0].Footer)
	assert.Equal(t, document.UnitDimensions{DPI: 200, Height: 2200, Width: 1700}, source.Units[0].Dimensions)

	normalized, err := document.NormalizeDocument(source, policy)
	require.NoError(t, err)
	require.NoError(t, document.ValidateNormalizedDocument(normalized))
	require.Len(t, normalized.Units, 1)
	require.NotEmpty(t, normalized.Units[0].HeadingMarks)
	require.NotEmpty(t, normalized.Chunks)
	require.NotEmpty(t, normalized.Chunks[0].Spans)
	assert.Equal(t, "Synthetic report", normalized.Units[0].HeadingMarks[0].Path[0])
	assert.Equal(t, 0, normalized.Chunks[0].Ordinal)
	assert.Equal(t, 0, normalized.Chunks[0].Spans[0].UnitIndex)

	stale := normalized
	stale.Chunks = append([]document.Chunk(nil), normalized.Chunks...)
	stale.Chunks[0].Text = "altered normalized evidence"
	stale.Chunks[0].CharCount = len([]rune(stale.Chunks[0].Text))
	require.Error(t, document.ValidateNormalizedDocument(stale))

	partial := normalized
	partial.Chunks = nil
	assert.ErrorContains(t, document.ValidateNormalizedDocument(partial), "checksum")
}
