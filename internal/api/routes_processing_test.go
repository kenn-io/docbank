package api_test

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/plaintext"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/processing"
)

func TestProcessingPlanRouteIsAuthenticatedAndReturnsReviewedDisclosure(t *testing.T) {
	ts, catalog := newTestServer(t, configureProcessingTestService(t))
	node := createFileWithContent(t, ts, catalog, "/private.txt", "private evidence\n")
	body := map[string]any{"selector": map[string]any{
		"node_id": node.ID, "content_version_id": node.CurrentVersionID, "profile": "private",
	}}

	unauthorized, unauthorizedBody := do(t, ts, http.MethodPost,
		"/api/v1/processing/plans", map[string]string{"X-Api-Key": ""}, body)
	assert.Equal(t, http.StatusUnauthorized, unauthorized.StatusCode, unauthorizedBody)
	unknown, unknownBody := do(t, ts, http.MethodPost, "/api/v1/processing/plans", nil,
		map[string]any{"selector": body["selector"], "unexpected": true})
	assert.Equal(t, http.StatusUnprocessableEntity, unknown.StatusCode, unknownBody)

	response, responseBody := do(t, ts, http.MethodPost, "/api/v1/processing/plans", nil, body)
	require.Equal(t, http.StatusOK, response.StatusCode, responseBody)
	assert.Contains(t, responseBody, `"profile_fingerprint"`)
	assert.Contains(t, responseBody, `"consent_required":true`)
	assert.Contains(t, responseBody, `"consent_state":"required"`)
	assert.Contains(t, responseBody, `"original_file"`)
	assert.NotContains(t, responseBody, catalog.BlobsDir)
}

func TestProcessingProfilesRouteListsExecutableProfilesDeterministically(t *testing.T) {
	ts, _ := newTestServer(t, configureProcessingTestService(t))

	response, body := get(t, ts, "/api/v1/processing/profiles", nil)
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	var profiles []api.ProcessingProfileSummary
	require.NoError(t, json.Unmarshal([]byte(body), &profiles))
	require.Len(t, profiles, 1)
	assert.Equal(t, "private", profiles[0].Name)
	assert.Len(t, profiles[0].Fingerprint, 64)
	assert.True(t, profiles[0].Rendition)
	assert.Empty(t, profiles[0].EmbeddingBindings)
}

func TestProcessingServicePopulatesSuppliedRenditionRuntimeRegistry(t *testing.T) {
	registry := processing.NewRenditionRuntimeRegistry()
	assert.False(t, registry.Ready())

	ts, _ := newTestServer(t, configureProcessingTestServiceWithRegistry(t, registry))
	response, body := get(t, ts, "/api/v1/processing/profiles", nil)
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	assert.True(t, registry.Ready(),
		"the daemon-supervised registry must receive configured providers for restart recovery")
}

func TestProcessingCoverageReportsConfiguredEmbeddingUnavailableBeforeFirstRun(t *testing.T) {
	ts, catalog := newTestServer(t, configureProcessingTestServiceWithEmbedding(t))
	node := createFileWithContent(t, ts, catalog, "/unprocessed.txt", "not processed yet\n")

	response, body := get(t, ts, "/api/v1/coverage?profile=private&vault_uid="+
		catalog.VaultID()+"&content_version_id="+node.CurrentVersionID, nil)
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	assert.Contains(t, body, `"name":"semantic"`)
	assert.Contains(t, body, `"state":"unavailable"`)
}

func TestProcessingJobStreamPublishesDurableIdentityAndSurvivesDisconnect(t *testing.T) {
	inner, err := plaintext.New(plaintext.Profile{MaxDocumentBytes: 1 << 20})
	require.NoError(t, err)
	provider := &blockingProcessingProvider{
		inner: inner, started: make(chan struct{}), release: make(chan struct{}),
	}
	t.Cleanup(func() { closeProcessingSignal(provider.release) })
	ts, catalog := newTestServer(t, configureProcessingTestServiceWithProvider(t, provider))
	node := createFileWithContent(t, ts, catalog, "/stream.txt", "durable before provider completion\n")
	selector := map[string]any{"node_id": node.ID,
		"content_version_id": node.CurrentVersionID, "profile": "private"}
	assertProcessingSurvivesDisconnect(t, ts, selector, provider.started, provider.release)
}

func TestEmbeddingOnlyJobSurvivesDisconnectAfterDurableIdentity(t *testing.T) {
	inner := newProcessingTestEmbeddingProvider(t)
	provider := &blockingProcessingEmbeddingProvider{
		inner: inner, started: make(chan struct{}), release: make(chan struct{}),
	}
	t.Cleanup(func() { closeProcessingSignal(provider.release) })
	ts, catalog := newTestServer(t, configureProcessingTestServiceWithEmbeddingProvider(t, provider, false))
	node := createFileWithContent(t, ts, catalog, "/embedding-only.txt", "embed after disconnect\n")
	selector := map[string]any{"node_id": node.ID,
		"content_version_id": node.CurrentVersionID, "profile": "private"}
	job := assertProcessingSurvivesDisconnect(t, ts, selector, provider.started, provider.release)
	assert.NotEmpty(t, job.EmbeddingJobIDs)
	assert.Empty(t, job.RenditionJobID)
}

func TestRenditionDisconnectDoesNotCancelFollowingEmbeddingEnqueue(t *testing.T) {
	innerRendition, err := plaintext.New(plaintext.Profile{MaxDocumentBytes: 1 << 20})
	require.NoError(t, err)
	rendition := &blockingProcessingProvider{
		inner: innerRendition, started: make(chan struct{}), release: make(chan struct{}),
	}
	t.Cleanup(func() { closeProcessingSignal(rendition.release) })
	embedding := &observedProcessingEmbeddingProvider{
		inner: newProcessingTestEmbeddingProvider(t), started: make(chan struct{}),
	}
	ts, catalog := newTestServer(t,
		configureProcessingTestServiceWithProviders(t, rendition, embedding, true))
	node := createFileWithContent(t, ts, catalog, "/combined.txt", "render and embed after disconnect\n")
	selector := map[string]any{"node_id": node.ID,
		"content_version_id": node.CurrentVersionID, "profile": "private"}
	assertProcessingSurvivesDisconnect(t, ts, selector, rendition.started, rendition.release)
	select {
	case <-embedding.started:
	case <-time.After(time.Second):
		t.Fatal("post-rendition embedding was not enqueued after the response disconnected")
	}
}

func assertProcessingSurvivesDisconnect(t *testing.T, ts *httptest.Server, selector map[string]any,
	started, release chan struct{},
) api.ProcessingJob {
	t.Helper()
	planResponse, planBody := do(t, ts, http.MethodPost, "/api/v1/processing/plans", nil,
		map[string]any{"selector": selector})
	require.Equal(t, http.StatusOK, planResponse.StatusCode, planBody)
	var plan api.ProcessingPlan
	require.NoError(t, json.Unmarshal([]byte(planBody), &plan))
	payload, err := json.Marshal(map[string]any{"selector": selector,
		"plan_fingerprint": plan.Fingerprint, "consent": true})
	require.NoError(t, err)
	request, err := http.NewRequest(http.MethodPost, ts.URL+"/api/v1/processing/jobs", bytes.NewReader(payload))
	require.NoError(t, err)
	request.Header.Set("Content-Type", "application/json")

	type responseResult struct {
		response *http.Response
		err      error
	}
	responseCh := make(chan responseResult, 1)
	go func() {
		response, requestErr := ts.Client().Do(request)
		responseCh <- responseResult{response: response, err: requestErr}
	}()
	select {
	case result := <-responseCh:
		require.NoError(t, result.err)
		require.Equal(t, http.StatusOK, result.response.StatusCode)
		t.Cleanup(func() { _ = result.response.Body.Close() })
		scanner := bufio.NewScanner(result.response.Body)
		require.True(t, scanner.Scan())
		var event api.ProcessingJobEvent
		require.NoError(t, json.Unmarshal(scanner.Bytes(), &event))
		require.NotNil(t, event.Job)
		assert.False(t, event.Terminal)
		jobID := event.Job.ID
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("provider did not start after durable job publication")
		}
		require.NoError(t, result.response.Body.Close())
		time.Sleep(50 * time.Millisecond)
		closeProcessingSignal(release)
		require.Eventually(t, func() bool {
			statusResponse, statusBody := get(t, ts, "/api/v1/processing/jobs/"+jobID, nil)
			return statusResponse.StatusCode == http.StatusOK && strings.Contains(statusBody, `"state":"completed"`)
		}, 3*time.Second, 10*time.Millisecond,
			"accepted processing did not complete after the response stream disconnected")
		return *event.Job
	case <-time.After(time.Second):
		closeProcessingSignal(release)
		result := <-responseCh
		if result.response != nil {
			_ = result.response.Body.Close()
		}
		t.Fatal("processing response headers waited for provider completion")
	}
	return api.ProcessingJob{}
}

func TestProcessingRoutesRunReadCoverAndSearchOneExactVersion(t *testing.T) {
	ts, catalog := newTestServer(t, configureProcessingTestService(t))
	outside := createFileWithContent(t, ts, catalog, "/outside.txt", "needle outside fence\n")
	_ = outside
	node := createFileWithContent(t, ts, catalog, "/private.txt", "needle inside fence\n")
	selector := map[string]any{"node_id": node.ID,
		"content_version_id": node.CurrentVersionID, "profile": "private"}

	planResponse, planBody := do(t, ts, http.MethodPost, "/api/v1/processing/plans", nil,
		map[string]any{"selector": selector})
	require.Equal(t, http.StatusOK, planResponse.StatusCode, planBody)
	var plan api.ProcessingPlan
	require.NoError(t, json.Unmarshal([]byte(planBody), &plan))

	staleResponse, staleBody := do(t, ts, http.MethodPost, "/api/v1/processing/jobs", nil,
		map[string]any{"selector": selector, "plan_fingerprint": processingTestHash("stale"), "consent": true})
	assert.Equal(t, http.StatusConflict, staleResponse.StatusCode, staleBody)
	assert.Contains(t, staleBody, `"code":"processing_plan_changed"`)

	jobResponse, jobBody := do(t, ts, http.MethodPost, "/api/v1/processing/jobs", nil,
		map[string]any{"selector": selector, "plan_fingerprint": plan.Fingerprint, "consent": true})
	require.Equal(t, http.StatusOK, jobResponse.StatusCode, jobBody)
	job := processingJobFromStream(t, jobBody)
	require.NotEmpty(t, job.ID)
	require.NotEmpty(t, job.AttachmentID)

	statusResponse, statusBody := get(t, ts, "/api/v1/processing/jobs/"+job.ID, nil)
	require.Equal(t, http.StatusOK, statusResponse.StatusCode, statusBody)
	assert.Contains(t, statusBody, `"state":"completed"`)

	renditionResponse, renditionBody := get(t, ts, "/api/v1/renditions/"+job.AttachmentID, nil)
	require.Equal(t, http.StatusOK, renditionResponse.StatusCode, renditionBody)
	assert.Contains(t, renditionBody, "docbank-sanitized-markdown/v1")
	assert.Contains(t, renditionBody, "needle inside fence")
	assert.Equal(t, job.AttachmentID, renditionResponse.Header.Get("X-Docbank-Rendition-Attachment"))
	assert.Equal(t, plan.ProfileFingerprint, renditionResponse.Header.Get("X-Docbank-Rendition-Profile"))
	assert.Equal(t, "degraded_provenance", renditionResponse.Header.Get("X-Docbank-Rendition-Completeness"))
	assert.Equal(t, "degraded_provenance", renditionResponse.Header.Get("X-Docbank-Rendition-Warnings"))
	assert.Equal(t, "no-store", renditionResponse.Header.Get("Cache-Control"))
	rangeResponse, rangeBody := get(t, ts, "/api/v1/renditions/"+job.AttachmentID,
		map[string]string{"Range": "bytes=0-31"})
	require.Equal(t, http.StatusPartialContent, rangeResponse.StatusCode, rangeBody)
	assert.Equal(t, renditionBody[:32], rangeBody)
	assert.Equal(t, "bytes 0-31/"+strconv.Itoa(len(renditionBody)), rangeResponse.Header.Get("Content-Range"))
	assert.Equal(t, "no-store", rangeResponse.Header.Get("Cache-Control"))
	assert.NotEmpty(t, rangeResponse.Trailer.Get("Content-Digest"))
	clampedResponse, clampedBody := get(t, ts, "/api/v1/renditions/"+job.AttachmentID,
		map[string]string{"Range": "bytes=0-9223372036854775807"})
	require.Equal(t, http.StatusPartialContent, clampedResponse.StatusCode, clampedBody)
	assert.Equal(t, renditionBody, clampedBody)

	coverageResponse, coverageBody := get(t, ts, "/api/v1/coverage?profile=private&vault_uid="+
		catalog.VaultID()+"&content_version_id="+node.CurrentVersionID, nil)
	require.Equal(t, http.StatusOK, coverageResponse.StatusCode, coverageBody)
	assert.Contains(t, coverageBody, `"state":"complete"`)

	searchResponse, searchBody := do(t, ts, http.MethodPost, "/api/v1/search", nil, map[string]any{
		"query": "needle", "mode": "lexical", "profile": "private",
		"fence": map[string]any{"vault_uid": catalog.VaultID(),
			"content_version_ids": []string{node.CurrentVersionID}},
	})
	require.Equal(t, http.StatusOK, searchResponse.StatusCode, searchBody)
	assert.Contains(t, searchBody, node.CurrentVersionID)
	assert.NotContains(t, searchBody, outside.CurrentVersionID)
}

func TestProcessingConsentRoutesRequireReviewedPlanAndRevocationFailsClosed(t *testing.T) {
	ts, catalog := newTestServer(t, configureProcessingTestService(t))
	node := createFileWithContent(t, ts, catalog, "/consent.txt", "private consent evidence\n")
	selector := map[string]any{"node_id": node.ID,
		"content_version_id": node.CurrentVersionID, "profile": "private"}
	planResponse, planBody := do(t, ts, http.MethodPost, "/api/v1/processing/plans", nil,
		map[string]any{"selector": selector})
	require.Equal(t, http.StatusOK, planResponse.StatusCode, planBody)
	var plan api.ProcessingPlan
	require.NoError(t, json.Unmarshal([]byte(planBody), &plan))

	staleResponse, staleBody := do(t, ts, http.MethodPost, "/api/v1/processing/consent/grants", nil,
		map[string]any{"selector": selector, "plan_fingerprint": processingTestHash("stale")})
	assert.Equal(t, http.StatusConflict, staleResponse.StatusCode, staleBody)
	assert.Contains(t, staleBody, `"code":"processing_plan_changed"`)

	grantResponse, grantBody := do(t, ts, http.MethodPost, "/api/v1/processing/consent/grants", nil,
		map[string]any{"selector": selector, "plan_fingerprint": plan.Fingerprint})
	require.Equal(t, http.StatusOK, grantResponse.StatusCode, grantBody)
	assert.Contains(t, grantBody, `"profile_fingerprint":"`+plan.ProfileFingerprint+`"`)
	activePlanResponse, activePlanBody := do(t, ts, http.MethodPost, "/api/v1/processing/plans", nil,
		map[string]any{"selector": selector})
	require.Equal(t, http.StatusOK, activePlanResponse.StatusCode, activePlanBody)
	var activePlan api.ProcessingPlan
	require.NoError(t, json.Unmarshal([]byte(activePlanBody), &activePlan))
	assert.Equal(t, "active", activePlan.ConsentState)
	assert.False(t, activePlan.ConsentRequired)
	assert.Equal(t, plan.Fingerprint, activePlan.Fingerprint,
		"consent state is not part of the reviewed provider-flow fingerprint")

	jobResponse, jobBody := do(t, ts, http.MethodPost, "/api/v1/processing/jobs", nil,
		map[string]any{"selector": selector, "plan_fingerprint": plan.Fingerprint, "consent": false})
	require.Equal(t, http.StatusOK, jobResponse.StatusCode, jobBody)

	revokeResponse, revokeBody := do(t, ts, http.MethodPost, "/api/v1/processing/consent/revocations", nil,
		map[string]any{})
	require.Equal(t, http.StatusOK, revokeResponse.StatusCode, revokeBody)
	assert.Contains(t, revokeBody, `"revoked_at":`)

	second := createFileWithContent(t, ts, catalog, "/revoked.txt", "must remain private\n")
	secondSelector := map[string]any{"node_id": second.ID,
		"content_version_id": second.CurrentVersionID, "profile": "private"}
	secondPlanResponse, secondPlanBody := do(t, ts, http.MethodPost, "/api/v1/processing/plans", nil,
		map[string]any{"selector": secondSelector})
	require.Equal(t, http.StatusOK, secondPlanResponse.StatusCode, secondPlanBody)
	require.NoError(t, json.Unmarshal([]byte(secondPlanBody), &plan))
	assert.Equal(t, "revoked", plan.ConsentState)
	assert.True(t, plan.ConsentRequired)
	revokedResponse, revokedBody := do(t, ts, http.MethodPost, "/api/v1/processing/jobs", nil,
		map[string]any{"selector": secondSelector, "plan_fingerprint": plan.Fingerprint, "consent": false})
	assert.Equal(t, http.StatusPreconditionFailed, revokedResponse.StatusCode, revokedBody)
	assert.Contains(t, revokedBody, `"code":"processing_consent_revoked"`)
}

func TestDerivativePurgeRequiresExactPreviewAndRemovesLiveRendition(t *testing.T) {
	ts, catalog := newTestServer(t, configureProcessingTestService(t))
	node := createFileWithContent(t, ts, catalog, "/purge.txt", "purge this rendition\n")
	selector := map[string]any{"node_id": node.ID,
		"content_version_id": node.CurrentVersionID, "profile": "private"}
	planResponse, planBody := do(t, ts, http.MethodPost, "/api/v1/processing/plans", nil,
		map[string]any{"selector": selector})
	require.Equal(t, http.StatusOK, planResponse.StatusCode, planBody)
	var processingPlan api.ProcessingPlan
	require.NoError(t, json.Unmarshal([]byte(planBody), &processingPlan))
	jobResponse, jobBody := do(t, ts, http.MethodPost, "/api/v1/processing/jobs", nil,
		map[string]any{"selector": selector, "plan_fingerprint": processingPlan.Fingerprint, "consent": true})
	require.Equal(t, http.StatusOK, jobResponse.StatusCode, jobBody)
	job := processingJobFromStream(t, jobBody)

	purgeRequest := map[string]any{"attachment_ids": []string{job.AttachmentID}}
	purgePlanResponse, purgePlanBody := do(t, ts, http.MethodPost, "/api/v1/derivatives/purge-plans", nil,
		purgeRequest)
	require.Equal(t, http.StatusOK, purgePlanResponse.StatusCode, purgePlanBody)
	var purgePlan api.DerivativePurgePlan
	require.NoError(t, json.Unmarshal([]byte(purgePlanBody), &purgePlan))
	assert.True(t, purgePlan.ImmutableBackupCopiesUntouched)

	staleResponse, staleBody := do(t, ts, http.MethodPost, "/api/v1/derivatives/purge-jobs", nil,
		map[string]any{"attachment_ids": []string{job.AttachmentID},
			"plan_fingerprint": processingTestHash("stale")})
	assert.Equal(t, http.StatusConflict, staleResponse.StatusCode, staleBody)
	assert.Contains(t, staleBody, `"code":"derivative_purge_plan_changed"`)

	purgeResponse, purgeBody := do(t, ts, http.MethodPost, "/api/v1/derivatives/purge-jobs", nil,
		map[string]any{"attachment_ids": []string{job.AttachmentID},
			"plan_fingerprint": purgePlan.Fingerprint})
	require.Equal(t, http.StatusOK, purgeResponse.StatusCode, purgeBody)
	receipt := derivativePurgeReceiptFromStream(t, purgeBody)
	assert.Equal(t, 1, receipt.RemovedAttachments)
	assert.True(t, receipt.ImmutableBackupCopiesUntouched)

	renditionResponse, _ := get(t, ts, "/api/v1/renditions/"+job.AttachmentID, nil)
	assert.Equal(t, http.StatusNotFound, renditionResponse.StatusCode)
}

func processingJobFromStream(t *testing.T, body string) api.ProcessingJob {
	t.Helper()
	lines := strings.Split(strings.TrimSpace(body), "\n")
	require.Len(t, lines, 2)
	var jobEvent, statusEvent api.ProcessingJobEvent
	require.NoError(t, json.Unmarshal([]byte(lines[0]), &jobEvent))
	require.NoError(t, json.Unmarshal([]byte(lines[1]), &statusEvent))
	require.Equal(t, 1, jobEvent.Sequence)
	require.Equal(t, "job", jobEvent.Type)
	require.NotNil(t, jobEvent.Job)
	require.False(t, jobEvent.Terminal)
	require.Equal(t, 2, statusEvent.Sequence)
	require.Equal(t, "status", statusEvent.Type)
	require.NotNil(t, statusEvent.Status)
	require.True(t, statusEvent.Terminal)
	require.Equal(t, jobEvent.Job.ID, statusEvent.Status.JobID)
	return *jobEvent.Job
}

func derivativePurgeReceiptFromStream(t *testing.T, body string) api.DerivativePurgeReceipt {
	t.Helper()
	lines := strings.Split(strings.TrimSpace(body), "\n")
	require.Len(t, lines, 1)
	var event api.DerivativePurgeEvent
	require.NoError(t, json.Unmarshal([]byte(lines[0]), &event))
	require.Equal(t, 1, event.Sequence)
	require.Equal(t, "result", event.Type)
	require.True(t, event.Terminal)
	require.NotNil(t, event.Receipt)
	return *event.Receipt
}

func configureProcessingTestService(t *testing.T) func(*api.Deps) {
	t.Helper()
	provider, err := plaintext.New(plaintext.Profile{MaxDocumentBytes: 1 << 20})
	require.NoError(t, err)
	return configureProcessingTestServiceWithProviderAndRegistry(t, provider, nil)
}

func configureProcessingTestServiceWithRegistry(t *testing.T,
	registry *processing.RenditionRuntimeRegistry,
) func(*api.Deps) {
	t.Helper()
	provider, err := plaintext.New(plaintext.Profile{MaxDocumentBytes: 1 << 20})
	require.NoError(t, err)
	return configureProcessingTestServiceWithProviderAndRegistry(t, provider, registry)
}

func configureProcessingTestServiceWithProvider(t *testing.T,
	provider document.RenditionProvider,
) func(*api.Deps) {
	t.Helper()
	return configureProcessingTestServiceWithProviderAndRegistry(t, provider, nil)
}

func configureProcessingTestServiceWithProviderAndRegistry(t *testing.T,
	provider document.RenditionProvider, registry *processing.RenditionRuntimeRegistry,
) func(*api.Deps) {
	t.Helper()
	return func(deps *api.Deps) {
		gate := api.NewOperationGate()
		deps.Gate = gate
		service, err := processing.NewService(processing.ServiceConfig{
			Catalog: deps.Store, Blobs: deps.Blobs, Gate: gate,
			SpoolDirectory:    filepath.Join(deps.VaultRoot, "blobs", "tmp"),
			RenditionRuntimes: registry,
			Profiles: map[string]processing.ProfileConfig{"private": {
				Profile: processingTestProfile(provider.Descriptor()), RenditionProvider: provider,
			}},
		})
		require.NoError(t, err)
		deps.Processing = service
	}
}

type blockingProcessingProvider struct {
	inner   document.RenditionProvider
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (provider *blockingProcessingProvider) Descriptor() document.RenditionDescriptor {
	return provider.inner.Descriptor()
}

func (provider *blockingProcessingProvider) Render(ctx context.Context, upload document.AuthorizedUpload,
	authorization document.RenditionAuthorization,
) (document.RenditionResult, error) {
	provider.once.Do(func() { close(provider.started) })
	select {
	case <-ctx.Done():
		return document.RenditionResult{}, ctx.Err()
	case <-provider.release:
		return provider.inner.Render(ctx, upload, authorization)
	}
}

type blockingProcessingEmbeddingProvider struct {
	inner   document.EmbeddingProvider
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

type observedProcessingEmbeddingProvider struct {
	inner   document.EmbeddingProvider
	started chan struct{}
	once    sync.Once
}

func (provider *observedProcessingEmbeddingProvider) Descriptor() document.EmbeddingDescriptor {
	return provider.inner.Descriptor()
}

func (provider *observedProcessingEmbeddingProvider) Embed(ctx context.Context,
	inputs []document.EmbeddingInput, authorization document.EmbeddingAuthorization,
) (document.EmbeddingResult, error) {
	provider.once.Do(func() { close(provider.started) })
	return provider.inner.Embed(ctx, inputs, authorization)
}

func (provider *blockingProcessingEmbeddingProvider) Descriptor() document.EmbeddingDescriptor {
	return provider.inner.Descriptor()
}

func (provider *blockingProcessingEmbeddingProvider) Embed(ctx context.Context,
	inputs []document.EmbeddingInput, authorization document.EmbeddingAuthorization,
) (document.EmbeddingResult, error) {
	provider.once.Do(func() { close(provider.started) })
	select {
	case <-ctx.Done():
		return document.EmbeddingResult{}, ctx.Err()
	case <-provider.release:
		return provider.inner.Embed(ctx, inputs, authorization)
	}
}

func closeProcessingSignal(signal chan struct{}) {
	select {
	case <-signal:
	default:
		close(signal)
	}
}

func configureProcessingTestServiceWithEmbedding(t *testing.T) func(*api.Deps) {
	t.Helper()
	return configureProcessingTestServiceWithEmbeddingProvider(t, newProcessingTestEmbeddingProvider(t), true)
}

func configureProcessingTestServiceWithEmbeddingProvider(t *testing.T,
	embedding document.EmbeddingProvider, withRendition bool,
) func(*api.Deps) {
	t.Helper()
	rendition, err := plaintext.New(plaintext.Profile{MaxDocumentBytes: 1 << 20})
	require.NoError(t, err)
	return configureProcessingTestServiceWithProviders(t, rendition, embedding, withRendition)
}

func configureProcessingTestServiceWithProviders(t *testing.T, rendition document.RenditionProvider,
	embedding document.EmbeddingProvider, withRendition bool,
) func(*api.Deps) {
	t.Helper()
	return func(deps *api.Deps) {
		profile := processingTestProfile(rendition.Descriptor())
		var renditionProvider = rendition
		if !withRendition {
			profile.Rendition = nil
			profile.RetentionDisclosure.RetainSanitizedMarkdown = false
			renditionProvider = nil
		}
		descriptor := embedding.Descriptor()
		profile.Embeddings = []document.EmbeddingBindingV1{{
			Activation: document.EmbeddingOptional, AuthorizationFingerprint: processingTestHash("embedding-authorization"),
			CompatibilityID: descriptor.CompatibilityID, CredentialBinding: "credential:test",
			Descriptor: document.ProviderDescriptorV1{ID: descriptor.ID, Fingerprint: descriptor.Fingerprint},
			Dimensions: descriptor.Dimension, DisclosureFingerprint: processingTestHash("embedding-disclosure"),
			DocumentFormatter: descriptor.DocumentFormatter, InputKind: document.EmbeddingInputOriginalFile,
			MaxBatchItems: 8, MaxInputBytes: 1 << 20, MaxResponseBytes: 1 << 20,
			Metric: descriptor.Metric, Model: descriptor.Model, ModelInput: descriptor.ModelInput,
			Name: "semantic", Normalization: descriptor.Normalization, QueryFormatter: descriptor.QueryFormatter,
			ScalarEncoding: descriptor.ScalarEncoding, TrustBoundary: string(descriptor.TrustBoundary),
		}}
		gate := api.NewOperationGate()
		deps.Gate = gate
		service, err := processing.NewService(processing.ServiceConfig{
			Catalog: deps.Store, Blobs: deps.Blobs, Gate: gate,
			SpoolDirectory: filepath.Join(deps.VaultRoot, "blobs", "tmp"),
			Profiles: map[string]processing.ProfileConfig{"private": {
				Profile: profile, RenditionProvider: renditionProvider,
				EmbeddingProviders: map[string]document.EmbeddingProvider{"semantic": embedding},
			}},
		})
		require.NoError(t, err)
		deps.Processing = service
	}
}

type processingTestEmbeddingProvider struct{ descriptor document.EmbeddingDescriptor }

func newProcessingTestEmbeddingProvider(t *testing.T) processingTestEmbeddingProvider {
	t.Helper()
	contract, err := document.NewModelInputContract(document.ModelInputContractConfig{
		Profile: document.ModelInputProfileNomic,
	})
	require.NoError(t, err)
	descriptor, err := document.NewEmbeddingDescriptor(document.EmbeddingDescriptor{
		ID: "synthetic.embedding-v1", ContractVersion: document.EmbeddingProviderContractVersion,
		PolicyFingerprint: processingTestHash("embedding-policy"), TrustBoundary: document.EmbeddingTrustLocalProcess,
		Model: "synthetic-model", ModelRevision: "v1", Dimension: 2,
		Metric: document.VectorMetricCosine, Normalization: document.VectorNormalizationNone,
		ScalarEncoding: "float32", DocumentFormatter: "synthetic/document-v1",
		QueryFormatter: "synthetic/query-v1", InputKinds: []document.EmbeddingInputKind{document.EmbeddingInputOriginalFile},
		CompatibilityID: contract.CompatibilityID, SupportsTextQuery: true, ModelInput: contract,
		SupportedRequestModes: []document.ModelInputMode{contract.Document.Mode},
	})
	require.NoError(t, err)
	return processingTestEmbeddingProvider{descriptor: descriptor}
}

func (provider processingTestEmbeddingProvider) Descriptor() document.EmbeddingDescriptor {
	return provider.descriptor
}

func (processingTestEmbeddingProvider) Embed(_ context.Context, inputs []document.EmbeddingInput,
	_ document.EmbeddingAuthorization,
) (document.EmbeddingResult, error) {
	vectors := make([]document.EmbeddingVector, len(inputs))
	for index, input := range inputs {
		vectors[index] = document.EmbeddingVector{Key: input.Key, Values: []float32{1, 0}}
	}
	return document.EmbeddingResult{Vectors: vectors}, nil
}

func processingTestProfile(descriptor document.RenditionDescriptor) document.ProcessingProfileV1 {
	hash := processingTestHash
	return document.ProcessingProfileV1{
		ContractVersion: document.ProcessingProfileContractV1,
		Rendition: &document.RenditionBindingV1{
			AdapterContract: "plaintext.in-process/v1", AuthorizationFingerprint: hash("authorization"),
			CredentialBinding: "credential:none", DeploymentFingerprint: hash("deployment"),
			Descriptor:            document.ProviderDescriptorV1{ID: descriptor.ID, Fingerprint: descriptor.Fingerprint},
			DisclosureFingerprint: hash("rendition-disclosure"), MaxDocumentBytes: 1 << 20,
			MaxResponseBytes: 1 << 20, MaxUnits: 1000, Name: "plaintext",
			RequestedArtifacts: []document.EvidenceArtifactRole{document.EvidenceArtifactStructured},
			TrustBoundary:      string(descriptor.TrustBoundary), UploadOptionsFingerprint: hash("upload"),
		},
		EvidenceLexical: document.EvidenceLexicalPolicyV1{
			CompletenessFingerprint: hash("completeness"), LexicalSegmenterFingerprint: hash("segments"),
			MaxSegmentRunes: 1000, MaxUnitRunes: 100_000,
			NormalizedEvidenceContract: document.NormalizedEvidenceContractV1,
			NormalizerFingerprint:      hash("normalizer"), RenditionContract: document.RenditionContractV1,
			SanitizerFingerprint: hash("sanitizer"), SourceEvidenceContract: document.SourceEvidenceContractV1,
		},
		RetentionDisclosure: document.RetentionDisclosurePolicyV1{
			AttachmentPolicyFingerprint: hash("attachment"), ConsentFingerprint: hash("consent"),
			RetainSanitizedMarkdown: true, TrustBoundary: string(descriptor.TrustBoundary),
		},
		Retrieval: document.RetrievalPolicyV1{LexicalLimit: 50, VectorLimit: 50},
	}
}

func processingTestHash(value string) string {
	digest := sha256.Sum256([]byte(value))
	return hex.EncodeToString(digest[:])
}
