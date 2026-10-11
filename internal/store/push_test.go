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
	source := PushSource{Name: "laptop", Ref: "notes.txt", Duplicates: "create"}
	first, outcome, err := s.AcceptPush(ctx, source, PushContent{ParentPath: "/", Name: "notes.txt", Hash: fakeHash("a1"), Size: 3, MIMEType: "text/plain"})
	require.NoError(t, err)
	require.Equal(t, "added", outcome)
	edited, _, err := s.ReplaceContent(ctx, first.ID, first.Revision, fakeHash("b2"), 4, "text/plain")
	require.NoError(t, err)
	same, outcome, err := s.AcceptPush(ctx, source, PushContent{ParentPath: "/", Name: "notes.txt", Hash: fakeHash("a1"), Size: 3, MIMEType: "text/plain"})
	require.NoError(t, err)
	assert.Equal(t, "skipped", outcome)
	assert.Equal(t, edited.CurrentVersionID, same.CurrentVersionID)
	assert.Equal(t, edited.Revision, same.Revision)
	var snapshot bytes.Buffer
	require.NoError(t, s.ExportMetadata(ctx, &snapshot))
	restored := newTestStore(t)
	require.NoError(t, restored.ImportMetadata(ctx, bytes.NewReader(snapshot.Bytes())))
	require.NoError(t, restored.ValidateMetadata(ctx))
	state, err := restored.PushSourceState(ctx, source)
	require.NoError(t, err)
	assert.Equal(t, fakeHash("a1"), state.Hash)
	assert.Equal(t, edited.CurrentVersionID, state.Node.CurrentVersionID)
	changed, outcome, err := restored.AcceptPush(ctx, source, PushContent{ParentPath: "/", Name: "notes.txt", Hash: fakeHash("c3"), Size: 5, MIMEType: "text/plain"})
	require.NoError(t, err)
	assert.Equal(t, "updated", outcome)
	assert.Equal(t, first.ID, changed.ID)
	assert.NotEqual(t, edited.CurrentVersionID, changed.CurrentVersionID)
}

func TestPushPreservesAcceptedCursorAfterPruningBoundVersion(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	source := PushSource{Name: "laptop", Ref: "notes.txt", Duplicates: "create"}
	firstHash := fakeHash("f001")
	acceptedHash := fakeHash("f002")
	manualHash := fakeHash("f003")

	_, _, err := s.AcceptPush(ctx, source, PushContent{ParentPath: "/", Name: "notes.txt", Hash: firstHash, Size: 5, MIMEType: "text/plain"})
	require.NoError(t, err)
	node, _, err := s.AcceptPush(ctx, source, PushContent{ParentPath: "/", Name: "notes.txt", Hash: acceptedHash, Size: 6, MIMEType: "text/plain"})
	require.NoError(t, err)
	acceptedVersion := node.CurrentVersionID
	node, _, err = s.ReplaceContent(ctx, node.ID, node.Revision, manualHash, 7, "text/plain")
	require.NoError(t, err)

	_, err = s.PruneContentVersions(ctx, node.ID, node.Revision,
		VersionPruneSelector{VersionIDs: []string{acceptedVersion}}, true)
	require.NoError(t, err)
	_, err = s.ContentVersionByID(ctx, acceptedVersion)
	require.ErrorIs(t, err, ErrNotFound)

	state, err := s.PushSourceState(ctx, source)
	require.NoError(t, err)
	assert.Equal(t, acceptedHash, state.Hash, "source cursor survives pruning its bound content version")
	assert.Equal(t, manualHash, state.Node.BlobHash)

	unchanged, outcome, err := s.AcceptPush(ctx, source, PushContent{ParentPath: "/", Name: "notes.txt", Hash: acceptedHash, Size: 6, MIMEType: "text/plain"})
	require.NoError(t, err)
	assert.Equal(t, "skipped", outcome)
	assert.Equal(t, manualHash, unchanged.BlobHash, "unchanged source bytes must preserve a later manual edit")
	assert.NoError(t, s.ValidateMetadata(ctx), "pruned push provenance remains valid backup metadata")
}

func TestPushLinkedSourcesRetainSeparateCursors(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	source := PushSource{Name: "laptop", Ref: "first.txt", Duplicates: "link"}
	first, _, err := s.AcceptPush(ctx, source, PushContent{ParentPath: "/", Name: "first.txt", Hash: fakeHash("a1"), Size: 3, MIMEType: "text/plain"})
	require.NoError(t, err)
	secondSource := source
	secondSource.Ref = "second.txt"
	linked, outcome, err := s.AcceptPush(ctx, secondSource, PushContent{ParentPath: "/", Name: "second.txt", Hash: fakeHash("a1"), Size: 3, MIMEType: "text/plain"})
	require.NoError(t, err)
	assert.Equal(t, "linked", outcome)
	require.Equal(t, first.ID, linked.ID)
	updated, _, err := s.AcceptPush(ctx, source, PushContent{ParentPath: "/", Name: "first.txt", Hash: fakeHash("b2"), Size: 4, MIMEType: "text/plain"})
	require.NoError(t, err)
	unchanged, outcome, err := s.AcceptPush(ctx, secondSource, PushContent{ParentPath: "/", Name: "second.txt", Hash: fakeHash("a1"), Size: 3, MIMEType: "text/plain"})
	require.NoError(t, err)
	assert.Equal(t, "skipped", outcome)
	assert.Equal(t, updated.CurrentVersionID, unchanged.CurrentVersionID)
	var snapshot bytes.Buffer
	require.NoError(t, s.ExportMetadata(ctx, &snapshot))
	restored := newTestStore(t)
	require.NoError(t, restored.ImportMetadata(ctx, bytes.NewReader(snapshot.Bytes())))
	require.NoError(t, restored.ValidateMetadata(ctx))
	state, err := restored.PushSourceState(ctx, secondSource)
	require.NoError(t, err)
	assert.Equal(t, fakeHash("a1"), state.Hash)
}

func TestPushRollsBackCollisionAndKeepsMonotonicObservation(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	source := PushSource{Name: "laptop", Ref: "file.txt", Duplicates: "create"}
	_, _, err := s.AcceptPush(ctx, source, PushContent{ParentPath: "/", Name: "file.txt", Hash: fakeHash("a1"), Size: 3, MIMEType: "text/plain"})
	require.NoError(t, err)
	other := source
	other.Name = "other"
	_, _, err = s.AcceptPush(ctx, other, PushContent{ParentPath: "/", Name: "file.txt", Hash: fakeHash("b2"), Size: 4, MIMEType: "text/plain"})
	require.ErrorIs(t, err, ErrExists)
	_, err = s.PushSourceState(ctx, other)
	require.ErrorIs(t, err, ErrNotFound)
	future := time.Now().Add(time.Hour).UTC().Format(timestampLayout)
	source.Ref = "future.txt"
	run, err := s.BeginIngest(ctx, "push", source.Name)
	require.NoError(t, err)
	run.record.StartedAt = future
	first, err := s.IngestFileExact(ctx, run, s.RootID(), "future.txt", fakeHash("a1"), 3, "text/plain", source.Ref, "")
	require.NoError(t, err)
	changed, _, err := s.AcceptPush(ctx, source, PushContent{ParentPath: "/", Name: "file.txt", Hash: fakeHash("c3"), Size: 5, MIMEType: "text/plain"})
	require.NoError(t, err)
	require.Equal(t, first.ID, changed.ID)
	state, err := s.PushSourceState(ctx, source)
	require.NoError(t, err)
	assert.Greater(t, state.AcceptedAt, future)
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
	source := PushSource{Name: "laptop", Ref: "nested/notes.txt", Duplicates: "create"}
	state, err := restored.PushSourceState(ctx, source)
	require.NoError(t, err)
	assert.Equal(t, fakeHash("b2"), state.Hash, "take over the watch's last accepted bytes, not the current head")
	assert.Equal(t, edited.CurrentVersionID, state.Node.CurrentVersionID)
	same, outcome, err := restored.AcceptPush(ctx, source, PushContent{ParentPath: "/", Name: "notes.txt", Hash: fakeHash("b2"), Size: 4, MIMEType: "text/plain"})
	require.NoError(t, err)
	assert.Equal(t, "skipped", outcome)
	assert.Equal(t, edited.Revision, same.Revision)
	changed, outcome, err := restored.AcceptPush(ctx, source, PushContent{ParentPath: "/", Name: "notes.txt", Hash: fakeHash("d4"), Size: 6, MIMEType: "text/plain"})
	require.NoError(t, err)
	assert.Equal(t, "updated", outcome)
	assert.Equal(t, node.ID, changed.ID)
	snapshot.Reset()
	require.NoError(t, restored.ExportMetadata(ctx, &snapshot))
	after := newTestStore(t)
	require.NoError(t, after.ImportMetadata(ctx, bytes.NewReader(snapshot.Bytes())))
	require.NoError(t, after.ValidateMetadata(ctx))
	state, err = after.PushSourceState(ctx, source)
	require.NoError(t, err)
	assert.Equal(t, fakeHash("d4"), state.Hash, "push observations take precedence over the old watch cursor")
	assert.Equal(t, node.ID, state.Node.ID)
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
	source := PushSource{Name: "laptop", Ref: "file.txt", Duplicates: "link"}
	_, err = s.PushSourceState(t.Context(), source)
	require.ErrorIs(t, err, ErrExists)
	_, _, err = s.AcceptPush(t.Context(), source, PushContent{ParentPath: "/", Name: "file.txt", Hash: fakeHash("b2"), Size: 4, MIMEType: "text/plain"})
	require.ErrorIs(t, err, ErrExists)
}

func TestPushAuditReplaysCreateLinkAndUpdate(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	seedInitialAuditAuthority(t, s, s.RootID())
	source := PushSource{Name: "laptop", Ref: "first.txt", Duplicates: "link"}
	first, _, err := s.AcceptPush(t.Context(), source, PushContent{ParentPath: "/", Name: "first.txt", Hash: fakeHash("a1"), Size: 3, MIMEType: "text/plain"})
	require.NoError(t, err)
	source.Ref = "linked.txt"
	linked, _, err := s.AcceptPush(t.Context(), source, PushContent{ParentPath: "/", Name: "linked.txt", Hash: fakeHash("a1"), Size: 3, MIMEType: "text/plain"})
	require.NoError(t, err)
	require.Equal(t, first.ID, linked.ID)
	_, _, err = s.AcceptPush(t.Context(), source, PushContent{ParentPath: "/", Name: "linked.txt", Hash: fakeHash("b2"), Size: 4, MIMEType: "text/plain"})
	require.NoError(t, err)
	require.NoError(t, s.ValidateMetadata(t.Context()))
	var snapshot bytes.Buffer
	require.NoError(t, s.ExportMetadata(t.Context(), &snapshot))
	restored := newTestStore(t)
	require.NoError(t, restored.ImportMetadata(t.Context(), bytes.NewReader(snapshot.Bytes())))
	require.NoError(t, restored.ValidateMetadata(t.Context()))
	state, err := restored.PushSourceState(t.Context(), source)
	require.NoError(t, err)
	assert.Equal(t, fakeHash("b2"), state.Hash)
}

func TestPushMappedTrashIsAnErrorAndNotAnUnknownSource(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	source := PushSource{Name: "laptop", Ref: "file.txt", Duplicates: "link"}
	node, _, err := s.AcceptPush(t.Context(), source, PushContent{ParentPath: "/", Name: "file.txt", Hash: fakeHash("a1"), Size: 3, MIMEType: "text/plain"})
	require.NoError(t, err)
	_, _, err = s.Trash(t.Context(), node.ID, node.Revision)
	require.NoError(t, err)
	_, err = s.PushSourceState(t.Context(), source)
	require.ErrorIs(t, err, ErrExists)
	_, _, err = s.AcceptPush(t.Context(), source, PushContent{ParentPath: "/", Name: "file.txt", Hash: fakeHash("b2"), Size: 4, MIMEType: "text/plain"})
	require.ErrorIs(t, err, ErrExists)
}

func TestPushConcurrentRetriesResolveOneNode(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	source := PushSource{Name: "laptop", Ref: "file.txt", Duplicates: "create"}
	type result struct {
		node Node
		err  error
	}
	outcomes := make(chan result, 8)
	for range 8 {
		go func() {
			node, _, err := s.AcceptPush(t.Context(), source, PushContent{ParentPath: "/", Name: "file.txt", Hash: fakeHash("a1"), Size: 3, MIMEType: "text/plain"})
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
	state, err := s.PushSourceState(t.Context(), source)
	require.NoError(t, err)
	assert.Equal(t, nodeID, state.Node.ID)
}

func TestPushNeverLinksToDocumentsItDoesNotOwn(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	run, err := s.BeginIngest(ctx, "cli", "unrelated import")
	require.NoError(t, err)
	template, err := s.IngestFileExact(ctx, run, s.RootID(), "template.txt", fakeHash("a1"), 3, "text/plain", "template.txt", "")
	require.NoError(t, err)
	other := PushSource{Name: "desktop", Ref: "template.txt", Duplicates: "create"}
	otherNode, _, err := s.AcceptPush(ctx, other, PushContent{ParentPath: "/desktop", Name: "template.txt", Hash: fakeHash("a1"), Size: 3, MIMEType: "text/plain"})
	require.NoError(t, err)

	source := PushSource{Name: "laptop", Ref: "acme/contract.txt", Duplicates: "link"}
	created, outcome, err := s.AcceptPush(ctx, source, PushContent{ParentPath: "/laptop/acme", Name: "contract.txt", Hash: fakeHash("a1"), Size: 3, MIMEType: "text/plain"})
	require.NoError(t, err)
	assert.Equal(t, "added", outcome, "documents owned by another source are not duplicate targets")
	assert.NotEqual(t, template.ID, created.ID)
	assert.NotEqual(t, otherNode.ID, created.ID)
	_, _, err = s.AcceptPush(ctx, source, PushContent{ParentPath: "/laptop/acme", Name: "contract.txt", Hash: fakeHash("b2"), Size: 4, MIMEType: "text/plain"})
	require.NoError(t, err)
	for _, unrelated := range []Node{template, otherNode} {
		after, err := s.NodeByID(ctx, unrelated.ID)
		require.NoError(t, err)
		assert.Equal(t, fakeHash("a1"), after.BlobHash)
		assert.Equal(t, unrelated.CurrentVersionID, after.CurrentVersionID)
	}

	skipped := PushSource{Name: "laptop", Ref: "copy.txt", Duplicates: "skip"}
	_, outcome, err = s.AcceptPush(ctx, skipped, PushContent{ParentPath: "/laptop", Name: "copy.txt", Hash: fakeHash("a1"), Size: 3, MIMEType: "text/plain"})
	require.NoError(t, err)
	assert.Equal(t, "added", outcome, "skip applies only to this push's own documents")
	require.NoError(t, s.ValidateMetadata(ctx))
}

func TestPushLinksToDocumentsAdoptedFromItsWatch(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	run, err := s.BeginIngest(ctx, "watch", "laptop")
	require.NoError(t, err)
	watched, err := s.IngestFileExact(ctx, run, s.RootID(), "notes.txt", fakeHash("a1"), 3, "text/plain", "notes.txt", "")
	require.NoError(t, err)
	source := PushSource{Name: "laptop", Ref: "copy/notes.txt", Duplicates: "link"}
	linked, outcome, err := s.AcceptPush(ctx, source, PushContent{ParentPath: "/archive/copy", Name: "notes.txt", Hash: fakeHash("a1"), Size: 3, MIMEType: "text/plain"})
	require.NoError(t, err)
	assert.Equal(t, "linked", outcome)
	assert.Equal(t, watched.ID, linked.ID)
	_, err = s.NodeByPath(ctx, "/archive")
	require.ErrorIs(t, err, ErrNotFound, "a linked source creates no destination directories")
	require.NoError(t, s.ValidateMetadata(ctx))
}

func TestPushCreatesDestinationWithNewNodeOnly(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	source := PushSource{Name: "laptop", Ref: "a/b/notes.txt", Duplicates: "create"}
	node, outcome, err := s.AcceptPush(ctx, source, PushContent{ParentPath: "/archive/a/b", Name: "notes.txt", Hash: fakeHash("a1"), Size: 3, MIMEType: "text/plain"})
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
	changed, outcome, err := s.AcceptPush(ctx, source, PushContent{ParentPath: "/archive/a/b", Name: "notes.txt", Hash: fakeHash("b2"), Size: 4, MIMEType: "text/plain"})
	require.NoError(t, err, "an existing source ignores its stale destination")
	assert.Equal(t, "updated", outcome)
	assert.Equal(t, node.ID, changed.ID)
	require.NotNil(t, changed.ParentID)
	assert.Equal(t, moved.ID, *changed.ParentID)

	_, _, err = s.AcceptPush(ctx, source, PushContent{ParentPath: "relative", Name: "notes.txt", Hash: fakeHash("b2"), Size: 4, MIMEType: "text/plain"})
	require.ErrorContains(t, err, "absolute virtual path")
}
