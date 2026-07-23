package tui

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/internal/api"
)

type fakeBackend struct {
	nodes      map[string]api.Node
	children   map[int64]api.NodePage
	search     api.SearchReport
	err        error
	childLimit int
	searchMax  int
	nodeIDs    []int64
	statPaths  []string
}

func newFakeBackend() *fakeBackend {
	root := api.Node{ID: 1, Kind: "dir", Name: "", Path: "/", Revision: 1}
	docs := api.Node{ID: 2, ParentID: new(int64(1)), Kind: "dir", Name: "docs", Path: "/docs", Revision: 1}
	readme := api.Node{
		ID: 3, ParentID: new(int64(1)), Kind: "file", Name: "README.txt", Revision: 2,
		CurrentVersionID: "11111111-1111-4111-8111-111111111111",
		BlobHash:         strings.Repeat("a", 64), Size: 12, MimeType: "text/plain",
		ModifiedAt: "2026-07-22T12:00:00Z",
	}
	report := api.Node{
		ID: 4, ParentID: new(int64(2)), Kind: "file", Name: "report.txt", Revision: 3,
		CurrentVersionID: "22222222-2222-4222-8222-222222222222",
		BlobHash:         strings.Repeat("b", 64), Size: 42, MimeType: "text/plain",
		ModifiedAt: "2026-07-22T13:00:00Z",
	}
	return &fakeBackend{
		nodes: map[string]api.Node{"/": root, "/docs": docs},
		children: map[int64]api.NodePage{
			1: {Items: []api.Node{docs, readme}, Total: 2, Limit: maxBrowserItems},
			2: {Items: []api.Node{report}, Total: 1, Limit: maxBrowserItems},
		},
		search: api.SearchReport{
			Hits:  []api.SearchHit{{Node: report, Path: "/docs/report.txt", Match: "content"}},
			Limit: maxSearchItems,
		},
	}
}

func (f *fakeBackend) Stat(_ context.Context, path string) (api.Node, error) {
	f.statPaths = append(f.statPaths, path)
	if f.err != nil {
		return api.Node{}, f.err
	}
	node, ok := f.nodes[path]
	if !ok {
		return api.Node{}, errors.New("not found")
	}
	return node, nil
}

func (f *fakeBackend) Node(_ context.Context, nodeID int64) (api.Node, error) {
	f.nodeIDs = append(f.nodeIDs, nodeID)
	if f.err != nil {
		return api.Node{}, f.err
	}
	for _, node := range f.nodes {
		if node.ID == nodeID {
			return node, nil
		}
	}
	return api.Node{}, errors.New("not found")
}

func (f *fakeBackend) ChildrenPage(
	_ context.Context, id int64, limit, _ int,
) (api.NodePage, error) {
	f.childLimit = limit
	if f.err != nil {
		return api.NodePage{}, f.err
	}
	return f.children[id], nil
}

func (f *fakeBackend) Search(
	_ context.Context, _ string, limit int,
) (api.SearchReport, error) {
	f.searchMax = limit
	if f.err != nil {
		return api.SearchReport{}, f.err
	}
	return f.search, nil
}

func TestModelNavigatesSearchesAndReturnsToTree(t *testing.T) {
	backend := newFakeBackend()
	model, err := New(t.Context(), backend)
	require.NoError(t, err)
	model = runModelCommand(t, model, model.loadDirectory(0, navigationInitial, model.requestID))
	assert.Equal(t, maxBrowserItems, backend.childLimit)
	assert.Equal(t, "/", model.directory.Path)
	require.Len(t, model.rows, 2)
	assert.Equal(t, "/docs", model.rows[0].path)

	model, cmd := updateModel(t, model, key(tea.KeyEnter))
	require.NotNil(t, cmd)
	model = runModelCommand(t, model, cmd)
	assert.Equal(t, "/docs", model.directory.Path)
	require.Len(t, model.rows, 1)
	assert.Equal(t, "/docs/report.txt", model.rows[0].path)

	model, cmd = updateModel(t, model, key(tea.KeyLeft))
	require.Nil(t, cmd)
	assert.Equal(t, "/", model.directory.Path)

	model, _ = updateModel(t, model, runeKey('/'))
	assert.True(t, model.searching)
	model.searchInput.SetValue("quarterly report")
	model, cmd = updateModel(t, model, key(tea.KeyEnter))
	require.NotNil(t, cmd)
	model = runModelCommand(t, model, cmd)
	assert.Equal(t, maxSearchItems, backend.searchMax)
	assert.Equal(t, modeSearch, model.mode)
	assert.Equal(t, "quarterly report", model.searchQuery)
	require.Len(t, model.rows, 1)
	assert.Equal(t, "content", model.rows[0].match)

	model, cmd = updateModel(t, model, key(tea.KeyEscape))
	require.Nil(t, cmd)
	assert.Equal(t, modeBrowse, model.mode)
	assert.Equal(t, "/", model.directory.Path)
	require.Len(t, model.rows, 2)
}

func TestModelPreservesViewStateAcrossNavigation(t *testing.T) {
	backend := newFakeBackend()
	model, err := New(t.Context(), backend)
	require.NoError(t, err)
	model = runModelCommand(t, model, model.loadDirectory(0, navigationInitial, model.requestID))
	model.cursor = 1

	model, _ = updateModel(t, model, runeKey('/'))
	model.searchInput.SetValue("quarterly report")
	model, cmd := updateModel(t, model, key(tea.KeyEnter))
	model = runModelCommand(t, model, cmd)
	require.Equal(t, modeSearch, model.mode)

	model, cmd = updateModel(t, model, key(tea.KeyEscape))
	require.Nil(t, cmd)
	assert.Equal(t, modeBrowse, model.mode)
	assert.Equal(t, 1, model.cursor)
	assert.Equal(t, "README.txt", model.rows[model.cursor].node.Name)
}

func TestBackIgnoresDelayedDirectoryRefresh(t *testing.T) {
	backend := newFakeBackend()
	model, err := New(t.Context(), backend)
	require.NoError(t, err)
	model = runModelCommand(t, model, model.loadDirectory(0, navigationInitial, model.requestID))

	model, cmd := updateModel(t, model, key(tea.KeyEnter))
	model = runModelCommand(t, model, cmd)
	require.Equal(t, "/docs", model.directory.Path)

	model, delayedRefresh := updateModel(t, model, runeKey('r'))
	require.NotNil(t, delayedRefresh)
	pendingRequestID := model.requestID
	model, cmd = updateModel(t, model, key(tea.KeyLeft))
	require.Nil(t, cmd)
	assert.Equal(t, "/", model.directory.Path)
	assert.Greater(t, model.requestID, pendingRequestID)

	model = runModelCommand(t, model, delayedRefresh)
	assert.Equal(t, "/", model.directory.Path)
	require.Len(t, model.rows, 2)
	assert.Equal(t, "/docs", model.rows[0].path)
}

func TestLeavingSearchIgnoresDelayedRefresh(t *testing.T) {
	backend := newFakeBackend()
	model, err := New(t.Context(), backend)
	require.NoError(t, err)
	model = runModelCommand(t, model, model.loadDirectory(0, navigationInitial, model.requestID))

	model, _ = updateModel(t, model, runeKey('/'))
	model.searchInput.SetValue("quarterly report")
	model, cmd := updateModel(t, model, key(tea.KeyEnter))
	model = runModelCommand(t, model, cmd)
	require.Equal(t, modeSearch, model.mode)

	model, delayedRefresh := updateModel(t, model, runeKey('r'))
	require.NotNil(t, delayedRefresh)
	pendingRequestID := model.requestID
	model, cmd = updateModel(t, model, key(tea.KeyEscape))
	require.Nil(t, cmd)
	assert.Equal(t, modeBrowse, model.mode)
	assert.Equal(t, "/", model.directory.Path)
	assert.Greater(t, model.requestID, pendingRequestID)

	model = runModelCommand(t, model, delayedRefresh)
	assert.Equal(t, modeBrowse, model.mode)
	assert.Equal(t, "/", model.directory.Path)
	require.Len(t, model.rows, 2)
}

func TestDirectoryLoadsFollowStableNodeIdentity(t *testing.T) {
	backend := newFakeBackend()
	model, err := New(t.Context(), backend)
	require.NoError(t, err)
	model = runModelCommand(t, model, model.loadDirectory(0, navigationInitial, model.requestID))

	moved := backend.nodes["/docs"]
	moved.Path = "/archive/docs"
	backend.nodes["/docs"] = moved
	model, cmd := updateModel(t, model, key(tea.KeyEnter))
	model = runModelCommand(t, model, cmd)
	assert.Equal(t, "/archive/docs", model.directory.Path)
	assert.Equal(t, []int64{2}, backend.nodeIDs)
	assert.Equal(t, []string{"/"}, backend.statPaths,
		"only initial root discovery should resolve a stored path")

	moved.Path = "/renamed/docs"
	backend.nodes["/docs"] = moved
	model, cmd = updateModel(t, model, runeKey('r'))
	model = runModelCommand(t, model, cmd)
	assert.Equal(t, "/renamed/docs", model.directory.Path)
	assert.Equal(t, []int64{2, 2}, backend.nodeIDs)
}

func TestOpeningDirectoryFromSearchResetsRelevanceSort(t *testing.T) {
	backend := newFakeBackend()
	model, err := New(t.Context(), backend)
	require.NoError(t, err)
	model.mode = modeSearch
	model.sortField = sortByRelevance
	model.sortDesc = true
	model.rows = []row{{node: backend.nodes["/docs"], path: "/docs", rank: 0}}
	model.total = 1

	model, cmd := updateModel(t, model, key(tea.KeyEnter))
	require.NotNil(t, cmd)
	model = runModelCommand(t, model, cmd)
	assert.Equal(t, modeBrowse, model.mode)
	assert.Equal(t, sortByName, model.sortField)
	assert.False(t, model.sortDesc)
	assert.Equal(t, "/docs", model.directory.Path)
}

func TestNarrowLayoutKeepsSelectionVisible(t *testing.T) {
	backend := newFakeBackend()
	model, err := New(t.Context(), backend)
	require.NoError(t, err)
	model.width, model.height = 52, 12
	model.directory = backend.nodes["/"]
	for index := range 10 {
		node := api.Node{ID: int64(index + 10), Kind: "file", Name: fmt.Sprintf("item-%02d", index)}
		model.rows = append(model.rows, row{node: node, path: "/" + node.Name})
	}
	model.total = len(model.rows)
	model.loading = false

	assert.Equal(t, 7, model.visibleRows())
	for range 5 {
		model.moveCursor(1)
	}
	assert.Equal(t, 5, model.cursor)
	assert.Equal(t, 0, model.offset)
	assert.Contains(t, model.renderList(model.width, 9), "item-05")
	assert.GreaterOrEqual(t, model.cursor, model.offset)
	assert.Less(t, model.cursor, model.offset+model.visibleRows())
}

func TestAnalyticalTableSortsWithoutChangingSelection(t *testing.T) {
	model, err := New(t.Context(), newFakeBackend())
	require.NoError(t, err)
	model.width, model.height = 100, 12
	model.directory = newFakeBackend().nodes["/"]
	model.rows = []row{
		{node: api.Node{ID: 10, Kind: "file", Name: "large.bin", Size: 2048,
			ModifiedAt: "2026-07-22T14:00:00Z"}, path: "/large.bin"},
		{node: api.Node{ID: 11, Kind: "dir", Name: "zeta",
			ModifiedAt: "2026-07-20T10:00:00Z"}, path: "/zeta"},
		{node: api.Node{ID: 12, Kind: "file", Name: "small.txt", Size: 12,
			ModifiedAt: "2026-07-21T09:30:00Z"}, path: "/small.txt"},
	}
	model.total = len(model.rows)
	model.loading = false
	model.sortRows()
	model.selectNode(10)

	model, _ = updateModel(t, model, runeKey('s')) // size
	require.Equal(t, sortBySize, model.sortField)
	assert.Equal(t, []int64{11, 12, 10}, rowIDs(model.rows))
	selected, ok := model.selected()
	require.True(t, ok)
	assert.Equal(t, int64(10), selected.node.ID)

	model, _ = updateModel(t, model, runeKey('v'))
	assert.True(t, model.sortDesc)
	assert.Equal(t, []int64{11, 10, 12}, rowIDs(model.rows),
		"directories remain first while file sizes reverse")
	content := model.View().Content
	assert.Contains(t, content, "SIZE↓")
	assert.Contains(t, content, "MODIFIED")
	assert.Contains(t, content, "2.0 KB")
	assert.Contains(t, content, "2026-07-22 14:00Z")
	assert.NotContains(t, content, "Document authority")
}

func TestSearchKeepsRelevanceUntilSortChanges(t *testing.T) {
	model, err := New(t.Context(), newFakeBackend())
	require.NoError(t, err)
	model.mode = modeSearch
	model.sortField = sortByRelevance
	model.rows = []row{
		{node: api.Node{ID: 20, Kind: "file", Name: "zeta"}, path: "/zeta", rank: 0},
		{node: api.Node{ID: 21, Kind: "file", Name: "alpha"}, path: "/alpha", rank: 1},
	}
	model.sortRows()
	assert.Equal(t, []int64{20, 21}, rowIDs(model.rows))

	model, _ = updateModel(t, model, runeKey('s'))
	assert.Equal(t, sortByName, model.sortField)
	assert.Equal(t, []int64{21, 20}, rowIDs(model.rows))

	model.sortField = sortBySize
	model.sortDesc = true
	model.searchQuery = "report"
	model.requestID = 7
	model.selectNode(20)
	refreshed, _ := updateModel(t, model, searchLoadedMsg{
		requestID: 7,
		query:     "report",
		report: api.SearchReport{Hits: []api.SearchHit{
			{Node: api.Node{ID: 20, Kind: "file", Name: "zeta", Size: 20}, Path: "/zeta"},
			{Node: api.Node{ID: 21, Kind: "file", Name: "alpha", Size: 10}, Path: "/alpha"},
		}},
	})
	assert.Equal(t, sortBySize, refreshed.sortField)
	assert.True(t, refreshed.sortDesc)
	assert.Equal(t, []int64{20, 21}, rowIDs(refreshed.rows))
	selected, ok := refreshed.selected()
	require.True(t, ok)
	assert.Equal(t, int64(20), selected.node.ID)
}

func TestExpandedDetailExposesCompleteAuthority(t *testing.T) {
	backend := newFakeBackend()
	model, err := New(t.Context(), backend)
	require.NoError(t, err)
	model = runModelCommand(t, model, model.loadDirectory(0, navigationInitial, model.requestID))
	model.width, model.height = 80, 12
	model.cursor = 1

	model, cmd := updateModel(t, model, key(tea.KeyEnter))
	require.Nil(t, cmd)
	require.True(t, model.detailOpen)
	selected, ok := model.selected()
	require.True(t, ok)
	content := model.View().Content
	assert.Contains(t, content, selected.node.CurrentVersionID)
	assert.Contains(t, content, selected.node.BlobHash)
	assert.Contains(t, content, "esc close")

	model.width, model.height = 24, 8
	lines := model.expandedDetailLines(model.width)
	compact := strings.ReplaceAll(strings.Join(lines, ""), " ", "")
	assert.Contains(t, compact, selected.node.CurrentVersionID)
	assert.Contains(t, compact, selected.node.BlobHash)
	model, _ = updateModel(t, model, key(tea.KeyEnd))
	assert.Positive(t, model.detailOffset)

	model, cmd = updateModel(t, model, key(tea.KeyEscape))
	require.Nil(t, cmd)
	assert.False(t, model.detailOpen)
}

func TestHelpAndSpinnerAreVisible(t *testing.T) {
	model, err := New(t.Context(), newFakeBackend())
	require.NoError(t, err)
	model.width, model.height = 80, 20

	model, _ = updateModel(t, model, runeKey('?'))
	assert.True(t, model.helpOpen)
	assert.Contains(t, model.View().Content, "Keyboard shortcuts")
	assert.Contains(t, model.View().Content, "Press any key to close")
	for line := range strings.SplitSeq(model.View().Content, "\n") {
		assert.LessOrEqual(t, lipgloss.Width(line), model.width)
	}

	model, _ = updateModel(t, model, runeKey('x'))
	assert.False(t, model.helpOpen)
	model.loading = true
	model, cmd := updateModel(t, model, spinnerTickMsg{})
	require.NotNil(t, cmd)
	assert.Equal(t, 1, model.spinnerFrame)
	assert.Contains(t, model.View().Content, "loading")
}

func TestChromeAdaptsWithoutDroppingPrimaryContext(t *testing.T) {
	model, err := New(t.Context(), newFakeBackend())
	require.NoError(t, err)
	model.width, model.height = 32, 12
	model.directory = newFakeBackend().nodes["/"]
	model.rows = []row{{node: api.Node{ID: 2, Kind: "dir", Name: "docs"}, path: "/docs"}}
	model.total = 1000
	model.loading = false

	assert.Contains(t, model.renderTitleBar(), "docbank")
	assert.Contains(t, model.renderTitleBar(), "READ ONLY")
	assert.Contains(t, model.renderLocation(), "1000")
	footer := model.renderFooter()
	assert.Contains(t, footer, "↑/↓ move")
	assert.NotContains(t, footer, "refresh", "low-priority hint should drop first")
	assert.LessOrEqual(t, lipgloss.Width(footer), model.width)

	model.sortField = sortBySize
	assert.Contains(t, model.renderLocation(), "size↑",
		"a hidden active sort column must remain visible")
}

func TestModifiedTimeIsRenderedAsUTC(t *testing.T) {
	assert.Equal(t, "2026-07-22 19:00Z", formatModified("2026-07-22T14:00:00-05:00"))
}

func TestFitHintsDropsLowestPriorityFirst(t *testing.T) {
	hints := []hint{
		{text: "move", priority: 100},
		{text: "refresh", priority: 10},
		{text: "search", priority: 90},
	}
	assert.Equal(t, "move │ refresh │ search", fitHints(hints, 100))
	narrow := fitHints(hints, len("move │ search"))
	assert.Equal(t, "move │ search", narrow)
}

func TestModelIgnoresStaleLoadsAndShowsCurrentErrors(t *testing.T) {
	backend := newFakeBackend()
	model, err := New(t.Context(), backend)
	require.NoError(t, err)
	model.requestID = 4
	stale, _ := updateModel(t, model, directoryLoadedMsg{
		requestID: 3, directory: backend.nodes["/"], page: backend.children[1],
	})
	assert.Empty(t, stale.rows)

	current, _ := updateModel(t, model, directoryLoadedMsg{
		requestID: 4, err: errors.New("daemon unavailable"),
	})
	require.ErrorContains(t, current.err, "daemon unavailable")
	current.width, current.height = 80, 12
	assert.Contains(t, current.View().Content, "daemon unavailable")
}

func TestViewEscapesTerminalTextAndFitsResponsiveLayouts(t *testing.T) {
	backend := newFakeBackend()
	model, err := New(t.Context(), backend)
	require.NoError(t, err)
	bad := api.Node{
		ID: 9, Kind: "file", Name: "bad\n\x1b[31m.txt", Revision: 1,
		CurrentVersionID: "33333333-3333-4333-8333-333333333333",
		BlobHash:         strings.Repeat("c", 64), ModifiedAt: "2026-07-22T14:00:00Z",
	}
	model.directory = backend.nodes["/"]
	model.rows = rowsForDirectory(model.directory, []api.Node{bad})
	model.total = 1
	model.loading = false

	for _, size := range []struct{ width, height int }{{100, 18}, {52, 18}, {24, 10}} {
		model.width, model.height = size.width, size.height
		content := model.View().Content
		assert.NotContains(t, content, "\x1b[31m.txt", "raw terminal escape must not render")
		if size.width >= 52 {
			assert.Contains(t, content, `bad\n\x1b[31m.txt`)
		}
		assert.Len(t, strings.Split(content, "\n"), size.height)
		for index, line := range strings.Split(content, "\n") {
			assert.LessOrEqual(t, lipgloss.Width(line), size.width,
				"line %d exceeds the %d-column frame", index, size.width)
		}
	}
}

func TestNewRejectsMissingDependencies(t *testing.T) {
	var nilContext context.Context
	_, err := New(nilContext, newFakeBackend())
	require.Error(t, err)
	_, err = New(t.Context(), nil)
	require.Error(t, err)
}

func updateModel(t *testing.T, model Model, msg tea.Msg) (Model, tea.Cmd) {
	t.Helper()
	next, cmd := model.Update(msg)
	result, ok := next.(Model)
	require.True(t, ok)
	return result, cmd
}

func runModelCommand(t *testing.T, model Model, cmd tea.Cmd) Model {
	t.Helper()
	require.NotNil(t, cmd)
	return firstModel(t, model, cmd())
}

func firstModel(t *testing.T, model Model, msg tea.Msg) Model {
	t.Helper()
	if batch, ok := msg.(tea.BatchMsg); ok {
		for _, cmd := range batch {
			if cmd != nil {
				model = firstModel(t, model, cmd())
			}
		}
		return model
	}
	next, _ := updateModel(t, model, msg)
	return next
}

func key(code rune) tea.KeyPressMsg {
	return tea.KeyPressMsg{Code: code}
}

func runeKey(value rune) tea.KeyPressMsg {
	return tea.KeyPressMsg{Code: value, Text: string(value)}
}

func rowIDs(rows []row) []int64 {
	ids := make([]int64, 0, len(rows))
	for _, item := range rows {
		ids = append(ids, item.node.ID)
	}
	return ids
}
