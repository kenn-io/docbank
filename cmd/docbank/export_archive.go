package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"
	"go.kenn.io/docbank/document/bundle"
	"go.kenn.io/docbank/internal/daemonconn"
)

func downloadExportArchive(ctx context.Context, connection *daemonconn.Connection,
	jobID, output string, overwrite bool,
) (bundle.Receipt, error) {
	var verified bundle.Receipt
	err := publishReportOutput(output, overwrite, func(file *os.File) error {
		stream, err := connection.OpenExportArchive(ctx, jobID)
		if err != nil {
			return err
		}
		defer func() { _ = stream.Close() }()
		if _, err = stream.CopyVerified(file); err != nil {
			return err
		}
		if _, err = file.Seek(0, io.SeekStart); err != nil {
			return err
		}
		verified, err = bundle.Verify(ctx, file, stream.Size, stream.PlanFingerprint)
		if err != nil {
			return err
		}
		if verified.Size != stream.Size || verified.SHA256 != stream.SHA256 ||
			verified.PlanFingerprint != stream.PlanFingerprint {
			return errors.New("export archive differs from retained receipt")
		}
		return nil
	})
	return verified, err
}

func newExportArchiveCommand() *cobra.Command {
	var output string
	var overwrite bool
	command := &cobra.Command{Use: "archive <job-id>",
		Short: "Download and verify one completed export archive",
		Args:  cobra.ExactArgs(1)}
	command.Flags().StringVar(&output, "output", "", "Destination for the verified ZIP archive")
	command.Flags().BoolVar(&overwrite, "overwrite", false, "Replace an existing destination")
	command.RunE = func(cmd *cobra.Command, args []string) error {
		if output == "" {
			return usageError(errors.New("export archive requires --output"))
		}
		if _, err := prepareGetDestination(output, overwrite); err != nil {
			return err
		}
		connection, err := daemonconn.Ensure(cmd.Context())
		if err != nil {
			return err
		}
		receipt, err := downloadExportArchive(cmd.Context(), connection, args[0], output, overwrite)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(cmd.OutOrStdout(), "%s · SHA-256 %s · plan %s\n",
			output, receipt.SHA256, receipt.PlanFingerprint)
		if err != nil {
			return fmt.Errorf("write export archive receipt: %w", err)
		}
		return nil
	}
	return command
}

func init() {
	root := &cobra.Command{Use: "export", Short: "Download verified document exports"}
	root.AddCommand(newExportArchiveCommand())
	rootCmd.AddCommand(root)
}
