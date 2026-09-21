package tui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/internal/api"
)

func naturalSearchReport(nodeID int64, versionID, excerpt string) api.DocumentSearchReport {
	return api.DocumentSearchReport{
		RequestedMode: "lexical", ActualMode: "lexical",
		Coverage: api.DocumentSearchCoverage{ScopedDocuments: 2, CompleteDocuments: 2, State: "complete"},
		Results: []api.DocumentSearchResult{{
			VaultUID: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", NodeID: nodeID,
			ContentVersionID: versionID, Rank: 1, Path: "/README.txt", Excerpt: excerpt,
			Evidence: []api.DocumentEvidenceReference{{Kind: "content_blob"}},
		}},
	}
}

func TestNaturalSearchReproductionStaleAndRerankState(t *testing.T) {
	fake := newFakeBackend()
	readme := fake.nodes["/README.txt"]
	fake.profiles = []api.ProcessingProfileSummary{{
		Name: "private", Fingerprint: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		RerankingAvailable: true,
	}}
	fake.naturalSearch = naturalSearchReport(readme.ID, readme.CurrentVersionID, "base excerpt")
	fake.naturalRerankSearch = naturalSearchReport(readme.ID, readme.CurrentVersionID, "reranked excerpt")
	fake.naturalRerankSearch.Reranking = &api.DocumentSearchRerankingReceipt{Outcome: "applied", CandidateCount: 1}

	model, err := New(t.Context(), fake)
	require.NoError(t, err)
	model.requestID = 7
	model.naturalSearchID = 3
	model.naturalProfiles = fake.profiles
	model.naturalMode = naturalLexical
	model.naturalRerank = true

	message := model.loadNaturalSearch("solar maintenance", model.requestID)()
	baseMessage, ok := message.(naturalSearchBaseLoadedMsg)
	require.True(t, ok)
	base, rerank := model.applyNaturalSearchBase(baseMessage)
	baseModel, ok := base.(Model)
	require.True(t, ok)
	require.NotNil(t, rerank)
	require.Len(t, fake.naturalSearchRequests, 1)
	assert.Equal(t, "lexical", fake.naturalSearchRequests[0].Mode)
	assert.Empty(t, fake.naturalSearchRequests[0].BindingID)
	assert.False(t, fake.naturalSearchRequests[0].Rerank)
	assert.Equal(t, "base excerpt", baseModel.rows[0].excerpt)
	assert.True(t, baseModel.naturalRerankPending)

	reranked := rerank()
	rerankedMessage, ok := reranked.(naturalSearchRerankLoadedMsg)
	require.True(t, ok)
	settled, _ := baseModel.applyNaturalSearchRerank(rerankedMessage)
	settledModel, ok := settled.(Model)
	require.True(t, ok)
	assert.Equal(t, "reranked excerpt", settledModel.rows[0].excerpt)
	assert.False(t, settledModel.naturalRerankPending)

	before := settledModel.rows[0].excerpt
	stale, _ := settledModel.applyNaturalSearchBase(naturalSearchBaseLoadedMsg{
		requestID: settledModel.requestID, searchID: settledModel.naturalSearchID - 1,
		query: "old", rows: []row{{excerpt: "stale"}},
	})
	staleModel, ok := stale.(Model)
	require.True(t, ok)
	assert.Equal(t, before, staleModel.rows[0].excerpt)
}

func TestNaturalSearchGoldenFrame(t *testing.T) {
	fake := newFakeBackend()
	readme := fake.nodes["/README.txt"]
	model, err := New(t.Context(), fake)
	require.NoError(t, err)
	model.width, model.height = 112, 18
	model.styles = newStyles(false)
	model.mode = modeSearch
	model.searchQuery = "solar maintenance"
	model.naturalMode = naturalHybrid
	model.naturalProfiles = []api.ProcessingProfileSummary{{
		Name: "private", RerankingAvailable: true, EmbeddingBindings: []string{"semantic"},
	}}
	model.naturalSearchNote = "Search note: provider temporarily unavailable"
	model.loading = false
	model.rows = []row{{
		node: readme, path: "/README.txt", rank: 0,
		excerpt: "Solar maintenance schedule", evidence: []string{"Text", "Semantic"},
	}}
	model.total = 1
	model.cursor = 0

	actual := strings.TrimRight(strings.ReplaceAll(ansi.Strip(model.render()), "\r\n", "\n"), "\n")
	actual = strings.Join(strings.Split(actual, "\n"), "\n")
	lines := strings.Split(actual, "\n")
	for index := range lines {
		lines[index] = strings.TrimRight(lines[index], " ")
	}
	actual = strings.Join(lines, "\n") + "\n"
	goldenPath := filepath.Join("testdata", "natural-search.golden")
	if os.Getenv("UPDATE_GOLDEN") == "1" {
		require.NoError(t, os.WriteFile(goldenPath, []byte(actual), 0o644))
		return
	}
	want, err := os.ReadFile(goldenPath)
	require.NoError(t, err)
	assert.Equal(t, string(want), actual)
}

func TestNaturalSearchLexicalProfileKeepsRerankCapability(t *testing.T) {
	profile := &api.ProcessingProfileSummary{Name: "private", RerankingAvailable: true}
	mode, binding, ok := naturalRequestMode(naturalLexical, profile)
	assert.True(t, ok)
	assert.Equal(t, naturalLexical, mode)
	assert.Empty(t, binding)
	model := Model{naturalProfiles: []api.ProcessingProfileSummary{*profile}, naturalMode: naturalLexical}
	model.toggleNaturalRerank()
	assert.True(t, model.naturalRerank)
}
