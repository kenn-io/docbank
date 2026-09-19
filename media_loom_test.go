package docbank

import (
	"bytes"
	"io"
	"testing"
	"time"
	"uuid"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/media/mediatest"
	"go.kenn.io/docbank/document/mediatranscript"
	internalprocessing "go.kenn.io/docbank/internal/processing"
)

func TestLoomManualExportEmbedded(t *testing.T) {
	runLoomManualExportEmbedded(t)
}

func TestLoomCaptionSearchHonorsExactFence(t *testing.T) {
	vault, err := New(t.Context(), Config{Root: t.TempDir()})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, vault.Close()) })
	type published struct {
		remote, original MediaReceipt
		selector         ProcessingSelector
		phrase           string
		occurrenceID     string
	}
	publish := func(ref, phrase, start, end string) published {
		remote, submitErr := vault.SubmitRemoteRecording(t.Context(), RemoteRecordingRequest{
			OperationID: uuid.New().String(), ReferenceURL: "https://private.invalid/" + ref,
			CanonicalURL: "https://www.loom.com/share/" + ref,
			Occurrence:   MediaOccurrenceInput{Ref: ref, Revision: "1", Filename: "loom.mp4"},
		})
		require.NoError(t, submitErr)
		video := mediatest.H264AACMP4()
		videoID := contentIdentity(video)
		original, importErr := vault.ImportRecordingArtifact(t.Context(), MediaArtifactRequest{
			OperationID: uuid.New().String(), SourceID: remote.SourceID, OccurrenceID: remote.OccurrenceID,
			Kind: "media", Origin: "supplied", Provider: "loom", Filename: "loom.mp4", MediaType: "video/mp4",
			SHA256: videoID.SHA256, ByteLength: videoID.Size, Content: bytes.NewReader(video),
		})
		require.NoError(t, importErr)
		srt := []byte("1\n" + start + " --> " + end + "\n" + phrase + "\n")
		captionID := contentIdentity(srt)
		caption, importErr := vault.ImportRecordingArtifact(t.Context(), MediaArtifactRequest{
			OperationID: uuid.New().String(), SourceID: remote.SourceID, OccurrenceID: remote.OccurrenceID,
			Kind: "caption", Origin: "supplied", Provider: "loom", Filename: "loom.srt",
			MediaType: "application/x-subrip", SHA256: captionID.SHA256, ByteLength: captionID.Size,
			Content: bytes.NewReader(srt),
		})
		require.NoError(t, importErr)
		node, statErr := vault.Stat(t.Context(), "/media/"+remote.SourceID+"/"+videoID.SHA256+".mp4")
		require.NoError(t, statErr)
		selector := ProcessingSelector{NodeID: node.ID, ContentVersionID: original.ContentVersionID,
			Profile: "supplied-captions"}
		plan, planErr := vault.PlanProcessing(t.Context(), ProcessingPlanRequest{Selector: selector})
		require.NoError(t, planErr)
		_, grantErr := vault.GrantProcessingPlanConsent(t.Context(), ProcessingConsentGrantRequest{
			PlanRequest: ProcessingPlanRequest{Selector: selector}, PlanFingerprint: plan.Fingerprint})
		require.NoError(t, grantErr)
		_, retryErr := vault.RetryMedia(t.Context(), uuid.New().String(), remote.SourceID,
			MediaProcessingRequest{Profile: "supplied-captions", SuppliedInputID: caption.SuppliedInputID})
		require.NoError(t, retryErr)
		return published{remote: remote, original: original, selector: selector, phrase: phrase,
			occurrenceID: remote.OccurrenceID}
	}
	first := publish("fence-a", "shared fence phrase", "00:00:01,000", "00:00:02,000")
	second := publish("fence-b", "shared fence phrase", "00:00:03,000", "00:00:04,000")
	for _, item := range []published{first, second} {
		require.Eventually(t, func() bool {
			status, statusErr := vault.MediaStatus(t.Context(), item.remote.SourceID)
			return statusErr == nil && status.OperationState == "succeeded" && status.CoverageState == "transcribed"
		}, 30*time.Second, 20*time.Millisecond)
	}
	search := func(versionID string) DocumentSearchReport {
		results, searchErr := vault.SearchDocuments(t.Context(), DocumentSearchRequest{
			Query: first.phrase, Mode: DocumentSearchLexical, Profile: "supplied-captions", Limit: 10,
			Fence: DocumentSourceFence{VaultUID: vault.ID(), ContentVersionIDs: []string{versionID}},
		})
		require.NoError(t, searchErr)
		return results
	}
	firstResults := search(first.original.ContentVersionID)
	secondResults := search(second.original.ContentVersionID)
	require.Len(t, firstResults.Results, 1)
	require.Len(t, secondResults.Results, 1)
	assert.Equal(t, first.original.ContentVersionID, firstResults.Results[0].ContentVersionID)
	assert.Equal(t, second.original.ContentVersionID, secondResults.Results[0].ContentVersionID)
	assert.Equal(t, &MediaTimeSpan{StartMS: 1_000, EndMS: 2_000}, searchSpan(firstResults.Results[0]))
	assert.Equal(t, &MediaTimeSpan{StartMS: 3_000, EndMS: 4_000}, searchSpan(secondResults.Results[0]))
	foreign, err := vault.SearchDocuments(t.Context(), DocumentSearchRequest{
		Query: first.phrase, Mode: DocumentSearchLexical, Profile: "supplied-captions", Limit: 10,
		Fence: DocumentSourceFence{VaultUID: vault.ID(), ContentVersionIDs: []string{"00000000-0000-4000-8000-000000000001"}},
	})
	require.NoError(t, err)
	assert.Empty(t, foreign.Results)
	_, err = vault.DeclareMediaOccurrence(t.Context(), uuid.New().String(), first.remote.SourceID,
		MediaOccurrenceInput{Ref: "fence-a-survivor", Revision: "1", Filename: "loom.mp4"})
	require.NoError(t, err)
	_, err = vault.RevokeMediaOccurrence(t.Context(), uuid.New().String(), first.occurrenceID, "1")
	require.NoError(t, err)
	status, err := vault.MediaStatus(t.Context(), first.remote.SourceID)
	require.NoError(t, err)
	assert.Equal(t, "stale", status.CoverageState)
	_, err = vault.Rendition(t.Context(), RenditionRequest{Selector: first.selector})
	require.ErrorIs(t, err, ErrNotFound)
	assert.Empty(t, search(first.original.ContentVersionID).Results)
	assert.Len(t, search(second.original.ContentVersionID).Results, 1)
}

func runLoomManualExportEmbedded(t *testing.T) {
	t.Helper()
	root := t.TempDir()
	vault, err := New(t.Context(), Config{Root: root})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, vault.Close()) })

	remote, err := vault.SubmitRemoteRecording(t.Context(), RemoteRecordingRequest{
		OperationID:  "00000000-0000-4000-8000-000000000501",
		ReferenceURL: "https://private.invalid/loom-manual",
		CanonicalURL: "https://www.loom.com/share/synthloom-embedded",
		Acquire:      true, Occurrence: MediaOccurrenceInput{Ref: "loom-embedded", Revision: "1", Filename: "loom.mp4"},
	})
	require.NoError(t, err)
	require.Equal(t, "unsupported", remote.Outcome)

	video := mediatest.H264AACMP4()
	videoID := contentIdentity(video)
	original, err := vault.ImportRecordingArtifact(t.Context(), MediaArtifactRequest{
		OperationID: "00000000-0000-4000-8000-000000000502", SourceID: remote.SourceID,
		OccurrenceID: remote.OccurrenceID, Kind: "media", Origin: "supplied", Provider: "loom",
		Filename: "loom.mp4", MediaType: "video/mp4", SHA256: videoID.SHA256, ByteLength: videoID.Size,
		Content: bytes.NewReader(video),
	})
	require.NoError(t, err)
	require.Equal(t, "content_available", original.Outcome)

	srt := []byte("1\r\n00:00:01,250 --> 00:00:02,750\r\nembedded first cue\r\n\r\n2\r\n00:00:02,500 --> 00:00:04,125\r\nembedded second cue\r\n")
	captionID := contentIdentity(srt)
	caption, err := vault.ImportRecordingArtifact(t.Context(), MediaArtifactRequest{
		OperationID: "00000000-0000-4000-8000-000000000503", SourceID: remote.SourceID,
		OccurrenceID: remote.OccurrenceID, Kind: "caption", Origin: "supplied", Provider: "loom",
		Filename: "loom.srt", MediaType: "application/x-subrip", SHA256: captionID.SHA256,
		ByteLength: captionID.Size, Content: bytes.NewReader(srt),
	})
	require.NoError(t, err)

	node, err := vault.Stat(t.Context(), "/media/"+remote.SourceID+"/"+videoID.SHA256+".mp4")
	require.NoError(t, err)
	selector := ProcessingSelector{NodeID: node.ID, ContentVersionID: original.ContentVersionID,
		Profile: "supplied-captions"}
	plan, err := vault.PlanProcessing(t.Context(), ProcessingPlanRequest{Selector: selector})
	require.NoError(t, err)
	_, err = vault.GrantProcessingPlanConsent(t.Context(), ProcessingConsentGrantRequest{
		PlanRequest: ProcessingPlanRequest{Selector: selector}, PlanFingerprint: plan.Fingerprint})
	require.NoError(t, err)
	vault.processingCancel()
	vault.processingWG.Wait()
	queued, err := vault.RetryMedia(t.Context(), "00000000-0000-4000-8000-000000000504", remote.SourceID,
		MediaProcessingRequest{Profile: "supplied-captions", SuppliedInputID: caption.SuppliedInputID})
	if err != nil {
		t.Fatalf("retry supplied captions: %v", err)
	}
	require.Equal(t, "queued", queued.OperationState)

	require.NoError(t, vault.Close())
	vault, err = New(t.Context(), Config{Root: root})
	require.NoError(t, err)
	vault.processingCancel()
	vault.processingWG.Wait()
	waiter, err := vault.metadata.RenditionJobWaiterByID(t.Context(), queued.JobID)
	require.NoError(t, err)
	worker, err := internalprocessing.NewRenditionWorker(internalprocessing.RenditionWorkerConfig{
		Catalog: vault.metadata, Blobs: vault.blobs, Runtime: vault.processing.RenditionRuntimes(),
		Gate: embeddedMutationGate{vault: vault}, Owner: "embedded-test-rendition-" + vault.ID(),
		LeaseDuration: time.Minute, IdleDelay: 25 * time.Millisecond,
	})
	require.NoError(t, err)
	processed, err := worker.RunJob(t.Context(), waiter.JobID)
	require.NoError(t, err)
	if processed {
		continuation := &internalprocessing.MediaContinuationWorker{Service: vault.processing, IdleDelay: 25 * time.Millisecond}
		contProcessed, contErr := continuation.RunOne(t.Context())
		require.NoError(t, contErr)
		require.True(t, contProcessed)
	}

	var status MediaReceipt
	if !assert.Eventually(t, func() bool {
		status, err = vault.MediaStatus(t.Context(), remote.SourceID)
		return err == nil && status.OperationState == "succeeded" && status.CoverageState == "transcribed"
	}, 30*time.Second, 20*time.Millisecond) {
		t.Fatalf("final status=%+v err=%v", status, err)
	}
	require.Equal(t, original.ContentVersionID, status.ContentVersionID)

	for _, mode := range []DocumentSearchMode{DocumentSearchLexical, DocumentSearchAuto} {
		results, searchErr := vault.SearchDocuments(t.Context(), DocumentSearchRequest{
			Query: "embedded second cue", Mode: mode, Profile: "supplied-captions", Limit: 10,
			Fence: DocumentSourceFence{VaultUID: vault.ID(), ContentVersionIDs: []string{original.ContentVersionID}},
		})
		require.NoError(t, searchErr)
		require.NotEmpty(t, results.Results)
		require.Equal(t, original.ContentVersionID, results.Results[0].ContentVersionID)
		assert.Equal(t, &MediaTimeSpan{StartMS: 2_500, EndMS: 4_125}, searchSpan(results.Results[0]))
	}
	for _, mode := range []DocumentSearchMode{DocumentSearchSemantic, DocumentSearchHybrid} {
		_, searchErr := vault.SearchDocuments(t.Context(), DocumentSearchRequest{
			Query: "embedded second cue", Mode: mode, Profile: "supplied-captions", Limit: 10,
			Fence: DocumentSourceFence{VaultUID: vault.ID(), ContentVersionIDs: []string{original.ContentVersionID}},
		})
		require.ErrorContains(t, searchErr, "semantic retrieval is not configured")
	}

	view, err := vault.metadata.ActiveRendition(t.Context(), original.ContentVersionID, plan.ProfileFingerprint)
	require.NoError(t, err)
	var artifactHash string
	for _, artifact := range view.Build.Artifacts {
		if artifact.Role == string(document.EvidenceArtifactTranscript) {
			artifactHash = artifact.BlobHash
			break
		}
	}
	require.NotEmpty(t, artifactHash)
	reader, _, err := vault.blobs.OpenStreamContext(t.Context(), artifactHash)
	require.NoError(t, err)
	artifactBytes, err := io.ReadAll(reader)
	require.NoError(t, err)
	require.NoError(t, reader.Close())
	artifact, err := mediatranscript.Unmarshal(artifactBytes)
	require.NoError(t, err)
	assert.Equal(t, "supplied", artifact.Origin)
	assert.Equal(t, "loom", artifact.Provider)
	assert.Empty(t, artifact.Language)
	assert.Equal(t, []mediatranscript.Segment{
		{Order: 0, StartMS: 1_250, EndMS: 2_750, Text: "embedded first cue"},
		{Order: 1, StartMS: 2_500, EndMS: 4_125, Text: "embedded second cue"},
	}, artifact.Segments)
}

func searchSpan(result DocumentSearchResult) *MediaTimeSpan {
	for _, evidence := range result.Evidence {
		if evidence.TimeSpan != nil {
			return evidence.TimeSpan
		}
	}
	return nil
}
