package cmd

import (
	"fmt"

	"github.com/spf13/cobra"
)

var mvCmd = &cobra.Command{
	Use:   "mv <src-path> <dest-path>",
	Short: "Move or rename a node (metadata only; bytes never move)",
	Args:  cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		v, err := openVault()
		if err != nil {
			return err
		}
		defer func() { _ = v.close() }()

		ctx := cmd.Context()
		moved, err := v.store.MovePath(ctx, args[0], args[1])
		if err != nil {
			return err
		}
		p, err := v.store.Path(ctx, moved.ID)
		if err != nil {
			return err
		}
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "moved [%d] %s\n", moved.ID, p)
		return nil
	},
}

func init() { rootCmd.AddCommand(mvCmd) }
