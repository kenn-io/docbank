package mcp

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
)

func TestProductionPackageDownloadToolPublishesOnlyVerifiedBytes(t *testing.T) {
	const jobID = "49494949-4949-4494-8494-494949494949"
	const operationID = "50505050-5050-4505-8505-505050505050"
	archive := []byte("Synthetic retained archive bytes.")
	hash := sha256.Sum256(archive)
	archiveSHA256 := hex.EncodeToString(hash[:])
	var downloads int
	corrupt := false
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "synthetic-key", r.Header.Get("X-Api-Key"))
		switch r.URL.Path {
		case "/api/v1/productions/jobs/" + jobID + "/packages/" + operationID + "/download":
			assert.Equal(t, http.MethodPost, r.Method)
			w.Header().Set("Content-Type", "application/json")
			assert.NoError(t, json.MarshalWrite(w, api.ProductionPackageDownloadTicket{
				URL:           "/api/daemon/web-download/file?ticket=synthetic",
				ArchiveSHA256: archiveSHA256, Size: int64(len(archive)),
				VersionID: "51515151-5151-4515-8515-515151515151",
			}))
		case "/api/daemon/web-download/file":
			assert.Equal(t, http.MethodGet, r.Method)
			downloads++
			if corrupt {
				_, _ = w.Write([]byte("Altered retained archive bytes."))
				return
			}
			_, _ = w.Write(archive)
		default:
			assert.Fail(t, "unexpected path", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(daemon.Close)
	require.Nil(t, catalogMap(toolCatalog(false))["download_production_package"])
	tool := catalogMap(toolCatalog(true))["download_production_package"]
	require.NotNil(t, tool)
	server := newBatesToolTestServer(t, daemon.URL, true)
	dir := t.TempDir()
	destination := filepath.Join(dir, "production.zip")
	args := map[string]any{"job_id": jobID, "operation_id": operationID,
		"destination_path": destination, "overwrite": false}
	first := callToolResult(t, server, "download_production_package", args)
	summary := objectField(t, first, "structuredContent")
	require.Equal(t, destination, summary["destination_path"])
	require.Equal(t, archiveSHA256, summary["archive_sha256"])
	require.Equal(t, "published", summary["state"])
	content, err := os.ReadFile(destination)
	require.NoError(t, err)
	require.Equal(t, archive, content)
	encoded, err := json.Marshal(first)
	require.NoError(t, err)
	require.NotContains(t, string(encoded), "?ticket=")
	require.Equal(t, 1, downloads)
	again := callToolResult(t, server, "download_production_package", args)
	require.Equal(t, true, again["isError"])
	require.Equal(t, "destination_exists", objectField(t, again, "structuredContent")["code"])
	require.Equal(t, 1, downloads)
	corrupt = true
	badDestination := filepath.Join(dir, "corrupt.zip")
	args["destination_path"] = badDestination
	bad := callToolResult(t, server, "download_production_package", args)
	require.Equal(t, true, bad["isError"])
	require.Equal(t, "package_download_failed", objectField(t, bad, "structuredContent")["code"])
	require.NoFileExists(t, badDestination)
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	assertSchemaAccepts(t, tool.InputSchema, args)
	args["job_id"] = "not-a-uuid"
	assertSchemaRejects(t, tool.InputSchema, args)
}
