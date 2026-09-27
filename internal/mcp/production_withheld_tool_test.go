package mcp

import (
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	documentproduction "go.kenn.io/docbank/document/production"
	"go.kenn.io/docbank/document/redaction"
	"go.kenn.io/docbank/internal/api"
)

func syntheticMCPWithheldFile(t *testing.T) (string, api.ProductionWithheldSelectionCreateRequest,
	documentproduction.WithheldSelection) {
	t.Helper()
	versionID := "89000000-0000-4000-8000-000000000004"
	request := api.ProductionWithheldSelectionCreateRequest{
		OperationID:  "89000000-0000-4000-8000-000000000001",
		SelectionID:  "89000000-0000-4000-8000-000000000002",
		PolicySHA256: strings.Repeat("a", 64),
		Members: []documentproduction.WithheldMember{{
			ID: "89000000-0000-4000-8000-000000000005", Ordinal: 1,
			SourceVersionID: versionID, SourceSHA256: strings.Repeat("b", 64),
			SourceSize: 100, FamilyOrder: 1,
			Family: redaction.FamilyContext{Kind: "standalone", RootVersionID: versionID},
		}},
	}
	result := documentproduction.WithheldSelection{
		Contract: documentproduction.WithheldSelectionContractV1,
		ID:       request.SelectionID, SetID: "89000000-0000-4000-8000-000000000003", Revision: 2,
		PolicySHA256: request.PolicySHA256, Members: request.Members,
	}
	var err error
	_, result.SHA256, err = documentproduction.CanonicalWithheldSelection(result)
	require.NoError(t, err)
	raw, err := json.Marshal(request)
	require.NoError(t, err)
	file := filepath.Join(t.TempDir(), "private-withheld.json")
	require.NoError(t, os.WriteFile(file, raw, 0o600))
	return file, request, result
}

func TestProductionWithheldToolRecordsPrivateExactSelection(t *testing.T) {
	file, request, result := syntheticMCPWithheldFile(t)
	var creates int
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		creates++
		assert.Equal(t, http.MethodPost, r.Method)
		assert.Equal(t, "synthetic-key", r.Header.Get("X-Api-Key"))
		assert.Equal(t, "/api/v1/productions/sets/"+result.SetID+"/revisions/2/withheld-selection", r.URL.Path)
		var actual api.ProductionWithheldSelectionCreateRequest
		assert.NoError(t, json.UnmarshalRead(r.Body, &actual))
		assert.Equal(t, request, actual)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		assert.NoError(t, json.MarshalWrite(w, result))
	}))
	t.Cleanup(daemon.Close)
	require.Nil(t, catalogMap(toolCatalog(false))["create_production_withheld_selection"])
	tool := catalogMap(toolCatalog(true))["create_production_withheld_selection"]
	require.NotNil(t, tool)
	args := map[string]any{"set_id": result.SetID, "revision": 2, "selection_file": file}
	assertSchemaAccepts(t, tool.InputSchema, args)
	server := newBatesToolTestServer(t, daemon.URL, true)
	created := callToolResult(t, server, "create_production_withheld_selection", args)
	require.NotEqual(t, true, created["isError"], created)
	summary := objectField(t, created, "structuredContent")
	require.Equal(t, result.ID, summary["selection_id"])
	require.Equal(t, result.SHA256, summary["sha256"])
	require.EqualValues(t, 1, summary["member_count"])
	encoded, err := json.Marshal(created)
	require.NoError(t, err)
	require.NotContains(t, string(encoded), result.Members[0].SourceVersionID)
	require.Equal(t, 1, creates)
	args["selection_file"] = "relative-withheld.json"
	bad := decodeWireError(t, exchangeRaw(t, server, requestFor("tools/call", map[string]any{
		"name": "create_production_withheld_selection", "arguments": args,
	})))
	require.NotContains(t, bad.Message, result.Members[0].SourceVersionID)
	require.Equal(t, 1, creates)
}

func TestProductionWithheldToolDoesNotRetryUnknownOutcome(t *testing.T) {
	file, _, result := syntheticMCPWithheldFile(t)
	var calls int
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte("{"))
	}))
	t.Cleanup(daemon.Close)
	created := callToolResult(t, newBatesToolTestServer(t, daemon.URL, true),
		"create_production_withheld_selection", map[string]any{
			"set_id": result.SetID, "revision": 2, "selection_file": file,
		})
	require.Equal(t, true, created["isError"])
	require.Equal(t, "production_withheld_outcome_unknown", objectField(t, created, "structuredContent")["code"])
	require.Equal(t, 1, calls)
}
