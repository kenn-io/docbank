package cmd

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"go.kenn.io/docbank/internal/client"
	"go.kenn.io/docbank/internal/store"
)

var treeCmd = &cobra.Command{
	Use:   "tree [path]",
	Short: "Print the virtual tree",
	Args:  cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		path := "/"
		if len(args) == 1 {
			path = args[0]
		}
		c, err := client.Ensure(cmd.Context())
		if err != nil {
			return err
		}
		ctx := cmd.Context()
		root, err := c.Stat(ctx, path)
		if err != nil {
			return fmt.Errorf("resolving %q: %w", path, err)
		}
		if root.Kind != "dir" {
			return fmt.Errorf("%s: %w", path, store.ErrNotDir)
		}
		_, _ = fmt.Fprintln(cmd.OutOrStdout(), path)
		return printTree(ctx, cmd.OutOrStdout(), c, root.ID, 1)
	},
}

func printTree(ctx context.Context, w io.Writer, c *client.Client, dirID int64, depth int) error {
	kids, err := c.Children(ctx, dirID)
	if err != nil {
		return err
	}
	for _, k := range kids {
		_, _ = fmt.Fprintf(w, "%s%s  [%d]\n", strings.Repeat("  ", depth), k.Name, k.ID)
		if k.Kind == "dir" {
			if err := printTree(ctx, w, c, k.ID, depth+1); err != nil {
				return err
			}
		}
	}
	return nil
}

func init() { rootCmd.AddCommand(treeCmd) }
