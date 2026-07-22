package main

import (
	"fmt"
	"io"
	"strconv"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/client"
)

var watchCmd = &cobra.Command{
	Use:   "watch",
	Short: "Inspect configured watched inboxes",
}

var watchListJSON bool

var watchListCmd = &cobra.Command{
	Use:   "list",
	Short: "List effective watch configuration and runner status",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		c, err := client.Ensure(cmd.Context())
		if err != nil {
			return err
		}
		items, err := c.WatchedInboxes(cmd.Context())
		if err != nil {
			return err
		}
		if watchListJSON {
			return writeCLIJSON(cmd.OutOrStdout(), api.WatchedInboxList{Items: items})
		}
		return writeWatchedInboxes(cmd.OutOrStdout(), items)
	},
}

func writeWatchedInboxes(w io.Writer, items []api.WatchedInbox) error {
	if len(items) == 0 {
		if _, err := fmt.Fprintln(w, "no watched inboxes configured"); err != nil {
			return fmt.Errorf("writing empty watched-inbox list: %w", err)
		}
		return nil
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	if _, err := fmt.Fprintln(tw,
		"NAME\tSTATUS\tSOURCE\tDESTINATION\tSETTLE\tMINIMUM AGE\tSCAN\tEXCLUDES\tERROR"); err != nil {
		return fmt.Errorf("writing watched-inbox header: %w", err)
	}
	for _, item := range items {
		status, problem := "unavailable", "-"
		if item.Job != nil {
			status = item.Job.Status
			if item.Job.Error != "" {
				problem = strconv.QuoteToASCII(item.Job.Error)
			}
		}
		if _, err := fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%d\t%s\n",
			item.Name, status, strconv.QuoteToASCII(item.Source),
			strconv.QuoteToASCII(item.Destination), item.SettleTime,
			item.MinimumAge, item.ScanInterval, len(item.Exclude), problem); err != nil {
			return fmt.Errorf("writing watched-inbox row: %w", err)
		}
	}
	if err := tw.Flush(); err != nil {
		return fmt.Errorf("writing watched-inbox list: %w", err)
	}
	return nil
}

func init() {
	watchListCmd.Flags().BoolVar(&watchListJSON, "json", false, "emit machine-readable JSON")
	watchCmd.AddCommand(watchListCmd)
	rootCmd.AddCommand(watchCmd)
}
