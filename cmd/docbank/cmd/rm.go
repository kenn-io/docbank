package cmd

import (
	"fmt"

	"github.com/spf13/cobra"
)

var rmCmd = &cobra.Command{
	Use:   "rm <path>",
	Short: "Move a node (and its subtree) to the trash",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		v, err := openVault()
		if err != nil {
			return err
		}
		defer func() { _ = v.close() }()

		n, err := v.store.TrashPath(cmd.Context(), args[0])
		if err != nil {
			return err
		}
		_, _ = fmt.Fprintf(cmd.OutOrStdout(),
			"trashed [%d] %s (restore with: docbank restore %d)\n", n.ID, args[0], n.ID)
		return nil
	},
}

func init() { rootCmd.AddCommand(rmCmd) }
