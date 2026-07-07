package cmd

import (
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"go.kenn.io/docbank/internal/store"
)

var catCmd = &cobra.Command{
	Use:   "cat <path>",
	Short: "Write a file's content to stdout",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		v, err := openVault()
		if err != nil {
			return err
		}
		defer func() { _ = v.close() }()

		n, err := v.store.NodeByPath(cmd.Context(), args[0])
		if err != nil {
			return fmt.Errorf("resolving %q: %w", args[0], err)
		}
		if n.IsDir() {
			return fmt.Errorf("%q: %w", args[0], store.ErrNotFile)
		}
		f, err := v.blobs.Open(n.BlobHash)
		if err != nil {
			return err
		}
		defer func() { _ = f.Close() }()
		if _, err := io.Copy(cmd.OutOrStdout(), f); err != nil {
			return fmt.Errorf("streaming %q: %w", args[0], err)
		}
		return nil
	},
}

func init() { rootCmd.AddCommand(catCmd) }
