package api_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/plaintext"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/client"
	"go.kenn.io/docbank/internal/processing"
	"go.kenn.io/docbank/internal/store"
	"go.kenn.io/kit/packstore"
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
	assert.Contains(t, responseBody, `"original_file"`)
	assert.Contains(t, responseBody, `"disclose_filename":true`)
	assert.Contains(t, responseBody, `"filename":"private.txt"`)
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
	rangeResponse, rangeBody := get(t, ts, "/api/v1/renditions/"+job.AttachmentID,
		map[string]string{"Range": "bytes=0-31"})
	require.Equal(t, http.StatusPartialContent, rangeResponse.StatusCode, rangeBody)
	assert.Equal(t, renditionBody[:32], rangeBody)
	assert.Equal(t, "bytes 0-31/"+strconv.Itoa(len(renditionBody)), rangeResponse.Header.Get("Content-Range"))
	assert.NotEmpty(t, rangeResponse.Trailer.Get("Content-Digest"))
	clampedResponse, clampedBody := get(t, ts, "/api/v1/renditions/"+job.AttachmentID,
		map[string]string{"Range": "bytes=0-9223372036854775807"})
	require.Equal(t, http.StatusPartialContent, clampedResponse.StatusCode, clampedBody)
	assert.Equal(t, renditionBody, clampedBody)

	invalidRange, invalidBody := get(t, ts, "/api/v1/renditions/"+job.AttachmentID, map[string]string{"Range": "bytes=999999999-"})
	require.Equal(t, http.StatusRequestedRangeNotSatisfiable, invalidRange.StatusCode, invalidBody)

	coverageResponse, coverageBody := get(t, ts, "/api/v1/coverage?profile=private&vault_uid="+
		catalog.VaultID()+"&content_version_id="+node.CurrentVersionID, nil)
	require.Equal(t, http.StatusOK, coverageResponse.StatusCode, coverageBody)
	assert.Contains(t, coverageBody, `"state":"complete"`)
	processingClient := client.New(ts.URL, testAPIKey)
	coverage, err := processingClient.DocumentCoverage(t.Context(), "private", api.DocumentSourceFence{
		VaultUID: catalog.VaultID(), ContentVersionIDs: []string{node.CurrentVersionID, outside.CurrentVersionID}})
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
	return func(deps *api.Deps) {
		provider, err := plaintext.New(plaintext.Profile{MaxDocumentBytes: 1 << 20})
		require.NoError(t, err)
		gate := api.NewOperationGate()
		deps.Gate = gate
		service, err := processing.NewService(processing.ServiceConfig{
			Catalog: deps.Store, Blobs: deps.Blobs, Gate: gate,
			SpoolDirectory: filepath.Join(deps.VaultRoot, "blobs", "tmp"),
			Profiles: map[string]processing.ProfileConfig{"private": {
				Profile: processingTestProfile(provider.Descriptor()), RenditionProvider: provider,
			}},
		})
		require.NoError(t, err)
		deps.Processing = service
	}
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

	slept bool
}

func (w *slowRenditionWriter) Write(p []byte) (int, error) {
	n, err := w.ResponseRecorder.Write(p)
	if err != nil {
		return n, fmt.Errorf("recording slow rendition: %w", err)
	}
	if !w.slept {
		w.slept = true
		time.Sleep(61 * time.Second)
	}
	return n, nil
}
func TestRenditionDownloadOutlivesRequestTimeout(t *testing.T) {
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
		writer := &slowRenditionWriter{ResponseRecorder: httptest.NewRecorder()}
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

func TestProcessingReportsConsentExpiredDuringRendition(t *testing.T) {
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
	c := client.New(ts.URL, testAPIKey)
	selector := api.ProcessingSelector{NodeID: node.ID, ContentVersionID: node.CurrentVersionID, Profile: "private"}
	plan, err := c.PlanProcessing(t.Context(), api.ProcessingPlanRequest{Selector: selector})
	require.NoError(t, err)
	grant := api.ProcessingConsentGrantRequest{Selector: selector, PlanFingerprint: plan.Fingerprint}
	expires := time.Now().UTC().Add(2 * time.Second)
	grant.ExpiresAt = expires.Format(time.RFC3339Nano)
	provider.afterRender = func() { time.Sleep(time.Until(expires) + time.Millisecond) }

	_, err = c.GrantProcessingConsent(t.Context(), grant)
	require.NoError(t, err)
	for range 2 {
		_, err = c.StartProcessing(t.Context(), api.StartProcessingRequest{Selector: selector,
			PlanFingerprint: plan.Fingerprint})
		require.ErrorIs(t, err, client.ErrProcessingConsent)
		code, ok := client.ProblemCode(err)
		require.True(t, ok)
		require.Equal(t, "processing_consent_expired", code)
	}
}

func TestDerivativePurgeReturnsCommittedReceiptWhenCleanupFails(t *testing.T) {
	ts, catalog := newTestServer(t, configureProcessingTestService(t))
	node := createFileWithContent(t, ts, catalog, "/purge-partial.txt", "synthetic partial purge evidence\n")
	c := client.New(ts.URL, testAPIKey)
	selector := api.ProcessingSelector{NodeID: node.ID, ContentVersionID: node.CurrentVersionID, Profile: "private"}
	plan, err := c.PlanProcessing(t.Context(), api.ProcessingPlanRequest{Selector: selector})
	require.NoError(t, err)
	job, err := c.StartProcessing(t.Context(), api.StartProcessingRequest{Selector: selector,
		PlanFingerprint: plan.Fingerprint, Consent: true})
	require.NoError(t, err)
	purgePlan, err := c.PlanDerivativePurge(t.Context(), api.DerivativePurgePlanRequest{AttachmentIDs: []string{job.AttachmentID}})
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
	ts, catalog := newTestServer(t, configureProcessingTestService(t))
	c := client.New(ts.URL, testAPIKey)
	first := createFileWithContent(t, ts, catalog, "/first.txt", "first synthetic source\n")
	selector := api.ProcessingSelector{NodeID: first.ID, ContentVersionID: first.CurrentVersionID, Profile: "private"}
	versionRequest := api.DerivativePurgePlanRequest{ContentVersionIDs: []string{first.CurrentVersionID}}
	before, err := c.PlanDerivativePurge(t.Context(), versionRequest)
	require.NoError(t, err)
	plan, err := c.PlanProcessing(t.Context(), api.ProcessingPlanRequest{Selector: selector})
	require.NoError(t, err)
	job, err := c.StartProcessing(t.Context(), api.StartProcessingRequest{Selector: selector,
		PlanFingerprint: plan.Fingerprint, Consent: true})
	require.NoError(t, err)
	_, err = c.RunDerivativePurge(t.Context(), api.DerivativePurgeJobRequest{
		ContentVersionIDs: versionRequest.ContentVersionIDs, PlanFingerprint: before.Fingerprint})
	require.ErrorIs(t, err, client.ErrProcessingPlanChanged)
	view, err := catalog.ActiveRenditionByAttachment(t.Context(), job.AttachmentID)
	require.NoError(t, err)
	requests := []api.DerivativePurgePlanRequest{versionRequest,
		{AttachmentIDs: []string{job.AttachmentID}}, {BuildIDs: []string{view.Build.ID}}, {All: true}}
	previews := make([]api.DerivativePurgePlan, len(requests))
	for index, request := range requests {
		previews[index], err = c.PlanDerivativePurge(t.Context(), request)
		require.NoError(t, err)
	}
	second := createFileWithContent(t, ts, catalog, "/second.txt", "second unrelated synthetic source\n")
	selector = api.ProcessingSelector{NodeID: second.ID, ContentVersionID: second.CurrentVersionID, Profile: "private"}
	plan, err = c.PlanProcessing(t.Context(), api.ProcessingPlanRequest{Selector: selector})
	require.NoError(t, err)
	_, err = c.StartProcessing(t.Context(), api.StartProcessingRequest{Selector: selector, PlanFingerprint: plan.Fingerprint})
	require.NoError(t, err)
	for index, request := range requests {
		after, err := c.PlanDerivativePurge(t.Context(), request)
		require.NoError(t, err)
		if request.All {
			assert.NotEqual(t, previews[index].Fingerprint, after.Fingerprint)
		} else {
			assert.Equal(t, previews[index].Fingerprint, after.Fingerprint)
		}
	}
}
