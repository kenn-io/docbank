package cmd

import (
	"fmt"
	"strconv"

	"github.com/spf13/cobra"
)

var restoreCmd = &cobra.Command{
	Use:   "restore <id>",
	Short: "Restore a trashed node to its original location",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		id, err := strconv.ParseInt(args[0], 10, 64)
		if err != nil {
			return fmt.Errorf("invalid node id %q: %w", args[0], err)
		}
		v, err := openVault()
		if err != nil {
			return err
		}
		defer func() { _ = v.close() }()

		n, err := v.store.Restore(cmd.Context(), id, -1)
		if err != nil {
			return err
		}
		p, err := v.store.Path(cmd.Context(), n.ID)
		if err != nil {
			return err
		}
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "restored [%d] %s\n", n.ID, p)
		return nil
	},
}

func init() { rootCmd.AddCommand(restoreCmd) }
