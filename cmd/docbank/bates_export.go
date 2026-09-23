package main

import (
	"encoding/json/v2"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/apiclient"
	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/docbank/internal/pdfstamp"
	"uuid"
)

func newBatesExportCommand() *cobra.Command {
	command := &cobra.Command{Use: "export", Short: "Run and inspect verified Bates exports"}
	command.AddCommand(newBatesExportRunCommand(), newBatesExportStatusCommand(),
		newBatesExportHistoryCommand(), newBatesExportDownloadCommand())
	return command
}

func newBatesExportRunCommand() *cobra.Command {
	var recipePath string
	var asJSON bool
	command := &cobra.Command{Use: "run <allocation-id>", Short: "Apply a Bates stamp and publish a verified PDF", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if recipePath == "" {
				return usageError(errors.New("--recipe is required"))
			}
			allocationID, err := uuid.Parse(args[0])
			if err != nil {
				return usageError(fmt.Errorf("allocation ID must be a UUID: %w", err))
			}
			raw, err := os.ReadFile(recipePath)
			if err != nil {
				return fmt.Errorf("reading Bates stamp recipe: %w", err)
			}
			var recipe pdfstamp.Recipe
			if err := json.Unmarshal(raw, &recipe, json.RejectUnknownMembers(true)); err != nil {
				return usageError(fmt.Errorf("decoding Bates stamp recipe: %w", err))
			}
			if err := recipe.Validate(); err != nil {
				return usageError(err)
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
	command.Flags().BoolVar(&asJSON, "json", false, "emit machine-readable JSON")
	return command
}

func newBatesExportStatusCommand() *cobra.Command {
	var asJSON bool
	command := &cobra.Command{Use: "status <allocation-id>", Short: "Read one verified Bates export", Args: cobra.ExactArgs(1),
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
	command.Flags().BoolVar(&asJSON, "json", false, "emit machine-readable JSON")
	return command
}

func newBatesExportHistoryCommand() *cobra.Command {
	var after string
	var limit int
	var asJSON bool
	command := &cobra.Command{Use: "history", Short: "List verified Bates export history", Args: cobra.NoArgs,
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
	command.Flags().BoolVar(&asJSON, "json", false, "emit machine-readable JSON")
	return command
}

func newBatesExportDownloadCommand() *cobra.Command {
	var overwrite bool
	command := &cobra.Command{Use: "download <allocation-id> <local-file>", Short: "Download a reverified Bates export", Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) (retErr error) {
			destination, err := prepareGetDestination(args[1], overwrite)
			if err != nil {
				return err
			}
			connection, err := daemonconn.Ensure(cmd.Context())
			if err != nil {
				return err
			}
			artifact, err := connection.BatesExport(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			data, err := connection.DownloadBatesExport(cmd.Context(), artifact)
			if err != nil {
				return err
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
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "%s · %d pages · SHA-256 %s\n",
				destination, artifact.PageCount, artifact.BlobSHA256)
			if err != nil {
				return fmt.Errorf("writing Bates export download receipt: %w", err)
			}
			return nil
		}}
	command.Flags().BoolVar(&overwrite, "overwrite", false, "replace an existing destination")
	return command
}
