package store_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/docbank/internal/store"
)

// This proof uses a real CLI daemon and a synthetic, already published package.
// It is opt-in because it compiles and starts a separate process.
func TestProductionPackageRealDaemonRestartKeepsArchiveButRevokesTicket(t *testing.T) {
	if os.Getenv("DOCBANK_PRODUCTION_PACKAGE_REAL_DAEMON") != "1" {
		t.Skip("opt-in real daemon production package proof")
	}
	vault, root, job, retained := store.PublishedProductionPackageHTTPFixture(t)
	require.NoError(t, vault.Close())
	name := "docbank"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	binary := filepath.Join(t.TempDir(), name)
	build := exec.CommandContext(t.Context(), "go", "build", "-tags", "fts5", "-o", binary,
		"go.kenn.io/docbank/cmd/docbank")
	output, err := build.CombinedOutput()
	require.NoError(t, err, string(output))
	stop := func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_, err := daemonconn.Stop(ctx, root)
		require.NoError(t, err)
	}
	t.Cleanup(stop)
	start := func() (*daemonconn.Connection, string, string) {
		t.Helper()
		cmd := exec.CommandContext(t.Context(), binary, "daemon", "start")
		cmd.Env = append(os.Environ(), "DOCBANK_HOME="+root)
		output, err := cmd.CombinedOutput()
		require.NoError(t, err, string(output))
		record, _, found, err := daemonconn.Find(t.Context(), root)
		require.NoError(t, err)
		require.True(t, found)
		key := record.Metadata["api_key"]
		require.NotEmpty(t, key)
		return daemonconn.New("http://"+record.Address, key), "http://" + record.Address, key
	}
	requestTicket := func(base, key string) (int, api.ProductionPackageDownloadTicket) {
		t.Helper()
		path := "/api/v1/productions/jobs/" + job.ID + "/packages/" + retained.Evidence.ID + "/download"
		request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, base+path,
			bytes.NewBufferString("{}"))
		require.NoError(t, err)
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("X-Api-Key", key)
		response, err := http.DefaultClient.Do(request)
		require.NoError(t, err)
		defer func() { require.NoError(t, response.Body.Close()) }()
		if response.StatusCode != http.StatusOK {
			return response.StatusCode, api.ProductionPackageDownloadTicket{}
		}
		var ticket api.ProductionPackageDownloadTicket
		require.NoError(t, json.UnmarshalRead(response.Body, &ticket))
		return response.StatusCode, ticket
	}
	_, firstBase, firstKey := start()
	status, oldTicket := requestTicket(firstBase, firstKey)
	require.Equal(t, http.StatusOK, status)
	require.Equal(t, retained.Archive.Version.BlobHash, oldTicket.ArchiveSHA256)
	stop()
	client, secondBase, secondKey := start()
	staleRequest, err := http.NewRequestWithContext(t.Context(), http.MethodGet,
		secondBase+oldTicket.URL, nil)
	require.NoError(t, err)
	staleRequest.Header.Set("X-Api-Key", secondKey)
	staleResponse, err := http.DefaultClient.Do(staleRequest)
	require.NoError(t, err)
	require.Equal(t, http.StatusNotFound, staleResponse.StatusCode)
	require.NoError(t, staleResponse.Body.Close())
	var archive bytes.Buffer
	newTicket, err := client.DownloadProductionPackageTo(t.Context(), job.ID, retained.Evidence.ID, &archive)
	require.NoError(t, err)
	require.Equal(t, retained.Archive.Version.BlobHash, newTicket.ArchiveSHA256)
	require.Equal(t, retained.Archive.Version.Size, int64(archive.Len()))
	sum := sha256.Sum256(archive.Bytes())
	require.Equal(t, newTicket.ArchiveSHA256, hex.EncodeToString(sum[:]))
	staging := filepath.Join(root, "web-downloads")
	require.Eventually(t, func() bool {
		entries, readErr := os.ReadDir(staging)
		return readErr == nil && len(entries) == 0
	}, 5*time.Second, 10*time.Millisecond, "the consumed ticket must release private staging")
	archivePath := filepath.Join(root, "blobs", newTicket.ArchiveSHA256[:2], newTicket.ArchiveSHA256)
	require.NoError(t, os.Remove(archivePath))
	status, _ = requestTicket(secondBase, secondKey)
	require.Equal(t, http.StatusInternalServerError, status)
	entries, err := os.ReadDir(staging)
	require.NoError(t, err)
	require.Empty(t, entries)
}
