package store_test

import (
	"bytes"
	"context"
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

// This proof starts the real daemon over a synthetic successful render. It is
// opt-in because every invocation compiles and spawns a separate process.
func TestProductionPackagePublicationRealDaemonPersistsAcrossRestart(t *testing.T) {
	if os.Getenv("DOCBANK_PRODUCTION_PUBLISH_REAL_DAEMON") != "1" {
		t.Skip("opt-in real daemon production package publication proof")
	}
	vault, root, job := store.PublishedProductionJobHTTPFixture(t)
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
	start := func() *daemonconn.Connection {
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
		return daemonconn.New("http://"+record.Address, key)
	}
	request := api.ProductionPackagePublishRequest{
		OperationID: "77777777-7777-4777-8777-777777777777",
		ProfileID:   "export-dat-opt-images-v1", MaxVolumeBytes: 50 << 20,
		MaxVolumeDocuments: 10,
	}
	first := start()
	published, err := first.PublishProductionPackage(t.Context(), job.ID, request)
	require.NoError(t, err)
	require.Equal(t, job.ID, published.JobID)
	var archive bytes.Buffer
	ticket, err := first.DownloadProductionPackageTo(t.Context(), job.ID, request.OperationID, &archive)
	require.NoError(t, err)
	require.Equal(t, published.ArchiveSHA256, ticket.ArchiveSHA256)
	require.Equal(t, published.Size, int64(archive.Len()))
	stop()
	second := start()
	replayed, err := second.PublishProductionPackage(t.Context(), job.ID, request)
	require.NoError(t, err)
	require.Equal(t, published, replayed)
	var afterRestart bytes.Buffer
	_, err = second.DownloadProductionPackageTo(t.Context(), job.ID, request.OperationID, &afterRestart)
	require.NoError(t, err)
	require.Equal(t, archive.Bytes(), afterRestart.Bytes())
}
