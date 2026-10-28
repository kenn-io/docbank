package main

import (
	"errors"

	"github.com/spf13/cobra"
	"go.kenn.io/docbank/internal/daemonconn"
)

func newMissLogCmd() *cobra.Command {
	command := &cobra.Command{Use: "miss-log", Short: "Manage private search misses through the daemon", GroupID: groupSearch,
		Long: `Record empty searches only after opting in with [miss_log] enabled = true.
The log contains private query text.
List and show print JSON; delete and purge remove current records.
Management uses the master daemon credential even when recording is off.`}
	var limit, offset int
	list := &cobra.Command{Use: "list", Short: "List private miss receipts as JSON", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
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
	remove := &cobra.Command{Use: "delete <record-id>", Short: "Delete one private miss receipt", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		c, err := daemonconn.Ensure(cmd.Context())
		if err != nil {
			return err
		}
		return c.DeleteMissLogRecord(cmd.Context(), args[0])
	}}
	purge := &cobra.Command{Use: "purge", Short: "Delete all private miss receipts", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		c, err := daemonconn.Ensure(cmd.Context())
		if err != nil {
			return err
		}
		return c.DeleteMissLog(cmd.Context())
	}}
	list.Example = "  docbank miss-log list --limit 20"
	show.Example = "  docbank miss-log show <record-id>"
	remove.Example = "  docbank miss-log delete <record-id>"
	purge.Example = "  docbank miss-log purge"
	command.AddCommand(list, show, remove, purge)
	return command
}
func init() { rootCmd.AddCommand(newMissLogCmd()) }
