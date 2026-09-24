package api_test

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/plaintext"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/apiclient"
	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/docbank/internal/processing"
	"go.kenn.io/docbank/internal/store"
)

type connectionTestTokenizer struct{}

func (connectionTestTokenizer) Identity() document.TokenizerIdentity {
	return document.TokenizerIdentity{Name: "synthetic-connection", Revision: "v1"}
}
func (connectionTestTokenizer) PrefixTokenCountsMonotonic() bool { return true }
func (connectionTestTokenizer) Tokenize(value string, limit int) ([]document.TokenBoundary, error) {
	if limit < 1 {
		return nil, document.ErrTokenizerLimit
	}
	return []document.TokenBoundary{{Start: 0, End: utf8.RuneCountInString(value)}}, nil
}

func TestConnectionSemanticRouteReturnsExactStoredPassagesWithoutEmbedding(t *testing.T) {
	provider := &similarCountingProvider{processingTestEmbeddingProvider: newProcessingTestEmbeddingProvider(t)}
	descriptor := provider.Descriptor()
	descriptor.InputKinds = []document.EmbeddingInputKind{document.EmbeddingInputRenditionChunk}
	var err error
	provider.descriptor, err = document.NewEmbeddingDescriptor(descriptor)
	require.NoError(t, err)
	renditionProvider, err := plaintext.New(plaintext.Profile{MaxDocumentBytes: 1 << 20})
	require.NoError(t, err)
	configure := func(deps *api.Deps) {
		profile := processingTestProfile(renditionProvider.Descriptor())
		d := provider.Descriptor()
		profile.Embeddings = []document.EmbeddingBindingV1{{
			Activation: document.EmbeddingOptional, AuthorizationFingerprint: processingTestHash("embedding-authorization"),
			CompatibilityID: d.CompatibilityID, CredentialBinding: "credential:test",
			Descriptor: document.ProviderDescriptorV1{ID: d.ID, Fingerprint: d.Fingerprint},
			Dimensions: d.Dimension, DisclosureFingerprint: processingTestHash("embedding-disclosure"),
			DocumentFormatter: d.DocumentFormatter, InputKind: document.EmbeddingInputRenditionChunk,
			MaxBatchItems: 8, MaxInputBytes: 1 << 20, MaxInputTokens: 128, MaxResponseBytes: 1 << 20,
			Metric: d.Metric, Model: d.Model, ModelInput: d.ModelInput, Name: "semantic",
			Normalization: d.Normalization, QueryFormatter: d.QueryFormatter,
			ScalarEncoding: d.ScalarEncoding, TrustBoundary: string(d.TrustBoundary),
			Chunk: &document.EmbeddingChunkPolicyV1{ContextFingerprint: processingTestHash("connection-context"),
				Formatter: "synthetic-connection/v1", MaxTokens: 128, OverlapTokens: 0,
				Tokenizer: "synthetic-connection", TokenizerRevision: "v1", TruncationPolicy: document.TruncationPolicyReject},
		}}
		gate := api.NewOperationGate()
		deps.Gate = gate
		deps.Processing, err = processing.NewService(processing.ServiceConfig{
			Catalog: deps.Store, Blobs: deps.Blobs, Gate: gate,
			SpoolDirectory: filepath.Join(deps.VaultRoot, "blobs", "tmp"),
			Profiles: map[string]processing.ProfileConfig{"private": {
				Profile: profile, RenditionProvider: renditionProvider,
				EmbeddingProviders: map[string]document.EmbeddingProvider{"semantic": provider},
				Tokenizers:         map[string]document.Tokenizer{"semantic": connectionTestTokenizer{}},
			}},
		})
		require.NoError(t, err)
	}
	var deps api.Deps
	ts, catalog := newTestServer(t, func(d *api.Deps) { configure(d); deps = *d })
	client := daemonconn.New(ts.URL, testAPIKey)
	var nodes []store.Node
	for _, name := range []string{"seed", "candidate", "duplicate"} {
		node := createFileWithContent(t, ts, catalog, "/"+name+".txt", "shared synthetic text")
		nodes = append(nodes, node)
		selector := api.ProcessingSelector{NodeID: node.ID, ContentVersionID: node.CurrentVersionID, Profile: "private"}
		plan, planErr := client.API().PlanDocumentProcessing(t.Context(), &apiclient.PlanDocumentProcessingRequestOptions{Body: &api.ProcessingPlanRequest{Selector: selector}})
		require.NoError(t, planErr)
		_, planErr = client.StartProcessing(t.Context(), api.StartProcessingRequest{Selector: selector,
			PlanFingerprint: plan.Fingerprint, Consent: true}, plan.ProfileFingerprint)
		require.NoError(t, planErr)
	}
	selected := nodes[0]
	rendition, err := deps.Processing.Rendition(t.Context(), processing.Selector{
		NodeID: selected.ID, ContentVersionID: selected.CurrentVersionID, Profile: "private"}, 0)
	require.NoError(t, err)
	content, err := io.ReadAll(rendition.Reader)
	require.NoError(t, err)
	require.True(t, rendition.Reader.Verified())
	require.NoError(t, rendition.Reader.Close())
	_, body, err := document.ParseRenditionFrontMatterV1(content)
	require.NoError(t, err)
	start := bytes.Index(body, []byte("shared synthetic text"))
	require.GreaterOrEqual(t, start, 0)
	identity, err := catalog.EnsureDocumentIdentity(t.Context(), selected.ID)
	require.NoError(t, err)
	version, err := catalog.ContentVersionByID(t.Context(), selected.CurrentVersionID)
	require.NoError(t, err)
	ref, err := document.NewPassageRefV1(document.PassageRefV1{VaultUID: catalog.VaultID(),
		DocumentUID: identity.DocumentUID, ContentVersionID: selected.CurrentVersionID,
		SourceSHA256: version.BlobHash, RenditionBuildID: rendition.BuildID,
		AttachmentID: rendition.AttachmentID}, body, start, start+len("shared synthetic text"))
	require.NoError(t, err)
	provider.calls.Store(0)
	report, err := client.SuggestConnections(t.Context(), api.ConnectionSuggestionRequest{
		Source: ref, Profile: "private", BindingID: "semantic", Method: "semantic",
		Fence: api.DocumentSourceFence{VaultUID: catalog.VaultID(), ContentVersionIDs: []string{
			selected.CurrentVersionID, nodes[1].CurrentVersionID, nodes[2].CurrentVersionID}},
	})
	require.NoError(t, err)
	require.Equal(t, "ready", report.State)
	require.Equal(t, 1, report.CandidateCount)
	require.Len(t, report.Candidates, 1)
	assert.Equal(t, "maximum_pair_score", report.Aggregation)
	assert.Equal(t, document.VectorMetricCosine, report.ScoreMetric)
	assert.NotEmpty(t, report.VectorSpaceID)
	assert.NotEmpty(t, report.IndexGenerationID)
	assert.NotEmpty(t, report.SourceManifestChecksum)
	assert.NotEmpty(t, report.SourceEmbeddingSetID)
	require.Len(t, report.SeedSegments, report.SeedSegmentCount)
	require.Len(t, report.SeedSegments, 1)
	assert.Equal(t, "shared synthetic text", report.SeedSegments[0].Quote)
	assert.NotEmpty(t, report.SeedSegments[0].InputID)
	require.NoError(t, document.ValidatePassageRefV1(report.SeedSegments[0].Passage, body))
	candidate := report.Candidates[0]
	assert.Equal(t, "stored_chunk_vector_match", candidate.Reason)
	assert.Equal(t, "shared synthetic text", candidate.SourceQuote)
	assert.Equal(t, "shared synthetic text", candidate.TargetQuote)
	assert.Contains(t, []string{nodes[1].CurrentVersionID, nodes[2].CurrentVersionID}, candidate.Target.ContentVersionID)
	assert.NotEmpty(t, candidate.SourceInputID)
	assert.NotEmpty(t, candidate.InputID)
	assert.NotEmpty(t, candidate.VectorSpaceID)
	require.NotNil(t, candidate.SourceSegment)
	require.NoError(t, document.ValidatePassageRefV1(*candidate.SourceSegment, body))
	assert.Equal(t, string(body[candidate.SourceSegment.ByteStart:candidate.SourceSegment.ByteEnd]),
		candidate.SourceSegmentQuote)
	require.Equal(t, 1, candidate.DuplicateCount)
	require.Len(t, candidate.DuplicateMembers, 1)
	duplicate := candidate.DuplicateMembers[0]
	assert.NotEqual(t, candidate.Target.ContentVersionID, duplicate.Target.ContentVersionID)
	assert.Contains(t, []string{nodes[1].CurrentVersionID, nodes[2].CurrentVersionID}, duplicate.Target.ContentVersionID)
	assert.Equal(t, "shared synthetic text", duplicate.TargetQuote)
	assert.NotEmpty(t, duplicate.InputID)
	assert.NotEmpty(t, duplicate.SourceInputID)
	assert.NotEmpty(t, duplicate.EmbeddingSetID)
	assert.NotEmpty(t, duplicate.InputGenerationID)
	assert.Equal(t, candidate.VectorSpaceID, duplicate.VectorSpaceID)
	assert.Equal(t, report.ScoreMetric, duplicate.ScoreMetric)
	assert.InDelta(t, candidate.Score, duplicate.Score, 0.001)

	noCandidates, err := client.SuggestConnections(t.Context(), api.ConnectionSuggestionRequest{
		Source: ref, Profile: "private", BindingID: "semantic", Method: "semantic", SeedKind: "document",
		Fence: api.DocumentSourceFence{VaultUID: catalog.VaultID(), ContentVersionIDs: []string{selected.CurrentVersionID}},
	})
	require.NoError(t, err)
	assert.Equal(t, "ready", noCandidates.State)
	assert.Zero(t, noCandidates.CandidateCount)
	assert.Equal(t, "maximum_pair_score", noCandidates.Aggregation)
	assert.Equal(t, document.VectorMetricCosine, noCandidates.ScoreMetric)
	assert.Equal(t, report.VectorSpaceID, noCandidates.VectorSpaceID)
	assert.Equal(t, report.IndexGenerationID, noCandidates.IndexGenerationID)
	assert.Equal(t, report.SourceManifestChecksum, noCandidates.SourceManifestChecksum)
	assert.Equal(t, report.SourceEmbeddingSetID, noCandidates.SourceEmbeddingSetID)
	assert.Equal(t, report.SeedSegments, noCandidates.SeedSegments)
	assert.Zero(t, provider.calls.Load(), "ordinary suggestions never call a provider")
}

func TestConnectionSuggestionsExplicitLexicalAndMissingChunkCoverageNeverEmbed(t *testing.T) {
	provider := &similarCountingProvider{processingTestEmbeddingProvider: newProcessingTestEmbeddingProvider(t)}
	configure := configureProcessingTestServiceWithEmbeddingProvider(t, provider, true)
	var deps api.Deps
	ts, catalog := newTestServer(t, func(d *api.Deps) { configure(d); deps = *d })
	client := daemonconn.New(ts.URL, testAPIKey)
	var nodes []store.Node
	for _, item := range []struct{ name, text string }{
		{"source", "shared synthetic text"}, {"copy", "shared synthetic text"},
		{"other", "unrelated synthetic text"}, {"excluded-copy", "shared synthetic text"},
	} {
		node := createFileWithContent(t, ts, catalog, "/"+item.name+".txt", item.text)
		nodes = append(nodes, node)
		selector := api.ProcessingSelector{NodeID: node.ID, ContentVersionID: node.CurrentVersionID, Profile: "private"}
		plan, err := client.API().PlanDocumentProcessing(t.Context(), &apiclient.PlanDocumentProcessingRequestOptions{Body: &api.ProcessingPlanRequest{Selector: selector}})
		require.NoError(t, err)
		_, err = client.StartProcessing(t.Context(), api.StartProcessingRequest{Selector: selector,
			PlanFingerprint: plan.Fingerprint, Consent: true}, plan.ProfileFingerprint)
		require.NoError(t, err)
	}
	source := nodes[0]
	rendition, err := deps.Processing.Rendition(t.Context(), processing.Selector{
		NodeID: source.ID, ContentVersionID: source.CurrentVersionID, Profile: "private"}, 0)
	require.NoError(t, err)
	content, err := io.ReadAll(rendition.Reader)
	require.NoError(t, err)
	require.True(t, rendition.Reader.Verified())
	require.NoError(t, rendition.Reader.Close())
	_, body, err := document.ParseRenditionFrontMatterV1(content)
	require.NoError(t, err)
	start := bytes.Index(body, []byte("shared synthetic text"))
	require.GreaterOrEqual(t, start, 0)
	identity, err := catalog.EnsureDocumentIdentity(t.Context(), source.ID)
	require.NoError(t, err)
	version, err := catalog.ContentVersionByID(t.Context(), source.CurrentVersionID)
	require.NoError(t, err)
	ref, err := document.NewPassageRefV1(document.PassageRefV1{VaultUID: catalog.VaultID(),
		DocumentUID: identity.DocumentUID, ContentVersionID: source.CurrentVersionID,
		SourceSHA256: version.BlobHash, RenditionBuildID: rendition.BuildID,
		AttachmentID: rendition.AttachmentID}, body, start, start+len("shared synthetic text"))
	require.NoError(t, err)
	request := api.ConnectionSuggestionRequest{Source: ref, Profile: "private", BindingID: "semantic",
		Method: "lexical", Fence: api.DocumentSourceFence{VaultUID: catalog.VaultID(),
			ContentVersionIDs: []string{nodes[0].CurrentVersionID, nodes[1].CurrentVersionID, nodes[2].CurrentVersionID}}}
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = api.SuggestConnections(canceled, deps, request)
	require.Error(t, err, "cancellation must stop evidence retrieval")
	request.Limit = 101
	_, err = api.SuggestConnections(t.Context(), deps, request)
	var bound *api.Error
	require.ErrorAs(t, err, &bound)
	assert.Equal(t, http.StatusUnprocessableEntity, bound.Status)
	request.Limit = 0
	provider.calls.Store(0)
	encoded, err := json.Marshal(request)
	require.NoError(t, err)
	post := func(key string) *http.Response {
		t.Helper()
		httpRequest, requestErr := http.NewRequestWithContext(t.Context(), http.MethodPost,
			ts.URL+"/api/v1/connections/suggest", bytes.NewReader(encoded))
		require.NoError(t, requestErr)
		httpRequest.Header.Set("Content-Type", "application/json")
		if key != "" {
			httpRequest.Header.Set("X-Api-Key", key)
		}
		response, requestErr := http.DefaultClient.Do(httpRequest)
		require.NoError(t, requestErr)
		t.Cleanup(func() { _ = response.Body.Close() })
		return response
	}
	unauthorized := post("")
	assert.Equal(t, http.StatusUnauthorized, unauthorized.StatusCode)
	deniedBody, err := io.ReadAll(unauthorized.Body)
	require.NoError(t, err)
	assert.NotContains(t, string(deniedBody), "shared synthetic text")
	response := post(testAPIKey)
	require.Equal(t, http.StatusOK, response.StatusCode)
	var wire api.ConnectionSuggestionReport
	require.NoError(t, json.UnmarshalRead(response.Body, &wire))
	require.Equal(t, "ready", wire.State)
	require.Len(t, wire.Candidates, 1)
	assert.Equal(t, "shared synthetic text", wire.Candidates[0].TargetQuote)
	typed, err := client.SuggestConnections(t.Context(), request)
	require.NoError(t, err)
	assert.Equal(t, wire, typed)
	assert.Zero(t, provider.calls.Load(), "HTTP and typed suggestion requests must not call a provider")
	report, err := api.SuggestConnections(t.Context(), deps, request)
	require.NoError(t, err)
	require.Equal(t, "ready", report.State)
	require.Equal(t, 1, report.CandidateCount)
	require.Len(t, report.Candidates, 1)
	assert.Equal(t, nodes[1].ID, report.Candidates[0].TargetNodeID)
	assert.Equal(t, "exact_quote_overlap", report.Candidates[0].Reason)
	assert.Equal(t, "shared synthetic text", report.Candidates[0].TargetQuote)
	assert.Equal(t, 0, int(provider.calls.Load()), "provider_calls=0")

	request.Fence.ContentVersionIDs = []string{source.CurrentVersionID, nodes[1].CurrentVersionID,
		nodes[3].CurrentVersionID}
	report, err = client.SuggestConnections(t.Context(), request)
	require.NoError(t, err)
	require.Len(t, report.Candidates, 1)
	assert.Equal(t, 1, report.Candidates[0].DuplicateCount)
	require.Len(t, report.Candidates[0].DuplicateMembers, 1)
	member := report.Candidates[0].DuplicateMembers[0]
	assert.Contains(t, []string{nodes[1].CurrentVersionID, nodes[3].CurrentVersionID}, member.Target.ContentVersionID)
	assert.NotEqual(t, report.Candidates[0].Target.ContentVersionID, member.Target.ContentVersionID)
	assert.Equal(t, "shared synthetic text", member.TargetQuote)
	assert.Equal(t, "reciprocal_lexical_rank", member.ScoreMetric)
	assert.Zero(t, provider.calls.Load())
	request.Fence.ContentVersionIDs = []string{source.CurrentVersionID, nodes[1].CurrentVersionID,
		nodes[2].CurrentVersionID}

	request.Method = "semantic"
	report, err = api.SuggestConnections(t.Context(), deps, request)
	require.NoError(t, err)
	assert.Equal(t, "unavailable", report.State)
	assert.Equal(t, "missing_seed_embedding", report.CoverageReason)
	assert.Zero(t, provider.calls.Load(), "provider_calls=0")

	request.Method = "tag"
	report, err = api.SuggestConnections(t.Context(), deps, request)
	require.NoError(t, err)
	assert.Equal(t, "requested_method_unavailable", report.CoverageReason)
	assert.Zero(t, provider.calls.Load(), "provider_calls=0")
	request.Method = "hybrid"
	report, err = api.SuggestConnections(t.Context(), deps, request)
	require.NoError(t, err)
	assert.Equal(t, "unavailable", report.State)
	assert.Equal(t, "tag_lane_unavailable", report.CoverageReason)
	assert.Empty(t, report.Candidates)
	request.Method = "lexical"
	request.SeedKind = "document"
	request.Source.DocumentUID = "99999999-9999-4999-8999-999999999999"
	_, err = api.SuggestConnections(t.Context(), deps, request)
	require.Error(t, err, "unavailable document method must still validate the source")
	request.Source = ref
	request.SeedKind = "passage"

	request.Method = "lexical"
	request.Fence.ContentVersionIDs = request.Fence.ContentVersionIDs[1:]
	_, err = api.SuggestConnections(t.Context(), deps, request)
	require.Error(t, err)
	var problem *api.Error
	require.ErrorAs(t, err, &problem)
	assert.Equal(t, http.StatusUnprocessableEntity, problem.Status)

	request.Fence.ContentVersionIDs = []string{source.CurrentVersionID, nodes[1].CurrentVersionID}
	hash, size, err := catalog.Blobs.Write(strings.NewReader("changed synthetic source"))
	require.NoError(t, err)
	_, _, err = catalog.ReplaceContent(t.Context(), source.ID, store.UnconditionalRev, hash, size, source.MimeType)
	require.NoError(t, err)
	_, err = api.SuggestConnections(t.Context(), deps, request)
	require.ErrorAs(t, err, &problem)
	assert.Equal(t, http.StatusConflict, problem.Status)
	assert.Equal(t, "stale_source", problem.Code)
	assert.Zero(t, provider.calls.Load(), "provider_calls=0")
}
