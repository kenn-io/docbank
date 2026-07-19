package main

import (
	"fmt"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/client"
)

var lsJSON bool

type directoryListing struct {
	Directory api.Node   `json:"directory"`
	Items     []api.Node `json:"items"`
}

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
		if lsJSON {
			return writeCLIJSON(cmd.OutOrStdout(), directoryListing{
				Directory: dir,
				Items:     kids,
			})
		}
		w := tabwriter.NewWriter(cmd.OutOrStdout(), 2, 4, 2, ' ', 0)
		_, _ = fmt.Fprintln(w, "ID\tKIND\tSIZE\tMODIFIED\tNAME")
		for _, k := range kids {
			_, _ = fmt.Fprintf(w, "%d\t%s\t%d\t%s\t%s\n", k.ID, k.Kind, k.Size, k.ModifiedAt, k.Name)
		}
		return w.Flush()
	},
}

func init() {
	lsCmd.Flags().BoolVar(&lsJSON, "json", false, "emit machine-readable JSON")
	rootCmd.AddCommand(lsCmd)
}
