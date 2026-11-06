package main

import (
	"github.com/spf13/cobra"
	"go.kenn.io/docbank/internal/apiclient"
	"go.kenn.io/docbank/internal/daemonconn"
)

const missLogListCommand = "list"

func newMissLogCmd() *cobra.Command {
	command := &cobra.Command{Use: "miss-log", Short: "Manage private search misses through the daemon", GroupID: groupSearch,
		Long: `Record empty searches only after opting in with [miss_log] enabled = true.
The log contains private query text.
Management uses the master daemon credential even when recording is off.`}
	var limit, offset int
	list := &cobra.Command{Use: missLogListCommand, Short: "List private miss receipts as JSON", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		c, err := daemonconn.Ensure(cmd.Context())
		if err != nil {
			return err
		}
		page, err := c.API().ListMissLog(cmd.Context(), &apiclient.ListMissLogRequestOptions{Query: &apiclient.ListMissLogQuery{Limit: new(int64(limit)), Offset: new(int64(offset))}})
		if err != nil {
			return err
		}
		return writeCLIJSON(cmd.OutOrStdout(), page)
	}}
	list.Flags().IntVar(&limit, "limit", 20, "maximum receipts (1–100)")
	list.Flags().IntVar(&offset, "offset", 0, "number of receipts to skip")
	remove := &cobra.Command{Use: "delete <record-id>", Short: "Delete one private miss receipt", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		c, err := daemonconn.Ensure(cmd.Context())
		if err != nil {
			return err
		}
		_, err = c.API().DeleteMissLogRecord(cmd.Context(), &apiclient.DeleteMissLogRecordRequestOptions{PathParams: &apiclient.DeleteMissLogRecordPath{ID: args[0]}})
		return err
	}}
	purge := &cobra.Command{Use: "purge", Short: "Delete all private miss receipts", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		c, err := daemonconn.Ensure(cmd.Context())
		if err != nil {
			return err
		}
		_, err = c.API().DeleteMissLog(cmd.Context())
		return err
	}}
	list.Example = "  docbank miss-log list --limit 20"
	remove.Example = "  docbank miss-log delete <record-id>"
	purge.Example = "  docbank miss-log purge"
	command.AddCommand(list, remove, purge)
	return command
}
func init() { rootCmd.AddCommand(newMissLogCmd()) }
