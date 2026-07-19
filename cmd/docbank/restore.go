package main

import (
	"errors"
	"fmt"
	"strconv"

	"github.com/spf13/cobra"

	"go.kenn.io/docbank/internal/client"
)

var restoreJSON bool

var restoreCmd = &cobra.Command{
	Use:   "restore <id>",
	Short: "Restore a trashed node to its original location",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		id, err := strconv.ParseInt(args[0], 10, 64)
		if err != nil || id < 1 {
			if err == nil {
				err = errors.New("node ID must be positive")
			}
			return usageError(fmt.Errorf("invalid node id %q: %w", args[0], err))
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
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "restored [%d] %s\n", restored.ID, restored.Path)
		return nil
	},
}

func init() {
	restoreCmd.Flags().BoolVar(&restoreJSON, "json", false,
		"emit a machine-readable node receipt")
	rootCmd.AddCommand(restoreCmd)
}
