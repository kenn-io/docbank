package client_test

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json/v2"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
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
		case (r.Method == http.MethodGet && r.URL.Path == "/api/v1/renditions/"+attachmentID) ||
			(r.Method == http.MethodPost && r.URL.Path == "/api/v1/renditions/select"):
			if r.Method == http.MethodPost {
				var request api.RenditionSelectorRequest
				if err := json.UnmarshalRead(r.Body, &request); err != nil {
					http.Error(w, err.Error(), http.StatusBadRequest)
					return
				}
				if request.Selector != (api.ProcessingSelector{NodeID: 7, ContentVersionID: versionID, Profile: "private"}) {
					http.Error(w, "unexpected rendition selector", http.StatusBadRequest)
					return
				}
			}
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
	selectedStream, err := c.RenditionForSelector(t.Context(), selector, int64(len(renditionBody)))
	require.NoError(t, err)
	var selected strings.Builder
	_, err = selectedStream.CopyVerified(&selected)
	require.NoError(t, err)
	assert.Equal(t, renditionBody, selected.String())
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

func TestProcessingClientDeliversDurableJobBeforeBlockedTerminal(t *testing.T) {
	const jobID = "dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"
	terminalRelease := make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(terminalRelease) })
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		job := api.ProcessingJob{ID: jobID, EmbeddingJobIDs: []string{},
			ProfileFingerprint: strings.Repeat("e", 64), ContentVersionID: "11111111-1111-4111-8111-111111111111"}
		if !assert.NoError(t, json.MarshalWrite(w, api.ProcessingJobEvent{Sequence: 1, Type: "job", Job: &job})) {
			return
		}
		flusher, ok := w.(http.Flusher)
		if !assert.True(t, ok) {
			return
		}
		flusher.Flush()
		<-terminalRelease
		status := api.ProcessingStatus{JobID: jobID, State: "completed", Phase: "published", EmbeddingJobIDs: []string{}}
		assert.NoError(t, json.MarshalWrite(w, api.ProcessingJobEvent{Sequence: 2, Type: "status", Status: &status, Terminal: true}))
	}))
	t.Cleanup(server.Close)
	stream, err := client.New(server.URL, serverKey).StartProcessingStream(t.Context(), api.StartProcessingRequest{})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, stream.Close()) })

	first, err := stream.Next()
	require.NoError(t, err)
	require.NotNil(t, first.Job)
	assert.Equal(t, jobID, first.Job.ID)
	releaseOnce.Do(func() { close(terminalRelease) })
	second, err := stream.Next()
	require.NoError(t, err)
	require.NotNil(t, second.Status)
	assert.Equal(t, "completed", second.Status.State)
	_, err = stream.Next()
	require.ErrorIs(t, err, io.EOF)
}

func TestProcessingClientRejectsMalformedAndTrailingIncrementalEvents(t *testing.T) {
	const jobID = "dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"
	job := `{"sequence":1,"type":"job","job":{"id":"` + jobID + `","embedding_job_ids":[],"profile_fingerprint":"` +
		strings.Repeat("e", 64) + `","content_version_id":"11111111-1111-4111-8111-111111111111"}}` + "\n"
	for _, testCase := range []struct {
		name string
		body string
		want string
	}{
		{name: "mismatched terminal job", body: job + `{"sequence":2,"type":"status","status":{"job_id":"` +
			strings.Repeat("f", 64) + `","state":"completed","phase":"published","embedding_job_ids":[],"completed_bindings":0},"terminal":true}` + "\n",
			want: "malformed terminal status"},
		{name: "trailing event", body: job + `{"sequence":2,"type":"status","status":{"job_id":"` + jobID +
			`","state":"completed","phase":"published","embedding_job_ids":[],"completed_bindings":0},"terminal":true}` + "\n" +
			`{"sequence":3,"type":"status"}` + "\n", want: "continued after"},
		{name: "oversized stream", body: job + strings.Repeat(" ", 64<<10), want: "too large"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/x-ndjson")
				_, _ = io.WriteString(w, testCase.body)
			}))
			t.Cleanup(server.Close)
			stream, err := client.New(server.URL, serverKey).StartProcessingStream(t.Context(), api.StartProcessingRequest{})
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, stream.Close()) })
			_, err = stream.Next()
			require.NoError(t, err)
			_, err = stream.Next()
			require.ErrorContains(t, err, testCase.want)
		})
	}
}

func TestProcessingClientValidatesSearchResponseAgainstExactFence(t *testing.T) {
	const (
		vaultID        = "11111111-1111-4111-8111-111111111111"
		versionID      = "22222222-2222-4222-8222-222222222222"
		otherVersionID = "55555555-5555-4555-8555-555555555555"
	)
	request := api.DocumentSearchRequest{Query: "visible", Mode: "semantic", Limit: 20,
		Profile: "private", Fence: api.DocumentSourceFence{VaultUID: vaultID,
			ContentVersionIDs: []string{versionID, otherVersionID}}}
	validReport := func() api.DocumentSearchReport {
		return api.DocumentSearchReport{RequestedMode: "semantic", ActualMode: "semantic",
			Coverage:     api.DocumentSearchCoverage{ScopedDocuments: 1, CompleteDocuments: 1, State: "complete"},
			Degradations: []string{}, Trace: []api.DocumentSearchTrace{}, Results: []api.DocumentSearchResult{{
				VaultUID: vaultID, NodeID: 7, ContentVersionID: versionID, Rank: 1, SemanticRank: 1,
				Score: 0.9, Path: "/visible.pdf", Evidence: []api.DocumentEvidenceReference{{
					Kind: "embedding", VectorSpaceID: strings.Repeat("a", 64),
					EmbeddingSetID: strings.Repeat("b", 64), InputGenerationID: strings.Repeat("c", 64),
					InputID: "chunk-000000-aaaaaaaaaaaa", InputKind: "rendition_chunk",
					SourceManifestChecksum: strings.Repeat("d", 64),
				}},
			}}}
	}
	tests := []struct {
		name   string
		mutate func(*api.DocumentSearchReport)
	}{
		{name: "foreign vault", mutate: func(report *api.DocumentSearchReport) {
			report.Results[0].VaultUID = "33333333-3333-4333-8333-333333333333"
		}},
		{name: "outside version", mutate: func(report *api.DocumentSearchReport) {
			report.Results[0].ContentVersionID = "44444444-4444-4444-8444-444444444444"
		}},
		{name: "duplicate result", mutate: func(report *api.DocumentSearchReport) {
			duplicate := report.Results[0]
			duplicate.Rank = 2
			duplicate.SemanticRank = 2
			report.Results = append(report.Results, duplicate)
		}},
		{name: "noncanonical rank", mutate: func(report *api.DocumentSearchReport) {
			report.Results[0].Rank = 2
		}},
		{name: "unbounded lane rank", mutate: func(report *api.DocumentSearchReport) {
			report.Results[0].SemanticRank = 1001
		}},
		{name: "duplicate lane rank", mutate: func(report *api.DocumentSearchReport) {
			duplicate := report.Results[0]
			duplicate.NodeID, duplicate.ContentVersionID, duplicate.Rank = 8, otherVersionID, 2
			duplicate.Evidence = append([]api.DocumentEvidenceReference(nil), duplicate.Evidence...)
			duplicate.Evidence[0].EmbeddingSetID = strings.Repeat("e", 64)
			report.Results = append(report.Results, duplicate)
		}},
		{name: "duplicate evidence", mutate: func(report *api.DocumentSearchReport) {
			report.Results[0].Evidence = append(report.Results[0].Evidence, report.Results[0].Evidence[0])
		}},
		{name: "unbounded evidence identity", mutate: func(report *api.DocumentSearchReport) {
			report.RequestedMode, report.ActualMode = "lexical", "lexical"
			report.Results[0].SemanticRank, report.Results[0].LexicalRank = 0, 1
			report.Results[0].Evidence = []api.DocumentEvidenceReference{{
				Kind: "rendition_segment", BuildID: strings.Repeat("e", 64), SegmentID: strings.Repeat("x", 1025),
			}}
		}},
		{name: "inconsistent evidence identity", mutate: func(report *api.DocumentSearchReport) {
			report.Results[0].Evidence[0].EmbeddingSetID = ""
		}},
		{name: "relative path", mutate: func(report *api.DocumentSearchReport) {
			report.Results[0].Path = "visible.pdf"
		}},
		{name: "oversized path", mutate: func(report *api.DocumentSearchReport) {
			report.Results[0].Path = "/" + strings.Repeat("p", (16<<10)+1)
		}},
		{name: "oversized excerpt", mutate: func(report *api.DocumentSearchReport) {
			report.Results[0].Excerpt = strings.Repeat("e", 513)
		}},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			report := validReport()
			testCase.mutate(&report)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				assert.NoError(t, json.MarshalWrite(w, report))
			}))
			t.Cleanup(server.Close)
			result, err := client.New(server.URL, serverKey).SearchDocuments(t.Context(), request)
			require.ErrorContains(t, err, "search response")
			assert.Empty(t, result.Results)
		})
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		assert.NoError(t, json.MarshalWrite(w, validReport()))
	}))
	t.Cleanup(server.Close)
	report, err := client.New(server.URL, serverKey).SearchDocuments(t.Context(), request)
	require.NoError(t, err)
	require.Len(t, report.Results, 1)
	assert.Equal(t, versionID, report.Results[0].ContentVersionID)
}

func TestProcessingClientValidatesSourceFenceRequestBeforeSending(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{}`)
	}))
	t.Cleanup(server.Close)
	c := client.New(server.URL, serverKey)
	empty := &api.DocumentSourceFenceFilters{}
	ids := make([]string, 4097)
	for index := range ids {
		ids[index] = fmt.Sprintf("00000000-0000-4000-8000-%012d", index)
	}
	for _, request := range []api.DocumentSourceFenceResolveRequest{
		{},
		{ContentVersionIDs: []string{"11111111-1111-4111-8111-111111111111"}, Filters: empty},
		{ContentVersionIDs: []string{"bad"}},
		{ContentVersionIDs: []string{
			"11111111-1111-4111-8111-111111111111", "11111111-1111-4111-8111-111111111111",
		}},
		{ContentVersionIDs: ids},
	} {
		_, err := c.ResolveDocumentSourceFence(t.Context(), request)
		require.ErrorContains(t, err, "source fence request")
	}
	assert.Zero(t, requests, "invalid typed requests must not reach the daemon")
}

func TestProcessingClientValidatesSourceFenceResponseAuthority(t *testing.T) {
	const (
		vaultID  = "11111111-1111-4111-8111-111111111111"
		firstID  = "22222222-2222-4222-8222-222222222222"
		secondID = "33333333-3333-4333-8333-333333333333"
	)
	valid := api.DocumentSourceFenceResolution{
		Fence:              api.ResolvedDocumentSourceFence{VaultUID: vaultID, ContentVersionIDs: []string{firstID, secondID}},
		FenceFingerprint:   "sha256:3c2a6756783fd03230bb89fe15de79ba10b3a6c1511d56be4042994e66d707cc",
		ObservedScopeCount: 2,
	}
	tests := []struct {
		name   string
		mutate func(*api.DocumentSourceFenceResolution)
	}{
		{name: "invalid vault", mutate: func(result *api.DocumentSourceFenceResolution) {
			result.Fence.VaultUID = "bad"
		}},
		{name: "unsorted IDs", mutate: func(result *api.DocumentSourceFenceResolution) {
			result.Fence.ContentVersionIDs[0], result.Fence.ContentVersionIDs[1] =
				result.Fence.ContentVersionIDs[1], result.Fence.ContentVersionIDs[0]
		}},
		{name: "duplicate ID", mutate: func(result *api.DocumentSourceFenceResolution) {
			result.Fence.ContentVersionIDs[1] = result.Fence.ContentVersionIDs[0]
		}},
		{name: "wrong fingerprint", mutate: func(result *api.DocumentSourceFenceResolution) {
			result.FenceFingerprint = "sha256:" + strings.Repeat("f", 64)
		}},
		{name: "wrong observed count", mutate: func(result *api.DocumentSourceFenceResolution) {
			result.ObservedScopeCount = 3
		}},
		{name: "oversized fence", mutate: func(result *api.DocumentSourceFenceResolution) {
			result.Fence.ContentVersionIDs = make([]string, 4097)
			for index := range result.Fence.ContentVersionIDs {
				result.Fence.ContentVersionIDs[index] = fmt.Sprintf(
					"00000000-0000-4000-8000-%012d", index)
			}
			result.ObservedScopeCount = len(result.Fence.ContentVersionIDs)
		}},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			response := valid
			response.Fence.ContentVersionIDs = append([]string(nil), valid.Fence.ContentVersionIDs...)
			testCase.mutate(&response)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				assert.NoError(t, json.MarshalWrite(w, response))
			}))
			t.Cleanup(server.Close)
			result, err := client.New(server.URL, serverKey).ResolveDocumentSourceFence(t.Context(),
				api.DocumentSourceFenceResolveRequest{Filters: &api.DocumentSourceFenceFilters{}})
			require.ErrorContains(t, err, "source fence response")
			assert.Empty(t, result.Fence.ContentVersionIDs)
		})
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		assert.NoError(t, json.MarshalWrite(w, valid))
	}))
	t.Cleanup(server.Close)
	result, err := client.New(server.URL, serverKey).ResolveDocumentSourceFence(t.Context(),
		api.DocumentSourceFenceResolveRequest{Filters: &api.DocumentSourceFenceFilters{}})
	require.NoError(t, err)
	assert.Equal(t, valid, result)
}

func TestProcessingClientBindsExplicitFenceResponseToRequestedIDs(t *testing.T) {
	const (
		vaultID  = "11111111-1111-4111-8111-111111111111"
		firstID  = "22222222-2222-4222-8222-222222222222"
		secondID = "33333333-3333-4333-8333-333333333333"
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		assert.NoError(t, json.MarshalWrite(w, api.DocumentSourceFenceResolution{
			Fence:              api.ResolvedDocumentSourceFence{VaultUID: vaultID, ContentVersionIDs: []string{firstID}},
			FenceFingerprint:   "sha256:e0fab7ab0d999b45c0686583a338626c6cb791a4bc3261b3148a72630baaa1f6",
			ObservedScopeCount: 1,
		}))
	}))
	t.Cleanup(server.Close)
	_, err := client.New(server.URL, serverKey).ResolveDocumentSourceFence(t.Context(),
		api.DocumentSourceFenceResolveRequest{ContentVersionIDs: []string{secondID, firstID}})
	require.ErrorContains(t, err, "explicit source authority changed")
}

func TestProcessingClientSortsClonedExplicitFenceIDsBeforeTransmission(t *testing.T) {
	const (
		vaultID  = "11111111-1111-4111-8111-111111111111"
		firstID  = "22222222-2222-4222-8222-222222222222"
		secondID = "33333333-3333-4333-8333-333333333333"
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		var received api.DocumentSourceFenceResolveRequest
		if !assert.NoError(t, json.UnmarshalRead(request.Body, &received, json.RejectUnknownMembers(true))) {
			http.Error(w, "invalid synthetic request", http.StatusBadRequest)
			return
		}
		assert.Equal(t, []string{firstID, secondID}, received.ContentVersionIDs)
		w.Header().Set("Content-Type", "application/json")
		assert.NoError(t, json.MarshalWrite(w, api.DocumentSourceFenceResolution{
			Fence: api.ResolvedDocumentSourceFence{VaultUID: vaultID,
				ContentVersionIDs: []string{firstID, secondID}},
			FenceFingerprint:   "sha256:3c2a6756783fd03230bb89fe15de79ba10b3a6c1511d56be4042994e66d707cc",
			ObservedScopeCount: 2,
		}))
	}))
	t.Cleanup(server.Close)
	ids := []string{secondID, firstID}
	_, err := client.New(server.URL, serverKey).ResolveDocumentSourceFence(t.Context(),
		api.DocumentSourceFenceResolveRequest{ContentVersionIDs: ids})
	require.NoError(t, err)
	assert.Equal(t, []string{secondID, firstID}, ids, "client must not mutate caller-owned request slices")
}

func TestProcessingClientAcceptsEmptyFenceAndRejectsNullIDs(t *testing.T) {
	const vaultID = "11111111-1111-4111-8111-111111111111"
	for _, testCase := range []struct {
		name    string
		idsJSON string
		wantErr string
	}{
		{name: "empty array", idsJSON: `[]`},
		{name: "null array", idsJSON: `null`, wantErr: "non-null"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"fence":{"vault_uid":"`+vaultID+`","content_version_ids":`+
					testCase.idsJSON+`},"fence_fingerprint":"sha256:460b958d02d96944be00a74a720c0b8af0248239d91c4351141b65d4b9551700","observed_scope_count":0}`)
			}))
			t.Cleanup(server.Close)
			resolved, err := client.New(server.URL, serverKey).ResolveDocumentSourceFence(t.Context(),
				api.DocumentSourceFenceResolveRequest{Filters: &api.DocumentSourceFenceFilters{}})
			if testCase.wantErr != "" {
				require.ErrorContains(t, err, testCase.wantErr)
				return
			}
			require.NoError(t, err)
			assert.NotNil(t, resolved.Fence.ContentVersionIDs)
			assert.Empty(t, resolved.Fence.ContentVersionIDs)
		})
	}
}

func TestProcessingClientPreservesTypedFilteredScopeOverflow(t *testing.T) {
	for _, observed := range []int{4097, 4096, 0} {
		t.Run(strconv.Itoa(observed), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/problem+json")
				w.WriteHeader(http.StatusUnprocessableEntity)
				assert.NoError(t, json.MarshalWrite(w, api.Error{Title: "Unprocessable Entity",
					Status: http.StatusUnprocessableEntity, Code: "scope_too_large",
					Detail:             "source scope exceeds 4096 current live content versions; narrow the source scope",
					ObservedScopeCount: observed}))
			}))
			t.Cleanup(server.Close)
			_, err := client.New(server.URL, serverKey).ResolveDocumentSourceFence(t.Context(),
				api.DocumentSourceFenceResolveRequest{Filters: &api.DocumentSourceFenceFilters{}})
			require.Error(t, err)
			code, ok := client.ProblemCode(err)
			assert.True(t, ok)
			assert.Equal(t, "scope_too_large", code)
			var overflow *client.SourceFenceScopeTooLargeError
			if observed > 4096 {
				require.ErrorAs(t, err, &overflow)
				assert.Equal(t, observed, overflow.ObservedScopeCount)
			} else {
				assert.NotErrorAs(t, err, &overflow, "non-overflow counts must not become typed overflow")
			}
		})
	}
}

func TestProcessingClientNormalizesSourceFenceFiltersOnTheWire(t *testing.T) {
	const (
		vaultID   = "11111111-1111-4111-8111-111111111111"
		versionID = "22222222-2222-4222-8222-222222222222"
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		var received api.DocumentSourceFenceResolveRequest
		if !assert.NoError(t, json.UnmarshalRead(request.Body, &received, json.RejectUnknownMembers(true))) ||
			!assert.NotNil(t, received.Filters) {
			http.Error(w, "invalid synthetic request", http.StatusBadRequest)
			return
		}
		assert.Equal(t, "text/plain", received.Filters.MIMEType)
		assert.Equal(t, "2026-08-28T10:00:00.000000000Z", received.Filters.ModifiedSince)
		assert.Equal(t, "2026-08-28T11:00:00.000000000Z", received.Filters.ModifiedBefore)
		w.Header().Set("Content-Type", "application/json")
		assert.NoError(t, json.MarshalWrite(w, api.DocumentSourceFenceResolution{
			Fence:              api.ResolvedDocumentSourceFence{VaultUID: vaultID, ContentVersionIDs: []string{versionID}},
			FenceFingerprint:   "sha256:e0fab7ab0d999b45c0686583a338626c6cb791a4bc3261b3148a72630baaa1f6",
			ObservedScopeCount: 1,
		}))
	}))
	t.Cleanup(server.Close)
	_, err := client.New(server.URL, serverKey).ResolveDocumentSourceFence(t.Context(),
		api.DocumentSourceFenceResolveRequest{Filters: &api.DocumentSourceFenceFilters{
			MIMEType: "TEXT/PLAIN", ModifiedSince: "2026-08-28T12:00:00+02:00",
			ModifiedBefore: "2026-08-28T13:00:00+02:00",
		}})
	require.NoError(t, err)
}

func TestProcessingClientRejectsResultRevokedFromConsumerFence(t *testing.T) {
	const vaultID = "11111111-1111-4111-8111-111111111111"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		assert.NoError(t, json.MarshalWrite(w, api.DocumentSearchReport{
			RequestedMode: "lexical", ActualMode: "lexical", Degradations: []string{}, Trace: []api.DocumentSearchTrace{},
			Coverage: api.DocumentSearchCoverage{State: "unknown"}, Results: []api.DocumentSearchResult{{
				VaultUID: vaultID, NodeID: 7, ContentVersionID: "33333333-3333-4333-8333-333333333333",
				Rank: 1, LexicalRank: 1, Score: 1, Path: "/revoked.pdf",
				Evidence: []api.DocumentEvidenceReference{{Kind: "node_name"}},
			}},
		}))
	}))
	t.Cleanup(server.Close)
	report, err := client.New(server.URL, serverKey).SearchDocuments(t.Context(), api.DocumentSearchRequest{
		Query: "revoked", Mode: "lexical", Limit: 20, Profile: "private",
		Fence: api.DocumentSourceFence{VaultUID: vaultID,
			ContentVersionIDs: []string{"22222222-2222-4222-8222-222222222222"}},
	})
	require.ErrorContains(t, err, "search response")
	assert.Empty(t, report.Results)
}
