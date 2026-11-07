package mcp

import (
	"context"
	"encoding/json/v2"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/processing"
)

type validityDocumentClient struct {
	documentClient

	resolution        api.DocumentSourceFenceResolution
	report            api.DocumentSearchReport
	searchRequest     api.DocumentSearchRequest
	validationRequest api.DocumentSearchValidationRequest
	searchCalls       int
	validationCalls   int
	resolved          [][]api.DocumentIdentity
}

func (c *validityDocumentClient) ResolveDocumentSourceFence(context.Context, api.DocumentSourceFenceResolveRequest) (api.DocumentSourceFenceResolution, error) {
	return c.resolution, nil
}

func (c *validityDocumentClient) SearchDocuments(_ context.Context, request api.DocumentSearchRequest) (api.DocumentSearchReport, error) {
	c.searchCalls++
	c.searchRequest = request
	return c.report, nil
}

func (c *validityDocumentClient) ValidateDocumentSearch(_ context.Context, request api.DocumentSearchValidationRequest) error {
	c.validationCalls++
	c.validationRequest = request
	return nil
}

func (c *validityDocumentClient) ResolveDocumentSummaries(_ context.Context, request api.DocumentSummaryResolveRequest) ([]api.DocumentSummary, error) {
	identities := append([]api.DocumentIdentity(nil), request.Identities...)
	c.resolved = append(c.resolved, identities)
	result := make([]api.DocumentSummary, len(identities))
	for index, identity := range identities {
		result[index] = api.DocumentSummary{
			NodeID: identity.NodeID, ContentVersionID: identity.ContentVersionID,
			Path: identity.Path, Name: "synthetic.txt", MediaType: "text/plain",
			ModifiedAt: "2025-01-01T00:00:00Z",
			ActiveRenditions: []api.DocumentRenditionIdentity{{
				AttachmentID: testAttachmentID, BuildID: testBuildID, ProfileFingerprint: testProfileID,
			}},
		}
	}
	return result, nil
}

func validityClient() *validityDocumentClient {
	return &validityDocumentClient{
		resolution: api.DocumentSourceFenceResolution{
			Fence:            api.ResolvedDocumentSourceFence{VaultUID: testVaultID, ContentVersionIDs: []string{testVersionID}},
			FenceFingerprint: testFenceFingerprint, ObservedScopeCount: 1,
		},
		report: api.DocumentSearchReport{
			AsOf: "2025-01-01", RequestedMode: "lexical", ActualMode: "lexical",
			Coverage:     api.DocumentSearchCoverage{State: "complete"},
			Degradations: []string{}, Results: []api.DocumentSearchResult{}, Trace: []api.DocumentSearchTrace{},
		},
	}
}

func runValiditySearch(tb testing.TB, client *validityDocumentClient, arguments map[string]any) (searchDocumentsOutput, error) {
	tb.Helper()
	raw, err := json.Marshal(arguments)
	require.NoError(tb, err)
	output, _, err := searchDocuments(tb.Context(), func(ctx context.Context, call func(context.Context, documentClient) error) error {
		return call(ctx, client)
	}, raw)
	return output, err
}

func validityArgs() map[string]any {
	return map[string]any{
		"query": "synthetic", "profile": "local", "mode": "lexical",
		"filters": map[string]any{},
	}
}

func setEmptyValidityFence(t *testing.T, client *validityDocumentClient) {
	t.Helper()
	client.resolution.Fence.ContentVersionIDs = []string{}
	fingerprint, err := processing.SourceFenceFingerprint(processing.SourceFence{VaultUID: testVaultID})
	require.NoError(t, err)
	client.resolution.FenceFingerprint = fingerprint
	client.resolution.ObservedScopeCount = 0
}

func TestSearchValidityPerResultOutputAndSchema(t *testing.T) {
	inputSchema, outputSchema := searchDocumentsSchemas()
	arguments := validityArgs()
	arguments["as_of"] = "2024-06-01"
	assertSchemaAccepts(t, inputSchema, arguments)

	relevance := 0.81
	client := validityClient()
	client.report.AsOf = "2024-06-01"
	client.report.Results = []api.DocumentSearchResult{{
		VaultUID: testVaultID, NodeID: 7, ContentVersionID: testVersionID,
		Rank: 1, Score: 0.77, RerankRelevance: &relevance, Path: "/old.txt",
		Evidence: []api.DocumentEvidenceReference{},
		Validity: &api.DocumentResultValidity{
			State: "superseded", EffectiveTo: "2023-12-31",
			SupersededBy: []api.DocumentValiditySuccessor{{NodeID: 8, Path: "/new.txt", EffectiveFrom: "2024-01-01"}},
		},
	}}
	output, err := runValiditySearch(t, client, arguments)
	require.NoError(t, err)
	assert.Equal(t, "2024-06-01", client.searchRequest.AsOf)
	require.Len(t, output.Results, 1)
	assert.Equal(t, "2024-06-01", output.AsOf)
	assert.Equal(t, &relevance, output.Results[0].RerankRelevance)
	require.NotNil(t, output.Results[0].Validity)
	assert.Equal(t, "superseded", output.Results[0].Validity.State)
	assert.Equal(t, "2023-12-31", output.Results[0].Validity.EffectiveTo)
	assert.Equal(t, []api.DocumentValiditySuccessor{{NodeID: 8, Path: "/new.txt", EffectiveFrom: "2024-01-01"}}, output.Results[0].Validity.SupersededBy)

	encoded, err := json.Marshal(output)
	require.NoError(t, err)
	var mapped map[string]any
	require.NoError(t, json.Unmarshal(encoded, &mapped))
	assertSchemaAccepts(t, outputSchema, mapped)
	assert.Equal(t, "2024-06-01", mapped["as_of"])
	hits, ok := mapped["results"].([]any)
	require.True(t, ok)
	require.Len(t, hits, 1)
	hit, ok := hits[0].(map[string]any)
	require.True(t, ok)
	validity := objectField(t, hit, "validity")
	assert.Equal(t, "superseded", validity["state"])
	successors, ok := validity["superseded_by"].([]any)
	require.True(t, ok)
	require.Len(t, successors, 1)
	successor, ok := successors[0].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "/new.txt", successor["path"])
	assert.InDelta(t, float64(8), successor["node_id"], 0)

	client.report.Results[0].Validity = nil
	output, err = runValiditySearch(t, client, arguments)
	require.NoError(t, err)
	encoded, err = json.Marshal(output)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(encoded, &mapped))
	hits, ok = mapped["results"].([]any)
	require.True(t, ok)
	hit, ok = hits[0].(map[string]any)
	require.True(t, ok)
	assert.NotContains(t, hit, "validity")
}

func TestSearchValidityDateInputAndEmptyFence(t *testing.T) {
	inputSchema, outputSchema := searchDocumentsSchemas()
	arguments := map[string]any{
		"query": "synthetic", "profile": "local", "mode": "lexical",
		"content_version_ids": []string{testVersionID}, "as_of": "2024-06-01",
	}
	assertSchemaAccepts(t, inputSchema, arguments)
	withOldOption := map[string]any{
		"query": "synthetic", "profile": "local", "filters": map[string]any{}, "superseded": "keep_order",
	}
	assertSchemaRejects(t, inputSchema, withOldOption)

	client := validityClient()
	setEmptyValidityFence(t, client)
	output, err := runValiditySearch(t, client, arguments)
	require.NoError(t, err)
	assert.Equal(t, 1, client.validationCalls)
	assert.Zero(t, client.searchCalls, "an empty exact fence must not become an unscoped search")
	assert.Equal(t, "2024-06-01", output.AsOf)
	require.NotNil(t, output.ObservedScopeCount)
	assert.Zero(t, *output.ObservedScopeCount)
	encoded, err := json.Marshal(output)
	require.NoError(t, err)
	var mapped map[string]any
	require.NoError(t, json.Unmarshal(encoded, &mapped))
	assertSchemaAccepts(t, outputSchema, mapped)

	client = validityClient()
	setEmptyValidityFence(t, client)
	delete(arguments, "as_of")
	output, err = runValiditySearch(t, client, arguments)
	require.NoError(t, err)
	assert.Empty(t, output.AsOf, "without a search report, an omitted date has no daemon default to echo")

	for _, value := range []string{"yesterday", "2025-1-01", "2025-02-30", "2024-02-30", "2025-01-01T00:00:00Z", "0000-01-01"} {
		invalid := map[string]any{
			"query": "synthetic", "profile": "local", "filters": map[string]any{}, "as_of": value,
		}
		wireErr := decodeWireError(t, callToolWire(t, "search_documents", invalid))
		assert.Equal(t, int64(jsonrpc.CodeInvalidParams), wireErr.Code, "as_of=%q", value)
	}
}
