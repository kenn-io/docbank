package tui

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/doordash-oss/oapi-codegen-dd/v3/pkg/runtime"
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
	report := fake.nodes["/docs/report.txt"]
	fake.naturalRerankSearch.Results = append(fake.naturalRerankSearch.Results,
		naturalSearchReport(report.ID, report.CurrentVersionID, "another result").Results...)
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
	assert.Equal(t, 100, fake.naturalSearchRequests[0].Limit)
	assert.Empty(t, fake.naturalSearchRequests[0].BindingID)
	assert.False(t, fake.naturalSearchRequests[0].Rerank)
	assert.Equal(t, "base excerpt", baseModel.rows[0].excerpt)
	assert.True(t, baseModel.naturalRerankPending)

	rename := readme
	rename.Path = "/renamed.txt"
	delete(fake.nodes, "/README.txt")
	fake.nodes[rename.Path] = rename
	reranked := rerank()
	rerankedMessage, ok := reranked.(naturalSearchRerankLoadedMsg)
	require.True(t, ok)
	settled, _ := baseModel.applyNaturalSearchRerank(rerankedMessage)
	settledModel, ok := settled.(Model)
	require.True(t, ok)
	assert.Equal(t, "reranked excerpt", settledModel.rows[0].excerpt)
	assert.Equal(t, "/renamed.txt", settledModel.rows[0].path)
	assert.False(t, settledModel.naturalRerankPending)
	assert.Equal(t, []int64{readme.ID, readme.ID, report.ID}, fake.nodeIDs, "reranking refreshes hydrated nodes")

	before := settledModel.rows[0].excerpt
	stale, _ := settledModel.applyNaturalSearchBase(naturalSearchBaseLoadedMsg{
		requestID: settledModel.requestID, searchID: settledModel.naturalSearchID - 1,
		query: "old", rows: []row{{excerpt: "stale"}},
	})
	staleModel, ok := stale.(Model)
	require.True(t, ok)
	assert.Equal(t, before, staleModel.rows[0].excerpt)

	readme.CurrentVersionID = report.CurrentVersionID
	fake.nodes["/README.txt"] = readme
	refreshed, ok := settledModel.loadNaturalSearch("new query", model.requestID)().(naturalSearchBaseLoadedMsg)
	require.True(t, ok)
	require.NoError(t, refreshed.err)
	assert.Empty(t, refreshed.rows, "a new request fetches current versions instead of reusing earlier nodes")
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
	wantText := strings.ReplaceAll(string(want), "\r\n", "\n")
	assert.Equal(t, wantText, actual)
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

	updated, fallback := model.applyNaturalSearchBase(naturalSearchBaseLoadedMsg{
		requestID: model.requestID, searchID: model.naturalSearchID, query: "old",
		err: errors.New("provider timed out"),
	})
	result, ok := updated.(Model)
	require.True(t, ok)
	assert.Contains(t, result.naturalSearchNote, "provider timed out")
	assert.Equal(t, naturalNames, result.naturalMode)
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

func TestNaturalSearchSettingsRestartActiveQuery(t *testing.T) {
	for _, test := range []struct {
		name       string
		key        tea.KeyPressMsg
		focused    bool
		mode       string
		rerank     bool
		wantMode   string
		wantRerank bool
	}{
		{name: "mode from results", key: key(tea.KeyTab), mode: naturalAuto, rerank: true, wantMode: naturalLexical, wantRerank: true},
		{name: "mode from input", key: key(tea.KeyTab), focused: true, mode: naturalHybrid, rerank: true, wantMode: naturalNames},
		{name: "enable rerank", key: tea.KeyPressMsg{Code: 'r', Mod: tea.ModCtrl}, mode: naturalLexical, wantMode: naturalLexical, wantRerank: true},
		{name: "disable rerank", key: tea.KeyPressMsg{Code: 'r', Mod: tea.ModCtrl}, focused: true, mode: naturalLexical, rerank: true, wantMode: naturalLexical},
	} {
		t.Run(test.name, func(t *testing.T) {
			fake := newFakeBackend()
			readme := fake.nodes["/README.txt"]
			fake.profiles[0].RerankingAvailable = true
			fake.naturalSearch = naturalSearchReport(readme.ID, readme.CurrentVersionID, "fresh base")
			model, err := New(t.Context(), fake)
			require.NoError(t, err)
			model.mode, model.searchQuery = modeSearch, "maintenance"
			model.searching = test.focused
			model.naturalProfiles = fake.profiles
			model.naturalMode, model.naturalRerank = test.mode, test.rerank
			model.loading = false
			model.spinnerActive = false
			model.rows = []row{{node: fake.nodes["/docs/report.txt"], excerpt: "old ranking"}}
			oldRequestID, oldSearchID := model.requestID, model.naturalSearchID

			model, cmd := updateModel(t, model, test.key)
			require.NotNil(t, cmd)
			assert.Equal(t, test.wantMode, model.naturalMode)
			assert.Equal(t, test.wantRerank, model.naturalRerank)
			assert.Greater(t, model.requestID, oldRequestID)
			assert.Greater(t, model.naturalSearchID, oldSearchID)
			assert.True(t, model.loading)
			assert.True(t, model.spinnerActive)
			for _, stale := range []tea.Msg{
				searchLoadedMsg{requestID: oldRequestID, query: "stale"},
				naturalSearchBaseLoadedMsg{requestID: oldRequestID, searchID: oldSearchID, query: "stale"},
				naturalSearchRerankLoadedMsg{requestID: oldRequestID, searchID: oldSearchID,
					report: api.DocumentSearchReport{Reranking: &api.DocumentSearchRerankingReceipt{Outcome: "applied"}}},
			} {
				model, _ = updateModel(t, model, stale)
				require.Len(t, model.rows, 1)
				assert.Equal(t, "old ranking", model.rows[0].excerpt)
				assert.True(t, model.loading)
			}
			model = runModelCommand(t, model, cmd)
			assert.False(t, model.loading)
			assert.Equal(t, "maintenance", model.searchQuery)
			assert.Equal(t, test.wantRerank, model.naturalRerankPending)
			if test.wantMode != naturalNames {
				require.Len(t, fake.naturalSearchRequests, 1)
				assert.Equal(t, test.wantMode, fake.naturalSearchRequests[0].Mode)
				assert.False(t, fake.naturalSearchRequests[0].Rerank)
				assert.Equal(t, "fresh base", model.rows[0].excerpt)
			}
		})
	}
}

func TestNaturalSearchSkipsUnavailableRows(t *testing.T) {
	for _, status := range []int{http.StatusNotFound, http.StatusInternalServerError, http.StatusUnauthorized, http.StatusForbidden} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			fake := newFakeBackend()
			readme, report := fake.nodes["/README.txt"], fake.nodes["/docs/report.txt"]
			fake.naturalSearch = naturalSearchReport(readme.ID, readme.CurrentVersionID, "unavailable")
			fake.naturalSearch.Results = append(fake.naturalSearch.Results,
				naturalSearchReport(report.ID, report.CurrentVersionID, "available").Results...)
			fake.nodeErrors = map[int64]error{readme.ID: runtime.NewClientAPIError(errors.New("node unavailable"), runtime.WithStatusCode(status))}
			model, err := New(t.Context(), fake)
			require.NoError(t, err)
			model.naturalProfiles = fake.profiles
			model.naturalMode = naturalLexical
			message, ok := model.loadNaturalSearch("maintenance", model.requestID)().(naturalSearchBaseLoadedMsg)
			require.True(t, ok)
			if status == http.StatusUnauthorized || status == http.StatusForbidden {
				require.Error(t, message.err)
				return
			}
			require.NoError(t, message.err)
			assert.Equal(t, []int64{report.ID}, rowIDs(message.rows))
		})
	}
}

func TestBrowseViewportFillsAfterResize(t *testing.T) {
	model, err := New(t.Context(), newFakeBackend())
	require.NoError(t, err)
	model.rows = make([]row, 12)
	model.cursor, model.offset = 11, 5
	model, _ = updateModel(t, model, tea.WindowSizeMsg{Width: 100, Height: 14})
	assert.Equal(t, 9, model.visibleRows())
	assert.Equal(t, 3, model.offset)
}

func TestNaturalSearchTruncationShowsDisplayedCount(t *testing.T) {
	model, err := New(t.Context(), newFakeBackend())
	require.NoError(t, err)
	model.width, model.height = 180, 14
	model.mode = modeSearch
	model.naturalResultMode = naturalLexical
	model.rows = make([]row, 100)
	model.truncated = true
	model.loading = false
	assert.Contains(t, ansi.Strip(model.renderLocation()), "first 100 result(s)")
}

func TestNaturalSearchProfilesFailureIsVisible(t *testing.T) {
	model, err := New(t.Context(), newFakeBackend())
	require.NoError(t, err)
	model.width, model.height = 160, 14
	model, _ = updateModel(t, model, naturalProfilesLoadedMsg{
		requestID: model.naturalProfilesRequest, err: errors.New("profiles unavailable"),
	})
	assert.Contains(t, ansi.Strip(model.render()), "Natural-language search unavailable: profiles unavailable")
}

func TestNaturalSearchFooterMatchesCapabilities(t *testing.T) {
	model, err := New(t.Context(), newFakeBackend())
	require.NoError(t, err)
	model.width, model.height = 180, 14
	model.searching = true
	assert.NotContains(t, ansi.Strip(model.renderFooter()), "tab mode")
	assert.NotContains(t, ansi.Strip(model.renderFooter()), "ctrl+r rerank")
	model.naturalProfiles = []api.ProcessingProfileSummary{{Name: "local", RerankingAvailable: true}}
	assert.Contains(t, ansi.Strip(model.renderFooter()), "tab mode")
	assert.NotContains(t, ansi.Strip(model.renderFooter()), "ctrl+r rerank")
	model.naturalMode = naturalLexical
	assert.Contains(t, ansi.Strip(model.renderFooter()), "ctrl+r rerank")
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
	model, _ = updateModel(t, model, key(tea.KeyLeft))
	model, _ = updateModel(t, model, runeKey('x'))

	rendered := ansi.Strip(model.render())
	assert.Equal(t, len(query), model.searchInput.Position())
	assert.Equal(t, strings.Repeat("q", 48)+"tail-visiblxe", model.searchInput.Value())
	assert.Contains(t, rendered, "tail-visiblxe")
}

func TestNaturalSearchProfileDefaultRestartsAcceptedQuery(t *testing.T) {
	fake := newFakeBackend()
	model, err := New(t.Context(), fake)
	require.NoError(t, err)
	model.mode = modeSearch
	model.searchQuery = "accepted query"
	model.naturalMode = naturalNames
	model.loading = false

	updated, cmd := model.Update(naturalProfilesLoadedMsg{
		requestID: model.naturalProfilesRequest,
		profiles:  []api.ProcessingProfileSummary{{Name: "private", EmbeddingBindings: []string{"embed"}}},
	})
	result, ok := updated.(Model)
	require.True(t, ok)
	assert.Equal(t, naturalAuto, result.naturalMode)
	require.NotNil(t, cmd)

	runModelCommand(t, result, cmd)
	require.Len(t, fake.naturalSearchRequests, 1)
	assert.Equal(t, "accepted query", fake.naturalSearchRequests[0].Query)
	assert.Equal(t, naturalHybrid, fake.naturalSearchRequests[0].Mode)
}

func TestNaturalSearchProfileDefaultWaitsForAcceptedQuery(t *testing.T) {
	fake := newFakeBackend()
	model, err := New(t.Context(), fake)
	require.NoError(t, err)
	model.mode = modeSearch
	model.searchQuery = "old query"
	model.submittedSearchQuery = "new query"
	model.submittedSearchID = model.requestID
	model.naturalMode = naturalNames
	model.loading = true

	updated, cmd := model.Update(naturalProfilesLoadedMsg{
		requestID: model.naturalProfilesRequest,
		profiles:  []api.ProcessingProfileSummary{{Name: "private", EmbeddingBindings: []string{"embed"}}},
	})
	result, ok := updated.(Model)
	require.True(t, ok)
	assert.Nil(t, cmd)
	assert.Equal(t, naturalNames, result.naturalMode)
	assert.True(t, result.naturalProfileDefaultPending)

	updated, cmd = result.applySearch(searchLoadedMsg{
		requestID: result.requestID,
		query:     "new query",
		report:    api.SearchReport{},
	})
	result, ok = updated.(Model)
	require.True(t, ok)
	assert.Equal(t, naturalAuto, result.naturalMode)
	assert.Equal(t, "new query", result.searchQuery)
	require.NotNil(t, cmd)

	runModelCommand(t, result, cmd)
	require.Len(t, fake.naturalSearchRequests, 1)
	assert.Equal(t, "new query", fake.naturalSearchRequests[0].Query)
	assert.Equal(t, naturalHybrid, fake.naturalSearchRequests[0].Mode)
}

func TestNaturalSearchProfileDefaultDoesNotReopenBrowseAfterNavigation(t *testing.T) {
	fake := newFakeBackend()
	model, err := New(t.Context(), fake)
	require.NoError(t, err)
	model.mode = modeSearch
	model.searchQuery = "old query"
	model.naturalMode = naturalNames

	updated, cmd := model.applyDirectory(directoryLoadedMsg{
		requestID: model.requestID,
		kind:      navigationForward,
		directory: fake.nodes["/"],
		page:      fake.children[1],
	})
	result, ok := updated.(Model)
	require.True(t, ok)
	assert.Nil(t, cmd)
	assert.Equal(t, modeBrowse, result.mode)

	updated, cmd = result.Update(naturalProfilesLoadedMsg{
		requestID: result.naturalProfilesRequest,
		profiles:  []api.ProcessingProfileSummary{{Name: "private", EmbeddingBindings: []string{"embed"}}},
	})
	result, ok = updated.(Model)
	require.True(t, ok)
	assert.Nil(t, cmd)
	assert.Equal(t, naturalAuto, result.naturalMode)
	assert.Empty(t, fake.naturalSearchRequests)
}

func TestNaturalSearchRowsPreserveViewOnlyForRefresh(t *testing.T) {
	fake := newFakeBackend()
	model, err := New(t.Context(), fake)
	require.NoError(t, err)
	selected := api.Node{ID: 2, Name: "selected.txt", Kind: nodeKindFile}
	rows := []row{
		{node: api.Node{ID: 1, Name: "zulu.txt", Kind: nodeKindFile}, path: "/zulu.txt"},
		{node: selected, path: "/selected.txt"},
		{node: api.Node{ID: 3, Name: "alpha.txt", Kind: nodeKindFile}, path: "/alpha.txt"},
	}
	model.mode = modeSearch
	model.searchQuery = "same query"
	model.sortField = sortByName
	model.sortDesc = true
	model.rows = rows
	model.cursor = 1
	model.offset = 1

	model.applyNaturalRows("same query", rows, false)
	current, ok := model.selected()
	require.True(t, ok)
	assert.Equal(t, selected.ID, current.node.ID)
	assert.Equal(t, sortByName, model.sortField)
	assert.True(t, model.sortDesc)
	assert.Equal(t, 1, model.offset)

	model.applyNaturalRows("new query", rows, false)
	current, ok = model.selected()
	require.True(t, ok)
	assert.NotEqual(t, selected.ID, current.node.ID)
	assert.Equal(t, sortByRelevance, model.sortField)
	assert.False(t, model.sortDesc)
	assert.Zero(t, model.offset)
}

func TestNaturalSearchSettingsUseLatestSubmittedQuery(t *testing.T) {
	for _, acceptedQuery := range []string{"", "older query"} {
		t.Run("previous="+acceptedQuery, func(t *testing.T) {
			fake := newFakeBackend()
			fake.profiles[0].RerankingAvailable = true
			model, err := New(t.Context(), fake)
			require.NoError(t, err)
			model.naturalProfiles = fake.profiles
			if acceptedQuery != "" {
				model.mode, model.searchQuery = modeSearch, acceptedQuery
			}
			model.searching = true
			model.searchInput.SetValue("latest submitted")
			model, first := updateModel(t, model, key(tea.KeyEnter))
			require.NotNil(t, first)
			oldReply := first()
			model, changed := updateModel(t, model, key(tea.KeyTab))
			require.NotNil(t, changed)
			model = firstModel(t, model, oldReply)
			assert.Equal(t, acceptedQuery, model.searchQuery)
			pendingReply := changed()
			model, _ = updateModel(t, model, runeKey('/'))
			model.searchInput.SetValue("unsubmitted draft")
			model, rerank := updateModel(t, model, tea.KeyPressMsg{Code: 'r', Mod: tea.ModCtrl})
			require.NotNil(t, rerank)
			model = firstModel(t, model, pendingReply)
			assert.Equal(t, acceptedQuery, model.searchQuery)
			model = runModelCommand(t, model, rerank)
			assert.Equal(t, "latest submitted", model.searchQuery)
			assert.Equal(t, "unsubmitted draft", model.searchInput.Value())
			assert.True(t, model.naturalRerankPending)
		})
	}
}

func TestNaturalSearchInactiveSettingsDoNotSubmit(t *testing.T) {
	model, err := New(t.Context(), newFakeBackend())
	require.NoError(t, err)
	model.searching = true
	model.searchInput.SetValue("unsubmitted")
	model.mode, model.searchQuery = modeSearch, "accepted"
	for _, key := range []tea.KeyPressMsg{key(tea.KeyTab), {Code: 'r', Mod: tea.ModCtrl}} {
		before := model.requestID
		var cmd tea.Cmd
		model, cmd = updateModel(t, model, key)
		assert.Nil(t, cmd)
		assert.Equal(t, before, model.requestID)
	}
	model.mode = modeBrowse
	model.naturalProfiles = []api.ProcessingProfileSummary{{Name: "local", RerankingAvailable: true}}
	model, cmd := updateModel(t, model, key(tea.KeyTab))
	assert.Nil(t, cmd)
	model, cmd = updateModel(t, model, tea.KeyPressMsg{Code: 'r', Mod: tea.ModCtrl})
	assert.Nil(t, cmd)
	assert.True(t, model.naturalRerank)
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

func TestNaturalSearchRefreshRestartsPendingQuery(t *testing.T) {
	model, err := New(t.Context(), newFakeBackend())
	require.NoError(t, err)
	model.mode, model.searchQuery = modeSearch, "old"
	model.searchInput.SetValue("new")
	updated, _ := model.updateSearchInput(key(tea.KeyEnter))
	model, ok := updated.(Model)
	require.True(t, ok)
	previous := model.requestID
	model, cmd := updateModel(t, model, runeKey('r'))
	require.NotNil(t, cmd)
	assert.Greater(t, model.requestID, previous)
	model = runModelCommand(t, model, cmd)
	assert.Equal(t, "new", model.searchQuery)
}
