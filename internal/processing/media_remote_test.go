package processing

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"sync"
	"testing"
	"time"
	"uuid"

	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/document/media/mediatest"
	"go.kenn.io/docbank/internal/store"
)

func newRemoteRecordingTestService(
	t *testing.T, fixture publicationFixture, principal string, maximum int64,
	origins map[string]MediaOriginPolicy,
) *Service {
	t.Helper()
	service, err := NewService(ServiceConfig{
		Catalog: fixture.catalog, Blobs: fixture.blobs, Gate: newWorkerTestGate(),
		SpoolDirectory: t.TempDir(), Principal: principal, MediaMaxBytes: maximum,
		MediaOrigins: origins, MediaTokenKey: [32]byte{1},
	})
	require.NoError(t, err)
	return service
}

func remoteRecordingTestOrigins() map[string]MediaOriginPolicy {
	return map[string]MediaOriginPolicy{"synthetic": {
		OriginID: "synthetic", Provider: "synthetic",
		ReferencePrefixes: []string{"https://recordings.invalid/"},
	}}
}

func remoteRecordingTestRequest(operationID, reference, canonicalURL, ref string) RemoteRecordingRequest {
	return RemoteRecordingRequest{
		OperationID: operationID, ReferenceURL: reference, CanonicalURL: canonicalURL,
		CredentialBinding: "synthetic-binding", ProviderHint: "synthetic",
		Occurrence: MediaOccurrenceInput{Ref: ref, Revision: "1", Filename: "call.wav"},
	}
}

func remoteRecordingTestArtifact(
	operationID, sourceID, occurrenceID, filename, mediaType, hash string, size int64, content []byte,
) MediaArtifactRequest {
	return MediaArtifactRequest{
		OperationID: operationID, SourceID: sourceID, OccurrenceID: occurrenceID,
		Kind: "media", Origin: "supplied", Filename: filename, MediaType: mediaType,
		SHA256: hash, ByteLength: size, Content: bytes.NewReader(content),
	}
}

func TestRemoteRecordingManualReference(t *testing.T) {
	fixture := newPublicationFixture(t)
	service := newRemoteRecordingTestService(t, fixture, "operator:remote", 0, nil)
	reference := "https://private.invalid/share/call?token=synthetic-secret"
	request := remoteRecordingTestRequest(
		uuid.New().String(), reference,
		"HTTPS://Recordings.INVALID:443/share/call?clip=1#ignored", "call-a")
	canonicalURL, origin, err := canonicalRemoteRecordingReference(request.CanonicalURL)
	require.NoError(t, err)
	require.Equal(t, "https://recordings.invalid/share/call?clip=1", canonicalURL)
	require.Equal(t, "https://recordings.invalid", origin)

	first, err := service.SubmitRemoteRecording(t.Context(), request)
	require.NoError(t, err)
	require.Equal(t, "unsupported", first.Outcome)
	require.Equal(t, "unprocessed", first.CoverageState)
	require.Equal(t, "succeeded", first.OperationState)
	require.Empty(t, first.SourceVersionID)

	secondRequest := request
	secondRequest.OperationID = uuid.New().String()
	secondRequest.CanonicalURL = "https://recordings.invalid:443/share/call?clip=1#another"
	secondRequest.Occurrence.Ref = "call-b"
	second, err := service.SubmitRemoteRecording(t.Context(), secondRequest)
	require.NoError(t, err)
	require.Equal(t, first.SourceID, second.SourceID)
	require.NotEqual(t, first.OccurrenceID, second.OccurrenceID)

	status, err := service.MediaStatus(t.Context(), first.SourceID)
	require.NoError(t, err)
	require.Equal(t, "unsupported", status.Outcome)
	require.Empty(t, status.SourceVersionID)
	require.Empty(t, status.ContentVersionID)
	projection, err := fixture.catalog.MediaSource(t.Context(), service.principal, first.SourceID)
	require.NoError(t, err)
	require.Equal(t, "remote_recording", projection.Kind)

	var metadata bytes.Buffer
	require.NoError(t, fixture.catalog.ExportMetadata(t.Context(), &metadata))
	require.Contains(t, metadata.String(), `"provider":"url"`)
	require.NotContains(t, metadata.String(), "recordings.invalid")
	require.NotContains(t, metadata.String(), "private.invalid")

	legacyService := newRemoteRecordingTestService(t, fixture, "operator:legacy", 0, remoteRecordingTestOrigins())
	legacyRequest := remoteRecordingTestRequest(uuid.New().String(),
		"https://recordings.invalid/legacy", "", "legacy")
	legacyRequest.ProviderHint = "synthetic"
	legacyRequest.CredentialBinding = "legacy-binding"
	legacy, err := legacyService.SubmitRemoteRecording(t.Context(), legacyRequest)
	require.NoError(t, err)
	require.Equal(t, "access_required", legacy.Outcome)
	restarted := newRemoteRecordingTestService(t, fixture, "operator:legacy", 0, nil)
	replayed, err := restarted.SubmitRemoteRecording(t.Context(), legacyRequest)
	require.NoError(t, err)
	require.Equal(t, legacy, replayed)
}

// Canonicalization is permanent source identity, so equivalent URL spellings
// must select the same source before any recording is imported.
func TestRemoteRecordingCanonicalURLIdentity(t *testing.T) {
	fixture := newPublicationFixture(t)
	service := newRemoteRecordingTestService(t, fixture, "operator:canonical", 0, nil)
	for _, tc := range []struct {
		name, first, second string
		same                bool
	}{
		{"https empty path", "https://example.com", "https://example.com/", true},
		{"http empty path", "http://example.com", "http://example.com/", true},
		{"unicode domain", "https://bücher.example/", "https://xn--bcher-kva.example/", true},
		{"unicode case mapping", "https://İ.example/", "https://xn--i-9bb.example/", true},
		{"trailing dot", "https://example.com./", "https://example.com/", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			first, err := service.SubmitRemoteRecording(t.Context(), remoteRecordingTestRequest(
				uuid.New().String(), tc.first, tc.first, tc.name+"-first"))
			require.NoError(t, err)
			second, err := service.SubmitRemoteRecording(t.Context(), remoteRecordingTestRequest(
				uuid.New().String(), tc.second, tc.second, tc.name+"-second"))
			require.NoError(t, err)
			if tc.same {
				require.Equal(t, first.SourceID, second.SourceID)
			} else {
				require.NotEqual(t, first.SourceID, second.SourceID)
			}
		})
	}
	_, err := service.SubmitRemoteRecording(t.Context(), remoteRecordingTestRequest(
		uuid.New().String(), "https://example.com/", "https://\u00ad/", "empty-normalized-host"))
	require.ErrorIs(t, err, ErrMediaPlanInvalid)
}

func TestRemoteRecordingManualImport(t *testing.T) {
	fixture := newPublicationFixture(t)
	service := newRemoteRecordingTestService(t, fixture, "operator:manual", 0, remoteRecordingTestOrigins())
	request := remoteRecordingTestRequest(uuid.New().String(),
		"https://recordings.invalid/call?token=synthetic-secret", "", "call")
	retained, err := service.SubmitRemoteRecording(t.Context(), request)
	require.NoError(t, err)
	require.Equal(t, "access_required", retained.Outcome)

	raw := mediatest.WAV()
	identity := processingSHA256(raw)
	artifactRequest := remoteRecordingTestArtifact(uuid.New().String(), retained.SourceID,
		retained.OccurrenceID, "call.wav", "audio/wav", identity, int64(len(raw)), raw)
	imported, err := service.ImportRecordingArtifact(t.Context(), artifactRequest)
	require.NoError(t, err)
	require.Equal(t, "content_available", imported.Outcome)
	require.Equal(t, "unprocessed", imported.CoverageState)
	require.Equal(t, "succeeded", imported.OperationState)
	require.Equal(t, retained.SourceID, imported.SourceID)
	require.Equal(t, retained.OccurrenceID, imported.OccurrenceID)
	require.NotEmpty(t, imported.SourceVersionID)
	require.NotEmpty(t, imported.ContentVersionID)
	require.NotEmpty(t, imported.SuppliedInputID)

	replayedRequest := artifactRequest
	replayedRequest.Content = bytes.NewReader(raw)
	replayed, err := service.ImportRecordingArtifact(t.Context(), replayedRequest)
	require.NoError(t, err)
	require.Equal(t, imported, replayed)

	rediscovered := request
	rediscovered.OperationID = uuid.New().String()
	var rediscoveredReceipt MediaReceipt
	rediscoveredReceipt, err = service.SubmitRemoteRecording(t.Context(), rediscovered)
	require.NoError(t, err)
	require.Equal(t, retained.SourceID, rediscoveredReceipt.SourceID)
	require.Equal(t, retained.OccurrenceID, rediscoveredReceipt.OccurrenceID)
	status, err := service.MediaStatus(t.Context(), retained.SourceID)
	require.NoError(t, err)
	require.Equal(t, imported.SourceVersionID, status.SourceVersionID)

	caption := []byte("WEBVTT\n\n00:00.000 --> 00:00.010\nsynthetic caption\n")
	captionIdentity := processingSHA256(caption)
	captionReceipt, err := service.ImportRecordingArtifact(t.Context(), MediaArtifactRequest{
		OperationID: uuid.New().String(), SourceID: retained.SourceID, OccurrenceID: retained.OccurrenceID,
		Kind: "caption", Origin: "supplied", Filename: "call.vtt", MediaType: "text/vtt",
		SHA256: captionIdentity, ByteLength: int64(len(caption)), Content: bytes.NewReader(caption),
	})
	require.NoError(t, err)
	require.Equal(t, imported.SourceVersionID, captionReceipt.SourceVersionID)
	require.Equal(t, "unprocessed", captionReceipt.CoverageState)
	require.NotEmpty(t, captionReceipt.SuppliedInputID)

	status, err = service.MediaStatus(t.Context(), retained.SourceID)
	require.NoError(t, err)
	require.Equal(t, "content_available", status.Outcome)
	require.Equal(t, imported.SourceVersionID, status.SourceVersionID)
	require.Equal(t, imported.ContentVersionID, status.ContentVersionID)
	require.Equal(t, "unprocessed", status.CoverageState)
	node, err := fixture.catalog.NodeByPath(t.Context(), "/media/"+retained.SourceID+"/"+identity+".wav")
	require.NoError(t, err)
	require.Equal(t, imported.ContentVersionID, node.CurrentVersionID)
	var metadata bytes.Buffer
	require.NoError(t, fixture.catalog.ExportMetadata(t.Context(), &metadata))
	require.Contains(t, metadata.String(), `"kind":"media"`)
	require.Contains(t, metadata.String(), `"kind":"caption"`)
}

func TestRemoteRecordingManualIsolation(t *testing.T) {
	fixture := newPublicationFixture(t)
	service := newRemoteRecordingTestService(t, fixture, "operator:isolation", 0, remoteRecordingTestOrigins())
	retained, err := service.SubmitRemoteRecording(t.Context(), remoteRecordingTestRequest(
		uuid.New().String(), "https://recordings.invalid/isolation", "", "first"))
	require.NoError(t, err)
	second, err := service.DeclareMediaOccurrence(t.Context(), uuid.New().String(), retained.SourceID,
		MediaOccurrenceInput{Ref: "second", Revision: "1", Filename: "second.wav"})
	require.NoError(t, err)

	firstRaw := mediatest.WAV()
	secondRaw := append([]byte(nil), firstRaw...)
	secondRaw[len(secondRaw)-1]++
	firstHash, secondHash := processingSHA256(firstRaw), processingSHA256(secondRaw)
	firstImport, err := service.ImportRecordingArtifact(t.Context(), remoteRecordingTestArtifact(
		uuid.New().String(), retained.SourceID, retained.OccurrenceID, "first.wav", "audio/wav",
		firstHash, int64(len(firstRaw)), firstRaw))
	require.NoError(t, err)
	secondImport, err := service.ImportRecordingArtifact(t.Context(), remoteRecordingTestArtifact(
		uuid.New().String(), retained.SourceID, second.OccurrenceID, "second.wav", "audio/wav",
		secondHash, int64(len(secondRaw)), secondRaw))
	require.NoError(t, err)
	require.NotEqual(t, firstImport.SourceVersionID, secondImport.SourceVersionID)

	changed, err := service.ImportRecordingArtifact(t.Context(), remoteRecordingTestArtifact(
		uuid.New().String(), retained.SourceID, retained.OccurrenceID, "first.wav", "audio/wav",
		secondHash, int64(len(secondRaw)), secondRaw))
	require.ErrorIs(t, err, store.ErrMediaSourceConflict)
	require.Empty(t, changed.SourceVersionID)

	other := newRemoteRecordingTestService(t, fixture, "operator:other", 0, nil)
	_, err = other.ImportRecordingArtifact(t.Context(), remoteRecordingTestArtifact(
		uuid.New().String(), retained.SourceID, retained.OccurrenceID, "first.wav", "audio/wav",
		firstHash, int64(len(firstRaw)), firstRaw))
	require.ErrorIs(t, err, store.ErrNotFound)

	mismatched, err := service.SubmitRemoteRecording(t.Context(), remoteRecordingTestRequest(
		uuid.New().String(), "https://recordings.invalid/mismatched", "", "mismatched"))
	require.NoError(t, err)
	_, err = service.ImportRecordingArtifact(t.Context(), remoteRecordingTestArtifact(
		uuid.New().String(), retained.SourceID, mismatched.OccurrenceID, "mismatch.wav", "audio/wav",
		firstHash, int64(len(firstRaw)), firstRaw))
	require.ErrorIs(t, err, store.ErrNotFound)

	revoked, err := service.SubmitRemoteRecording(t.Context(), remoteRecordingTestRequest(
		uuid.New().String(), "https://recordings.invalid/revoked", "", "revoked"))
	require.NoError(t, err)
	_, err = service.RevokeMediaOccurrence(t.Context(), uuid.New().String(), revoked.OccurrenceID, "1")
	require.NoError(t, err)
	_, err = service.ImportRecordingArtifact(t.Context(), remoteRecordingTestArtifact(
		uuid.New().String(), revoked.SourceID, revoked.OccurrenceID, "revoked.wav", "audio/wav",
		firstHash, int64(len(firstRaw)), firstRaw))
	require.ErrorIs(t, err, store.ErrNotFound)

	racing, err := service.SubmitRemoteRecording(t.Context(), remoteRecordingTestRequest(
		uuid.New().String(), "https://recordings.invalid/racing", "", "racing"))
	require.NoError(t, err)
	raceCtx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	var wait sync.WaitGroup
	importErr := make(chan error, 1)
	wait.Go(func() {
		_, importErrValue := service.ImportRecordingArtifact(raceCtx, remoteRecordingTestArtifact(
			uuid.New().String(), racing.SourceID, racing.OccurrenceID, "racing.wav", "audio/wav",
			firstHash, int64(len(firstRaw)), firstRaw))
		importErr <- importErrValue
	})
	revokeErr := make(chan error, 1)
	wait.Go(func() {
		_, revokeErrValue := service.RevokeMediaOccurrence(raceCtx, uuid.New().String(), racing.OccurrenceID, "1")
		revokeErr <- revokeErrValue
	})
	wait.Wait()
	require.NoError(t, <-revokeErr)
	importResult := <-importErr
	require.True(t, importResult == nil || errors.Is(importResult, store.ErrNotFound), importResult)
	_, err = fixture.catalog.MediaOccurrence(t.Context(), service.principal, racing.OccurrenceID)
	require.ErrorIs(t, err, store.ErrNotFound)
}

func TestRemoteRecordingManualBounds(t *testing.T) {
	wav := mediatest.WAV()
	mp3 := mediatest.MP3()
	maximum := int64(max(len(wav), len(mp3)))
	tests := []struct {
		name, filename, mediaType string
		content                   []byte
		expectedHash              string
		length                    int64
		maximum                   int64
	}{
		{name: "oversize", filename: "oversize.wav", mediaType: "audio/wav",
			content: append(append([]byte(nil), wav...), 0), expectedHash: "",
			length: int64(len(wav) + 1), maximum: int64(len(wav))},
		{name: "short", filename: "short.wav", mediaType: "audio/wav",
			content: wav[:len(wav)-1], expectedHash: processingSHA256(wav),
			length: int64(len(wav)), maximum: maximum},
		{name: "changed digest", filename: "changed.wav", mediaType: "audio/wav",
			content: wav, expectedHash: processingSHA256([]byte("changed digest")),
			length: int64(len(wav)), maximum: maximum},
		{name: "text disguised as WAV", filename: "text.wav", mediaType: "audio/wav",
			content: []byte("synthetic text is not audio"), expectedHash: "",
			length: int64(len([]byte("synthetic text is not audio"))), maximum: maximum},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newPublicationFixture(t)
			service := newRemoteRecordingTestService(t, fixture, "operator:bounds", test.maximum,
				remoteRecordingTestOrigins())
			retained, err := service.SubmitRemoteRecording(t.Context(), remoteRecordingTestRequest(
				uuid.New().String(), "https://recordings.invalid/bounds/"+test.name, "", test.name))
			require.NoError(t, err)
			digest := test.expectedHash
			if digest == "" {
				digest = processingSHA256(test.content)
			}
			_, err = service.ImportRecordingArtifact(t.Context(), remoteRecordingTestArtifact(
				uuid.New().String(), retained.SourceID, retained.OccurrenceID, test.filename,
				test.mediaType, digest, test.length, test.content))
			require.Error(t, err)
			assertNoRemoteOriginal(t, fixture, service, retained.SourceID, digest)
			require.Zero(t, service.mediaStagedBytes)
		})
	}

	overlong := overlongRemoteWAV()
	fixture := newPublicationFixture(t)
	service := newRemoteRecordingTestService(t, fixture, "operator:duration", int64(len(overlong))+1,
		remoteRecordingTestOrigins())
	retained, err := service.SubmitRemoteRecording(t.Context(), remoteRecordingTestRequest(
		uuid.New().String(), "https://recordings.invalid/overlong", "", "overlong"))
	require.NoError(t, err)
	_, err = service.ImportRecordingArtifact(t.Context(), remoteRecordingTestArtifact(
		uuid.New().String(), retained.SourceID, retained.OccurrenceID, "overlong.wav", "audio/wav",
		processingSHA256(overlong), int64(len(overlong)), overlong))
	require.Error(t, err)
	assertNoRemoteOriginal(t, fixture, service, retained.SourceID, processingSHA256(overlong))
	require.Zero(t, service.mediaStagedBytes)

	fixture = newPublicationFixture(t)
	service = newRemoteRecordingTestService(t, fixture, "operator:mp3", maximum, remoteRecordingTestOrigins())
	retained, err = service.SubmitRemoteRecording(t.Context(), remoteRecordingTestRequest(
		uuid.New().String(), "https://recordings.invalid/parameters", "", "parameters"))
	require.NoError(t, err)
	identity := processingSHA256(mp3)
	accepted, err := service.ImportRecordingArtifact(t.Context(), remoteRecordingTestArtifact(
		uuid.New().String(), retained.SourceID, retained.OccurrenceID, "parameters.mp3",
		"audio/mpeg; charset=synthetic", identity, int64(len(mp3)), mp3))
	require.NoError(t, err)
	require.Equal(t, "content_available", accepted.Outcome)
	require.NotEmpty(t, accepted.ContentVersionID)
	require.Zero(t, service.mediaStagedBytes)
}

func TestRemoteRecordingManualStatus(t *testing.T) {
	fixture := newPublicationFixture(t)
	service := newRemoteRecordingTestService(t, fixture, "operator:status-remote", 0,
		remoteRecordingTestOrigins())
	retained, err := service.SubmitRemoteRecording(t.Context(), remoteRecordingTestRequest(
		uuid.New().String(), "https://recordings.invalid/status", "", "status-a"))
	require.NoError(t, err)
	initial, err := service.MediaStatus(t.Context(), retained.SourceID)
	require.NoError(t, err)
	require.Equal(t, "access_required", initial.Outcome)
	require.Empty(t, initial.SourceVersionID)
	require.Empty(t, initial.ContentVersionID)

	second, err := service.DeclareMediaOccurrence(t.Context(), uuid.New().String(), retained.SourceID,
		MediaOccurrenceInput{Ref: "status-b", Revision: "1", Filename: "status-b.wav"})
	require.NoError(t, err)
	firstRaw := mediatest.WAV()
	secondRaw := append([]byte(nil), firstRaw...)
	secondRaw[len(secondRaw)-1]++
	firstHash, secondHash := processingSHA256(firstRaw), processingSHA256(secondRaw)
	firstImport, err := service.ImportRecordingArtifact(t.Context(), remoteRecordingTestArtifact(
		uuid.New().String(), retained.SourceID, retained.OccurrenceID, "status-a.wav", "audio/wav",
		firstHash, int64(len(firstRaw)), firstRaw))
	require.NoError(t, err)
	secondImport, err := service.ImportRecordingArtifact(t.Context(), remoteRecordingTestArtifact(
		uuid.New().String(), retained.SourceID, second.OccurrenceID, "status-b.wav", "audio/wav",
		secondHash, int64(len(secondRaw)), secondRaw))
	require.NoError(t, err)

	firstTranscript := importRemoteTranscript(t, service, retained.SourceID, retained.OccurrenceID,
		firstImport.SourceVersionID, "first exact transcript")
	secondTranscript := importRemoteTranscript(t, service, retained.SourceID, second.OccurrenceID,
		secondImport.SourceVersionID, "second exact transcript")
	firstOperation := queueRemoteAttempt(t, fixture.catalog, service, retained.SourceID, firstImport, firstTranscript,
		"first-attempt")
	_, err = fixture.catalog.FinishMediaProcessing(t.Context(), firstOperation.OperationID, service.principal, true)
	require.NoError(t, err)
	status, err := service.MediaStatus(t.Context(), retained.SourceID)
	require.NoError(t, err)
	require.Equal(t, secondImport.SourceVersionID, status.SourceVersionID)
	require.Equal(t, "unprocessed", status.CoverageState)

	secondOperation := queueRemoteAttempt(t, fixture.catalog, service, retained.SourceID, secondImport,
		secondTranscript, "second-success-final")
	_, err = fixture.catalog.FinishMediaProcessing(t.Context(), secondOperation.OperationID, service.principal, true)
	require.NoError(t, err)
	failedRetry := queueRemoteAttempt(t, fixture.catalog, service, retained.SourceID, secondImport,
		secondTranscript, "second-failed")
	_, err = fixture.catalog.FailMediaProcessing(t.Context(), failedRetry.OperationID, service.principal)
	require.NoError(t, err)
	status, err = service.MediaStatus(t.Context(), retained.SourceID)
	require.NoError(t, err)
	require.Equal(t, failedRetry.OperationID, status.OperationID)
	require.Equal(t, "failed", status.OperationState)
	require.Equal(t, "transcribed", status.CoverageState)
	require.Equal(t, secondTranscript, status.SuppliedInputID)
	page, err := service.ListMediaSources(t.Context(), MediaListOptions{Limit: 10})
	require.NoError(t, err)
	require.Len(t, page.Items, 1)
	require.Equal(t, "transcribed", page.Items[0].CoverageState)
}

func TestRemoteRecordingEqualBytesKeepTranscriptBindingsBySource(t *testing.T) {
	fixture := newPublicationFixture(t)
	service := newRemoteRecordingTestService(t, fixture, "operator:equal-bytes", 0, nil)
	raw := mediatest.WAV()
	hash := processingSHA256(raw)
	first, err := service.SubmitRemoteRecording(t.Context(), remoteRecordingTestRequest(
		uuid.New().String(), "https://private.invalid/a", "https://recordings.invalid/a", "a"))
	require.NoError(t, err)
	second, err := service.SubmitRemoteRecording(t.Context(), remoteRecordingTestRequest(
		uuid.New().String(), "https://private.invalid/b", "https://recordings.invalid/b", "b"))
	require.NoError(t, err)
	firstImport, err := service.ImportRecordingArtifact(t.Context(), remoteRecordingTestArtifact(
		uuid.New().String(), first.SourceID, first.OccurrenceID, "a.wav", "audio/wav", hash, int64(len(raw)), raw))
	require.NoError(t, err)
	secondImport, err := service.ImportRecordingArtifact(t.Context(), remoteRecordingTestArtifact(
		uuid.New().String(), second.SourceID, second.OccurrenceID, "b.wav", "audio/wav", hash, int64(len(raw)), raw))
	require.NoError(t, err)
	require.NotEqual(t, firstImport.ContentVersionID, secondImport.ContentVersionID)
	secondTranscript := importRemoteTranscript(t, service, second.SourceID, second.OccurrenceID,
		secondImport.SourceVersionID, "second source transcript")
	firstSource, firstSourceVersion, err := fixture.catalog.MediaSourceBindingForContentVersion(
		t.Context(), service.principal, firstImport.ContentVersionID)
	require.NoError(t, err)
	secondSource, secondSourceVersion, err := fixture.catalog.MediaSourceBindingForContentVersion(
		t.Context(), service.principal, secondImport.ContentVersionID)
	require.NoError(t, err)
	require.Equal(t, first.SourceID, firstSource)
	require.Equal(t, firstImport.SourceVersionID, firstSourceVersion)
	require.Equal(t, second.SourceID, secondSource)
	require.Equal(t, secondImport.SourceVersionID, secondSourceVersion)
	_, err = service.resolveMediaInputBinding(t.Context(), SuppliedMediaProfileName, hash,
		mediaSourceBinding{sourceID: first.SourceID, sourceVersionID: firstImport.SourceVersionID}, "")
	require.ErrorIs(t, err, store.ErrNotFound)
	bound, err := service.resolveMediaInputBinding(t.Context(), SuppliedMediaProfileName, hash,
		mediaSourceBinding{sourceID: second.SourceID, sourceVersionID: secondImport.SourceVersionID}, "")
	require.NoError(t, err)
	require.Equal(t, secondTranscript, bound)
}

func importRemoteTranscript(
	t *testing.T, service *Service, sourceID, occurrenceID, sourceVersionID, text string,
) string {
	t.Helper()
	content := []byte(text + "\n")
	receipt, err := service.ImportRecordingArtifact(t.Context(), MediaArtifactRequest{
		OperationID: uuid.New().String(), SourceID: sourceID, OccurrenceID: occurrenceID,
		Kind: "transcript", Origin: "supplied", Filename: "transcript.txt", MediaType: "text/plain",
		SHA256: processingSHA256(content), ByteLength: int64(len(content)), Content: bytes.NewReader(content),
	})
	require.NoError(t, err)
	require.Equal(t, sourceVersionID, receipt.SourceVersionID)
	return receipt.SuppliedInputID
}

func queueRemoteAttempt(
	t *testing.T, catalog *store.Store, service *Service, sourceID string,
	retained MediaReceipt, inputID, suffix string,
) store.MediaPublicationReceipt {
	t.Helper()
	operation := store.MediaOperation{ID: uuid.New().String(), Principal: service.principal,
		Verb: "retry_media", RequestSHA256: processingHash(suffix), SourceID: sourceID}
	receipt := store.MediaPublicationReceipt{VaultUID: retained.VaultUID, SourceID: sourceID,
		SourceVersionID: retained.SourceVersionID, ContentVersionID: retained.ContentVersionID,
		OccurrenceID: retained.OccurrenceID, OperationID: operation.ID, OperationState: "queued",
		CoverageState: "pending", ProcessingProfile: "speech", SuppliedInputID: inputID}
	_, err := catalog.QueueMediaRetry(t.Context(), operation, receipt)
	require.NoError(t, err)
	return receipt
}

func assertNoRemoteOriginal(
	t *testing.T, fixture publicationFixture, service *Service, sourceID, digest string,
) {
	t.Helper()
	status, err := service.MediaStatus(t.Context(), sourceID)
	require.NoError(t, err)
	require.Empty(t, status.SourceVersionID)
	require.Empty(t, status.ContentVersionID)
	node, err := fixture.catalog.NodeByPath(t.Context(), "/media/"+sourceID+"/"+digest+".wav")
	require.ErrorIs(t, err, store.ErrNotFound)
	require.Empty(t, node.ID)
	present, err := fixture.catalog.HasBlob(t.Context(), digest)
	require.NoError(t, err)
	require.False(t, present)
}

func overlongRemoteWAV() []byte {
	const audioBytes = 86_401
	data := make([]byte, 44+audioBytes)
	copy(data[0:4], "RIFF")
	binary.LittleEndian.PutUint32(data[4:8], uint32(len(data)-8))
	copy(data[8:12], "WAVE")
	copy(data[12:16], "fmt ")
	binary.LittleEndian.PutUint32(data[16:20], 16)
	binary.LittleEndian.PutUint16(data[20:22], 1)
	binary.LittleEndian.PutUint16(data[22:24], 1)
	binary.LittleEndian.PutUint32(data[24:28], 1)
	binary.LittleEndian.PutUint32(data[28:32], 1)
	binary.LittleEndian.PutUint16(data[32:34], 1)
	binary.LittleEndian.PutUint16(data[34:36], 8)
	copy(data[36:40], "data")
	binary.LittleEndian.PutUint32(data[40:44], audioBytes)
	return data
}
