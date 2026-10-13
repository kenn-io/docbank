package main

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
)

func TestSearchAgentLexical(t *testing.T) {
	cmd, out := processingTestCommand()
	report := api.SearchReport{Limit: 1, Hits: []api.SearchHit{{Node: api.Node{ID: 42, Kind: "file", Name: "report\n2026.txt"}, Path: "/docs/report\n2026.txt", Match: "name"}}, Truncated: true}
	require.NoError(t, writeLexicalSearchAgent(cmd, "report 2026", report, "processing_failed"))
	assert.Equal(t, "OK search count=1 query=\"report 2026\" mode=lexical degraded=processing_failed truncated=true\n- selector=id:42 match=name score=0 path=\"/docs/report 2026.txt\" excerpt=\"report 2026.txt\"\n", out.String())
	out.Reset()
	require.NoError(t, writeLexicalSearchAgent(cmd, "none", api.SearchReport{Limit: 50}, ""))
	assert.Equal(t, "OK search count=0 query=none mode=lexical degraded=false truncated=false\n", out.String())
}

func TestSearchAgentProcessingLanes(t *testing.T) {
	cmd, out := processingTestCommand()
	report := searchAgentDocumentReport(
		api.DocumentSearchResult{NodeID: 1, Path: "/one", Score: .5, LexicalRank: 1, SemanticRank: 1, Excerpt: "alpha\n\t beta"},
		api.DocumentSearchResult{NodeID: 2, Path: "/two", Score: .25, LexicalRank: 2},
		api.DocumentSearchResult{NodeID: 3, Path: "/three", Score: .125, SemanticRank: 2},
	)
	report.Coverage.State, report.Coverage.CompleteDocuments = "incomplete", 2
	require.NoError(t, writeDocumentSearchAgent(cmd, "alpha", report))
	assert.Equal(t, "OK search count=3 query=alpha mode=hybrid degraded=false truncated=false coverage=incomplete complete=2 scoped=3\n- selector=id:1 match=hybrid score=0.5 path=/one excerpt=\"alpha beta\"\n- selector=id:2 match=lexical score=0.25 path=/two excerpt=\"\"\n- selector=id:3 match=semantic score=0.125 path=/three excerpt=\"\"\n", out.String())
}

func TestSearchAgentSafeValues(t *testing.T) {
	assert.Equal(t, "\"a\\\"b\\\\c\"", searchAgentValue("a\"b\\c"))
	assert.Equal(t, "red", searchAgentValue("\x1b[31mred\x1b[0m"))
	assert.Equal(t, "\"line one line two\"", searchAgentValue("line one\r\nline two"))
	assert.Equal(t, strings.Repeat("界", 160), searchAgentExcerpt(strings.Repeat("界", 161)))
	assert.Equal(t, "alpha beta", searchAgentExcerpt("alpha\n\t beta"))
	assert.Equal(t, "/"+strings.Repeat("x", 200), searchAgentValue("/"+strings.Repeat("x", 200)))
}

func TestSearchAgentRejectsJSONBeforeDaemon(t *testing.T) {
	t.Setenv("DOCBANK_HOME", t.TempDir())
	t.Cleanup(func() { resetFlags(rootCmd) })
	_, err := runCLI(t, "search", "query", "--agent", "--json")
	require.ErrorContains(t, err, "--agent and --json are mutually exclusive")
}

type searchFailingWriter struct{}

func (searchFailingWriter) Write([]byte) (int, error) { return 0, errors.New("output closed") }
func TestSearchAgentWriteFailure(t *testing.T) {
	cmd, _ := processingTestCommand()
	cmd.SetOut(searchFailingWriter{})
	require.ErrorContains(t, writeLexicalSearchAgent(cmd, "query", api.SearchReport{Limit: 50}, ""), "writing search agent header: output closed")
	require.ErrorContains(t, writeDocumentSearchAgent(cmd, "query", searchAgentDocumentReport()), "writing search agent header: output closed")
}

func FuzzSearchAgentValueLine(f *testing.F) {
	for _, seed := range []string{"", "hello", "a\r\nb", "\x1b[31mred\x1b[0m", "quotes\"\\", "界\u2028other", string([]byte{0xff, 0, 0x1b})} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, value string) {
		rendered := searchAgentValue(value)
		for _, r := range rendered {
			require.False(t, unicode.IsControl(r) || r == '\u2028' || r == '\u2029', "unsafe output %q", rendered)
		}
		if strings.HasPrefix(rendered, "\"") {
			decoded, err := strconv.Unquote(rendered)
			require.NoError(t, err)
			require.Equal(t, searchAgentText(value), decoded)
		} else {
			require.Equal(t, searchAgentText(value), rendered)
		}
	})
}

func FuzzSearchAgentExcerptBound(f *testing.F) {
	f.Add(strings.Repeat("界", 161))
	f.Add("a\n\tb")
	f.Fuzz(func(t *testing.T, value string) {
		excerpt := searchAgentExcerpt(value)
		require.LessOrEqual(t, utf8.RuneCountInString(excerpt), 160)
		require.Equal(t, strings.Join(strings.Fields(excerpt), " "), excerpt)
	})
}

func TestSearchAgentSimilarUnavailable(t *testing.T) {
	cmd, out := processingTestCommand()
	report := api.DocumentSimilarReport{State: "unavailable", Source: api.DocumentSimilarSource{NodeID: 42, ContentVersionID: processingTestVersionID}, BindingID: "semantic",
		MissingCoverage: &api.DocumentMissingCoverage{Kind: "embedding", BindingID: "semantic", ProfileFingerprint: strings.Repeat("a", 64), ContentVersionID: processingTestVersionID}, Coverage: api.DocumentSearchCoverage{State: "unknown"}}
	require.NoError(t, writeSimilarSearchAgent(cmd, report))
	assert.Equal(t, "OK search count=0 query=id:42 mode=semantic degraded=embedding_unavailable truncated=false coverage=unknown complete=0 scoped=0\n", out.String())
}

func FuzzSearchAgentReportLines(f *testing.F) {
	f.Add("query\nnext", "/path\x1b[0m", "excerpt\r\nnext", byte(3))
	f.Fuzz(func(t *testing.T, query, path, excerpt string, count byte) {
		cmd, out := processingTestCommand()
		var results []api.DocumentSearchResult
		for i := range int(count) % 4 {
			results = append(results, api.DocumentSearchResult{NodeID: int64(i + 1), LexicalRank: i + 1, Path: path, Excerpt: excerpt, Score: 0.25})
		}
		report := searchAgentDocumentReport(results...)
		require.NoError(t, writeDocumentSearchAgent(cmd, query, report))
		require.Equal(t, len(report.Results)+1, strings.Count(out.String(), "\n"))
		require.Contains(t, out.String(), "count="+strconv.Itoa(len(report.Results)))
	})
}

// A failed hit write must stop the page before any later hit is attempted.
type searchHitFailingWriter struct{ calls int }

func (w *searchHitFailingWriter) Write(p []byte) (int, error) {
	w.calls++
	if w.calls == 1 {
		return len(p), nil
	}
	return 0, errors.New("output closed")
}
func TestSearchAgentHitWriteFailure(t *testing.T) {
	cmd, _ := processingTestCommand()
	writer := &searchHitFailingWriter{}
	cmd.SetOut(writer)
	report := api.SearchReport{Limit: 50, Hits: []api.SearchHit{{Node: api.Node{ID: 1, Kind: "file", Name: "one"}, Path: "/one", Match: "name"}, {Node: api.Node{ID: 2, Kind: "file", Name: "two"}, Path: "/two", Match: "name"}}}
	require.ErrorContains(t, writeLexicalSearchAgent(cmd, "q", report, ""), "writing search agent hit: output closed")
	assert.Equal(t, 2, writer.calls)
}

func TestSearchAgentProcessingDiagnostics(t *testing.T) {
	cmd, out := processingTestCommand()
	report := searchAgentDocumentReport(api.DocumentSearchResult{NodeID: 1, Path: "/one", LexicalRank: 1}, api.DocumentSearchResult{NodeID: 2, Path: "/two", LexicalRank: 2})
	report.Degradations = []string{"reranking_degraded"}
	report.Trace = []api.DocumentSearchTrace{{Code: "lexical_candidates", Count: 2}, {Code: "semantic_candidates", Count: 0}, {Code: "fused_candidates", Count: 2}}
	report.Reranking = &api.DocumentSearchRerankingReceipt{Outcome: "degraded", CandidateCount: 2, Cause: "unavailable"}
	require.NoError(t, writeDocumentSearchAgent(cmd, "q", report))
	assert.Contains(t, out.String(), "mode=hybrid degraded=reranking_degraded")
	assert.Contains(t, out.String(), "trace=lexical_candidates:2,semantic_candidates:0,fused_candidates:2 rerank=degraded rerank_candidates=2 rerank_cause=unavailable")
	assert.Equal(t, 3, strings.Count(out.String(), "\n"))
}

// Keep report identities, evidence, and coverage valid while individual tests
// vary the rendered fields. The report fuzz target deliberately varies text.
func searchAgentDocumentReport(results ...api.DocumentSearchResult) api.DocumentSearchReport {
	for i := range results {
		hit := &results[i]
		hit.VaultUID = "22222222-2222-4222-8222-222222222222"
		hit.ContentVersionID = fmt.Sprintf("33333333-3333-4333-8333-%012d", i+1)
		hit.Rank = i + 1
		if hit.LexicalRank > 0 {
			hit.Evidence = append(hit.Evidence, api.DocumentEvidenceReference{Kind: "node_name"})
		}
		if hit.SemanticRank > 0 {
			hash := strings.Repeat("a", 64)
			hit.Evidence = append(hit.Evidence, api.DocumentEvidenceReference{Kind: "embedding", VectorSpaceID: hash,
				EmbeddingSetID: hash, InputGenerationID: hash, InputID: hit.ContentVersionID, InputKind: "original_file", SourceManifestChecksum: hash})
		}
	}
	return api.DocumentSearchReport{RequestedMode: "hybrid", ActualMode: "hybrid", Results: results,
		Coverage: api.DocumentSearchCoverage{State: "complete", BindingRequired: true, ScopedDocuments: max(1, len(results)), CompleteDocuments: max(1, len(results))}}
}
