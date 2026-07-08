package cmd

import (
	"fmt"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"
)

var searchCmd = &cobra.Command{
	Use:   "search <query>...",
	Short: "Full-text search over node names",
	Args:  cobra.MinimumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		v, err := openVault()
		if err != nil {
			return err
		}
		defer func() { _ = v.close() }()

		hits, err := v.store.Search(cmd.Context(), strings.Join(args, " "), 50)
		if err != nil {
			return err
		}
		if len(hits) == 0 {
			_, _ = fmt.Fprintln(cmd.OutOrStdout(), "no matches")
			return nil
		}
		w := tabwriter.NewWriter(cmd.OutOrStdout(), 2, 4, 2, ' ', 0)
		_, _ = fmt.Fprintln(w, "ID\tPATH")
		for _, h := range hits {
			_, _ = fmt.Fprintf(w, "%d\t%s\n", h.Node.ID, h.Path)
		}
		return w.Flush()
	},
}

func init() { rootCmd.AddCommand(searchCmd) }
