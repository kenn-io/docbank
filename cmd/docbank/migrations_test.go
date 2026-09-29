package main

import (
	"encoding/json/v2"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/fotobanktest"
	"go.kenn.io/docbank/internal/store"
)

func TestMigrationCLIUsesDaemon(t *testing.T) {
	require.Nil(t, migrateFotobankInventoryCmd.Flags().Lookup("owner-map-path"))
	setupVaultHome(t)
	fixture, err := fotobanktest.CreateInstall(filepath.Join(t.TempDir(), "fotobank"), store.DefaultSQLiteDriver())
	require.NoError(t, err)
	outputDir := filepath.Join(t.TempDir(), "inventory")
	out, err := runCLI(t, "photos", "migrate", "fotobank", "inventory",
		"--catalog-path", fixture.CatalogPath, "--vault-root", fixture.VaultRoot,
		"--output-dir", outputDir)
	require.NoError(t, err)
	var inventory api.FotobankInventory
	require.NoError(t, json.Unmarshal([]byte(out), &inventory))
	require.Equal(t, filepath.Join(outputDir, "report.json"), inventory.ReportPath)
	require.Equal(t, filepath.Join(outputDir, "owner-map.json"), inventory.OwnerMapPath)
	require.FileExists(t, inventory.ReportPath)
	require.FileExists(t, inventory.OwnerMapPath)
	require.Equal(t, int64(1), inventory.Report.Counts.Owners)
	require.Equal(t, int64(1), inventory.Report.Counts.AlbumMemberships)
	require.Equal(t, int64(1), inventory.Report.Counts.CheckoutEntries)
}

func TestMigrationInventoryFlagsRequireAbsolutePaths(t *testing.T) {
	previousCatalog, previousVault := migrationCatalogPath, migrationVaultRoot
	previousArchive, previousOutputDir := migrationArchiveRoot, migrationOutputDir
	t.Cleanup(func() {
		migrationCatalogPath, migrationVaultRoot = previousCatalog, previousVault
		migrationArchiveRoot, migrationOutputDir = previousArchive, previousOutputDir
	})
	absoluteOutputDir := filepath.Join(t.TempDir(), "inventory")
	for _, test := range []struct {
		name, catalog, vaultRoot, archiveRoot, outputDir string
		message                                          string
	}{
		{name: "catalog", catalog: "catalog.sqlite", vaultRoot: filepath.VolumeName(t.TempDir()) + string(filepath.Separator) + "vault", outputDir: absoluteOutputDir, message: "--catalog-path and --vault-root must be absolute"},
		{name: "vault root", catalog: filepath.VolumeName(t.TempDir()) + string(filepath.Separator) + "catalog.sqlite", vaultRoot: "vault", outputDir: absoluteOutputDir, message: "--catalog-path and --vault-root must be absolute"},
		{name: "archive", archiveRoot: "archive", outputDir: absoluteOutputDir, message: "--archive-root must be absolute"},
		{name: "output dir", catalog: filepath.VolumeName(t.TempDir()) + string(filepath.Separator) + "catalog.sqlite", vaultRoot: filepath.VolumeName(t.TempDir()) + string(filepath.Separator) + "vault", outputDir: "inventory", message: "--output-dir must be absolute"},
	} {
		t.Run(test.name, func(t *testing.T) {
			migrationCatalogPath, migrationVaultRoot = test.catalog, test.vaultRoot
			migrationArchiveRoot, migrationOutputDir = test.archiveRoot, test.outputDir
			_, err := migrationInventoryRequest()
			require.ErrorContains(t, err, test.message)
		})
	}
}
