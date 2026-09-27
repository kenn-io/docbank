package main

import (
	"bytes"
	"context"
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
	"go.kenn.io/docbank/internal/daemonconn"
)

func TestProductionPackageCLIDownloadsOnlyVerifiedRetainedBytes(t *testing.T) {
	const jobID = "46464646-4646-4464-8464-464646464646"
	const operationID = "47474747-4747-4474-8474-474747474747"
	archive := []byte("Synthetic verified production archive bytes.")
	hash := sha256.Sum256(archive)
	archiveSHA256 := hex.EncodeToString(hash[:])
	var downloads int
	corrupt := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "synthetic-api-key", r.Header.Get("X-Api-Key"))
		switch r.URL.Path {
		case "/api/v1/productions/jobs/" + jobID + "/packages/" + operationID + "/download":
			assert.Equal(t, http.MethodPost, r.Method)
			w.Header().Set("Content-Type", "application/json")
			assert.NoError(t, json.MarshalWrite(w, api.ProductionPackageDownloadTicket{
				URL:           "/api/daemon/web-download/file?ticket=synthetic",
				ArchiveSHA256: archiveSHA256, Size: int64(len(archive)),
				VersionID: "48484848-4848-4484-8484-484848484848",
			}))
		case "/api/daemon/web-download/file":
			assert.Equal(t, http.MethodGet, r.Method)
			downloads++
			if corrupt {
				_, _ = w.Write([]byte("Altered bytes with the same declared size!"))
				return
			}
			_, _ = w.Write(archive)
		default:
			assert.Fail(t, "unexpected path", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	ensure := func(ctx context.Context) (*daemonconn.Connection, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return daemonconn.New(server.URL, "synthetic-api-key"), nil
	}
	dir := t.TempDir()
	destination := filepath.Join(dir, "production.zip")
	command := func(path string) (*bytes.Buffer, error) {
		t.Helper()
		cmd := newProductionPackageDownloadCommandWithEnsure(ensure)
		var out bytes.Buffer
		cmd.SetOut(&out)
		cmd.SetArgs([]string{jobID, operationID, path})
		return &out, cmd.ExecuteContext(t.Context())
	}
	out, err := command(destination)
	require.NoError(t, err)
	require.Contains(t, out.String(), archiveSHA256)
	written, err := os.ReadFile(destination)
	require.NoError(t, err)
	require.Equal(t, archive, written)
	require.Equal(t, 1, downloads)
	machineDestination := filepath.Join(dir, "production-json.zip")
	jsonCommand := newProductionPackageDownloadCommandWithEnsure(ensure)
	var jsonOutput bytes.Buffer
	jsonCommand.SetOut(&jsonOutput)
	jsonCommand.SetArgs([]string{jobID, operationID, machineDestination, "--json"})
	require.NoError(t, jsonCommand.ExecuteContext(t.Context()))
	var receipt productionPackageDownloadReceipt
	require.NoError(t, json.Unmarshal(jsonOutput.Bytes(), &receipt))
	require.Equal(t, machineDestination, receipt.Destination)
	require.Equal(t, archiveSHA256, receipt.ArchiveSHA256)
	require.NotContains(t, jsonOutput.String(), "?ticket=")
	require.Equal(t, 2, downloads)
	_, err = command(destination)
	require.ErrorContains(t, err, "already exists")
	require.Equal(t, 2, downloads)
	corrupt = true
	badDestination := filepath.Join(dir, "corrupt.zip")
	_, err = command(badDestination)
	require.Error(t, err)
	require.NoFileExists(t, badDestination)
	require.Equal(t, 3, downloads)
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	require.Len(t, entries, 2, "private staging must be removed after verification failure")
}
