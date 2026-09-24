package store

import (
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/report"
)

func TestTermReportNativeTextRejectsBeforeLoading(t *testing.T) {
	for _, tc := range []struct {
		name       string
		runes      int
		textBudget int64
		wantErr    error
	}{
		{"byte limit", 9 << 20, 32 << 20, report.ErrReportLimit},
		{"budget denial", 4 << 20, 0, report.ErrBudgetExhausted},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestStore(t)
			text := strings.Repeat("é", tc.runes)
			hash := testSHA256([]byte(text))
			node, err := s.CreateFile(t.Context(), s.RootID(), "native.txt", hash, int64(len(text)), "text/plain")
			require.NoError(t, err)
			require.NoError(t, s.RecordExtraction(t.Context(), ExtractionResult{
				BlobHash: hash, Extractor: "synthetic-native", ExtractorVersion: 1,
				Status: ExtractionOK, Text: text,
			}))
			identity := report.Identity{NodeID: node.ID, VersionID: node.CurrentVersionID, SHA256: hash}
			frame := report.Frame{Members: []report.Member{{Identity: identity}}}
			budget := report.NewBudget(1024)
			defer func() { _ = budget.Close() }()
			textBudget := report.NewBudget(tc.textBudget)
			defer func() { _ = textBudget.Close() }()
			runtime.GC()
			var before, after runtime.MemStats
			runtime.ReadMemStats(&before)
			err = s.withLexicalGenerationRead(t.Context(), func(q metadataQuerier, _ LexicalGeneration) error {
				return readTermReportNativeText(t.Context(), q, termReportVersion{identity: identity}, 0,
					budget, textBudget, &frame)
			})
			runtime.ReadMemStats(&after)
			require.ErrorIs(t, err, tc.wantErr)
			// Allow ample query overhead, but reject allocating even half the payload.
			require.Less(t, after.TotalAlloc-before.TotalAlloc, uint64(len(text)/2),
				"rejected native text must not be loaded into Go memory")
			require.Zero(t, textBudget.Used())
			require.Empty(t, frame.Texts)
		})
	}
}

func TestTermReportNativeTextBindsSelectedRow(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	const text = "dated 2024-05-06 café"
	hash := testSHA256([]byte(text))
	node, err := s.CreateFile(t.Context(), s.RootID(), "native.txt", hash, int64(len(text)), "text/plain")
	require.NoError(t, err)
	for _, result := range []ExtractionResult{
		{Extractor: "older", Status: ExtractionOK, Text: "other text"},
		{Extractor: "selected", Status: ExtractionOK, Text: text},
		{Extractor: "failed", Status: ExtractionFailed, Error: "synthetic failure"},
	} {
		result.BlobHash, result.ExtractorVersion = hash, 1
		require.NoError(t, s.RecordExtraction(t.Context(), result))
	}
	identity := report.Identity{NodeID: node.ID, VersionID: node.CurrentVersionID, SHA256: hash}
	frame := report.Frame{Members: []report.Member{{Identity: identity}}}
	budget := report.NewBudget(1024)
	defer func() { _ = budget.Close() }()
	textBudget := report.NewBudget(int64(len(text)))
	defer func() { _ = textBudget.Close() }()
	require.NoError(t, s.withLexicalGenerationRead(t.Context(), func(q metadataQuerier, _ LexicalGeneration) error {
		return readTermReportNativeText(t.Context(), q, termReportVersion{identity: identity}, 0,
			budget, textBudget, &frame)
	}))
	require.Len(t, frame.Texts, 1)
	require.Equal(t, "selected", frame.Texts[0].Native.ExtractorID)
	require.Equal(t, text, string(frame.Texts[0].Native.Text))
	require.Equal(t, int64(len(text)), textBudget.Used())
}
