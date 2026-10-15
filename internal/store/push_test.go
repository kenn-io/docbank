package store

import (
	"bytes"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPushPreservesIndependentEditAndPortableCursor(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	source := PushSource{Name: "laptop", Ref: "notes.txt"}
	first, outcome, err := s.AcceptPush(ctx, source, PushContent{RunID: testRunID(t), ParentPath: "/", Name: "notes.txt", Hash: fakeHash("a1"), Size: 3, MIMEType: "text/plain"})
	require.NoError(t, err)
	require.Equal(t, "added", outcome)
	edited, _, err := s.ReplaceContent(ctx, first.ID, first.Revision, fakeHash("b2"), 4, "text/plain")
	require.NoError(t, err)
	same, outcome, err := s.AcceptPush(ctx, source, PushContent{RunID: testRunID(t), ParentPath: "/", Name: "notes.txt", Hash: fakeHash("a1"), Size: 3, MIMEType: "text/plain"})
	require.NoError(t, err)
	assert.Equal(t, "skipped", outcome)
	assert.Equal(t, edited.CurrentVersionID, same.CurrentVersionID)
	assert.Equal(t, edited.Revision, same.Revision)
	var snapshot bytes.Buffer
	require.NoError(t, s.ExportMetadata(ctx, &snapshot))
	restored := newTestStore(t)
	require.NoError(t, restored.ImportMetadata(ctx, bytes.NewReader(snapshot.Bytes())))
	require.NoError(t, restored.ValidateMetadata(ctx))
	state := requirePushCursor(t, restored, source)
	assert.Equal(t, fakeHash("a1"), state.Hash)
	assert.Equal(t, edited.CurrentVersionID, requireNodeByID(t, restored, state.NodeID).CurrentVersionID)
	changed, outcome, err := restored.AcceptPush(ctx, source, PushContent{RunID: testRunID(t), ParentPath: "/", Name: "notes.txt", Hash: fakeHash("c3"), Size: 5, MIMEType: "text/plain"})
	require.NoError(t, err)
	assert.Equal(t, "updated", outcome)
	assert.Equal(t, first.ID, changed.ID)
	assert.NotEqual(t, edited.CurrentVersionID, changed.CurrentVersionID)
}

func TestPushPreservesAcceptedCursorAfterPruningBoundVersion(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	source := PushSource{Name: "laptop", Ref: "notes.txt"}
	firstHash := fakeHash("f001")
	acceptedHash := fakeHash("f002")
	manualHash := fakeHash("f003")

	_, _, err := s.AcceptPush(ctx, source, PushContent{RunID: testRunID(t), ParentPath: "/", Name: "notes.txt", Hash: firstHash, Size: 5, MIMEType: "text/plain"})
	require.NoError(t, err)
	node, _, err := s.AcceptPush(ctx, source, PushContent{RunID: testRunID(t), ParentPath: "/", Name: "notes.txt", Hash: acceptedHash, Size: 6, MIMEType: "text/plain"})
	require.NoError(t, err)
	acceptedVersion := node.CurrentVersionID
	node, _, err = s.ReplaceContent(ctx, node.ID, node.Revision, manualHash, 7, "text/plain")
	require.NoError(t, err)

	_, err = s.PruneContentVersions(ctx, node.ID, node.Revision,
		VersionPruneSelector{VersionIDs: []string{acceptedVersion}}, true)
	require.NoError(t, err)
	_, err = s.ContentVersionByID(ctx, acceptedVersion)
	require.ErrorIs(t, err, ErrNotFound)

	state := requirePushCursor(t, s, source)
	assert.Equal(t, acceptedHash, state.Hash, "source cursor survives pruning its bound content version")
	assert.Equal(t, manualHash, requireNodeByID(t, s, state.NodeID).BlobHash)

	unchanged, outcome, err := s.AcceptPush(ctx, source, PushContent{RunID: testRunID(t), ParentPath: "/", Name: "notes.txt", Hash: acceptedHash, Size: 6, MIMEType: "text/plain"})
	require.NoError(t, err)
	assert.Equal(t, "skipped", outcome)
	assert.Equal(t, manualHash, unchanged.BlobHash, "unchanged source bytes must preserve a later manual edit")
	assert.NoError(t, s.ValidateMetadata(ctx), "pruned push provenance remains valid backup metadata")
}

func TestPushRollsBackCollisionAndKeepsMonotonicObservation(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	source := PushSource{Name: "laptop", Ref: "file.txt"}
	_, _, err := s.AcceptPush(ctx, source, PushContent{RunID: testRunID(t), ParentPath: "/", Name: "file.txt", Hash: fakeHash("a1"), Size: 3, MIMEType: "text/plain"})
	require.NoError(t, err)
	other := source
	other.Name = "other"
	_, _, err = s.AcceptPush(ctx, other, PushContent{RunID: testRunID(t), ParentPath: "/", Name: "file.txt", Hash: fakeHash("b2"), Size: 4, MIMEType: "text/plain"})
	require.ErrorIs(t, err, ErrExists)
	_, found := findPushCursor(t, s, other)
	assert.False(t, found, "a refused source has no cursor")
	future := time.Now().Add(time.Hour).UTC().Format(timestampLayout)
	source.Ref = "future.txt"
	run, err := s.BeginIngest(ctx, "push", source.Name)
	require.NoError(t, err)
	run.record.StartedAt = future
	first, err := s.IngestFileExact(ctx, run, s.RootID(), "future.txt", fakeHash("a1"), 3, "text/plain", source.Ref, "")
	require.NoError(t, err)
	changed, _, err := s.AcceptPush(ctx, source, PushContent{RunID: testRunID(t), ParentPath: "/", Name: "file.txt", Hash: fakeHash("c3"), Size: 5, MIMEType: "text/plain"})
	require.NoError(t, err)
	require.Equal(t, first.ID, changed.ID)
	state := requirePushCursor(t, s, source)
	var acceptedAt string
	require.NoError(t, s.db.QueryRowContext(ctx, `SELECT accepted_at FROM push_sources WHERE push_name=? AND source_ref=?`,
		source.Name, source.Ref).Scan(&acceptedAt))
	assert.Greater(t, acceptedAt, future)
	assert.Equal(t, fakeHash("c3"), state.Hash)
}

func TestPushTakesOverRestoredWatchCursorAndPreservesIndependentEdit(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	seedInitialAuditAuthority(t, s, s.RootID())
	ctx := t.Context()
	run, err := s.BeginIngest(ctx, "watch", "laptop")
	require.NoError(t, err)
	node, err := s.IngestFileExact(ctx, run, s.RootID(), "notes.txt", fakeHash("a1"), 3, "text/plain", "nested/notes.txt", "")
	require.NoError(t, err)
	watched, _, _, err := s.SyncWatchedContent(ctx, "laptop", "nested/notes.txt", fakeHash("b2"), 4, "text/plain")
	require.NoError(t, err)
	edited, _, err := s.ReplaceContent(ctx, watched.ID, watched.Revision, fakeHash("c3"), 5, "text/plain")
	require.NoError(t, err)
	var snapshot bytes.Buffer
	require.NoError(t, s.ExportMetadata(ctx, &snapshot))
	restored := newTestStore(t)
	require.NoError(t, restored.ImportMetadata(ctx, bytes.NewReader(snapshot.Bytes())))
	source := PushSource{Name: "laptop", Ref: "nested/notes.txt"}
	state := requirePushCursor(t, restored, source)
	assert.Equal(t, fakeHash("b2"), state.Hash, "take over the watch's last accepted bytes, not the current head")
	assert.Equal(t, edited.CurrentVersionID, requireNodeByID(t, restored, state.NodeID).CurrentVersionID)
	same, outcome, err := restored.AcceptPush(ctx, source, PushContent{RunID: testRunID(t), ParentPath: "/", Name: "notes.txt", Hash: fakeHash("b2"), Size: 4, MIMEType: "text/plain"})
	require.NoError(t, err)
	assert.Equal(t, "skipped", outcome)
	assert.Equal(t, edited.Revision, same.Revision)
	changed, outcome, err := restored.AcceptPush(ctx, source, PushContent{RunID: testRunID(t), ParentPath: "/", Name: "notes.txt", Hash: fakeHash("d4"), Size: 6, MIMEType: "text/plain"})
	require.NoError(t, err)
	assert.Equal(t, "updated", outcome)
	assert.Equal(t, node.ID, changed.ID)
	snapshot.Reset()
	require.NoError(t, restored.ExportMetadata(ctx, &snapshot))
	after := newTestStore(t)
	require.NoError(t, after.ImportMetadata(ctx, bytes.NewReader(snapshot.Bytes())))
	require.NoError(t, after.ValidateMetadata(ctx))
	state = requirePushCursor(t, after, source)
	assert.Equal(t, fakeHash("d4"), state.Hash, "push observations take precedence over the old watch cursor")
	assert.Equal(t, node.ID, state.NodeID)
}

func TestPushWatchTakeoverRefusesTrash(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	run, err := s.BeginIngest(t.Context(), "watch", "laptop")
	require.NoError(t, err)
	node, err := s.IngestFileExact(t.Context(), run, s.RootID(), "file.txt", fakeHash("a1"), 3, "text/plain", "file.txt", "")
	require.NoError(t, err)
	_, _, err = s.Trash(t.Context(), node.ID, node.Revision)
	require.NoError(t, err)
	source := PushSource{Name: "laptop", Ref: "file.txt"}
	assert.True(t, requirePushCursor(t, s, source).Trashed)
	_, _, err = s.AcceptPush(t.Context(), source, PushContent{RunID: testRunID(t), ParentPath: "/", Name: "file.txt", Hash: fakeHash("b2"), Size: 4, MIMEType: "text/plain"})
	require.ErrorIs(t, err, ErrExists)
}

func TestPushMappedTrashIsAnErrorAndNotAnUnknownSource(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	source := PushSource{Name: "laptop", Ref: "file.txt"}
	node, _, err := s.AcceptPush(t.Context(), source, PushContent{RunID: testRunID(t), ParentPath: "/", Name: "file.txt", Hash: fakeHash("a1"), Size: 3, MIMEType: "text/plain"})
	require.NoError(t, err)
	_, _, err = s.Trash(t.Context(), node.ID, node.Revision)
	require.NoError(t, err)
	assert.True(t, requirePushCursor(t, s, source).Trashed)
	_, _, err = s.AcceptPush(t.Context(), source, PushContent{RunID: testRunID(t), ParentPath: "/", Name: "file.txt", Hash: fakeHash("b2"), Size: 4, MIMEType: "text/plain"})
	require.ErrorIs(t, err, ErrExists)
}

func TestPushConcurrentRetriesResolveOneNode(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	source := PushSource{Name: "laptop", Ref: "file.txt"}
	type result struct {
		node Node
		err  error
	}
	outcomes := make(chan result, 8)
	runID := testRunID(t)
	for range 8 {
		go func() {
			node, _, err := s.AcceptPush(t.Context(), source, PushContent{RunID: runID, ParentPath: "/", Name: "file.txt", Hash: fakeHash("a1"), Size: 3, MIMEType: "text/plain"})
			outcomes <- result{node: node, err: err}
		}()
	}
	var nodeID int64
	for range 8 {
		outcome := <-outcomes
		require.NoError(t, outcome.err)
		if nodeID == 0 {
			nodeID = outcome.node.ID
		}
		assert.Equal(t, nodeID, outcome.node.ID)
	}
	state := requirePushCursor(t, s, source)
	assert.Equal(t, nodeID, state.NodeID)
}

func TestPushCreatesDestinationWithNewNodeOnly(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	source := PushSource{Name: "laptop", Ref: "a/b/notes.txt"}
	node, outcome, err := s.AcceptPush(ctx, source, PushContent{RunID: testRunID(t), ParentPath: "/archive/a/b", Name: "notes.txt", Hash: fakeHash("a1"), Size: 3, MIMEType: "text/plain"})
	require.NoError(t, err)
	assert.Equal(t, "added", outcome)
	resolved, err := s.NodeByPath(ctx, "/archive/a/b/notes.txt")
	require.NoError(t, err)
	assert.Equal(t, node.ID, resolved.ID)

	moved, err := s.Mkdir(ctx, s.RootID(), "moved")
	require.NoError(t, err)
	node, _, err = s.Move(ctx, node.ID, moved.ID, "renamed.txt", node.Revision)
	require.NoError(t, err)
	oldParent, err := s.NodeByPath(ctx, "/archive/a/b")
	require.NoError(t, err)
	_, _, err = s.Trash(ctx, oldParent.ID, oldParent.Revision)
	require.NoError(t, err)
	changed, outcome, err := s.AcceptPush(ctx, source, PushContent{RunID: testRunID(t), ParentPath: "/archive/a/b", Name: "notes.txt", Hash: fakeHash("b2"), Size: 4, MIMEType: "text/plain"})
	require.NoError(t, err, "an existing source ignores its stale destination")
	assert.Equal(t, "updated", outcome)
	assert.Equal(t, node.ID, changed.ID)
	require.NotNil(t, changed.ParentID)
	assert.Equal(t, moved.ID, *changed.ParentID)

	_, _, err = s.AcceptPush(ctx, source, PushContent{RunID: testRunID(t), ParentPath: "relative", Name: "notes.txt", Hash: fakeHash("b2"), Size: 4, MIMEType: "text/plain"})
	require.ErrorContains(t, err, "absolute virtual path")
}

func testRunID(t *testing.T) string {
	t.Helper()
	id, err := newUUIDv4()
	require.NoError(t, err)
	return id
}

func findPushCursor(t *testing.T, s *Store, source PushSource) (PushCursor, bool) {
	t.Helper()
	cursors, err := s.PushCursors(t.Context(), source.Name, "", MaxPushCursorPage)
	require.NoError(t, err)
	for _, cursor := range cursors {
		if cursor.Ref == source.Ref {
			return cursor, true
		}
	}
	return PushCursor{}, false
}

func requirePushCursor(t *testing.T, s *Store, source PushSource) PushCursor {
	t.Helper()
	cursor, found := findPushCursor(t, s, source)
	require.True(t, found, "no cursor for %s/%s", source.Name, source.Ref)
	return cursor
}

func requireNodeByID(t *testing.T, s *Store, id int64) Node {
	t.Helper()
	node, err := s.NodeByID(t.Context(), id)
	require.NoError(t, err)
	return node
}

func TestPushIdenticalSourcesGetSeparateDocuments(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	run, err := s.BeginIngest(ctx, "cli", "unrelated import")
	require.NoError(t, err)
	template, err := s.IngestFileExact(ctx, run, s.RootID(), "template.txt", fakeHash("a1"), 3, "text/plain", "template.txt", "")
	require.NoError(t, err)
	runID := testRunID(t)
	first := PushSource{Name: "laptop", Ref: "a/contract.txt"}
	firstNode, outcome, err := s.AcceptPush(ctx, first, PushContent{RunID: runID, ParentPath: "/laptop/a", Name: "contract.txt", Hash: fakeHash("a1"), Size: 3, MIMEType: "text/plain"})
	require.NoError(t, err)
	assert.Equal(t, "added", outcome)
	second := PushSource{Name: "laptop", Ref: "b/contract.txt"}
	secondNode, outcome, err := s.AcceptPush(ctx, second, PushContent{RunID: runID, ParentPath: "/laptop/b", Name: "contract.txt", Hash: fakeHash("a1"), Size: 3, MIMEType: "text/plain"})
	require.NoError(t, err)
	assert.Equal(t, "added", outcome)
	assert.NotEqual(t, firstNode.ID, secondNode.ID)
	assert.NotEqual(t, template.ID, firstNode.ID)

	_, _, err = s.AcceptPush(ctx, first, PushContent{RunID: testRunID(t), ParentPath: "/laptop/a", Name: "contract.txt", Hash: fakeHash("b2"), Size: 4, MIMEType: "text/plain"})
	require.NoError(t, err)
	assert.Equal(t, fakeHash("a1"), requireNodeByID(t, s, secondNode.ID).BlobHash, "an edit to one source never versions another")
	assert.Equal(t, template.CurrentVersionID, requireNodeByID(t, s, template.ID).CurrentVersionID)
	require.NoError(t, s.ValidateMetadata(ctx))
}

func TestPushRunGroupsOneScanIntoOneCollection(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	runID := testRunID(t)
	for _, name := range []string{"a.txt", "b.txt", "c.txt"} {
		source := PushSource{Name: "laptop", Ref: name}
		_, _, err := s.AcceptPush(ctx, source, PushContent{RunID: runID, ParentPath: "/archive", Name: name, Hash: fakeHash(name[:1] + "1"), Size: 3, MIMEType: "text/plain"})
		require.NoError(t, err)
	}
	_, total, err := s.Collections(ctx, 100, 0)
	require.NoError(t, err)
	assert.Equal(t, 1, total, "one scan is one collection")

	source := PushSource{Name: "laptop", Ref: "a.txt"}
	_, outcome, err := s.AcceptPush(ctx, source, PushContent{RunID: runID, ParentPath: "/archive", Name: "a.txt", Hash: fakeHash("a2"), Size: 4, MIMEType: "text/plain"})
	require.NoError(t, err, "a second observation in one run gets its own ordered run")
	assert.Equal(t, "updated", outcome)
	require.NoError(t, s.ValidateMetadata(ctx))
	var snapshot bytes.Buffer
	require.NoError(t, s.ExportMetadata(ctx, &snapshot))
	restored := newTestStore(t)
	require.NoError(t, restored.ImportMetadata(ctx, bytes.NewReader(snapshot.Bytes())))
	require.NoError(t, restored.ValidateMetadata(ctx))

	other := PushSource{Name: "desktop", Ref: "d.txt"}
	_, _, err = s.AcceptPush(ctx, other, PushContent{RunID: runID, ParentPath: "/desktop", Name: "d.txt", Hash: fakeHash("d1"), Size: 3, MIMEType: "text/plain"})
	require.ErrorIs(t, err, ErrExists, "a run belongs to one push name")
	_, _, err = s.AcceptPush(ctx, other, PushContent{RunID: "not-a-uuid", ParentPath: "/desktop", Name: "d.txt", Hash: fakeHash("d1"), Size: 3, MIMEType: "text/plain"})
	require.ErrorContains(t, err, "push run ID")
}

func TestPushCursorsPageInReferenceOrderAndIncludeAdoptedWatchCursors(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	run, err := s.BeginIngest(ctx, "watch", "laptop")
	require.NoError(t, err)
	watched, err := s.IngestFileExact(ctx, run, s.RootID(), "b.txt", fakeHash("b1"), 3, "text/plain", "b.txt", "")
	require.NoError(t, err)
	_, err = s.IngestFileExact(ctx, run, s.RootID(), "c.txt", fakeHash("c1"), 3, "text/plain", "c.txt", "")
	require.NoError(t, err)
	runID := testRunID(t)
	for _, name := range []string{"a.txt", "d.txt"} {
		_, _, err := s.AcceptPush(ctx, PushSource{Name: "laptop", Ref: name}, PushContent{RunID: runID, ParentPath: "/archive", Name: name, Hash: fakeHash(name[:1] + "1"), Size: 3, MIMEType: "text/plain"})
		require.NoError(t, err)
	}
	_, _, err = s.AcceptPush(ctx, PushSource{Name: "laptop", Ref: "c.txt"}, PushContent{RunID: testRunID(t), ParentPath: "/", Name: "c.txt", Hash: fakeHash("c2"), Size: 4, MIMEType: "text/plain"})
	require.NoError(t, err)
	_, _, err = s.Trash(ctx, watched.ID, watched.Revision)
	require.NoError(t, err)

	var refs, hashes []string
	var trashed []bool
	after := ""
	for {
		page, err := s.PushCursors(ctx, "laptop", after, 2)
		require.NoError(t, err)
		if len(page) == 0 {
			break
		}
		for _, cursor := range page {
			refs, hashes, trashed = append(refs, cursor.Ref), append(hashes, cursor.Hash), append(trashed, cursor.Trashed)
		}
		after = page[len(page)-1].Ref
	}
	assert.Equal(t, []string{"a.txt", "b.txt", "c.txt", "d.txt"}, refs)
	assert.Equal(t, []string{fakeHash("a1"), fakeHash("b1"), fakeHash("c2"), fakeHash("d1")}, hashes,
		"a push observation takes precedence over the watch cursor it adopted")
	assert.Equal(t, []bool{false, true, false, false}, trashed)
	_, err = s.PushCursors(ctx, "laptop", "", MaxPushCursorPage+1)
	require.Error(t, err)
	empty, err := s.PushCursors(ctx, "nobody", "", 10)
	require.NoError(t, err)
	assert.Empty(t, empty)
}

func TestPushAuditReplaysASharedRunAndAnUpdate(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	seedInitialAuditAuthority(t, s, s.RootID())
	ctx := t.Context()
	runID := testRunID(t)
	for name, seed := range map[string]string{"first.txt": "a1", "second.txt": "b1"} {
		_, _, err := s.AcceptPush(ctx, PushSource{Name: "laptop", Ref: name}, PushContent{RunID: runID, ParentPath: "/archive", Name: name, Hash: fakeHash(seed), Size: 3, MIMEType: "text/plain"})
		require.NoError(t, err)
	}
	source := PushSource{Name: "laptop", Ref: "second.txt"}
	_, outcome, err := s.AcceptPush(ctx, source, PushContent{RunID: testRunID(t), ParentPath: "/archive", Name: "second.txt", Hash: fakeHash("b2"), Size: 4, MIMEType: "text/plain"})
	require.NoError(t, err)
	assert.Equal(t, "updated", outcome)
	require.NoError(t, s.ValidateMetadata(ctx))
	var snapshot bytes.Buffer
	require.NoError(t, s.ExportMetadata(ctx, &snapshot))
	restored := newTestStore(t)
	require.NoError(t, restored.ImportMetadata(ctx, bytes.NewReader(snapshot.Bytes())))
	require.NoError(t, restored.ValidateMetadata(ctx))
	assert.Equal(t, fakeHash("b2"), requirePushCursor(t, restored, source).Hash)
}
