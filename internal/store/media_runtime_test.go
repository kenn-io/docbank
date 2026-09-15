package store

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func suppliedMediaPublicationFixture(t *testing.T, s *Store) MediaPublicationRequest {
	t.Helper()
	file, err := s.CreateFile(t.Context(), s.RootID(), "recording.mp3", testSHA256([]byte("recording")), 10, "audio/mpeg")
	require.NoError(t, err)
	version, err := s.ContentVersionByID(t.Context(), file.CurrentVersionID)
	require.NoError(t, err)
	sourceID := testSHA256([]byte("supplied-source"))
	return MediaPublicationRequest{
		Operation: MediaOperation{ID: "00000000-0000-4000-8000-000000000061", Principal: "operator",
			Verb: "submit_supplied_media", RequestSHA256: testSHA256([]byte("request")), SourceID: sourceID},
		SourceID: sourceID, ContentVersion: version, CaptureJSON: "{}", ClaimSHA256: digestCatalogJSON([]byte("{}")),
		Occurrence: MediaOccurrenceInput{SourceID: sourceID, Principal: "operator", Ref: "recording",
			Revision: "1", Filename: "recording.mp3", MessageJSON: "{}"},
	}
}

func TestRetainSuppliedMediaReusesCallerRevisionAcrossOperations(t *testing.T) {
	s := newTestStore(t)
	request := suppliedMediaPublicationFixture(t, s)
	first, err := s.RetainSuppliedMedia(t.Context(), request)
	require.NoError(t, err)
	request.Operation.ID = "00000000-0000-4000-8000-000000000062"
	second, err := s.RetainSuppliedMedia(t.Context(), request)
	require.NoError(t, err)
	require.Equal(t, first.OccurrenceID, second.OccurrenceID)
	require.Equal(t, first.SourceVersionID, second.SourceVersionID)
	var fence int64
	require.NoError(t, s.db.QueryRow(`SELECT fence FROM media_visibility_fences WHERE caller_principal='operator'`).Scan(&fence))
	require.Equal(t, int64(1), fence)
	request.Operation.ID = "00000000-0000-4000-8000-000000000063"
	request.Occurrence.Filename = "changed.mp3"
	_, err = s.RetainSuppliedMedia(t.Context(), request)
	require.ErrorIs(t, err, ErrMediaOccurrenceConflict)
}

func TestImportMediaInputArtifactRejectsChangedImmutableAuthority(t *testing.T) {
	s := newTestStore(t)
	publication := suppliedMediaPublicationFixture(t, s)
	retained, err := s.RetainSuppliedMedia(t.Context(), publication)
	require.NoError(t, err)
	request := MediaInputArtifactRequest{
		Operation: MediaOperation{ID: "00000000-0000-4000-8000-000000000064", Principal: "operator",
			Verb: "import_recording_artifact", SourceID: retained.SourceID, RequestSHA256: testSHA256([]byte("artifact-request"))},
		InputID: testSHA256([]byte("artifact")), OccurrenceID: retained.OccurrenceID, SourceVersionID: retained.SourceVersionID,
		VirtualPath: "/media-input/transcript.txt", MediaType: "text/plain", ByteLength: 20,
		Physical: BlobPhysical{Encoding: "raw", StoredBytes: 20, Created: true},
		Kind:     "transcript", Origin: "supplied", Language: "en", InputSHA: testSHA256([]byte("transcript")),
	}
	first, err := s.ImportMediaInputArtifact(t.Context(), request)
	require.NoError(t, err)
	request.Operation.ID = "00000000-0000-4000-8000-000000000065"
	replayed, err := s.ImportMediaInputArtifact(t.Context(), request)
	require.NoError(t, err)
	require.Equal(t, first.SuppliedInputID, replayed.SuppliedInputID)
	for _, field := range []string{"origin", "provider", "language"} {
		t.Run(field, func(t *testing.T) {
			changed := request
			changed.Operation.ID = "00000000-0000-4000-8000-000000000066"
			switch field {
			case "origin":
				changed.Origin = "provider"
			case "provider":
				changed.Provider = "synthetic-provider"
			case "language":
				changed.Language = "fr"
			}
			_, err := s.ImportMediaInputArtifact(t.Context(), changed)
			require.ErrorIs(t, err, ErrMediaOperationConflict)
		})
	}
}
