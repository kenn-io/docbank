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
	"go.kenn.io/docbank/internal/store"
)

type fakeBackend struct {
	nodes          map[string]api.Node
	children       map[int64]api.NodePage
	search         api.SearchReport
	err            error
	childLimit     int
	searchMax      int
	nodeIDs        []int64
	statPaths      []string
	history        map[string]api.AuditEventPage
	historyErr     error
	historyIDs     []int64
	historyCursors []string
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
		nodes: map[string]api.Node{
			"/": root, "/docs": docs, "/README.txt": readme, "/docs/report.txt": report,
		},
		children: map[int64]api.NodePage{
			1: {Items: []api.Node{docs, readme}, Total: 2, Limit: maxBrowserItems},
			2: {Items: []api.Node{report}, Total: 1, Limit: maxBrowserItems},
		},
		search: api.SearchReport{
			Hits:  []api.SearchHit{{Node: report, Path: "/docs/report.txt", Match: "content"}},
			Limit: maxSearchItems,
		},
		history: map[string]api.AuditEventPage{
			"": {
				Node: readme, Path: "/README.txt", Total: 2, Limit: maxHistoryItems,
				Items: []api.AuditEvent{
					{
						ID:                strings.Repeat("c", 64),
						OperationID:       "33333333-3333-4333-8333-333333333333",
						OperationSequence: 4, Ordinal: 0, NodeID: readme.ID,
						Kind: "node_path", ScopeID: "44444444-4444-4444-8444-444444444444",
						RecordedAt: "2026-07-22T14:00:00Z", Origin: "cli",
						PriorNodeRevision: 1, ResultingNodeRevision: 2,
						OldPath: &api.AuditPathState{Path: "/notes.txt", State: "live"},
						NewPath: &api.AuditPathState{Path: "/README.txt", State: "live"},
					},
					{
						ID:                strings.Repeat("d", 64),
						OperationID:       "55555555-5555-4555-8555-555555555555",
						OperationSequence: 3, Ordinal: 0, NodeID: readme.ID,
						Kind: "content_replace", ScopeID: "44444444-4444-4444-8444-444444444444",
						RecordedAt: "2026-07-22T13:00:00Z", Origin: "agent",
						PriorNodeRevision: 1, ResultingNodeRevision: 2,
						PriorCurrentVersionID:     new("66666666-6666-4666-8666-666666666666"),
						ResultingCurrentVersionID: new("77777777-7777-4777-8777-777777777777"),
					},
				},
			},
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

func (f *fakeBackend) AuditHistory(
	_ context.Context, _ string, nodeID int64, limit int, cursor string,
) (api.AuditEventPage, error) {
	f.historyIDs = append(f.historyIDs, nodeID)
	f.historyCursors = append(f.historyCursors, cursor)
	if f.historyErr != nil {
		return api.AuditEventPage{}, f.historyErr
	}
	page, ok := f.history[cursor]
	if !ok {
		return api.AuditEventPage{}, errors.New("history page not found")
	}
	page.Limit = limit
	page.Cursor = cursor
	return page, nil
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

func TestModelBrowsesAuditedHistoryAndReturnsToTree(t *testing.T) {
	backend := newFakeBackend()
	model, err := New(t.Context(), backend)
	require.NoError(t, err)
	model.width, model.height = 120, 24
	model = runModelCommand(t, model, model.loadDirectory(0, navigationInitial, model.requestID))
	model.selectNode(3)

	model, cmd := updateModel(t, model, runeKey('a'))
	require.NotNil(t, cmd)
	assert.True(t, model.historyOpen)
	model = runModelCommand(t, model, cmd)
	require.Equal(t, []int64{3}, backend.historyIDs)
	require.Len(t, model.historyPages, 1)
	assert.Contains(t, model.render(), "Audit history")
	assert.Contains(t, model.render(), "node_path")
	assert.Contains(t, model.render(), `"/notes.txt" → "/README.txt"`)

	model, cmd = updateModel(t, model, key(tea.KeyEnter))
	require.Nil(t, cmd)
	assert.True(t, model.historyDetail)
	detail := model.render()
	assert.Contains(t, detail, strings.Repeat("c", 64))
	assert.Contains(t, detail, "33333333-3333-4333-8333-333333333333")
	assert.Contains(t, detail, "44444444-4444-4444-8444-444444444444")

	model, _ = updateModel(t, model, key(tea.KeyEscape))
	assert.False(t, model.historyDetail)
	model, _ = updateModel(t, model, key(tea.KeyEscape))
	assert.False(t, model.historyOpen)
	selected, ok := model.selected()
	require.True(t, ok)
	assert.Equal(t, int64(3), selected.node.ID)
}

func TestHistoryDetailShowsCompleteAttachmentTransition(t *testing.T) {
	backend := newFakeBackend()
	page := backend.history[""]
	page.Items = []api.AuditEvent{{
		ID:                strings.Repeat("f", 64),
		OperationID:       "99999999-9999-4999-8999-999999999999",
		OperationSequence: 5, Ordinal: 1, NodeID: page.Node.ID,
		Kind: "tag_rename", ScopeID: "44444444-4444-4444-8444-444444444444",
		RecordedAt: "2026-07-22T15:00:00Z", Origin: "agent",
		PriorNodeRevision: 2, ResultingNodeRevision: 2,
		Attachment: &api.AuditAttachmentChange{
			Kind: "tag_definition",
			Identity: api.AuditAttachmentIdentity{
				TagID: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa",
			},
			Before: &api.AuditAttachmentState{
				TagID: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", TagName: "draft",
			},
			After: &api.AuditAttachmentState{
				TagID: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", TagName: "final",
			},
		},
	}}
	backend.history[""] = page
	model, err := New(t.Context(), backend)
	require.NoError(t, err)
	model.width, model.height = 100, 24
	model = runModelCommand(t, model, model.loadDirectory(0, navigationInitial, model.requestID))
	model.selectNode(3)
	model, cmd := updateModel(t, model, runeKey('a'))
	model = runModelCommand(t, model, cmd)
	model, _ = updateModel(t, model, key(tea.KeyEnter))

	detail := model.render()
	assert.Contains(t, detail, "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa")
	assert.Contains(t, detail, `Tag name: "draft"`)
	assert.Contains(t, detail, `Tag name: "final"`)
}

func TestHistoryPathsRemainTerminalSafe(t *testing.T) {
	event := api.AuditEvent{
		OldPath: &api.AuditPathState{Path: "/old\nname", State: "live"},
		NewPath: &api.AuditPathState{Path: "/new\x1b[31m", State: "live"},
	}
	summary := historyEventSummary(event)
	assert.NotContains(t, summary, "\n")
	assert.NotContains(t, summary, "\x1b")
	assert.Contains(t, summary, `\n`)
	assert.Contains(t, summary, `\x1b`)
}

func TestModelReportsUnauditedNodePlainly(t *testing.T) {
	backend := newFakeBackend()
	backend.historyErr = store.ErrAuditNotEnrolled
	model, err := New(t.Context(), backend)
	require.NoError(t, err)
	model.width, model.height = 100, 20
	model = runModelCommand(t, model, model.loadDirectory(0, navigationInitial, model.requestID))
	model.selectNode(3)

	model, cmd := updateModel(t, model, runeKey('a'))
	model = runModelCommand(t, model, cmd)
	assert.True(t, model.historyOpen)
	assert.Contains(t, model.render(), "not protected by permanent audit history")
}

func TestHistoryPaginatesOlderAndNewerWithoutLosingTreeState(t *testing.T) {
	backend := newFakeBackend()
	first := backend.history[""]
	first.NextCursor = "older"
	first.Total = 3
	backend.history[""] = first
	backend.history["older"] = api.AuditEventPage{
		Node: first.Node, Path: first.Path, Total: 3, Limit: maxHistoryItems,
		Items: []api.AuditEvent{{
			ID:                strings.Repeat("e", 64),
			OperationID:       "88888888-8888-4888-8888-888888888888",
			OperationSequence: 1, Ordinal: 0, NodeID: first.Node.ID,
			Kind: "audit_enroll", ScopeID: "44444444-4444-4444-8444-444444444444",
			RecordedAt: "2026-07-22T12:00:00Z", Origin: "cli",
		}},
	}
	model, err := New(t.Context(), backend)
	require.NoError(t, err)
	model.width, model.height = 100, 20
	model = runModelCommand(t, model, model.loadDirectory(0, navigationInitial, model.requestID))
	model.selectNode(3)
	model, cmd := updateModel(t, model, runeKey('a'))
	model = runModelCommand(t, model, cmd)

	model, cmd = updateModel(t, model, runeKey('n'))
	require.NotNil(t, cmd)
	model = runModelCommand(t, model, cmd)
	assert.Equal(t, 1, model.historyPage)
	assert.Equal(t, []string{"", "older"}, backend.historyCursors)
	assert.Contains(t, model.render(), "audit_enroll")

	model, cmd = updateModel(t, model, runeKey('p'))
	require.Nil(t, cmd)
	assert.Equal(t, 0, model.historyPage)
	assert.Contains(t, model.render(), "node_path")
}

func TestHistoryPaginationKeepsInitialTimelineTotal(t *testing.T) {
	backend := newFakeBackend()
	first := backend.history[""]
	first.NextCursor = "older"
	first.Total = 3
	backend.history[""] = first
	older := first
	older.Cursor = "older"
	older.NextCursor = ""
	older.Total = 4
	older.Items = older.Items[:1]
	backend.history["older"] = older

	model, err := New(t.Context(), backend)
	require.NoError(t, err)
	model.width, model.height = 100, 20
	model = runModelCommand(t, model, model.loadDirectory(0, navigationInitial, model.requestID))
	model.selectNode(3)
	model, cmd := updateModel(t, model, runeKey('a'))
	model = runModelCommand(t, model, cmd)
	assert.Contains(t, model.render(), "of 3")

	model, cmd = updateModel(t, model, runeKey('n'))
	model = runModelCommand(t, model, cmd)
	require.Equal(t, 1, model.historyPage)
	assert.Equal(t, 3, model.historyPages[1].Total)
	assert.Contains(t, model.render(), "of 3")
	assert.NotContains(t, model.render(), "of 4")
}

func TestOlderHistoryLoadErrorRemainsVisibleWithCachedEvents(t *testing.T) {
	backend := newFakeBackend()
	first := backend.history[""]
	first.NextCursor = "older"
	backend.history[""] = first

	model, err := New(t.Context(), backend)
	require.NoError(t, err)
	model.width, model.height = 100, 20
	model = runModelCommand(t, model, model.loadDirectory(0, navigationInitial, model.requestID))
	model.selectNode(3)
	model, cmd := updateModel(t, model, runeKey('a'))
	model = runModelCommand(t, model, cmd)
	require.Len(t, model.historyPages, 1)

	applied, _ := model.applyHistory(historyLoadedMsg{
		requestID: model.requestID,
		pageIndex: 1,
		err:       errors.New("synthetic older-page failure"),
	})
	appliedModel, ok := applied.(Model)
	require.True(t, ok)
	model = appliedModel
	rendered := model.render()
	assert.Contains(t, rendered, "synthetic older-page failure")
	assert.Contains(t, rendered, "node_path")
}

func TestClosingHistoryInvalidatesDelayedResponse(t *testing.T) {
	backend := newFakeBackend()
	model, err := New(t.Context(), backend)
	require.NoError(t, err)
	model = runModelCommand(t, model, model.loadDirectory(0, navigationInitial, model.requestID))
	model.selectNode(3)

	model, delayed := updateModel(t, model, runeKey('a'))
	require.NotNil(t, delayed)
	pendingRequestID := model.requestID
	model, _ = updateModel(t, model, key(tea.KeyEscape))
	assert.False(t, model.historyOpen)
	assert.Greater(t, model.requestID, pendingRequestID)

	model = runModelCommand(t, model, delayed)
	assert.False(t, model.historyOpen)
	assert.Empty(t, model.historyPages)
}

func TestNewerHistoryNavigationInvalidatesDelayedOlderPage(t *testing.T) {
	backend := newFakeBackend()
	first := backend.history[""]
	first.NextCursor = "older"
	first.Total = 4
	backend.history[""] = first
	older := first
	older.Items = older.Items[:1]
	older.Cursor = "older"
	older.NextCursor = "oldest"
	backend.history["older"] = older
	oldest := older
	oldest.Cursor = "oldest"
	oldest.NextCursor = ""
	backend.history["oldest"] = oldest

	model, err := New(t.Context(), backend)
	require.NoError(t, err)
	model = runModelCommand(t, model, model.loadDirectory(0, navigationInitial, model.requestID))
	model.selectNode(3)
	model, cmd := updateModel(t, model, runeKey('a'))
	model = runModelCommand(t, model, cmd)
	model, cmd = updateModel(t, model, runeKey('n'))
	model = runModelCommand(t, model, cmd)
	require.Equal(t, 1, model.historyPage)

	model, delayed := updateModel(t, model, runeKey('n'))
	require.NotNil(t, delayed)
	pendingRequestID := model.requestID
	model, _ = updateModel(t, model, runeKey('p'))
	assert.Equal(t, 0, model.historyPage)
	assert.False(t, model.loading)
	assert.Greater(t, model.requestID, pendingRequestID)

	model = runModelCommand(t, model, delayed)
	assert.Equal(t, 0, model.historyPage)
	assert.Len(t, model.historyPages, 2)
}

func TestHistoryInspectionInvalidatesDelayedOlderPage(t *testing.T) {
	backend := newFakeBackend()
	first := backend.history[""]
	first.NextCursor = "older"
	first.Total = 3
	backend.history[""] = first
	backend.history["older"] = first

	model, err := New(t.Context(), backend)
	require.NoError(t, err)
	model = runModelCommand(t, model, model.loadDirectory(0, navigationInitial, model.requestID))
	model.selectNode(3)
	model, cmd := updateModel(t, model, runeKey('a'))
	model = runModelCommand(t, model, cmd)
	selectedBefore, ok := model.selectedHistoryEvent()
	require.True(t, ok)

	model, delayed := updateModel(t, model, runeKey('n'))
	require.NotNil(t, delayed)
	pendingRequestID := model.requestID
	model, _ = updateModel(t, model, key(tea.KeyEnter))
	assert.True(t, model.historyDetail)
	assert.False(t, model.loading)
	assert.Greater(t, model.requestID, pendingRequestID)

	model = runModelCommand(t, model, delayed)
	selectedAfter, ok := model.selectedHistoryEvent()
	require.True(t, ok)
	assert.True(t, model.historyDetail)
	assert.Equal(t, 0, model.historyPage)
	assert.Equal(t, selectedBefore.ID, selectedAfter.ID)
}

func TestInitialHistoryLoadSurvivesNavigationKeys(t *testing.T) {
	backend := newFakeBackend()
	model, err := New(t.Context(), backend)
	require.NoError(t, err)
	model = runModelCommand(t, model, model.loadDirectory(0, navigationInitial, model.requestID))
	model.selectNode(3)

	model, delayed := updateModel(t, model, runeKey('a'))
	require.NotNil(t, delayed)
	pendingRequestID := model.requestID
	for _, pressed := range []tea.KeyPressMsg{
		key(tea.KeyUp), runeKey('p'), key(tea.KeyHome),
	} {
		model, _ = updateModel(t, model, pressed)
	}
	assert.True(t, model.loading)
	assert.Equal(t, pendingRequestID, model.requestID)

	model = runModelCommand(t, model, delayed)
	assert.False(t, model.loading)
	require.Len(t, model.historyPages, 1)
	event, ok := model.selectedHistoryEvent()
	require.True(t, ok)
	assert.Equal(t, "node_path", event.Kind)
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
