package processing

import (
	"bytes"
	"testing"
	"uuid"

	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/document/media/mediatest"
	"go.kenn.io/docbank/internal/retrieval"
	"go.kenn.io/docbank/internal/store"
)

func TestMediaTranscriptRequiresCanonicalContentVersionID(t *testing.T) {
	t.Parallel()
	fixture := newPublicationFixture(t)
	service, err := NewService(ServiceConfig{Catalog: fixture.catalog, Blobs: fixture.blobs,
		Gate: newWorkerTestGate(), SpoolDirectory: t.TempDir(), Principal: "operator:transcript"})
	require.NoError(t, err)
	for _, contentVersionID := range []string{
		"",
		"not-a-uuid",
		"00000000-0000-4000-8000-00000000000A",
		"00000000-0000-7000-8000-000000000001",
		"00000000-0000-4000-0000-000000000001",
	} {
		_, err = service.MediaTranscript(t.Context(), MediaTranscriptRequest{
			SourceID: "source", SourceVersionID: "version", ContentVersionID: contentVersionID})
		require.ErrorIs(t, err, ErrMediaTranscriptInvalid, contentVersionID)
	}
	_, err = service.MediaTranscript(t.Context(), MediaTranscriptRequest{
		SourceID: "source", SourceVersionID: "version", ContentVersionID: "00000000-0000-4000-8000-000000000001"})
	require.ErrorIs(t, err, store.ErrNotFound, "a canonical UUIDv4 passes validation and reaches the lookup")
}

func TestMediaTranscriptUsesCoverageProfileDuringRetry(t *testing.T) {
	t.Parallel()
	coverage := store.MediaPublicationReceipt{OperationState: "succeeded",
		ProcessingProfile: "generated-media", ProcessingProfileFingerprint: "generated-fingerprint",
	}
	processing := store.MediaPublicationReceipt{OperationState: "queued",
		ProcessingProfile: SuppliedCaptionProfileName, ProcessingProfileFingerprint: "supplied-fingerprint",
	}
	profile := func() string {
		t.Helper()
		item := store.MediaSourceProjection{ProcessingReceipts: []store.MediaPublicationReceipt{processing, coverage}}
		fingerprint, err := (&Service{}).mediaTranscriptProfile(t.Context(), item)
		require.NoError(t, err)
		return fingerprint
	}
	require.Equal(t, "generated-fingerprint", profile())
	coverage.ProcessingProfile, coverage.ProcessingProfileFingerprint = "removed-profile", ""
	require.Empty(t, profile(), "a later retry cannot stand in for the covering profile")
}

func TestMediaTranscriptMismatchedContentVersionReturnsStaleWithoutText(t *testing.T) {
	t.Parallel()
	fixture := newPublicationFixture(t)
	raw := mediatest.WAV()
	written, err := fixture.blobs.WriteDetailedContext(t.Context(), bytes.NewReader(raw))
	require.NoError(t, err)
	node, err := fixture.catalog.CreateFile(t.Context(), fixture.catalog.RootID(), "call.wav", written.Hash, written.Size,
		"audio/wav", processingBlobPhysical(t, written))
	require.NoError(t, err)
	service, err := NewService(ServiceConfig{Catalog: fixture.catalog, Blobs: fixture.blobs,
		Gate: newWorkerTestGate(), SpoolDirectory: t.TempDir(), Principal: "operator:transcript"})
	require.NoError(t, err)
	retained, err := service.SubmitSuppliedMedia(t.Context(), SuppliedMediaRequest{
		OperationID: uuid.New().String(), Filename: "call.wav", MediaType: "audio/wav", SHA256: written.Hash,
		ByteLength: written.Size, ExistingContentVersionID: node.CurrentVersionID,
		Occurrence: MediaOccurrenceInput{Ref: "call", Revision: "1"},
	})
	require.NoError(t, err)
	wrong := uuid.New().String()
	result, err := service.MediaTranscript(t.Context(), MediaTranscriptRequest{
		SourceID: retained.SourceID, SourceVersionID: retained.SourceVersionID, ContentVersionID: wrong,
	})
	require.NoError(t, err)
	require.Equal(t, mediaTranscriptEvidenceStale, result.EvidenceState)
	require.Nil(t, result.Transcript)
	require.Equal(t, wrong, result.ContentVersionID)
}

type captionFixture struct {
	fixture        publicationFixture
	base, captions *Service
	remote         MediaReceipt
	video          []byte
	videoReceipt   MediaReceipt
	profile        string
}

func newCaptionFixture(t *testing.T, ref string) captionFixture {
	t.Helper()
	fixture := newPublicationFixture(t)
	base := newRemoteRecordingTestService(t, fixture, "operator:"+ref, 0, nil)
	remote := loomRemoteForTest(t, base, ref)
	video := mediatest.H264AACMP4()
	videoReceipt, err := base.ImportRecordingArtifact(t.Context(), MediaArtifactRequest{
		OperationID: uuid.New().String(), SourceID: remote.SourceID, OccurrenceID: remote.OccurrenceID,
		Kind: "media", Origin: "supplied", Provider: "synthetic", Filename: "call.mp4", MediaType: "video/mp4",
		SHA256: processingSHA256(video), ByteLength: int64(len(video)), Content: bytes.NewReader(video),
	})
	require.NoError(t, err)
	name, profile, err := NewSuppliedCaptionProfile(fixture.catalog, fixture.blobs, base.principal)
	require.NoError(t, err)
	captions, err := NewService(ServiceConfig{Catalog: fixture.catalog, Blobs: fixture.blobs,
		Gate: newWorkerTestGate(), SpoolDirectory: t.TempDir(), Principal: base.principal,
		Profiles: map[string]ProfileConfig{name: profile}})
	require.NoError(t, err)
	version, err := fixture.catalog.ContentVersionByID(t.Context(), videoReceipt.ContentVersionID)
	require.NoError(t, err)
	selector := Selector{NodeID: version.NodeID, ContentVersionID: version.ID, Profile: name}
	plan, err := captions.Plan(t.Context(), selector)
	require.NoError(t, err)
	_, err = captions.GrantConsent(t.Context(), ConsentGrantRequest{Selector: selector, PlanFingerprint: plan.Fingerprint})
	require.NoError(t, err)
	return captionFixture{fixture: fixture, base: base, captions: captions, remote: remote, video: video,
		videoReceipt: videoReceipt, profile: name}
}

// caption imports one SRT cue for an occurrence and queues its processing.
func (f captionFixture) caption(t *testing.T, occurrenceID, cue string) MediaReceipt {
	t.Helper()
	srt := []byte("1\n00:00:00,000 --> 00:00:01,000\n" + cue + "\n")
	artifact, err := f.base.ImportRecordingArtifact(t.Context(), MediaArtifactRequest{
		OperationID: uuid.New().String(), SourceID: f.remote.SourceID, OccurrenceID: occurrenceID,
		Kind: "caption", Origin: "supplied", Provider: "synthetic", Filename: "call.srt",
		MediaType: "application/x-subrip", SHA256: processingSHA256(srt), ByteLength: int64(len(srt)),
		Content: bytes.NewReader(srt),
	})
	require.NoError(t, err)
	queued, err := f.captions.RetryMedia(t.Context(), uuid.New().String(), f.remote.SourceID,
		MediaProcessingRequest{Profile: f.profile, SuppliedInputID: artifact.SuppliedInputID})
	require.NoError(t, err)
	return queued
}

func TestMediaTranscriptFollowsCoverageAndTheCurrentFile(t *testing.T) {
	t.Parallel()
	f := newCaptionFixture(t, "transcript-read")
	request := MediaTranscriptRequest{SourceID: f.remote.SourceID,
		SourceVersionID: f.videoReceipt.SourceVersionID, ContentVersionID: f.videoReceipt.ContentVersionID}
	read := func() MediaTranscript {
		t.Helper()
		result, err := f.captions.MediaTranscript(t.Context(), request)
		require.NoError(t, err)
		return result
	}
	initial := read()
	require.Equal(t, mediaTranscriptEvidenceUnavailable, initial.EvidenceState)
	require.Equal(t, "unprocessed", initial.CoverageState)
	require.Nil(t, initial.Transcript)

	queued := f.caption(t, f.remote.OccurrenceID, "synthetic cue")
	pending := read()
	require.Equal(t, mediaTranscriptEvidencePending, pending.EvidenceState)
	require.Equal(t, "queued", pending.OperationState)
	require.Nil(t, pending.Transcript)
	runLoomRenditionJob(t, f.captions, queued.JobID)
	ready := read()
	require.Equal(t, mediaTranscriptEvidenceReady, ready.EvidenceState)
	require.Equal(t, "transcribed", ready.CoverageState)
	require.NotNil(t, ready.Transcript)
	require.Equal(t, "supplied", ready.Transcript.Origin)
	active, err := f.fixture.catalog.ActiveRendition(t.Context(), request.ContentVersionID,
		f.captions.profiles[f.profile].record.Fingerprint)
	require.NoError(t, err)
	require.Equal(t, active.Build.ID, ready.Transcript.BuildID)
	require.Equal(t, queued.SuppliedInputID, ready.Transcript.SuppliedInputID)
	require.Len(t, ready.Transcript.Units, 1)
	require.Equal(t, "synthetic cue", ready.Transcript.Units[0].Text)
	require.Equal(t, &retrieval.MediaTimeSpan{StartMS: 0, EndMS: 1000}, ready.Transcript.Units[0].TimeSpan)

	failedQueued := f.caption(t, f.remote.OccurrenceID, "caf\xe9")
	runLoomRenditionJob(t, f.captions, failedQueued.JobID)
	failed := read()
	require.Equal(t, mediaTranscriptEvidenceReady, failed.EvidenceState, "a failed retry keeps covering evidence")
	require.Equal(t, "failed", failed.OperationState)
	require.Equal(t, "synthetic cue", failed.Transcript.Units[0].Text)
	require.Equal(t, ready.Transcript.BuildID, failed.Transcript.BuildID)
	require.Equal(t, ready.Transcript.SuppliedInputID, failed.Transcript.SuppliedInputID)
	require.NotEqual(t, failedQueued.SuppliedInputID, failed.Transcript.SuppliedInputID)

	replacement := f.caption(t, f.remote.OccurrenceID, "replacement cue")
	require.Equal(t, request.SourceVersionID, replacement.SourceVersionID)
	require.Equal(t, request.ContentVersionID, replacement.ContentVersionID)
	runLoomRenditionJob(t, f.captions, replacement.JobID)
	updated := read()
	require.Equal(t, "replacement cue", updated.Transcript.Units[0].Text)
	require.NotEqual(t, ready.Transcript.BuildID, updated.Transcript.BuildID)
	require.Equal(t, replacement.SuppliedInputID, updated.Transcript.SuppliedInputID)

	version, err := f.fixture.catalog.ContentVersionByID(t.Context(), request.ContentVersionID)
	require.NoError(t, err)
	node, err := f.fixture.catalog.NodeByID(t.Context(), version.NodeID)
	require.NoError(t, err)
	changed := append([]byte(nil), f.video...)
	changed[len(changed)-1]++
	changedBlob, err := f.fixture.blobs.WriteDetailedContext(t.Context(), bytes.NewReader(changed))
	require.NoError(t, err)
	node, _, err = f.fixture.catalog.ReplaceContent(t.Context(), node.ID, node.Revision,
		changedBlob.Hash, changedBlob.Size, "video/mp4", processingBlobPhysical(t, changedBlob))
	require.NoError(t, err)
	replaced := read()
	require.Equal(t, mediaTranscriptEvidenceStale, replaced.EvidenceState)
	require.Nil(t, replaced.Transcript)

	_, _, err = f.fixture.catalog.Trash(t.Context(), node.ID, node.Revision)
	require.NoError(t, err)
	trashed := read()
	require.Equal(t, mediaTranscriptEvidenceUnavailable, trashed.EvidenceState)
	require.Nil(t, trashed.Transcript)
}

func TestMediaTranscriptGeneratedBuildHasNoSuppliedInput(t *testing.T) {
	t.Parallel()
	f := newMediaStateFixture(t)
	retained, err := f.service.SubmitSuppliedMedia(t.Context(),
		f.suppliedRequest(uuid.New().String(), &MediaProcessingRequest{Profile: "speech"}))
	require.NoError(t, err)
	require.NoError(t, f.run(t, retained.JobID))
	result, err := f.service.MediaTranscript(t.Context(), MediaTranscriptRequest{
		SourceID: retained.SourceID, SourceVersionID: retained.SourceVersionID,
		ContentVersionID: retained.ContentVersionID})
	require.NoError(t, err)
	require.Equal(t, mediaTranscriptEvidenceReady, result.EvidenceState)
	active, err := f.catalog.ActiveRendition(t.Context(), retained.ContentVersionID,
		f.service.profiles["speech"].record.Fingerprint)
	require.NoError(t, err)
	require.Equal(t, active.Build.ID, result.Transcript.BuildID)
	require.Equal(t, "generated", result.Transcript.Origin)
	require.Empty(t, result.Transcript.SuppliedInputID)
}

func TestMediaTranscriptSharedRecordingKeepsForeignCaptionOut(t *testing.T) {
	t.Parallel()
	f := newCaptionFixture(t, "transcript-shared")
	queuedA := f.caption(t, f.remote.OccurrenceID, "caption A")
	runLoomRenditionJob(t, f.captions, queuedA.JobID)

	occurrenceB, sourceVersionB := "occurrence-shared-b", "source-version-shared-b"
	_, err := f.fixture.catalog.RecordMediaOccurrence(t.Context(), store.MediaOperation{
		ID: uuid.New().String(), Principal: f.base.principal, Verb: "declare_occurrence",
		RequestSHA256: processingSHA256([]byte(occurrenceB)), SourceID: f.remote.SourceID,
	}, store.MediaOccurrenceInput{ID: occurrenceB, SourceID: f.remote.SourceID, Principal: f.base.principal,
		Ref: occurrenceB, Revision: "1", Filename: "call.mp4", MessageJSON: "{}"})
	require.NoError(t, err)
	require.NoError(t, f.fixture.catalog.PublishMediaSourceVersion(t.Context(), store.MediaSourceVersionInput{
		ID: sourceVersionB, SourceID: f.remote.SourceID, Revision: 2, ExpectedHeadRevision: 1,
		ContentVersionID: f.videoReceipt.ContentVersionID, CaptureJSON: "{}",
		BindOccurrenceIDs: []string{occurrenceB},
	}))
	queuedB := f.caption(t, occurrenceB, "caption B")
	require.Equal(t, sourceVersionB, queuedB.SourceVersionID)
	runLoomRenditionJob(t, f.captions, queuedB.JobID)

	requestA := MediaTranscriptRequest{SourceID: f.remote.SourceID, SourceVersionID: queuedA.SourceVersionID,
		ContentVersionID: f.videoReceipt.ContentVersionID}
	requestB := requestA
	requestB.SourceVersionID = sourceVersionB
	a, err := f.captions.MediaTranscript(t.Context(), requestA)
	require.NoError(t, err)
	require.Equal(t, mediaTranscriptEvidenceStale, a.EvidenceState, "version A must not read caption B")
	require.Nil(t, a.Transcript)
	b, err := f.captions.MediaTranscript(t.Context(), requestB)
	require.NoError(t, err)
	require.Equal(t, mediaTranscriptEvidenceReady, b.EvidenceState)
	require.Equal(t, "caption B", b.Transcript.Units[0].Text)

	_, err = f.captions.RevokeMediaOccurrence(t.Context(), uuid.New().String(), occurrenceB, "1")
	require.NoError(t, err)
	_, err = f.captions.MediaTranscript(t.Context(), requestB)
	require.ErrorIs(t, err, store.ErrNotFound)
}
