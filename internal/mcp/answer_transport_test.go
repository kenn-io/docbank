package mcp

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/config"
	"go.kenn.io/docbank/internal/processing"
	"go.kenn.io/docbank/internal/retrieval"
	"go.kenn.io/docbank/internal/store"
)

type questionOutageProvider struct{ descriptor document.EmbeddingDescriptor }

func (p questionOutageProvider) Descriptor() document.EmbeddingDescriptor { return p.descriptor }

func (questionOutageProvider) Embed(_ context.Context, inputs []document.EmbeddingInput, _ document.EmbeddingAuthorization) (document.EmbeddingResult, error) {
	if inputs[0].Role == document.EmbeddingRoleQuery {
		return document.EmbeddingResult{}, errors.New("query provider returned 503")
	}
	vectors := make([]document.EmbeddingVector, len(inputs))
	for i, input := range inputs {
		vectors[i] = document.EmbeddingVector{Key: input.Key, Values: []float32{1, 0}}
	}
	return document.EmbeddingResult{Vectors: vectors}, nil
}

func TestAnswerQuestionQueryProviderOutageKeepsLexicalEvidence(t *testing.T) {
	f := newNativeExportFixture(t)
	written, err := f.blobs.WriteDetailedContext(t.Context(), strings.NewReader("The observatory opens at noon."))
	require.NoError(t, err)
	encoding, err := written.EncodingName()
	require.NoError(t, err)
	node, err := f.catalog.CreateFile(t.Context(), f.catalog.RootID(), "notes.txt", written.Hash, written.Size, "text/plain",
		store.BlobPhysical{Encoding: encoding, StoredBytes: written.StoredSize, PackEligible: written.PackEligible, MD5: written.MD5, Created: written.Created})
	require.NoError(t, err)
	require.NoError(t, f.catalog.RecordExtraction(t.Context(), store.ExtractionResult{
		BlobHash: written.Hash, Extractor: "plain-text", ExtractorVersion: 1,
		Status: store.ExtractionOK, Text: "The observatory opens at noon.",
	}))
	contract, err := document.NewModelInputContract(document.ModelInputContractConfig{Profile: document.ModelInputProfileNomic})
	require.NoError(t, err)
	descriptor, err := document.NewEmbeddingDescriptor(document.EmbeddingDescriptor{
		ID: "synthetic.embedding-v1", ContractVersion: document.EmbeddingProviderContractVersion,
		PolicyFingerprint: strings.Repeat("a", 64), TrustBoundary: document.EmbeddingTrustLocalProcess,
		Model: "synthetic-model", ModelRevision: "v1", Dimension: 2,
		Metric: document.VectorMetricCosine, Normalization: document.VectorNormalizationNone,
		ScalarEncoding: "float32", DocumentFormatter: "synthetic/document-v1", QueryFormatter: "synthetic/query-v1",
		InputKinds:      []document.EmbeddingInputKind{document.EmbeddingInputOriginalFile},
		CompatibilityID: contract.CompatibilityID, SupportsTextQuery: true, ModelInput: contract,
		SupportedRequestModes: []document.ModelInputMode{contract.Document.Mode},
	})
	require.NoError(t, err)
	name, profile, err := processing.NewSuppliedMediaProfile(f.catalog, f.blobs, "test:operator")
	require.NoError(t, err)
	profile.Profile.Rendition = nil
	profile.RenditionProvider = nil
	profile.Profile.RetentionDisclosure.RetainSanitizedMarkdown = false
	profile.Profile.Embeddings = []document.EmbeddingBindingV1{{
		Name: "query", Activation: document.EmbeddingOptional, AuthorizationFingerprint: strings.Repeat("b", 64),
		CredentialBinding: "credential:synthetic", DisclosureFingerprint: strings.Repeat("c", 64),
		Descriptor: document.ProviderDescriptorV1{ID: descriptor.ID, Fingerprint: descriptor.Fingerprint},
		Dimensions: descriptor.Dimension, CompatibilityID: descriptor.CompatibilityID,
		DocumentFormatter: descriptor.DocumentFormatter, QueryFormatter: descriptor.QueryFormatter,
		InputKind: document.EmbeddingInputOriginalFile, Model: descriptor.Model, ModelInput: descriptor.ModelInput,
		Metric: descriptor.Metric, Normalization: descriptor.Normalization, ScalarEncoding: descriptor.ScalarEncoding,
		TrustBoundary: string(descriptor.TrustBoundary), MaxBatchItems: 8, MaxInputBytes: 1 << 20, MaxResponseBytes: 1 << 20,
	}}
	profile.EmbeddingProviders = map[string]document.EmbeddingProvider{"query": questionOutageProvider{descriptor}}
	service, err := processing.NewService(processing.ServiceConfig{Catalog: f.catalog, Blobs: f.blobs, Gate: api.NewOperationGate(),
		SpoolDirectory: t.TempDir(), Profiles: map[string]processing.ProfileConfig{name: profile}})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, service.Shutdown(context.Background())) })
	selector := processing.Selector{NodeID: node.ID, ContentVersionID: node.CurrentVersionID, Profile: name}
	plan, err := service.Plan(t.Context(), selector)
	require.NoError(t, err)
	_, err = service.Start(t.Context(), processing.StartRequest{Selector: selector, PlanFingerprint: plan.Fingerprint, Consent: true})
	require.NoError(t, err)
	cfg := config.Default()
	cfg.Server.APIKey = "synthetic-export-key"
	apiServer := api.NewServer(api.Deps{Store: f.catalog, Blobs: f.blobs, Processing: service, Cfg: cfg, VaultRoot: os.Getenv("DOCBANK_HOME")})
	t.Cleanup(apiServer.Close)
	httpServer := httptest.NewServer(apiServer.Handler())
	t.Cleanup(httpServer.Close)
	server := newServerWithOptionsAndDaemon(testImplementation(), ServerOptions{}, exportTestLease(t, httpServer.URL))
	output := exportCall(t, server, "answer_question", map[string]any{"question": "observatory"})
	require.Equal(t, "ANSWERED", output["status"])
	passages, ok := output["passages"].([]any)
	require.True(t, ok)
	require.Len(t, passages, 1)
	require.Equal(t, node.CurrentVersionID, schemaMap(t, passages[0])["content_version_id"])
	require.Contains(t, schemaMap(t, passages[0])["text"], "observatory")
	require.Contains(t, output["coverage_reasons"], "semantic_unavailable")
	require.Equal(t, "processing_failed", output["semantic_failure"])
	require.Equal(t, "lexical", schemaMap(t, output["receipt"])["actual_mode"])
}

type consentDeniedReranker struct{ t *testing.T }

func (p consentDeniedReranker) Rerank(context.Context, retrieval.RerankingRequest) ([]retrieval.RerankScore, error) {
	p.t.Error("revoked consent reached the reranking provider")
	return nil, nil
}

func TestAnswerQuestionRerankRevokedConsent(t *testing.T) {
	f := newNativeExportFixture(t)
	member := f.addFile(t, "notes.txt", "The observatory opens at noon.")
	require.NoError(t, f.catalog.RecordExtraction(t.Context(), store.ExtractionResult{
		BlobHash: member.SHA256, Extractor: "plain-text", ExtractorVersion: 1,
		Status: store.ExtractionOK, Text: "The observatory opens at noon.",
	}))
	name, profile, err := processing.NewSuppliedMediaProfile(f.catalog, f.blobs, "test:operator")
	require.NoError(t, err)
	profile.RerankingProvider = consentDeniedReranker{t}
	profile.RerankingProfile = retrieval.RerankingProfile{ID: "synthetic-reranker", MaxCandidates: 2, MaxExcerptBytes: 4096}
	profile.RerankingDisclosure = processing.RuntimeDisclosure{
		ImmediateProcessor: "synthetic-reranker", UltimateProcessor: "synthetic-reranker",
		Endpoint: "https://reranker.example.invalid", Deployment: strings.Repeat("a", 64),
		Model: "synthetic-reranker", ModelRevision: "v1",
		MetadataClasses: []string{"query_text", "candidate_excerpt", "candidate_identity", "evidence_reference"},
	}
	profile.RerankingDeadline = time.Second
	profile.RerankingFailurePolicy = retrieval.ProviderFailureDegrade
	service, err := processing.NewService(processing.ServiceConfig{Catalog: f.catalog, Blobs: f.blobs, Gate: api.NewOperationGate(),
		SpoolDirectory: filepath.Join(t.TempDir(), "spool"), Profiles: map[string]processing.ProfileConfig{name: profile}})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, service.Shutdown(context.Background())) })
	selector := processing.Selector{NodeID: member.NodeID, ContentVersionID: member.VersionID, Profile: name}
	plan, err := service.Plan(t.Context(), selector)
	require.NoError(t, err)
	_, err = service.GrantConsent(t.Context(), processing.ConsentGrantRequest{Selector: selector, PlanFingerprint: plan.Fingerprint})
	require.NoError(t, err)
	_, err = service.RevokeConsent(t.Context())
	require.NoError(t, err)
	cfg := config.Default()
	cfg.Server.APIKey = "synthetic-export-key"
	apiServer := api.NewServer(api.Deps{Store: f.catalog, Blobs: f.blobs, Processing: service, Cfg: cfg, VaultRoot: os.Getenv("DOCBANK_HOME")})
	t.Cleanup(apiServer.Close)
	httpServer := httptest.NewServer(apiServer.Handler())
	t.Cleanup(httpServer.Close)
	server := newServerWithOptionsAndDaemon(testImplementation(), ServerOptions{}, exportTestLease(t, httpServer.URL))
	output := exportCall(t, server, "answer_question", map[string]any{"question": "observatory", "rerank": true})
	reranking := schemaMap(t, schemaMap(t, output["receipt"])["reranking"])
	require.Equal(t, "degraded", reranking["outcome"])
	require.Equal(t, "authorization_denied", reranking["cause"])
}

func TestAnswerQuestionScopeLimitTransport(t *testing.T) {
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = fmt.Fprint(w, `{"status":422,"code":"scope_too_large","observed_scope_count":4097}`)
	}))
	defer daemon.Close()
	server := newServerWithOptionsAndDaemon(testImplementation(), ServerOptions{}, exportTestLease(t, daemon.URL))
	output := exportCall(t, server, "answer_question", map[string]any{"question": "example"})
	require.Equal(t, "INCOMPLETE_COVERAGE", output["status"])
	receipt := schemaMap(t, output["receipt"])
	require.EqualValues(t, 4097, receipt["observed_scope_count"])
}

func TestAnswerQuestionProcessingErrorTransport(t *testing.T) {
	for _, tc := range []struct {
		name, code, profile, binding string
		local                        bool
	}{
		{name: "daemon unavailable", code: "processing_unavailable"},
		{name: "lexical retry processing failed", code: "processing_failed"},
		{name: "lexical retry reranking failed", code: "reranking_failed"},
		{name: "reranking execution failed", code: "reranking_failed"},
		{name: "no profiles", code: "processing_unavailable", local: true},
		{name: "missing profile", code: "processing_profile_unavailable", profile: "missing"},
		{name: "missing binding", code: "processing_profile_unavailable", profile: "chosen", binding: "missing"},
		{name: "no reranking support", code: "reranking_unavailable", local: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fence := api.ResolvedDocumentSourceFence{VaultUID: testVaultID, ContentVersionIDs: []string{testVersionID}}
			fingerprint, err := processing.SourceFenceFingerprint(processing.SourceFence{VaultUID: fence.VaultUID, ContentVersionIDs: fence.ContentVersionIDs})
			require.NoError(t, err)
			searchCalls := 0
			daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/api/v1/processing/source-fences/resolve":
					writeDaemonJSON(t, w, api.DocumentSourceFenceResolution{Fence: fence, FenceFingerprint: fingerprint, ObservedScopeCount: len(fence.ContentVersionIDs)})
				case "/api/v1/processing/profiles":
					profiles := []api.ProcessingProfileSummary{{Name: "chosen", Fingerprint: strings.Repeat("a", 64), QueryEmbeddingBindings: []string{"query"}, RerankingAvailable: tc.name != "no reranking support"}}
					if tc.local && tc.code == "processing_unavailable" {
						profiles = []api.ProcessingProfileSummary{}
					}
					writeDaemonJSON(t, w, profiles)
				case "/api/v1/search", "/api/v1/search/validate":
					searchCalls++
					var request api.DocumentSearchRequest
					if err := json.UnmarshalRead(r.Body, &request); err != nil {
						t.Error(err)
						return
					}
					lexicalRetry := strings.HasPrefix(tc.name, "lexical retry")
					if lexicalRetry && searchCalls == 2 {
						assert.Equal(t, "lexical", request.Mode)
					}
					if tc.profile != "" {
						assert.Equal(t, tc.profile, request.Profile)
					}
					if tc.binding != "" {
						assert.Equal(t, tc.binding, request.BindingID)
					}
					code := tc.code
					if lexicalRetry && searchCalls == 1 {
						code = "processing_failed"
					}
					w.Header().Set("Content-Type", "application/problem+json")
					status := http.StatusServiceUnavailable
					if code == "processing_failed" {
						status = http.StatusInternalServerError
					}
					if code == "reranking_failed" {
						status = http.StatusBadGateway
					}
					w.WriteHeader(status)
					_, _ = fmt.Fprintf(w, `{"status":%d,"code":%q,"detail":"private provider details"}`, status, code)
				default:
					t.Errorf("unexpected request: %s", r.URL.Path)
					http.NotFound(w, r)
				}
			}))
			defer daemon.Close()
			server := newServerWithOptionsAndDaemon(testImplementation(), ServerOptions{}, exportTestLease(t, daemon.URL))
			args := map[string]any{"question": "example", "rerank": true}
			if tc.profile != "" {
				args["profile"] = tc.profile
			}
			if tc.binding != "" {
				args["binding_id"] = tc.binding
			}
			raw := exchangeRaw(t, server, requestFor("tools/call", map[string]any{"name": "answer_question", "arguments": args}))
			require.NotContains(t, string(raw), "private provider details")
			if tc.code == "processing_failed" {
				require.EqualValues(t, jsonrpc.CodeInternalError, decodeWireError(t, raw).Code)
			} else {
				result := decodeResult(t, raw)
				require.Equal(t, true, result["isError"])
				require.Equal(t, tc.code, objectField(t, result, "structuredContent")["code"])
			}
			wantCalls := 1
			if tc.local {
				wantCalls = 0
			}
			if strings.HasPrefix(tc.name, "lexical retry") {
				wantCalls = 2
			}
			require.Equal(t, wantCalls, searchCalls)
		})
	}
}

func TestAnswerQuestionSearchSelectionTransport(t *testing.T) {
	for _, tc := range []struct {
		name, profile, binding                string
		rerank, exhaust, noBindings           bool
		failures                              map[string]string
		wantAttempts                          []string
		wantProfile, wantBinding, wantFailure string
	}{
		{name: "retry bindings in name order", failures: map[string]string{"a": "not_found", "c": "stale_index"},
			wantAttempts: []string{"a:a:hybrid", "a:c:hybrid", "z:b:hybrid"}, wantProfile: "z", wantBinding: "b"},
		{name: "provider outage tries healthy next binding", failures: map[string]string{"a": "processing_failed"},
			wantAttempts: []string{"a:a:hybrid", "a:c:hybrid"}, wantProfile: "a", wantBinding: "c"},
		{name: "exhaustion falls back lexically", exhaust: true, failures: map[string]string{"a": "not_found", "c": "stale_index", "b": "stale_index"},
			wantAttempts: []string{"a:a:hybrid", "a:c:hybrid", "z:b:hybrid", "a::lexical"}, wantProfile: "a", wantFailure: "not_found"},
		{name: "reranking fallback uses compatible profile", rerank: true, exhaust: true, failures: map[string]string{"b": "not_found"},
			wantAttempts: []string{"z:b:hybrid", "z::lexical"}, wantProfile: "z", wantFailure: "not_found"},
		{name: "reranking without bindings searches lexically", rerank: true, noBindings: true,
			wantAttempts: []string{"z::lexical"}, wantProfile: "z"},
		{name: "explicit profile uses first binding", profile: "a",
			wantAttempts: []string{"a:a:hybrid"}, wantProfile: "a", wantBinding: "a"},
		{name: "explicit binding", profile: "a", binding: "c", failures: map[string]string{"a": "stale_index"},
			wantAttempts: []string{"a:c:hybrid"}, wantProfile: "a", wantBinding: "c"},
		{name: "explicit profile failure stops", profile: "a", failures: map[string]string{"a": "stale_index"},
			wantAttempts: []string{"a:a:hybrid"}, wantFailure: "stale_index"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			profiles := []api.ProcessingProfileSummary{
				{Name: "a", Fingerprint: strings.Repeat("a", 64), QueryEmbeddingBindings: []string{"c", "a"}},
				{Name: "z", Fingerprint: strings.Repeat("b", 64), QueryEmbeddingBindings: []string{"b"}, RerankingAvailable: true},
			}
			if tc.noBindings {
				profiles[1].QueryEmbeddingBindings = nil
			}
			fence := api.ResolvedDocumentSourceFence{VaultUID: testVaultID, ContentVersionIDs: []string{testVersionID}}
			fingerprint, err := processing.SourceFenceFingerprint(processing.SourceFence{VaultUID: fence.VaultUID, ContentVersionIDs: fence.ContentVersionIDs})
			require.NoError(t, err)
			var attempts []string
			daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/api/v1/processing/source-fences/resolve":
					writeDaemonJSON(t, w, api.DocumentSourceFenceResolution{Fence: fence, FenceFingerprint: fingerprint, ObservedScopeCount: 1})
				case "/api/v1/processing/profiles":
					writeDaemonJSON(t, w, profiles)
				case "/api/v1/search":
					var request api.DocumentSearchRequest
					if err := json.UnmarshalRead(r.Body, &request); err != nil {
						t.Error(err)
						return
					}
					attempts = append(attempts, request.Profile+":"+request.BindingID+":"+request.Mode)
					if request.Rerank != tc.rerank {
						t.Errorf("rerank = %t, want %t", request.Rerank, tc.rerank)
					}
					if code := tc.failures[request.BindingID]; code != "" {
						status := http.StatusConflict
						if code == "not_found" {
							status = http.StatusNotFound
						}
						if code == "processing_failed" {
							status = http.StatusInternalServerError
						}
						w.Header().Set("Content-Type", "application/problem+json")
						w.WriteHeader(status)
						_, _ = fmt.Fprintf(w, `{"status":%d,"code":%q}`, status, code)
						return
					}
					report := api.DocumentSearchReport{RequestedMode: request.Mode, ActualMode: request.Mode,
						Coverage: api.DocumentSearchCoverage{State: "complete", ScopedDocuments: 1, CompleteDocuments: 1}}
					if request.Rerank {
						report.Reranking = &api.DocumentSearchRerankingReceipt{Outcome: "skipped"}
					}
					writeDaemonJSON(t, w, report)
				default:
					t.Errorf("unexpected request: %s", r.URL.Path)
					http.NotFound(w, r)
				}
			}))
			defer daemon.Close()
			server := newServerWithOptionsAndDaemon(testImplementation(), ServerOptions{}, exportTestLease(t, daemon.URL))
			args := map[string]any{"question": "example", "rerank": tc.rerank}
			if tc.profile != "" {
				args["profile"] = tc.profile
			}
			if tc.binding != "" {
				args["binding_id"] = tc.binding
			}
			output := exportCall(t, server, "answer_question", args)
			require.Equal(t, tc.wantAttempts, attempts)
			if tc.wantProfile == "" {
				require.Equal(t, tc.wantFailure, output["code"])
				return
			}
			receipt := schemaMap(t, output["receipt"])
			profile := schemaMap(t, receipt["profile"])
			require.Equal(t, tc.wantProfile, profile["name"])
			if tc.profile != "" {
				require.Equal(t, strings.Repeat("a", 64), profile["fingerprint"])
			}
			if tc.exhaust {
				require.Equal(t, "INCOMPLETE_COVERAGE", output["status"])
			} else if !tc.noBindings {
				require.Equal(t, "NO_MATCH", output["status"])
			}
			if tc.exhaust || tc.noBindings {
				require.Equal(t, "lexical", receipt["actual_mode"])
			} else {
				require.Equal(t, tc.wantBinding, receipt["binding_id"])
			}
			if tc.wantFailure != "" {
				require.Equal(t, tc.wantFailure, output["semantic_failure"])
			}
		})
	}
}

func TestAnswerQuestionRejectsBlankQuestionTransport(t *testing.T) {
	fingerprint, err := processing.SourceFenceFingerprint(processing.SourceFence{VaultUID: testVaultID, ContentVersionIDs: []string{testVersionID}})
	require.NoError(t, err)
	calls := 0
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		switch r.URL.Path {
		case "/api/v1/processing/source-fences/resolve":
			writeDaemonJSON(t, w, api.DocumentSourceFenceResolution{Fence: api.ResolvedDocumentSourceFence{VaultUID: testVaultID, ContentVersionIDs: []string{testVersionID}}, FenceFingerprint: fingerprint, ObservedScopeCount: 1})
		case "/api/v1/processing/profiles":
			writeDaemonJSON(t, w, []api.ProcessingProfileSummary{{Name: "chosen", Fingerprint: strings.Repeat("a", 64), QueryEmbeddingBindings: []string{"query"}}})
		case "/api/v1/search":
			w.Header().Set("Content-Type", "application/problem+json")
			w.WriteHeader(http.StatusUnprocessableEntity)
			_, _ = fmt.Fprint(w, `{"status":422,"code":"search_query_required"}`)
		default:
			t.Errorf("unexpected request: %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer daemon.Close()
	server := newServerWithOptionsAndDaemon(testImplementation(), ServerOptions{}, exportTestLease(t, daemon.URL))
	raw := exchangeRaw(t, server, requestFor("tools/call", map[string]any{"name": "answer_question", "arguments": map[string]any{"question": " \t\n "}}))
	require.EqualValues(t, jsonrpc.CodeInvalidParams, decodeWireError(t, raw).Code)
	require.Zero(t, calls)
}

func TestAnswerQuestionRealDaemonEvidence(t *testing.T) {
	f := newNativeExportFixture(t)
	member := f.addFile(t, "notes.txt", "The observatory opens at noon.")
	require.NoError(t, f.catalog.RecordExtraction(t.Context(), store.ExtractionResult{
		BlobHash: member.SHA256, Extractor: "plain-text", ExtractorVersion: 1,
		Status: store.ExtractionOK, Text: "The observatory opens at noon.",
	}))
	server, _, _ := newProcessingDocumentServer(t, f)
	output := exportCall(t, server, "answer_question", map[string]any{"question": "observatory"})
	require.Equal(t, "ANSWERED", output["status"])
	passages, ok := output["passages"].([]any)
	require.True(t, ok)
	require.Len(t, passages, 1)
	passage := schemaMap(t, passages[0])
	require.EqualValues(t, member.NodeID, passage["node_id"])
	require.Equal(t, member.VersionID, passage["content_version_id"])
	require.Contains(t, passage["text"], "observatory")
	require.Equal(t, true, schemaMap(t, output["receipt"])["search_executed"])

	empty := exportCall(t, server, "answer_question", map[string]any{
		"question": "observatory", "filters": map[string]any{"mime_type": "application/pdf"},
	})
	require.EqualValues(t, 0, schemaMap(t, empty["receipt"])["observed_scope_count"], "MIME filter must exclude the text document")
	require.Empty(t, empty["passages"])
	require.Empty(t, empty["leads"])
	for _, filters := range []map[string]any{{}, {"mime_type": "application/pdf"}} {
		unknown := exportCall(t, server, "answer_question", map[string]any{
			"question": "observatory", "profile": "missing", "filters": filters,
		})
		require.Equal(t, "processing_profile_unavailable", unknown["code"])
	}
}
