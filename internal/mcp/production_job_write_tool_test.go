package mcp

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document/redaction"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/store"
)

func TestProductionMCPFinalizeAdmitAndCancelUseDaemon(t *testing.T) {
	const createID = "79000000-0000-4000-8000-0000000000e1"
	const finalizeID = "79000000-0000-4000-8000-0000000000e2"
	const admitID = "79000000-0000-4000-8000-0000000000e3"
	const cancelID = "79000000-0000-4000-8000-0000000000e4"
	const namespaceID = "79000000-0000-4000-8000-0000000000e5"
	const snapshotID = "79000000-0000-4000-8000-0000000000e6"
	const jobID = "79000000-0000-4000-8000-0000000000e7"
	digest := strings.Repeat("a", 64)
	vault, err := store.Open(filepath.Join(t.TempDir(), "docbank.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, vault.Close()) })
	set, draft, err := vault.CreateProductionSet(t.Context(), "synthetic-operator", redaction.CreateRequest{
		OperationID: createID, Name: "Synthetic MCP job"})
	require.NoError(t, err)
	draft.State = "finalized"
	var calls int
	daemon := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		calls++
		assert.Equal(t, http.MethodPost, request.Method)
		assert.Equal(t, "1", request.Header.Get("If-Match"))
		switch request.URL.Path {
		case "/api/v1/productions/sets/" + set.ID + "/revisions/1/finalize":
			var body api.ProductionFinalizeRequest
			if !assert.NoError(t, decodeDaemonJSON(request.Body, &body)) {
				return
			}
			assert.Equal(t, finalizeID, body.OperationID)
			assert.Equal(t, namespaceID, body.NamespaceID)
			assert.Equal(t, snapshotID, body.SnapshotID)
			if body.StartAt == 55 {
				response.Header().Set("Content-Type", "application/problem+json")
				response.WriteHeader(http.StatusConflict)
				writeDaemonJSON(t, response, api.NewError(http.StatusConflict,
					"approval_required", "synthetic private gate detail"))
				return
			}
			writeDaemonJSON(t, response, api.ProductionFinalizationResult(store.ProductionFinalizationResult{
				Draft: draft, OperationID: finalizeID, NamespaceID: namespaceID,
				SnapshotID: snapshotID, PreparedSHA256: digest, ReceiptSHA256: digest}))
		case "/api/v1/productions/sets/" + set.ID + "/revisions/1/jobs":
			var body api.ProductionJobAdmissionRequest
			if !assert.NoError(t, decodeDaemonJSON(request.Body, &body)) {
				return
			}
			assert.Equal(t, admitID, body.OperationID)
			assert.Equal(t, jobID, body.JobID)
			response.Header().Set("Content-Type", "application/json")
			response.WriteHeader(http.StatusCreated)
			writeDaemonJSON(t, response, api.ProductionJobStatus{JobID: jobID, SetID: set.ID,
				Revision: 1, State: "queued", RevisionSHA256: digest})
		case "/api/v1/productions/sets/" + set.ID + "/jobs/" + jobID + "/cancel":
			var body api.ProductionJobCancelRequest
			if !assert.NoError(t, decodeDaemonJSON(request.Body, &body)) {
				return
			}
			assert.Equal(t, cancelID, body.OperationID)
			writeDaemonJSON(t, response, api.ProductionReceipt(redaction.Receipt{
				OperationID: cancelID, SetID: set.ID, Revision: 1, ETag: 1, RequestSHA256: digest}))
		default:
			http.NotFound(response, request)
		}
	}))
	t.Cleanup(daemon.Close)
	server := newBatesToolTestServer(t, daemon.URL, true)
	finalized := objectField(t, callToolResult(t, server, "finalize_production_draft", map[string]any{
		"set_id": set.ID, "revision": 1, "etag": 1, "operation_id": finalizeID,
		"namespace_id": namespaceID, "snapshot_id": snapshotID}), "structuredContent")
	assert.Equal(t, "finalized", objectField(t, finalized, "draft")["state"])
	admitted := objectField(t, callToolResult(t, server, "admit_production_job", map[string]any{
		"set_id": set.ID, "revision": 1, "etag": 1, "operation_id": admitID,
		"job_id": jobID}), "structuredContent")
	assert.Equal(t, "queued", objectField(t, admitted, "job")["state"])
	canceled := objectField(t, callToolResult(t, server, "cancel_production_job", map[string]any{
		"set_id": set.ID, "job_id": jobID, "etag": 1, "operation_id": cancelID}), "structuredContent")
	assert.Equal(t, cancelID, canceled["operation_id"])
	gate := callToolResult(t, server, "finalize_production_draft", map[string]any{
		"set_id": set.ID, "revision": 1, "etag": 1, "operation_id": finalizeID,
		"namespace_id": namespaceID, "snapshot_id": snapshotID, "start_at": 55})
	assert.Equal(t, true, gate["isError"])
	assert.Equal(t, "approval_required", objectField(t, gate, "structuredContent")["code"])
	assert.Equal(t, 4, calls)
}

func TestProductionMCPJobWriteSchemasRequireIDsAndFences(t *testing.T) {
	readOnly := catalogMap(toolCatalog(false))
	assert.NotContains(t, readOnly, "finalize_production_draft")
	tools := catalogMap(toolCatalog(true))
	finalize := tools["finalize_production_draft"]
	require.NotNil(t, finalize)
	assertSchemaRejects(t, finalize.InputSchema, map[string]any{
		"set_id": "79000000-0000-4000-8000-0000000000e1", "revision": 1, "etag": 1,
		"operation_id": "79000000-0000-4000-8000-0000000000e2", "namespace_id": "bad",
		"snapshot_id": "79000000-0000-4000-8000-0000000000e6"})
	admit := tools["admit_production_job"]
	require.NotNil(t, admit)
	assertSchemaRejects(t, admit.InputSchema, map[string]any{
		"set_id": "79000000-0000-4000-8000-0000000000e1", "revision": 1,
		"operation_id": "79000000-0000-4000-8000-0000000000e3", "job_id": "79000000-0000-4000-8000-0000000000e7"})
	cancel := tools["cancel_production_job"]
	require.NotNil(t, cancel)
	assertSchemaRejects(t, cancel.InputSchema, map[string]any{
		"set_id": "79000000-0000-4000-8000-0000000000e1", "etag": 1,
		"operation_id": "79000000-0000-4000-8000-0000000000e4", "job_id": "bad"})
}
