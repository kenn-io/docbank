package tui

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/store"
)

type fakeBackend struct {
	nodes                     map[string]api.Node
	children                  map[int64]api.NodePage
	search                    api.SearchReport
	err                       error
	childLimit                int
	searchMax                 int
	nodeIDs                   []int64
	statPaths                 []string
	history                   map[string]api.AuditEventPage
	historyErr                error
	historyIDs                []int64
	historyCursors            []string
	tags                      map[int64]api.TagPage
	tagNodeIDs                []int64
	jobs                      []api.Job
	jobsErr                   error
	jobCalls                  int
	info                      api.VaultInfo
	infoErr                   error
	infoCalls                 int
	snapshots                 []api.BackupSnapshot
	backupErr                 error
	backupCalls               int
	profiles                  []api.ProcessingProfileSummary
	profileCalls              int
	plan                      api.ProcessingPlan
	plans                     map[string]api.ProcessingPlan
	planCalls                 int
	coverage                  api.CoverageReport
	coverageCalls             int
	processingSearch          api.DocumentSearchReport
	processingSearchCalls     int
	processingJob             api.ProcessingJob
	processingStatus          api.ProcessingStatus
	processingTerminalRelease <-chan struct{}
	rendition                 Rendition
	processingStarts          []api.StartProcessingRequest
	trash                     api.TrashPage
	trashCalls                int
	trashed                   []api.Node
	restored                  []api.Node
	mutationErr               error
	mutationReceiptErr        error
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
		tags: map[int64]api.TagPage{
			readme.ID: {
				Items: []api.Tag{
					{
						ID:   "88888888-8888-4888-8888-888888888888",
						Name: "reviewed", Revision: 2, AssignmentCount: 3,
					},
					{
						ID:   "99999999-9999-4999-8999-999999999999",
						Name: "tax", Revision: 4, AssignmentCount: 7,
					},
				},
				Total: 2, Limit: maxBrowserItems,
			},
		},
		jobs: []api.Job{
			{
				Name: "text-extraction", Status: "running",
				StartedAt: "2026-07-22T12:00:00Z",
			},
			{
				Name: "watch:inbox", Status: "failed",
				StartedAt: "2026-07-22T11:00:00Z", FinishedAt: "2026-07-22T11:05:00Z",
				Error: "source is temporarily unavailable; check the configured inbox path",
			},
		},
		info: api.VaultInfo{
			VaultID:   "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa",
			LiveFiles: 12, LiveDirectories: 4, ContentVersions: 19,
			TrackedBlobs: 15, TrackedBlobBytes: 3_000_000,
			Storage: api.StorageStatus{
				LooseBlobs: 5, LooseBytes: 1_000_000,
				Packs: 2, PackStoredBytes: 900_000,
				PackedBlobs: 10, PackedRawBytes: 2_000_000,
				PackedStoredBytes: 800_000, DeadPackedBytes: 100_000,
			},
		},
		snapshots: []api.BackupSnapshot{
			{
				ID:        "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc",
				CreatedAt: "2026-07-22T14:30:00+02:00", Tag: "baseline",
				Files: 10, BytesAdded: 700_000,
			},
			{
				ID:        "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
				CreatedAt: "2026-07-22T13:00:00Z", Tag: "weekly",
				Files: 12, BytesAdded: 750_000,
			},
		},
		trash: api.TrashPage{
			Items: []api.Node{
				{
					ID: 20, Kind: "file", Name: "old-report.txt", Revision: 4,
					Size: 42, TrashedAt: "2026-07-22T15:00:00Z",
				},
			},
			Total: 1, Limit: maxTrashItems,
		},
		profiles: []api.ProcessingProfileSummary{{
			Name: "private", Fingerprint: strings.Repeat("a", 64), Rendition: true,
			EmbeddingBindings: []string{"semantic"},
		}},
		plan: api.ProcessingPlan{
			Fingerprint: strings.Repeat("b", 64), VaultUID: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa",
			Selector:           api.ProcessingSelector{NodeID: readme.ID, ContentVersionID: readme.CurrentVersionID, Profile: "private"},
			ProfileFingerprint: strings.Repeat("a", 64),
			Flow: []api.ProcessingFlowHop{
				{Capability: "rendition", ProviderID: "docling-local", TrustBoundary: "operator_network", InputClasses: []string{"original_file"},
					RuntimeDisclosure: api.ProcessingRuntimeDisclosure{ImmediateProcessor: "docbank adapter", UltimateProcessor: "Docling Serve",
						Endpoint: "http://docling.internal:5001", Deployment: strings.Repeat("d", 64), Model: "layout", ModelRevision: "2026.08",
						MetadataClasses: []string{"synthetic_filename"}, RetainedArtifactRoles: []string{"sanitized_markdown"}}},
				{Capability: "embedding", ProviderID: "local-embed", TrustBoundary: "local_process", InputClasses: []string{"rendition_chunk"},
					RuntimeDisclosure: api.ProcessingRuntimeDisclosure{ImmediateProcessor: "local-embed", UltimateProcessor: "local-embed",
						Endpoint: "in-process", Deployment: strings.Repeat("e", 64), Model: "nomic", ModelRevision: "v1", VectorSpace: strings.Repeat("f", 64),
						MetadataClasses: []string{"chunk_key"}, RetainedArtifactRoles: []string{"embedding_vector_set"}}},
			},
			DisclosedClasses: []string{"original_file", "rendition_chunk"},
			RetainedClasses:  []string{"sanitized_markdown", "embedding_vector_set"},
			Estimate:         api.ProcessingEstimate{SourceBytes: 12, ProviderCalls: 2, VectorSpaces: 1},
			ConsentRequired:  true, ConsentState: "required", BackupConsequence: "retained derivatives enter future backups",
		},
		coverage: api.CoverageReport{
			VaultUID: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", ProfileFingerprint: strings.Repeat("a", 64), State: "partial",
			Renditions: api.CoverageClass{Name: "rendition", Required: true, State: "complete", Complete: 1, Total: 1},
			Embeddings: []api.CoverageClass{{Name: "semantic", State: "unavailable", Unavailable: 1, Total: 1}},
		},
		processingJob:    api.ProcessingJob{ID: strings.Repeat("e", 64), AttachmentID: strings.Repeat("f", 64), ContentVersionID: readme.CurrentVersionID},
		processingStatus: api.ProcessingStatus{JobID: strings.Repeat("e", 64), State: "completed", Phase: "published"},
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

func (f *fakeBackend) NodeTags(
	_ context.Context, nodeID int64, limit, offset int,
) (api.TagPage, error) {
	f.tagNodeIDs = append(f.tagNodeIDs, nodeID)
	if f.err != nil {
		return api.TagPage{}, f.err
	}
	page := f.tags[nodeID]
	page.Limit = limit
	page.Offset = offset
	return page, nil
}

func (f *fakeBackend) Jobs(_ context.Context) ([]api.Job, error) {
	f.jobCalls++
	if f.jobsErr != nil {
		return nil, f.jobsErr
	}
	return append([]api.Job(nil), f.jobs...), nil
}

func (f *fakeBackend) Info(_ context.Context) (api.VaultInfo, error) {
	f.infoCalls++
	if f.infoErr != nil {
		return api.VaultInfo{}, f.infoErr
	}
	return f.info, nil
}

func (f *fakeBackend) BackupList(
	_ context.Context,
) ([]api.BackupSnapshot, error) {
	f.backupCalls++
	if f.backupErr != nil {
		return nil, f.backupErr
	}
	return append([]api.BackupSnapshot(nil), f.snapshots...), nil
}

func (f *fakeBackend) ProcessingProfiles(_ context.Context) ([]api.ProcessingProfileSummary, error) {
	f.profileCalls++
	if f.err != nil {
		return nil, f.err
	}
	return append([]api.ProcessingProfileSummary(nil), f.profiles...), nil
}

func (f *fakeBackend) PlanProcessing(_ context.Context, request api.ProcessingPlanRequest) (api.ProcessingPlan, error) {
	f.planCalls++
	if f.err != nil {
		return api.ProcessingPlan{}, f.err
	}
	plan := f.plan
	if f.plans != nil {
		plan = f.plans[request.Selector.Profile]
	}
	if request.Selector != plan.Selector {
		return api.ProcessingPlan{}, errors.New("unexpected processing selector")
	}
	return plan, nil
}

func (f *fakeBackend) DocumentCoverage(_ context.Context, profile string, fence api.DocumentSourceFence) (api.CoverageReport, error) {
	f.coverageCalls++
	if f.err != nil {
		return api.CoverageReport{}, f.err
	}
	plan := f.plan
	if candidate, ok := f.plans[profile]; ok {
		plan = candidate
	}
	if profile != plan.Selector.Profile || fence.VaultUID != plan.VaultUID ||
		!assert.ObjectsAreEqual(fence.ContentVersionIDs, []string{plan.Selector.ContentVersionID}) {
		return api.CoverageReport{}, errors.New("unexpected processing coverage fence")
	}
	return f.coverage, nil
}

func (f *fakeBackend) SearchDocuments(_ context.Context, request api.DocumentSearchRequest) (api.DocumentSearchReport, error) {
	f.processingSearchCalls++
	if f.err != nil {
		return api.DocumentSearchReport{}, f.err
	}
	if request.Limit != maxProcessingSearchItems || request.Mode != "auto" || !request.Explain ||
		request.Profile != f.plan.Selector.Profile || request.Fence.VaultUID != f.plan.VaultUID ||
		!assert.ObjectsAreEqual(request.Fence.ContentVersionIDs, []string{f.plan.Selector.ContentVersionID}) {
		return api.DocumentSearchReport{}, errors.New("unexpected document search request")
	}
	return f.processingSearch, nil
}

func (f *fakeBackend) StartProcessingStream(ctx context.Context, request api.StartProcessingRequest) (ProcessingEventStream, error) {
	f.processingStarts = append(f.processingStarts, request)
	if request.Selector != f.plan.Selector || request.PlanFingerprint != f.plan.Fingerprint || request.Consent != f.plan.ConsentRequired {
		return nil, errors.New("unexpected processing start")
	}
	job, status := f.processingJob, f.processingStatus
	return &fakeProcessingEventStream{
		events: []api.ProcessingJobEvent{
			{Sequence: 1, Type: "job", Job: &job},
			{Sequence: 2, Type: "status", Status: &status, Terminal: true},
		},
		terminalRelease: f.processingTerminalRelease,
		ctx:             ctx,
	}, nil
}

type fakeProcessingEventStream struct {
	events          []api.ProcessingJobEvent
	terminalRelease <-chan struct{}
	ctx             context.Context
	next            int
}

func (stream *fakeProcessingEventStream) Next() (api.ProcessingJobEvent, error) {
	if stream.next >= len(stream.events) {
		return api.ProcessingJobEvent{}, errors.New("processing stream exhausted")
	}
	if stream.next == 1 && stream.terminalRelease != nil {
		select {
		case <-stream.terminalRelease:
		case <-stream.ctx.Done():
			return api.ProcessingJobEvent{}, stream.ctx.Err()
		}
	}
	event := stream.events[stream.next]
	stream.next++
	return event, nil
}

func (stream *fakeProcessingEventStream) Close() error { return nil }

func (f *fakeBackend) ProcessingStatus(_ context.Context, jobID string) (api.ProcessingStatus, error) {
	if jobID != f.processingJob.ID {
		return api.ProcessingStatus{}, errors.New("unexpected processing job")
	}
	return f.processingStatus, nil
}

func (f *fakeBackend) RenditionForSelector(_ context.Context, selector api.ProcessingSelector, _ int64) (Rendition, error) {
	if selector != f.plan.Selector {
		return Rendition{}, errors.New("unexpected rendition selector")
	}
	return f.rendition, nil
}

func (f *fakeBackend) TrashPage(
	_ context.Context, limit, offset int,
) (api.TrashPage, error) {
	f.trashCalls++
	if f.err != nil {
		return api.TrashPage{}, f.err
	}
	page := f.trash
	page.Limit = limit
	page.Offset = offset
	return page, nil
}

func (f *fakeBackend) Trash(
	_ context.Context, nodeID, revision int64,
) (api.Node, error) {
	f.trashed = append(f.trashed, api.Node{ID: nodeID, Revision: revision})
	if f.mutationErr != nil {
		return api.Node{}, f.mutationErr
	}
	for itemPath, node := range f.nodes {
		if node.ID == nodeID {
			livePath := itemPath
			node.TrashedAt = "2026-07-22T16:00:00Z"
			node.Path = ""
			f.nodes[itemPath] = node
			if node.ParentID != nil {
				f.bumpNodeRevision(*node.ParentID)
			}
			for parentID, page := range f.children {
				filtered := page.Items[:0]
				for _, item := range page.Items {
					if item.ID != nodeID &&
						(livePath == "" || !strings.HasPrefix(item.Path, livePath+"/")) {
						filtered = append(filtered, item)
					}
				}
				page.Items = filtered
				page.Total = len(filtered)
				f.children[parentID] = page
			}
			hits := f.search.Hits[:0]
			for _, hit := range f.search.Hits {
				if hit.Node.ID != nodeID &&
					(livePath == "" || !strings.HasPrefix(hit.Path, livePath+"/")) {
					hits = append(hits, hit)
				}
			}
			f.search.Hits = hits
			receipt := node
			receipt.Path = livePath
			return receipt, f.mutationReceiptErr
		}
	}
	return api.Node{}, errors.New("not found")
}

func (f *fakeBackend) bumpNodeRevision(nodeID int64) {
	for nodePath, node := range f.nodes {
		if node.ID != nodeID {
			continue
		}
		node.Revision++
		f.nodes[nodePath] = node
		for parentID, page := range f.children {
			for index := range page.Items {
				if page.Items[index].ID == nodeID {
					page.Items[index] = node
				}
			}
			f.children[parentID] = page
		}
		return
	}
}

func (f *fakeBackend) Restore(
	_ context.Context, nodeID, revision int64,
) (api.Node, error) {
	f.restored = append(f.restored, api.Node{ID: nodeID, Revision: revision})
	if f.mutationErr != nil {
		return api.Node{}, f.mutationErr
	}
	for index, node := range f.trash.Items {
		if node.ID == nodeID {
			f.trash.Items = append(f.trash.Items[:index], f.trash.Items[index+1:]...)
			f.trash.Total--
			node.Revision++
			node.TrashedAt = ""
			node.Path = "/restored/" + node.Name
			return node, f.mutationReceiptErr
		}
	}
	return api.Node{}, errors.New("not found")
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

func TestModelConfirmsRevisionBoundTrashAndRestore(t *testing.T) {
	backend := newFakeBackend()
	model, err := New(t.Context(), backend)
	require.NoError(t, err)
	model = runModelCommand(t, model, model.loadDirectory(0, navigationInitial, model.requestID))
	model.width, model.height = 100, 16
	model.cursor = 1

	model, cmd := updateModel(t, model, runeKey('x'))
	require.Nil(t, cmd)
	require.NotNil(t, model.confirmation)
	assert.Equal(t, mutationTrash, model.confirmation.action)
	assert.Contains(t, model.View().Content, "Move to recoverable trash?")
	assert.Contains(t, model.View().Content, "Bound revision: 2")
	assert.Empty(t, backend.trashed)

	model, cmd = updateModel(t, model, key(tea.KeyEnter))
	require.NotNil(t, cmd)
	model = runModelCommand(t, model, cmd)
	require.Len(t, backend.trashed, 1)
	assert.Equal(t, api.Node{ID: 3, Revision: 2}, backend.trashed[0])
	assert.Nil(t, model.confirmation)
	assert.Contains(t, model.notice, "recoverable trash")
	assert.NotContains(t, rowIDs(model.rows), int64(3))

	model, cmd = updateModel(t, model, runeKey('T'))
	require.NotNil(t, cmd)
	model = runModelCommand(t, model, cmd)
	assert.True(t, model.trashOpen)
	require.Len(t, model.trashItems, 1)
	assert.Contains(t, model.View().Content, `"old-report.txt"`)

	model, cmd = updateModel(t, model, key(tea.KeyEnter))
	require.Nil(t, cmd)
	require.NotNil(t, model.confirmation)
	assert.Equal(t, mutationRestore, model.confirmation.action)
	assert.Contains(t, model.View().Content, "Restore this node?")
	assert.Contains(t, model.View().Content, "Bound revision: 4")

	model, cmd = updateModel(t, model, key(tea.KeyEnter))
	require.NotNil(t, cmd)
	model = runModelCommand(t, model, cmd)
	require.Len(t, backend.restored, 1)
	assert.Equal(t, api.Node{ID: 20, Revision: 4}, backend.restored[0])
	assert.Contains(t, model.notice, `/restored/old-report.txt`)
	assert.Empty(t, model.trashItems)
}

func TestModelCancelsTrashConfirmationWithoutMutation(t *testing.T) {
	backend := newFakeBackend()
	model, err := New(t.Context(), backend)
	require.NoError(t, err)
	model = runModelCommand(t, model, model.loadDirectory(0, navigationInitial, model.requestID))

	model, _ = updateModel(t, model, runeKey('x'))
	require.NotNil(t, model.confirmation)
	model, _ = updateModel(t, model, key(tea.KeyEscape))
	assert.Nil(t, model.confirmation)
	assert.Empty(t, backend.trashed)
}

func TestTrashWaitsForNavigationAndUsesAuthoritativeReceiptPath(t *testing.T) {
	backend := newFakeBackend()
	backend.search.Hits = []api.SearchHit{{
		Node: backend.nodes["/docs"], Path: "/docs", Match: "name",
	}}
	model, err := New(t.Context(), backend)
	require.NoError(t, err)
	model = runModelCommand(t, model, model.loadDirectory(
		0, navigationInitial, model.requestID,
	))
	model, cmd := updateModel(t, model, key(tea.KeyEnter))
	model = runModelCommand(t, model, cmd)
	assert.Equal(t, "/docs", model.directory.Path)

	model, delayedRefresh := updateModel(t, model, runeKey('r'))
	require.NotNil(t, delayedRefresh)
	model, _ = updateModel(t, model, runeKey('x'))
	assert.Nil(t, model.confirmation)
	assert.Contains(t, model.notice, "finish loading")
	model = runModelCommand(t, model, delayedRefresh)

	model, _ = updateModel(t, model, runeKey('/'))
	model.searchInput.SetValue("docs")
	model, cmd = updateModel(t, model, key(tea.KeyEnter))
	model = runModelCommand(t, model, cmd)
	model, _ = updateModel(t, model, runeKey('x'))
	require.NotNil(t, model.confirmation)
	assert.Equal(t, "/docs", model.confirmation.target.path)

	docs := backend.nodes["/docs"]
	report := backend.nodes["/docs/report.txt"]
	delete(backend.nodes, "/docs")
	delete(backend.nodes, "/docs/report.txt")
	docs.Name = "documents"
	docs.Path = "/documents"
	docs.Revision++
	report.Path = "/documents/report.txt"
	backend.nodes["/documents"] = docs
	backend.nodes["/documents/report.txt"] = report
	backend.children[1] = api.NodePage{
		Items: []api.Node{docs, backend.nodes["/README.txt"]},
		Total: 2, Limit: maxBrowserItems,
	}
	backend.children[2] = api.NodePage{
		Items: []api.Node{report}, Total: 1, Limit: maxBrowserItems,
	}

	model, mutation := updateModel(t, model, key(tea.KeyEnter))
	model, rootLoad := updateModel(t, model, mutation())
	require.NotNil(t, rootLoad)
	assert.Contains(t, model.notice, `"/documents"`)
	assert.NotContains(t, model.notice, `"/docs"`)
	assert.Empty(t, model.rows)
	assert.Empty(t, model.stack)
	assert.Nil(t, model.searchReturn)

	model = runModelCommand(t, model, rootLoad)
	assert.Equal(t, "/", model.directory.Path)
	require.Len(t, model.rows, 1)
	assert.Equal(t, "/README.txt", model.rows[0].path)
}

func TestUnconfirmedMutationsInvalidateAuthority(t *testing.T) {
	t.Run("trash reloads live root", func(t *testing.T) {
		backend := newFakeBackend()
		backend.mutationReceiptErr = NewMutationUnconfirmedError(
			"trash", errors.New("receipt was truncated"),
		)
		model, err := New(t.Context(), backend)
		require.NoError(t, err)
		model = runModelCommand(t, model, model.loadDirectory(
			0, navigationInitial, model.requestID,
		))
		model, cmd := updateModel(t, model, key(tea.KeyEnter))
		model = runModelCommand(t, model, cmd)
		require.Len(t, model.stack, 1)

		model, _ = updateModel(t, model, runeKey('x'))
		model, mutation := updateModel(t, model, key(tea.KeyEnter))
		require.NotNil(t, mutation)
		model, refresh := updateModel(t, model, mutation())
		require.NotNil(t, refresh)
		assert.True(t, model.loading)
		assert.Empty(t, model.rows)
		assert.Empty(t, model.stack)
		assert.Nil(t, model.searchReturn)
		assert.Contains(t, model.notice, "trash outcome is unconfirmed")

		backend.err = errors.New("daemon temporarily unavailable")
		model = runModelCommand(t, model, refresh)
		require.ErrorContains(t, model.err, "daemon temporarily unavailable")
		assert.Empty(t, model.directory.Path)

		backend.err = nil
		model, refresh = updateModel(t, model, runeKey('r'))
		require.NotNil(t, refresh)
		model = runModelCommand(t, model, refresh)
		assert.Equal(t, "/", model.directory.Path)
		require.Len(t, model.rows, 2)
		assert.Equal(t, int64(2), model.rows[0].node.Revision)
	})

	t.Run("restore refreshes trash and reloads root on close", func(t *testing.T) {
		backend := newFakeBackend()
		backend.mutationReceiptErr = NewMutationUnconfirmedError(
			"restore", errors.New("receipt was truncated"),
		)
		model, err := New(t.Context(), backend)
		require.NoError(t, err)
		model = runModelCommand(t, model, model.loadDirectory(
			0, navigationInitial, model.requestID,
		))
		model, trashLoad := updateModel(t, model, runeKey('T'))
		model = runModelCommand(t, model, trashLoad)

		model, _ = updateModel(t, model, key(tea.KeyEnter))
		model, mutation := updateModel(t, model, key(tea.KeyEnter))
		require.NotNil(t, mutation)
		model, refresh := updateModel(t, model, mutation())
		require.NotNil(t, refresh)
		assert.True(t, model.trashChanged)
		assert.True(t, model.trashLoading)
		assert.Contains(t, model.notice, "restore outcome is unconfirmed")

		model = runModelCommand(t, model, refresh)
		assert.Empty(t, model.trashItems)
		model, rootLoad := updateModel(t, model, key(tea.KeyEscape))
		require.NotNil(t, rootLoad)
		assert.Empty(t, model.rows)
		assert.Empty(t, model.stack)
		model = runModelCommand(t, model, rootLoad)
		assert.Equal(t, "/", model.directory.Path)
	})
}

func TestTrashOverlayDoesNotCancelUnderlyingLoad(t *testing.T) {
	backend := newFakeBackend()
	model, err := New(t.Context(), backend)
	require.NoError(t, err)
	underlyingRequestID := model.requestID
	delayedDirectory := model.loadDirectory(0, navigationInitial, underlyingRequestID)

	model, delayedTrash := updateModel(t, model, runeKey('T'))
	require.NotNil(t, delayedTrash)
	assert.Equal(t, underlyingRequestID, model.requestID)
	assert.True(t, model.loading)
	assert.True(t, model.trashLoading)

	pendingTrashRequestID := model.trashRequestID
	model, _ = updateModel(t, model, key(tea.KeyEscape))
	assert.False(t, model.trashOpen)
	assert.False(t, model.trashLoading)
	assert.True(t, model.loading)
	assert.Greater(t, model.trashRequestID, pendingTrashRequestID)

	model = runModelCommand(t, model, delayedDirectory)
	assert.False(t, model.loading)
	assert.Equal(t, "/", model.directory.Path)
	require.Len(t, model.rows, 2)

	model = runModelCommand(t, model, delayedTrash)
	assert.False(t, model.trashOpen)
	assert.Empty(t, model.trashItems)
}

func TestTrashHelpShortcutOpensHelp(t *testing.T) {
	model, err := New(t.Context(), newFakeBackend())
	require.NoError(t, err)
	model, _ = updateModel(t, model, runeKey('T'))
	model, _ = updateModel(t, model, runeKey('?'))
	assert.True(t, model.trashOpen)
	assert.True(t, model.helpOpen)
}

func TestSuccessfulTrashPreservesBackAndSearchNavigation(t *testing.T) {
	t.Run("nested directory", func(t *testing.T) {
		backend := newFakeBackend()
		model, err := New(t.Context(), backend)
		require.NoError(t, err)
		model = runModelCommand(t, model, model.loadDirectory(
			0, navigationInitial, model.requestID,
		))
		model, cmd := updateModel(t, model, key(tea.KeyEnter))
		model = runModelCommand(t, model, cmd)
		require.Len(t, model.stack, 1)

		model, _ = updateModel(t, model, runeKey('x'))
		model, cmd = updateModel(t, model, key(tea.KeyEnter))
		model = runModelCommand(t, model, cmd)
		require.Len(t, model.stack, 1)
		assert.True(t, model.stack[0].stale)

		model, cmd = updateModel(t, model, key(tea.KeyLeft))
		require.NotNil(t, cmd)
		assert.True(t, model.loading)
		model = runModelCommand(t, model, cmd)
		assert.Equal(t, "/", model.directory.Path)
		require.Len(t, model.rows, 2)
		assert.Equal(t, int64(2), model.rows[0].node.Revision)
	})

	t.Run("search results", func(t *testing.T) {
		backend := newFakeBackend()
		model, err := New(t.Context(), backend)
		require.NoError(t, err)
		model = runModelCommand(t, model, model.loadDirectory(
			0, navigationInitial, model.requestID,
		))
		model, _ = updateModel(t, model, runeKey('/'))
		model.searchInput.SetValue("report")
		model, cmd := updateModel(t, model, key(tea.KeyEnter))
		model = runModelCommand(t, model, cmd)
		require.NotNil(t, model.searchReturn)

		model, _ = updateModel(t, model, runeKey('x'))
		model, cmd = updateModel(t, model, key(tea.KeyEnter))
		model = runModelCommand(t, model, cmd)
		require.NotNil(t, model.searchReturn)
		assert.True(t, model.searchReturn.stale)

		model, cmd = updateModel(t, model, key(tea.KeyEscape))
		require.NotNil(t, cmd)
		model = runModelCommand(t, model, cmd)
		assert.Equal(t, modeBrowse, model.mode)
		assert.Equal(t, "/", model.directory.Path)
		require.Len(t, model.rows, 2)
		assert.Equal(t, int64(2), model.rows[0].node.Revision)
	})
}

func TestTrashAncestorFromSearchFallsBackToLiveParent(t *testing.T) {
	backend := newFakeBackend()
	backend.search.Hits = []api.SearchHit{{
		Node: backend.nodes["/docs"], Path: "/docs", Match: "name",
	}}
	model, err := New(t.Context(), backend)
	require.NoError(t, err)
	model = runModelCommand(t, model, model.loadDirectory(
		0, navigationInitial, model.requestID,
	))
	model, cmd := updateModel(t, model, key(tea.KeyEnter))
	model = runModelCommand(t, model, cmd)
	assert.Equal(t, "/docs", model.directory.Path)
	require.Len(t, model.stack, 1)

	model, _ = updateModel(t, model, runeKey('/'))
	model.searchInput.SetValue("docs")
	model, cmd = updateModel(t, model, key(tea.KeyEnter))
	model = runModelCommand(t, model, cmd)
	require.NotNil(t, model.searchReturn)
	assert.Equal(t, "/docs", model.searchReturn.directory.Path)

	model, _ = updateModel(t, model, runeKey('x'))
	model, cmd = updateModel(t, model, key(tea.KeyEnter))
	model, rootLoad := updateModel(t, model, cmd())
	require.NotNil(t, rootLoad)
	assert.Nil(t, model.searchReturn)
	assert.Empty(t, model.stack)
	model = runModelCommand(t, model, rootLoad)
	assert.Equal(t, modeBrowse, model.mode)
	assert.Equal(t, "/", model.directory.Path)
	require.Len(t, model.rows, 1)
	assert.Equal(t, "/README.txt", model.rows[0].path)
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
	require.NotNil(t, cmd)
	require.True(t, model.detailOpen)
	model = runModelCommand(t, model, cmd)
	assert.Equal(t, []int64{3}, backend.tagNodeIDs)
	selected, ok := model.selected()
	require.True(t, ok)
	content := model.View().Content
	assert.Contains(t, content, selected.node.CurrentVersionID)
	assert.Contains(t, content, selected.node.BlobHash)
	assert.Contains(t, content, "esc close")
	fullDetail := strings.Join(model.expandedDetailLines(model.width), "\n")
	assert.Contains(t, fullDetail, `"reviewed"`)
	assert.Contains(t, fullDetail, "88888888-8888-4888-8888-888888888888")
	assert.Contains(t, fullDetail, `"tax"`)
	assert.Contains(t, fullDetail, "99999999-9999-4999-8999-999999999999")

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

func TestProcessingViewShowsPrivateFlowAndIndependentCoverage(t *testing.T) {
	backend := newFakeBackend()
	model, err := New(t.Context(), backend)
	require.NoError(t, err)
	model = runModelCommand(t, model, model.loadDirectory(0, navigationInitial, model.requestID))
	model.width, model.height = 100, 22
	model.cursor = 1

	model, cmd := updateModel(t, model, runeKey('P'))
	require.NotNil(t, cmd)
	model = runModelCommand(t, model, cmd)
	model = runModelCommand(t, model, model.loadProcessingPlan("private", model.processingRequestID))
	require.NotNil(t, model.processingPlan)
	model = runModelCommand(t, model, model.loadProcessingCoverage(*model.processingPlan, model.processingRequestID))

	content := strings.Join(model.processingLines(model.width), "\n")
	assert.Contains(t, content, "Document processing")
	assert.Contains(t, content, "Profile: private")
	assert.Contains(t, content, "Private network")
	assert.Contains(t, content, "Local process")
	assert.Contains(t, content, "Docling Serve")
	assert.Contains(t, content, "http://docling.internal:5001")
	assert.Contains(t, content, "layout@2026.08")
	assert.Contains(t, content, "synthetic_filename")
	assert.Contains(t, content, "nomic@v1")
	assert.Contains(t, content, "Rendition · required · complete · 1/1 complete")
	assert.Contains(t, content, "semantic · optional · unavailable · 0/1 complete")
	assert.Contains(t, content, "unavailable: 1")
	assert.Contains(t, content, "processing consent requires explicit confirmation")
	assert.Equal(t, 1, backend.profileCalls)
	assert.Equal(t, 1, backend.planCalls)
	assert.Equal(t, 1, backend.coverageCalls)
}

func TestProcessingViewMakesHostedRebuildAndDegradedProvenanceExplicit(t *testing.T) {
	backend := newFakeBackend()
	backend.profiles = []api.ProcessingProfileSummary{{
		Name: "hosted", Fingerprint: strings.Repeat("c", 64), EmbeddingBindings: []string{"direct-file"},
	}}
	backend.plan.Selector.Profile = "hosted"
	backend.plan.Flow = []api.ProcessingFlowHop{{
		Capability: "embedding", ProviderID: "gemini-file", TrustBoundary: "hosted_provider", InputClasses: []string{"original_file"},
	}}
	backend.coverage.State = "rebuilding"
	backend.coverage.Renditions = api.CoverageClass{Name: "rendition", State: "ineligible", Ineligible: 1, Total: 1}
	backend.coverage.Embeddings = []api.CoverageClass{{Name: "direct-file", Required: true, State: "rebuilding", Rebuilding: 1, PreviousGenerationServing: 1, Total: 1}}
	backend.processingSearch = api.DocumentSearchReport{
		ActualMode: "semantic", Coverage: api.DocumentSearchCoverage{BindingRequired: true, ScopedDocuments: 1, CompleteDocuments: 1, State: "complete"},
		Degradations: []string{"degraded_provenance"},
		Results:      []api.DocumentSearchResult{{Rank: 1, Path: "/README.txt", Evidence: []api.DocumentEvidenceReference{{Kind: "direct_file"}}}},
	}
	model, err := New(t.Context(), backend)
	require.NoError(t, err)
	model = runModelCommand(t, model, model.loadDirectory(0, navigationInitial, model.requestID))
	model.width, model.height = 100, 22
	model.cursor = 1

	model, cmd := updateModel(t, model, runeKey('P'))
	model = runModelCommand(t, model, cmd)
	model = runModelCommand(t, model, model.loadProcessingPlan("hosted", model.processingRequestID))
	require.NotNil(t, model.processingPlan)
	model = runModelCommand(t, model, model.loadProcessingCoverage(*model.processingPlan, model.processingRequestID))
	content := strings.Join(model.processingLines(model.width), "\n")
	assert.Contains(t, content, "Hosted provider")
	assert.Contains(t, content, "Document data leaves this machine for this step")
	assert.Contains(t, content, "Previous complete generation remains available while the rebuild runs")
	assert.Contains(t, content, "direct-file · required · rebuilding")

	model, cmd = updateModel(t, model, runeKey('/'))
	require.NotNil(t, cmd)
	model.searchInput.SetValue("synthetic")
	model, cmd = updateModel(t, model, key(tea.KeyEnter))
	require.NotNil(t, cmd)
	model = runModelCommand(t, model, cmd)
	content = strings.Join(model.processingLines(model.width), "\n")
	assert.Contains(t, content, "Direct-file result; no text excerpt.")
	assert.Contains(t, content, "Warning: degraded_provenance")
	assert.Contains(t, content, "Evidence: direct_file")
	assert.Equal(t, 1, backend.processingSearchCalls)
}

func TestProcessingViewSelectsProfilesAndShowsRunStatusAndRendition(t *testing.T) {
	backend := newFakeBackend()
	backend.profiles = append(backend.profiles, api.ProcessingProfileSummary{
		Name: "hosted", Fingerprint: strings.Repeat("c", 64), EmbeddingBindings: []string{"direct-file"},
	})
	backend.plans = map[string]api.ProcessingPlan{
		"private": backend.plan,
		"hosted": {
			Fingerprint: strings.Repeat("d", 64), VaultUID: backend.plan.VaultUID,
			Selector:           api.ProcessingSelector{NodeID: 3, ContentVersionID: backend.plan.Selector.ContentVersionID, Profile: "hosted"},
			ProfileFingerprint: strings.Repeat("c", 64), Flow: []api.ProcessingFlowHop{{Capability: "embedding", ProviderID: "hosted-embed", TrustBoundary: "hosted_provider", InputClasses: []string{"original_file"}}},
		},
	}
	backend.rendition = Rendition{Markdown: "# Synthetic rendition\n", BuildID: strings.Repeat("b", 64), ArtifactID: strings.Repeat("c", 64), SHA256: strings.Repeat("d", 64), Completeness: "degraded_provenance", Warnings: []string{"degraded_provenance"}}
	model, err := New(t.Context(), backend)
	require.NoError(t, err)
	model = runModelCommand(t, model, model.loadDirectory(0, navigationInitial, model.requestID))
	model.width, model.height = 100, 30
	model.cursor = 1
	model, cmd := updateModel(t, model, runeKey('P'))
	model = runModelCommand(t, model, cmd)
	model = runModelCommand(t, model, model.loadProcessingPlan("private", model.processingRequestID))
	require.NotNil(t, model.processingPlan)
	model = runModelCommand(t, model, model.loadProcessingCoverage(*model.processingPlan, model.processingRequestID))

	model, cmd = updateModel(t, model, runeKey(']'))
	require.NotNil(t, cmd)
	model = runModelCommand(t, model, cmd)
	model = runModelCommand(t, model, model.loadProcessingCoverage(*model.processingPlan, model.processingRequestID))
	assert.Contains(t, strings.Join(model.processingLines(model.width), "\n"), "Profile: hosted (2/2)")
	assert.Contains(t, strings.Join(model.processingLines(model.width), "\n"), "Hosted provider")

	model, cmd = updateModel(t, model, runeKey('['))
	model = runModelCommand(t, model, cmd)
	model = runModelCommand(t, model, model.loadProcessingCoverage(*model.processingPlan, model.processingRequestID))
	model, cmd = updateModel(t, model, runeKey('b'))
	require.Nil(t, cmd)
	model, cmd = updateModel(t, model, key(tea.KeyEnter))
	require.NotNil(t, cmd)
	model = runModelCommand(t, model, cmd)
	model = runModelCommand(t, model, model.loadProcessingStatus(model.processingJob.ID, model.processingRequestID))
	model, cmd = updateModel(t, model, runeKey('R'))
	model = runModelCommand(t, model, cmd)
	content := strings.Join(model.processingLines(model.width), "\n")
	assert.Contains(t, content, "Processing status: completed · published")
	assert.Contains(t, content, "Sanitized Markdown rendition")
	assert.Contains(t, content, "Build: "+backend.rendition.BuildID)
	assert.Contains(t, content, "SHA-256: "+backend.rendition.SHA256)
	assert.Contains(t, content, "Warning: degraded_provenance")
}

func TestProcessingRunExposesDurableJobBeforeBlockedTerminalStatus(t *testing.T) {
	backend := newFakeBackend()
	terminalRelease := make(chan struct{})
	backend.processingTerminalRelease = terminalRelease
	model, err := New(t.Context(), backend)
	require.NoError(t, err)
	model = runModelCommand(t, model, model.loadDirectory(0, navigationInitial, model.requestID))
	model.cursor = 1
	model, cmd := updateModel(t, model, runeKey('P'))
	model = runModelCommand(t, model, cmd)
	model = runModelCommand(t, model, model.loadProcessingPlan("private", model.processingRequestID))
	require.NotNil(t, model.processingPlan)

	started := make(chan tea.Msg, 1)
	go func() {
		started <- model.startProcessing(*model.processingPlan, model.processingRequestID, true)()
	}()
	var startMsg tea.Msg
	select {
	case startMsg = <-started:
	case <-time.After(250 * time.Millisecond):
		close(terminalRelease)
		<-started
		t.Fatal("the TUI did not receive the durable job while terminal status was blocked")
	}
	model, terminalCmd := updateModel(t, model, startMsg)
	require.NotNil(t, model.processingJob)
	assert.Equal(t, backend.processingJob.ID, model.processingJob.ID)
	require.NotNil(t, model.processingStatus)
	assert.Equal(t, "running", model.processingStatus.State)
	require.NotNil(t, terminalCmd)

	close(terminalRelease)
	model = runModelCommand(t, model, terminalCmd)
	require.NotNil(t, model.processingStatus)
	assert.Equal(t, "completed", model.processingStatus.State)
	assert.Equal(t, "published", model.processingStatus.Phase)
}

func TestClosingProcessingViewCancelsBlockedTerminalStatus(t *testing.T) {
	backend := newFakeBackend()
	backend.processingTerminalRelease = make(chan struct{})
	model, err := New(t.Context(), backend)
	require.NoError(t, err)
	model = runModelCommand(t, model, model.loadDirectory(0, navigationInitial, model.requestID))
	model.cursor = 1
	model, cmd := updateModel(t, model, runeKey('P'))
	model = runModelCommand(t, model, cmd)
	model = runModelCommand(t, model, model.loadProcessingPlan("private", model.processingRequestID))
	require.NotNil(t, model.processingPlan)

	startMsg := model.beginProcessing(*model.processingPlan, model.processingRequestID, true)()
	model, terminalCmd := updateModel(t, model, startMsg)
	require.NotNil(t, model.processingJob)
	require.NotNil(t, terminalCmd)
	terminal := make(chan tea.Msg, 1)
	go func() { terminal <- terminalCmd() }()
	model, _ = updateModel(t, model, key(tea.KeyEscape))
	assert.False(t, model.processingOpen)
	select {
	case msg := <-terminal:
		terminalMsg, ok := msg.(processingTerminalMsg)
		require.True(t, ok)
		require.ErrorIs(t, terminalMsg.err, context.Canceled)
	case <-time.After(250 * time.Millisecond):
		t.Fatal("closing the processing view did not cancel its blocked status stream")
	}
}

func TestProcessingBuildRequiresExplicitConsentConfirmation(t *testing.T) {
	backend := newFakeBackend()
	backend.profiles = []api.ProcessingProfileSummary{{Name: "hosted", Fingerprint: strings.Repeat("c", 64)}}
	backend.plan.Selector.Profile = "hosted"
	backend.plan.Flow = []api.ProcessingFlowHop{{
		Capability: "embedding", ProviderID: "hosted-embed", TrustBoundary: "hosted_provider", InputClasses: []string{"original_file"},
	}}
	model, err := New(t.Context(), backend)
	require.NoError(t, err)
	model = runModelCommand(t, model, model.loadDirectory(0, navigationInitial, model.requestID))
	model.width, model.height, model.cursor = 100, 30, 1
	model, cmd := updateModel(t, model, runeKey('P'))
	model = runModelCommand(t, model, cmd)
	model = runModelCommand(t, model, model.loadProcessingPlan("hosted", model.processingRequestID))
	model = runModelCommand(t, model, model.loadProcessingCoverage(*model.processingPlan, model.processingRequestID))

	model, cmd = updateModel(t, model, runeKey('b'))
	require.Nil(t, cmd, "initial build keypress must not start a provider operation")
	assert.Empty(t, backend.processingStarts)
	assert.Contains(t, model.View().Content, "Confirm processing consent")
	assert.Contains(t, model.View().Content, "Hosted provider")
	assert.Contains(t, model.View().Content, "Retained")
}

func TestProcessingBuildConsentConfirmationCancelsWithoutStarting(t *testing.T) {
	backend := newFakeBackend()
	model, err := New(t.Context(), backend)
	require.NoError(t, err)
	model = runModelCommand(t, model, model.loadDirectory(0, navigationInitial, model.requestID))
	model.cursor = 1
	model, cmd := updateModel(t, model, runeKey('P'))
	model = runModelCommand(t, model, cmd)
	model = runModelCommand(t, model, model.loadProcessingPlan("private", model.processingRequestID))
	model = runModelCommand(t, model, model.loadProcessingCoverage(*model.processingPlan, model.processingRequestID))

	model, cmd = updateModel(t, model, runeKey('b'))
	require.Nil(t, cmd)
	model, cmd = updateModel(t, model, key(tea.KeyEscape))
	require.Nil(t, cmd)
	assert.True(t, model.processingOpen)
	assert.Empty(t, backend.processingStarts)
	assert.NotContains(t, model.View().Content, "Confirm processing consent")
}

func TestProcessingBuildConsentConfirmationStartsOnlyAfterEnter(t *testing.T) {
	backend := newFakeBackend()
	model, err := New(t.Context(), backend)
	require.NoError(t, err)
	model = runModelCommand(t, model, model.loadDirectory(0, navigationInitial, model.requestID))
	model.cursor = 1
	model, cmd := updateModel(t, model, runeKey('P'))
	model = runModelCommand(t, model, cmd)
	model = runModelCommand(t, model, model.loadProcessingPlan("private", model.processingRequestID))
	model = runModelCommand(t, model, model.loadProcessingCoverage(*model.processingPlan, model.processingRequestID))

	model, cmd = updateModel(t, model, runeKey('b'))
	require.Nil(t, cmd)
	assert.Empty(t, backend.processingStarts)
	model, cmd = updateModel(t, model, key(tea.KeyEnter))
	require.NotNil(t, cmd)
	assert.Empty(t, backend.processingStarts)
	_ = runModelCommand(t, model, cmd)
	require.Len(t, backend.processingStarts, 1)
	assert.True(t, backend.processingStarts[0].Consent)
}

func TestProcessingBuildWithActiveConsentStartsWithoutConfirmation(t *testing.T) {
	backend := newFakeBackend()
	backend.plan.ConsentRequired = false
	backend.plan.ConsentState = "active"
	model, err := New(t.Context(), backend)
	require.NoError(t, err)
	model = runModelCommand(t, model, model.loadDirectory(0, navigationInitial, model.requestID))
	model.cursor = 1
	model, cmd := updateModel(t, model, runeKey('P'))
	model = runModelCommand(t, model, cmd)
	model = runModelCommand(t, model, model.loadProcessingPlan("private", model.processingRequestID))
	model = runModelCommand(t, model, model.loadProcessingCoverage(*model.processingPlan, model.processingRequestID))

	model, cmd = updateModel(t, model, runeKey('b'))
	require.NotNil(t, cmd)
	assert.NotContains(t, model.View().Content, "Confirm processing consent")
	_ = runModelCommand(t, model, cmd)
	require.Len(t, backend.processingStarts, 1)
	assert.False(t, backend.processingStarts[0].Consent)
}

func TestProcessingRefreshInvalidatesInFlightSearch(t *testing.T) {
	backend := newFakeBackend()
	model, err := New(t.Context(), backend)
	require.NoError(t, err)
	model = runModelCommand(t, model, model.loadDirectory(0, navigationInitial, model.requestID))
	model.cursor = 1
	model, cmd := updateModel(t, model, runeKey('P'))
	model = runModelCommand(t, model, cmd)
	model = runModelCommand(t, model, model.loadProcessingPlan("private", model.processingRequestID))
	model = runModelCommand(t, model, model.loadProcessingCoverage(*model.processingPlan, model.processingRequestID))

	searchRequestID := model.processingRequestID
	model.processingSearchBusy = true
	model.processingSearchErr = errors.New("stale search error")
	model.processingSearchReport = &api.DocumentSearchReport{
		Results: []api.DocumentSearchResult{{Rank: 1, Path: "/README.txt", Excerpt: "stale retained evidence"}},
	}
	model, cmd = updateModel(t, model, runeKey('r'))
	require.NotNil(t, cmd)
	assert.False(t, model.processingSearchBusy)
	require.NoError(t, model.processingSearchErr)
	assert.Nil(t, model.processingSearchReport)

	model, _ = updateModel(t, model, processingSearchLoadedMsg{
		requestID: searchRequestID,
		report:    api.DocumentSearchReport{Results: []api.DocumentSearchResult{{Rank: 1, Path: "/README.txt", Excerpt: "late retained evidence"}}},
	})
	assert.False(t, model.processingSearchBusy)
	assert.Nil(t, model.processingSearchReport)
}

func TestProcessingProfileSwitchInvalidatesSearchState(t *testing.T) {
	backend := newFakeBackend()
	backend.profiles = append(backend.profiles, api.ProcessingProfileSummary{
		Name: "hosted", Fingerprint: strings.Repeat("c", 64), EmbeddingBindings: []string{"direct-file"},
	})
	model, err := New(t.Context(), backend)
	require.NoError(t, err)
	model = runModelCommand(t, model, model.loadDirectory(0, navigationInitial, model.requestID))
	model.cursor = 1
	model, cmd := updateModel(t, model, runeKey('P'))
	model = runModelCommand(t, model, cmd)

	searchRequestID := model.processingRequestID
	model.processingSearchBusy = true
	model.processingSearchErr = errors.New("previous profile search error")
	model.processingSearchReport = &api.DocumentSearchReport{
		Results: []api.DocumentSearchResult{{Rank: 1, Path: "/README.txt", Excerpt: "private profile result"}},
	}
	model, cmd = updateModel(t, model, runeKey(']'))
	require.NotNil(t, cmd)
	assert.Equal(t, 1, model.processingProfile)
	assert.False(t, model.processingSearchBusy)
	require.NoError(t, model.processingSearchErr)
	assert.Nil(t, model.processingSearchReport)

	model, _ = updateModel(t, model, processingSearchLoadedMsg{
		requestID: searchRequestID,
		report:    api.DocumentSearchReport{Results: []api.DocumentSearchResult{{Rank: 1, Path: "/README.txt", Excerpt: "late private result"}}},
	})
	assert.False(t, model.processingSearchBusy)
	assert.Nil(t, model.processingSearchReport)
}

func TestProcessingRefreshReloadsExistingJobStatus(t *testing.T) {
	backend := newFakeBackend()
	model, err := New(t.Context(), backend)
	require.NoError(t, err)
	model = runModelCommand(t, model, model.loadDirectory(0, navigationInitial, model.requestID))
	model.cursor = 1
	model, cmd := updateModel(t, model, runeKey('P'))
	model = runModelCommand(t, model, cmd)
	model = runModelCommand(t, model, model.loadProcessingPlan("private", model.processingRequestID))
	model = runModelCommand(t, model, model.loadProcessingCoverage(*model.processingPlan, model.processingRequestID))

	job := backend.processingJob
	model.processingJob = &job
	model.processingStatus = &api.ProcessingStatus{JobID: job.ID, State: "running", Phase: "rendering"}
	backend.processingStatus = api.ProcessingStatus{JobID: job.ID, State: "completed", Phase: "published"}
	model, cmd = updateModel(t, model, runeKey('r'))
	require.NotNil(t, cmd)
	require.NotNil(t, model.processingJob)
	assert.Equal(t, job.ID, model.processingJob.ID)

	model = runModelCommand(t, model, cmd)
	require.NotNil(t, model.processingStatus)
	assert.Equal(t, "completed", model.processingStatus.State)
	assert.Equal(t, "published", model.processingStatus.Phase)
}

func TestProcessingViewScrollsToBoundedSearchResults(t *testing.T) {
	backend := newFakeBackend()
	backend.processingSearch = api.DocumentSearchReport{
		ActualMode: "lexical", Coverage: api.DocumentSearchCoverage{State: "complete"},
		Results: []api.DocumentSearchResult{{Rank: 1, Path: "/README.txt", Excerpt: "Synthetic retained evidence", Evidence: []api.DocumentEvidenceReference{{Kind: "lexical_segment"}}}},
	}
	model, err := New(t.Context(), backend)
	require.NoError(t, err)
	model = runModelCommand(t, model, model.loadDirectory(0, navigationInitial, model.requestID))
	model.width, model.height = 100, 12
	model.cursor = 1
	model, cmd := updateModel(t, model, runeKey('P'))
	model = runModelCommand(t, model, cmd)
	model = runModelCommand(t, model, model.loadProcessingPlan("private", model.processingRequestID))
	require.NotNil(t, model.processingPlan)
	model = runModelCommand(t, model, model.loadProcessingCoverage(*model.processingPlan, model.processingRequestID))
	model, cmd = updateModel(t, model, runeKey('/'))
	require.NotNil(t, cmd)
	model.searchInput.SetValue("synthetic")
	model, cmd = updateModel(t, model, key(tea.KeyEnter))
	model = runModelCommand(t, model, cmd)

	model, _ = updateModel(t, model, key(tea.KeyEnd))
	assert.Positive(t, model.processingOffset)
	assert.Contains(t, model.View().Content, "Synthetic retained evidence")
}

func TestProcessingSearchRestoresDocumentSearchPromptOnClose(t *testing.T) {
	backend := newFakeBackend()
	model, err := New(t.Context(), backend)
	require.NoError(t, err)
	model = runModelCommand(t, model, model.loadDirectory(0, navigationInitial, model.requestID))
	model.cursor = 1
	model, cmd := updateModel(t, model, runeKey('P'))
	model = runModelCommand(t, model, cmd)
	model = runModelCommand(t, model, model.loadProcessingPlan("private", model.processingRequestID))
	model, _ = updateModel(t, model, runeKey('/'))
	model, _ = updateModel(t, model, key(tea.KeyEscape))
	model, _ = updateModel(t, model, key(tea.KeyEscape))
	model, _ = updateModel(t, model, runeKey('/'))
	assert.Equal(t, "search names and extracted text", model.searchInput.Placeholder)
}

func TestClosingDetailInvalidatesDelayedTags(t *testing.T) {
	model, err := New(t.Context(), newFakeBackend())
	require.NoError(t, err)
	model = runModelCommand(t, model, model.loadDirectory(0, navigationInitial, model.requestID))
	model.cursor = 1

	model, delayed := updateModel(t, model, key(tea.KeyEnter))
	require.NotNil(t, delayed)
	pendingRequestID := model.detailRequestID
	model, _ = updateModel(t, model, key(tea.KeyEscape))
	assert.False(t, model.detailOpen)
	assert.Greater(t, model.detailRequestID, pendingRequestID)

	model = runModelCommand(t, model, delayed)
	assert.False(t, model.detailOpen)
	assert.Empty(t, model.detailTags)
}

func TestDetailSnapshotSurvivesDelayedRefresh(t *testing.T) {
	backend := newFakeBackend()
	model, err := New(t.Context(), backend)
	require.NoError(t, err)
	model = runModelCommand(t, model, model.loadDirectory(0, navigationInitial, model.requestID))
	model.cursor = 1

	model, delayedTags := updateModel(t, model, key(tea.KeyEnter))
	require.NotNil(t, delayedTags)
	replacement := api.Node{
		ID: 10, ParentID: new(int64(1)), Kind: "file", Name: "replacement.txt",
		Revision: 1, CurrentVersionID: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa",
		BlobHash: strings.Repeat("f", 64), Size: 99, ModifiedAt: "2026-07-28T12:00:00Z",
	}
	model, _ = updateModel(t, model, directoryLoadedMsg{
		requestID: model.requestID,
		kind:      navigationRefresh,
		directory: model.directory,
		page: api.NodePage{
			Directory: model.directory,
			Items:     []api.Node{replacement},
			Total:     1,
			Limit:     maxBrowserItems,
		},
	})
	require.Equal(t, int64(10), model.rows[0].node.ID)

	model = runModelCommand(t, model, delayedTags)
	detail := strings.Join(model.expandedDetailLines(120), "\n")
	assert.Contains(t, detail, `Path: "/README.txt"`)
	assert.Contains(t, detail, strings.Repeat("a", 64))
	assert.Contains(t, detail, `"reviewed"`)
	assert.NotContains(t, detail, "replacement.txt")
	assert.NotContains(t, detail, strings.Repeat("f", 64))
}

func TestDetailRejectsTagsAfterNodeRevisionChanges(t *testing.T) {
	backend := newFakeBackend()
	model, err := New(t.Context(), backend)
	require.NoError(t, err)
	model = runModelCommand(t, model, model.loadDirectory(0, navigationInitial, model.requestID))
	model.cursor = 1

	model, delayedTags := updateModel(t, model, key(tea.KeyEnter))
	require.NotNil(t, delayedTags)
	changed := backend.nodes["/README.txt"]
	changed.Revision++
	backend.nodes["/README.txt"] = changed

	model = runModelCommand(t, model, delayedTags)
	require.ErrorIs(t, model.detailTagsErr, errDetailNodeChanged)
	assert.Empty(t, model.detailTags)
	assert.Contains(t, strings.Join(model.expandedDetailLines(120), "\n"),
		"document changed while loading tags")
}

func TestJobsViewShowsLifecycleAndCompleteFailure(t *testing.T) {
	backend := newFakeBackend()
	model, err := New(t.Context(), backend)
	require.NoError(t, err)
	model = runModelCommand(t, model, model.loadDirectory(0, navigationInitial, model.requestID))
	model.width, model.height = 100, 14

	model, cmd := updateModel(t, model, runeKey('J'))
	require.NotNil(t, cmd)
	assert.True(t, model.jobsOpen)
	assert.True(t, model.jobsLoading)
	model = runModelCommand(t, model, cmd)

	content := model.View().Content
	assert.Contains(t, content, "Daemon activity")
	assert.Contains(t, content, "1 running · 2 total")
	assert.Contains(t, content, `"text-extraction"`)
	assert.Contains(t, content, `"watch:inbox"`)
	assert.Equal(t, 1, backend.jobCalls)

	model, _ = updateModel(t, model, key(tea.KeyDown))
	model, _ = updateModel(t, model, key(tea.KeyEnter))
	assert.True(t, model.jobDetail)
	detail := strings.Join(model.jobDetailLines(model.width), "\n")
	assert.Contains(t, detail, "Status: failed")
	assert.Contains(t, detail,
		"source is temporarily unavailable; check the configured inbox path")

	model.width = 36
	for line := range strings.SplitSeq(model.View().Content, "\n") {
		assert.LessOrEqual(t, lipgloss.Width(line), model.width)
	}
}

func TestClosingJobsInvalidatesDelayedLoad(t *testing.T) {
	model, err := New(t.Context(), newFakeBackend())
	require.NoError(t, err)

	model, delayed := updateModel(t, model, runeKey('J'))
	require.NotNil(t, delayed)
	pendingRequestID := model.jobsRequestID
	model, _ = updateModel(t, model, key(tea.KeyEscape))
	assert.False(t, model.jobsOpen)
	assert.Greater(t, model.jobsRequestID, pendingRequestID)

	model = runModelCommand(t, model, delayed)
	assert.False(t, model.jobsOpen)
	assert.Empty(t, model.jobs)
}

func TestOperationsViewShowsStorageAndRecoveryPoints(t *testing.T) {
	backend := newFakeBackend()
	model, err := New(t.Context(), backend)
	require.NoError(t, err)
	model = runModelCommand(t, model, model.loadDirectory(0, navigationInitial, model.requestID))
	model.width, model.height = 100, 20

	model, cmd := updateModel(t, model, runeKey('O'))
	require.NotNil(t, cmd)
	assert.True(t, model.operationsOpen)
	assert.True(t, model.operationsInfoBusy)
	assert.True(t, model.operationsBackupBusy)
	model = runModelCommand(t, model, cmd)

	content := model.View().Content
	assert.Contains(t, content, "Vault operations")
	assert.Contains(t, content, "Loose inventory: 5 files")
	assert.Contains(t, content, "Dead packed payload: 97.7 KB")
	assert.Contains(t, content, "Backup recovery points")
	assert.Contains(t, content, `"weekly"`)
	assert.Contains(t, content, strings.Repeat("b", 64))
	assert.Less(t, strings.Index(content, `"weekly"`), strings.Index(content, `"baseline"`))
	assert.Equal(t, 1, backend.infoCalls)
	assert.Equal(t, 1, backend.backupCalls)
}

func TestOperationsStorageRendersWhileBackupsAreLoading(t *testing.T) {
	backend := newFakeBackend()
	model, err := New(t.Context(), backend)
	require.NoError(t, err)
	model.width, model.height = 100, 20

	model, _ = updateModel(t, model, runeKey('O'))
	model = runModelCommand(
		t, model, model.loadOperationsInfo(model.operationsRequestID),
	)

	content := model.View().Content
	assert.False(t, model.operationsInfoBusy)
	assert.True(t, model.operationsBackupBusy)
	assert.Contains(t, content, "Loose inventory: 5 files")
	assert.Contains(t, content, "Loading backup recovery points...")
	assert.Equal(t, 1, backend.infoCalls)
	assert.Equal(t, 0, backend.backupCalls)
}

func TestOperationsEndReachesLastLineWithNotice(t *testing.T) {
	backend := newFakeBackend()
	model, err := New(t.Context(), backend)
	require.NoError(t, err)
	model.width, model.height = 100, 10
	model.notice = "Trash changes applied"

	model, _ = updateModel(t, model, runeKey('O'))
	model = runModelCommand(
		t, model, model.loadOperationsInfo(model.operationsRequestID),
	)
	model = runModelCommand(
		t, model, model.loadOperationsBackups(model.operationsRequestID),
	)
	model, _ = updateModel(t, model, runeKey('G'))

	assert.Contains(t, model.View().Content, strings.Repeat("c", 64))
}

func TestClosingOperationsInvalidatesDelayedLoad(t *testing.T) {
	model, err := New(t.Context(), newFakeBackend())
	require.NoError(t, err)

	model, delayed := updateModel(t, model, runeKey('O'))
	require.NotNil(t, delayed)
	pendingRequestID := model.operationsRequestID
	model, _ = updateModel(t, model, key(tea.KeyEscape))
	assert.False(t, model.operationsOpen)
	assert.Greater(t, model.operationsRequestID, pendingRequestID)

	model = runModelCommand(t, model, delayed)
	assert.False(t, model.operationsOpen)
	assert.Empty(t, model.operationsInfo.VaultID)
	assert.Empty(t, model.operationsSnapshots)
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
	assert.Contains(t, model.renderTitleBar(), "RECOVERABLE")
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
