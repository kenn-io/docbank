package client_test

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json/v2"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/client"
)

func TestProcessingClientUsesTypedRoutesAndVerifiesRenditionStream(t *testing.T) {
	const versionID = "11111111-1111-4111-8111-111111111111"
	const attachmentID = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	const buildID = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	const artifactID = "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
	const jobID = "dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"
	const profileFingerprint = "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
	body := []byte("# Needle\n\nneedle\n")
	bodyHash := sha256.Sum256(body)
	rendered, _, err := document.EnvelopeRenditionV1(document.RenditionV1{
		ContractVersion: document.RenditionContractV1, Completeness: document.EvidenceComplete,
		EvidenceChecksum: profileFingerprint, Markdown: body, MarkdownChecksum: hex.EncodeToString(bodyHash[:]),
		Units: []document.NormalizedUnitV1{{EvidenceUnitID: "section:000000", Order: 0,
			Text: string(body), Locator: document.EvidenceLocatorV1{Kind: document.EvidenceLocatorSection,
				IndexOrigin: document.EvidenceIndexOriginNone}}},
	}, document.RenditionEnvelopeV1{BuildID: buildID, SourceSHA256: artifactID,
		SourceFormat: "txt", SourceMediaType: "text/plain",
		RenditionRequestFingerprint: profileFingerprint, EvidenceLexicalFingerprint: jobID,
		NormalizedEvidenceContract: document.NormalizedEvidenceContractV1, UnitKind: document.EvidenceUnitSection})
	require.NoError(t, err)
	renditionBody := string(rendered.Markdown)
	renderedHash := sha256.Sum256([]byte(renditionBody))
	headerCompleteness := "complete"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, serverKey, r.Header.Get("X-Api-Key"))
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/processing/profiles":
			_ = json.MarshalWrite(w, []api.ProcessingProfileSummary{{Name: "private",
				Fingerprint: profileFingerprint, Rendition: true, EmbeddingBindings: []string{}}})
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/processing/plans":
			_ = json.MarshalWrite(w, api.ProcessingPlan{Fingerprint: profileFingerprint,
				VaultUID: versionID, Selector: api.ProcessingSelector{NodeID: 7,
					ContentVersionID: versionID, Profile: "private"}, ProfileFingerprint: profileFingerprint,
				Flow: []api.ProcessingFlowHop{}, DisclosedClasses: []string{}, RetainedClasses: []string{},
				ConsentRequired: true})
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/processing/consent/grants":
			_ = json.MarshalWrite(w, api.ProcessingConsentGrant{PlanFingerprint: profileFingerprint,
				ProfileFingerprint: profileFingerprint})
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/processing/consent/revocations":
			_ = json.MarshalWrite(w, api.ProcessingConsentRevocation{RevokedAt: "2026-08-28T00:00:00Z"})
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/derivatives/purge-plans":
			_ = json.MarshalWrite(w, api.DerivativePurgePlan{Fingerprint: profileFingerprint,
				VaultUID: versionID, AttachmentIDs: []string{attachmentID},
				ImmutableBackupCopiesUntouched: true})
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/derivatives/purge-jobs":
			w.Header().Set("Content-Type", "application/x-ndjson")
			receipt := api.DerivativePurgeReceipt{ID: jobID,
				PlanFingerprint: profileFingerprint, RemovedAttachments: 1,
				ImmutableBackupCopiesUntouched: true}
			_ = json.MarshalWrite(w, api.DerivativePurgeEvent{Sequence: 1, Type: "result",
				Receipt: &receipt, Terminal: true})
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/processing/jobs":
			w.Header().Set("Content-Type", "application/x-ndjson")
			job := api.ProcessingJob{ID: jobID, AttachmentID: attachmentID,
				EmbeddingJobIDs: []string{}, ProfileFingerprint: profileFingerprint, ContentVersionID: versionID}
			status := api.ProcessingStatus{JobID: jobID, State: "completed", Phase: "published",
				EmbeddingJobIDs: []string{}}
			_ = json.MarshalWrite(w, api.ProcessingJobEvent{Sequence: 1, Type: "job", Job: &job})
			_ = json.MarshalWrite(w, api.ProcessingJobEvent{Sequence: 2, Type: "status",
				Status: &status, Terminal: true})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/processing/jobs/"+jobID:
			_ = json.MarshalWrite(w, api.ProcessingStatus{JobID: jobID, State: "completed",
				Phase: "published", EmbeddingJobIDs: []string{}})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/renditions/"+attachmentID:
			w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
			w.Header().Set(api.RenditionAttachmentHeader, attachmentID)
			w.Header().Set(api.RenditionBuildHeader, buildID)
			w.Header().Set(api.RenditionArtifactHeader, artifactID)
			w.Header().Set(api.RenditionProfileHeader, profileFingerprint)
			w.Header().Set(api.RenditionCompletenessHeader, headerCompleteness)
			w.Header().Set(api.RenditionWarningsHeader, "degraded_provenance")
			w.Header().Set(api.ContentVersionHeader, versionID)
			w.Header().Set(api.BlobHashHeader, hex.EncodeToString(renderedHash[:]))
			w.Header().Set(api.BlobSizeHeader, strconv.Itoa(len(renditionBody)))
			w.Header().Set("Trailer", "Content-Digest")
			if r.Header.Get("Range") != "" {
				selected := []byte(renditionBody[:16])
				digest := sha256.Sum256(selected)
				w.Header().Set("Content-Range", "bytes 0-15/"+strconv.Itoa(len(renditionBody)))
				w.WriteHeader(http.StatusPartialContent)
				_, _ = w.Write(selected)
				w.Header().Set("Content-Digest", "sha-256=:"+base64.StdEncoding.EncodeToString(digest[:])+":")
			} else {
				w.WriteHeader(http.StatusOK)
				_, _ = io.WriteString(w, renditionBody)
				w.Header().Set("Content-Digest", "sha-256=:"+base64.StdEncoding.EncodeToString(renderedHash[:])+":")
			}
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	c := client.New(server.URL, serverKey)
	profiles, err := c.ProcessingProfiles(t.Context())
	require.NoError(t, err)
	require.Len(t, profiles, 1)
	assert.Equal(t, "private", profiles[0].Name)
	selector := api.ProcessingSelector{NodeID: 7, ContentVersionID: versionID, Profile: "private"}
	plan, err := c.PlanProcessing(t.Context(), api.ProcessingPlanRequest{Selector: selector})
	require.NoError(t, err)
	assert.Equal(t, profileFingerprint, plan.Fingerprint)
	grant, err := c.GrantProcessingConsent(t.Context(), api.ProcessingConsentGrantRequest{
		Selector: selector, PlanFingerprint: plan.Fingerprint})
	require.NoError(t, err)
	assert.Equal(t, profileFingerprint, grant.ProfileFingerprint)
	job, err := c.StartProcessing(t.Context(), api.StartProcessingRequest{Selector: selector,
		PlanFingerprint: plan.Fingerprint})
	require.NoError(t, err)
	status, err := c.ProcessingStatus(t.Context(), job.ID)
	require.NoError(t, err)
	assert.Equal(t, "completed", status.State)
	stream, err := c.Rendition(t.Context(), job.AttachmentID, 0)
	require.NoError(t, err)
	var copied strings.Builder
	written, err := stream.CopyVerified(&copied)
	require.NoError(t, err)
	assert.Equal(t, int64(len(renditionBody)), written)
	assert.Equal(t, renditionBody, copied.String())
	assert.Equal(t, buildID, stream.FrontMatter.Rendition.BuildID)
	assert.Equal(t, profileFingerprint, stream.ProfileFingerprint)
	assert.Equal(t, "complete", stream.Completeness)
	assert.Equal(t, []string{"degraded_provenance"}, stream.Warnings)
	rangeStream, err := c.RenditionRange(t.Context(), job.AttachmentID, 0, 16)
	require.NoError(t, err)
	var ranged strings.Builder
	written, err = rangeStream.CopyVerified(&ranged)
	require.NoError(t, err)
	assert.Equal(t, int64(16), written)
	assert.Equal(t, renditionBody[:16], ranged.String())
	assert.Equal(t, int64(len(renditionBody)), rangeStream.TotalSize)
	headerCompleteness = "partial"
	mismatched, err := c.Rendition(t.Context(), job.AttachmentID, 0)
	require.NoError(t, err)
	_, err = mismatched.CopyVerified(io.Discard)
	require.ErrorContains(t, err, "completeness")
	headerCompleteness = "complete"
	revocation, err := c.RevokeProcessingConsent(t.Context())
	require.NoError(t, err)
	assert.Equal(t, "2026-08-28T00:00:00Z", revocation.RevokedAt)
	purgePlan, err := c.PlanDerivativePurge(t.Context(), api.DerivativePurgePlanRequest{
		AttachmentIDs: []string{attachmentID}})
	require.NoError(t, err)
	receipt, err := c.RunDerivativePurge(t.Context(), api.DerivativePurgeJobRequest{
		AttachmentIDs: []string{attachmentID}, PlanFingerprint: purgePlan.Fingerprint})
	require.NoError(t, err)
	assert.Equal(t, 1, receipt.RemovedAttachments)
}
