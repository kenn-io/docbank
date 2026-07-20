package main

import (
	"fmt"

	"github.com/spf13/cobra"

	"go.kenn.io/docbank/internal/client"
	"go.kenn.io/docbank/internal/store"
)

var catCmd = &cobra.Command{
	Use:   "cat <path-or-id>",
	Short: "Write a file's content to stdout",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		selector, err := parseNodeSelector(args[0])
		if err != nil {
			return err
		}
		c, err := client.Ensure(cmd.Context())
		if err != nil {
			return err
		}
		n, err := selector.resolve(cmd.Context(), c)
		if err != nil {
			return err
		}
		if n.Kind == "dir" {
			return fmt.Errorf("%q: %w", args[0], store.ErrNotFile)
		}
		// Read the immutable version selected by Stat. A concurrent replacement
		// may advance the node, but it cannot make this stream silently switch
		// to different bytes.
		rc, err := c.VersionContent(cmd.Context(), n.CurrentVersionID)
		if err != nil {
			return err
		}
		defer func() { _ = rc.Close() }()
		if _, err := rc.CopyVerified(cmd.OutOrStdout()); err != nil {
			return fmt.Errorf("streaming %q: %w", args[0], err)
		}
		return nil
	},
}

func init() { rootCmd.AddCommand(catCmd) }
