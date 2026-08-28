package api_test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

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
