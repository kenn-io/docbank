package main

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"

	"github.com/spf13/cobra"
	"go.kenn.io/docbank/internal/daemonconn"
)

var productionPackageCmd = &cobra.Command{Use: "package", Short: "Download verified retained production packages"}

type productionPackageDownloadReceipt struct {
	Destination   string `json:"destination"`
	ArchiveSHA256 string `json:"archive_sha256"`
	Size          int64  `json:"size"`
	VersionID     string `json:"version_id"`
}

func newProductionPackageDownloadCommand() *cobra.Command {
	return newProductionPackageDownloadCommandWithEnsure(daemonconn.Ensure)
}

func newProductionPackageDownloadCommandWithEnsure(
	ensure func(context.Context) (*daemonconn.Connection, error)) *cobra.Command {
	var overwrite, asJSON bool
	cmd := &cobra.Command{Use: "download <job-id> <operation-id> <local-file>",
		Short: "Download and verify one exact retained production archive",
		Args:  cobra.ExactArgs(3),
		RunE: func(cmd *cobra.Command, args []string) (retErr error) {
			destination, err := prepareGetDestination(args[2], overwrite)
			if err != nil {
				return err
			}
			staging, err := makePrivateStagingDirAt(filepath.Dir(destination), "docbank-production-package-")
			if err != nil {
				return err
			}
			defer func() { retErr = errors.Join(retErr, staging.removeAll()) }()
			file, path, err := staging.createFile(filepath.Base(destination))
			if err != nil {
				return err
			}
			connection, err := ensure(cmd.Context())
			if err != nil {
				_ = file.Close()
				return err
			}
			ticket, err := connection.DownloadProductionPackageTo(cmd.Context(), args[0], args[1], file)
			if err != nil {
				_ = file.Close()
				return err
			}
			if err := errors.Join(file.Sync(), file.Close()); err != nil {
				return err
			}
			if err := publishGetFile(path, destination, overwrite); err != nil {
				return err
			}
			if asJSON {
				return writeCLIJSON(cmd.OutOrStdout(), productionPackageDownloadReceipt{
					Destination: destination, ArchiveSHA256: ticket.ArchiveSHA256,
					Size: ticket.Size, VersionID: ticket.VersionID,
				})
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "%s · SHA-256 %s · version %s\n",
				destination, ticket.ArchiveSHA256, ticket.VersionID)
			if err != nil {
				return fmt.Errorf("writing production package receipt: %w", err)
			}
			return nil
		}}
	cmd.Flags().BoolVar(&overwrite, "overwrite", false, "replace an existing destination after verification")
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit machine-readable download receipt JSON")
	return cmd
}

func init() {
	productionPackageCmd.AddCommand(newProductionPackageDownloadCommand())
	productionCmd.AddCommand(productionPackageCmd)
}
