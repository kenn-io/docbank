package store

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/require"
)

func publishedSource(tb testing.TB, s *Store, scan NotePublicationScan) NotePublicationSource {
	tb.Helper()
	page, err := s.NotePublicationSources(tb.Context(), NotePublicationSourcesRequest{Name: "notebook", ScanID: scan.ScanID, Limit: 100})
	require.NoError(tb, err)
	require.Len(tb, page.Items, 1)
	return page.Items[0]
}

func TestPublishedSourcePrunedVersionRestore(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	scan := beginNoteScan(t, s)
	a := acceptNote(t, s, noteUpload(t, scan, "note.md", fakeHash("ab"), 0))
	completeNoteScan(t, s, scan)
	node, err := s.NodeByID(t.Context(), a.Source.Identity.NodeID)
	require.NoError(t, err)
	node, b, err := s.ReplaceContent(t.Context(), node.ID, node.Revision, fakeHash("bc"), 4, "text/markdown")
	require.NoError(t, err)
	before, err := s.NotePublicationStatus(t.Context(), "notebook")
	require.NoError(t, err)
	_, err = s.PruneContentVersions(t.Context(), node.ID, node.Revision, VersionPruneSelector{KeepNewest: 1}, true)
	require.NoError(t, err)
	after, err := s.NotePublicationStatus(t.Context(), "notebook")
	require.NoError(t, err)
	require.Greater(t, after.NamespaceRevision, before.NamespaceRevision, "pruning the accepted version invalidates graph authority")
	require.EqualValues(t, 1, after.Unavailable)
	current := publishedSource(t, s, scan)
	require.Equal(t, "unavailable", current.Availability)
	require.Equal(t, a.Source.Identity.ContentVersionID, current.Identity.ContentVersionID)
	require.Equal(t, b.ID, current.ArchiveHead.ContentVersionID)
	var data bytes.Buffer
	require.NoError(t, s.ExportMetadata(t.Context(), &data))
	restored := newTestStore(t)
	require.NoError(t, restored.ImportMetadata(t.Context(), &data))
	current = publishedSource(t, restored, beginNoteScan(t, restored))
	require.Equal(t, "unavailable", current.Availability)
	require.Equal(t, a.Source.Identity.ContentVersionID, current.Identity.ContentVersionID)
	require.NotEqual(t, b.ID, current.Identity.ContentVersionID)
	_, err = restored.AcceptPublishedNote(t.Context(), noteUpload(t, beginNoteScan(t, restored), "note.md", fakeHash("ab"), current.Identity.SourceRevision))
	require.ErrorIs(t, err, ErrNoteSourceUnavailable, "a tombstone cannot silently mint a replacement")
}

func TestPublishedSourcePermanentNodeTombstone(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	scan := beginNoteScan(t, s)
	a := acceptNote(t, s, noteUpload(t, scan, "note.md", fakeHash("ab"), 0))
	node, err := s.NodeByID(t.Context(), a.Source.Identity.NodeID)
	require.NoError(t, err)
	_, _, err = s.Trash(t.Context(), node.ID, node.Revision)
	require.NoError(t, err)
	before, err := s.NotePublicationStatus(t.Context(), "notebook")
	require.NoError(t, err)
	_, err = s.TrashEmpty(t.Context(), 0, true)
	require.NoError(t, err)
	after, err := s.NotePublicationStatus(t.Context(), "notebook")
	require.NoError(t, err)
	require.Greater(t, after.NamespaceRevision, before.NamespaceRevision)
	current := publishedSource(t, s, scan)
	require.Equal(t, a.Source.Identity.ContentVersionID, current.Identity.ContentVersionID)
	require.Equal(t, a.Source.Identity.SourceRevision, current.Identity.SourceRevision)
	require.Equal(t, "present", current.Presence)
	require.Equal(t, "unavailable", current.Availability)
	require.Nil(t, current.ArchiveHead)
	var data bytes.Buffer
	require.NoError(t, s.ExportMetadata(t.Context(), &data))
	restored := newTestStore(t)
	require.NoError(t, restored.ImportMetadata(t.Context(), &data))
	current = publishedSource(t, restored, beginNoteScan(t, restored))
	require.Equal(t, a.Source.Identity.ContentVersionID, current.Identity.ContentVersionID)
	require.Equal(t, "unavailable", current.Availability)
}

func TestPublishedSourceArchiveChangesInvalidate(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	scan := beginNoteScan(t, s)
	a := acceptNote(t, s, noteUpload(t, scan, "note.md", fakeHash("ab"), 0))
	last := a.Source.Identity.NamespaceRevision
	assertInvalidated := func() NotePublicationSource {
		t.Helper()
		source := publishedSource(t, s, scan)
		require.Greater(t, source.Identity.NamespaceRevision, last)
		last = source.Identity.NamespaceRevision
		require.Equal(t, "note.md", source.Identity.SourceRef)
		require.Equal(t, a.Source.Identity.ContentVersionID, source.Identity.ContentVersionID)
		return source
	}
	node, err := s.NodeByID(t.Context(), a.Source.Identity.NodeID)
	require.NoError(t, err)
	node, _, err = s.Move(t.Context(), node.ID, s.RootID(), "moved.md", node.Revision)
	require.NoError(t, err)
	require.Empty(t, assertInvalidated().Conflict)
	node, _, err = s.Trash(t.Context(), node.ID, node.Revision)
	require.NoError(t, err)
	require.Equal(t, "trashed", assertInvalidated().Conflict)
	node, _, err = s.Restore(t.Context(), node.ID, node.Revision)
	require.NoError(t, err)
	require.Empty(t, assertInvalidated().Conflict)
	node, _, err = s.ReplaceContent(t.Context(), node.ID, node.Revision, fakeHash("bc"), 4, "text/markdown")
	require.NoError(t, err)
	require.Equal(t, "diverged", assertInvalidated().Conflict)
	node, reverted, _, err := s.RevertContent(t.Context(), node.ID, node.Revision, a.Source.Identity.ContentVersionID)
	require.NoError(t, err)
	require.Equal(t, node.CurrentVersionID, reverted.ID)
	require.Equal(t, "diverged", assertInvalidated().Conflict, "revert creates a new exact version even with the original digest")
}

func TestPublishedSourceAcceptLocalDigestEqual(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	scan := beginNoteScan(t, s)
	a := acceptNote(t, s, noteUpload(t, scan, "note.md", fakeHash("ab"), 0))
	node, err := s.NodeByID(t.Context(), a.Source.Identity.NodeID)
	require.NoError(t, err)
	_, manual, err := s.ReplaceContent(t.Context(), node.ID, node.Revision, fakeHash("ab"), 4, "text/markdown")
	require.NoError(t, err)
	request := NotePublicationAcceptLocalRequest{Name: "notebook", DatabaseEpoch: scan.DatabaseEpoch, SourceRef: "note.md", ExpectedHeadVersionID: manual.ID,
		ExpectedSourceRevision: a.Source.Identity.SourceRevision, OperationID: noteOperationID(t)}
	grant, err := s.AcceptLocalPublishedNote(t.Context(), request)
	require.NoError(t, err)
	replayed, err := s.AcceptLocalPublishedNote(t.Context(), request)
	require.NoError(t, err)
	require.Equal(t, grant, replayed)
	require.Equal(t, &grant, publishedSource(t, s, scan).Recovery)
	node, err = s.NodeByID(t.Context(), node.ID)
	require.NoError(t, err)
	_, _, err = s.Move(t.Context(), node.ID, s.RootID(), "relocated.md", node.Revision)
	require.NoError(t, err)
	require.Equal(t, &grant, publishedSource(t, s, scan).Recovery, "an archive move keeps the reviewed head and source identity")
	status, err := s.NotePublicationStatus(t.Context(), "notebook")
	require.NoError(t, err)
	require.EqualValues(t, 1, status.RecoveryPending)
	_, err = s.AcknowledgePublishedNotes(t.Context(), NotePublicationSeenRequest{Name: "notebook", ScanID: scan.ScanID, Bindings: []PublishedNoteIdentity{a.Source.Identity}})
	require.ErrorIs(t, err, ErrPublishedSourceDiverged, "a pending grant requires verified upload, never seen-only adoption")
	upload := noteUpload(t, scan, "note.md", fakeHash("ab"), a.Source.Identity.SourceRevision)
	recovered := acceptNote(t, s, upload)
	require.NotEqual(t, a.Source.Identity.ContentVersionID, recovered.Source.Identity.ContentVersionID)
	require.NotEqual(t, manual.ID, recovered.Source.Identity.ContentVersionID)
	require.Nil(t, recovered.Source.Recovery)
	require.Empty(t, recovered.Source.Conflict)
	_, err = s.ContentVersionByID(t.Context(), manual.ID)
	require.NoError(t, err, "manual history is retained")
	_, err = s.AcceptLocalPublishedNote(t.Context(), request)
	require.ErrorIs(t, err, ErrPublicationSourceStale, "replayed grant cannot be recreated after it was consumed")
	retry, err := s.AcceptPublishedNote(t.Context(), upload)
	require.NoError(t, err)
	require.Equal(t, recovered, retry)
}

func TestPublishedSourceRecoveryExpiresWithSourceRevision(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	scan := beginNoteScan(t, s)
	a := acceptNote(t, s, noteUpload(t, scan, "note.md", fakeHash("ab"), 0))
	completeNoteScan(t, s, scan)
	node, err := s.NodeByID(t.Context(), a.Source.Identity.NodeID)
	require.NoError(t, err)
	_, manual, err := s.ReplaceContent(t.Context(), node.ID, node.Revision, fakeHash("bc"), 4, "text/markdown")
	require.NoError(t, err)
	_, err = s.AcceptLocalPublishedNote(t.Context(), NotePublicationAcceptLocalRequest{Name: "notebook", DatabaseEpoch: scan.DatabaseEpoch, SourceRef: "note.md", ExpectedHeadVersionID: manual.ID,
		ExpectedSourceRevision: a.Source.Identity.SourceRevision, OperationID: noteOperationID(t)})
	require.NoError(t, err)
	scan = beginNoteScan(t, s)
	status := completeNoteScan(t, s, scan)
	require.Zero(t, status.RecoveryPending, "an absent source has a new source revision, so old permission no longer applies")
	require.Nil(t, publishedSource(t, s, scan).Recovery)
}

func TestPublishedSourceRecoveryRejectsChangedHeadAndRestore(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	scan := beginNoteScan(t, s)
	a := acceptNote(t, s, noteUpload(t, scan, "note.md", fakeHash("ab"), 0))
	node, err := s.NodeByID(t.Context(), a.Source.Identity.NodeID)
	require.NoError(t, err)
	node, manual, err := s.ReplaceContent(t.Context(), node.ID, node.Revision, fakeHash("bc"), 4, "text/markdown")
	require.NoError(t, err)
	request := NotePublicationAcceptLocalRequest{Name: "notebook", DatabaseEpoch: scan.DatabaseEpoch, SourceRef: "note.md", ExpectedHeadVersionID: manual.ID,
		ExpectedSourceRevision: a.Source.Identity.SourceRevision, OperationID: noteOperationID(t)}
	_, err = s.AcceptLocalPublishedNote(t.Context(), request)
	require.NoError(t, err)
	var data bytes.Buffer
	require.NoError(t, s.ExportMetadata(t.Context(), &data))
	restored := newTestStore(t)
	require.NoError(t, restored.ImportMetadata(t.Context(), &data))
	require.Nil(t, publishedSource(t, restored, beginNoteScan(t, restored)).Recovery)
	_, err = restored.AcceptLocalPublishedNote(t.Context(), request)
	require.ErrorIs(t, err, ErrPublicationScanSuperseded, "restore cannot recreate an old operational grant by retry")
	_, _, err = s.ReplaceContent(t.Context(), node.ID, node.Revision, fakeHash("cd"), 4, "text/markdown")
	require.NoError(t, err)
	require.Nil(t, publishedSource(t, s, scan).Recovery)
	_, err = s.AcceptLocalPublishedNote(t.Context(), request)
	require.ErrorIs(t, err, ErrPublishedSourceDiverged)
	_, err = s.AcceptPublishedNote(t.Context(), noteUpload(t, scan, "note.md", fakeHash("ab"), a.Source.Identity.SourceRevision))
	require.ErrorIs(t, err, ErrPublishedSourceDiverged)
}

func TestPublishedSourceRecoveryOperationPayloadFence(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	scan := beginNoteScan(t, s)
	a := acceptNote(t, s, noteUpload(t, scan, "note.md", fakeHash("ab"), 0))
	node, err := s.NodeByID(t.Context(), a.Source.Identity.NodeID)
	require.NoError(t, err)
	_, manual, err := s.ReplaceContent(t.Context(), node.ID, node.Revision, fakeHash("bc"), 4, "text/markdown")
	require.NoError(t, err)
	r := NotePublicationAcceptLocalRequest{Name: "notebook", DatabaseEpoch: scan.DatabaseEpoch, SourceRef: "note.md", ExpectedHeadVersionID: manual.ID,
		ExpectedSourceRevision: a.Source.Identity.SourceRevision, OperationID: noteOperationID(t)}
	_, err = s.AcceptLocalPublishedNote(t.Context(), r)
	require.NoError(t, err)
	changed := r
	changed.ExpectedHeadVersionID = a.Source.Identity.ContentVersionID
	_, err = s.AcceptLocalPublishedNote(t.Context(), changed)
	require.ErrorIs(t, err, ErrPublicationOperationMismatch)
	begin := NotePublicationBeginRequest{Name: "notebook", Destination: "/notes", PolicyFingerprint: fakeHash("ab"), OperationID: r.OperationID}
	_, err = s.BeginNotePublicationScan(t.Context(), begin)
	require.ErrorIs(t, err, ErrPublicationOperationMismatch)
	upload := noteUpload(t, scan, "note.md", fakeHash("ab"), a.Source.Identity.SourceRevision)
	upload.OperationID = r.OperationID
	_, err = s.AcceptPublishedNote(t.Context(), upload)
	require.ErrorIs(t, err, ErrPublicationOperationMismatch)
}

func TestPublishedSourceMetadataRejectsContradictoryPushIdentity(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	scan := beginNoteScan(t, s)
	acceptNote(t, s, noteUpload(t, scan, "note.md", fakeHash("ab"), 0))
	_, err := s.db.ExecContext(t.Context(), `UPDATE published_note_sources SET source_ref='other.md' WHERE publication_name='notebook'`)
	require.NoError(t, err)
	var out bytes.Buffer
	require.Error(t, s.ExportMetadata(t.Context(), &out), "an existing push identity cannot be relabeled as another source")
}

func TestPublishedSourceAuditedSubtreeInvalidation(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	scan := beginNoteScan(t, s)
	a := acceptNote(t, s, noteUpload(t, scan, "nested/note.md", fakeHash("ab"), 0))
	_, err := s.initializeAuditAuthority(t.Context(), s.RootID(), "api", nil)
	require.NoError(t, err)
	folder, err := s.NodeByPath(t.Context(), "/notes")
	require.NoError(t, err)
	folder, _, err = s.Move(t.Context(), folder.ID, s.RootID(), "relocated", folder.Revision)
	require.NoError(t, err)
	moved := publishedSource(t, s, scan)
	require.Greater(t, moved.Identity.NamespaceRevision, a.Source.Identity.NamespaceRevision)
	require.Equal(t, "nested/note.md", moved.Identity.SourceRef)
	folder, _, err = s.Trash(t.Context(), folder.ID, folder.Revision)
	require.NoError(t, err)
	trashed := publishedSource(t, s, scan)
	require.Greater(t, trashed.Identity.NamespaceRevision, moved.Identity.NamespaceRevision)
	require.Equal(t, "trashed", trashed.Conflict)
	_, _, err = s.Restore(t.Context(), folder.ID, folder.Revision)
	require.NoError(t, err)
	restored := publishedSource(t, s, scan)
	require.Greater(t, restored.Identity.NamespaceRevision, trashed.Identity.NamespaceRevision)
	require.Empty(t, restored.Conflict)
	node, err := s.NodeByID(t.Context(), a.Source.Identity.NodeID)
	require.NoError(t, err)
	_, _, err = s.ReplaceContent(t.Context(), node.ID, node.Revision, fakeHash("bc"), 4, "text/markdown")
	require.NoError(t, err)
	edited := publishedSource(t, s, scan)
	require.Greater(t, edited.Identity.NamespaceRevision, restored.Identity.NamespaceRevision)
	require.Equal(t, "diverged", edited.Conflict)
}
