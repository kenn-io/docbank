package mcp

import (
	"context"
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

func TestSuggestConnectionsToolPreservesExactUnavailableCoverage(t *testing.T) {
	const vault = "11111111-1111-4111-8111-111111111111"
	const version = "22222222-2222-4222-8222-222222222222"
	ref, err := document.NewPassageRefV1(document.PassageRefV1{VaultUID: vault,
		DocumentUID: "33333333-3333-4333-8333-333333333333", ContentVersionID: version,
		SourceSHA256: strings.Repeat("a", 64), RenditionBuildID: strings.Repeat("b", 64),
		AttachmentID: strings.Repeat("c", 64)}, []byte("seed"), 0, 4)
	require.NoError(t, err)
	request := api.ConnectionSuggestionRequest{Source: ref, Profile: "local", BindingID: "semantic",
		Method: "semantic", Fence: api.DocumentSourceFence{VaultUID: vault, ContentVersionIDs: []string{version}}}
	report := api.ConnectionSuggestionReport{State: "unavailable", CoverageReason: "missing_seed_embedding",
		Source: ref, SeedKind: "passage", Method: "semantic", Candidates: []api.ConnectionSuggestion{},
		SeedSegments:    []api.ConnectionSeedSegment{},
		FallbackMethods: []string{"lexical"}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/api/v1/connections/suggest", r.URL.Path)
		assert.Equal(t, "synthetic-key", r.Header.Get("X-Api-Key"))
		assert.NoError(t, json.MarshalWrite(w, report))
	}))
	t.Cleanup(server.Close)
	lease := newDaemonLeaseWith(func(context.Context) (*daemonconn.Connection, error) {
		return daemonconn.New(server.URL, "synthetic-key"), nil
	}, func(*daemonconn.Connection) error { return nil })
	require.Contains(t, catalogMap(toolCatalog(false, false)), "suggest_connections")
	var arguments map[string]any
	encoded, err := json.Marshal(request)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(encoded, &arguments))
	result, err := invokeReadTool(t.Context(), lease, "suggest_connections", arguments)
	require.NoError(t, err)
	require.False(t, result.IsError)
	output := structuredMap(t, result.StructuredContent)
	assert.Equal(t, "unavailable", output["state"])
	assert.Equal(t, "missing_seed_embedding", output["coverage_reason"])
	assert.Equal(t, []any{}, output["candidates"])
	assert.EqualValues(t, 0, output["ttlMs"])
	assert.Equal(t, "private", output["cacheScope"])

	const targetVersion = "44444444-4444-4444-8444-444444444444"
	target, err := document.NewPassageRefV1(document.PassageRefV1{VaultUID: vault,
		DocumentUID: "55555555-5555-4555-8555-555555555555", ContentVersionID: targetVersion,
		SourceSHA256: strings.Repeat("d", 64), RenditionBuildID: strings.Repeat("e", 64),
		AttachmentID: strings.Repeat("f", 64)}, []byte("seed"), 0, 4)
	require.NoError(t, err)
	request.Method = "lexical"
	request.Fence.ContentVersionIDs = append(request.Fence.ContentVersionIDs, targetVersion)
	report = api.ConnectionSuggestionReport{State: "ready", Source: ref, SeedKind: "passage",
		Method: "lexical", CandidateCount: 1, Aggregation: "exact_quote",
		ScoreMetric: "reciprocal_lexical_rank", SeedSegments: []api.ConnectionSeedSegment{},
		Candidates: []api.ConnectionSuggestion{{
			ID: strings.Repeat("a", 64), Method: "lexical", Reason: "exact_quote_overlap",
			Source: ref, SourceQuote: "seed", Target: target, TargetQuote: "seed",
			TargetNodeID: 9, TargetPath: "/target.txt", Score: 1,
			ScoreMetric: "reciprocal_lexical_rank", Aggregation: "exact_quote",
			DuplicateMembers: []api.ConnectionDuplicateMember{},
		}}, FallbackMethods: []string{"lexical"}}
	encoded, err = json.Marshal(request)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(encoded, &arguments))
	result, err = invokeReadTool(t.Context(), lease, "suggest_connections", arguments)
	require.NoError(t, err)
	require.False(t, result.IsError)
	output = structuredMap(t, result.StructuredContent)
	assert.Equal(t, "exact_quote", output["aggregation"])
	assert.Equal(t, "reciprocal_lexical_rank", output["score_metric"])
	assert.Equal(t, []any{}, output["seed_segments"])
	candidates, ok := output["candidates"].([]any)
	require.True(t, ok)
	require.Len(t, candidates, 1)
	listed, ok := candidates[0].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, []any{}, listed["duplicate_members"])
	output = structuredMap(t, result.StructuredContent)
	assert.Equal(t, "ready", output["state"])
	assert.EqualValues(t, 1, output["candidate_count"])
	assertSchemaAccepts(t, catalogMap(toolCatalog(false, false))["suggest_connections"].OutputSchema, output)
	request.Method = "semantic"
	request.Fence.ContentVersionIDs = []string{version}
	report = api.ConnectionSuggestionReport{State: "ready", Source: ref, SeedKind: "passage",
		Method: "semantic", Aggregation: "maximum_pair_score", ScoreMetric: "cosine",
		VectorSpaceID: strings.Repeat("1", 64), IndexGenerationID: strings.Repeat("2", 64),
		SourceManifestChecksum: strings.Repeat("3", 64), SourceEmbeddingSetID: strings.Repeat("4", 64),
		SeedSegmentCount: 1, SeedSegments: []api.ConnectionSeedSegment{{InputID: "seed-input",
			Passage: ref, Quote: "seed", Span: document.ChunkSpan{UnitIndex: 0, CharStart: 0, CharEnd: 4},
			GenerationID: strings.Repeat("5", 64)}},
		Candidates: []api.ConnectionSuggestion{}, FallbackMethods: []string{"lexical"}}
	encoded, err = json.Marshal(request)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(encoded, &arguments))
	result, err = invokeReadTool(t.Context(), lease, "suggest_connections", arguments)
	require.NoError(t, err)
	require.False(t, result.IsError)
	output = structuredMap(t, result.StructuredContent)
	assert.Equal(t, report.IndexGenerationID, output["index_generation_id"])
	assert.Equal(t, report.SourceManifestChecksum, output["source_manifest_checksum"])
	assert.EqualValues(t, 1, output["seed_segment_count"])
	assertSchemaAccepts(t, catalogMap(toolCatalog(false, false))["suggest_connections"].OutputSchema, output)

	request.Method = "lexical"
	request.Fence.ContentVersionIDs = []string{targetVersion}
	encoded, err = json.Marshal(request)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(encoded, &arguments))
	_, err = invokeReadTool(t.Context(), lease, "suggest_connections", arguments)
	require.Error(t, err, "a seed outside the exact source fence must be denied")
}
