package main

import (
	"errors"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/client"
)

var mvJSON bool

var mvCmd = &cobra.Command{
	Use:   "mv <source-path-or-id> <dest-path>",
	Short: "Move or rename a node (metadata only; bytes never move)",
	Args:  cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		source, err := parseNodeSelector(args[0])
		if err != nil {
			return err
		}
		if !strings.HasPrefix(args[1], "/") {
			return usageError(errors.New("move destination must be an absolute virtual path"))
		}
		c, err := client.Ensure(cmd.Context())
		if err != nil {
			return err
		}
		var moved api.Node
		if source.isID() {
			current, resolveErr := source.resolve(cmd.Context(), c)
			if resolveErr != nil {
				return resolveErr
			}
			moved, err = c.MoveToPath(
				cmd.Context(), current.ID, current.Revision, args[1],
			)
		} else {
			moved, err = c.MovePath(cmd.Context(), source.path, args[1])
		}
		if err != nil {
			return err
		}
		if mvJSON {
			return writeCLIJSON(cmd.OutOrStdout(), moved)
		}
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "moved [%s] %s\n",
			formatNodeSelector(moved.ID), moved.Path)
		return nil
	},
}

func init() {
	mvCmd.Flags().BoolVar(&mvJSON, "json", false, "emit a machine-readable node receipt")
	rootCmd.AddCommand(mvCmd)
}
