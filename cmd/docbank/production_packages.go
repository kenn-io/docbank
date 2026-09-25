package main

import (
	"errors"
	"fmt"
	"path/filepath"

	"github.com/spf13/cobra"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/daemonconn"
	"uuid"
)

func newProductionPackagePublishCommand() *cobra.Command {
	var profileID, operationID string
	var maxVolumeBytes int64
	var maxVolumeDocuments int
	var asJSON bool
	command := &cobra.Command{Use: "publish <job-id>", Short: "Publish a verified recipient package from a successful job", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			effectiveOperationID := operationID
			if effectiveOperationID == "" {
				effectiveOperationID = uuid.NewV4().String()
			}
			request := api.ProductionPackagePublishRequest{OperationID: effectiveOperationID,
				ProfileID: profileID, MaxVolumeBytes: maxVolumeBytes, MaxVolumeDocuments: maxVolumeDocuments}
			if !request.Valid() {
				return usageError(errors.New("invalid production package request"))
			}
			connection, err := daemonconn.Ensure(cmd.Context())
			if err != nil {
				return err
			}
			published, err := connection.PublishProductionPackage(cmd.Context(), args[0], request)
			if err != nil {
				return fmt.Errorf("publishing production package (operation %s): %w", effectiveOperationID, err)
			}
			if asJSON {
				return writeCLIJSON(cmd.OutOrStdout(), published)
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "%s operation_id=%s profile=%s bytes=%d sha256=%s\n",
				published.JobID, published.OperationID, published.ProfileID, published.Size, published.ArchiveSHA256)
			if err != nil {
				return fmt.Errorf("writing production package publication: %w", err)
			}
			return nil
		}}
	command.Flags().StringVar(&profileID, "profile", "export-dat-pdf-v1", "recipient profile: export-dat-pdf-v1, export-dat-opt-images-v1, or export-dat-lfp-images-v1")
	command.Flags().Int64Var(&maxVolumeBytes, "max-volume-bytes", 1<<30, "maximum bytes per package volume (1 to 50 GiB)")
	command.Flags().IntVar(&maxVolumeDocuments, "max-volume-documents", 10_000, "maximum documents per package volume (1 to 100000)")
	command.Flags().StringVar(&operationID, "operation-id", "", "idempotency UUID; generated when omitted")
	command.Flags().BoolVar(&asJSON, "json", false, "emit machine-readable JSON")
	return command
}

func newProductionPackageDownloadCommand() *cobra.Command {
	var overwrite bool
	var asJSON bool
	command := &cobra.Command{Use: "download <job-id> <publish-operation-id> <local-file>",
		Short: "Download and verify a retained production package", Args: cobra.ExactArgs(3),
		RunE: func(cmd *cobra.Command, args []string) (retErr error) {
			destination, err := prepareGetDestination(args[2], overwrite)
			if err != nil {
				return err
			}
			connection, err := daemonconn.Ensure(cmd.Context())
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
			ticket, err := connection.DownloadProductionPackageTo(cmd.Context(), args[0], args[1], file)
			if err != nil {
				return errors.Join(err, file.Close())
			}
			if err := errors.Join(file.Sync(), file.Close()); err != nil {
				return fmt.Errorf("syncing production package download: %w", err)
			}
			if err := publishGetFile(path, destination, overwrite); err != nil {
				return fmt.Errorf("publishing production package download: %w", err)
			}
			if asJSON {
				return writeCLIJSON(cmd.OutOrStdout(), struct {
					Path          string `json:"path"`
					ArchiveSHA256 string `json:"archive_sha256"`
					Size          int64  `json:"size"`
					VersionID     string `json:"version_id"`
				}{destination, ticket.ArchiveSHA256, ticket.Size, ticket.VersionID})
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "%s bytes=%d sha256=%s version=%s\n",
				destination, ticket.Size, ticket.ArchiveSHA256, ticket.VersionID)
			if err != nil {
				return fmt.Errorf("writing production package download receipt: %w", err)
			}
			return nil
		}}
	command.Flags().BoolVar(&overwrite, "overwrite", false, "replace an existing destination")
	command.Flags().BoolVar(&asJSON, "json", false, "emit machine-readable JSON ticket")
	return command
}
