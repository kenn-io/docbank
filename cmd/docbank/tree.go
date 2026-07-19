package main

import (
	"context"
	"fmt"
	"io"
	"path"
	"strings"

	"github.com/spf13/cobra"

	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/client"
	"go.kenn.io/docbank/internal/store"
)

var treeJSON bool

type treeEntry struct {
	Node  api.Node `json:"node"`
	Path  string   `json:"path"`
	Depth int      `json:"depth"`
}

type treeListing struct {
	Root  api.Node    `json:"root"`
	Items []treeEntry `json:"items"`
}

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
		if treeJSON {
			items := []treeEntry{}
			if err := collectTree(ctx, c, root.Path, root.ID, 1, &items); err != nil {
				return err
			}
			return writeCLIJSON(cmd.OutOrStdout(), treeListing{Root: root, Items: items})
		}
		_, _ = fmt.Fprintln(cmd.OutOrStdout(), path)
		return printTree(ctx, cmd.OutOrStdout(), c, root.ID, 1)
	},
}

func collectTree(
	ctx context.Context,
	c *client.Client,
	parentPath string,
	dirID int64,
	depth int,
	items *[]treeEntry,
) error {
	kids, err := c.Children(ctx, dirID)
	if err != nil {
		return err
	}
	for _, kid := range kids {
		kidPath := path.Join(parentPath, kid.Name)
		*items = append(*items, treeEntry{Node: kid, Path: kidPath, Depth: depth})
		if kid.Kind == "dir" {
			if err := collectTree(ctx, c, kidPath, kid.ID, depth+1, items); err != nil {
				return err
			}
		}
	}
	return nil
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

func init() {
	treeCmd.Flags().BoolVar(&treeJSON, "json", false, "emit machine-readable JSON")
	rootCmd.AddCommand(treeCmd)
}
