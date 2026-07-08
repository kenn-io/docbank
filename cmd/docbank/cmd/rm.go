package cmd

import (
	"fmt"

	"github.com/spf13/cobra"

	"go.kenn.io/docbank/internal/client"
)

var rmCmd = &cobra.Command{
	Use:   "rm <path>",
	Short: "Move a node (and its subtree) to the trash",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		c, err := client.Ensure(cmd.Context())
		if err != nil {
			return err
		}
		n, err := c.Stat(cmd.Context(), args[0])
		if err != nil {
			return fmt.Errorf("resolving %q: %w", args[0], err)
		}
		if _, err := c.Trash(cmd.Context(), n.ID, n.Revision); err != nil {
			return err
		}
		_, _ = fmt.Fprintf(cmd.OutOrStdout(),
			"trashed [%d] %s (restore with: docbank restore %d)\n", n.ID, args[0], n.ID)
		return nil
	},
}

func init() { rootCmd.AddCommand(rmCmd) }
