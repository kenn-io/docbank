package main

import (
	"errors"
	"fmt"
	"path/filepath"

	"github.com/spf13/cobra"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/apiclient"
	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/docbank/internal/processing"
	"uuid"
)

func newBatesExportCommand() *cobra.Command {
	command := &cobra.Command{
		Long: `run publishes PDFs for an allocation from bates reserve and a stamp recipe
from bates plan --recipe-out. status and history show export IDs; download
saves a hash-checked export to disk.`,
		Use:   "export",
		Short: "Run and inspect verified Bates exports"}
	command.AddCommand(newBatesExportRunCommand(), newBatesExportStatusCommand(),
		newBatesExportHistoryCommand(), newBatesExportDownloadCommand())
	return command
}

func newBatesExportRunCommand() *cobra.Command {
	var recipePath string
	var asJSON bool
	command := &cobra.Command{
		Example: `  docbank bates export run <allocation-id> --recipe r.json`,
		Use:     "run <allocation-id>",
		Short:   "Apply a Bates stamp and publish a verified PDF",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if recipePath == "" {
				return usageError(errors.New("--recipe is required"))
			}
			allocationID, err := uuid.Parse(args[0])
			if err != nil {
				return usageError(fmt.Errorf("allocation ID must be a UUID: %w", err))
			}
			recipe, err := readBatesRecipe(recipePath)
			if err != nil {
				return err
			}
			connection, err := daemonconn.Ensure(cmd.Context())
			if err != nil {
				return err
			}
			artifact, err := connection.API().PublishBatesExport(cmd.Context(), &apiclient.PublishBatesExportRequestOptions{
				Body: &api.BatesExportRequest{AllocationID: allocationID.String(), Recipe: recipe}})
			if err != nil {
				return err
			}
			if asJSON {
				return writeCLIJSON(cmd.OutOrStdout(), artifact)
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "Bates export %s verified · %d pages · SHA-256 %s\n",
				artifact.AllocationID, artifact.PageCount, artifact.BlobSHA256)
			if err != nil {
				return fmt.Errorf("writing Bates export: %w", err)
			}
			return nil
		}}
	command.Flags().StringVar(&recipePath, "recipe", "", "path to the canonical Bates stamp recipe JSON")
	command.Flags().BoolVar(&asJSON, "json", false, "print JSON to stdout")
	return command
}

func newBatesExportStatusCommand() *cobra.Command {
	var asJSON bool
	command := &cobra.Command{
		Example: `  docbank bates export status <allocation-id> --json`,
		Use:     "status <allocation-id>",
		Short:   "Read one verified Bates export",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			connection, err := daemonconn.Ensure(cmd.Context())
			if err != nil {
				return err
			}
			artifact, err := connection.BatesExport(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			if asJSON {
				return writeCLIJSON(cmd.OutOrStdout(), artifact)
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "%s %s · %d pages · SHA-256 %s\n",
				artifact.AllocationID, artifact.State, artifact.PageCount, artifact.BlobSHA256)
			if err != nil {
				return fmt.Errorf("writing Bates export status: %w", err)
			}
			return nil
		}}
	command.Flags().BoolVar(&asJSON, "json", false, "print JSON to stdout")
	return command
}

func newBatesExportHistoryCommand() *cobra.Command {
	var after string
	var limit int
	var asJSON bool
	command := &cobra.Command{
		Example: `  docbank bates export history --json`,
		Use:     "history",
		Short:   "List verified Bates export history",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if limit < 1 || limit > 250 {
				return usageError(errors.New("--limit must be between 1 and 250"))
			}
			connection, err := daemonconn.Ensure(cmd.Context())
			if err != nil {
				return err
			}
			pageLimit := int64(limit)
			page, err := connection.API().ListBatesExports(cmd.Context(), &apiclient.ListBatesExportsRequestOptions{
				Query: &apiclient.ListBatesExportsQuery{After: &after, Limit: &pageLimit}})
			if err != nil {
				return err
			}
			if asJSON {
				return writeCLIJSON(cmd.OutOrStdout(), page)
			}
			for _, artifact := range page.Items {
				if _, err := fmt.Fprintf(cmd.OutOrStdout(), "%s %s · %d pages · SHA-256 %s\n",
					artifact.AllocationID, artifact.State, artifact.PageCount, artifact.BlobSHA256); err != nil {
					return fmt.Errorf("writing Bates export history: %w", err)
				}
			}
			if page.NextAfter != "" {
				_, err = fmt.Fprintf(cmd.OutOrStdout(), "next_after: %s\n", page.NextAfter)
			}
			if err != nil {
				return fmt.Errorf("writing Bates export history cursor: %w", err)
			}
			return nil
		}}
	command.Flags().StringVar(&after, "after", "", "continue after an artifact ID")
	command.Flags().IntVar(&limit, "limit", 100, "maximum exports to list (1-250)")
	command.Flags().BoolVar(&asJSON, "json", false, "print JSON to stdout")
	return command
}

func newBatesExportDownloadCommand() *cobra.Command {
	var overwrite, asJSON bool
	var prepPath, masksPath, reviewPath string
	command := &cobra.Command{
		Example: `  docbank bates export download <allocation-id> ./stamped.pdf`,
		Use:     "download <allocation-id> <local-file>",
		Short:   "Download a hash-checked Bates export",
		Args:    cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) (retErr error) {
			if !daemonconn.IsCanonicalUUIDv4(args[0]) {
				return usageError(errors.New("allocation must be a canonical lowercase UUIDv4"))
			}
			var review *processing.RedactionReview
			if prepPath != "" || masksPath != "" || reviewPath != "" {
				if prepPath == "" || masksPath == "" || reviewPath == "" {
					return usageError(errors.New("provide --preparation, --masks and --review together"))
				}
				inputs, err := readRedactionInputs(prepPath, masksPath)
				if err != nil {
					return err
				}
				checked, err := readRedactionReview(reviewPath, inputs)
				if err != nil {
					return err
				}
				review = &checked
			}
			destination, err := prepareGetDestination(args[1], overwrite)
			if err != nil {
				return err
			}
			connection, err := daemonconn.Ensure(cmd.Context())
			if err != nil {
				return err
			}
			defer func() { _ = connection.Close() }()
			artifact, err := connection.BatesExport(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			data, verified, err := connection.DownloadBatesExport(cmd.Context(), artifact, review)
			if err != nil {
				return redactionCLIError(err)
			}
			staging, err := makePrivateStagingDirAt(filepath.Dir(destination), "docbank-bates-export-")
			if err != nil {
				return err
			}
			defer func() { retErr = errors.Join(retErr, staging.removeAll()) }()
			file, path, err := staging.createFile(filepath.Base(destination))
			if err != nil {
				return err
			}
			_, writeErr := file.Write(data)
			if err := errors.Join(writeErr, file.Sync(), file.Close()); err != nil {
				return err
			}
			if err := publishGetFile(path, destination, overwrite); err != nil {
				return err
			}
			if asJSON {
				err := writeCLIJSON(cmd.OutOrStdout(), struct {
					daemonconn.BatesDownloadResult

					Destination string `json:"destination"`
					State       string `json:"state"`
				}{verified, destination, "published"})
				if err != nil {
					return fmt.Errorf("verified PDF is saved at %q: %w", destination, err)
				}
				return nil
			}
			claim := "Stamped labels verified."
			switch verified.Verification {
			case "recorded_bytes_and_numbers":
				claim = "Recorded PDF bytes and numbering metadata verified; masks, approved text and rendered numbers were not rechecked."
			case "masks_text_and_numbers":
				claim = "PDF masks, text and numbers match the supplied review and reservation metadata."
			}
			if _, err = fmt.Fprintln(cmd.OutOrStdout(), claim); err != nil {
				return fmt.Errorf("verified PDF is saved at %q: %w", destination, err)
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "%s · %d pages · SHA-256 %s\n",
				destination, artifact.PageCount, artifact.BlobSHA256)
			if err != nil {
				return fmt.Errorf("verified PDF is saved at %q: %w", destination, err)
			}
			return nil
		}}
	command.Flags().BoolVar(&asJSON, "json", false, "print JSON saved identity and verification scope")
	command.Flags().StringVar(&prepPath, "preparation", "", "preparation file for full numbered-PDF verification")
	command.Flags().StringVar(&masksPath, "masks", "", "masks file for full numbered-PDF verification")
	command.Flags().StringVar(&reviewPath, "review", "", "review file for full numbered-PDF verification")
	command.Flags().BoolVar(&overwrite, "overwrite", false, "replace an existing destination")
	return command
}
