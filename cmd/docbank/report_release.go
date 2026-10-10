package main

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"
	"go.kenn.io/docbank/internal/daemonconn"
)

func newReportReleaseCommand() *cobra.Command {
	cmd := &cobra.Command{
		Example: `  docbank search-export release <report-id> --json`,
		Use:     "release <report-id>",
		Short:   "Release a live report and its retained evidence",
		Long: `Discard this report's live packet and date-review evidence. History, saved
files and child reports remain.
If report_retained is returned, retry after the download closes; even
download && release can need a deliberate retry.
After an uncertain reply, inspect search-export show <report-id>. HTTP 410 means
no owned live handle remains. HTTP 503 means reporting is unavailable and does
not confirm release.`,
		Args: func(cmd *cobra.Command, args []string) error {
			return usageError(cobra.ExactArgs(1)(cmd, args))
		},
	}
	var jsonOutput bool
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "print JSON to stdout")
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		id := args[0]
		if !daemonconn.IsTermReportID(id) {
			return usageError(errors.New("report ID must be 48 lowercase hexadecimal characters"))
		}
		connection, err := daemonconn.Ensure(cmd.Context())
		if err != nil {
			return err
		}
		if err := connection.ReleaseTermReport(cmd.Context(), id); err != nil {
			return err
		}
		if jsonOutput {
			err = writeCLIJSON(cmd.OutOrStdout(), struct {
				ReportID string `json:"report_id"`
				Released bool   `json:"released"`
			}{ReportID: id, Released: true})
		} else {
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "released report %s\n", id)
		}
		if err != nil {
			return fmt.Errorf("report %s was released; writing release result: %w", id, err)
		}
		return nil
	}
	return cmd
}
