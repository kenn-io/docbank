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
	require.Nil(t, migrateFotobankInventoryCmd.Flags().Lookup("owner-map"))
	setupVaultHome(t)
	fixture, err := fotobanktest.CreateInstall(filepath.Join(t.TempDir(), "fotobank"), store.DefaultSQLiteDriver())
	require.NoError(t, err)
	ownerMap := filepath.Join(t.TempDir(), "owner-map.json")
	out, err := runCLI(t, "photos", "migrate", "fotobank", "inventory",
		"--catalog-path", fixture.CatalogPath, "--vault-root", fixture.VaultRoot,
		"--owner-map-path", ownerMap)
	require.NoError(t, err)
	var run api.MigrationRun
	require.NoError(t, json.Unmarshal([]byte(out), &run))
	require.NotEmpty(t, run.ID)
	require.Equal(t, int64(1), run.Report.Counts.Owners)
	require.Equal(t, int64(1), run.Report.Counts.AlbumMemberships)
	require.Equal(t, int64(1), run.Report.Counts.CheckoutEntries)

	out, err = runCLI(t, "photos", "migrate", "runs", "show", run.ID)
	require.NoError(t, err)
	var shown api.MigrationRun
	require.NoError(t, json.Unmarshal([]byte(out), &shown))
	require.Equal(t, run.ID, shown.ID)
	require.Equal(t, int64(1), shown.Report.Counts.AlbumMemberships)
	require.Equal(t, int64(1), shown.Report.Counts.CheckoutEntries)

	out, err = runCLI(t, "photos", "migrate", "runs", "list", "--limit", "1")
	require.NoError(t, err)
	var page api.MigrationRunPage
	require.NoError(t, json.Unmarshal([]byte(out), &page))
	require.Equal(t, 1, page.Total)
	require.Len(t, page.Items, 1)
	require.Equal(t, run.ID, page.Items[0].ID)
	require.Equal(t, int64(1), page.Items[0].Report.Counts.AlbumMemberships)
	require.Equal(t, int64(1), page.Items[0].Report.Counts.CheckoutEntries)
}

func TestMigrationInventoryFlagsRequireAbsolutePaths(t *testing.T) {
	previousCatalog, previousVault := migrationCatalogPath, migrationVaultRoot
	previousArchive, previousOwnerMap := migrationArchiveRoot, migrationOwnerMap
	t.Cleanup(func() {
		migrationCatalogPath, migrationVaultRoot = previousCatalog, previousVault
		migrationArchiveRoot, migrationOwnerMap = previousArchive, previousOwnerMap
	})
	absoluteOwnerMap := filepath.Join(t.TempDir(), "owner-map.json")
	for _, test := range []struct {
		name, catalog, vaultRoot, archiveRoot, ownerMap string
		message                                         string
	}{
		{name: "catalog", catalog: "catalog.sqlite", vaultRoot: filepath.VolumeName(t.TempDir()) + string(filepath.Separator) + "vault", ownerMap: absoluteOwnerMap, message: "--catalog-path and --vault-root must be absolute"},
		{name: "vault root", catalog: filepath.VolumeName(t.TempDir()) + string(filepath.Separator) + "catalog.sqlite", vaultRoot: "vault", ownerMap: absoluteOwnerMap, message: "--catalog-path and --vault-root must be absolute"},
		{name: "archive", archiveRoot: "archive", ownerMap: absoluteOwnerMap, message: "--archive-root must be absolute"},
		{name: "owner map", catalog: filepath.VolumeName(t.TempDir()) + string(filepath.Separator) + "catalog.sqlite", vaultRoot: filepath.VolumeName(t.TempDir()) + string(filepath.Separator) + "vault", ownerMap: "owner-map.json", message: "--owner-map-path must be absolute"},
	} {
		t.Run(test.name, func(t *testing.T) {
			migrationCatalogPath, migrationVaultRoot = test.catalog, test.vaultRoot
			migrationArchiveRoot, migrationOwnerMap = test.archiveRoot, test.ownerMap
			_, err := migrationInventoryRequest()
			require.ErrorContains(t, err, test.message)
		})
	}
}
