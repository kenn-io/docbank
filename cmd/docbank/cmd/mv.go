package cmd

import (
	"fmt"

	"github.com/spf13/cobra"

	"go.kenn.io/docbank/internal/client"
)

var mvCmd = &cobra.Command{
	Use:   "mv <src-path> <dest-path>",
	Short: "Move or rename a node (metadata only; bytes never move)",
	Args:  cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		c, err := client.Ensure(cmd.Context())
		if err != nil {
			return err
		}
		moved, err := c.MovePath(cmd.Context(), args[0], args[1])
		if err != nil {
			return err
		}
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "moved [%d] %s\n", moved.ID, moved.Path)
		return nil
	},
}

func init() { rootCmd.AddCommand(mvCmd) }
