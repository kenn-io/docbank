package store

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAppendNodeProvenanceAppendsAndFencesHistory(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	run, err := s.BeginIngest(ctx, "cli", "/source")
	require.NoError(t, err)
	node, err := s.IngestFileExact(ctx, run, s.RootID(), "report.txt", fakeHash("a1"),
		7, "text/plain", "/source/report.txt", "")
	require.NoError(t, err)

	mtime := "2026-08-26T12:00:00Z"
	first, err := s.AppendNodeProvenance(ctx, ProvenanceAppendInput{
		NodeID: node.ID, IfRevision: node.Revision, SourceKind: "agent",
		SourceDescription: "triage", OriginalPath: "laptop/Downloads/report.txt",
		OriginalMTime: &mtime,
	})
	require.NoError(t, err)
	assert.Equal(t, node.Revision+1, first.Node.Revision)
	assert.Equal(t, "/report.txt", first.Path)
	assert.Equal(t, "agent", first.Fact.SourceKind)
	assert.True(t, first.Fact.Active)

	second, err := s.AppendNodeProvenance(ctx, ProvenanceAppendInput{
		NodeID: node.ID, IfRevision: first.Node.Revision, SourceKind: "agent",
		SourceDescription: "reconcile", OriginalPath: "drive/report.txt",
		Supersedes: &first.Fact.Identity,
	})
	require.NoError(t, err)
	assert.Equal(t, first.Node.Revision+1, second.Node.Revision)
	assert.Equal(t, &first.Fact.Identity, second.Fact.Supersedes)

	page, err := s.NodeProvenance(ctx, node.ID, 10, 0)
	require.NoError(t, err)
	require.Len(t, page.Items, 3)
	active := make(map[string]bool, len(page.Items))
	for _, item := range page.Items {
		active[item.Identity] = item.Active
	}
	assert.True(t, active[second.Fact.Identity])
	assert.False(t, active[first.Fact.Identity])
	_, err = s.AppendNodeProvenance(ctx, ProvenanceAppendInput{
		NodeID: node.ID, IfRevision: first.Node.Revision, SourceKind: "agent",
		SourceDescription: "stale", OriginalPath: "stale",
	})
	require.ErrorIs(t, err, ErrStaleRevision)
	_, err = s.AppendNodeProvenance(ctx, ProvenanceAppendInput{
		NodeID: node.ID, IfRevision: second.Node.Revision, SourceKind: "agent",
		SourceDescription: "bad", OriginalPath: "bad", Supersedes: &first.Fact.Identity,
	})
	require.ErrorIs(t, err, ErrProvenanceMismatch)
	_, err = s.AppendNodeProvenance(ctx, ProvenanceAppendInput{
		NodeID: s.RootID(), IfRevision: 1, SourceKind: "agent",
		SourceDescription: "dir", OriginalPath: "dir",
	})
	require.ErrorIs(t, err, ErrNotFile)
	other, err := s.CreateFile(ctx, s.RootID(), "other.txt", fakeHash("b2"), 7, "text/plain")
	require.NoError(t, err)
	_, err = s.AppendNodeProvenance(ctx, ProvenanceAppendInput{
		NodeID: other.ID, IfRevision: other.Revision, SourceKind: "agent",
		SourceDescription: "cross-node", OriginalPath: "other", Supersedes: &second.Fact.Identity,
	})
	require.ErrorIs(t, err, ErrProvenanceMismatch)
	_, err = s.AppendNodeProvenance(ctx, ProvenanceAppendInput{
		NodeID: 99999, IfRevision: 1, SourceKind: "agent",
		SourceDescription: "missing", OriginalPath: "missing",
	})
	require.ErrorIs(t, err, ErrNotFound)
	require.NoError(t, s.ValidateMetadata(ctx))
}

// TestAppendNodeProvenanceWatchKindStaysEvidenceOnly pins the generic-source
// negative space: appending a fact whose caller-supplied kind is "watch"
// records the evidence string and nothing operational; no watch_sources row
// appears, because the internal ingest run is namespaced under "embedded:".
func TestAppendNodeProvenanceWatchKindStaysEvidenceOnly(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	run, err := s.BeginIngest(ctx, "cli", "/source")
	require.NoError(t, err)
	node, err := s.IngestFileExact(ctx, run, s.RootID(), "watched.txt", fakeHash("w1"),
		4, "text/plain", "/source/watched.txt", "")
	require.NoError(t, err)

	appended, err := s.AppendNodeProvenance(ctx, ProvenanceAppendInput{
		NodeID: node.ID, IfRevision: node.Revision, SourceKind: "watch",
		SourceDescription: "inbox sweep", OriginalPath: "inbox/watched.txt",
	})
	require.NoError(t, err)
	assert.Equal(t, "watch", appended.Fact.SourceKind, "the evidence string reads back verbatim")

	var watchRows int
	require.NoError(t, s.db.QueryRow(`SELECT COUNT(*) FROM watch_sources`).Scan(&watchRows))
	assert.Zero(t, watchRows, "a watch-kind fact must not create operational watch state")
}

func TestAppendNodeProvenanceRejectsMissingPredecessor(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	// CreateFile leaves the node with no provenance facts at all, so the
	// supersedes target cannot exist anywhere; the append must fail before
	// writing and leave node revision and history untouched.
	node, err := s.CreateFile(ctx, s.RootID(), "bare.txt", fakeHash("c3"), 5, "text/plain")
	require.NoError(t, err)
	missing := fakeHash("no-such-fact")
	_, err = s.AppendNodeProvenance(ctx, ProvenanceAppendInput{
		NodeID: node.ID, IfRevision: node.Revision, SourceKind: "agent",
		SourceDescription: "orphan correction", OriginalPath: "laptop/bare.txt",
		Supersedes: &missing,
	})
	require.ErrorIs(t, err, ErrProvenanceMismatch)
	require.ErrorContains(t, err, "was not found")

	refreshed, err := s.NodeByID(ctx, node.ID)
	require.NoError(t, err)
	assert.Equal(t, node.Revision, refreshed.Revision, "failed append must not advance the revision")
	page, err := s.NodeProvenance(ctx, node.ID, 10, 0)
	require.NoError(t, err)
	assert.Empty(t, page.Items, "failed append must not record a fact")
}

func TestAppendNodeProvenanceReturnsEmptyPathForTrashedNodes(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	trashRoot, err := s.CreateFile(ctx, s.RootID(), "trash-root.txt", fakeHash("a1"), 1, "text/plain")
	require.NoError(t, err)
	trashedRoot, _, err := s.Trash(ctx, trashRoot.ID, trashRoot.Revision)
	require.NoError(t, err)
	appendedRoot, err := s.AppendNodeProvenance(ctx, ProvenanceAppendInput{
		NodeID: trashedRoot.ID, IfRevision: trashedRoot.Revision, SourceKind: "agent",
		SourceDescription: "trash-root", OriginalPath: "opaque://trash-root",
	})
	require.NoError(t, err)
	assert.Empty(t, appendedRoot.Path)
	assert.NotNil(t, appendedRoot.Node.TrashedAt)

	directory, err := s.Mkdir(ctx, s.RootID(), "trash-parent")
	require.NoError(t, err)
	child, err := s.CreateFile(ctx, directory.ID, "child.txt", fakeHash("b2"), 1, "text/plain")
	require.NoError(t, err)
	directory, err = s.NodeByID(ctx, directory.ID)
	require.NoError(t, err)
	_, _, err = s.Trash(ctx, directory.ID, directory.Revision)
	require.NoError(t, err)
	trashedChild, err := s.NodeByID(ctx, child.ID)
	require.NoError(t, err)
	appendedChild, err := s.AppendNodeProvenance(ctx, ProvenanceAppendInput{
		NodeID: trashedChild.ID, IfRevision: trashedChild.Revision, SourceKind: "agent",
		SourceDescription: "trash-child", OriginalPath: "opaque://trash-child",
	})
	require.NoError(t, err)
	assert.Empty(t, appendedChild.Path)
	assert.NotNil(t, appendedChild.Node.TrashedAt)
}

func TestAppendNodeProvenanceRejectsInvalidOriginalMTime(t *testing.T) {
	s := newTestStore(t)
	node, err := s.CreateFile(t.Context(), s.RootID(), "report.txt", fakeHash("a1"), 1, "text/plain")
	require.NoError(t, err)
	for _, value := range []string{"not-a-time", "2026-08-26T14:00:00+02:00", ""} {
		_, err = s.AppendNodeProvenance(t.Context(), ProvenanceAppendInput{
			NodeID: node.ID, IfRevision: node.Revision, SourceKind: "agent",
			SourceDescription: "invalid-time", OriginalPath: "opaque://report", OriginalMTime: &value,
		})
		require.ErrorIs(t, err, ErrInvalidProvenanceTime)
	}
}

func TestAppendNodeProvenanceAuditedRoundTrips(t *testing.T) {
	s := newTestStore(t)
	seedMetadataRoundTrip(t, s)
	scope, err := s.NodeByPath(t.Context(), "/Projects")
	require.NoError(t, err)
	target, err := s.NodeByPath(t.Context(), "/Projects/report.txt")
	require.NoError(t, err)
	seedInitialAuditAuthority(t, s, scope.ID)

	appended, err := s.AppendNodeProvenance(t.Context(), ProvenanceAppendInput{
		NodeID: target.ID, IfRevision: target.Revision, SourceKind: "agent",
		SourceDescription: "reconcile", OriginalPath: "opaque://report",
	})
	require.NoError(t, err)
	require.NoError(t, s.ValidateMetadata(t.Context()))
	var exported bytes.Buffer
	require.NoError(t, s.ExportMetadata(t.Context(), &exported))
	restored := newTestStore(t)
	require.NoError(t, restored.ImportMetadata(t.Context(), bytes.NewReader(exported.Bytes())))
	require.NoError(t, restored.ValidateMetadata(t.Context()))
	page, err := restored.NodeProvenance(t.Context(), target.ID, 10, 0)
	require.NoError(t, err)
	assert.Equal(t, appended.Fact.Identity, page.Items[0].Identity)
}
