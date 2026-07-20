package main

import (
	"fmt"

	"github.com/spf13/cobra"

	"go.kenn.io/docbank/internal/client"
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
		c, err := client.Ensure(cmd.Context())
		if err != nil {
			return err
		}
		n, err := c.Node(cmd.Context(), id)
		if err != nil {
			return err
		}
		restored, err := c.Restore(cmd.Context(), id, n.Revision)
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
