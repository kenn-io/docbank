package main

import (
	"errors"
	"fmt"
	"path/filepath"

	"github.com/spf13/cobra"

	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/daemonconn"
)

var (
	photoImportDestination string
	photoImportJSON        bool
)

var photoImportCmd = &cobra.Command{
	Example: `  docbank photos import ./photos /cases/acme/photos`,
	Use:     "import <source-root> [destination]",
	Short:   "Import grouped camera files from a folder",
	Args:    cobra.RangeArgs(1, 2),
	RunE: func(cmd *cobra.Command, args []string) error {
		destination := photoImportDestination
		if len(args) == 2 {
			destination = args[1]
		}
		// filepath.Abs resolves an empty path to the working directory.
		if args[0] == "" {
			return usageError(errors.New("source-root must not be empty"))
		}
		root, err := filepath.Abs(args[0])
		if err != nil {
			return fmt.Errorf("resolving %q: %w", args[0], err)
		}
		connection, err := daemonconn.Ensure(cmd.Context())
		if err != nil {
			return err
		}
		operation, err := connection.StartPhotoImport(cmd.Context(), root, destination)
		if err != nil {
			return err
		}
		return writePhotoImportOutput(cmd, operation)
	},
}

func writePhotoImportOutput(cmd *cobra.Command, operation api.StorageOperation) error {
	if photoImportJSON {
		return writeCLIJSON(cmd.OutOrStdout(), operation)
	}
	if _, err := fmt.Fprintf(cmd.OutOrStdout(), "import: %s (%s)\nProgress and unpaired groups: docbank jobs show %s --json\nCancel: docbank jobs cancel %s\n",
		operation.ID, operation.State, operation.ID, operation.ID); err != nil {
		return fmt.Errorf("writing photo import output: %w", err)
	}
	return nil
}

func init() {
	photosCmd.AddCommand(photoImportCmd)
	photoImportCmd.Flags().StringVar(&photoImportDestination, "destination", "/", "vault destination path")
	photoImportCmd.Flags().BoolVar(&photoImportJSON, "json", false, "print JSON to stdout")
}
