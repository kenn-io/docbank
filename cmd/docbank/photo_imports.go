package main

import (
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
	Use:   "import <source-root> [destination]",
	Short: "Import grouped camera files from a folder",
	Args:  cobra.RangeArgs(1, 2),
	RunE: func(cmd *cobra.Command, args []string) error {
		destination := photoImportDestination
		if len(args) == 2 {
			destination = args[1]
		}
		root, err := filepath.Abs(args[0])
		if err != nil {
			return fmt.Errorf("resolving %q: %w", args[0], err)
		}
		connection, err := daemonconn.Ensure(cmd.Context())
		if err != nil {
			return err
		}
		run, err := connection.StartPhotoImport(cmd.Context(), root, destination)
		if err != nil {
			return err
		}
		return writePhotoImportOutput(cmd, run)
	},
}

var photoImportsCmd = &cobra.Command{
	Use:   "imports",
	Short: "Inspect or cancel photo imports",
	Args:  cobra.NoArgs,
	RunE:  func(cmd *cobra.Command, _ []string) error { return cmd.Help() },
}

var photoImportShowCmd = &cobra.Command{
	Use:   "show <import-id>",
	Short: "Show one photo import and its ambiguous groups",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		connection, err := daemonconn.Ensure(cmd.Context())
		if err != nil {
			return err
		}
		run, err := connection.PhotoImport(cmd.Context(), args[0])
		if err != nil {
			return err
		}
		return writePhotoImportOutput(cmd, run)
	},
}

var photoImportCancelCmd = &cobra.Command{
	Use:   "cancel <import-id>",
	Short: "Stop a photo import before its next group",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		connection, err := daemonconn.Ensure(cmd.Context())
		if err != nil {
			return err
		}
		run, err := connection.CancelPhotoImport(cmd.Context(), args[0])
		if err != nil {
			return err
		}
		return writePhotoImportOutput(cmd, run)
	},
}

func writePhotoImportOutput(cmd *cobra.Command, run api.PhotoImportRun) error {
	if photoImportJSON {
		return writeCLIJSON(cmd.OutOrStdout(), run)
	}
	out := cmd.OutOrStdout()
	if _, err := fmt.Fprintf(out, "import: %s\nstate: %s\nprogress: %d/%d groups\nadded: %d\nskipped: %d\nchanged during import: %d\nfailed: %d\nambiguous: %d\n",
		run.ID, run.State, run.CompletedGroups, run.TotalGroups, run.AddedGroups, run.SkippedGroups, run.ChangedGroups, run.FailedGroups, run.AmbiguousGroups); err != nil {
		return fmt.Errorf("writing photo import output: %w", err)
	}
	if run.Error != "" {
		if _, err := fmt.Fprintf(out, "error: %s\n", run.Error); err != nil {
			return fmt.Errorf("writing photo import output: %w", err)
		}
	}
	for index, ambiguity := range run.Ambiguities {
		if _, err := fmt.Fprintf(out, "ambiguous group %d (%s):\n", index+1, ambiguity.Reason); err != nil {
			return fmt.Errorf("writing photo import ambiguity: %w", err)
		}
		for _, file := range ambiguity.Files {
			asset := file.AssetID
			if asset == "" {
				asset = "no photo"
			}
			if _, err := fmt.Fprintf(out, "  id:%d %s %s %s\n", file.NodeID, file.Role, asset, file.SourcePath); err != nil {
				return fmt.Errorf("writing photo import ambiguity: %w", err)
			}
		}
	}
	if more := run.AmbiguousGroups - int64(len(run.Ambiguities)); more > 0 {
		details := "not listed in this receipt"
		if run.State == "queued" || run.State == "running" {
			details = "will be listed when this import finishes"
		}
		if _, err := fmt.Fprintf(out, "%d more ambiguous groups %s.\n", more, details); err != nil {
			return fmt.Errorf("writing photo import output: %w", err)
		}
	}
	if len(run.Ambiguities) > 0 {
		if _, err := fmt.Fprintln(out, "To pair a group, move its RAW and JPEG files into one photo: 'docbank photos assets inspect <asset-id>' shows file IDs, "+
			"'docbank photos assets detach <asset-id> <file-id>' frees a file, and 'docbank photos assets attach <asset-id> id:<node>' adds it. The sidecar follows on the next import."); err != nil {
			return fmt.Errorf("writing photo import output: %w", err)
		}
	}
	return nil
}

func init() {
	photosCmd.AddCommand(photoImportCmd, photoImportsCmd)
	photoImportsCmd.AddCommand(photoImportShowCmd, photoImportCancelCmd)
	photoImportCmd.Flags().StringVar(&photoImportDestination, "destination", "/", "vault destination path")
	for _, command := range []*cobra.Command{photoImportCmd, photoImportShowCmd, photoImportCancelCmd} {
		command.Flags().BoolVar(&photoImportJSON, "json", false, "emit machine-readable JSON")
	}
}
