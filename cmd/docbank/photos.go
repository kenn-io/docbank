package main

import (
	"errors"

	"github.com/spf13/cobra"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/daemonconn"
)

var (
	photoKind      string
	photoRole      string
	photoRevision  int64
	photoExcluded  bool
	photoSidecarOf string
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
	Use:   "inspect <asset-id>",
	Short: "Inspect one asset",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		c, err := daemonconn.Ensure(cmd.Context())
		if err != nil {
			return err
		}
		asset, err := c.PhotoAsset(cmd.Context(), args[0])
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
		if err := requirePhotoRevision(); err != nil {
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
		asset, err := c.AttachPhotoFile(cmd.Context(), args[0], photoRevision, node.ID, photoRole, sidecarOf)
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
		if err := requirePhotoRevision(); err != nil {
			return err
		}
		c, err := daemonconn.Ensure(cmd.Context())
		if err != nil {
			return err
		}
		asset, err := c.DetachPhotoFile(cmd.Context(), args[0], photoRevision, args[1])
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
		if err := requirePhotoRevision(); err != nil {
			return err
		}
		c, err := daemonconn.Ensure(cmd.Context())
		if err != nil {
			return err
		}
		asset, err := c.ExcludePhotoAsset(cmd.Context(), args[0], photoRevision, photoExcluded)
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
		c, node, err := photoNode(cmd, args[0])
		if err != nil {
			return err
		}
		asset, err := c.PromotePhotoNode(cmd.Context(), node.ID, photoRole, photoKind)
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
		if err := requirePhotoRevision(); err != nil {
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
		asset, err := c.SetPhotoDisplay(cmd.Context(), args[0], photoRevision, fileID)
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
		if err := requirePhotoRevision(); err != nil {
			return err
		}
		if args[0] != "raw" && args[0] != "image" {
			return usageError(errors.New("preference must be raw or image"))
		}
		c, err := daemonconn.Ensure(cmd.Context())
		if err != nil {
			return err
		}
		settings, err := c.SetPhotoSettings(cmd.Context(), photoRevision, &args[0])
		if err != nil {
			return err
		}
		return writeCLIJSON(cmd.OutOrStdout(), settings)
	},
}

var photoSettingsResetCmd = &cobra.Command{
	Use:   "reset",
	Short: "Reset the display preference",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		if err := requirePhotoRevision(); err != nil {
			return err
		}
		c, err := daemonconn.Ensure(cmd.Context())
		if err != nil {
			return err
		}
		settings, err := c.SetPhotoSettings(cmd.Context(), photoRevision, nil)
		if err != nil {
			return err
		}
		return writeCLIJSON(cmd.OutOrStdout(), settings)
	},
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

func requirePhotoRevision() error {
	if photoRevision < 1 {
		return usageError(errors.New("--revision must be a positive integer"))
	}
	return nil
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
	photoExcludeCmd.Flags().BoolVar(&photoExcluded, "excluded", true, "exclude the asset")
	for _, command := range []*cobra.Command{photoAttachCmd, photoDetachCmd, photoExcludeCmd, photoDisplayCmd,
		photoSettingsSetCmd, photoSettingsResetCmd} {
		command.Flags().Int64Var(&photoRevision, "revision", 0, "expected asset or settings revision")
	}
}
