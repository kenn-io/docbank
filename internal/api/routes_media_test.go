package api_test

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/document/media/mediatest"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/apiclient"
	"go.kenn.io/docbank/internal/config"
	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/docbank/internal/processing"
	"go.kenn.io/docbank/internal/store"
)

func TestMediaRoutesAreAuthenticatedAndCoverTheTwelveContracts(t *testing.T) {
	t.Parallel()
	ts, catalog := newTestServer(t, configureMediaTestService(t))
	unauthorized, body := get(t, ts, "/api/v1/media/sources?limit=10",
		map[string]string{"X-Api-Key": ""})
	require.Equal(t, 401, unauthorized.StatusCode, body)
	c := daemonconn.New(ts.URL, testAPIKey)
	spoofed, spoofedBody := do(t, ts, http.MethodPost, "/api/v1/media/sources", nil,
		map[string]any{"operation_id": "00000000-0000-4000-8000-000000000300",
			"reference_url": "https://recordings.invalid/private", "caller_principal": "other"})
	require.Equal(t, http.StatusUnprocessableEntity, spoofed.StatusCode, spoofedBody)

	empty, err := c.MediaSources(t.Context(), "", 10)
	require.NoError(t, err)
	require.Empty(t, empty.Items)
	require.NotNil(t, empty.Items)

	wav := mediatest.WAV()
	digest := processingTestHash(string(wav))
	receipt, err := c.SubmitSuppliedMedia(t.Context(), api.MediaSuppliedMetadata{
		OperationID: "00000000-0000-4000-8000-000000000301", Filename: "call.wav",
		MediaType: "audio/wav", SHA256: digest, ByteLength: int64(len(wav)),
		Occurrence: api.MediaOccurrenceBody{Ref: "message-1", Revision: "1", Filename: "call.wav"},
	}, bytes.NewReader(wav))
	require.NoError(t, err)
	require.Equal(t, "succeeded", receipt.OperationState)

	status, err := c.MediaStatus(t.Context(), receipt.SourceID)
	require.NoError(t, err)
	require.Equal(t, receipt.ContentVersionID, status.ContentVersionID)
	require.Equal(t, receipt.OperationID, status.OperationID)
	page, err := c.MediaSources(t.Context(), "", 10)
	require.NoError(t, err)
	require.Len(t, page.Items, 1)
	occurrences, err := c.MediaOccurrences(t.Context(), daemonconn.MediaOccurrenceOptions{Limit: 10})
	require.NoError(t, err)
	require.Len(t, occurrences.Items, 1)

	declared, err := c.DeclareMediaOccurrence(t.Context(), api.MediaOccurrenceMutationBody{
		OperationID: "00000000-0000-4000-8000-000000000302", SourceID: receipt.SourceID,
		Occurrence: api.MediaOccurrenceBody{Ref: "message-2", Revision: "1", Filename: "copy.wav"},
	})
	require.NoError(t, err)
	require.NotEmpty(t, declared.OccurrenceID)
	revoked, err := c.RevokeMediaOccurrence(t.Context(), declared.OccurrenceID,
		api.MediaOccurrenceRevokeBody{OperationID: "00000000-0000-4000-8000-000000000303", Revision: "1"})
	require.NoError(t, err)
	require.Equal(t, declared.OccurrenceID, revoked.OccurrenceID)

	transcript := []byte("synthetic exact phrase\n")
	artifact, err := c.ImportMediaArtifact(t.Context(), receipt.SourceID, api.MediaArtifactMetadata{
		OperationID: "00000000-0000-4000-8000-000000000304", OccurrenceID: receipt.OccurrenceID,
		Kind: "transcript", Filename: "call.txt", MediaType: "text/plain",
		SHA256: processingTestHash(string(transcript)), ByteLength: int64(len(transcript)),
	}, bytes.NewReader(transcript))
	require.NoError(t, err)
	require.NotEmpty(t, artifact.ContentVersionID)
	require.NotEmpty(t, artifact.SuppliedInputID)
	var metadataExport bytes.Buffer
	require.NoError(t, catalog.ExportMetadata(t.Context(), &metadataExport))
	require.Contains(t, metadataExport.String(), `"origin":"supplied"`)
	_, err = c.ImportMediaArtifact(t.Context(), receipt.SourceID, api.MediaArtifactMetadata{
		OperationID: "00000000-0000-4000-8000-000000000399", OccurrenceID: receipt.OccurrenceID,
		Kind: "transcript", Provider: strings.Repeat("p", 129), Filename: "invalid.txt", MediaType: "text/plain",
		SHA256: processingTestHash(string(transcript)), ByteLength: int64(len(transcript)),
	}, bytes.NewReader(transcript))
	require.Error(t, err)

	missingProcessing, missingBody := do(t, ts, http.MethodPost,
		"/api/v1/media/sources/"+receipt.SourceID+"/retry", nil,
		map[string]any{"operation_id": "00000000-0000-4000-8000-000000000309"})
	require.Equal(t, http.StatusUnprocessableEntity, missingProcessing.StatusCode, missingBody)

	version, err := catalog.ContentVersionByID(t.Context(), receipt.ContentVersionID)
	require.NoError(t, err)
	selector := api.ProcessingSelector{NodeID: version.NodeID,
		ContentVersionID: version.ID, Profile: processing.SuppliedMediaProfileName}
	processingPlan, err := c.API().PlanDocumentProcessing(t.Context(), &apiclient.PlanDocumentProcessingRequestOptions{Body: &api.ProcessingPlanRequest{Selector: selector}})

	require.NoError(t, err)
	_, err = c.API().GrantDocumentProcessingConsent(t.Context(), &apiclient.GrantDocumentProcessingConsentRequestOptions{Body: &api.ProcessingConsentGrantRequest{Selector: selector, PlanFingerprint: processingPlan.Fingerprint}})

	require.NoError(t, err)
	queued, err := c.RetryMedia(t.Context(), receipt.SourceID, api.MediaRetryBody{
		OperationID: "00000000-0000-4000-8000-000000000305",
		Processing: &api.MediaProcessingBody{Profile: processing.SuppliedMediaProfileName,
			SuppliedInputID: artifact.SuppliedInputID},
	})
	require.NoError(t, err)
	require.NotEmpty(t, queued.JobID)
	require.Eventually(t, func() bool {
		status, statusErr := c.MediaStatus(t.Context(), receipt.SourceID)
		return statusErr == nil && status.OperationID == queued.OperationID &&
			status.OperationState == "succeeded" && status.CoverageState == "transcribed"
	}, 30*time.Second, 20*time.Millisecond)
	jobStatus, err := c.ProcessingStatus(t.Context(), queued.JobID)
	require.NoError(t, err)
	require.Equal(t, "completed", jobStatus.State)
	waiter, err := catalog.RenditionJobWaiterByID(t.Context(), queued.JobID)
	require.NoError(t, err)
	rendition, err := c.Rendition(t.Context(), waiter.AttachmentID, 1<<20)
	require.NoError(t, err)
	var renditionBytes bytes.Buffer
	_, err = rendition.CopyVerified(&renditionBytes)
	require.NoError(t, err)
	require.NoError(t, rendition.Close())
	require.Contains(t, renditionBytes.String(), "synthetic exact phrase")
	search, err := c.SearchDocuments(t.Context(), api.DocumentSearchRequest{Query: "synthetic exact phrase",
		Mode: "lexical", Profile: processing.SuppliedMediaProfileName, Limit: 10,
		Fence: api.DocumentSourceFence{VaultUID: catalog.VaultID(), ContentVersionIDs: []string{version.ID}}})
	require.NoError(t, err)
	require.NotEmpty(t, search.Results)

	origins, err := c.MediaOrigins(t.Context())
	require.NoError(t, err)
	require.Len(t, origins.Items, 1)
	privateReference := "https://recordings.invalid/private-id?token=SECRET"
	plan, err := c.PlanMediaAcquisition(t.Context(), api.MediaReferenceBody{ReferenceURL: privateReference})
	require.NoError(t, err)
	require.NotContains(t, plan.PlanToken, "SECRET")
	grant, err := c.GrantMediaAcquisition(t.Context(), api.MediaAcquisitionGrantBody{
		OperationID: "00000000-0000-4000-8000-000000000306", PlanToken: plan.PlanToken})
	require.NoError(t, err)
	require.NotEmpty(t, grant.GrantID)
	revoke, err := c.RevokeMediaAcquisition(t.Context(), api.MediaAcquisitionRevokeBody{
		OperationID: "00000000-0000-4000-8000-000000000307", OriginID: "synthetic"})
	require.NoError(t, err)
	require.Equal(t, int64(1), revoke.Fence)

	remote, err := c.SubmitRemoteRecording(t.Context(), api.MediaReferenceBody{
		OperationID: "00000000-0000-4000-8000-000000000308", ReferenceURL: privateReference,
		Occurrence: api.MediaOccurrenceBody{Ref: "remote-1", Revision: "1", Filename: "remote.wav"},
	})
	require.NoError(t, err)
	require.Equal(t, "access_required", remote.Outcome)
	remoteStatus, err := c.MediaStatus(t.Context(), remote.SourceID)
	require.NoError(t, err)
	require.Equal(t, remote.OperationID, remoteStatus.OperationID)
	require.Equal(t, "access_required", remoteStatus.Outcome)
	listed, err := c.MediaSources(t.Context(), "", 10)
	require.NoError(t, err)
	require.Contains(t, listed.Items, api.MediaSourceRow{
		SourceID: remote.SourceID, Filename: "remote.wav", Outcome: "access_required",
		CoverageState: "unprocessed",
	})
	require.NotContains(t, remote.SourceID, "private-id")
	require.NotContains(t, body, catalog.BlobsDir)
}

func TestMediaUploadsOutliveRequestTimeout(t *testing.T) {
	t.Parallel()
	for _, artifact := range []bool{false, true} {
		t.Run(fmt.Sprintf("artifact=%t", artifact), func(t *testing.T) {
			ts, catalog := newTestServer(t, configureMediaTestService(t))
			wav := mediatest.WAV()
			supplied := api.MediaSuppliedMetadata{
				OperationID: "00000000-0000-4000-8000-000000000601", Filename: "slow.wav",
				MediaType: "audio/wav", SHA256: processingTestHash(string(wav)), ByteLength: int64(len(wav)),
				Occurrence: api.MediaOccurrenceBody{Ref: "slow-upload", Revision: "1"},
			}
			endpoint := "/api/v1/media/sources"
			var metadata any = supplied
			if artifact {
				retained, err := daemonconn.New(ts.URL, testAPIKey).SubmitSuppliedMedia(t.Context(), supplied, bytes.NewReader(wav))
				require.NoError(t, err)
				endpoint += "/" + retained.SourceID + "/artifacts"
				metadata = api.MediaArtifactMetadata{
					OperationID: "00000000-0000-4000-8000-000000000602", OccurrenceID: retained.OccurrenceID,
					Kind: "media", Filename: supplied.Filename, MediaType: supplied.MediaType,
					SHA256: supplied.SHA256, ByteLength: supplied.ByteLength,
				}
			}
			var body bytes.Buffer
			writer := multipart.NewWriter(&body)
			part, err := writer.CreateFormField("metadata")
			require.NoError(t, err)
			require.NoError(t, json.MarshalWrite(part, metadata))
			part, err = writer.CreateFormFile("file", supplied.Filename)
			require.NoError(t, err)
			_, err = part.Write(wav)
			require.NoError(t, err)
			require.NoError(t, writer.Close())
			synctest.Test(t, func(t *testing.T) {
				reader, stream := io.Pipe()
				defer func() { _ = reader.Close() }()
				go func() {
					time.Sleep(61 * time.Second)
					_, writeErr := stream.Write(body.Bytes())
					_ = stream.CloseWithError(writeErr)
				}()
				request := httptest.NewRequest(http.MethodPost, endpoint, reader)
				request.Header.Set("X-Api-Key", testAPIKey)
				request.Header.Set("Content-Type", writer.FormDataContentType())
				response := httptest.NewRecorder()
				catalog.Server.Handler().ServeHTTP(response, request)
				require.Equal(t, http.StatusOK, response.Code, response.Body.String())
				var receipt api.MediaReceipt
				require.NoError(t, json.Unmarshal(response.Body.Bytes(), &receipt))
				require.Equal(t, "succeeded", receipt.OperationState)
				require.NotEmpty(t, receipt.ContentVersionID)
			})
		})
	}
}

func TestMediaMultipartRejectsIncompleteEnvelopeBeforeRetention(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name      string
		extraPart bool
		malformed bool
		oversized bool
	}{
		{name: "third part", extraPart: true},
		{name: "malformed final boundary", malformed: true},
		{name: "oversized metadata", oversized: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			ts, catalog := newTestServer(t, configureMediaTestService(t))
			wav := mediatest.WAV()
			metadata := api.MediaSuppliedMetadata{OperationID: "00000000-0000-4000-8000-000000000391",
				Filename: "bounded.wav", MediaType: "audio/wav", SHA256: processingTestHash(string(wav)),
				ByteLength: int64(len(wav)), Occurrence: api.MediaOccurrenceBody{Ref: "bounded", Revision: "1"}}
			var body bytes.Buffer
			writer := multipart.NewWriter(&body)
			metadataPart, err := writer.CreateFormField("metadata")
			require.NoError(t, err)
			if test.oversized {
				_, err = metadataPart.Write(bytes.Repeat([]byte("x"), (64<<10)+1))
			} else {
				require.NoError(t, json.MarshalWrite(metadataPart, metadata))
			}
			require.NoError(t, err)
			filePart, err := writer.CreateFormFile("file", "bounded.wav")
			require.NoError(t, err)
			_, err = filePart.Write(wav)
			require.NoError(t, err)
			if test.extraPart {
				extra, createErr := writer.CreateFormField("extra")
				require.NoError(t, createErr)
				_, err = extra.Write([]byte("must not be drained or retained"))
				require.NoError(t, err)
			}
			require.NoError(t, writer.Close())
			raw := body.Bytes()
			if test.malformed {
				raw = raw[:len(raw)-10]
			}
			request, err := http.NewRequestWithContext(t.Context(), http.MethodPost,
				ts.URL+"/api/v1/media/sources", bytes.NewReader(raw))
			require.NoError(t, err)
			request.Header.Set("Content-Type", writer.FormDataContentType())
			response, err := ts.Client().Do(request)
			require.NoError(t, err)
			responseBody, err := io.ReadAll(response.Body)
			require.NoError(t, err)
			require.NoError(t, response.Body.Close())
			require.Equal(t, http.StatusUnprocessableEntity, response.StatusCode, string(responseBody))
			page, err := daemonconn.New(ts.URL, testAPIKey).MediaSources(t.Context(), "", 10)
			require.NoError(t, err)
			require.Empty(t, page.Items)
			staged, err := filepath.Glob(filepath.Join(catalog.BlobsDir, "tmp", ".docbank-media-*"))
			require.NoError(t, err)
			require.Empty(t, staged)
		})
	}
}

func TestMediaRetryClassifiesProcessingErrors(t *testing.T) { //nolint:paralleltest // the two-second consent expiry is measured on the real clock
	ts, catalog := newTestServer(t, configureMediaTestService(t))
	c := daemonconn.New(ts.URL, testAPIKey)
	wav := mediatest.WAV()
	receipt, err := c.SubmitSuppliedMedia(t.Context(), api.MediaSuppliedMetadata{
		OperationID: "00000000-0000-4000-8000-000000000401", Filename: "call.wav",
		MediaType: "audio/wav", SHA256: processingTestHash(string(wav)), ByteLength: int64(len(wav)),
		Occurrence: api.MediaOccurrenceBody{Ref: "call", Revision: "1"},
	}, bytes.NewReader(wav))
	require.NoError(t, err)
	transcript := "synthetic transcript\n"
	metadata := api.MediaArtifactMetadata{
		OperationID: "00000000-0000-4000-8000-000000000402", OccurrenceID: receipt.OccurrenceID,
		Kind: "transcript", Filename: "call.txt", MediaType: "text/plain",
		SHA256: processingTestHash(transcript), ByteLength: int64(len(transcript)),
	}
	_, err = c.ImportMediaArtifact(t.Context(), receipt.SourceID, metadata, strings.NewReader(transcript))
	require.NoError(t, err)
	version, err := catalog.ContentVersionByID(t.Context(), receipt.ContentVersionID)
	require.NoError(t, err)
	selector := api.ProcessingSelector{NodeID: version.NodeID, ContentVersionID: version.ID,
		Profile: processing.SuppliedMediaProfileName}
	plan, err := c.API().PlanDocumentProcessing(t.Context(), &apiclient.PlanDocumentProcessingRequestOptions{Body: &api.ProcessingPlanRequest{Selector: selector}})

	require.NoError(t, err)
	for _, test := range []struct {
		name, profile, code string
		status              int
	}{
		{"unknown profile", "typo", "processing_profile_unavailable", http.StatusUnprocessableEntity},
		{"required", selector.Profile, "processing_consent_required", http.StatusPreconditionRequired},
		{"expired", selector.Profile, "processing_consent_expired", http.StatusPreconditionFailed},
		{"revoked", selector.Profile, "processing_consent_revoked", http.StatusPreconditionFailed},
	} {
		t.Run(test.name, func(t *testing.T) {
			if test.name == "expired" || test.name == "revoked" {
				grant := api.ProcessingConsentGrantRequest{Selector: selector, PlanFingerprint: plan.Fingerprint}
				expires := time.Now().Add(2 * time.Second)
				if test.name == "expired" {
					grant.ExpiresAt = expires.Format(time.RFC3339Nano)
				}
				_, err := c.API().GrantDocumentProcessingConsent(t.Context(), &apiclient.GrantDocumentProcessingConsentRequestOptions{Body: new(grant)})

				require.NoError(t, err)
				if test.name == "expired" {
					time.Sleep(time.Until(expires) + time.Millisecond) //nolint:kennlint // consent expiry is checked against the store's real clock, which has no seam
				} else {
					_, err := c.API().RevokeDocumentProcessingConsent(t.Context())

					require.NoError(t, err)
				}
			}
			response, body := do(t, ts, http.MethodPost,
				"/api/v1/media/sources/"+receipt.SourceID+"/retry", nil, api.MediaRetryBody{
					OperationID: "00000000-0000-4000-8000-000000000403",
					Processing:  &api.MediaProcessingBody{Profile: test.profile},
				})
			require.Equal(t, test.status, response.StatusCode, body)
			var problem api.Error
			require.NoError(t, json.Unmarshal([]byte(body), &problem))
			require.Equal(t, test.code, problem.Code)
		})
	}
}

func TestMediaReferenceRejectsUnsupportedProcessing(t *testing.T) {
	t.Parallel()
	ts, _ := newTestServer(t, configureMediaTestService(t))
	response, body := do(t, ts, http.MethodPost, "/api/v1/media/sources", nil, api.MediaReferenceBody{
		OperationID: "00000000-0000-4000-8000-000000000404", ReferenceURL: "https://recordings.invalid/call",
		Occurrence: api.MediaOccurrenceBody{Ref: "call", Revision: "1"},
		Processing: &api.MediaProcessingBody{Profile: processing.SuppliedMediaProfileName},
	})
	require.Equal(t, http.StatusUnprocessableEntity, response.StatusCode, body)
	var problem api.Error
	require.NoError(t, json.Unmarshal([]byte(body), &problem))
	require.Equal(t, "media_processing_unsupported", problem.Code)
}

func TestMediaHTTPMP3OrdinaryProcessingFreezesRevokedInput(t *testing.T) {
	t.Parallel()
	ts, catalog := newTestServer(t, configureMediaTestService(t))
	c := daemonconn.New(ts.URL, testAPIKey)
	mp3 := mediatest.MP3()
	receipt, err := c.SubmitSuppliedMedia(t.Context(), api.MediaSuppliedMetadata{
		OperationID: "00000000-0000-4000-8000-000000000351", Filename: "call.mp3",
		MediaType: "audio/mpeg", SHA256: processingTestHash(string(mp3)), ByteLength: int64(len(mp3)),
		Occurrence: api.MediaOccurrenceBody{Ref: "mp3-a", Revision: "1", Filename: "call.mp3"},
	}, bytes.NewReader(mp3))
	require.NoError(t, err)
	_, err = c.DeclareMediaOccurrence(t.Context(), api.MediaOccurrenceMutationBody{
		OperationID: "00000000-0000-4000-8000-000000000352", SourceID: receipt.SourceID,
		Occurrence: api.MediaOccurrenceBody{Ref: "mp3-b", Revision: "1", Filename: "call.mp3"},
	})
	require.NoError(t, err)
	transcript := []byte("authenticated ordinary mp3 transcript\n")
	artifact, err := c.ImportMediaArtifact(t.Context(), receipt.SourceID, api.MediaArtifactMetadata{
		OperationID: "00000000-0000-4000-8000-000000000353", OccurrenceID: receipt.OccurrenceID,
		Kind: "transcript", Origin: "supplied", Filename: "call.txt", MediaType: "text/plain",
		SHA256: processingTestHash(string(transcript)), ByteLength: int64(len(transcript)),
	}, bytes.NewReader(transcript))
	require.NoError(t, err)
	require.NotEmpty(t, artifact.SuppliedInputID)
	version, err := catalog.ContentVersionByID(t.Context(), receipt.ContentVersionID)
	require.NoError(t, err)
	selector := api.ProcessingSelector{NodeID: version.NodeID, ContentVersionID: version.ID,
		Profile: processing.SuppliedMediaProfileName}
	plan, err := c.API().PlanDocumentProcessing(t.Context(), &apiclient.PlanDocumentProcessingRequestOptions{Body: &api.ProcessingPlanRequest{Selector: selector}})

	require.NoError(t, err)
	job, err := c.StartProcessing(t.Context(), api.StartProcessingRequest{
		Selector: selector, PlanFingerprint: plan.Fingerprint, Consent: true,
	}, plan.ProfileFingerprint)
	require.NoError(t, err)
	rendered, err := c.Rendition(t.Context(), job.AttachmentID, 1<<20)
	require.NoError(t, err)
	var body bytes.Buffer
	_, err = rendered.CopyVerified(&body)
	require.NoError(t, err)
	require.NoError(t, rendered.Close())
	require.Contains(t, body.String(), "authenticated ordinary mp3 transcript")
	_, err = c.RevokeMediaOccurrence(t.Context(), receipt.OccurrenceID,
		api.MediaOccurrenceRevokeBody{OperationID: "00000000-0000-4000-8000-000000000354", Revision: "1"})
	require.NoError(t, err)
	_, err = c.Rendition(t.Context(), job.AttachmentID, 1<<20)
	require.ErrorIs(t, err, store.ErrNotFound)
}

func TestMediaRouteRejectsCursorOwnedByAnotherPrincipal(t *testing.T) {
	t.Parallel()
	ts, catalog := newTestServer(t, configureMediaTestService(t))
	c := daemonconn.New(ts.URL, testAPIKey)
	for index, operationID := range []string{
		"00000000-0000-4000-8000-000000000361",
		"00000000-0000-4000-8000-000000000362",
	} {
		_, err := c.SubmitRemoteRecording(t.Context(), api.MediaReferenceBody{
			OperationID:  operationID,
			ReferenceURL: fmt.Sprintf("https://recordings.invalid/cursor-%d", index),
			Occurrence:   api.MediaOccurrenceBody{Ref: fmt.Sprintf("cursor-%d", index), Revision: "1"},
		})
		require.NoError(t, err)
	}
	page, err := c.MediaSources(t.Context(), "", 1)
	require.NoError(t, err)
	require.NotEmpty(t, page.NextCursor)

	gate := api.NewOperationGate()
	name, profile, err := processing.NewSuppliedMediaProfile(catalog.Store, catalog.Blobs, "daemon:other")
	require.NoError(t, err)
	var tokenKey [32]byte
	tokenKey[0] = 7
	service, err := processing.NewService(processing.ServiceConfig{
		Catalog: catalog.Store, Blobs: catalog.Blobs, Gate: gate,
		SpoolDirectory: filepath.Join(filepath.Dir(catalog.DBPath), "blobs", "tmp"),
		Principal:      "daemon:other",
		Profiles:       map[string]processing.ProfileConfig{name: profile}, MediaTokenKey: tokenKey,
	})
	require.NoError(t, err)
	cfg := config.Default()
	cfg.Server.APIKey = testAPIKey
	otherServer := api.NewServer(api.Deps{Store: catalog.Store, Blobs: catalog.Blobs,
		VaultRoot: filepath.Dir(catalog.DBPath), Cfg: cfg, Gate: gate, Processing: service,
		WebURL: testWebURL})
	t.Cleanup(otherServer.Close)
	otherHTTP := httptest.NewServer(otherServer.Handler())
	t.Cleanup(otherHTTP.Close)
	otherHTTP.Client().Transport = &apiKeyTransport{key: testAPIKey, next: otherHTTP.Client().Transport}
	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet,
		otherHTTP.URL+"/api/v1/media/sources?limit=1&cursor="+url.QueryEscape(page.NextCursor), nil)
	require.NoError(t, err)
	response, err := otherHTTP.Client().Do(request)
	require.NoError(t, err)
	defer func() { require.NoError(t, response.Body.Close()) }()
	require.Equal(t, http.StatusUnprocessableEntity, response.StatusCode)
}

func configureMediaTestService(t *testing.T) func(*api.Deps) {
	t.Helper()
	return func(deps *api.Deps) {
		gate := api.NewOperationGate()
		deps.Gate = gate
		name, profile, err := processing.NewSuppliedMediaProfile(deps.Store, deps.Blobs, "daemon:operator")
		require.NoError(t, err)
		var key [32]byte
		key[0] = 7
		service, err := processing.NewService(processing.ServiceConfig{Catalog: deps.Store,
			Blobs: deps.Blobs, Gate: gate, SpoolDirectory: filepath.Join(deps.VaultRoot, "blobs", "tmp"),
			Principal: "daemon:operator",
			Profiles:  map[string]processing.ProfileConfig{name: profile}, MediaTokenKey: key,
			MediaOrigins: map[string]processing.MediaOriginPolicy{"synthetic": {
				OriginID: "synthetic", Provider: "synthetic", ResolverFingerprint: processingTestHash("resolver"),
				IdentityFingerprint: processingTestHash("identity"), DisclosureFingerprint: processingTestHash("disclosure"),
				InputClasses: []string{"recording_reference"}, RetainedClasses: []string{"recording_bytes"},
				ReferencePrefixes: []string{"https://recordings.invalid/"}, AcquisitionAvailable: true,
			}},
		})
		require.NoError(t, err)
		deps.Processing = service
		worker, err := processing.NewRenditionWorker(processing.RenditionWorkerConfig{
			Catalog: deps.Store, Blobs: deps.Blobs, Runtime: service.RenditionRuntimes(), Gate: gate,
			Owner: "media-route-worker", LeaseDuration: time.Minute, IdleDelay: time.Millisecond,
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
