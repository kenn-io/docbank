package api_test

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/document/media/mediatest"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/apiclient"
	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/docbank/internal/processing"
)

func configureLoomManualTestService(t *testing.T) func(*api.Deps) {
	t.Helper()
	return func(deps *api.Deps) {
		gate := api.NewOperationGate()
		deps.Gate = gate
		transcriptName, transcriptProfile, err := processing.NewSuppliedMediaProfile(deps.Store, deps.Blobs, "daemon:operator")
		require.NoError(t, err)
		captionName, captionProfile, err := processing.NewSuppliedCaptionProfile(deps.Store, deps.Blobs, "daemon:operator")
		require.NoError(t, err)
		service, err := processing.NewService(processing.ServiceConfig{
			Catalog: deps.Store, Blobs: deps.Blobs, Gate: gate,
			SpoolDirectory: filepath.Join(deps.VaultRoot, "blobs", "tmp"), Principal: "daemon:operator",
			Profiles: map[string]processing.ProfileConfig{transcriptName: transcriptProfile, captionName: captionProfile},
		})
		require.NoError(t, err)
		deps.Processing = service
		worker, err := processing.NewRenditionWorker(processing.RenditionWorkerConfig{
			Catalog: deps.Store, Blobs: deps.Blobs, Runtime: service.RenditionRuntimes(), Gate: gate,
			Owner: "loom-manual-test-worker", LeaseDuration: time.Minute, IdleDelay: time.Millisecond,
		})
		require.NoError(t, err)
		workerContext, cancelWorker := context.WithCancel(context.Background())
		var workers sync.WaitGroup
		workers.Go(func() { _ = worker.Run(workerContext) })
		continuation := &processing.MediaContinuationWorker{Service: service, IdleDelay: time.Millisecond}
		workers.Go(func() { _ = continuation.Run(workerContext) })
		t.Cleanup(func() { cancelWorker(); workers.Wait() })
	}
}

func TestLoomManualExportHTTP(t *testing.T) {
	ts, catalog := newTestServer(t, configureLoomManualTestService(t))
	client := daemonconn.New(ts.URL, testAPIKey)
	remote, err := client.SubmitRemoteRecording(t.Context(), api.MediaReferenceBody{
		OperationID: "00000000-0000-4000-8000-000000000551", ReferenceURL: "https://private.invalid/loom-http",
		CanonicalURL: "https://www.loom.com/share/synthloom-http", Acquire: true,
		Occurrence: api.MediaOccurrenceBody{Ref: "loom-http", Revision: "1", Filename: "loom.mp4"},
	})
	require.NoError(t, err)
	require.Equal(t, "unsupported", remote.Outcome)

	video := mediatest.H264AACMP4()
	videoHash := processingTestHash(string(video))
	original, err := client.ImportMediaArtifact(t.Context(), remote.SourceID, api.MediaArtifactMetadata{
		OperationID: "00000000-0000-4000-8000-000000000552", OccurrenceID: remote.OccurrenceID,
		Kind: "media", Origin: "supplied", Provider: "loom", Filename: "loom.mp4", MediaType: "video/mp4",
		SHA256: videoHash, ByteLength: int64(len(video)),
	}, bytes.NewReader(video))
	require.NoError(t, err)

	srt := []byte("1\r\n00:00:01,250 --> 00:00:02,750\r\nhttp first cue\r\n\r\n2\r\n00:00:02,500 --> 00:00:04,125\r\nhttp second cue\r\n")
	captionHash := processingTestHash(string(srt))
	caption, err := client.ImportMediaArtifact(t.Context(), remote.SourceID, api.MediaArtifactMetadata{
		OperationID: "00000000-0000-4000-8000-000000000553", OccurrenceID: remote.OccurrenceID,
		Kind: "caption", Origin: "supplied", Provider: "loom", Filename: "loom.srt",
		MediaType: "application/x-subrip", SHA256: captionHash, ByteLength: int64(len(srt)),
	}, bytes.NewReader(srt))
	require.NoError(t, err)

	version, err := catalog.ContentVersionByID(t.Context(), original.ContentVersionID)
	require.NoError(t, err)
	selector := api.ProcessingSelector{NodeID: version.NodeID, ContentVersionID: version.ID,
		Profile: processing.SuppliedCaptionProfileName}
	before, err := client.SearchDocuments(t.Context(), api.DocumentSearchRequest{
		Query: "http second cue", Mode: "lexical", Profile: processing.SuppliedCaptionProfileName, Limit: 10,
		Fence: api.DocumentSourceFence{VaultUID: catalog.VaultID(), ContentVersionIDs: []string{original.ContentVersionID}},
	})
	require.NoError(t, err)
	assert.Empty(t, before.Results)
	plan, err := client.API().PlanDocumentProcessing(t.Context(), &apiclient.PlanDocumentProcessingRequestOptions{
		Body: &api.ProcessingPlanRequest{Selector: selector}})
	require.NoError(t, err)
	_, err = client.API().GrantDocumentProcessingConsent(t.Context(), &apiclient.GrantDocumentProcessingConsentRequestOptions{
		Body: &api.ProcessingConsentGrantRequest{Selector: selector, PlanFingerprint: plan.Fingerprint}})
	require.NoError(t, err)
	queued, err := client.RetryMedia(t.Context(), remote.SourceID, api.MediaRetryBody{
		OperationID: "00000000-0000-4000-8000-000000000554",
		Processing:  &api.MediaProcessingBody{Profile: processing.SuppliedCaptionProfileName, SuppliedInputID: caption.SuppliedInputID},
	})
	require.NoError(t, err)
	_, err = client.RetryMedia(t.Context(), remote.SourceID, api.MediaRetryBody{
		OperationID: "00000000-0000-4000-8000-000000000555",
		Processing:  &api.MediaProcessingBody{Profile: processing.SuppliedMediaProfileName, SuppliedInputID: caption.SuppliedInputID},
	})
	require.Error(t, err)
	require.Equal(t, "queued", queued.OperationState)

	var status api.MediaReceipt
	require.Eventually(t, func() bool {
		status, err = client.MediaStatus(t.Context(), remote.SourceID)
		return err == nil && status.OperationState == "succeeded" && status.CoverageState == "transcribed"
	}, 30*time.Second, 20*time.Millisecond)
	require.Equal(t, original.ContentVersionID, status.ContentVersionID)
	results, err := client.SearchDocuments(t.Context(), api.DocumentSearchRequest{
		Query: "http second cue", Mode: "lexical", Profile: processing.SuppliedCaptionProfileName, Limit: 10,
		Fence: api.DocumentSourceFence{VaultUID: catalog.VaultID(), ContentVersionIDs: []string{original.ContentVersionID}},
	})
	require.NoError(t, err)
	require.NotEmpty(t, results.Results)
	require.Equal(t, original.ContentVersionID, results.Results[0].ContentVersionID)
	assert.Equal(t, int64(2_500), results.Results[0].Evidence[0].TimeSpan.StartMS)
	assert.Equal(t, int64(4_125), results.Results[0].Evidence[0].TimeSpan.EndMS)
	stream, err := client.RenditionForSelector(t.Context(), selector, 1<<20)
	require.NoError(t, err)
	var rendition bytes.Buffer
	_, err = stream.CopyVerified(&rendition)
	require.NoError(t, err)
	assert.Contains(t, rendition.String(), "http second cue")

	var metadata bytes.Buffer
	require.NoError(t, catalog.ExportMetadata(t.Context(), &metadata))
	assert.Contains(t, metadata.String(), `"provider":"loom"`)
	assert.NotContains(t, metadata.String(), "synthloom-http")
}

func TestLoomManualExportHTTPRejectsOversizeVideoBeforeStaging(t *testing.T) {
	blockedSpool := filepath.Join(t.TempDir(), "spool-file")
	require.NoError(t, os.WriteFile(blockedSpool, []byte("not a directory"), 0o600))
	ts, _ := newTestServer(t, func(deps *api.Deps) {
		gate := api.NewOperationGate()
		deps.Gate = gate
		service, err := processing.NewService(processing.ServiceConfig{
			Catalog: deps.Store, Blobs: deps.Blobs, Gate: gate,
			SpoolDirectory: blockedSpool, Principal: "daemon:operator",
		})
		require.NoError(t, err)
		deps.Processing = service
	})
	client := daemonconn.New(ts.URL, testAPIKey)
	remote, err := client.SubmitRemoteRecording(t.Context(), api.MediaReferenceBody{
		OperationID: "00000000-0000-4000-8000-000000000556", ReferenceURL: "https://private.invalid/loom-oversize",
		CanonicalURL: "https://www.loom.com/share/synthloom-oversize", Acquire: true,
		Occurrence: api.MediaOccurrenceBody{Ref: "loom-oversize", Revision: "1", Filename: "loom.mp4"},
	})
	require.NoError(t, err)
	require.Equal(t, "unsupported", remote.Outcome)

	_, err = client.ImportMediaArtifact(t.Context(), remote.SourceID, api.MediaArtifactMetadata{
		OperationID: "00000000-0000-4000-8000-000000000557", OccurrenceID: remote.OccurrenceID,
		Kind: "media", Origin: "supplied", Provider: "loom", Filename: "loom.mp4", MediaType: "video/mp4",
		SHA256: strings.Repeat("0", 64), ByteLength: 20<<20 + 1,
	}, bytes.NewReader(nil))
	require.ErrorContains(t, err, "byte_limit")
}

func TestLoomManualExportHTTPReplaysArtifactAfterRevocation(t *testing.T) {
	ts, _ := newTestServer(t, configureLoomManualTestService(t))
	client := daemonconn.New(ts.URL, testAPIKey)
	remote, err := client.SubmitRemoteRecording(t.Context(), api.MediaReferenceBody{
		OperationID: "00000000-0000-4000-8000-000000000558", ReferenceURL: "https://private.invalid/loom-replay",
		CanonicalURL: "https://www.loom.com/share/synthloom-replay", Acquire: true,
		Occurrence: api.MediaOccurrenceBody{Ref: "loom-replay", Revision: "1", Filename: "loom.mp4"},
	})
	require.NoError(t, err)
	video := mediatest.H264AACMP4()
	metadata := api.MediaArtifactMetadata{OperationID: "00000000-0000-4000-8000-000000000559",
		OccurrenceID: remote.OccurrenceID, Kind: "media", Origin: "supplied", Provider: "loom",
		Filename: "loom.mp4", MediaType: "video/mp4", SHA256: processingTestHash(string(video)), ByteLength: int64(len(video))}
	first, err := client.ImportMediaArtifact(t.Context(), remote.SourceID, metadata, bytes.NewReader(video))
	require.NoError(t, err)
	_, err = client.RevokeMediaOccurrence(t.Context(), remote.OccurrenceID, api.MediaOccurrenceRevokeBody{
		OperationID: "00000000-0000-4000-8000-000000000560", Revision: "1"})
	require.NoError(t, err)
	replay, err := client.ImportMediaArtifact(t.Context(), remote.SourceID, metadata, bytes.NewReader(video))
	require.NoError(t, err)
	require.Equal(t, first.ContentVersionID, replay.ContentVersionID)
	require.Equal(t, first.OperationID, replay.OperationID)
}
