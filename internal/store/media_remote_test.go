package store

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/internal/canonical"
)

func remoteStoreReference(
	t *testing.T, s *Store, sourceID, operationID, occurrenceID string,
) (MediaPublicationReceipt, MediaOperation) {
	t.Helper()
	operation := MediaOperation{ID: operationID, Principal: "operator:remote",
		Verb: "submit_remote_recording", RequestSHA256: testSHA256([]byte(operationID)), SourceID: sourceID}
	receipt, err := s.RetainMediaReference(t.Context(), MediaReferencePublicationRequest{
		Operation: operation, Provider: "url", OriginScope: testSHA256([]byte("origin:" + sourceID)),
		IdentitySHA256: testSHA256([]byte("identity:" + sourceID)), Outcome: "unsupported",
		Occurrence: MediaOccurrenceInput{ID: occurrenceID, SourceID: sourceID,
			Principal: operation.Principal, Ref: occurrenceID, Revision: "1",
			Filename: "recording.wav", MessageJSON: "{}"},
	})
	require.NoError(t, err)
	return receipt, operation
}

func remoteStoreArtifact(
	operationID, sourceID, occurrenceID, sourceVersionID, inputID, inputHash string, size int64,
	physical BlobPhysical,
) MediaInputArtifactRequest {
	return MediaInputArtifactRequest{
		Operation: MediaOperation{ID: operationID, Principal: "operator:remote",
			Verb: "import_recording_artifact", RequestSHA256: testSHA256([]byte(operationID)), SourceID: sourceID},
		InputID: inputID, OccurrenceID: occurrenceID, SourceVersionID: sourceVersionID,
		VirtualPath: "/media/" + sourceID + "/" + inputHash + ".wav", MediaType: "audio/wav",
		ByteLength: size, Physical: physical, Kind: "media", Origin: "supplied", InputSHA: inputHash,
	}
}

func TestRetainRemoteRecordingMedia(t *testing.T) {
	s := newTestStore(t)
	sourceID := testSHA256([]byte("remote-source"))
	retained, _ := remoteStoreReference(t, s, sourceID,
		"00000000-0000-4000-8000-000000000801", "remote-occurrence-a")
	firstHash := testSHA256([]byte("remote-original-a"))
	first := remoteStoreArtifact("00000000-0000-4000-8000-000000000802", sourceID,
		retained.OccurrenceID, "", testSHA256([]byte("remote-input-a")), firstHash, 10,
		BlobPhysical{Encoding: "raw", StoredBytes: 10, Created: true})
	firstReceipt, err := s.RetainRemoteRecordingMedia(t.Context(), first)
	require.NoError(t, err)
	require.Equal(t, "content_available", firstReceipt.Outcome)
	require.Equal(t, "unprocessed", firstReceipt.CoverageState)
	require.Equal(t, "succeeded", firstReceipt.OperationState)
	require.Equal(t, retained.OccurrenceID, firstReceipt.OccurrenceID)
	require.NotEmpty(t, firstReceipt.SourceVersionID)
	require.NotEmpty(t, firstReceipt.ContentVersionID)
	require.Equal(t, first.InputID, firstReceipt.SuppliedInputID)

	require.NoError(t, s.DeclareMediaOccurrence(t.Context(), MediaOccurrenceInput{
		ID: "remote-occurrence-b", SourceID: sourceID, Principal: "operator:remote",
		Ref: "remote-occurrence-b", Revision: "1", Filename: "recording-b.wav", MessageJSON: "{}",
	}))
	second := remoteStoreArtifact("00000000-0000-4000-8000-000000000803", sourceID,
		"remote-occurrence-b", "", testSHA256([]byte("remote-input-b")), firstHash, 10,
		BlobPhysical{Encoding: "raw", StoredBytes: 10, Created: false})
	secondReceipt, err := s.RetainRemoteRecordingMedia(t.Context(), second)
	require.NoError(t, err)
	require.Equal(t, firstReceipt.SourceVersionID, secondReceipt.SourceVersionID)
	require.Equal(t, firstReceipt.ContentVersionID, secondReceipt.ContentVersionID)

	secondHash := testSHA256([]byte("remote-original-b"))
	require.NoError(t, s.DeclareMediaOccurrence(t.Context(), MediaOccurrenceInput{
		ID: "remote-occurrence-c", SourceID: sourceID, Principal: "operator:remote",
		Ref: "remote-occurrence-c", Revision: "1", Filename: "recording-c.wav", MessageJSON: "{}",
	}))
	third := remoteStoreArtifact("00000000-0000-4000-8000-000000000804", sourceID,
		"remote-occurrence-c", "", testSHA256([]byte("remote-input-c")), secondHash, 10,
		BlobPhysical{Encoding: "raw", StoredBytes: 10, Created: true})
	thirdReceipt, err := s.RetainRemoteRecordingMedia(t.Context(), third)
	require.NoError(t, err)
	require.NotEqual(t, firstReceipt.SourceVersionID, thirdReceipt.SourceVersionID)
	var headRevision int64
	require.NoError(t, s.db.QueryRow(`SELECT revision FROM media_source_heads WHERE source_id=?`, sourceID).Scan(&headRevision))
	require.Equal(t, int64(2), headRevision)

	changed := remoteStoreArtifact("00000000-0000-4000-8000-000000000805", sourceID,
		retained.OccurrenceID, firstReceipt.SourceVersionID, testSHA256([]byte("changed-input")), secondHash, 10,
		BlobPhysical{Encoding: "raw", StoredBytes: 10, Created: true})
	_, err = s.RetainRemoteRecordingMedia(t.Context(), changed)
	require.ErrorIs(t, err, ErrMediaSourceConflict)

	require.NoError(t, s.DeclareMediaOccurrence(t.Context(), MediaOccurrenceInput{
		ID: "remote-occurrence-revoked", SourceID: sourceID, Principal: "operator:remote",
		Ref: "remote-occurrence-revoked", Revision: "1", Filename: "revoked.wav", MessageJSON: "{}",
	}))
	_, err = s.RevokeMediaOccurrence(t.Context(), "operator:remote", "remote-occurrence-revoked")
	require.NoError(t, err)
	revoked := remoteStoreArtifact("00000000-0000-4000-8000-000000000806", sourceID,
		"remote-occurrence-revoked", "", testSHA256([]byte("revoked-input")), firstHash, 10,
		BlobPhysical{Encoding: "raw", StoredBytes: 10, Created: true})
	_, err = s.RetainRemoteRecordingMedia(t.Context(), revoked)
	require.ErrorIs(t, err, ErrNotFound)

	foreign := remoteStoreArtifact("00000000-0000-4000-8000-000000000807", sourceID,
		retained.OccurrenceID, firstReceipt.SourceVersionID, testSHA256([]byte("foreign-input")), firstHash, 10,
		BlobPhysical{Encoding: "raw", StoredBytes: 10, Created: true})
	foreign.Operation.Principal = "operator:foreign"
	_, err = s.RetainRemoteRecordingMedia(t.Context(), foreign)
	require.ErrorIs(t, err, ErrNotFound)

	otherSource := testSHA256([]byte("other-remote-source"))
	otherReference, _ := remoteStoreReference(t, s, otherSource,
		"00000000-0000-4000-8000-000000000808", "other-occurrence")
	mismatched := remoteStoreArtifact("00000000-0000-4000-8000-000000000809", sourceID,
		otherReference.OccurrenceID, "", testSHA256([]byte("mismatched-input")), firstHash, 10,
		BlobPhysical{Encoding: "raw", StoredBytes: 10, Created: true})
	_, err = s.RetainRemoteRecordingMedia(t.Context(), mismatched)
	require.ErrorIs(t, err, ErrNotFound)

	var versions int
	require.NoError(t, s.db.QueryRow(`SELECT count(*) FROM media_source_versions WHERE source_id=?`, sourceID).Scan(&versions))
	require.Equal(t, 2, versions)
}

func TestRemoteRecordingManualMetadata(t *testing.T) {
	s := newTestStore(t)
	sourceID := testSHA256([]byte("metadata-remote-source"))
	reference, referenceOperation := remoteStoreReference(t, s, sourceID,
		"00000000-0000-4000-8000-000000000811", "metadata-occurrence")
	hash := testSHA256([]byte("metadata-original"))
	request := remoteStoreArtifact("00000000-0000-4000-8000-000000000812", sourceID,
		reference.OccurrenceID, "", testSHA256([]byte("metadata-input")), hash, 17,
		BlobPhysical{Encoding: "raw", StoredBytes: 17, Created: true})
	receipt, err := s.RetainRemoteRecordingMedia(t.Context(), request)
	require.NoError(t, err)

	interruptedSource := testSHA256([]byte("interrupted-source"))
	interrupted, _ := remoteStoreReference(t, s, interruptedSource,
		"00000000-0000-4000-8000-000000000813", "interrupted-occurrence")
	interruptedHash := testSHA256([]byte("interrupted-original"))
	bad := remoteStoreArtifact("00000000-0000-4000-8000-000000000814", interruptedSource,
		interrupted.OccurrenceID, "", testSHA256([]byte("interrupted-input")), interruptedHash, 17,
		BlobPhysical{Encoding: "raw", StoredBytes: 16, Created: true})
	_, err = s.RetainRemoteRecordingMedia(t.Context(), bad)
	require.Error(t, err)
	status, err := s.MediaSource(t.Context(), "operator:remote", interruptedSource)
	require.NoError(t, err)
	require.Empty(t, status.SourceVersionID)
	require.Empty(t, status.ContentVersionID)
	_, err = s.NodeByPath(t.Context(), bad.VirtualPath)
	require.ErrorIs(t, err, ErrNotFound)
	present, err := s.HasBlob(t.Context(), interruptedHash)
	require.NoError(t, err)
	require.False(t, present)

	var exported bytes.Buffer
	require.NoError(t, s.ExportMetadata(t.Context(), &exported))
	restored := newTestStore(t)
	require.NoError(t, restored.ImportMetadata(t.Context(), &exported))
	replayedRaw, err := restored.MediaOperationReceipt(t.Context(), request.Operation)
	require.NoError(t, err)
	replayed, err := canonical.Decode[MediaPublicationReceipt]([]byte(replayedRaw))
	require.NoError(t, err)
	require.Equal(t, receipt, replayed)
	restoredStatus, err := restored.MediaSource(t.Context(), referenceOperation.Principal, sourceID)
	require.NoError(t, err)
	require.Equal(t, "remote_recording", restoredStatus.Kind)
	require.Equal(t, receipt.SourceVersionID, restoredStatus.SourceVersionID)
	require.Equal(t, receipt.ContentVersionID, restoredStatus.ContentVersionID)
	var inputCount int
	require.NoError(t, restored.db.QueryRow(`SELECT count(*) FROM media_input_artifacts WHERE input_id=?`, request.InputID).Scan(&inputCount))
	require.Equal(t, 1, inputCount)
}
