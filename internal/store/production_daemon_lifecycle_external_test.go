package store_test

import (
	"bytes"
	"context"
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

// This proof uses the CLI daemon and a synthetic, fully reviewed vault. It is
// opt-in because it compiles and starts a separate process.
func TestProductionRealDaemonFinalizationSurvivesRestartAndRotatesKey(t *testing.T) {
	if os.Getenv("DOCBANK_PRODUCTION_REAL_DAEMON") != "1" {
		t.Skip("opt-in real daemon production proof")
	}
	vault, root, setID, revision, etag, namespaceID := store.ProductionFinalizeHTTPFixture(t)
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
	first, _, firstKey := start()
	request := api.ProductionFinalizeRequest{
		OperationID: "75000000-0000-4000-8000-000000000070",
		NamespaceID: namespaceID,
		SnapshotID:  "75000000-0000-4000-8000-000000000071",
	}
	finalized, err := first.FinalizeProductionDraft(t.Context(), setID, revision, etag, request)
	require.NoError(t, err)
	require.Equal(t, "finalized", finalized.Draft.State)
	require.NotEmpty(t, finalized.ReceiptSHA256)
	stop()
	second, base, secondKey := start()
	require.NotEqual(t, firstKey, secondKey, "the runtime API key must rotate on restart")
	path := "/api/v1/productions/sets/" + setID
	unauthorized, err := http.NewRequestWithContext(t.Context(), http.MethodGet, base+path, nil)
	require.NoError(t, err)
	unauthorized.Header.Set("X-Api-Key", firstKey)
	response, err := http.DefaultClient.Do(unauthorized)
	require.NoError(t, err)
	require.Equal(t, http.StatusUnauthorized, response.StatusCode)
	require.NoError(t, response.Body.Close())
	draft, err := second.ProductionDraft(t.Context(), setID, revision)
	require.NoError(t, err)
	require.Equal(t, finalized.Draft, draft)
	replayed, err := second.FinalizeProductionDraft(t.Context(), setID, revision, etag, request)
	require.NoError(t, err)
	require.Equal(t, finalized, replayed)
	jobRequest := api.ProductionJobAdmissionRequest{
		JobID: "75000000-0000-4000-8000-000000000072", OperationID: "75000000-0000-4000-8000-000000000073",
	}
	admitted, err := second.AdmitProductionJob(t.Context(), setID, revision, etag, jobRequest)
	require.NoError(t, err)
	require.Equal(t, jobRequest.JobID, admitted.JobID)
	status, err := second.ProductionJobStatus(t.Context(), setID, jobRequest.JobID)
	require.NoError(t, err)
	require.Equal(t, admitted.RevisionSHA256, status.RevisionSHA256)
	canceled, err := second.CancelProductionJob(t.Context(), setID, jobRequest.JobID, etag,
		api.ProductionJobCancelRequest{OperationID: "75000000-0000-4000-8000-000000000074"})
	require.NoError(t, err)
	require.Equal(t, setID, canceled.SetID)
	status, err = second.ProductionJobStatus(t.Context(), setID, jobRequest.JobID)
	require.NoError(t, err)
	require.Equal(t, "canceled", status.State)
	var archive bytes.Buffer
	_, err = second.DownloadProductionPackageTo(t.Context(), jobRequest.JobID,
		"75000000-0000-4000-8000-000000000075", &archive)
	require.Error(t, err, "a canceled job has no downloadable package")
	require.Zero(t, archive.Len())
}
