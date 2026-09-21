package tui

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
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
	model.naturalResultMode = naturalHybrid
	model.naturalProfiles = []api.ProcessingProfileSummary{{
		Name: "private", RerankingAvailable: true, EmbeddingBindings: []string{"semantic"},
	}}
	model.naturalSearchNote = "Search note: provider temporarily unavailable"
	model.loading = false
	model.rows = []row{{
		node: readme, path: "/README.txt", rank: 0,
		excerpt: "Solar maintenance schedule", evidence: []string{"Text", "Semantic"}, naturalMode: naturalHybrid,
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

func TestNaturalSearchWhyStripsTerminalControls(t *testing.T) {
	fake := newFakeBackend()
	model, err := New(t.Context(), fake)
	require.NoError(t, err)
	model.width, model.height = 100, 9
	model.styles = newStyles(false)
	model.mode = modeSearch
	model.rows = []row{{
		node: fake.nodes["/README.txt"], path: "/README.txt", naturalMode: naturalLexical,
		excerpt:  "safe\x1b]8;;https://evil.example\x1b\\visible\x1b]8;;\x1b\\\x1b[2J",
		evidence: []string{"Text"},
	}}

	rendered := ansi.Strip(model.renderList(100, 6))
	assert.Contains(t, rendered, "visible")
	assert.NotContains(t, rendered, "evil.example")
	assert.NotContains(t, rendered, "\x1b")
}

func TestNaturalSearchWhyLinesScrollWithRows(t *testing.T) {
	fake := newFakeBackend()
	model, err := New(t.Context(), fake)
	require.NoError(t, err)
	model.width, model.height = 100, 9
	model.styles = newStyles(false)
	model.mode = modeSearch
	model.naturalMode = naturalHybrid
	model.rows = []row{
		{node: fake.nodes["/README.txt"], path: "/one.txt", naturalMode: naturalLexical, excerpt: "one"},
		{node: fake.nodes["/README.txt"], path: "/two.txt", naturalMode: naturalLexical, excerpt: "two"},
		{node: fake.nodes["/README.txt"], path: "/three.txt", naturalMode: naturalLexical, excerpt: "three"},
	}
	model.cursor = 2
	model.clampSelection()

	assert.Equal(t, 1, model.offset)
	rendered := ansi.Strip(model.renderList(100, 6))
	assert.NotContains(t, rendered, "/one.txt")
	assert.Contains(t, rendered, "/two.txt")
	assert.Contains(t, rendered, "/three.txt")
}

func TestNaturalSearchFallbackKeepsCause(t *testing.T) {
	fake := newFakeBackend()
	readme := fake.nodes["/README.txt"]
	model, err := New(t.Context(), fake)
	require.NoError(t, err)
	model.requestID = 4
	model.naturalSearchID = 2
	model.naturalMode = naturalHybrid
	model.naturalProfiles = fake.profiles
	model.naturalSearchRequest = api.DocumentSearchRequest{Query: "old", Mode: naturalHybrid}

	updated, fallback := model.applyNaturalSearchBase(naturalSearchBaseLoadedMsg{
		requestID: model.requestID, searchID: model.naturalSearchID, query: "old",
		err: errors.New("provider timed out"),
	})
	result, ok := updated.(Model)
	require.True(t, ok)
	assert.Contains(t, result.naturalSearchNote, "provider timed out")
	assert.Equal(t, naturalNames, result.naturalMode)
	assert.Empty(t, result.naturalSearchRequest.Query)
	require.NotNil(t, fallback)

	loaded, _ := result.applySearch(searchLoadedMsg{
		requestID: result.requestID, query: "old", report: api.SearchReport{Hits: []api.SearchHit{{Node: readme, Path: readme.Path}}},
	})
	loadedModel, ok := loaded.(Model)
	require.True(t, ok)
	assert.Empty(t, loadedModel.rows[0].naturalMode)
}

func TestNaturalSearchModeProvenanceSurvivesModeChange(t *testing.T) {
	fake := newFakeBackend()
	readme := fake.nodes["/README.txt"]
	model, err := New(t.Context(), fake)
	require.NoError(t, err)
	model.width, model.height = 100, 9
	model.styles = newStyles(false)
	model.mode = modeSearch
	model.naturalMode = naturalHybrid
	model.rows = []row{{node: readme, path: readme.Path, naturalMode: naturalLexical, excerpt: "lexical result"}}
	model.naturalMode = naturalSemantic
	model.naturalResultMode = naturalHybrid
	model.naturalProfiles = []api.ProcessingProfileSummary{{Name: "private", RerankingAvailable: true}}
	model.loading = false

	assert.Contains(t, ansi.Strip(model.renderList(100, 6)), "Why: lexical result")
	location := ansi.Strip(model.renderLocation())
	assert.Contains(t, location, "Hybrid")
	assert.NotContains(t, location, "Semantic")
	assert.Contains(t, location, "rerank disabled")
	model.naturalRerank = true
	assert.Contains(t, ansi.Strip(model.renderLocation()), "rerank enabled")
}

func TestNaturalSearchCtrlRUsesAcceptedResultMode(t *testing.T) {
	fake := newFakeBackend()
	readme := fake.nodes["/README.txt"]
	fake.profiles = []api.ProcessingProfileSummary{{Name: "private", RerankingAvailable: true}}
	fake.naturalRerankSearch = naturalSearchReport(readme.ID, readme.CurrentVersionID, "reranked excerpt")
	fake.naturalRerankSearch.Reranking = &api.DocumentSearchRerankingReceipt{Outcome: "applied", CandidateCount: 1}
	model, err := New(t.Context(), fake)
	require.NoError(t, err)
	model.mode = modeSearch
	model.searchQuery = "solar maintenance"
	model.naturalMode = naturalSemantic
	model.naturalResultMode = naturalLexical
	model.naturalProfiles = fake.profiles
	model.loading = false
	model.naturalSearchRequest = api.DocumentSearchRequest{
		Query: "solar maintenance", Mode: naturalLexical,
		Fence: api.DocumentSourceFence{ContentVersionIDs: []string{"version"}},
	}

	updated, rerank := model.updateKeys(tea.KeyPressMsg{Code: 'r', Mod: tea.ModCtrl})
	result, ok := updated.(Model)
	require.True(t, ok)
	require.NotNil(t, rerank)

	message, ok := rerank().(naturalSearchRerankLoadedMsg)
	require.True(t, ok)
	assert.Equal(t, naturalLexical, message.naturalMode)
	assert.Equal(t, naturalLexical, fake.naturalSearchRequests[0].Mode)

	settled, _ := result.applyNaturalSearchRerank(message)
	settledModel, ok := settled.(Model)
	require.True(t, ok)
	assert.Equal(t, naturalLexical, settledModel.naturalResultMode)
}

func TestNaturalSearchQueryInputShowsRerankStatus(t *testing.T) {
	fake := newFakeBackend()
	model, err := New(t.Context(), fake)
	require.NoError(t, err)
	model.width, model.height = 100, 10
	model.styles = newStyles(false)
	model.mode = modeSearch
	model.searching = true
	model.naturalMode = naturalSemantic
	model.naturalProfiles = []api.ProcessingProfileSummary{{Name: "private", RerankingAvailable: true}}
	model.searchInput.SetValue("solar maintenance")

	model.naturalRerank = true
	assert.Contains(t, ansi.Strip(model.render()), "Semantic · rerank enabled")
	model.naturalRerank = false
	assert.Contains(t, ansi.Strip(model.render()), "Semantic · rerank disabled")
}

func TestNaturalSearchQueryInputKeepsLongCursorViewportVisible(t *testing.T) {
	fake := newFakeBackend()
	model, err := New(t.Context(), fake)
	require.NoError(t, err)
	updated, _ := model.Update(tea.WindowSizeMsg{Width: 80, Height: 10})
	model, ok := updated.(Model)
	require.True(t, ok)
	model.styles = newStyles(false)
	model.mode = modeSearch
	model.searching = true
	model.naturalMode = naturalSemantic
	model.naturalProfiles = []api.ProcessingProfileSummary{{Name: "private", RerankingAvailable: true}}
	query := strings.Repeat("q", 48) + "tail-visible"
	model.searchInput.SetValue(query)
	model.searchInput.Focus()

	rendered := ansi.Strip(model.render())
	assert.Equal(t, len(query), model.searchInput.Position())
	assert.Contains(t, rendered, "tail-visible")
}

func TestNaturalSearchCtrlRDoesNotRerankDuringRefresh(t *testing.T) {
	fake := newFakeBackend()
	model, err := New(t.Context(), fake)
	require.NoError(t, err)
	model.mode = modeSearch
	model.searchQuery = "solar maintenance"
	model.naturalMode = naturalLexical
	model.naturalProfiles = fake.profiles
	model.naturalSearchRequest = api.DocumentSearchRequest{
		Query: "old", Mode: naturalLexical,
		Fence: api.DocumentSourceFence{ContentVersionIDs: []string{"old-version"}},
	}

	updated, refresh := model.updateKeys(tea.KeyPressMsg{Code: 'r'})
	refreshed, ok := updated.(Model)
	require.True(t, ok)
	require.NotNil(t, refresh)
	assert.True(t, refreshed.loading)
	assert.Empty(t, refreshed.naturalSearchRequest.Query)

	updated, rerank := refreshed.updateKeys(tea.KeyPressMsg{Code: 'r', Mod: tea.ModCtrl})
	result, ok := updated.(Model)
	require.True(t, ok)
	assert.Nil(t, rerank)
	assert.False(t, result.naturalRerank)
	assert.True(t, result.loading)
}

func TestNaturalSearchEmptyFenceDoesNotScheduleRerank(t *testing.T) {
	fake := newFakeBackend()
	fake.sourceFence.Fence.ContentVersionIDs = nil
	model, err := New(t.Context(), fake)
	require.NoError(t, err)
	model.requestID = 4
	model.naturalSearchID = 2
	model.naturalProfiles = fake.profiles
	model.naturalMode = naturalLexical
	model.naturalRerank = true

	message := model.loadNaturalSearch("nothing", model.requestID)()
	baseMessage, ok := message.(naturalSearchBaseLoadedMsg)
	require.True(t, ok)
	updated, rerank := model.applyNaturalSearchBase(baseMessage)
	result, ok := updated.(Model)
	require.True(t, ok)
	assert.Nil(t, rerank)
	assert.Empty(t, result.naturalSearchRequest.Query)
	assert.False(t, result.naturalRerankPending)
	assert.Empty(t, fake.naturalSearchRequests)
	assert.Equal(t, naturalLexical, result.naturalResultMode)
}

func TestNaturalSearchRerankNotesPreserveReceiptOutcomeAndCause(t *testing.T) {
	fake := newFakeBackend()
	model, err := New(t.Context(), fake)
	require.NoError(t, err)
	model.requestID = 5
	model.naturalSearchID = 3
	model.naturalRerank = true
	model.naturalRerankPending = true

	updated, _ := model.applyNaturalSearchRerank(naturalSearchRerankLoadedMsg{
		requestID: model.requestID, searchID: model.naturalSearchID,
		err: errors.New("authorization_denied: provider consent was not granted"),
	})
	result, ok := updated.(Model)
	require.True(t, ok)
	assert.Contains(t, result.naturalSearchNote, "authorization_denied")
	assert.False(t, result.naturalRerankPending)

	for _, test := range []struct {
		outcome string
		cause   string
		want    string
	}{
		{outcome: "skipped", cause: "no candidates", want: "Reranking skipped (no candidates)"},
		{outcome: "degraded", cause: "timed_out", want: "Reranking degraded (timed_out)"},
	} {
		t.Run(test.outcome, func(t *testing.T) {
			updated, _ := result.applyNaturalSearchRerank(naturalSearchRerankLoadedMsg{
				requestID: result.requestID, searchID: result.naturalSearchID,
				report: api.DocumentSearchReport{Reranking: &api.DocumentSearchRerankingReceipt{
					Outcome: test.outcome, Cause: test.cause,
				}},
			})
			next, ok := updated.(Model)
			require.True(t, ok)
			assert.Contains(t, next.naturalSearchNote, test.want)
		})
	}
}

func TestNaturalSearchSubmitClearsRerankRequestBeforeNewSequence(t *testing.T) {
	fake := newFakeBackend()
	model, err := New(t.Context(), fake)
	require.NoError(t, err)
	model.mode = modeSearch
	model.naturalMode = naturalNames
	model.naturalSearchRequest = api.DocumentSearchRequest{Query: "old", Mode: naturalHybrid}
	model.searchInput.SetValue("new")

	updated, cmd := model.updateSearchInput(tea.KeyPressMsg{Code: tea.KeyEnter})
	result, ok := updated.(Model)
	require.True(t, ok)
	require.NotNil(t, cmd)
	assert.Empty(t, result.naturalSearchRequest.Query)

	result.naturalMode = naturalLexical
	result.mode = modeSearch
	result.naturalRerank = false
	_, rerank := result.updateKeys(tea.KeyPressMsg{Code: 'r', Mod: tea.ModCtrl})
	assert.Nil(t, rerank)
}
