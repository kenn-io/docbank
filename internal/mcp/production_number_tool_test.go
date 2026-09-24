package mcp

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
)

func TestProductionNumberToolReadsExactAndBoundedRange(t *testing.T) {
	match := api.ProductionNumberReference{Label: "OUR000041",
		JobID: testBatesOperationID, SetID: testBatesSnapshotID, Revision: 1,
		ProductionReceiptSHA256: testProfileID, ArtifactManifestSHA256: testProfileID,
		SourceVersionID: testBatesAllocationID, OccurrenceID: testBatesSnapshotID,
		Page: 1, ArtifactID: testBatesAllocationID, ArtifactSHA256: testProfileID,
		ArtifactPath: "VOL001/custom-output.pdf", Volume: "VOL001"}
	var calls int
	daemon := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		calls++
		assert.Equal(t, http.MethodGet, request.Method)
		assert.Equal(t, "/api/v1/productions/numbers", request.URL.Path)
		query := request.URL.Query()
		if query.Get("label") != "" {
			assert.Equal(t, match.Label, query.Get("label"))
			writeDaemonJSON(t, response, api.ProductionNumberPage{Items: []api.ProductionNumberReference{match}})
			return
		}
		assert.Equal(t, testBatesNamespaceID, query.Get("namespace_id"))
		assert.Equal(t, "41", query.Get("start_sequence"))
		assert.Equal(t, "42", query.Get("end_sequence"))
		assert.Equal(t, "1", query.Get("limit"))
		writeDaemonJSON(t, response, api.ProductionNumberPage{
			Items: []api.ProductionNumberReference{match}, NextSequence: 41})
	}))
	t.Cleanup(daemon.Close)
	server := newBatesToolTestServer(t, daemon.URL, false)
	exact := callToolResult(t, server, "find_production_numbers", map[string]any{"label": match.Label})
	exactItems := objectField(t, exact, "structuredContent")["items"]
	require.Len(t, exactItems, 1)
	ranged := callToolResult(t, server, "find_production_numbers", map[string]any{
		"namespace_id": testBatesNamespaceID, "start_sequence": 41, "end_sequence": 42, "limit": 1})
	require.Len(t, objectField(t, ranged, "structuredContent")["items"], 1)
	assert.EqualValues(t, 41, objectField(t, ranged, "structuredContent")["next_sequence"])
	assert.Equal(t, 2, calls)
}

func TestProductionNumberToolBoundsAndRequiresOneSelector(t *testing.T) {
	tool := catalogMap(toolCatalog(false))["find_production_numbers"]
	require.NotNil(t, tool)
	assertSchemaAccepts(t, tool.InputSchema, map[string]any{"label": "OUR000041"})
	ranged := map[string]any{"namespace_id": testBatesNamespaceID,
		"start_sequence": 41, "end_sequence": 42, "limit": 25}
	assertSchemaAccepts(t, tool.InputSchema, ranged)
	ranged["limit"] = 26
	assertSchemaRejects(t, tool.InputSchema, ranged)
	_, err := findProductionNumbers(t.Context(), nil, []byte(`{}`))
	require.Error(t, err)
	_, err = findProductionNumbers(t.Context(), nil, []byte(`{"label":"OUR000041","namespace_id":"11111111-1111-4111-8111-111111111111"}`))
	require.Error(t, err)
	_, err = findProductionNumbers(t.Context(), nil, []byte(`{"namespace_id":"11111111-1111-4111-8111-111111111111"}`))
	require.Error(t, err)
}
