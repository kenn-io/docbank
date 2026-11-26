package processing_test

import (
	"archive/zip"
	"io"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document/bundle"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/pgstoretest"
	"go.kenn.io/docbank/internal/processing"
	"go.kenn.io/docbank/internal/store"
)

func postgresArchiveWorkerFixture(t *testing.T, gate processing.RenditionMutationGate) (*processing.ExportWorker, *store.Store, bundle.Job, string) {
	t.Helper()
	_, cfg, connection := pgstoretest.NewDatabase(t)
	root := t.TempDir()
	s, err := store.OpenPostgres(t.Context(), connection, cfg, root)
	require.NoError(t, err)
	w, job := workerFixtureWithStore(t, gate, s, root)
	return w, s, job, root
}

func TestArchiveWorkerPostgres(t *testing.T) {
	t.Parallel()
	for _, scenario := range []struct {
		name  string
		check func(*testing.T, archiveWorkerFixtureFactory)
	}{
		{"ReleaseRacesArchiveLease", checkReleaseRacesArchiveLease},
		{"ReleasePreservesJobWhenArchiveRemovalFails", checkReleasePreservesJobWhenArchiveRemovalFails},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			t.Parallel()
			scenario.check(t, postgresArchiveWorkerFixture)
		})
	}
	t.Run("PublishesVerifiedOriginal", func(t *testing.T) {
		t.Parallel()
		w, s, job, root := postgresArchiveWorkerFixture(t, api.NewOperationGate())
		processed, err := w.RunOne(t.Context())
		require.NoError(t, err)
		require.True(t, processed)
		stored, err := s.ExportJob(t.Context(), "master", job.ID)
		require.NoError(t, err)
		require.Equal(t, "completed", stored.State)
		file, receipt, unlease, err := w.Lease(t.Context(), "master", job.ID)
		require.NoError(t, err)
		t.Cleanup(unlease)
		require.Equal(t, *stored.Receipt, receipt)
		archive, err := zip.NewReader(file, receipt.Size)
		require.NoError(t, err)
		originals := 0
		for _, entry := range archive.File {
			if !strings.HasSuffix(entry.Name, "/original") {
				continue
			}
			originals++
			body, err := entry.Open()
			require.NoError(t, err)
			data, err := io.ReadAll(body)
			require.NoError(t, err)
			require.NoError(t, body.Close())
			require.Equal(t, "synthetic original\n", string(data))
		}
		require.Equal(t, 1, originals)
		require.ErrorIs(t, w.Release(t.Context(), "master", job.ID), bundle.ErrRetained)
		unlease()
		require.NoError(t, w.Release(t.Context(), "master", job.ID))
		_, err = s.ExportJob(t.Context(), "master", job.ID)
		require.ErrorIs(t, err, store.ErrNotFound)
		require.NoFileExists(t, filepath.Join(root, processing.ExportArchiveDirectory, job.ID+".zip"))
	})
}
