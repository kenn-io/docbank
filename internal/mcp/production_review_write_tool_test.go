package mcp

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document/redaction"
	"go.kenn.io/docbank/internal/api"
)

func TestProductionMCPInstructionsSealAndReviewBindExactDraft(t *testing.T) {
	const setID = "79000000-0000-4000-8000-0000000000c1"
	const memberID = "79000000-0000-4000-8000-0000000000c2"
	const editID = "79000000-0000-4000-8000-0000000000c3"
	const sealID = "79000000-0000-4000-8000-0000000000c4"
	const reviewID = "79000000-0000-4000-8000-0000000000c5"
	digest := strings.Repeat("a", 64)
	var calls int
	daemon := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		calls++
		assert.Equal(t, "2", request.Header.Get("If-Match"))
		var operationID string
		switch request.URL.Path {
		case "/api/v1/productions/sets/" + setID + "/revisions/1/instructions":
			assert.Equal(t, http.MethodPut, request.Method)
			var body api.ProductionInstructionsRequest
			if !assert.NoError(t, decodeDaemonJSON(request.Body, &body)) {
				return
			}
			assert.Equal(t, "Synthetic instructions", body.Instructions)
			operationID = body.OperationID
			assert.Equal(t, editID, operationID)
		case "/api/v1/productions/sets/" + setID + "/revisions/1/seal":
			assert.Equal(t, http.MethodPost, request.Method)
			var body api.ProductionMembershipSealRequest
			if !assert.NoError(t, decodeDaemonJSON(request.Body, &body)) {
				return
			}
			assert.Equal(t, 1, body.Total)
			assert.Equal(t, digest, body.MemberHash)
			operationID = body.OperationID
			assert.Equal(t, sealID, operationID)
		case "/api/v1/productions/sets/" + setID + "/revisions/1/members/" + memberID + "/review":
			assert.Equal(t, http.MethodPost, request.Method)
			var body api.ProductionMemberReviewRequest
			if !assert.NoError(t, decodeDaemonJSON(request.Body, &body)) {
				return
			}
			assert.Equal(t, digest, body.Binding)
			assert.True(t, body.Complete)
			operationID = body.OperationID
			assert.Equal(t, reviewID, operationID)
		default:
			http.NotFound(response, request)
			return
		}
		writeDaemonJSON(t, response, api.ProductionReceipt(redaction.Receipt{
			OperationID: operationID, SetID: setID, Revision: 1, ETag: 3, RequestSHA256: digest}))
	}))
	t.Cleanup(daemon.Close)
	server := newBatesToolTestServer(t, daemon.URL, true)
	edit := map[string]any{"set_id": setID, "revision": 1, "etag": 2,
		"operation_id": editID, "instructions": "Synthetic instructions"}
	edited := objectField(t, callToolResult(t, server, "edit_production_instructions", edit), "structuredContent")
	assert.EqualValues(t, 3, edited["etag"])
	assert.Equal(t, "private", edited["cacheScope"])
	seal := map[string]any{"set_id": setID, "revision": 1, "etag": 2,
		"operation_id": sealID, "total": 1, "member_hash": digest}
	sealed := objectField(t, callToolResult(t, server, "seal_production_membership", seal), "structuredContent")
	assert.Equal(t, sealID, sealed["operation_id"])
	review := map[string]any{"set_id": setID, "revision": 1, "etag": 2,
		"operation_id": reviewID, "member_id": memberID, "binding": digest}
	reviewed := objectField(t, callToolResult(t, server, "review_production_member", review), "structuredContent")
	assert.Equal(t, reviewID, reviewed["operation_id"])
	assert.Equal(t, 3, calls)
}

func TestProductionMCPReviewWriteSchemasRejectMissingFences(t *testing.T) {
	readOnly := catalogMap(toolCatalog(false))
	assert.NotContains(t, readOnly, "edit_production_instructions")
	tools := catalogMap(toolCatalog(true))
	edit := tools["edit_production_instructions"]
	require.NotNil(t, edit)
	assertSchemaRejects(t, edit.InputSchema, map[string]any{
		"set_id": "79000000-0000-4000-8000-0000000000c1", "revision": 1,
		"operation_id": "79000000-0000-4000-8000-0000000000c3", "instructions": "x"})
	seal := tools["seal_production_membership"]
	require.NotNil(t, seal)
	assertSchemaRejects(t, seal.InputSchema, map[string]any{
		"set_id": "79000000-0000-4000-8000-0000000000c1", "revision": 1, "etag": 2,
		"operation_id": "79000000-0000-4000-8000-0000000000c4", "total": 1, "member_hash": "bad"})
	review := tools["review_production_member"]
	require.NotNil(t, review)
	assertSchemaRejects(t, review.InputSchema, map[string]any{
		"set_id": "79000000-0000-4000-8000-0000000000c1", "revision": 1, "etag": 2,
		"operation_id": "79000000-0000-4000-8000-0000000000c5", "member_id": "bad", "binding": strings.Repeat("a", 64)})
}
