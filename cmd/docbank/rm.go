package main

import (
	"fmt"

	"github.com/spf13/cobra"

	"go.kenn.io/docbank/internal/client"
)

var rmCmd = &cobra.Command{
	Use:   "rm <path>",
	Short: "Move a node (and its subtree) to the trash",
	Long: "Move a node (and its subtree) to recoverable trash. rm never permanently " +
		"deletes metadata or reclaims content; use trash empty, gc, and storage repack " +
		"as separate explicit maintenance steps.",
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		c, err := client.Ensure(cmd.Context())
		if err != nil {
			return err
		}
		n, err := c.TrashPath(cmd.Context(), args[0])
		if err != nil {
			return err
		}
		_, _ = fmt.Fprintf(cmd.OutOrStdout(),
			"trashed [%d] %s (restore with: docbank restore %d)\n", n.ID, args[0], n.ID)
		return nil
	},
}

func init() { rootCmd.AddCommand(rmCmd) }
