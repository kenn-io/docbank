package main

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"
	"go.kenn.io/docbank/internal/daemonconn"
)

func newReportDownloadCommand() *cobra.Command {
	cmd := &cobra.Command{
		Example: `  docbank search-export download <report-id> --output out.zip`,
		Use:     "download <report-id>",
		Short:   "Save a verified report evidence ZIP",
		Args:    cobra.ExactArgs(1)}
	var output string
	var overwrite bool
	cmd.Flags().StringVar(&output, "output", "", "destination for the evidence packet")
	cmd.Flags().BoolVar(&overwrite, "overwrite", false, "replace an existing destination")
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		id := args[0]
		if !daemonconn.IsTermReportID(id) {
			return usageError(errors.New("report ID must be 48 lowercase hexadecimal characters"))
		}
		if output == "" {
			return usageError(errors.New("search-export download requires --output"))
		}
		if _, err := prepareGetDestination(output, overwrite); err != nil {
			return usageError(err)
		}
		connection, err := daemonconn.Ensure(cmd.Context())
		if err != nil {
			return err
		}
		if err := downloadReportPacket(cmd.Context(), connection, id, output, overwrite); err != nil {
			return reportDeliveryError(id, output, err)
		}
		_, err = fmt.Fprintf(cmd.OutOrStdout(),
			"%s · export %s · internally verified; source evidence not checked offline\n", output, id)
		if err != nil {
			return fmt.Errorf("report %s verified file is saved at %q; writing report output: %w",
				id, output, err)
		}
		return nil
	}
	return cmd
}

func reportDeliveryError(id, output string, err error) error {
	if code, ok := daemonconn.ProblemCode(err); ok && code == "report_unavailable" {
		return fmt.Errorf("report %s is unavailable; create a new report with "+
			"docbank search-export create --input <request.json> --output <path>: %w", id, err)
	}
	if errors.Is(err, daemonconn.ErrReportReviewRequired) {
		return fmt.Errorf("report %s needs date review; run docbank search-export dates %s, "+
			"then docbank search-export revise %s --choices choices.json --output <path>: %w",
			id, id, id, err)
	}
	return fmt.Errorf("report %s delivery did not finish cleanly; inspect %q before retrying with "+
		"docbank search-export download %s --output <path>: %w", id, output, id, err)
}
