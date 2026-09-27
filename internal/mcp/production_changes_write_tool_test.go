package mcp

import (
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document/redaction"
	"go.kenn.io/docbank/internal/api"
)

func TestProductionMCPAppendAndChangeBatchesUseDaemon(t *testing.T) {
	const setID = "79000000-0000-4000-8000-0000000000d1"
	const memberID = "79000000-0000-4000-8000-0000000000d2"
	const versionID = "79000000-0000-4000-8000-0000000000d3"
	const appendID = "79000000-0000-4000-8000-0000000000d4"
	const changesID = "79000000-0000-4000-8000-0000000000d5"
	digest := strings.Repeat("a", 64)
	member := redaction.Member{ID: memberID, VaultID: "79000000-0000-4000-8000-0000000000d6",
		SourceVersionID: versionID, SourceSHA256: digest, PDFSHA256: digest, NodeID: 1,
		SourceSize: 10, PDFSize: 10, Ordinal: 1,
		Family: redaction.FamilyContext{Kind: "standalone", RootVersionID: versionID},
		Mode:   "keep_selected", MapSHA256: digest, PageInventorySHA256: digest}
	decision := redaction.Decision{ID: "79000000-0000-4000-8000-0000000000d7", MemberID: memberID,
		Action: "keep", Reason: "synthetic reason", Label: "synthetic", Selector: redaction.Selector{
			Kind: "text", MapSHA256: digest, Span: &redaction.Span{Start: 0, End: 1}}}
	memberBytes, err := json.Marshal([]api.ProductionMember{api.ProductionMember(member)})
	require.NoError(t, err)
	changeBytes, err := json.Marshal([]api.ProductionChange{{Kind: "decision", Decision: new(api.ProductionDecision(decision))}})
	require.NoError(t, err)
	var calls int
	daemon := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		calls++
		assert.Equal(t, http.MethodPost, request.Method)
		assert.Equal(t, "2", request.Header.Get("If-Match"))
		var operationID string
		switch request.URL.Path {
		case "/api/v1/productions/sets/" + setID + "/revisions/1/members":
			var body api.ProductionMemberAppendRequest
			if !assert.NoError(t, decodeDaemonJSON(request.Body, &body)) {
				return
			}
			assert.Len(t, body.Members, 1)
			assert.Equal(t, memberID, body.Members[0].ID)
			operationID = body.OperationID
			assert.Equal(t, appendID, operationID)
		case "/api/v1/productions/sets/" + setID + "/revisions/1/changes":
			var body api.ProductionChangesRequest
			if !assert.NoError(t, decodeDaemonJSON(request.Body, &body)) {
				return
			}
			assert.Len(t, body.Changes, 1)
			assert.Equal(t, "decision", body.Changes[0].Kind)
			operationID = body.OperationID
			assert.Equal(t, changesID, operationID)
		default:
			http.NotFound(response, request)
			return
		}
		writeDaemonJSON(t, response, api.ProductionReceipt(redaction.Receipt{
			OperationID: operationID, SetID: setID, Revision: 1, ETag: 3, RequestSHA256: digest}))
	}))
	t.Cleanup(daemon.Close)
	server := newBatesToolTestServer(t, daemon.URL, true)
	appendResult := objectField(t, callToolResult(t, server, "append_production_members", map[string]any{
		"set_id": setID, "revision": 1, "etag": 2, "operation_id": appendID,
		"members_json": string(memberBytes)}), "structuredContent")
	assert.Equal(t, appendID, appendResult["operation_id"])
	changesResult := objectField(t, callToolResult(t, server, "apply_production_changes", map[string]any{
		"set_id": setID, "revision": 1, "etag": 2, "operation_id": changesID,
		"changes_json": string(changeBytes)}), "structuredContent")
	assert.Equal(t, changesID, changesResult["operation_id"])
	assert.Equal(t, 2, calls)
}

func TestProductionMCPBatchWritesRejectUnknownAndOversizeJSON(t *testing.T) {
	tools := catalogMap(toolCatalog(true))
	appendTool := tools["append_production_members"]
	require.NotNil(t, appendTool)
	changesTool := tools["apply_production_changes"]
	require.NotNil(t, changesTool)
	base := map[string]any{"set_id": "79000000-0000-4000-8000-0000000000d1", "revision": 1,
		"etag": 2, "operation_id": "79000000-0000-4000-8000-0000000000d4"}
	assertSchemaRejects(t, appendTool.InputSchema, base)
	base["members_json"] = strings.Repeat("x", redaction.MaxCommandBytes+1)
	assertSchemaRejects(t, appendTool.InputSchema, base)
	server := newBatesToolTestServer(t, "http://127.0.0.1:1", true)
	base["members_json"] = `[{"unknown":1}]`
	badMember := exchangeRaw(t, server, requestFor("tools/call", map[string]any{
		"name": "append_production_members", "arguments": base,
	}))
	assert.NotZero(t, decodeWireError(t, badMember).Code)
	delete(base, "members_json")
	base["changes_json"] = `[{"kind":"invented"}]`
	badChange := exchangeRaw(t, server, requestFor("tools/call", map[string]any{
		"name": "apply_production_changes", "arguments": base,
	}))
	assert.NotZero(t, decodeWireError(t, badChange).Code)
}
