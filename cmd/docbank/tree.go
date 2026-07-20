package main

import (
	"context"
	"errors"
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

const (
	defaultTreeDepth      = 4
	defaultTreeMaxEntries = 1000
	treePageSize          = 500
)

var (
	treeDepth      int
	treeMaxEntries int
	treeAll        bool
)

type treeEntry struct {
	Node  api.Node `json:"node"`
	Path  string   `json:"path"`
	Depth int      `json:"depth"`
}

type treeListing struct {
	Root      api.Node       `json:"root"`
	Items     []treeEntry    `json:"items"`
	Truncated bool           `json:"truncated"`
	Omissions []treeOmission `json:"omissions"`
}

type treeOmission struct {
	Path           string `json:"path"`
	Reason         string `json:"reason" enum:"depth_limit,entry_limit"`
	DirectChildren int    `json:"direct_children"`
}

type treeWalker struct {
	client     *client.Client
	maxDepth   int
	maxEntries int
	items      []treeEntry
	omissions  []treeOmission
}

var treeCmd = &cobra.Command{
	Use:   "tree [path]",
	Short: "Print the virtual tree",
	Args:  cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if treeDepth < 1 {
			return usageError(errors.New("tree --depth must be at least 1"))
		}
		if treeMaxEntries < 1 {
			return usageError(errors.New("tree --max-entries must be at least 1"))
		}
		if treeAll && (cmd.Flags().Changed("depth") || cmd.Flags().Changed("max-entries")) {
			return usageError(errors.New("tree --all cannot be combined with --depth or --max-entries"))
		}
		rootPath := "/"
		if len(args) == 1 {
			rootPath = args[0]
		}
		c, err := client.Ensure(cmd.Context())
		if err != nil {
			return err
		}
		ctx := cmd.Context()
		root, err := c.Stat(ctx, rootPath)
		if err != nil {
			return fmt.Errorf("resolving %q: %w", rootPath, err)
		}
		if root.Kind != "dir" {
			return fmt.Errorf("%s: %w", rootPath, store.ErrNotDir)
		}
		walker := treeWalker{
			client: c, maxDepth: treeDepth, maxEntries: treeMaxEntries,
			items: []treeEntry{}, omissions: []treeOmission{},
		}
		if treeAll {
			walker.maxDepth, walker.maxEntries = 0, 0
		}
		if err := walker.walk(ctx, root.Path, root.ID, 1); err != nil {
			return err
		}
		listing := treeListing{
			Root: root, Items: walker.items, Truncated: len(walker.omissions) > 0,
			Omissions: walker.omissions,
		}
		if treeJSON {
			return writeCLIJSON(cmd.OutOrStdout(), listing)
		}
		return writeTree(cmd.OutOrStdout(), listing)
	},
}

func (w *treeWalker) exhausted() bool {
	return w.maxEntries > 0 && len(w.items) >= w.maxEntries
}

func (w *treeWalker) walk(ctx context.Context, parentPath string, dirID int64, depth int) error {
	for offset := 0; ; {
		limit := treePageSize
		if w.maxEntries > 0 {
			limit = min(limit, w.maxEntries-len(w.items))
		}
		if limit == 0 {
			return nil
		}
		page, err := w.client.ChildrenPage(ctx, dirID, limit, offset)
		if err != nil {
			return err
		}
		for i, kid := range page.Items {
			kidPath := path.Join(parentPath, kid.Name)
			w.items = append(w.items, treeEntry{Node: kid, Path: kidPath, Depth: depth})
			if kid.Kind == "dir" {
				switch {
				case w.maxDepth > 0 && depth >= w.maxDepth:
					if err := w.omitChildren(ctx, kidPath, kid.ID, "depth_limit"); err != nil {
						return err
					}
				case w.exhausted():
					if err := w.omitChildren(ctx, kidPath, kid.ID, "entry_limit"); err != nil {
						return err
					}
				default:
					if err := w.walk(ctx, kidPath, kid.ID, depth+1); err != nil {
						return err
					}
				}
			}
			if w.exhausted() {
				if remaining := page.Total - (offset + i + 1); remaining > 0 {
					w.omissions = append(w.omissions, treeOmission{
						Path: parentPath, Reason: "entry_limit", DirectChildren: remaining,
					})
				}
				return nil
			}
		}
		offset += len(page.Items)
		if offset >= page.Total {
			return nil
		}
	}
}

func (w *treeWalker) omitChildren(
	ctx context.Context, dirPath string, dirID int64, reason string,
) error {
	page, err := w.client.ChildrenPage(ctx, dirID, 1, 0)
	if err != nil {
		return err
	}
	if page.Total > 0 {
		w.omissions = append(w.omissions, treeOmission{
			Path: dirPath, Reason: reason, DirectChildren: page.Total,
		})
	}
	return nil
}

func writeTree(w io.Writer, listing treeListing) error {
	if _, err := fmt.Fprintln(w, listing.Root.Path); err != nil {
		return fmt.Errorf("writing tree: %w", err)
	}
	for _, item := range listing.Items {
		if _, err := fmt.Fprintf(w, "%s%s  [%d]\n",
			strings.Repeat("  ", item.Depth), item.Node.Name, item.Node.ID); err != nil {
			return fmt.Errorf("writing tree: %w", err)
		}
	}
	if !listing.Truncated {
		return nil
	}
	if _, err := fmt.Fprintf(w, "... tree truncated at %d boundary(s):\n", len(listing.Omissions)); err != nil {
		return fmt.Errorf("writing tree: %w", err)
	}
	for _, omission := range listing.Omissions {
		reason := "depth limit"
		if omission.Reason == "entry_limit" {
			reason = "entry limit"
		}
		entryWord := "entries"
		if omission.DirectChildren == 1 {
			entryWord = "entry"
		}
		if _, err := fmt.Fprintf(w, "  %s: %d direct %s hidden by %s\n",
			omission.Path, omission.DirectChildren, entryWord, reason); err != nil {
			return fmt.Errorf("writing tree: %w", err)
		}
	}
	if _, err := fmt.Fprintln(w,
		"narrow the path, raise --depth/--max-entries, or use --all deliberately"); err != nil {
		return fmt.Errorf("writing tree: %w", err)
	}
	return nil
}

func init() {
	treeCmd.Flags().BoolVar(&treeJSON, "json", false, "emit machine-readable JSON")
	treeCmd.Flags().IntVarP(&treeDepth, "depth", "L", defaultTreeDepth,
		"maximum depth beneath the root")
	treeCmd.Flags().IntVar(&treeMaxEntries, "max-entries", defaultTreeMaxEntries,
		"maximum nodes to print")
	treeCmd.Flags().BoolVar(&treeAll, "all", false,
		"print the complete subtree without depth or entry limits")
	rootCmd.AddCommand(treeCmd)
}
