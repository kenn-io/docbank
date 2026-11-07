package store

import (
	"bytes"
	"context"
	"path"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

func noteOperationID(t testing.TB) string {
	t.Helper()
	id, err := newUUIDv4()
	require.NoError(t, err)
	return id
}

func beginNoteScan(t testing.TB, s *Store) NotePublicationScan {
	t.Helper()
	scan, err := s.BeginNotePublicationScan(t.Context(), NotePublicationBeginRequest{
		Name: "notebook", Destination: "/notes", PolicyFingerprint: fakeHash("ab"), OperationID: noteOperationID(t),
	})
	require.NoError(t, err)
	return scan
}

func noteUpload(t testing.TB, scan NotePublicationScan, ref, hash string, revision int64) NotePublicationUploadRequest {
	t.Helper()
	return NotePublicationUploadRequest{Name: "notebook", ScanID: scan.ScanID, SourceRef: ref,
		FileName: path.Base(ref), ParentPath: path.Join("/notes", path.Dir(ref)), OperationID: noteOperationID(t), Hash: hash, Size: 4,
		ExpectedSourceRevision: revision}
}

func acceptNote(t testing.TB, s *Store, request NotePublicationUploadRequest) NotePublicationReceipt {
	t.Helper()
	receipt, err := s.AcceptPublishedNote(t.Context(), request,
		BlobPhysical{Encoding: "raw", StoredBytes: request.Size, PackEligible: true, Created: true})
	require.NoError(t, err)
	return receipt
}

func completeNoteScan(t testing.TB, s *Store, scan NotePublicationScan) NotePublicationStatus {
	t.Helper()
	status, err := s.CompleteNotePublicationScan(t.Context(), NotePublicationCompleteRequest{
		Name: "notebook", ScanID: scan.ScanID, PolicyFingerprint: scan.PolicyFingerprint, InventoryComplete: true,
	})
	require.NoError(t, err)
	return status
}

func TestNotePublicationScanCASReplayAndSeal(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	begin := NotePublicationBeginRequest{Name: "notebook", Destination: "/notes", PolicyFingerprint: fakeHash("ab"), OperationID: noteOperationID(t)}
	scan, err := s.BeginNotePublicationScan(t.Context(), begin)
	require.NoError(t, err)
	replayed, err := s.BeginNotePublicationScan(t.Context(), begin)
	require.NoError(t, err)
	require.Equal(t, scan, replayed)
	changedBegin := begin
	changedBegin.Destination = "/different"
	_, err = s.BeginNotePublicationScan(t.Context(), changedBegin)
	require.ErrorIs(t, err, ErrPublicationOperationMismatch)

	a := noteUpload(t, scan, "note.md", fakeHash("bc"), 0)
	first := acceptNote(t, s, a)
	b := noteUpload(t, scan, "note.md", fakeHash("cd"), first.Source.Identity.SourceRevision)
	second := acceptNote(t, s, b)
	require.NotEqual(t, first.Source.Identity.ContentVersionID, second.Source.Identity.ContentVersionID)
	identical, err := s.AcceptPublishedNote(t.Context(), a)
	require.NoError(t, err)
	require.Equal(t, first, identical, "retry returns its original receipt without rewinding B")
	delayed := a
	delayed.OperationID = noteOperationID(t)
	_, err = s.AcceptPublishedNote(t.Context(), delayed)
	require.ErrorIs(t, err, ErrPublicationSourceStale)
	changedReplay := a
	changedReplay.Hash = fakeHash("ef")
	_, err = s.AcceptPublishedNote(t.Context(), changedReplay)
	require.ErrorIs(t, err, ErrPublicationOperationMismatch)
	page, err := s.NotePublicationSources(t.Context(), NotePublicationSourcesRequest{Name: "notebook", ScanID: scan.ScanID, Limit: 100})
	require.NoError(t, err)
	require.Len(t, page.Items, 1)
	require.Equal(t, second.Source.Identity.ContentVersionID, page.Items[0].Identity.ContentVersionID)
	status := completeNoteScan(t, s, scan)
	require.False(t, status.Incomplete)
	require.EqualValues(t, 1, status.Present)
	require.Equal(t, status, completeNoteScan(t, s, scan), "completion itself is idempotent")
	_, err = s.AcceptPublishedNote(t.Context(), noteUpload(t, scan, "other.md", fakeHash("ef"), 0))
	require.ErrorIs(t, err, ErrPublicationScanSealed)
	_, err = s.AcknowledgePublishedNotes(t.Context(), NotePublicationSeenRequest{Name: "notebook", ScanID: scan.ScanID, Bindings: []PublishedNoteIdentity{second.Source.Identity}})
	require.ErrorIs(t, err, ErrPublicationScanSealed)
	identical, err = s.AcceptPublishedNote(t.Context(), a)
	require.NoError(t, err)
	require.Equal(t, first, identical)
	newScan := beginNoteScan(t, s)
	require.Greater(t, newScan.Epoch, scan.Epoch)
	_, err = s.BeginNotePublicationScan(t.Context(), begin)
	require.ErrorIs(t, err, ErrPublicationScanSuperseded, "retry cannot start the old publisher again")
	_, err = s.AcceptPublishedNote(t.Context(), a)
	require.ErrorIs(t, err, ErrPublicationScanSuperseded)
	_, err = s.CompleteNotePublicationScan(t.Context(), NotePublicationCompleteRequest{Name: "notebook", ScanID: scan.ScanID, PolicyFingerprint: scan.PolicyFingerprint, InventoryComplete: true})
	require.ErrorIs(t, err, ErrPublicationScanSuperseded)
}

func TestNotePublicationQuietScanRevision(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	firstScan := beginNoteScan(t, s)
	first := acceptNote(t, s, noteUpload(t, firstScan, "note.md", fakeHash("bc"), 0))
	before := completeNoteScan(t, s, firstScan)
	for range 3 {
		scan := beginNoteScan(t, s)
		page, err := s.NotePublicationSources(t.Context(), NotePublicationSourcesRequest{Name: "notebook", ScanID: scan.ScanID, Limit: 1})
		require.NoError(t, err)
		require.Len(t, page.Items, 1)
		require.Equal(t, first.Source.Identity.SourceRevision, page.Items[0].Identity.SourceRevision)
		_, err = s.AcknowledgePublishedNotes(t.Context(), NotePublicationSeenRequest{Name: "notebook", ScanID: scan.ScanID, Bindings: []PublishedNoteIdentity{page.Items[0].Identity}})
		require.NoError(t, err)
		after := completeNoteScan(t, s, scan)
		require.Equal(t, before.NamespaceRevision, after.NamespaceRevision)
		require.False(t, after.Incomplete)
	}
	// An empty successful inventory changes membership once; another quiet
	// empty inventory changes only freshness, and reappearance keeps identity.
	absent := completeNoteScan(t, s, beginNoteScan(t, s))
	require.EqualValues(t, 1, absent.Absent)
	require.Greater(t, absent.NamespaceRevision, before.NamespaceRevision)
	require.Equal(t, absent.NamespaceRevision, completeNoteScan(t, s, beginNoteScan(t, s)).NamespaceRevision)
	scan := beginNoteScan(t, s)
	page, err := s.NotePublicationSources(t.Context(), NotePublicationSourcesRequest{Name: "notebook", ScanID: scan.ScanID, Limit: 100})
	require.NoError(t, err)
	require.Equal(t, "absent", page.Items[0].Presence)
	_, err = s.AcknowledgePublishedNotes(t.Context(), NotePublicationSeenRequest{Name: "notebook", ScanID: scan.ScanID, Bindings: []PublishedNoteIdentity{page.Items[0].Identity}})
	require.NoError(t, err)
	reappeared := completeNoteScan(t, s, scan)
	require.EqualValues(t, 1, reappeared.Present)
	page, err = s.NotePublicationSources(t.Context(), NotePublicationSourcesRequest{Name: "notebook", ScanID: scan.ScanID, Limit: 100})
	require.NoError(t, err)
	require.Equal(t, first.Source.Identity.ContentVersionID, page.Items[0].Identity.ContentVersionID)
}

func TestNotePublicationScanAtomicSeenIncompleteAndRestore(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	scan := beginNoteScan(t, s)
	one := acceptNote(t, s, noteUpload(t, scan, "one.md", fakeHash("bc"), 0))
	two := acceptNote(t, s, noteUpload(t, scan, "two.md", fakeHash("cd"), 0))
	completeNoteScan(t, s, scan)
	scan = beginNoteScan(t, s)
	invalid := two.Source.Identity
	invalid.SourceRevision++
	_, err := s.AcknowledgePublishedNotes(t.Context(), NotePublicationSeenRequest{Name: "notebook", ScanID: scan.ScanID, Bindings: []PublishedNoteIdentity{one.Source.Identity, invalid}})
	require.ErrorIs(t, err, ErrPublicationSourceStale)
	_, err = s.CompleteNotePublicationScan(t.Context(), NotePublicationCompleteRequest{Name: "notebook", ScanID: scan.ScanID, PolicyFingerprint: scan.PolicyFingerprint})
	require.Error(t, err)
	status, err := s.NotePublicationStatus(t.Context(), "notebook")
	require.NoError(t, err)
	require.True(t, status.Incomplete)
	require.EqualValues(t, 2, status.Present)
	// Failed seen batches leave no positive marks: completing this inventory
	// empty retires both sources, rather than half-accepting the first binding.
	status = completeNoteScan(t, s, scan)
	require.EqualValues(t, 2, status.Absent)
	scan = beginNoteScan(t, s)
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = s.CompleteNotePublicationScan(cancelled, NotePublicationCompleteRequest{Name: "notebook", ScanID: scan.ScanID, PolicyFingerprint: scan.PolicyFingerprint, InventoryComplete: true})
	require.ErrorIs(t, err, context.Canceled)
	var metadata bytes.Buffer
	require.NoError(t, s.ExportMetadata(t.Context(), &metadata))
	restored := newTestStore(t)
	require.NoError(t, restored.ImportMetadata(t.Context(), bytes.NewReader(metadata.Bytes())))
	_, err = restored.NotePublicationSources(t.Context(), NotePublicationSourcesRequest{Name: "notebook", ScanID: scan.ScanID, Limit: 100})
	require.ErrorIs(t, err, ErrPublicationScanSuperseded)
	status, err = restored.NotePublicationStatus(t.Context(), "notebook")
	require.NoError(t, err)
	require.EqualValues(t, 2, status.Absent)
	require.True(t, status.Incomplete)
}

func TestNotePublicationScanConcurrentCAS(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	scan := beginNoteScan(t, s)
	first := acceptNote(t, s, noteUpload(t, scan, "note.md", fakeHash("bc"), 0))
	requests := []NotePublicationUploadRequest{
		noteUpload(t, scan, "note.md", fakeHash("cd"), first.Source.Identity.SourceRevision),
		noteUpload(t, scan, "note.md", fakeHash("ef"), first.Source.Identity.SourceRevision),
	}
	type result struct {
		receipt NotePublicationReceipt
		err     error
	}
	results := make(chan result, 2)
	var start sync.WaitGroup
	start.Add(1)
	for _, request := range requests {
		go func() {
			start.Wait()
			receipt, err := s.AcceptPublishedNote(t.Context(), request, BlobPhysical{Encoding: "raw", StoredBytes: request.Size, PackEligible: true, Created: true})
			results <- result{receipt, err}
		}()
	}
	start.Done()
	a, b := <-results, <-results
	if a.err != nil {
		a, b = b, a
	}
	require.NoError(t, a.err)
	require.ErrorIs(t, b.err, ErrPublicationSourceStale)
	page, err := s.NotePublicationSources(t.Context(), NotePublicationSourcesRequest{Name: "notebook", ScanID: scan.ScanID, Limit: 100})
	require.NoError(t, err)
	require.Len(t, page.Items, 1)
	require.Equal(t, a.receipt.Source.Identity, page.Items[0].Identity)
}
