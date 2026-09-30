package exporter_test

import (
	"encoding/json/v2"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"uuid"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document/bundle"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/store"
)

func TestReleaseRacesArchiveLease(t *testing.T) {
	w, s, job, _, _ := workerFixture(t, api.NewOperationGate())
	_, err := w.RunOne(t.Context())
	require.NoError(t, err)
	start := make(chan struct{})
	released := make(chan error, 1)
	go func() {
		<-start
		released <- w.Release(t.Context(), "master", job.ID)
	}()
	close(start)
	file, _, unlease, leaseErr := w.Lease(t.Context(), "master", job.ID)
	releaseErr := <-released
	if errors.Is(leaseErr, store.ErrNotFound) {
		require.NoError(t, releaseErr)
	} else {
		require.NoError(t, leaseErr)
		t.Cleanup(unlease)
		require.ErrorIs(t, releaseErr, bundle.ErrRetained)
		_, err := file.Stat()
		require.NoError(t, err, "a winning lease keeps its open archive")
		unlease()
		require.NoError(t, w.Release(t.Context(), "master", job.ID))
	}
	_, err = s.ExportJob(t.Context(), "master", job.ID)
	require.ErrorIs(t, err, store.ErrNotFound)
}

func TestReleaseReclaimsCapacityWithoutRemovingLeasedArchives(t *testing.T) {
	w, s, first, driver, dbPath := workerFixture(t, api.NewOperationGate())
	require.ErrorIs(t, w.Release(t.Context(), "master", first.ID), bundle.ErrConflict)
	_, err := w.RunOne(t.Context())
	require.NoError(t, err)
	request := bundle.JobRequest{OperationID: uuid.New().String(), PlanID: first.PlanID, Fingerprint: first.Fingerprint}
	second, err := s.QueueExportJob(t.Context(), "master", request)
	require.NoError(t, err)
	_, err = w.RunOne(t.Context())
	require.NoError(t, err)
	second, err = s.ExportJob(t.Context(), "master", second.ID)
	require.NoError(t, err)
	request.OperationID = uuid.New().String()
	_, err = s.QueueExportJob(t.Context(), "master", request)
	require.ErrorIs(t, err, bundle.ErrLimit)

	_, _, release, err := w.Lease(t.Context(), "master", first.ID)
	require.NoError(t, err)
	t.Cleanup(release)
	require.ErrorIs(t, w.Release(t.Context(), "other", first.ID), store.ErrNotFound)
	require.ErrorIs(t, w.Release(t.Context(), "master", first.ID), bundle.ErrRetained)
	archive := filepath.Join(filepath.Dir(dbPath), "export-archives", first.ID+".zip")
	require.FileExists(t, archive)
	release()
	require.NoError(t, w.Release(t.Context(), "master", first.ID))
	require.NoFileExists(t, archive)
	_, err = s.ExportJob(t.Context(), "master", first.ID)
	require.ErrorIs(t, err, store.ErrNotFound)
	require.ErrorIs(t, w.Release(t.Context(), "master", first.ID), store.ErrNotFound)
	var retainedUntil string
	require.NoError(t, driver.dbs[0].QueryRow(`SELECT expires_at FROM export_plans WHERE id=?`, first.PlanID).Scan(&retainedUntil))
	require.Equal(t, second.ExpiresAt, retainedUntil, "the other completed job keeps its retention")

	third, err := s.QueueExportJob(t.Context(), "master", request)
	require.NoError(t, err)
	_, err = w.RunOne(t.Context())
	require.NoError(t, err)
	third, err = s.ExportJob(t.Context(), "master", third.ID)
	require.NoError(t, err)
	require.Equal(t, "completed", third.State, "source bytes must still build another archive")
	plan, err := s.ExportPlan(t.Context(), "master", first.PlanID)
	require.NoError(t, err)
	require.NoError(t, w.Release(t.Context(), "master", second.ID))
	require.NoError(t, w.Release(t.Context(), "master", third.ID))
	require.NoError(t, driver.dbs[0].QueryRow(`SELECT expires_at FROM export_plans WHERE id=?`, first.PlanID).Scan(&retainedUntil))
	require.Equal(t, plan.ExpiresAt, retainedUntil, "release restores the original admission deadline")
}

func TestReleaseRetriesAfterArchiveRemovalAndStoreFailure(t *testing.T) {
	w, s, job, driver, dbPath := workerFixture(t, api.NewOperationGate())
	_, err := w.RunOne(t.Context())
	require.NoError(t, err)
	db := driver.dbs[0]
	_, err = db.Exec(`CREATE TRIGGER fail_release BEFORE DELETE ON export_jobs BEGIN SELECT RAISE(ABORT, 'synthetic delete failure'); END`)
	require.NoError(t, err)
	require.ErrorContains(t, w.Release(t.Context(), "master", job.ID), "synthetic delete failure")
	require.NoFileExists(t, filepath.Join(filepath.Dir(dbPath), "export-archives", job.ID+".zip"))
	_, err = s.ExportJob(t.Context(), "master", job.ID)
	require.NoError(t, err, "a failed transaction leaves a retryable job")
	_, _, unlease, err := w.Lease(t.Context(), "master", job.ID)
	if unlease != nil {
		t.Cleanup(unlease)
	}
	require.ErrorIs(t, err, bundle.ErrExpired)
	_, err = db.Exec(`DROP TRIGGER fail_release`)
	require.NoError(t, err)
	require.NoError(t, w.Release(t.Context(), "master", job.ID))
}

func TestReleaseTerminalAndExpiredJobs(t *testing.T) {
	for _, state := range []string{"canceled", "failed", "expired"} {
		t.Run(state, func(t *testing.T) {
			w, s, job, driver, _ := workerFixture(t, api.NewOperationGate())
			switch state {
			case "canceled":
				require.NoError(t, s.CancelExportJob(t.Context(), "master", job.ID))
			case "failed":
				claim, err := s.ClaimExportJob(t.Context())
				require.NoError(t, err)
				require.NoError(t, s.FinishExportJob(t.Context(), claim, nil, "", "synthetic failure"))
			case "expired":
				_, err := w.RunOne(t.Context())
				require.NoError(t, err)
				job, err = s.ExportJob(t.Context(), "master", job.ID)
				require.NoError(t, err)
				job.ExpiresAt = "2000-01-01T00:00:00.000000000Z"
				raw, err := json.Marshal(job)
				require.NoError(t, err)
				_, err = driver.dbs[0].Exec(`UPDATE export_jobs SET expires_at=?,canonical_json=? WHERE id=?`, job.ExpiresAt, raw, job.ID)
				require.NoError(t, err)
			}
			require.NoError(t, w.Release(t.Context(), "master", job.ID))
			_, err := s.ExportJob(t.Context(), "master", job.ID)
			require.ErrorIs(t, err, store.ErrNotFound)
		})
	}
}

func TestReleasePreservesJobWhenArchiveRemovalFails(t *testing.T) {
	w, s, job, _, dbPath := workerFixture(t, api.NewOperationGate())
	require.NoError(t, s.CancelExportJob(t.Context(), "master", job.ID))
	archive := filepath.Join(filepath.Dir(dbPath), "export-archives", job.ID+".zip")
	require.NoError(t, os.MkdirAll(archive, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(archive, "synthetic-obstruction"), []byte("keep"), 0o600))
	require.Error(t, w.Release(t.Context(), "master", job.ID))
	retained, err := s.ExportJob(t.Context(), "master", job.ID)
	require.NoError(t, err)
	require.Equal(t, "canceled", retained.State)
}
