package retrieval

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/store"
)

func TestPassageEvaluationCapturesServingExcerptAndAcceptsTopK(t *testing.T) {
	catalog, err := store.Open(filepath.Join(t.TempDir(), "catalog.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, catalog.Close()) })
	for i := range 4 {
		text := strings.Repeat("unrelated material ", 100) + "lantern archive is deep in this text"
		hash := fmt.Sprintf("%064x", i+1)
		_, err := catalog.CreateFile(t.Context(), catalog.RootID(), fmt.Sprintf("document-%d.txt", i), hash, int64(len(text)), "text/plain")
		require.NoError(t, err)
		require.NoError(t, catalog.RecordExtraction(t.Context(), store.ExtractionResult{BlobHash: hash, Extractor: "synthetic", ExtractorVersion: 1, Status: store.ExtractionOK, Text: text}))
	}
	searcher, err := NewSearcher(SearcherConfig{Backend: catalog, Owner: "offline", LeaseDuration: time.Minute})
	require.NoError(t, err)
	query := Query{Text: "lantern archive", Mode: ModeLexical, Limit: 2}
	baseline, err := searcher.Search(t.Context(), query)
	require.NoError(t, err)
	prepared, err := searcher.PreparePassageEvaluation(t.Context(), query)
	require.NoError(t, err)
	require.True(t, prepared.Report.Truncated)
	require.Len(t, prepared.Candidates, 2)
	want := rerankingCandidates(baseline.Results, 2, 4096)
	for i, candidate := range prepared.Candidates {
		require.Equal(t, want[i].Document.ContentVersionID, candidate.DocumentID)
		require.Equal(t, want[i].Excerpt, candidate.Excerpt)
		require.Contains(t, candidate.Excerpt, "lantern")
		require.NotContains(t, candidate.Excerpt, strings.Repeat("unrelated material ", 100))
	}
	selected, diagnostics, err := prepared.Select(t.Context(), 3)
	require.NoError(t, err)
	require.Equal(t, 2, diagnostics.Fallbacks)
	require.Equal(t, prepared.Candidates, selected)
	after, err := searcher.Search(t.Context(), query)
	require.NoError(t, err)
	require.Equal(t, baseline, after, "capture must leave the serving searcher unchanged")
	node, err := catalog.NodeByID(t.Context(), baseline.Results[0].Document.NodeID)
	require.NoError(t, err)
	_, _, err = catalog.Trash(t.Context(), node.ID, node.Revision)
	require.NoError(t, err)
	_, _, err = prepared.Select(t.Context(), 1)
	require.ErrorContains(t, err, "stale")
}

func TestPassageOverlapIsSourceBuildAndUnitScoped(t *testing.T) {
	prior := evaluationPassage{source: "lexical", build: "b", unit: "u", start: 0, end: 100}
	for _, tc := range []struct {
		source, build, unit string
		start, end          int
		overlap             bool
	}{
		{"lexical", "b", "u", 40, 100, true},
		{"lexical", "b", "u", 50, 150, false},
		{"chunk", "b", "u", 0, 100, false},
		{"lexical", "other", "u", 0, 100, false},
		{"lexical", "b", "other", 0, 100, false},
	} {
		require.Equal(t, tc.overlap, overlapsEvaluationPassage([]evaluationPassage{prior}, evaluationPassage{source: tc.source, build: tc.build, unit: tc.unit, start: tc.start, end: tc.end}))
	}
}
