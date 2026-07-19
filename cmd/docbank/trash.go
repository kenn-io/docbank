package main

import (
	"fmt"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/client"
)

var trashListJSON bool

type trashListing struct {
	Items []api.Node `json:"items"`
}

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
		if trashListJSON {
			return writeCLIJSON(cmd.OutOrStdout(), trashListing{Items: roots})
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

var (
	trashOlderThan string
	trashRun       bool
	trashEmptyJSON bool
)

var trashEmptyCmd = &cobra.Command{
	Use:   "empty",
	Short: "Report or permanently delete trashed nodes",
	Long: "Report or permanently delete trashed tree metadata. Content bytes remain " +
		"until they are unreachable and an explicit gc run removes their authority; " +
		"packed space then requires repack.",
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		if _, err := api.ParseAge(trashOlderThan); err != nil {
			return usageError(err)
		}
		c, err := client.Ensure(cmd.Context())
		if err != nil {
			return err
		}
		rep, err := c.TrashEmpty(cmd.Context(), trashOlderThan, trashRun)
		if err != nil {
			return err
		}
		if trashEmptyJSON {
			return writeCLIJSON(cmd.OutOrStdout(), rep)
		}
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "%d trashed root(s) eligible for permanent deletion\n",
			rep.CandidateRoots)
		if !rep.Run {
			if rep.CandidateRoots > 0 {
				_, _ = fmt.Fprintln(cmd.OutOrStdout(), "dry run — pass --run to delete")
			}
			return nil
		}
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "deleted %d trashed root(s)\n", rep.Deleted)
		return nil
	},
}

func init() {
	trashListCmd.Flags().BoolVar(&trashListJSON, "json", false,
		"emit machine-readable JSON")
	trashEmptyCmd.Flags().StringVar(&trashOlderThan, "older-than", "",
		"select only items trashed at least this long ago (e.g. 30d)")
	trashEmptyCmd.Flags().BoolVar(&trashRun, "run", false,
		"actually delete (default is dry-run)")
	trashEmptyCmd.Flags().BoolVar(&trashEmptyJSON, "json", false,
		"emit a machine-readable report")
	trashCmd.AddCommand(trashListCmd, trashEmptyCmd)
	rootCmd.AddCommand(trashCmd)
}
