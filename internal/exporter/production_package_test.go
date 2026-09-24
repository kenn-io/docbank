package exporter_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/production"
)

func TestProductionPackageWorkerCleansStagingWhenPublishedJobIsMissing(t *testing.T) {
	worker, catalog, job, driver, dbPath := workerFixture(t, api.NewOperationGate())
	require.NotNil(t, catalog)
	require.NotEmpty(t, job.ID)
	require.NotNil(t, driver)
	_, err := worker.PublishProductionPackage(t.Context(),
		"77777777-7777-4777-8777-777777777777",
		"88888888-8888-4888-8888-888888888888", "export-dat-pdf-v1",
		production.PackageLimits{MaxVolumeBytes: 1000, MaxVolumeDocuments: 10})
	require.Error(t, err)
	entries, err := os.ReadDir(filepath.Join(filepath.Dir(dbPath), "export-archives"))
	require.NoError(t, err)
	for _, entry := range entries {
		require.NotContains(t, entry.Name(), ".production-package-")
	}
}
