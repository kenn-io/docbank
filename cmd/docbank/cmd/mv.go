package cmd

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"go.kenn.io/docbank/internal/client"
	"go.kenn.io/docbank/internal/store"
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
		ctx := cmd.Context()
		src, err := c.Stat(ctx, args[0])
		if err != nil {
			return fmt.Errorf("resolving %q: %w", args[0], err)
		}
		newParentID, newName, err := resolveMoveDest(ctx, c, args[1], src.Name)
		if err != nil {
			return err
		}
		moved, err := c.Move(ctx, src.ID, src.Revision, &newParentID, &newName)
		if err != nil {
			return err
		}
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "moved [%d] %s\n", moved.ID, moved.Path)
		return nil
	},
}

// resolveMoveDest mirrors the store-side MovePath rules this CLI used
// before the daemon: an existing live directory means "move into, keep
// name"; an existing file is a conflict; otherwise the parent must exist
// and the basename becomes the new name. Segments are validated literally
// (no dot-segment Cleaning) via the same NormalizeName the server applies.
func resolveMoveDest(ctx context.Context, c *client.Client, destPath, keepName string) (int64, string, error) {
	segs := splitVirtualPath(destPath)
	for i, seg := range segs {
		norm, err := store.NormalizeName(seg)
		if err != nil {
			return 0, "", fmt.Errorf("destination %q: %w", destPath, err)
		}
		segs[i] = norm
	}
	if dest, err := c.Stat(ctx, destPath); err == nil {
		if dest.Kind == "dir" {
			return dest.ID, keepName, nil
		}
		return 0, "", fmt.Errorf("destination %q: %w", destPath, store.ErrExists)
	} else if !errors.Is(err, store.ErrNotFound) {
		return 0, "", fmt.Errorf("resolving destination %q: %w", destPath, err)
	}
	if len(segs) == 0 {
		return 0, "", fmt.Errorf("destination %q: %w", destPath, store.ErrExists)
	}
	parentPath := "/" + strings.Join(segs[:len(segs)-1], "/")
	parent, err := c.Stat(ctx, parentPath)
	if err != nil {
		return 0, "", fmt.Errorf("resolving destination parent %q: %w", parentPath, err)
	}
	return parent.ID, segs[len(segs)-1], nil
}

// splitVirtualPath is store.splitPath's behavior for the CLI side: "/a/b/"
// → ["a","b"]; "" and "/" → nil.
func splitVirtualPath(path string) []string {
	var segs []string
	for seg := range strings.SplitSeq(path, "/") {
		if seg != "" {
			segs = append(segs, seg)
		}
	}
	return segs
}

func init() { rootCmd.AddCommand(mvCmd) }
