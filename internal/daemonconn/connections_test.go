package daemonconn_test

import (
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/daemonconn"
)

func TestSuggestConnectionsRejectsCandidateOutsideExactFence(t *testing.T) {
	const vault = "11111111-1111-4111-8111-111111111111"
	const sourceVersion = "22222222-2222-4222-8222-222222222222"
	const targetVersion = "33333333-3333-4333-8333-333333333333"
	const hiddenVersion = "44444444-4444-4444-8444-444444444444"
	makeRef := func(version, docID, quote string) document.PassageRefV1 {
		t.Helper()
		ref, err := document.NewPassageRefV1(document.PassageRefV1{
			VaultUID: vault, DocumentUID: docID, ContentVersionID: version,
			SourceSHA256: strings.Repeat("a", 64), RenditionBuildID: strings.Repeat("b", 64),
			AttachmentID: strings.Repeat("c", 64),
		}, []byte(quote), 0, len(quote))
		require.NoError(t, err)
		return ref
	}
	source := makeRef(sourceVersion, "55555555-5555-4555-8555-555555555555", "seed")
	target := makeRef(targetVersion, "66666666-6666-4666-8666-666666666666", "seed")
	request := api.ConnectionSuggestionRequest{Source: source, Profile: "local", BindingID: "semantic",
		Method: "lexical", Fence: api.DocumentSourceFence{VaultUID: vault,
			ContentVersionIDs: []string{sourceVersion, targetVersion}}}
	report := api.ConnectionSuggestionReport{State: "ready", Source: source, SeedKind: "passage",
		Method: "lexical", CandidateCount: 1, Aggregation: "exact_quote",
		ScoreMetric: "reciprocal_lexical_rank", SeedSegments: []api.ConnectionSeedSegment{},
		Candidates: []api.ConnectionSuggestion{{
			ID: strings.Repeat("d", 64), Source: source, SourceQuote: "seed", Target: target,
			TargetQuote: "seed", TargetNodeID: 7, TargetPath: "/match.txt", Method: "lexical",
			Reason: "exact_quote_overlap", Score: 1, ScoreMetric: "reciprocal_lexical_rank", Aggregation: "exact_quote",
			DuplicateMembers: []api.ConnectionDuplicateMember{},
		}}, FallbackMethods: []string{"lexical"}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/api/v1/connections/suggest", r.URL.Path)
		assert.Equal(t, "synthetic-key", r.Header.Get("X-Api-Key"))
		assert.NoError(t, json.MarshalWrite(w, report))
	}))
	t.Cleanup(server.Close)
	client := daemonconn.New(server.URL, "synthetic-key")
	got, err := client.SuggestConnections(t.Context(), request)
	require.NoError(t, err)
	assert.Equal(t, report, got)
	report.SeedSegmentCount = 1
	_, err = client.SuggestConnections(t.Context(), request)
	require.ErrorContains(t, err, "connection response is invalid",
		"a seed count without exact selected segment refs is not auditable")
	report.SeedSegmentCount = 0
	const permittedDuplicateVersion = "77777777-7777-4777-8777-777777777777"
	request.Fence.ContentVersionIDs = append(request.Fence.ContentVersionIDs, permittedDuplicateVersion)
	report.Candidates[0].DuplicateCount = 1
	report.Candidates[0].DuplicateMembers = []api.ConnectionDuplicateMember{{
		ID:          strings.Repeat("e", 64),
		Target:      makeRef(permittedDuplicateVersion, "88888888-8888-4888-8888-888888888888", "seed"),
		TargetQuote: "seed", TargetNodeID: 8, TargetPath: "/duplicate.txt",
		Score: 0.5, ScoreMetric: "reciprocal_lexical_rank",
	}}
	_, err = client.SuggestConnections(t.Context(), request)
	require.NoError(t, err, "a complete permitted duplicate is valid")
	report.Candidates[0].DuplicateMembers[0].Target.ContentVersionID = hiddenVersion
	_, err = client.SuggestConnections(t.Context(), request)
	require.ErrorContains(t, err, "connection response is invalid",
		"a listed duplicate must not escape the exact target fence")
	report.Candidates[0].DuplicateMembers = []api.ConnectionDuplicateMember{}
	request.Fence.ContentVersionIDs = request.Fence.ContentVersionIDs[:2]
	report.Candidates[0].DuplicateCount = 1
	_, err = client.SuggestConnections(t.Context(), request)
	require.ErrorContains(t, err, "connection response is invalid",
		"duplicate count must equal its fully listed verified members")
	report.Candidates[0].DuplicateCount = int(^uint(0) >> 1)
	_, err = client.SuggestConnections(t.Context(), request)
	require.ErrorContains(t, err, "connection response is invalid",
		"overflow cannot make an unlisted duplicate count valid")
	report.Candidates[0].DuplicateCount = 0

	report.Candidates[0].Target.ContentVersionID = hiddenVersion
	_, err = client.SuggestConnections(t.Context(), request)
	require.ErrorContains(t, err, "connection response is invalid")
	report.Candidates[0].Target.ContentVersionID = targetVersion
	report.Candidates[0].TargetQuote = "other"
	_, err = client.SuggestConnections(t.Context(), request)
	require.ErrorContains(t, err, "connection response is invalid")
}

func TestSuggestConnectionsRequiresZeroCandidateSemanticProvenance(t *testing.T) {
	ref, err := document.NewPassageRefV1(document.PassageRefV1{
		VaultUID:         "11111111-1111-4111-8111-111111111111",
		DocumentUID:      "22222222-2222-4222-8222-222222222222",
		ContentVersionID: "33333333-3333-4333-8333-333333333333",
		SourceSHA256:     strings.Repeat("a", 64), RenditionBuildID: strings.Repeat("b", 64),
		AttachmentID: strings.Repeat("c", 64),
	}, []byte("seed"), 0, 4)
	require.NoError(t, err)
	request := api.ConnectionSuggestionRequest{Source: ref, Profile: "local", BindingID: "semantic",
		Method: "semantic", Fence: api.DocumentSourceFence{VaultUID: ref.VaultUID,
			ContentVersionIDs: []string{ref.ContentVersionID}}}
	report := api.ConnectionSuggestionReport{State: "ready", Source: ref, SeedKind: "passage",
		Method: "semantic", Aggregation: "maximum_pair_score", ScoreMetric: "cosine",
		SeedSegmentCount: 1, SeedSegments: []api.ConnectionSeedSegment{{InputID: "seed-input",
			Passage: ref, Quote: "seed", Span: document.ChunkSpan{UnitIndex: 0, CharStart: 0, CharEnd: 4},
			GenerationID: strings.Repeat("d", 64)}},
		Candidates: []api.ConnectionSuggestion{}, FallbackMethods: []string{"lexical"}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		assert.NoError(t, json.MarshalWrite(w, report))
	}))
	t.Cleanup(server.Close)
	client := daemonconn.New(server.URL, "synthetic-key")
	_, err = client.SuggestConnections(t.Context(), request)
	require.ErrorContains(t, err, "connection response is invalid",
		"zero candidates still require selected vector and index provenance")
	report.VectorSpaceID = strings.Repeat("e", 64)
	report.IndexGenerationID = strings.Repeat("f", 64)
	report.SourceManifestChecksum = strings.Repeat("1", 64)
	report.SourceEmbeddingSetID = strings.Repeat("2", 64)
	_, err = client.SuggestConnections(t.Context(), request)
	require.NoError(t, err)
	report.SeedSegmentCount = 0
	report.SeedSegments = []api.ConnectionSeedSegment{}
	report.Truncated = true
	_, err = client.SuggestConnections(t.Context(), request)
	require.NoError(t, err, "a fully truncated ready page is not an unavailable result")
	report.Truncated = false
	_, err = client.SuggestConnections(t.Context(), request)
	require.ErrorContains(t, err, "connection response is invalid",
		"an untruncated semantic result must retain seed provenance")
}

func TestSuggestConnectionsRejectsOversizedValidResponse(t *testing.T) {
	ref, err := document.NewPassageRefV1(document.PassageRefV1{
		VaultUID:         "11111111-1111-4111-8111-111111111111",
		DocumentUID:      "22222222-2222-4222-8222-222222222222",
		ContentVersionID: "33333333-3333-4333-8333-333333333333",
		SourceSHA256:     strings.Repeat("a", 64), RenditionBuildID: strings.Repeat("b", 64),
		AttachmentID: strings.Repeat("c", 64),
	}, []byte("seed"), 0, 4)
	require.NoError(t, err)
	request := api.ConnectionSuggestionRequest{Source: ref, Profile: "local", BindingID: "semantic",
		Method: "lexical", Fence: api.DocumentSourceFence{VaultUID: ref.VaultUID,
			ContentVersionIDs: []string{ref.ContentVersionID}}}
	report := api.ConnectionSuggestionReport{State: "unavailable", CoverageReason: "empty_seed_passage",
		Source: ref, SeedKind: "passage", Method: "lexical", Candidates: []api.ConnectionSuggestion{},
		SeedSegments: []api.ConnectionSeedSegment{}, FallbackMethods: []string{"lexical"}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		assert.NoError(t, json.MarshalWrite(w, report))
		_, err := w.Write([]byte(strings.Repeat(" ", 8<<20)))
		assert.NoError(t, err)
	}))
	t.Cleanup(server.Close)
	_, err = daemonconn.New(server.URL, "synthetic-key").SuggestConnections(t.Context(), request)
	require.ErrorContains(t, err, "response exceeds byte limit")
}
