package main

import (
	"fmt"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/client"
)

var trashCmd = &cobra.Command{
	Use:   "trash",
	Short: "Inspect and empty the trash",
}

var trashListCmd = &cobra.Command{
	Use:   "list",
	Short: "List restorable trashed nodes",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		c, err := client.Ensure(cmd.Context())
		if err != nil {
			return err
		}
		roots, err := c.TrashList(cmd.Context())
		if err != nil {
			return err
		}
		if len(roots) == 0 {
			_, _ = fmt.Fprintln(cmd.OutOrStdout(), "trash is empty")
			return nil
		}
		w := tabwriter.NewWriter(cmd.OutOrStdout(), 2, 4, 2, ' ', 0)
		_, _ = fmt.Fprintln(w, "ID\tTRASHED AT\tNAME")
		for _, n := range roots {
			_, _ = fmt.Fprintf(w, "%d\t%s\t%s\n", n.ID, n.TrashedAt, n.Name)
		}
		return w.Flush()
	},
}

var trashOlderThan string

var trashEmptyCmd = &cobra.Command{
	Use:   "empty",
	Short: "Permanently delete trashed nodes (their blobs become gc candidates)",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		if _, err := api.ParseAge(trashOlderThan); err != nil {
			return err
		}
		c, err := client.Ensure(cmd.Context())
		if err != nil {
			return err
		}
		n, err := c.TrashEmpty(cmd.Context(), trashOlderThan)
		if err != nil {
			return err
		}
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "deleted %d trashed node(s)\n", n)
		return nil
	},
}

func init() {
	trashEmptyCmd.Flags().StringVar(&trashOlderThan, "older-than", "",
		"only delete items trashed at least this long ago (e.g. 30d)")
	trashCmd.AddCommand(trashListCmd, trashEmptyCmd)
	rootCmd.AddCommand(trashCmd)
}
