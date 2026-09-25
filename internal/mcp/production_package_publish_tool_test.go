package mcp

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
)

func TestProductionMCPPackagePublishUsesVerifiedDaemonReceipt(t *testing.T) {
	const jobID = "79000000-0000-4000-8000-0000000000f1"
	const operationID = "79000000-0000-4000-8000-0000000000f2"
	const versionID = "79000000-0000-4000-8000-0000000000f3"
	digest := strings.Repeat("a", 64)
	var calls int
	daemon := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		calls++
		assert.Equal(t, http.MethodPost, request.Method)
		assert.Equal(t, "/api/v1/productions/jobs/"+jobID+"/packages", request.URL.Path)
		var body api.ProductionPackagePublishRequest
		if !assert.NoError(t, decodeDaemonJSON(request.Body, &body)) {
			return
		}
		assert.Equal(t, operationID, body.OperationID)
		assert.Equal(t, "export-dat-pdf-v1", body.ProfileID)
		if body.MaxVolumeDocuments == 2 {
			response.Header().Set("Content-Type", "application/problem+json")
			response.WriteHeader(http.StatusConflict)
			writeDaemonJSON(t, response, api.NewError(http.StatusConflict,
				"production_package_conflict", "synthetic private detail"))
			return
		}
		response.Header().Set("Content-Type", "application/json")
		response.WriteHeader(http.StatusCreated)
		writeDaemonJSON(t, response, api.ProductionPackagePublished{JobID: jobID,
			OperationID: operationID, ProfileID: body.ProfileID, VersionID: versionID,
			ArchiveSHA256: digest, EvidenceSHA256: digest, Size: 1024})
	}))
	t.Cleanup(daemon.Close)
	server := newBatesToolTestServer(t, daemon.URL, true)
	args := map[string]any{"job_id": jobID, "operation_id": operationID,
		"profile_id": "export-dat-pdf-v1", "max_volume_bytes": 1 << 30, "max_volume_documents": 10_000}
	published := objectField(t, callToolResult(t, server, "publish_production_package", args), "structuredContent")
	assert.Equal(t, versionID, published["version_id"])
	assert.Equal(t, digest, published["archive_sha256"])
	assert.Equal(t, "private", published["cacheScope"])
	assert.NotContains(t, published, "url")
	args["max_volume_documents"] = 2
	conflict := callToolResult(t, server, "publish_production_package", args)
	assert.Equal(t, true, conflict["isError"])
	assert.Equal(t, "production_package_conflict", objectField(t, conflict, "structuredContent")["code"])
	assert.Equal(t, 2, calls)
}

func TestProductionMCPPackagePublishSchemaBounds(t *testing.T) {
	readOnly := catalogMap(toolCatalog(false))
	assert.NotContains(t, readOnly, "publish_production_package")
	tool := catalogMap(toolCatalog(true))["publish_production_package"]
	require.NotNil(t, tool)
	args := map[string]any{"job_id": "79000000-0000-4000-8000-0000000000f1",
		"operation_id": "79000000-0000-4000-8000-0000000000f2", "profile_id": "export-dat-pdf-v1",
		"max_volume_bytes": 1 << 30, "max_volume_documents": 10_000}
	assertSchemaAccepts(t, tool.InputSchema, args)
	args["max_volume_bytes"] = int64(50<<30) + 1
	assertSchemaRejects(t, tool.InputSchema, args)
	args["max_volume_bytes"] = 1 << 30
	args["profile_id"] = "source-native"
	assertSchemaRejects(t, tool.InputSchema, args)
}
