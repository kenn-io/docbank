package main

import (
	"errors"
	"strings"

	"github.com/spf13/cobra"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/docbank/internal/store"
)

var (
	photoKind                   string
	photoRole                   string
	photoRevision               int64
	photoExcluded               bool
	photoSidecarOf              string
	photoClearDependentSidecars bool
)

var photosCmd = &cobra.Command{
	Use:   "photos",
	Short: "Manage photo assets",
	Args:  cobra.NoArgs,
	RunE:  func(cmd *cobra.Command, _ []string) error { return cmd.Help() },
}

var photoAssetsCmd = &cobra.Command{
	Use:   "assets",
	Short: "Inspect and mutate photo assets",
	Args:  cobra.NoArgs,
	RunE:  func(cmd *cobra.Command, _ []string) error { return cmd.Help() },
}

var photoCreateCmd = &cobra.Command{
	Use:   "create <node-selector>",
	Short: "Create an asset for one file",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		c, node, err := photoNode(cmd, args[0])
		if err != nil {
			return err
		}
		asset, err := c.CreatePhotoAsset(cmd.Context(), node.ID, photoRole, photoKind)
		if err != nil {
			return err
		}
		return writeCLIJSON(cmd.OutOrStdout(), asset)
	},
}

var photoInspectCmd = &cobra.Command{
	Use:   "inspect <asset-id|node-selector>",
	Short: "Inspect one asset by its ID or by a member file",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		var (
			c     *daemonconn.Connection
			asset api.PhotoAsset
			err   error
		)
		if strings.HasPrefix(args[0], nodeIDSelectorPrefix) || strings.HasPrefix(args[0], "/") {
			var selector nodeSelector
			if selector, err = parseNodeSelector(args[0]); err != nil {
				return err
			}
			if c, err = daemonconn.Ensure(cmd.Context()); err != nil {
				return err
			}
			// Read-only: a stable ID may still name a trashed member file.
			var node api.Node
			if node, err = selector.resolveIncludingTrash(cmd.Context(), c); err == nil {
				asset, err = c.PhotoAssetForNode(cmd.Context(), node.ID)
			}
		} else if c, err = daemonconn.Ensure(cmd.Context()); err == nil {
			asset, err = c.PhotoAsset(cmd.Context(), args[0])
		}
		if err != nil {
			return err
		}
		return writeCLIJSON(cmd.OutOrStdout(), asset)
	},
}

var photoAttachCmd = &cobra.Command{
	Use:   "attach <asset-id> <node-selector>",
	Short: "Attach one file to an asset",
	Args:  cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		if err := checkPhotoRevisionFlag(cmd); err != nil {
			return err
		}
		c, node, err := photoNode(cmd, args[1])
		if err != nil {
			return err
		}
		var sidecarOf *string
		if photoSidecarOf != "" {
			sidecarOf = &photoSidecarOf
		}
		asset, err := withPhotoRevision(cmd, photoAssetRevision(cmd, c, args[0]), func(revision *int64) (api.PhotoAsset, error) {
			return c.AttachPhotoFile(cmd.Context(), args[0], *revision, node.ID, photoRole, sidecarOf)
		})
		if err != nil {
			return err
		}
		return writeCLIJSON(cmd.OutOrStdout(), asset)
	},
}

var photoDetachCmd = &cobra.Command{
	Use:   "detach <asset-id> <file-id>",
	Short: "Detach one file from an asset",
	Args:  cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		if err := checkPhotoRevisionFlag(cmd); err != nil {
			return err
		}
		c, err := daemonconn.Ensure(cmd.Context())
		if err != nil {
			return err
		}
		asset, err := withPhotoRevision(cmd, photoAssetRevision(cmd, c, args[0]), func(revision *int64) (api.PhotoAsset, error) {
			return c.DetachPhotoFile(cmd.Context(), args[0], *revision, args[1], photoClearDependentSidecars)
		})
		if err != nil {
			return err
		}
		return writeCLIJSON(cmd.OutOrStdout(), asset)
	},
}

var photoExcludeCmd = &cobra.Command{
	Use:   "exclude <asset-id>",
	Short: "Exclude or include an asset",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if err := checkPhotoRevisionFlag(cmd); err != nil {
			return err
		}
		c, err := daemonconn.Ensure(cmd.Context())
		if err != nil {
			return err
		}
		asset, err := withPhotoRevision(cmd, photoAssetRevision(cmd, c, args[0]), func(revision *int64) (api.PhotoAsset, error) {
			return c.ExcludePhotoAsset(cmd.Context(), args[0], *revision, photoExcluded)
		})
		if err != nil {
			return err
		}
		return writeCLIJSON(cmd.OutOrStdout(), asset)
	},
}

var photoPromoteCmd = &cobra.Command{
	Use:   "promote <node-selector>",
	Short: "Promote one file into an asset",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if err := checkPhotoRevisionFlag(cmd); err != nil {
			return err
		}
		c, node, err := photoNode(cmd, args[0])
		if err != nil {
			return err
		}
		current := func() (*int64, error) {
			asset, err := c.PhotoAssetForNode(cmd.Context(), node.ID)
			if errors.Is(err, store.ErrNotFound) {
				return nil, nil //nolint:nilnil // an unowned node has no asset yet, so promotion sends no revision
			}
			if err != nil {
				return nil, err
			}
			return &asset.Revision, nil
		}
		asset, err := withPhotoRevision(cmd, current, func(revision *int64) (api.PhotoAsset, error) {
			return c.PromotePhotoNode(cmd.Context(), node.ID, revision, photoRole, photoKind)
		})
		if err != nil {
			return err
		}
		return writeCLIJSON(cmd.OutOrStdout(), asset)
	},
}

var photoDisplayCmd = &cobra.Command{
	Use:   "display <asset-id> [file-id]",
	Short: "Set or reset an asset display override",
	Args:  cobra.RangeArgs(1, 2),
	RunE: func(cmd *cobra.Command, args []string) error {
		if err := checkPhotoRevisionFlag(cmd); err != nil {
			return err
		}
		c, err := daemonconn.Ensure(cmd.Context())
		if err != nil {
			return err
		}
		var fileID *string
		if len(args) == 2 {
			fileID = &args[1]
		}
		asset, err := withPhotoRevision(cmd, photoAssetRevision(cmd, c, args[0]), func(revision *int64) (api.PhotoAsset, error) {
			return c.SetPhotoDisplay(cmd.Context(), args[0], *revision, fileID)
		})
		if err != nil {
			return err
		}
		return writeCLIJSON(cmd.OutOrStdout(), asset)
	},
}

var photoSettingsCmd = &cobra.Command{
	Use:   "settings",
	Short: "Inspect or change the inherited display preference",
	Args:  cobra.NoArgs,
	RunE:  func(cmd *cobra.Command, _ []string) error { return cmd.Help() },
}

var photoSettingsShowCmd = &cobra.Command{
	Use:   "show",
	Short: "Show the display preference",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		c, err := daemonconn.Ensure(cmd.Context())
		if err != nil {
			return err
		}
		settings, err := c.PhotoSettings(cmd.Context())
		if err != nil {
			return err
		}
		return writeCLIJSON(cmd.OutOrStdout(), settings)
	},
}

var photoSettingsSetCmd = &cobra.Command{
	Use:   "set <raw|image>",
	Short: "Set the display preference",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if err := checkPhotoRevisionFlag(cmd); err != nil {
			return err
		}
		if args[0] != "raw" && args[0] != "image" {
			return usageError(errors.New("preference must be raw or image"))
		}
		return writePhotoSettings(cmd, &args[0])
	},
}

var photoSettingsResetCmd = &cobra.Command{
	Use:   "reset",
	Short: "Reset the display preference",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		if err := checkPhotoRevisionFlag(cmd); err != nil {
			return err
		}
		return writePhotoSettings(cmd, nil)
	},
}

func writePhotoSettings(cmd *cobra.Command, preference *string) error {
	c, err := daemonconn.Ensure(cmd.Context())
	if err != nil {
		return err
	}
	current := func() (*int64, error) {
		settings, err := c.PhotoSettings(cmd.Context())
		if err != nil {
			return nil, err
		}
		return &settings.Revision, nil
	}
	settings, err := withPhotoRevision(cmd, current, func(revision *int64) (api.PhotoSettings, error) {
		return c.SetPhotoSettings(cmd.Context(), *revision, preference)
	})
	if err != nil {
		return err
	}
	return writeCLIJSON(cmd.OutOrStdout(), settings)
}

func photoNode(cmd *cobra.Command, raw string) (*daemonconn.Connection, api.Node, error) {
	selector, err := parseNodeSelector(raw)
	if err != nil {
		return nil, api.Node{}, err
	}
	c, err := daemonconn.Ensure(cmd.Context())
	if err != nil {
		return nil, api.Node{}, err
	}
	node, err := selector.resolve(cmd.Context(), c)
	if err != nil {
		return nil, api.Node{}, err
	}
	return c, node, nil
}

func checkPhotoRevisionFlag(cmd *cobra.Command) error {
	if cmd.Flags().Changed("revision") && photoRevision < 1 {
		return usageError(errors.New("--revision must be a positive integer"))
	}
	return nil
}

func photoAssetRevision(cmd *cobra.Command, c *daemonconn.Connection, assetID string) func() (*int64, error) {
	return func() (*int64, error) {
		asset, err := c.PhotoAsset(cmd.Context(), assetID)
		if err != nil {
			return nil, err
		}
		return &asset.Revision, nil
	}
}

// withPhotoRevision sends an explicit --revision unchanged. Without one it
// reads the current revision and retries once when a concurrent write makes
// that revision stale.
func withPhotoRevision[T any](cmd *cobra.Command, current func() (*int64, error), write func(*int64) (T, error)) (T, error) {
	if cmd.Flags().Changed("revision") {
		return write(&photoRevision)
	}
	for attempt := 0; ; attempt++ {
		revision, err := current()
		if err != nil {
			var zero T
			return zero, err
		}
		result, err := write(revision)
		if attempt == 0 && errors.Is(err, store.ErrStaleRevision) {
			continue
		}
		return result, err
	}
}

func init() {
	photoAssetsCmd.AddCommand(photoCreateCmd, photoInspectCmd, photoAttachCmd, photoDetachCmd,
		photoExcludeCmd, photoPromoteCmd, photoDisplayCmd)
	photoSettingsCmd.AddCommand(photoSettingsShowCmd, photoSettingsSetCmd, photoSettingsResetCmd)
	photosCmd.AddCommand(photoAssetsCmd, photoSettingsCmd)
	rootCmd.AddCommand(photosCmd)

	for _, command := range []*cobra.Command{photoCreateCmd, photoPromoteCmd} {
		command.Flags().StringVar(&photoKind, "kind", "", "asset kind: photo or video")
		command.Flags().StringVar(&photoRole, "role", "", "file role: raw, image, video, or sidecar")
	}
	photoAttachCmd.Flags().StringVar(&photoRole, "role", "", "file role: raw, image, video, or sidecar")
	photoAttachCmd.Flags().StringVar(&photoSidecarOf, "sidecar-of-file-id", "", "same-asset RAW file ID for a sidecar")
	photoDetachCmd.Flags().BoolVar(&photoClearDependentSidecars, "clear-dependent-sidecars", false, "detach dependent sidecars with a RAW file")
	photoExcludeCmd.Flags().BoolVar(&photoExcluded, "excluded", true, "exclude the asset")
	for _, command := range []*cobra.Command{photoAttachCmd, photoDetachCmd, photoExcludeCmd, photoDisplayCmd,
		photoSettingsSetCmd, photoSettingsResetCmd} {
		command.Flags().Int64Var(&photoRevision, "revision", 0, "expected asset or settings revision (default: current, retried once)")
	}
	photoPromoteCmd.Flags().Int64Var(&photoRevision, "revision", 0, "expected existing asset revision (default: current, retried once)")
}
