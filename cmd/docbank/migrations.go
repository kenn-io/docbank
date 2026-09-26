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
	migrationSnapshotID  string
	migrationOwnerMap    string
	migrationRunOffset   int
	migrationRunLimit    int
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
		run, err := connection.CreateFotobankInventory(cmd.Context(), request)
		if err != nil {
			return err
		}
		return writeCLIJSON(cmd.OutOrStdout(), run)
	},
}

var migrateRunsCmd = &cobra.Command{
	Use:   "runs",
	Short: "Read photo migration inventories",
	Args:  cobra.NoArgs,
	RunE:  func(cmd *cobra.Command, _ []string) error { return cmd.Help() },
}

var migrateRunsListCmd = &cobra.Command{
	Use:   "list",
	Short: "List completed migration inventories",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		if migrationRunOffset < 0 || migrationRunLimit < 1 || migrationRunLimit > 50 {
			return usageError(errors.New("--offset must be non-negative and --limit must be between 1 and 50"))
		}
		connection, err := daemonconn.Ensure(cmd.Context())
		if err != nil {
			return err
		}
		page, err := connection.ListMigrationRuns(cmd.Context(), migrationRunOffset, migrationRunLimit)
		if err != nil {
			return err
		}
		return writeCLIJSON(cmd.OutOrStdout(), page)
	},
}

var migrateRunsShowCmd = &cobra.Command{
	Use:   "show <run-id>",
	Short: "Show one completed migration inventory",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		connection, err := daemonconn.Ensure(cmd.Context())
		if err != nil {
			return err
		}
		run, err := connection.MigrationRun(cmd.Context(), args[0])
		if err != nil {
			return err
		}
		return writeCLIJSON(cmd.OutOrStdout(), run)
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
	if strings.TrimSpace(migrationOwnerMap) == "" {
		return api.FotobankInventoryRequest{}, errors.New("--owner-map-path is required")
	}
	if !filepath.IsAbs(migrationOwnerMap) {
		return api.FotobankInventoryRequest{}, errors.New("--owner-map-path must be absolute")
	}
	if install && (!filepath.IsAbs(migrationCatalogPath) || !filepath.IsAbs(migrationVaultRoot)) {
		return api.FotobankInventoryRequest{}, errors.New("--catalog-path and --vault-root must be absolute")
	}
	if archive && !filepath.IsAbs(migrationArchiveRoot) {
		return api.FotobankInventoryRequest{}, errors.New("--archive-root must be absolute")
	}
	return api.FotobankInventoryRequest{
		CatalogPath: migrationCatalogPath, VaultRoot: migrationVaultRoot,
		ArchiveRoot: migrationArchiveRoot, SnapshotID: migrationSnapshotID,
		OwnerMapPath: migrationOwnerMap,
	}, nil
}

func init() {
	migrateFotobankCmd.AddCommand(migrateFotobankInventoryCmd)
	migrateRunsCmd.AddCommand(migrateRunsListCmd, migrateRunsShowCmd)
	photosMigrateCmd.AddCommand(migrateFotobankCmd, migrateRunsCmd)
	photosCmd.AddCommand(photosMigrateCmd)

	migrateFotobankInventoryCmd.Flags().StringVar(&migrationCatalogPath, "catalog-path", "", "absolute Fotobank catalog path")
	migrateFotobankInventoryCmd.Flags().StringVar(&migrationVaultRoot, "vault-root", "", "absolute embedded Docbank vault root")
	migrateFotobankInventoryCmd.Flags().StringVar(&migrationArchiveRoot, "archive-root", "", "absolute Fotobank recovery archive root")
	migrateFotobankInventoryCmd.Flags().StringVar(&migrationSnapshotID, "snapshot-id", "", "archive snapshot ID (defaults to latest)")
	migrateFotobankInventoryCmd.Flags().StringVar(&migrationOwnerMap, "owner-map-path", "", "absolute exclusive owner-map output path")
	migrateRunsListCmd.Flags().IntVar(&migrationRunOffset, "offset", 0, "number of runs to skip")
	migrateRunsListCmd.Flags().IntVar(&migrationRunLimit, "limit", 50, "number of runs to return (1-50)")
}
