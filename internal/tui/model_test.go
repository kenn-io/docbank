package tui

import (
	"context"
	"errors"
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
	if f.err != nil {
		return api.Node{}, f.err
	}
	node, ok := f.nodes[path]
	if !ok {
		return api.Node{}, errors.New("not found")
	}
	return node, nil
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
	model = runModelCommand(t, model, model.loadDirectory("/", navigationInitial, model.requestID))
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
	require.NotNil(t, cmd)
	model = runModelCommand(t, model, cmd)
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
	require.NotNil(t, cmd)
	model = runModelCommand(t, model, cmd)
	assert.Equal(t, modeBrowse, model.mode)
	assert.Equal(t, "/", model.directory.Path)
	require.Len(t, model.rows, 2)
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

	for _, size := range []struct{ width, height int }{{100, 18}, {52, 18}} {
		model.width, model.height = size.width, size.height
		content := model.View().Content
		assert.NotContains(t, content, "\x1b[31m.txt", "raw terminal escape must not render")
		assert.Contains(t, content, `bad\n\x1b[31m.txt`)
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
