package api_test

import (
	"context"
	"encoding/json/v2"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
	"uuid"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/processing"
)

func admittedPackageWorkerJob(t *testing.T) (*testStore, api.PackageImportJob) {
	t.Helper()
	srv, catalog := newPackageTestServer(t)
	root := t.TempDir()
	volume := filepath.Join(root, "VOL001")
	require.NoError(t, os.Mkdir(volume, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(volume, "records.dat"),
		[]byte("þDOCIDþ\x14þNATIVEþ\r\nþDOC-Aþ\x14þA.txtþ\r\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(volume, "A.txt"), []byte("synthetic native document"), 0o600))
	response := srv.post(t, mustPackageJSON(t, api.PackagePreflightRequest{
		Profile: "dat-concordance-v1", Encoding: "utf-8", SourceKind: "root", SourceRef: root,
	}))
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	var preview api.PackagePreflight
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &preview))
	require.False(t, preview.Blocking)
	response = srv.call(t, http.MethodPost, "/api/v1/packages/imports", mustPackageJSON(t, api.PackageImportRequest{
		PreflightID: preview.PreflightID, Into: "/", Name: "synthetic-maintenance", OperationID: uuid.New().String(),
	}), nil)
	require.Equal(t, http.StatusAccepted, response.Code, response.Body.String())
	var job api.PackageImportJob
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &job))
	return catalog, job
}

func TestPackageImportWorkerClaimWaitsForMaintenance(t *testing.T) {
	catalog, job := admittedPackageWorkerJob(t)
	synctest.Test(t, func(t *testing.T) {
		gate := api.NewOperationGate()
		worker, err := processing.NewPackageImportWorker(processing.PackageImportConfig{
			Catalog: catalog.Store, Blobs: catalog.Blobs, Owner: "synthetic-worker", Mutate: gate.MutateContext,
		})
		require.NoError(t, err)
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		done := make(chan error, 1)
		require.NoError(t, gate.MaintainContext(t.Context(), func() error {
			go func() { _, err := worker.ProcessOnce(ctx); done <- err }()
			synctest.Wait()
			stored, err := catalog.PackageImportJob(t.Context(), "vault:"+catalog.VaultID(), job.OperationID)
			require.NoError(t, err)
			require.Equal(t, "queued", stored.State)
			require.Zero(t, stored.Epoch)
			cancel()
			synctest.Wait()
			require.ErrorIs(t, <-done, context.Canceled)
			return nil
		}))
		more, err := worker.ProcessOnce(t.Context())
		require.NoError(t, err)
		require.True(t, more)
		stored, err := catalog.PackageImportJob(t.Context(), "vault:"+catalog.VaultID(), job.OperationID)
		require.NoError(t, err)
		require.Equal(t, "complete", stored.State)
	})
}

func TestPackageImportWorkerRenewalAndReleaseWaitForMaintenance(t *testing.T) {
	for _, holdUntilTimeout := range []bool{false, true} {
		name := "release_after_maintenance"
		if holdUntilTimeout {
			name = "bounded_release_wait"
		}
		t.Run(name, func(t *testing.T) {
			catalog, job := admittedPackageWorkerJob(t)
			synctest.Test(t, func(t *testing.T) {
				gate := api.NewOperationGate()
				directoryReady, proceed := make(chan struct{}), make(chan struct{})
				var paused atomic.Bool
				worker, err := processing.NewPackageImportWorker(processing.PackageImportConfig{
					Catalog: catalog.Store, Blobs: catalog.Blobs, Owner: "synthetic-worker", LeaseDuration: time.Minute,
					Mutate: func(ctx context.Context, fn func() error) error {
						if err := gate.MutateContext(ctx, fn); err != nil {
							return err
						}
						// Pause after the package directory exists, before processing
						// its first record. Maintenance can acquire the real gate here.
						if _, err := catalog.NodeByPath(ctx, "/"+job.PackageID); err == nil && paused.CompareAndSwap(false, true) {
							close(directoryReady)
							<-proceed
						}
						return nil
					},
				})
				require.NoError(t, err)
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				done := make(chan error, 1)
				go func() { _, err := worker.ProcessOnce(ctx); done <- err }()
				<-directoryReady
				claimed, err := catalog.PackageImportJob(t.Context(), "vault:"+catalog.VaultID(), job.OperationID)
				require.NoError(t, err)
				require.Equal(t, "running", claimed.State)
				require.NoError(t, gate.MaintainContext(t.Context(), func() error {
					time.Sleep(time.Second)
					close(proceed)
					synctest.Wait()
					record, err := catalog.PackageImportJob(t.Context(), claimed.Owner, job.OperationID)
					require.NoError(t, err)
					require.Equal(t, claimed.LeaseExpiresAt, record.LeaseExpiresAt, "record renewal must wait")
					time.Sleep(20 * time.Second)
					synctest.Wait()
					periodic, err := catalog.PackageImportJob(t.Context(), claimed.Owner, job.OperationID)
					require.NoError(t, err)
					require.Equal(t, claimed.LeaseExpiresAt, periodic.LeaseExpiresAt, "periodic renewal must wait")
					cancel()
					synctest.Wait()
					if holdUntilTimeout {
						time.Sleep(5 * time.Second)
						synctest.Wait()
						err := <-done
						require.ErrorIs(t, err, context.Canceled)
						require.ErrorIs(t, err, context.DeadlineExceeded)
					}
					waiting, err := catalog.PackageImportJob(t.Context(), claimed.Owner, job.OperationID)
					require.NoError(t, err)
					require.Equal(t, "running", waiting.State, "release must wait")
					require.Equal(t, claimed.Epoch, waiting.Epoch)
					return nil
				}))
				if !holdUntilTimeout {
					require.ErrorIs(t, <-done, context.Canceled)
					released, err := catalog.PackageImportJob(t.Context(), claimed.Owner, job.OperationID)
					require.NoError(t, err)
					require.Equal(t, "queued", released.State)
					require.Greater(t, released.Epoch, claimed.Epoch)
				}
			})
		})
	}
}
