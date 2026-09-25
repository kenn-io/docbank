package api_test

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"io"
	"mime/multipart"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/document/media/mediatest"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/apiclient"
	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/docbank/internal/processing"
)

func configureRemoteManualTestService(t *testing.T) func(*api.Deps) {
	t.Helper()
	return func(deps *api.Deps) {
		gate := api.NewOperationGate()
		deps.Gate = gate
		name, profile, err := processing.NewSuppliedMediaProfile(deps.Store, deps.Blobs, "daemon:operator")
		require.NoError(t, err)
		service, err := processing.NewService(processing.ServiceConfig{
			Catalog: deps.Store, Blobs: deps.Blobs, Gate: gate,
			SpoolDirectory: filepath.Join(deps.VaultRoot, "blobs", "tmp"), Principal: "daemon:operator",
			Profiles: map[string]processing.ProfileConfig{name: profile},
		})
		require.NoError(t, err)
		deps.Processing = service
		worker, err := processing.NewRenditionWorker(processing.RenditionWorkerConfig{
			Catalog: deps.Store, Blobs: deps.Blobs, Runtime: service.RenditionRuntimes(), Gate: gate,
			Owner: "remote-manual-test-worker", LeaseDuration: time.Minute, IdleDelay: time.Millisecond,
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

func TestRemoteRecordingManualHTTP(t *testing.T) {
	t.Parallel()
	ts, catalog := newTestServer(t, configureRemoteManualTestService(t))
	c := daemonconn.New(ts.URL, testAPIKey)
	protectedPrefix := "https://private.invalid/share/http?token="
	protectedReference := protectedPrefix + strings.Repeat("p", 8192-len(protectedPrefix))
	require.Len(t, protectedReference, 8192)
	canonicalReference := "HTTPS://Recordings.INVALID:443/share/http?clip=2#fragment"
	remote, err := c.SubmitRemoteRecording(t.Context(), api.MediaReferenceBody{
		OperationID: "00000000-0000-4000-8000-000000000451", ReferenceURL: protectedReference,
		CanonicalURL: canonicalReference, CredentialBinding: "private-binding-value",
		Occurrence: api.MediaOccurrenceBody{Ref: "http-call", Revision: "1", Filename: "http.wav"},
	})
	require.NoError(t, err)
	require.Equal(t, "unsupported", remote.Outcome)
	require.Empty(t, remote.ContentVersionID)

	status, err := c.MediaStatus(t.Context(), remote.SourceID)
	require.NoError(t, err)
	require.Equal(t, "unsupported", status.Outcome)
	require.Empty(t, status.ContentVersionID)

	badResponse, badBody := rawJSONRequest(t, ts.URL, http.MethodPost, "/api/v1/media/sources",
		map[string]string{"X-Api-Key": testAPIKey}, `{"operation_id":"00000000-0000-4000-8000-000000000452","reference_url":"`+protectedReference+`",`)
	require.Equal(t, http.StatusUnprocessableEntity, badResponse.StatusCode, badBody)
	var badProblem api.Error
	require.NoError(t, json.Unmarshal([]byte(badBody), &badProblem))
	require.Equal(t, "invalid media reference body", badProblem.Detail)
	require.NotContains(t, badBody, protectedReference)

	missingKey, missingBody := rawJSONRequest(t, ts.URL, http.MethodPost, "/api/v1/media/sources",
		map[string]string{"X-Api-Key": ""}, `{"operation_id":"00000000-0000-4000-8000-000000000453","reference_url":"`+protectedReference+`","canonical_url":"`+canonicalReference+`"}`)
	require.Equal(t, http.StatusUnauthorized, missingKey.StatusCode, missingBody)
	require.NotContains(t, missingBody, protectedReference)

	webResponse, webBody := rawJSONRequest(t, ts.URL, http.MethodPost, "/api/daemon/web-session",
		map[string]string{"X-Api-Key": testAPIKey}, "")
	require.Equal(t, http.StatusCreated, webResponse.StatusCode, webBody)
	var session struct {
		Token string `json:"token"`
	}
	require.NoError(t, json.Unmarshal([]byte(webBody), &session))
	require.NotEmpty(t, session.Token)
	browserResponse, browserBody := rawJSONRequest(t, ts.URL, http.MethodPost, "/api/v1/media/sources",
		map[string]string{"X-Api-Key": "", api.WebSessionHeader: session.Token}, `{"operation_id":"00000000-0000-4000-8000-000000000454","reference_url":"`+protectedReference+`","canonical_url":"`+canonicalReference+`"}`)
	require.Equal(t, http.StatusForbidden, browserResponse.StatusCode, browserBody)
	require.NotContains(t, browserBody, protectedReference)

	raw := mediatest.WAV()
	identity := processingTestHash(string(raw))
	original, err := c.ImportMediaArtifact(t.Context(), remote.SourceID, api.MediaArtifactMetadata{
		OperationID: "00000000-0000-4000-8000-000000000455", OccurrenceID: remote.OccurrenceID,
		Kind: "media", Origin: "supplied", Filename: "http.wav", MediaType: "audio/wav",
		SHA256: identity, ByteLength: int64(len(raw)),
	}, bytes.NewReader(raw))
	require.NoError(t, err)
	require.Equal(t, "content_available", original.Outcome)
	require.Equal(t, remote.SourceID, original.SourceID)

	phrase := "http remote recording transcript phrase"
	transcript := []byte(phrase + "\n")
	transcriptIdentity := processingTestHash(string(transcript))
	input, err := c.ImportMediaArtifact(t.Context(), remote.SourceID, api.MediaArtifactMetadata{
		OperationID: "00000000-0000-4000-8000-000000000456", OccurrenceID: remote.OccurrenceID,
		Kind: "transcript", Origin: "supplied", Filename: "http.txt", MediaType: "text/plain",
		SHA256: transcriptIdentity, ByteLength: int64(len(transcript)),
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
		OperationID: "00000000-0000-4000-8000-000000000457",
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
	require.Equal(t, original.SourceVersionID, processed.SourceVersionID)
	require.Equal(t, original.ContentVersionID, processed.ContentVersionID)
	search, err := c.SearchDocuments(t.Context(), api.DocumentSearchRequest{
		Query: phrase, Mode: "lexical", Profile: processing.SuppliedMediaProfileName, Limit: 10,
		Fence: api.DocumentSourceFence{VaultUID: catalog.VaultID(), ContentVersionIDs: []string{original.ContentVersionID}},
	})
	require.NoError(t, err)
	require.NotEmpty(t, search.Results)
	require.Equal(t, original.ContentVersionID, search.Results[0].ContentVersionID)

	var metadata bytes.Buffer
	require.NoError(t, catalog.ExportMetadata(t.Context(), &metadata))
	captured := badBody + missingBody + browserBody + metadata.String()
	require.NotContains(t, captured, protectedReference)
	require.NotContains(t, captured, "private-binding-value")
}

func TestRemoteRecordingManualHTTPRejectsExtraArtifactPart(t *testing.T) {
	t.Parallel()
	ts, _ := newTestServer(t, configureRemoteManualTestService(t))
	c := daemonconn.New(ts.URL, testAPIKey)
	remote, err := c.SubmitRemoteRecording(t.Context(), api.MediaReferenceBody{
		OperationID: "00000000-0000-4000-8000-000000000458", ReferenceURL: "https://private.invalid/share/extra?token=synthetic",
		CanonicalURL: "https://recordings.invalid/share/extra", Occurrence: api.MediaOccurrenceBody{Ref: "extra", Revision: "1"},
	})
	require.NoError(t, err)
	wav := mediatest.WAV()
	metadata := api.MediaArtifactMetadata{OperationID: "00000000-0000-4000-8000-000000000459",
		OccurrenceID: remote.OccurrenceID, Kind: "media", Origin: "supplied", Filename: "extra.wav",
		MediaType: "audio/wav", SHA256: processingTestHash(string(wav)), ByteLength: int64(len(wav))}
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	metadataPart, err := writer.CreateFormField("metadata")
	require.NoError(t, err)
	require.NoError(t, json.MarshalWrite(metadataPart, metadata))
	filePart, err := writer.CreateFormFile("file", metadata.Filename)
	require.NoError(t, err)
	_, err = filePart.Write(wav)
	require.NoError(t, err)
	extra, err := writer.CreateFormField("extra")
	require.NoError(t, err)
	_, err = extra.Write([]byte("unexpected"))
	require.NoError(t, err)
	require.NoError(t, writer.Close())
	request, err := http.NewRequestWithContext(t.Context(), http.MethodPost,
		ts.URL+"/api/v1/media/sources/"+remote.SourceID+"/artifacts", bytes.NewReader(body.Bytes()))
	require.NoError(t, err)
	request.Header.Set("X-Api-Key", testAPIKey)
	request.Header.Set("Content-Type", writer.FormDataContentType())
	response, err := ts.Client().Do(request)
	require.NoError(t, err)
	responseBody, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	require.NoError(t, response.Body.Close())
	require.Equal(t, http.StatusUnprocessableEntity, response.StatusCode, string(responseBody))
	status, err := c.MediaStatus(t.Context(), remote.SourceID)
	require.NoError(t, err)
	require.Empty(t, status.ContentVersionID)
}

func TestMediaAcquisitionPlanRejectsOversizedCanonicalURLWithoutEcho(t *testing.T) {
	t.Parallel()
	ts, _ := newTestServer(t, configureMediaTestService(t))
	secret := strings.Repeat("c", 8193)
	body := `{"operation_id":"00000000-0000-0000-0000-000000000460","reference_url":"https://recordings.invalid/call","canonical_url":"` + secret + `"}`
	response, responseBody := rawJSONRequest(t, ts.URL, http.MethodPost, "/api/v1/media/acquisition-plan",
		map[string]string{"X-Api-Key": testAPIKey}, body)
	require.Equal(t, http.StatusUnprocessableEntity, response.StatusCode, responseBody)
	require.NotContains(t, responseBody, secret)
	require.Contains(t, responseBody, "invalid media reference body")
}

func TestMediaAcquisitionPlanRejectsOversizedReferenceURLWithoutEcho(t *testing.T) {
	t.Parallel()
	ts, _ := newTestServer(t, configureMediaTestService(t))
	secret := strings.Repeat("r", 8193)
	body := `{"operation_id":"00000000-0000-0000-0000-000000000461","reference_url":"` + secret + `","canonical_url":"https://recordings.invalid/call"}`
	response, responseBody := rawJSONRequest(t, ts.URL, http.MethodPost, "/api/v1/media/acquisition-plan",
		map[string]string{"X-Api-Key": testAPIKey}, body)
	require.Equal(t, http.StatusUnprocessableEntity, response.StatusCode, responseBody)
	require.NotContains(t, responseBody, secret)
	require.Contains(t, responseBody, "invalid media reference body")
}
