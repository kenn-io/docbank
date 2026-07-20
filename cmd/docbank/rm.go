package main

import (
	"fmt"

	"github.com/spf13/cobra"

	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/client"
)

var rmJSON bool

var rmCmd = &cobra.Command{
	Use:   "rm <path-or-id>",
	Short: "Move a node (and its subtree) to the trash",
	Long: "Move a node (and its subtree) to recoverable trash. rm never permanently " +
		"deletes metadata or reclaims content; use trash empty, gc, and storage repack " +
		"as separate explicit maintenance steps.",
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		selector, err := parseNodeSelector(args[0])
		if err != nil {
			return err
		}
		c, err := client.Ensure(cmd.Context())
		if err != nil {
			return err
		}
		var n api.Node
		if selector.isID() {
			current, resolveErr := selector.resolve(cmd.Context(), c)
			if resolveErr != nil {
				return resolveErr
			}
			n, err = c.Trash(cmd.Context(), current.ID, current.Revision)
		} else {
			n, err = c.TrashPath(cmd.Context(), selector.path)
		}
		if err != nil {
			return err
		}
		if rmJSON {
			return writeCLIJSON(cmd.OutOrStdout(), n)
		}
		_, _ = fmt.Fprintf(cmd.OutOrStdout(),
			"trashed [%s] %s (restore with: docbank restore %s)\n",
			formatNodeSelector(n.ID), n.Path, formatNodeSelector(n.ID))
		return nil
	},
}

func init() {
	rmCmd.Flags().BoolVar(&rmJSON, "json", false, "emit a machine-readable node receipt")
	rootCmd.AddCommand(rmCmd)
}
