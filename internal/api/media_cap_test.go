package api_test

import (
	"bytes"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/document/media/mediatest"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/apiclient"
	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/docbank/internal/processing"
)

// A received Cap Cloud link has no documented download route, so acquisition
// is reported as unsupported and the exact file reaches processing manually.
func TestCapCloudRecordingManualHTTP(t *testing.T) {
	t.Parallel()
	ts, catalog := newTestServer(t, configureRemoteManualTestService(t))
	c := daemonconn.New(ts.URL, testAPIKey)
	reference := "https://cap.so/s/synthcap-http?t=private-synthetic"
	remote, err := c.SubmitRemoteRecording(t.Context(), api.MediaReferenceBody{
		OperationID: "00000000-0000-4000-8000-000000000461", ReferenceURL: reference,
		CanonicalURL: "https://cap.so/s/synthcap-http", Acquire: true,
		Occurrence: api.MediaOccurrenceBody{Ref: "cap-http", Revision: "1", Filename: "cap.wav"},
	})
	require.NoError(t, err)
	require.Equal(t, "unsupported", remote.Outcome)
	require.Empty(t, remote.ContentVersionID)

	badResponse, badBody := rawJSONRequest(t, ts.URL, http.MethodPost, "/api/v1/media/sources",
		map[string]string{"X-Api-Key": testAPIKey},
		`{"operation_id":"00000000-0000-4000-8000-000000000462","reference_url":"`+reference+`",`)
	require.Equal(t, http.StatusUnprocessableEntity, badResponse.StatusCode, badBody)

	raw := mediatest.WAV()
	identity := processingTestHash(string(raw))
	original, err := c.ImportMediaArtifact(t.Context(), remote.SourceID, api.MediaArtifactMetadata{
		OperationID: "00000000-0000-4000-8000-000000000463", OccurrenceID: remote.OccurrenceID,
		Kind: "media", Origin: "supplied", Filename: "cap.wav", MediaType: "audio/wav",
		SHA256: identity, ByteLength: int64(len(raw)),
	}, bytes.NewReader(raw))
	require.NoError(t, err)
	require.Equal(t, "content_available", original.Outcome)
	require.Equal(t, remote.SourceID, original.SourceID)

	phrase := "cap cloud synthetic transcript phrase"
	transcript := []byte(phrase + "\n")
	input, err := c.ImportMediaArtifact(t.Context(), remote.SourceID, api.MediaArtifactMetadata{
		OperationID: "00000000-0000-4000-8000-000000000464", OccurrenceID: remote.OccurrenceID,
		Kind: "transcript", Origin: "supplied", Filename: "cap.txt", MediaType: "text/plain",
		SHA256: processingTestHash(string(transcript)), ByteLength: int64(len(transcript)),
	}, bytes.NewReader(transcript))
	require.NoError(t, err)

	version, err := catalog.ContentVersionByID(t.Context(), original.ContentVersionID)
	require.NoError(t, err)
	selector := api.ProcessingSelector{NodeID: version.NodeID, ContentVersionID: version.ID,
		Profile: processing.SuppliedMediaProfileName}
	plan, err := c.API().PlanDocumentProcessing(t.Context(), &apiclient.PlanDocumentProcessingRequestOptions{Body: &api.ProcessingPlanRequest{Selector: selector}})
	require.NoError(t, err)
	_, err = c.API().GrantDocumentProcessingConsent(t.Context(), &apiclient.GrantDocumentProcessingConsentRequestOptions{Body: &api.ProcessingConsentGrantRequest{
		Selector: selector, PlanFingerprint: plan.Fingerprint}})
	require.NoError(t, err)
	queued, err := c.RetryMedia(t.Context(), remote.SourceID, api.MediaRetryBody{
		OperationID: "00000000-0000-4000-8000-000000000465",
		Processing:  &api.MediaProcessingBody{Profile: processing.SuppliedMediaProfileName, SuppliedInputID: input.SuppliedInputID},
	})
	require.NoError(t, err)
	require.Equal(t, "queued", queued.OperationState)
	var processed api.MediaReceipt
	require.Eventually(t, func() bool {
		processed, err = c.MediaStatus(t.Context(), remote.SourceID)
		return err == nil && processed.OperationID == queued.OperationID &&
			processed.OperationState == "succeeded" && processed.CoverageState == "transcribed"
	}, 30*time.Second, 20*time.Millisecond)
	require.Equal(t, original.ContentVersionID, processed.ContentVersionID)
	search, err := c.SearchDocuments(t.Context(), api.DocumentSearchRequest{
		Query: phrase, Mode: "lexical", Profile: processing.SuppliedMediaProfileName, Limit: 10,
		Fence: api.DocumentSourceFence{VaultUID: catalog.VaultID(), ContentVersionIDs: []string{original.ContentVersionID}},
	})
	require.NoError(t, err)
	require.NotEmpty(t, search.Results)
	require.Equal(t, original.ContentVersionID, search.Results[0].ContentVersionID)

	// The newest visible occurrence stays pending until its own exact file is
	// imported; the processed source version above is not replaced.
	embed, err := c.SubmitRemoteRecording(t.Context(), api.MediaReferenceBody{
		OperationID: "00000000-0000-4000-8000-000000000466", ReferenceURL: "https://www.cap.so/embed/synthcap-http",
		CanonicalURL: "https://www.cap.so/embed/synthcap-http",
		Occurrence:   api.MediaOccurrenceBody{Ref: "cap-http-2", Revision: "1", Filename: "cap.wav"},
	})
	require.NoError(t, err)
	require.Equal(t, remote.SourceID, embed.SourceID)
	require.NotEqual(t, remote.OccurrenceID, embed.OccurrenceID)
	pending, err := c.MediaStatus(t.Context(), remote.SourceID)
	require.NoError(t, err)
	require.Equal(t, "unsupported", pending.Outcome)
	require.Empty(t, pending.ContentVersionID)
	_, err = c.ImportMediaArtifact(t.Context(), remote.SourceID, api.MediaArtifactMetadata{
		OperationID: "00000000-0000-4000-8000-000000000467", OccurrenceID: embed.OccurrenceID,
		Kind: "media", Origin: "supplied", Filename: "cap.wav", MediaType: "audio/wav",
		SHA256: identity, ByteLength: int64(len(raw)),
	}, bytes.NewReader(raw))
	require.NoError(t, err)
	available, err := c.MediaStatus(t.Context(), remote.SourceID)
	require.NoError(t, err)
	require.Equal(t, "content_available", available.Outcome)
	require.NotEmpty(t, available.ContentVersionID)

	var metadata bytes.Buffer
	require.NoError(t, catalog.ExportMetadata(t.Context(), &metadata))
	captured := badBody + metadata.String()
	require.NotContains(t, captured, "private-synthetic")
	require.NotContains(t, captured, "synthcap-http")
}
