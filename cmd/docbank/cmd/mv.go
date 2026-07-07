package cmd

import (
	"errors"
	"fmt"
	"path"

	"github.com/spf13/cobra"

	"go.kenn.io/docbank/internal/store"
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
		src, err := v.store.NodeByPath(ctx, args[0])
		if err != nil {
			return fmt.Errorf("resolving %q: %w", args[0], err)
		}

		destParentID, destName, err := resolveMoveTarget(cmd, v, args[1], src.Name)
		if err != nil {
			return err
		}
		moved, err := v.store.Move(ctx, src.ID, destParentID, destName)
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

// resolveMoveTarget interprets destPath: an existing live directory means
// "move into, keep name"; otherwise its dirname must exist and its basename
// is the new name.
func resolveMoveTarget(cmd *cobra.Command, v *vault, destPath, keepName string) (int64, string, error) {
	ctx := cmd.Context()
	if dest, err := v.store.NodeByPath(ctx, destPath); err == nil {
		if dest.IsDir() {
			return dest.ID, keepName, nil
		}
		return 0, "", fmt.Errorf("destination %q: %w", destPath, store.ErrExists)
	} else if !errors.Is(err, store.ErrNotFound) {
		return 0, "", fmt.Errorf("resolving destination %q: %w", destPath, err)
	}
	parent, err := v.store.NodeByPath(ctx, path.Dir(destPath))
	if err != nil {
		return 0, "", fmt.Errorf("resolving destination parent %q: %w", path.Dir(destPath), err)
	}
	return parent.ID, path.Base(destPath), nil
}

func init() { rootCmd.AddCommand(mvCmd) }
