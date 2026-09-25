package api_test

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"
	"uuid"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/plaintext"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/apiclient"
	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/docbank/internal/processing"
	"go.kenn.io/docbank/internal/retrieval"
	"go.kenn.io/docbank/internal/store"
	"go.kenn.io/kit/packstore"
)

func TestProcessingPlanRouteIsAuthenticatedAndReturnsReviewedDisclosure(t *testing.T) {
	t.Parallel()
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
	assert.Contains(t, responseBody, `"disclose_filename":true`)
	assert.Contains(t, responseBody, `"filename":"private.txt"`)
	var disclosure map[string]any
	require.NoError(t, json.Unmarshal([]byte(responseBody), &disclosure))
	flow, ok := disclosure["flow"].([]any)
	require.True(t, ok)
	require.NotEmpty(t, flow)
	hop, ok := flow[0].(map[string]any)
	require.True(t, ok)
	runtime, ok := hop["runtime_disclosure"].(map[string]any)
	require.True(t, ok, "each provider hop must expose its complete runtime disclosure")
	assert.Equal(t, "plaintext.in-process-v1", runtime["immediate_processor"])
	assert.Equal(t, "plaintext.in-process-v1", runtime["ultimate_processor"])
	assert.Equal(t, "in-process", runtime["endpoint"])
	assert.Equal(t, []any{"byte_length", "content_hash", "detected_media_type", "sanitized_filename"},
		runtime["metadata_classes"])
	assert.Equal(t, []any{"normalized_evidence", "sanitized_markdown"}, runtime["retained_artifact_roles"])
	assert.NotContains(t, responseBody, catalog.BlobsDir)
	assert.NotContains(t, responseBody, "credential:")
}

func TestProcessingProfilesRouteListsExecutableProfilesDeterministically(t *testing.T) {
	t.Parallel()
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
	assert.Empty(t, profiles[0].QueryEmbeddingBindings)
}

func TestProcessingProfilesReportQueryEmbeddingBindings(t *testing.T) {
	t.Parallel()
	t.Run("query capable", func(t *testing.T) {
		ts, _ := newTestServer(t, configureProcessingTestServiceWithEmbeddingProvider(t,
			newProcessingTestEmbeddingProvider(t), true))
		response, body := get(t, ts, "/api/v1/processing/profiles", nil)
		require.Equal(t, http.StatusOK, response.StatusCode, body)
		var profiles []api.ProcessingProfileSummary
		require.NoError(t, json.Unmarshal([]byte(body), &profiles))
		require.Len(t, profiles, 1)
		assert.Equal(t, []string{"semantic"}, profiles[0].EmbeddingBindings)
		assert.Equal(t, []string{"semantic"}, profiles[0].QueryEmbeddingBindings)
	})

	t.Run("processing only", func(t *testing.T) {
		ts, _ := newTestServer(t, configureProcessingTestServiceWithEmbeddingProvider(t,
			newProcessingTestEmbeddingProviderWithoutQuery(t), true))
		response, body := get(t, ts, "/api/v1/processing/profiles", nil)
		require.Equal(t, http.StatusOK, response.StatusCode, body)
		var profiles []api.ProcessingProfileSummary
		require.NoError(t, json.Unmarshal([]byte(body), &profiles))
		require.Len(t, profiles, 1)
		assert.Equal(t, []string{"semantic"}, profiles[0].EmbeddingBindings)
		assert.Empty(t, profiles[0].QueryEmbeddingBindings)
	})
}

func TestProcessingProfilesReportReranking(t *testing.T) {
	t.Parallel()
	t.Run("absent without provider", func(t *testing.T) {
		ts, _ := newTestServer(t, configureProcessingTestService(t))
		response, body := get(t, ts, "/api/v1/processing/profiles", nil)
		require.Equal(t, http.StatusOK, response.StatusCode, body)
		var profiles []api.ProcessingProfileSummary
		require.NoError(t, json.Unmarshal([]byte(body), &profiles))
		require.Len(t, profiles, 1)
		assert.False(t, profiles[0].RerankingAvailable)
		assert.NotContains(t, body, "reranking_available")
	})

	t.Run("true with provider", func(t *testing.T) {
		ts, _ := newTestServer(t, configureProcessingTestServiceWithReranker(t,
			&routeRerankingProvider{}, retrieval.ProviderFailureDegrade))
		response, body := get(t, ts, "/api/v1/processing/profiles", nil)
		require.Equal(t, http.StatusOK, response.StatusCode, body)
		var profiles []api.ProcessingProfileSummary
		require.NoError(t, json.Unmarshal([]byte(body), &profiles))
		require.Len(t, profiles, 1)
		assert.True(t, profiles[0].RerankingAvailable)
		assert.Contains(t, body, `"reranking_available":true`)
	})
}

func TestProcessingServicePopulatesSuppliedRenditionRuntimeRegistry(t *testing.T) {
	t.Parallel()
	registry := processing.NewRenditionRuntimeRegistry()
	assert.False(t, registry.Ready())

	ts, _ := newTestServer(t, configureProcessingTestServiceWithRegistry(t, registry))
	response, body := get(t, ts, "/api/v1/processing/profiles", nil)
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	assert.True(t, registry.Ready(),
		"the daemon-supervised registry must receive configured providers for restart recovery")
}

func TestProcessingCoverageReportsConfiguredEmbeddingUnavailableBeforeFirstRun(t *testing.T) {
	t.Parallel()
	ts, catalog := newTestServer(t, configureProcessingTestServiceWithEmbedding(t))
	node := createFileWithContent(t, ts, catalog, "/unprocessed.txt", "not processed yet\n")

	response, body := get(t, ts, "/api/v1/coverage?profile=private&vault_uid="+
		catalog.VaultID()+"&content_version_id="+node.CurrentVersionID, nil)
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	assert.Contains(t, body, `"name":"semantic"`)
	assert.Contains(t, body, `"state":"unavailable"`)
}

func TestProcessingClientReturnsCompletedEmbeddingIDs(t *testing.T) {
	t.Parallel()
	ts, catalog := newTestServer(t, configureProcessingTestServiceWithEmbedding(t))
	node := createFileWithContent(t, ts, catalog, "/combined-receipt.txt", "synthetic combined receipt\n")
	c := daemonconn.New(ts.URL, testAPIKey)
	selector := api.ProcessingSelector{NodeID: node.ID, ContentVersionID: node.CurrentVersionID, Profile: "private"}
	plan, err := c.API().PlanDocumentProcessing(t.Context(), &apiclient.PlanDocumentProcessingRequestOptions{Body: &api.ProcessingPlanRequest{Selector: selector}})

	require.NoError(t, err)
	job, err := c.StartProcessing(t.Context(), api.StartProcessingRequest{Selector: selector,
		PlanFingerprint: plan.Fingerprint, Consent: true}, plan.ProfileFingerprint)
	require.NoError(t, err)
	status, err := c.ProcessingStatus(t.Context(), job.ID)
	require.NoError(t, err)
	require.Len(t, status.EmbeddingJobIDs, 1)
	require.Equal(t, status.EmbeddingJobIDs, job.EmbeddingJobIDs)
}

func TestProcessingClientReportsRequiredEmbeddingFailure(t *testing.T) {
	t.Parallel()
	rendition, err := plaintext.New(plaintext.Profile{MaxDocumentBytes: 1 << 20})
	require.NoError(t, err)
	provider := invalidProcessingEmbeddingProvider{EmbeddingProvider: newProcessingTestEmbeddingProvider(t)}
	for _, activation := range []document.EmbeddingActivation{document.EmbeddingRequired, document.EmbeddingOptional} {
		t.Run(string(activation), func(t *testing.T) {
			ts, catalog := newTestServer(t,
				configureProcessingTestServiceWithProviders(t, rendition, provider, true, activation))
			node := createFileWithContent(t, ts, catalog, "/embedding-failure.txt", "synthetic embedding failure\n")
			c := daemonconn.New(ts.URL, testAPIKey)
			selector := api.ProcessingSelector{NodeID: node.ID, ContentVersionID: node.CurrentVersionID, Profile: "private"}
			plan, err := c.API().PlanDocumentProcessing(t.Context(), &apiclient.PlanDocumentProcessingRequestOptions{Body: &api.ProcessingPlanRequest{Selector: selector}})

			require.NoError(t, err)
			job, startErr := c.StartProcessing(t.Context(), api.StartProcessingRequest{
				Selector: selector, PlanFingerprint: plan.Fingerprint, Consent: true}, plan.ProfileFingerprint)
			require.NotEmpty(t, job.ID)
			status, err := c.ProcessingStatus(t.Context(), job.ID)
			require.NoError(t, err)
			require.Equal(t, "invalid_response", status.FailureCode)
			if activation == document.EmbeddingRequired {
				require.Equal(t, "failed", status.State)
				require.Error(t, startErr)
			} else {
				require.Equal(t, "partial", status.State)
				require.NoError(t, startErr)
			}
		})
	}
}

type invalidProcessingEmbeddingProvider struct{ document.EmbeddingProvider }

func (invalidProcessingEmbeddingProvider) Embed(context.Context, []document.EmbeddingInput,
	document.EmbeddingAuthorization,
) (document.EmbeddingResult, error) {
	return document.EmbeddingResult{}, nil
}

func TestProcessingCoverageTracksTrashRestoreAndSupersessionEligibility(t *testing.T) {
	t.Parallel()
	ts, catalog := newTestServer(t, configureProcessingTestService(t))
	node := createFileWithContent(t, ts, catalog, "/coverage.txt", "current retained evidence\n")
	runProcessingForCoverage(t, ts, node)

	readCoverage := func(versionID string) api.CoverageReport {
		t.Helper()
		response, body := get(t, ts, "/api/v1/coverage?profile=private&vault_uid="+
			catalog.VaultID()+"&content_version_id="+versionID, nil)
		require.Equal(t, http.StatusOK, response.StatusCode, body)
		var report api.CoverageReport
		require.NoError(t, json.Unmarshal([]byte(body), &report))
		return report
	}
	assert.Equal(t, 1, readCoverage(node.CurrentVersionID).Renditions.Complete)

	trashed, _, err := catalog.Trash(t.Context(), node.ID, node.Revision)
	require.NoError(t, err)
	trashedCoverage := readCoverage(node.CurrentVersionID)
	assert.Equal(t, "stale", trashedCoverage.Renditions.State)
	assert.Equal(t, 1, trashedCoverage.Renditions.Stale)
	assert.Zero(t, trashedCoverage.Renditions.Complete)

	restored, _, err := catalog.Restore(t.Context(), trashed.ID, trashed.Revision)
	require.NoError(t, err)
	assert.Equal(t, 1, readCoverage(node.CurrentVersionID).Renditions.Complete)

	replacementHash, replacementSize, err := catalog.Blobs.Write(strings.NewReader("replacement evidence\n"))
	require.NoError(t, err)
	replaced, replacement, err := catalog.ReplaceContent(t.Context(), restored.ID, restored.Revision,
		replacementHash, replacementSize, "text/plain")
	require.NoError(t, err)
	assert.Equal(t, replacement.ID, replaced.CurrentVersionID)
	superseded := readCoverage(node.CurrentVersionID)
	assert.Equal(t, "stale", superseded.Renditions.State)
	assert.Equal(t, 1, superseded.Renditions.Stale)
	current := readCoverage(replacement.ID)
	assert.Equal(t, "unavailable", current.Renditions.State)
	assert.Equal(t, 1, current.Renditions.Unavailable)
}

func TestProcessingSourceFenceResolveRouteSupportsExactIDsAndMetadataFilters(t *testing.T) {
	t.Parallel()
	ts, catalog := newTestServer(t, configureProcessingTestService(t))
	scope, err := catalog.Mkdir(t.Context(), catalog.RootID(), "scope")
	require.NoError(t, err)
	first := createFileWithContent(t, ts, catalog, "/first.txt", "first\n")
	second, err := catalog.CreateFile(t.Context(), scope.ID, "second.pdf", testHash("second"), 6,
		"Application/PDF; version=1")
	require.NoError(t, err)
	tag, err := catalog.CreateTag(t.Context(), "selected")
	require.NoError(t, err)
	_, err = catalog.AssignTag(t.Context(), tag.ID, second.ID, second.Revision)
	require.NoError(t, err)

	response, body := do(t, ts, http.MethodPost, "/api/v1/processing/source-fences/resolve", nil,
		map[string]any{"content_version_ids": []string{second.CurrentVersionID, first.CurrentVersionID}})
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	var explicit api.DocumentSourceFenceResolution
	require.NoError(t, json.Unmarshal([]byte(body), &explicit))
	assert.Equal(t, catalog.VaultID(), explicit.Fence.VaultUID)
	assert.ElementsMatch(t, []string{first.CurrentVersionID, second.CurrentVersionID},
		explicit.Fence.ContentVersionIDs)
	assert.Equal(t, 2, explicit.ObservedScopeCount)
	assert.Regexp(t, `^sha256:[0-9a-f]{64}$`, explicit.FenceFingerprint)

	response, body = do(t, ts, http.MethodPost, "/api/v1/processing/source-fences/resolve", nil,
		map[string]any{"filters": map[string]any{
			"tag_id": tag.ID, "mime_type": "APPLICATION/PDF", "under_node_id": scope.ID,
		}})
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	var filtered api.DocumentSourceFenceResolution
	require.NoError(t, json.Unmarshal([]byte(body), &filtered))
	assert.Equal(t, []string{second.CurrentVersionID}, filtered.Fence.ContentVersionIDs)
	assert.Equal(t, 1, filtered.ObservedScopeCount)

	response, body = do(t, ts, http.MethodPost, "/api/v1/processing/source-fences/resolve", nil,
		map[string]any{"filters": map[string]any{}})
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	require.NoError(t, json.Unmarshal([]byte(body), &filtered))
	assert.Equal(t, 2, filtered.ObservedScopeCount)
}

func TestProcessingSourceFenceResolveRouteReturnsNonNullEmptyFence(t *testing.T) {
	t.Parallel()
	ts, _ := newTestServer(t, configureProcessingTestService(t))
	response, body := do(t, ts, http.MethodPost, "/api/v1/processing/source-fences/resolve", nil,
		map[string]any{"filters": map[string]any{}})
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	assert.Contains(t, body, `"content_version_ids":[]`)
	assert.NotContains(t, body, `"content_version_ids":null`)
	var resolved api.DocumentSourceFenceResolution
	require.NoError(t, json.Unmarshal([]byte(body), &resolved))
	assert.NotNil(t, resolved.Fence.ContentVersionIDs)
	assert.Empty(t, resolved.Fence.ContentVersionIDs)
	assert.Equal(t, 0, resolved.ObservedScopeCount)
	assert.Regexp(t, `^sha256:[0-9a-f]{64}$`, resolved.FenceFingerprint)
}

func TestProcessingSearchValidationMatchesSearchQueryAndProfileErrors(t *testing.T) {
	t.Parallel()
	ts, catalog := newTestServer(t, configureProcessingTestService(t))
	node := createFileWithContent(t, ts, catalog, "/search-validation.txt", "synthetic search evidence\n")

	for _, test := range []struct {
		name, query, profile string
	}{
		{name: "whitespace query", query: "   ", profile: "private"},
		{name: "unconfigured profile", query: "synthetic", profile: "missing"},
	} {
		t.Run(test.name, func(t *testing.T) {
			validationResponse, validationBody := do(t, ts, http.MethodPost, "/api/v1/search/validate", nil,
				map[string]any{"query": test.query, "mode": "lexical", "limit": 1, "profile": test.profile})
			searchResponse, searchBody := do(t, ts, http.MethodPost, "/api/v1/search", nil,
				map[string]any{"query": test.query, "mode": "lexical", "limit": 1, "profile": test.profile,
					"fence": map[string]any{"vault_uid": catalog.VaultID(),
						"content_version_ids": []string{node.CurrentVersionID}}})
			assert.Equal(t, searchResponse.StatusCode, validationResponse.StatusCode)
			var validationProblem, searchProblem api.Error
			require.NoError(t, json.Unmarshal([]byte(validationBody), &validationProblem))
			require.NoError(t, json.Unmarshal([]byte(searchBody), &searchProblem))
			assert.Equal(t, searchProblem.Code, validationProblem.Code)
		})
	}
}

func TestProcessingSourceFenceResolveRouteFailsClosedWithStableSanitizedErrors(t *testing.T) {
	t.Parallel()
	ts, catalog := newTestServer(t, configureProcessingTestService(t))
	current := createFileWithContent(t, ts, catalog, "/current.txt", "current\n")
	oldID := current.CurrentVersionID
	replacementHash, replacementSize, err := catalog.Blobs.Write(strings.NewReader("replacement\n"))
	require.NoError(t, err)
	current, _, err = catalog.ReplaceContent(t.Context(), current.ID, current.Revision,
		replacementHash, replacementSize, "text/plain")
	require.NoError(t, err)
	tooMany := make([]string, processing.MaxSourceFenceIDs+1)
	for index := range tooMany {
		tooMany[index] = fmt.Sprintf("00000000-0000-4000-8000-%012d", index)
	}
	response, body := do(t, ts, http.MethodPost, "/api/v1/processing/source-fences/resolve", nil,
		map[string]any{"content_version_ids": tooMany})
	assert.Equal(t, http.StatusUnprocessableEntity, response.StatusCode, body)
	var scopeProblem api.Error
	require.NoError(t, json.Unmarshal([]byte(body), &scopeProblem))
	assert.Equal(t, "scope_too_large", scopeProblem.Code)
	assert.Equal(t, processing.MaxSourceFenceIDs+1, scopeProblem.ObservedScopeCount)
	assert.Contains(t, scopeProblem.Detail, "narrow the source scope")

	for _, testCase := range []struct {
		name       string
		request    map[string]any
		wantStatus int
		wantCode   string
	}{
		{name: "missing mode", request: map[string]any{}, wantStatus: http.StatusUnprocessableEntity, wantCode: "validation"},
		{name: "both modes", request: map[string]any{"content_version_ids": []string{current.CurrentVersionID}, "filters": map[string]any{}}, wantStatus: http.StatusUnprocessableEntity, wantCode: "validation"},
		{name: "unknown filter", request: map[string]any{"filters": map[string]any{"path": "/private/host/path"}}, wantStatus: http.StatusUnprocessableEntity, wantCode: "validation"},
		{name: "stale version", request: map[string]any{"content_version_ids": []string{oldID}}, wantStatus: http.StatusConflict, wantCode: "stale_version"},
		{name: "missing version", request: map[string]any{"content_version_ids": []string{"33333333-3333-4333-8333-333333333333"}}, wantStatus: http.StatusNotFound, wantCode: "not_found"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			response, body := do(t, ts, http.MethodPost, "/api/v1/processing/source-fences/resolve", nil,
				testCase.request)
			assert.Equal(t, testCase.wantStatus, response.StatusCode, body)
			var problem api.Error
			require.NoError(t, json.Unmarshal([]byte(body), &problem))
			assert.Equal(t, testCase.wantCode, problem.Code)
			assert.NotContains(t, body, catalog.DBPath)
			assert.NotContains(t, body, catalog.BlobsDir)
			assert.NotContains(t, body, "SELECT")
		})
	}
}

func runProcessingForCoverage(t *testing.T, ts *httptest.Server, node store.Node) {
	t.Helper()
	selector := map[string]any{"node_id": node.ID,
		"content_version_id": node.CurrentVersionID, "profile": "private"}
	planResponse, planBody := do(t, ts, http.MethodPost, "/api/v1/processing/plans", nil,
		map[string]any{"selector": selector})
	require.Equal(t, http.StatusOK, planResponse.StatusCode, planBody)
	var plan api.ProcessingPlan
	require.NoError(t, json.Unmarshal([]byte(planBody), &plan))
	jobResponse, jobBody := do(t, ts, http.MethodPost, "/api/v1/processing/jobs", nil,
		map[string]any{"selector": selector, "plan_fingerprint": plan.Fingerprint, "consent": true})
	require.Equal(t, http.StatusOK, jobResponse.StatusCode, jobBody)
	processingJobFromStream(t, jobBody)
}

func TestProcessingJobStreamPublishesDurableIdentityAndSurvivesDisconnect(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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

func TestProcessingShutdownDrainsAcceptedJobAndPreservesRecovery(t *testing.T) {
	t.Parallel()
	inner := newProcessingTestEmbeddingProvider(t)
	provider := &shutdownProcessingEmbeddingProvider{
		EmbeddingProvider: inner, started: make(chan struct{}),
		cancelled: make(chan struct{}), release: make(chan struct{}),
	}
	ts, catalog := newTestServer(t, configureProcessingTestServiceWithEmbeddingProvider(t, provider, false))
	t.Cleanup(func() { closeProcessingSignal(provider.release) })
	node := createFileWithContent(t, ts, catalog, "/shutdown.txt", "recover accepted processing after shutdown\n")
	c := daemonconn.New(ts.URL, testAPIKey)
	selector := api.ProcessingSelector{NodeID: node.ID, ContentVersionID: node.CurrentVersionID, Profile: "private"}
	plan, err := c.API().PlanDocumentProcessing(t.Context(), &apiclient.PlanDocumentProcessingRequestOptions{Body: &api.ProcessingPlanRequest{Selector: selector}})

	require.NoError(t, err)
	start := api.StartProcessingRequest{Selector: selector, PlanFingerprint: plan.Fingerprint, Consent: true}
	payload, err := json.Marshal(start)
	require.NoError(t, err)
	request, err := http.NewRequestWithContext(t.Context(), http.MethodPost,
		ts.URL+"/api/v1/processing/jobs", bytes.NewReader(payload))
	require.NoError(t, err)
	request.Header.Set("Content-Type", "application/json")
	response, err := ts.Client().Do(request)
	require.NoError(t, err)
	defer func() { _ = response.Body.Close() }()
	require.Equal(t, http.StatusOK, response.StatusCode)
	scanner := bufio.NewScanner(response.Body)
	require.True(t, scanner.Scan())
	var event api.ProcessingJobEvent
	require.NoError(t, json.Unmarshal(scanner.Bytes(), &event))
	require.NotNil(t, event.Job)
	require.Len(t, event.Job.EmbeddingJobIDs, 1)
	select {
	case <-provider.started:
	case <-time.After(3 * time.Second):
		t.Fatal("accepted embedding did not reach the provider")
	}
	require.NoError(t, response.Body.Close())

	// HTTP shutdown cannot drain the worker after this client has disconnected.
	// The API owner must cancel it and wait for provider cleanup itself.
	shutdownCtx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	require.ErrorIs(t, catalog.Server.Shutdown(shutdownCtx), context.DeadlineExceeded)
	select {
	case <-provider.cancelled:
	case <-time.After(10 * time.Second):
		t.Fatal("shutdown did not cancel accepted processing")
	}
	closed := make(chan struct{})
	go func() {
		catalog.Server.Close()
		close(closed)
	}()
	select {
	case <-closed:
		t.Fatal("server closed while the provider still owned processing resources")
	case <-time.After(50 * time.Millisecond):
	}
	closeProcessingSignal(provider.release)
	select {
	case <-closed:
	case <-time.After(3 * time.Second):
		t.Fatal("server did not drain accepted processing after provider cleanup")
	}

	rejected, err := c.StartProcessing(t.Context(), start, plan.ProfileFingerprint)
	require.Error(t, err, "shutdown must reject new processing starts")
	require.Empty(t, rejected.ID)

	jobID := event.Job.EmbeddingJobIDs[0]
	interrupted, err := catalog.EmbeddingJobByID(t.Context(), jobID)
	require.NoError(t, err)
	require.Equal(t, "queued", interrupted.State)
	require.Empty(t, interrupted.FailureCode)
	// A restarted daemon reclaims the same durable job immediately after cleanup.
	runtime, err := processing.NewProviderEmbeddingRuntime(inner, catalog.Blobs, t.TempDir(),
		func(error) (processing.EmbeddingProviderFailure, time.Duration) {
			return processing.EmbeddingProviderInvalidResponse, 0
		})
	require.NoError(t, err)
	worker, err := processing.NewEmbeddingWorker(processing.EmbeddingWorkerConfig{
		Catalog: catalog.Store, Authority: catalog.Store, Blobs: catalog.Blobs,
		GenerationBlobs: catalog.Blobs, Runtime: runtime, Gate: api.NewOperationGate(),
		Owner: "restarted-processing-worker", LeaseDuration: time.Minute, IdleDelay: time.Millisecond,
		RetryLimit: 3, RetryBaseDelay: time.Millisecond, MaxRetryDelay: time.Second,
		AttemptLifetime: 10 * time.Minute, MaxRows: 100_000, MaxDimensions: 1_048_576,
		MaxVectorBlobBytes:     64 << 20,
		DescriptorFingerprints: []string{inner.Descriptor().Fingerprint},
	})
	require.NoError(t, err)
	processed, err := worker.RunJob(t.Context(), jobID)
	require.NoError(t, err)
	require.True(t, processed)
	recovered, err := catalog.EmbeddingJobByID(t.Context(), jobID)
	require.NoError(t, err)
	require.Equal(t, "completed", recovered.State)
}

type shutdownProcessingEmbeddingProvider struct {
	document.EmbeddingProvider

	started, cancelled, release chan struct{}
}

func (provider *shutdownProcessingEmbeddingProvider) Embed(ctx context.Context,
	inputs []document.EmbeddingInput, authorization document.EmbeddingAuthorization,
) (document.EmbeddingResult, error) {
	close(provider.started)
	select {
	case <-ctx.Done():
		close(provider.cancelled)
		<-provider.release
		return document.EmbeddingResult{}, ctx.Err()
	case <-provider.release:
		return provider.EmbeddingProvider.Embed(ctx, inputs, authorization)
	}
}

func TestRenditionDisconnectDoesNotCancelFollowingEmbeddingEnqueue(t *testing.T) {
	t.Parallel()
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
		configureProcessingTestServiceWithProviders(t, rendition, embedding, true, document.EmbeddingOptional))
	node := createFileWithContent(t, ts, catalog, "/combined.txt", "render and embed after disconnect\n")
	selector := map[string]any{"node_id": node.ID,
		"content_version_id": node.CurrentVersionID, "profile": "private"}
	assertProcessingSurvivesDisconnect(t, ts, selector, rendition.started, rendition.release)
	select {
	case <-embedding.started:
	case <-time.After(10 * time.Second):
		t.Fatal("post-rendition embedding was not enqueued after the response disconnected")
	}
}

func assertProcessingSurvivesDisconnect(t *testing.T, ts *httptest.Server, selector map[string]any,
	started, release chan struct{},
) api.ProcessingJob {
	t.Helper()
	// These ordering checks include real SQLite, blob, and HTTP work on CI.
	const waitBudget = 10 * time.Second
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
		case <-time.After(waitBudget):
			t.Fatal("provider did not start after durable job publication")
		}
		require.NoError(t, result.response.Body.Close())
		time.Sleep(50 * time.Millisecond) //nolint:kennlint // gives the server's real TCP connection time to observe the client disconnect
		closeProcessingSignal(release)
		require.Eventually(t, func() bool {
			statusResponse, statusBody := get(t, ts, "/api/v1/processing/jobs/"+jobID, nil)
			return statusResponse.StatusCode == http.StatusOK && strings.Contains(statusBody, `"state":"completed"`)
		}, waitBudget, 10*time.Millisecond,
			"accepted processing did not complete after the response stream disconnected")
		return *event.Job
	case <-time.After(waitBudget):
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
	t.Parallel()
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

	invalidRange, invalidBody := get(t, ts, "/api/v1/renditions/"+job.AttachmentID, map[string]string{"Range": "bytes=999999999-"})
	require.Equal(t, http.StatusRequestedRangeNotSatisfiable, invalidRange.StatusCode, invalidBody)

	selectorResponse, selectorBody := do(t, ts, http.MethodPost, "/api/v1/renditions/select", nil,
		map[string]any{"selector": selector, "max_bytes": len(renditionBody)})
	require.Equal(t, http.StatusOK, selectorResponse.StatusCode, selectorBody)
	assert.Equal(t, renditionBody, selectorBody)
	assert.Equal(t, job.AttachmentID, selectorResponse.Header.Get("X-Docbank-Rendition-Attachment"))

	windowResponse, windowBody := do(t, ts, http.MethodPost, "/api/v1/renditions/windows", nil,
		map[string]any{"vault_id": catalog.VaultID(), "node_id": node.ID,
			"content_version_id": node.CurrentVersionID, "attachment_id": job.AttachmentID,
			"offset": 5, "max_chars": 9})
	require.Equal(t, http.StatusOK, windowResponse.StatusCode, windowBody)
	var window api.RenditionTextWindow
	require.NoError(t, json.Unmarshal([]byte(windowBody), &window))
	runes := []rune(renditionBody)
	assert.Equal(t, string(runes[5:14]), window.Text)
	assert.Equal(t, 5, window.RequestedOffset)
	assert.Equal(t, 14, window.NextOffset)
	assert.Equal(t, len(window.Text), window.ResponseBytes)
	assert.Equal(t, job.AttachmentID, window.AttachmentID)
	assert.Equal(t, node.CurrentVersionID, window.ContentVersionID)
	assert.Equal(t, "text/markdown", window.MediaType)

	mismatchResponse, mismatchBody := do(t, ts, http.MethodPost, "/api/v1/renditions/windows", nil,
		map[string]any{"vault_id": catalog.VaultID(), "node_id": node.ID + 1,
			"content_version_id": node.CurrentVersionID, "attachment_id": job.AttachmentID,
			"offset": 0, "max_chars": 1})
	assert.Equal(t, http.StatusNotFound, mismatchResponse.StatusCode, mismatchBody)
	assert.Contains(t, mismatchBody, `"code":"not_found"`)

	coverageResponse, coverageBody := get(t, ts, "/api/v1/coverage?profile=private&vault_uid="+
		catalog.VaultID()+"&content_version_id="+node.CurrentVersionID, nil)
	require.Equal(t, http.StatusOK, coverageResponse.StatusCode, coverageBody)
	assert.Contains(t, coverageBody, `"state":"complete"`)
	processingClient := daemonconn.New(ts.URL, testAPIKey)
	vaultID, err := uuid.Parse(catalog.VaultID())
	require.NoError(t, err)
	coverage, err := processingClient.API().GetDocumentProcessingCoverage(t.Context(), &apiclient.GetDocumentProcessingCoverageRequestOptions{Query: &apiclient.GetDocumentProcessingCoverageQuery{Profile: "private", VaultUID: vaultID, ContentVersionID: []string{node.CurrentVersionID, outside.CurrentVersionID}}})
	require.NoError(t, err)
	assert.Equal(t, 1, coverage.Renditions.Complete)
	assert.Equal(t, 1, coverage.Renditions.Unavailable)

	searchResponse, searchBody := do(t, ts, http.MethodPost, "/api/v1/search", nil, map[string]any{
		"query": "needle", "mode": "lexical", "profile": "private",
		"fence": map[string]any{"vault_uid": catalog.VaultID(),
			"content_version_ids": []string{node.CurrentVersionID}},
	})
	require.Equal(t, http.StatusOK, searchResponse.StatusCode, searchBody)
	assert.Contains(t, searchBody, node.CurrentVersionID)
	assert.NotContains(t, searchBody, outside.CurrentVersionID)
}

func TestDocumentSearchRejectsWhitespaceQuery(t *testing.T) {
	t.Parallel()
	ts, catalog := newTestServer(t, configureProcessingTestService(t))
	node := createFileWithContent(t, ts, catalog, "/search.txt", "searchable content\n")
	response, body := do(t, ts, http.MethodPost, "/api/v1/search", nil, map[string]any{
		"query": " \t\n", "mode": "lexical", "profile": "private",
		"fence": map[string]any{"vault_uid": catalog.VaultID(),
			"content_version_ids": []string{node.CurrentVersionID}},
	})
	require.Equal(t, http.StatusUnprocessableEntity, response.StatusCode, body)
	require.Contains(t, body, `"code":"search_query_required"`)
}

func TestDocumentSearchRerankingReturnsAppliedAndDegradedReceipts(t *testing.T) {
	t.Parallel()
	t.Run("applied", func(t *testing.T) {
		provider := &routeRerankingProvider{}
		ts, catalog := newTestServer(t, configureProcessingTestServiceWithReranker(t, provider,
			retrieval.ProviderFailureDegrade))
		first := createFileWithContent(t, ts, catalog, "/first.txt", "needle first result\n")
		second := createFileWithContent(t, ts, catalog, "/second.txt", "needle second result\n")
		runProcessingForCoverage(t, ts, first)
		runProcessingForCoverage(t, ts, second)
		baseRequest := map[string]any{"query": "needle", "mode": "lexical", "profile": "private",
			"fence": map[string]any{"vault_uid": catalog.VaultID(),
				"content_version_ids": []string{first.CurrentVersionID, second.CurrentVersionID}}}
		baseResponse, baseBody := do(t, ts, http.MethodPost, "/api/v1/search", nil, baseRequest)
		require.Equal(t, http.StatusOK, baseResponse.StatusCode, baseBody)
		falseRequest := maps.Clone(baseRequest)
		falseRequest["rerank"] = false
		falseResponse, falseBody := do(t, ts, http.MethodPost, "/api/v1/search", nil, falseRequest)
		require.Equal(t, http.StatusOK, falseResponse.StatusCode, falseBody)
		assert.Equal(t, baseBody, falseBody)
		revocationResponse, revocationBody := do(t, ts, http.MethodPost,
			"/api/v1/processing/consent/revocations", nil, nil)
		require.Equal(t, http.StatusOK, revocationResponse.StatusCode, revocationBody)

		request := map[string]any{"query": "needle", "mode": "lexical", "profile": "private",
			"rerank": true, "fence": map[string]any{"vault_uid": catalog.VaultID(),
				"content_version_ids": []string{first.CurrentVersionID, second.CurrentVersionID}}}
		deniedResponse, deniedBody := do(t, ts, http.MethodPost, "/api/v1/search", nil, request)
		require.Equal(t, http.StatusOK, deniedResponse.StatusCode, deniedBody)
		assert.Contains(t, deniedBody, `"cause":"authorization_denied"`)
		assert.Zero(t, provider.calls)
		grantProcessingTestConsent(t, ts, first)
		response, body := do(t, ts, http.MethodPost, "/api/v1/search", nil, request)
		require.Equal(t, http.StatusOK, response.StatusCode, body)
		assert.Contains(t, body, `"outcome":"applied"`)
		assert.Contains(t, body, `"candidate_count":2`)
		assert.NotContains(t, body, "private query")
		assert.Positive(t, provider.calls)
		var report api.DocumentSearchReport
		require.NoError(t, json.Unmarshal([]byte(body), &report))
		require.Len(t, report.Results, 2)
		assert.Equal(t, second.CurrentVersionID, report.Results[0].ContentVersionID)
	})

	t.Run("degraded provider", func(t *testing.T) {
		provider := &routeRerankingProvider{err: errors.New("provider body and secret")}
		ts, catalog := newTestServer(t, configureProcessingTestServiceWithReranker(t, provider,
			retrieval.ProviderFailureDegrade))
		first := createFileWithContent(t, ts, catalog, "/degraded.txt", "needle degraded result\n")
		runProcessingForCoverage(t, ts, first)
		grantProcessingTestConsent(t, ts, first)
		planResponse, planBody := do(t, ts, http.MethodPost, "/api/v1/processing/plans", nil,
			map[string]any{"selector": map[string]any{"node_id": first.ID,
				"content_version_id": first.CurrentVersionID, "profile": "private"}})
		require.Equal(t, http.StatusOK, planResponse.StatusCode, planBody)
		var plan api.ProcessingPlan
		require.NoError(t, json.Unmarshal([]byte(planBody), &plan))
		var rerankingHop *api.ProcessingFlowHop
		for index := range plan.Flow {
			if plan.Flow[index].Capability == "reranking" {
				rerankingHop = &plan.Flow[index]
			}
		}
		require.NotNil(t, rerankingHop)
		assert.Equal(t, processingTestHash("reranker-policy"), rerankingHop.RuntimeDisclosure.Deployment)
		assert.Equal(t, 1, plan.Estimate.ProviderCalls)
		request := map[string]any{"query": "needle", "mode": "lexical", "profile": "private",
			"rerank": true, "fence": map[string]any{"vault_uid": catalog.VaultID(),
				"content_version_ids": []string{first.CurrentVersionID}}}
		response, body := do(t, ts, http.MethodPost, "/api/v1/search", nil, request)
		require.Equal(t, http.StatusOK, response.StatusCode, body)
		assert.Contains(t, body, `"reranking_degraded"`)
		assert.Contains(t, body, `"cause":"unavailable"`)
		assert.NotContains(t, body, "provider body")
	})
}

func TestDocumentSearchRerankingRejectsUnavailableAndFailClosed(t *testing.T) {
	t.Parallel()
	t.Run("unavailable", func(t *testing.T) {
		ts, catalog := newTestServer(t, configureProcessingTestService(t))
		node := createFileWithContent(t, ts, catalog, "/unavailable.txt", "needle unavailable\n")
		runProcessingForCoverage(t, ts, node)
		response, body := do(t, ts, http.MethodPost, "/api/v1/search", nil, map[string]any{
			"query": "needle", "mode": "lexical", "profile": "private", "rerank": true,
			"fence": map[string]any{"vault_uid": catalog.VaultID(),
				"content_version_ids": []string{node.CurrentVersionID}},
		})
		require.Equal(t, http.StatusUnprocessableEntity, response.StatusCode, body)
		assert.Contains(t, body, `"code":"reranking_unavailable"`)
	})

	t.Run("fail closed", func(t *testing.T) {
		provider := &routeRerankingProvider{err: errors.New("provider failure")}
		ts, catalog := newTestServer(t, configureProcessingTestServiceWithReranker(t, provider,
			retrieval.ProviderFailureFailClosed))
		node := createFileWithContent(t, ts, catalog, "/fail-closed.txt", "needle fail closed\n")
		runProcessingForCoverage(t, ts, node)
		grantProcessingTestConsent(t, ts, node)
		response, body := do(t, ts, http.MethodPost, "/api/v1/search", nil, map[string]any{
			"query": "needle", "mode": "lexical", "profile": "private", "rerank": true,
			"fence": map[string]any{"vault_uid": catalog.VaultID(),
				"content_version_ids": []string{node.CurrentVersionID}},
		})
		require.Equal(t, http.StatusBadGateway, response.StatusCode, body)
		assert.Contains(t, body, `"code":"reranking_failed"`)
		assert.NotContains(t, body, "provider failure")
	})
}

func TestProcessingConsentRoutesRequireReviewedPlanAndRevocationFailsClosed(t *testing.T) {
	t.Parallel()
	ts, catalog := newTestServer(t, configureProcessingTestService(t))
	node := createFileWithContent(t, ts, catalog, "/consent.txt", "private consent evidence\n")
	selector := map[string]any{"node_id": node.ID,
		"content_version_id": node.CurrentVersionID, "profile": "private"}
	planResponse, planBody := do(t, ts, http.MethodPost, "/api/v1/processing/plans", nil,
		map[string]any{"selector": selector})
	require.Equal(t, http.StatusOK, planResponse.StatusCode, planBody)
	var plan api.ProcessingPlan
	require.NoError(t, json.Unmarshal([]byte(planBody), &plan))

	requiredResponse, requiredBody := do(t, ts, http.MethodPost, "/api/v1/processing/jobs", nil,
		map[string]any{"selector": selector, "plan_fingerprint": plan.Fingerprint, "consent": false})
	require.Equal(t, http.StatusPreconditionRequired, requiredResponse.StatusCode, requiredBody)
	require.Contains(t, requiredBody, `"code":"processing_consent_required"`)

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

	grantedResponse, grantedBody := do(t, ts, http.MethodPost, "/api/v1/processing/plans", nil,
		map[string]any{"selector": selector})
	require.Equal(t, http.StatusOK, grantedResponse.StatusCode, grantedBody)
	var grantedPlan api.ProcessingPlan
	require.NoError(t, json.Unmarshal([]byte(grantedBody), &grantedPlan))
	assert.False(t, grantedPlan.ConsentRequired)
	assert.Equal(t, plan.Fingerprint, grantedPlan.Fingerprint)

	jobResponse, jobBody := do(t, ts, http.MethodPost, "/api/v1/processing/jobs", nil,
		map[string]any{"selector": selector, "plan_fingerprint": plan.Fingerprint, "consent": false})
	require.Equal(t, http.StatusOK, jobResponse.StatusCode, jobBody)

	revokeResponse, revokeBody := do(t, ts, http.MethodPost, "/api/v1/processing/consent/revocations", nil, nil)
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
	t.Parallel()
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
	packed, err := catalog.Blobs.Maintainer().Pack(t.Context(), packstore.PackOptions{})
	require.NoError(t, err)
	require.Positive(t, packed.PacksSealed)

	purgeRequest := map[string]any{"attachment_ids": []string{job.AttachmentID}}
	purgePlanResponse, purgePlanBody := do(t, ts, http.MethodPost, "/api/v1/derivatives/purge-plans", nil,
		purgeRequest)
	require.Equal(t, http.StatusOK, purgePlanResponse.StatusCode, purgePlanBody)
	var purgePlan api.DerivativePurgePlan
	require.NoError(t, json.Unmarshal([]byte(purgePlanBody), &purgePlan))
	assert.True(t, purgePlan.ImmutableBackupCopiesUntouched)

	createFileWithContent(t, ts, catalog, "/unrelated.txt", "unrelated ingest must not invalidate the preview\n")

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

func TestDerivativePurgeRoutesRejectNonCanonicalContentVersionIDs(t *testing.T) {
	t.Parallel()
	ts, _ := newTestServer(t, configureProcessingTestService(t))
	for _, route := range []string{"purge-plans", "purge-jobs"} {
		t.Run(route, func(t *testing.T) {
			request := map[string]any{"content_version_ids": []string{"ABCDEFAB-1234-4ABC-8DEF-123456789ABC"}}
			if route == "purge-jobs" {
				request["plan_fingerprint"] = processingTestHash("preview")
			}
			response, body := do(t, ts, http.MethodPost, "/api/v1/derivatives/"+route, nil, request)
			require.Equal(t, http.StatusUnprocessableEntity, response.StatusCode, body)
			require.Contains(t, body, "content_version_ids")
		})
	}
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

func configureProcessingTestServiceWithReranker(t *testing.T, reranker retrieval.RerankingProvider,
	failurePolicy retrieval.ProviderFailurePolicy,
) func(*api.Deps) {
	t.Helper()
	return func(deps *api.Deps) {
		rendition, err := plaintext.New(plaintext.Profile{MaxDocumentBytes: 1 << 20})
		require.NoError(t, err)
		gate := api.NewOperationGate()
		deps.Gate = gate
		service, err := processing.NewService(processing.ServiceConfig{
			Catalog: deps.Store, Blobs: deps.Blobs, Gate: gate,
			SpoolDirectory: filepath.Join(deps.VaultRoot, "blobs", "tmp"),
			Profiles: map[string]processing.ProfileConfig{"private": {
				Profile: processingTestProfile(rendition.Descriptor()), RenditionProvider: rendition,
				RerankingProvider: reranker,
				RerankingProfile: retrieval.RerankingProfile{ID: "synthetic-reranker", MaxCandidates: 2,
					MaxExcerptBytes: 4096},
				RerankingDisclosure: processing.RuntimeDisclosure{
					ImmediateProcessor: "synthetic-reranker", UltimateProcessor: "synthetic-reranker",
					Endpoint: "https://reranker.example.invalid", Deployment: processingTestHash("reranker-policy"),
					Model: "synthetic-reranker", ModelRevision: "v1",
					MetadataClasses: []string{"query_text", "candidate_excerpt", "candidate_identity", "evidence_reference"},
				},
				RerankingDeadline: time.Second, RerankingFailurePolicy: failurePolicy,
			}},
		})
		require.NoError(t, err)
		deps.Processing = service
	}
}

func grantProcessingTestConsent(t *testing.T, ts *httptest.Server, node store.Node) {
	t.Helper()
	selector := map[string]any{"node_id": node.ID, "content_version_id": node.CurrentVersionID, "profile": "private"}
	planResponse, planBody := do(t, ts, http.MethodPost, "/api/v1/processing/plans", nil,
		map[string]any{"selector": selector})
	require.Equal(t, http.StatusOK, planResponse.StatusCode, planBody)
	var plan api.ProcessingPlan
	require.NoError(t, json.Unmarshal([]byte(planBody), &plan))
	grantResponse, grantBody := do(t, ts, http.MethodPost, "/api/v1/processing/consent/grants", nil,
		map[string]any{"selector": selector, "plan_fingerprint": plan.Fingerprint})
	require.Equal(t, http.StatusOK, grantResponse.StatusCode, grantBody)
}

type routeRerankingProvider struct {
	err        error
	calls      int
	candidates []retrieval.RerankingCandidate
}

func (provider *routeRerankingProvider) Rerank(_ context.Context, request retrieval.RerankingRequest) ([]retrieval.RerankScore, error) {
	provider.calls++
	provider.candidates = append([]retrieval.RerankingCandidate(nil), request.Candidates...)
	if provider.err != nil {
		return nil, provider.err
	}
	scores := make([]retrieval.RerankScore, len(request.Candidates))
	for index, candidate := range request.Candidates {
		scores[index] = retrieval.RerankScore{Document: candidate.Document, Score: float64(index + 1)}
	}
	return scores, nil
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
	return configureProcessingTestServiceWithProviders(t, rendition, embedding, withRendition, document.EmbeddingOptional)
}

func configureProcessingTestServiceWithProviders(t *testing.T, rendition document.RenditionProvider,
	embedding document.EmbeddingProvider, withRendition bool, activation document.EmbeddingActivation,
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
			Activation: activation, AuthorizationFingerprint: processingTestHash("embedding-authorization"),
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
	return newProcessingTestEmbeddingProviderWithQuerySupport(t, true)
}

func newProcessingTestEmbeddingProviderWithoutQuery(t *testing.T) processingTestEmbeddingProvider {
	t.Helper()
	return newProcessingTestEmbeddingProviderWithQuerySupport(t, false)
}

func newProcessingTestEmbeddingProviderWithQuerySupport(t *testing.T, supportsQuery bool) processingTestEmbeddingProvider {
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
		CompatibilityID: contract.CompatibilityID, SupportsTextQuery: supportsQuery, ModelInput: contract,
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
			MaxResponseBytes: 1 << 20, MaxUnits: 1000, Name: "plaintext", DiscloseFilename: true,
			RequestedArtifacts: []document.EvidenceArtifactRole{document.EvidenceArtifactStructured},
			TrustBoundary:      string(descriptor.TrustBoundary), UploadOptionsFingerprint: hash("upload"),
		},
		EvidenceLexical: document.EvidenceLexicalPolicyV1{
			CompletenessFingerprint: hash("completeness"), LexicalSegmenterFingerprint: hash("segments"),
			MaxDocumentChars: 1 << 20, MaxSegmentRunes: 1000, MaxUnitRunes: 100_000,
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

type slowRenditionWriter struct {
	*httptest.ResponseRecorder

	afterFirstWrite func()
}

func (w *slowRenditionWriter) Write(p []byte) (int, error) {
	n, err := w.ResponseRecorder.Write(p)
	if err != nil {
		return n, fmt.Errorf("recording slow rendition: %w", err)
	}
	if w.afterFirstWrite != nil {
		w.afterFirstWrite()
		w.afterFirstWrite = nil
	}
	return n, nil
}
func TestRenditionDownloadOutlivesRequestTimeout(t *testing.T) {
	t.Parallel()
	ts, catalog := newTestServer(t, configureProcessingTestService(t))
	node := createFileWithContent(t, ts, catalog, "/synthetic.txt", strings.Repeat("synthetic transfer evidence\n", 3000))
	selector := map[string]any{"node_id": node.ID, "content_version_id": node.CurrentVersionID, "profile": "private"}
	response, body := do(t, ts, http.MethodPost, "/api/v1/processing/plans", nil, map[string]any{"selector": selector})
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	var plan api.ProcessingPlan
	require.NoError(t, json.Unmarshal([]byte(body), &plan))
	response, body = do(t, ts, http.MethodPost, "/api/v1/processing/jobs", nil, map[string]any{"selector": selector, "plan_fingerprint": plan.Fingerprint, "consent": true})
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	job := processingJobFromStream(t, body)
	_, full := get(t, ts, "/api/v1/renditions/"+job.AttachmentID, nil)
	synctest.Test(t, func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/renditions/"+job.AttachmentID, nil)
		req.Header.Set("X-Api-Key", testAPIKey)
		writer := &slowRenditionWriter{ResponseRecorder: httptest.NewRecorder(),
			afterFirstWrite: func() { time.Sleep(61 * time.Second) }}
		catalog.Server.Handler().ServeHTTP(writer, req)
		require.Equal(t, full, writer.Body.String())
		require.NotEmpty(t, writer.Result().Trailer.Get("Content-Digest"))
	})
}

type consentChangeProvider struct {
	document.RenditionProvider

	afterRender func()
}

func (p *consentChangeProvider) Render(ctx context.Context, upload document.AuthorizedUpload,
	auth document.RenditionAuthorization) (document.RenditionResult, error) {
	result, err := p.RenditionProvider.Render(ctx, upload, auth)
	if err == nil {
		p.afterRender()
	}
	return result, err
}

func TestProcessingReportsConsentExpiredDuringRendition(t *testing.T) { //nolint:paralleltest // the two-second consent expiry is measured on the real clock
	base, err := plaintext.New(plaintext.Profile{MaxDocumentBytes: 1 << 20})
	require.NoError(t, err)
	provider := &consentChangeProvider{RenditionProvider: base}
	ts, catalog := newTestServer(t, func(deps *api.Deps) {
		gate := api.NewOperationGate()
		deps.Gate = gate
		deps.Processing, err = processing.NewService(processing.ServiceConfig{
			Catalog: deps.Store, Blobs: deps.Blobs, Gate: gate,
			SpoolDirectory: filepath.Join(deps.VaultRoot, "blobs", "tmp"),
			Profiles: map[string]processing.ProfileConfig{"private": {
				Profile: processingTestProfile(provider.Descriptor()), RenditionProvider: provider}},
		})
		require.NoError(t, err)
	})
	node := createFileWithContent(t, ts, catalog, "/consent-change.txt", "synthetic consent evidence\n")
	c := daemonconn.New(ts.URL, testAPIKey)
	selector := api.ProcessingSelector{NodeID: node.ID, ContentVersionID: node.CurrentVersionID, Profile: "private"}
	plan, err := c.API().PlanDocumentProcessing(t.Context(), &apiclient.PlanDocumentProcessingRequestOptions{Body: &api.ProcessingPlanRequest{Selector: selector}})

	require.NoError(t, err)
	grant := api.ProcessingConsentGrantRequest{Selector: selector, PlanFingerprint: plan.Fingerprint}
	expires := time.Now().UTC().Add(2 * time.Second)
	grant.ExpiresAt = expires.Format(time.RFC3339Nano)
	provider.afterRender = func() { time.Sleep(time.Until(expires) + time.Millisecond) } //nolint:kennlint // consent expiry is checked against the store's real clock, which has no seam

	_, err = c.API().GrantDocumentProcessingConsent(t.Context(), &apiclient.GrantDocumentProcessingConsentRequestOptions{Body: new(grant)})

	require.NoError(t, err)
	for range 2 {
		_, err = c.StartProcessing(t.Context(), api.StartProcessingRequest{Selector: selector,
			PlanFingerprint: plan.Fingerprint}, plan.ProfileFingerprint)
		require.ErrorIs(t, err, daemonconn.ErrProcessingConsent)
		code, ok := daemonconn.ProblemCode(err)
		require.True(t, ok)
		require.Equal(t, "processing_consent_expired", code)
	}
}

func TestDerivativePurgeReturnsCommittedReceiptWhenCleanupFails(t *testing.T) {
	t.Parallel()
	ts, catalog := newTestServer(t, configureProcessingTestService(t))
	node := createFileWithContent(t, ts, catalog, "/purge-partial.txt", "synthetic partial purge evidence\n")
	c := daemonconn.New(ts.URL, testAPIKey)
	selector := api.ProcessingSelector{NodeID: node.ID, ContentVersionID: node.CurrentVersionID, Profile: "private"}
	plan, err := c.API().PlanDocumentProcessing(t.Context(), &apiclient.PlanDocumentProcessingRequestOptions{Body: &api.ProcessingPlanRequest{Selector: selector}})

	require.NoError(t, err)
	job, err := c.StartProcessing(t.Context(), api.StartProcessingRequest{Selector: selector,
		PlanFingerprint: plan.Fingerprint, Consent: true}, plan.ProfileFingerprint)
	require.NoError(t, err)
	purgePlan, err := c.API().PlanDerivativePurge(t.Context(), &apiclient.PlanDerivativePurgeRequestOptions{Body: &api.DerivativePurgePlanRequest{AttachmentIDs: []string{job.AttachmentID}}})

	require.NoError(t, err)
	view, err := catalog.ActiveRenditionByAttachment(t.Context(), job.AttachmentID)
	require.NoError(t, err)
	// A nonempty directory at a loose object path makes physical removal fail
	// after the catalog purge, on every supported platform.
	hash := view.Build.Artifacts[0].BlobHash
	path := filepath.Join(catalog.BlobsDir, hash[:2], hash)
	require.FileExists(t, path)
	require.NoError(t, os.Remove(path))
	require.NoError(t, os.Mkdir(path, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(path, "locked"), []byte("synthetic obstruction"), 0o600))
	receipt, err := c.RunDerivativePurge(t.Context(), api.DerivativePurgeJobRequest{
		AttachmentIDs: []string{job.AttachmentID}, PlanFingerprint: purgePlan.Fingerprint})
	require.Error(t, err)
	require.NotEmpty(t, receipt.ID)
	assert.Equal(t, "partial", receipt.Outcome)
	assert.Equal(t, 1, receipt.RemovedAttachments)
	_, err = catalog.ActiveRenditionByAttachment(t.Context(), job.AttachmentID)
	require.ErrorIs(t, err, store.ErrNotFound)
	// Cleanup failure must leave the original readable.
	response, _ := get(t, ts, "/api/v1/versions/"+node.CurrentVersionID+"/content", nil)
	require.Equal(t, http.StatusOK, response.StatusCode)
}

func TestDerivativePurgePreviewTracksOnlySelectedDerivatives(t *testing.T) {
	t.Parallel()
	ts, catalog := newTestServer(t, configureProcessingTestService(t))
	c := daemonconn.New(ts.URL, testAPIKey)
	first := createFileWithContent(t, ts, catalog, "/first.txt", "first synthetic source\n")
	selector := api.ProcessingSelector{NodeID: first.ID, ContentVersionID: first.CurrentVersionID, Profile: "private"}
	versionRequest := api.DerivativePurgePlanRequest{ContentVersionIDs: []string{first.CurrentVersionID}}
	before, err := c.API().PlanDerivativePurge(t.Context(), &apiclient.PlanDerivativePurgeRequestOptions{Body: new(versionRequest)})

	require.NoError(t, err)
	plan, err := c.API().PlanDocumentProcessing(t.Context(), &apiclient.PlanDocumentProcessingRequestOptions{Body: &api.ProcessingPlanRequest{Selector: selector}})

	require.NoError(t, err)
	job, err := c.StartProcessing(t.Context(), api.StartProcessingRequest{Selector: selector,
		PlanFingerprint: plan.Fingerprint, Consent: true}, plan.ProfileFingerprint)
	require.NoError(t, err)
	_, err = c.RunDerivativePurge(t.Context(), api.DerivativePurgeJobRequest{
		ContentVersionIDs: versionRequest.ContentVersionIDs, PlanFingerprint: before.Fingerprint})
	require.ErrorIs(t, err, daemonconn.ErrProcessingPlanChanged)
	view, err := catalog.ActiveRenditionByAttachment(t.Context(), job.AttachmentID)
	require.NoError(t, err)
	requests := []api.DerivativePurgePlanRequest{versionRequest,
		{AttachmentIDs: []string{job.AttachmentID}}, {BuildIDs: []string{view.Build.ID}}, {All: true}}
	previews := make([]*api.DerivativePurgePlan, len(requests))
	for index, request := range requests {
		previews[index], err = c.API().PlanDerivativePurge(t.Context(), &apiclient.PlanDerivativePurgeRequestOptions{Body: new(request)})

		require.NoError(t, err)
	}
	second := createFileWithContent(t, ts, catalog, "/second.txt", "second unrelated synthetic source\n")
	selector = api.ProcessingSelector{NodeID: second.ID, ContentVersionID: second.CurrentVersionID, Profile: "private"}
	plan, err = c.API().PlanDocumentProcessing(t.Context(), &apiclient.PlanDocumentProcessingRequestOptions{Body: &api.ProcessingPlanRequest{Selector: selector}})

	require.NoError(t, err)
	_, err = c.StartProcessing(t.Context(), api.StartProcessingRequest{Selector: selector, PlanFingerprint: plan.Fingerprint}, plan.ProfileFingerprint)
	require.NoError(t, err)
	for index, request := range requests {
		after, err := c.API().PlanDerivativePurge(t.Context(), &apiclient.PlanDerivativePurgeRequestOptions{Body: new(request)})

		require.NoError(t, err)
		if request.All {
			assert.NotEqual(t, previews[index].Fingerprint, after.Fingerprint)
		} else {
			assert.Equal(t, previews[index].Fingerprint, after.Fingerprint)
		}
	}
}
