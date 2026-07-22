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
	Use:   "ls [path-or-id]",
	Short: "List a virtual directory",
	Args:  cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		raw := "/"
		if len(args) == 1 {
			raw = args[0]
		}
		selector, err := parseNodeSelector(raw)
		if err != nil {
			return err
		}
		c, err := client.Ensure(cmd.Context())
		if err != nil {
			return err
		}
		dir, err := selector.resolve(cmd.Context(), c)
		if err != nil {
			return err
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
		_, _ = fmt.Fprintln(w, "SELECTOR\tKIND\tSIZE\tMODIFIED\tNAME")
		for _, k := range kids {
			modifiedAt, err := formatHumanTimestamp(k.ModifiedAt)
			if err != nil {
				return err
			}
			_, _ = fmt.Fprintf(w, "%s\t%s\t%d\t%s\t%s\n",
				formatNodeSelector(k.ID), k.Kind, k.Size, modifiedAt, k.Name)
		}
		return w.Flush()
	},
}

func init() {
	lsCmd.Flags().BoolVar(&lsJSON, "json", false, "emit machine-readable JSON")
	rootCmd.AddCommand(lsCmd)
}
