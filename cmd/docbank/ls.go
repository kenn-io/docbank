package main

import (
	"fmt"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"go.kenn.io/docbank/internal/client"
)

var lsCmd = &cobra.Command{
	Use:   "ls [path]",
	Short: "List a virtual directory",
	Args:  cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		path := "/"
		if len(args) == 1 {
			path = args[0]
		}
		c, err := client.Ensure(cmd.Context())
		if err != nil {
			return err
		}
		dir, err := c.Stat(cmd.Context(), path)
		if err != nil {
			return fmt.Errorf("resolving %q: %w", path, err)
		}
		kids, err := c.Children(cmd.Context(), dir.ID)
		if err != nil {
			return err
		}
		w := tabwriter.NewWriter(cmd.OutOrStdout(), 2, 4, 2, ' ', 0)
		_, _ = fmt.Fprintln(w, "ID\tKIND\tSIZE\tMODIFIED\tNAME")
		for _, k := range kids {
			_, _ = fmt.Fprintf(w, "%d\t%s\t%d\t%s\t%s\n", k.ID, k.Kind, k.Size, k.ModifiedAt, k.Name)
		}
		return w.Flush()
	},
}

func init() { rootCmd.AddCommand(lsCmd) }
