package main

import (
	"fmt"
	"strconv"

	"github.com/spf13/cobra"

	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/apiclient"
	"go.kenn.io/docbank/internal/daemonconn"
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
		c, err := daemonconn.Ensure(cmd.Context())
		if err != nil {
			return err
		}
		var n *api.Node
		if selector.isID() {
			current, resolveErr := selector.resolve(cmd.Context(), c)
			if resolveErr != nil {
				return resolveErr
			}
			n, err = c.API().TrashNode(cmd.Context(), &apiclient.TrashNodeRequestOptions{PathParams: &apiclient.TrashNodePath{ID: current.ID}, Header: &apiclient.TrashNodeHeaders{IfMatch: strconv.Quote(strconv.FormatInt(current.Revision, 10))}})

		} else {
			n, err = c.API().TrashPath(cmd.Context(), &apiclient.TrashPathRequestOptions{Body: &apiclient.TrashPathBody{Path: selector.path}})

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
