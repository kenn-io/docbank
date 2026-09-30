package main

import (
	"errors"
	"fmt"
	"path/filepath"

	"github.com/spf13/cobra"
	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/docbank/internal/filepublish"
	"go.kenn.io/docbank/internal/home"
)

var exportDownloadCmd = &cobra.Command{
	Use:   "download <job-id> <local-file>",
	Short: "Verify and save a completed export",
	Args:  cobra.ExactArgs(2),
	RunE:  downloadNativeExport,
}

func downloadNativeExport(cmd *cobra.Command, args []string) (retErr error) {
	if err := validateExportID("job ID", args[0]); err != nil {
		return err
	}
	destination, err := prepareGetDestination(args[1], exportOverwrite)
	if err != nil {
		return err
	}
	layout, err := home.Resolve()
	if err != nil {
		return err
	}
	inside, err := layout.ContainsDirectory(filepath.Dir(destination))
	if err != nil {
		return err
	}
	if inside {
		return usageError(errors.New(
			"export destination must be outside the Docbank data directory"))
	}
	stage, err := filepublish.CreateStage(filepath.Dir(destination), "docbank-native-export-")
	if err != nil {
		return err
	}
	var published bool
	defer func() {
		retErr = errors.Join(retErr, stage.Cleanup())
		if retErr != nil && published {
			retErr = fmt.Errorf("verified export is already saved at %q: %w", destination, retErr)
		}
	}()
	connection, err := daemonconn.Ensure(cmd.Context())
	if err != nil {
		return err
	}
	receipt, err := connection.DownloadExportArchiveTo(cmd.Context(), args[0], stage.File)
	if err != nil {
		return err
	}
	if err := errors.Join(stage.File.Sync(), stage.File.Close()); err != nil {
		return err
	}
	published, err = filepublish.Publish(stage.Path(), destination, exportOverwrite)
	if err != nil {
		return err
	}
	if err := stage.Cleanup(); err != nil {
		return err
	}
	if exportJSON {
		return writeCLIJSON(cmd.OutOrStdout(), receipt)
	}
	_, err = fmt.Fprintf(cmd.OutOrStdout(),
		"verified export saved to %s · %d bytes · SHA-256 %s\n"+
			"Run docbank export release %s when the retained archive is no longer needed; "+
			"release frees its shared job slot.\n",
		destination, receipt.Size, receipt.SHA256, args[0])
	if err != nil {
		return fmt.Errorf("writing export receipt: %w", err)
	}
	return nil
}
