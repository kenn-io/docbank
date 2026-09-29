package main

import (
	"errors"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/daemonconn"
)

var (
	migrationCatalogPath string
	migrationVaultRoot   string
	migrationArchiveRoot string
	migrationOutputDir   string
)

var photosMigrateCmd = &cobra.Command{
	Use:   "migrate",
	Short: "Inventory photo migration sources",
	Args:  cobra.NoArgs,
	RunE:  func(cmd *cobra.Command, _ []string) error { return cmd.Help() },
}

var migrateFotobankCmd = &cobra.Command{
	Use:   "fotobank",
	Short: "Inventory a Fotobank source",
	Args:  cobra.NoArgs,
	RunE:  func(cmd *cobra.Command, _ []string) error { return cmd.Help() },
}

var migrateFotobankInventoryCmd = &cobra.Command{
	Use:   "inventory",
	Short: "Inventory a stopped Fotobank install or recovery archive",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		request, err := migrationInventoryRequest()
		if err != nil {
			return usageError(err)
		}
		connection, err := daemonconn.Ensure(cmd.Context())
		if err != nil {
			return err
		}
		inventory, err := connection.CreateFotobankInventory(cmd.Context(), request)
		if err != nil {
			return err
		}
		return writeCLIJSON(cmd.OutOrStdout(), inventory)
	},
}

func migrationInventoryRequest() (api.FotobankInventoryRequest, error) {
	install := migrationCatalogPath != "" || migrationVaultRoot != ""
	archive := migrationArchiveRoot != ""
	if install == archive {
		return api.FotobankInventoryRequest{}, errors.New("choose --catalog-path and --vault-root, or --archive-root")
	}
	if install && (migrationCatalogPath == "" || migrationVaultRoot == "") {
		return api.FotobankInventoryRequest{}, errors.New("--catalog-path and --vault-root are both required for an install")
	}
	if archive && migrationCatalogPath != "" || archive && migrationVaultRoot != "" {
		return api.FotobankInventoryRequest{}, errors.New("archive inventory cannot include install paths")
	}
	if strings.TrimSpace(migrationOutputDir) == "" {
		return api.FotobankInventoryRequest{}, errors.New("--output-dir is required")
	}
	if !filepath.IsAbs(migrationOutputDir) {
		return api.FotobankInventoryRequest{}, errors.New("--output-dir must be absolute")
	}
	if install && (!filepath.IsAbs(migrationCatalogPath) || !filepath.IsAbs(migrationVaultRoot)) {
		return api.FotobankInventoryRequest{}, errors.New("--catalog-path and --vault-root must be absolute")
	}
	if archive && !filepath.IsAbs(migrationArchiveRoot) {
		return api.FotobankInventoryRequest{}, errors.New("--archive-root must be absolute")
	}
	return api.FotobankInventoryRequest{
		CatalogPath: migrationCatalogPath, VaultRoot: migrationVaultRoot,
		ArchiveRoot: migrationArchiveRoot, OutputDir: migrationOutputDir,
	}, nil
}

func init() {
	migrateFotobankCmd.AddCommand(migrateFotobankInventoryCmd)
	photosMigrateCmd.AddCommand(migrateFotobankCmd)
	photosCmd.AddCommand(photosMigrateCmd)

	migrateFotobankInventoryCmd.Flags().StringVar(&migrationCatalogPath, "catalog-path", "", "absolute Fotobank catalog path")
	migrateFotobankInventoryCmd.Flags().StringVar(&migrationVaultRoot, "vault-root", "", "absolute embedded Docbank vault root")
	migrateFotobankInventoryCmd.Flags().StringVar(&migrationArchiveRoot, "archive-root", "", "absolute Fotobank recovery archive root")
	migrateFotobankInventoryCmd.Flags().StringVar(&migrationOutputDir, "output-dir", "", "absolute directory for report.json and owner-map.json")
}
