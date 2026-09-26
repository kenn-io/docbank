package processing

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"

	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/internal/store"
)

func TestReconstructSegmentRangesKeepsExactRepeatedUnicodeBytes(t *testing.T) {
	body := []byte("# 同じ\n\n👋 é\n\n---\n\n# 同じ\n\n👋 é\n")
	digest := sha256.Sum256(body)
	build := store.RenditionBuildRecord{
		MarkdownChecksum: hex.EncodeToString(digest[:]),
		Units: []store.RenditionUnitRecord{
			{ID: "first"}, {ID: "second"},
		},
		LexicalSegments: []store.RenditionLexicalSegmentRecord{
			{ID: "one", UnitID: "first", CharStart: 0, CharEnd: 10, Text: "# 同じ\n\n👋 é"},
			{ID: "two", UnitID: "second", CharStart: 0, CharEnd: 10, Text: "# 同じ\n\n👋 é"},
		},
	}
	got, spans, err := reconstructSegmentRanges(build)
	require.NoError(t, err)
	require.Equal(t, body, got)
	require.NotEqual(t, spans["one"], spans["two"])
	require.Equal(t, "# 同じ\n\n👋 é", string(got[spans["two"].Start:spans["two"].End]))
}
