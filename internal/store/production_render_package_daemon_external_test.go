package store_test

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
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

// This opt-in proof uses the real CLI daemon, renderer, and a synthetic PDF.
func TestProductionRealDaemonRendersAndPublishesRecipientPackage(t *testing.T) {
	if os.Getenv("DOCBANK_PRODUCTION_RENDER_PACKAGE_REAL_DAEMON") != "1" {
		t.Skip("opt-in real daemon production render and package proof")
	}
	vault, root, setID, revision, etag, namespaceID := store.ProductionRenderDaemonHTTPFixture(t)
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
	first := start()
	finalized, err := first.FinalizeProductionDraft(t.Context(), setID, revision, etag,
		api.ProductionFinalizeRequest{OperationID: "79000000-0000-4000-8000-000000000001",
			NamespaceID: namespaceID, SnapshotID: "79000000-0000-4000-8000-000000000002"})
	require.NoError(t, err)
	require.Equal(t, "finalized", finalized.Draft.State)
	jobRequest := api.ProductionJobAdmissionRequest{JobID: "79000000-0000-4000-8000-000000000003",
		OperationID: "79000000-0000-4000-8000-000000000004"}
	admitted, err := first.AdmitProductionJob(t.Context(), setID, revision, etag, jobRequest)
	require.NoError(t, err)
	require.Equal(t, jobRequest.JobID, admitted.JobID)
	status := waitForRenderedProductionJob(t, first, setID, jobRequest.JobID)
	require.Equal(t, "succeeded", status.State)
	require.NotEmpty(t, status.ReceiptSHA256)
	request := api.ProductionPackagePublishRequest{OperationID: "79000000-0000-4000-8000-000000000005",
		ProfileID: "export-dat-opt-images-v1", MaxVolumeBytes: 50 << 20, MaxVolumeDocuments: 10}
	published, err := first.PublishProductionPackage(t.Context(), jobRequest.JobID, request)
	require.NoError(t, err)
	var archive bytes.Buffer
	ticket, err := first.DownloadProductionPackageTo(t.Context(), jobRequest.JobID, request.OperationID, &archive)
	require.NoError(t, err)
	require.Equal(t, published.VersionID, ticket.VersionID)
	require.Equal(t, published.Size, int64(archive.Len()))
	sum := sha256.Sum256(archive.Bytes())
	require.Equal(t, published.ArchiveSHA256, hex.EncodeToString(sum[:]))
	reader, err := zip.NewReader(bytes.NewReader(archive.Bytes()), int64(archive.Len()))
	require.NoError(t, err)
	require.NotEmpty(t, reader.File)
	stop()
	second := start()
	restored, err := second.ProductionJobStatus(t.Context(), setID, jobRequest.JobID)
	require.NoError(t, err)
	require.Equal(t, status, restored)
	replayed, err := second.PublishProductionPackage(t.Context(), jobRequest.JobID, request)
	require.NoError(t, err)
	require.Equal(t, published, replayed)
	var afterRestart bytes.Buffer
	_, err = second.DownloadProductionPackageTo(t.Context(), jobRequest.JobID, request.OperationID, &afterRestart)
	require.NoError(t, err)
	require.Equal(t, archive.Bytes(), afterRestart.Bytes())
}

func waitForRenderedProductionJob(t *testing.T, client *daemonconn.Connection,
	setID, jobID string) api.ProductionJobStatus {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for {
		status, err := client.ProductionJobStatus(ctx, setID, jobID)
		require.NoError(t, err)
		if status.State == "succeeded" || status.State == "failed" || status.State == "canceled" {
			return status
		}
		select {
		case <-ctx.Done():
			t.Fatalf("production job did not finish: last state %q: %v", status.State, ctx.Err())
		case <-tick.C:
		}
	}
}
