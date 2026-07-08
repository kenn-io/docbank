package cmd

import (
	"fmt"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"go.kenn.io/docbank/internal/api"
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
		v, err := openVault()
		if err != nil {
			return err
		}
		defer func() { _ = v.close() }()

		roots, err := v.store.TrashedRoots(cmd.Context())
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
			trashedAt := ""
			if n.TrashedAt != nil {
				trashedAt = *n.TrashedAt
			}
			_, _ = fmt.Fprintf(w, "%d\t%s\t%s\n", n.ID, trashedAt, n.Name)
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
		age, err := api.ParseAge(trashOlderThan)
		if err != nil {
			return err
		}
		v, err := openVault()
		if err != nil {
			return err
		}
		defer func() { _ = v.close() }()

		n, err := v.store.EmptyTrash(cmd.Context(), age)
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
