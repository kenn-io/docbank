package main

import (
	"fmt"
	"strconv"

	"github.com/spf13/cobra"

	"go.kenn.io/docbank/internal/apiclient"
	"go.kenn.io/docbank/internal/daemonconn"
)

var restoreJSON bool

var restoreCmd = &cobra.Command{
	Use:   "restore <node-id>",
	Short: "Restore a trashed node to its original location",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		id, err := parseRestoreNodeID(args[0])
		if err != nil {
			return err
		}
		c, err := daemonconn.Ensure(cmd.Context())
		if err != nil {
			return err
		}
		n, err := c.API().GetNode(cmd.Context(), &apiclient.GetNodeRequestOptions{PathParams: &apiclient.GetNodePath{ID: id}})

		if err != nil {
			return err
		}
		restored, err := c.API().RestoreNode(cmd.Context(), &apiclient.RestoreNodeRequestOptions{PathParams: &apiclient.RestoreNodePath{ID: id}, Header: &apiclient.RestoreNodeHeaders{IfMatch: strconv.Quote(strconv.FormatInt(n.Revision, 10))}})

		if err != nil {
			return err
		}
		if restoreJSON {
			return writeCLIJSON(cmd.OutOrStdout(), restored)
		}
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "restored [%s] %s\n",
			formatNodeSelector(restored.ID), restored.Path)
		return nil
	},
}

func init() {
	restoreCmd.Flags().BoolVar(&restoreJSON, "json", false,
		"emit a machine-readable node receipt")
	rootCmd.AddCommand(restoreCmd)
}
