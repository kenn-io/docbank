package mcp

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
)

func TestProductionMCPDownloadVerifiesBeforePublishingLocalFile(t *testing.T) {
	const jobID = "79000000-0000-4000-8000-000000000101"
	const operationID = "79000000-0000-4000-8000-000000000102"
	const badOperationID = "79000000-0000-4000-8000-000000000103"
	const versionID = "79000000-0000-4000-8000-000000000104"
	const ticketID = "79000000-0000-4000-8000-000000000105"
	archive := []byte("synthetic verified archive bytes")
	digest := sha256.Sum256(archive)
	expectedHash := hex.EncodeToString(digest[:])
	var calls int
	daemon := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		calls++
		if request.URL.Path == "/api/daemon/web-download/file" {
			assert.Equal(t, http.MethodGet, request.Method)
			assert.Equal(t, ticketID, request.URL.Query().Get("ticket"))
			_, err := response.Write(archive)
			assert.NoError(t, err)
			return
		}
		assert.Equal(t, http.MethodPost, request.Method)
		assert.True(t, strings.HasPrefix(request.URL.Path,
			"/api/v1/productions/jobs/"+jobID+"/packages/"))
		checksum := expectedHash
		if strings.Contains(request.URL.Path, badOperationID) {
			checksum = strings.Repeat("b", 64)
		}
		writeDaemonJSON(t, response, api.ProductionPackageDownloadTicket{
			URL:           "/api/daemon/web-download/file?ticket=" + ticketID,
			ArchiveSHA256: checksum, Size: int64(len(archive)), VersionID: versionID})
	}))
	t.Cleanup(daemon.Close)
	server := newBatesToolTestServer(t, daemon.URL, true)
	parent := t.TempDir()
	destination := filepath.Join(parent, "package.zip")
	args := map[string]any{"job_id": jobID, "operation_id": operationID,
		"destination_path": destination, "overwrite": false}
	result := objectField(t, callToolResult(t, server, "download_production_package", args), "structuredContent")
	assert.Equal(t, expectedHash, result["archive_sha256"])
	assert.Equal(t, versionID, result["version_id"])
	assert.Equal(t, "published", result["state"])
	assert.NotContains(t, result, "url")
	actual, err := os.ReadFile(destination)
	require.NoError(t, err)
	assert.Equal(t, archive, actual)
	before := calls
	blocked := exchangeRaw(t, server, requestFor("tools/call", map[string]any{
		"name": "download_production_package", "arguments": args,
	}))
	assert.NotZero(t, decodeWireError(t, blocked).Code)
	assert.Equal(t, before, calls)
	failedPath := filepath.Join(parent, "failed.zip")
	bad := callToolResult(t, server, "download_production_package", map[string]any{
		"job_id": jobID, "operation_id": badOperationID,
		"destination_path": failedPath, "overwrite": false})
	assert.Equal(t, true, bad["isError"])
	_, err = os.Stat(failedPath)
	require.ErrorIs(t, err, os.ErrNotExist)
	staged, err := filepath.Glob(filepath.Join(parent, ".docbank-production-*"))
	require.NoError(t, err)
	assert.Empty(t, staged)
}

func TestProductionMCPDownloadSchemaRequiresDestination(t *testing.T) {
	tool := catalogMap(toolCatalog(true))["download_production_package"]
	require.NotNil(t, tool)
	assertSchemaRejects(t, tool.InputSchema, map[string]any{
		"job_id":       "79000000-0000-4000-8000-000000000101",
		"operation_id": "79000000-0000-4000-8000-000000000102", "overwrite": false})
}
