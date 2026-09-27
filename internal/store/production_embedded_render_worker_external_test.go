package store_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/store"
)

func TestEmbeddedProductionWorkerRendersAdmittedJobAndPublishesPackage(t *testing.T) {
	storeVault, root, setID, revision, etag, namespaceID := store.ProductionRenderDaemonHTTPFixture(t)
	require.NoError(t, storeVault.Close())
	first, err := docbank.New(t.Context(), docbank.Config{Root: root})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, first.Close()) })
	second, err := docbank.New(t.Context(), docbank.Config{Root: t.TempDir()})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, second.Close()) })
	finalized, err := first.FinalizeProductionDraft(t.Context(), "synthetic-operator", setID,
		revision, etag, api.ProductionFinalizeRequest{
			OperationID: "79000000-0000-4000-8000-000000000011",
			NamespaceID: namespaceID, SnapshotID: "79000000-0000-4000-8000-000000000012",
		})
	require.NoError(t, err)
	require.Equal(t, "finalized", finalized.Draft.State)
	jobRequest := api.ProductionJobAdmissionRequest{JobID: "79000000-0000-4000-8000-000000000013",
		OperationID: "79000000-0000-4000-8000-000000000014"}
	admitted, err := first.AdmitProductionJob(t.Context(), setID, revision, etag, jobRequest)
	require.NoError(t, err)
	require.Equal(t, jobRequest.JobID, admitted.JobID)
	_, err = second.ProductionJobStatus(t.Context(), setID, jobRequest.JobID)
	require.ErrorIs(t, err, store.ErrNotFound)
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	var status docbank.ProductionJobStatus
	for {
		status, err = first.ProductionJobStatus(ctx, setID, jobRequest.JobID)
		require.NoError(t, err)
		if status.State == "succeeded" || status.State == "failed" || status.State == "canceled" {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatalf("embedded production job did not finish: last state %q: %v", status.State, ctx.Err())
		case <-tick.C:
		}
	}
	require.Equal(t, "succeeded", status.State)
	require.NotEmpty(t, status.ReceiptSHA256)
	request := docbank.ProductionPackagePublishRequest{OperationID: "79000000-0000-4000-8000-000000000015",
		ProfileID: "export-dat-opt-images-v1", MaxVolumeBytes: 50 << 20, MaxVolumeDocuments: 10}
	_, err = second.PublishProductionPackage(t.Context(), jobRequest.JobID, request)
	require.ErrorIs(t, err, store.ErrNotFound)
	published, err := first.PublishProductionPackage(t.Context(), jobRequest.JobID, request)
	require.NoError(t, err)
	var archive bytes.Buffer
	receipt, err := first.DownloadProductionPackageTo(t.Context(), jobRequest.JobID,
		request.OperationID, &archive)
	require.NoError(t, err)
	require.Equal(t, published.VersionID, receipt.VersionID)
	require.Equal(t, published.Size, int64(archive.Len()))
	sum := sha256.Sum256(archive.Bytes())
	require.Equal(t, published.ArchiveSHA256, hex.EncodeToString(sum[:]))
	require.NoError(t, first.Close())
	first, err = docbank.New(t.Context(), docbank.Config{Root: root})
	require.NoError(t, err)
	restored, err := first.ProductionJobStatus(t.Context(), setID, jobRequest.JobID)
	require.NoError(t, err)
	require.Equal(t, status, restored)
	replayed, err := first.PublishProductionPackage(t.Context(), jobRequest.JobID, request)
	require.NoError(t, err)
	require.Equal(t, published, replayed)
	var afterRestart bytes.Buffer
	_, err = first.DownloadProductionPackageTo(t.Context(), jobRequest.JobID,
		request.OperationID, &afterRestart)
	require.NoError(t, err)
	require.Equal(t, archive.Bytes(), afterRestart.Bytes())
}
