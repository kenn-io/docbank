package main

import (
	"context"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/jobs"
	"go.kenn.io/docbank/internal/processing"
	"go.kenn.io/docbank/internal/store"
)

func TestPackagePreflightMaintenanceExpiresReceiptsHourly(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		catalog, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
		require.NoError(t, err)
		defer func() { require.NoError(t, catalog.Close()) }()
		record := store.PackagePreflightRecord{
			PreflightID: "11111111-1111-4111-8111-111111111111", Owner: "synthetic", SourceKind: "root", SourceRef: "synthetic-root",
			ProfileSHA256: strings.Repeat("a", 64), MappingSHA256: strings.Repeat("b", 64),
			ManifestSHA256: strings.Repeat("c", 64), ManifestBlobSHA256: strings.Repeat("c", 64),
			CanonicalJSON: []byte("{}"), DiagnosticsJSON: []byte("[]"),
			CreatedAt: time.Now().UTC().Format("2006-01-02T15:04:05.000000000Z07:00"), ExpiresAt: time.Now().UTC().Add(30 * time.Minute).Format("2006-01-02T15:04:05.000000000Z07:00"),
		}
		_, err = catalog.PutPackagePreflight(t.Context(), record)
		require.NoError(t, err)
		fresh := record
		fresh.PreflightID = "22222222-2222-4222-8222-222222222222"
		fresh.ExpiresAt = time.Now().UTC().Add(24 * time.Hour).Format("2006-01-02T15:04:05.000000000Z07:00")
		_, err = catalog.PutPackagePreflight(t.Context(), fresh)
		require.NoError(t, err)
		logger := slog.New(slog.DiscardHandler)
		supervisor := jobs.New(t.Context(), logger)
		defer func() { require.NoError(t, supervisor.Shutdown(context.Background())) }()
		require.NoError(t, startProcessingJobs(supervisor, catalog, nil, t.TempDir(), processing.NewRenditionRuntimeRegistry(), api.NewOperationGate(), logger))
		synctest.Wait()
		_, err = catalog.PackagePreflight(t.Context(), record.Owner, record.PreflightID)
		require.NoError(t, err)
		time.Sleep(time.Hour + time.Second)
		synctest.Wait()
		_, err = catalog.PackagePreflight(t.Context(), record.Owner, record.PreflightID)
		require.ErrorIs(t, err, store.ErrNotFound)
		// Reusing the primary key proves maintenance deleted the expired row.
		_, err = catalog.PutPackagePreflight(t.Context(), record)
		require.NoError(t, err)
		_, err = catalog.PackagePreflight(t.Context(), fresh.Owner, fresh.PreflightID)
		require.NoError(t, err)
	})
}
