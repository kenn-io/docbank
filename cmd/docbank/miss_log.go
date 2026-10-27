package main

import (
	"errors"
	"time"

	"github.com/spf13/cobra"
	"go.kenn.io/docbank/internal/daemonconn"
)

func newMissLogCmd() *cobra.Command {
	command := &cobra.Command{Use: "miss-log", Short: "Manage private search misses through the daemon", GroupID: groupSearch,
		Long: `Record empty searches only after opting in with [miss_log] enabled = true
or "docbank daemon run --miss-log". The log contains private query text.
List and show print JSON; delete and purge remove current records.
Management uses the master daemon credential even when recording is off.`}
	var limit, offset int
	list := &cobra.Command{Use: photoCheckoutListCommand, Short: "List private miss receipts as JSON", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		if limit < 1 || limit > 100 || offset < 0 {
			return usageError(errors.New("limit must be 1–100 and offset nonnegative"))
		}
		c, err := daemonconn.Ensure(cmd.Context())
		if err != nil {
			return err
		}
		page, err := c.ListMissLog(cmd.Context(), limit, offset)
		if err != nil {
			return err
		}
		return writeCLIJSON(cmd.OutOrStdout(), page)
	}}
	list.Flags().IntVar(&limit, "limit", 20, "maximum receipts (1–100)")
	list.Flags().IntVar(&offset, "offset", 0, "number of receipts to skip")
	show := &cobra.Command{Use: "show <record-id>", Short: "Show one private miss receipt as JSON", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		c, err := daemonconn.Ensure(cmd.Context())
		if err != nil {
			return err
		}
		receipt, err := c.MissLogRecord(cmd.Context(), args[0])
		if err != nil {
			return err
		}
		return writeCLIJSON(cmd.OutOrStdout(), receipt)
	}}
	var before string
	remove := &cobra.Command{Use: "delete [record-id]", Short: "Delete one receipt or receipts older than --before", Args: cobra.MaximumNArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		if (len(args) == 1) == (before != "") {
			return usageError(errors.New("supply either a record ID or --before"))
		}
		if before != "" {
			if _, err := time.Parse(time.RFC3339, before); err != nil {
				return usageError(errors.New("before must be an RFC 3339 timestamp"))
			}
		}
		c, err := daemonconn.Ensure(cmd.Context())
		if err != nil {
			return err
		}
		if len(args) == 1 {
			return c.DeleteMissLogRecord(cmd.Context(), args[0])
		}
		return c.DeleteMissLog(cmd.Context(), before)
	}}
	remove.Flags().StringVar(&before, "before", "", "delete receipts strictly older than this RFC 3339 timestamp")
	purge := &cobra.Command{Use: "purge", Short: "Delete all private miss receipts", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		c, err := daemonconn.Ensure(cmd.Context())
		if err != nil {
			return err
		}
		return c.DeleteMissLog(cmd.Context(), "")
	}}
	list.Example = "  docbank miss-log list --limit 20"
	show.Example = "  docbank miss-log show <record-id>"
	remove.Example = "  docbank miss-log delete <record-id>\n  docbank miss-log delete --before 2026-01-01T00:00:00Z"
	purge.Example = "  docbank miss-log purge"
	command.AddCommand(list, show, remove, purge)
	return command
}
func init() { rootCmd.AddCommand(newMissLogCmd()) }
