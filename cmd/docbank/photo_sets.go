package main

import (
	"errors"
	"strings"

	"github.com/spf13/cobra"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/docbank/internal/query"
)

func photoAlbumRevision(cmd *cobra.Command, c *daemonconn.Connection, id string) func() (*int64, error) {
	return func() (*int64, error) {
		album, err := c.PhotoAlbum(cmd.Context(), id)
		if err != nil {
			return nil, err
		}
		return &album.Revision, nil
	}
}

func photoAlbumWrite(use, short, example string, args cobra.PositionalArgs, write func(*cobra.Command, *daemonconn.Connection, []string, int64) (api.PhotoAlbum, error)) *cobra.Command {
	command := &cobra.Command{
		Use:     use,
		Short:   short,
		Example: example,
		Args:    args,
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := checkPhotoRevisionFlag(cmd); err != nil {
				return err
			}
			c, err := daemonconn.Ensure(cmd.Context())
			if err != nil {
				return err
			}
			album, err := withPhotoRevision(cmd, photoAlbumRevision(cmd, c, args[0]), func(revision *int64) (api.PhotoAlbum, error) { return write(cmd, c, args, *revision) })
			if err != nil {
				return err
			}
			return writeCLIJSON(cmd.OutOrStdout(), album)
		}}
	command.Flags().Int64Var(&photoRevision, "revision", 0, "expected album revision (default: current, retried once)")
	return command
}

func init() {
	albums := &cobra.Command{
		Long: `create returns an album ID; add attaches asset IDs from photos assets inspect.
members browses the album; remove detaches members; delete keeps the photos.
Output: JSON.`,
		Use:   "albums",
		Short: "Organize photos into albums",
		Args:  cobra.NoArgs,
		RunE:  func(cmd *cobra.Command, _ []string) error { return cmd.Help() }}
	list := &cobra.Command{
		Example: `  docbank photos albums list`,
		Use:     "list",
		Short:   "List albums and included counts",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := daemonconn.Ensure(cmd.Context())
			if err != nil {
				return err
			}
			out, err := c.PhotoAlbums(cmd.Context())
			if err != nil {
				return err
			}
			return writeCLIJSON(cmd.OutOrStdout(), out)
		}}
	show := &cobra.Command{
		Example: `  docbank photos albums show <album-id>`,
		Use:     "show <album-id>",
		Short:   "Show an album",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := daemonconn.Ensure(cmd.Context())
			if err != nil {
				return err
			}
			out, err := c.PhotoAlbum(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			return writeCLIJSON(cmd.OutOrStdout(), out)
		}}
	create := &cobra.Command{
		Example: `  docbank photos albums create 'Acme site visit'`,
		Use:     "create <name>",
		Short:   "Create an empty album",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := daemonconn.Ensure(cmd.Context())
			if err != nil {
				return err
			}
			out, err := c.CreatePhotoAlbum(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			return writeCLIJSON(cmd.OutOrStdout(), out)
		}}
	rename := photoAlbumWrite("rename <album-id> <name>", "Rename an album", "  docbank photos albums rename <album-id> 'Acme site visit'", cobra.ExactArgs(2), func(cmd *cobra.Command, c *daemonconn.Connection, args []string, revision int64) (api.PhotoAlbum, error) {
		return c.UpdatePhotoAlbum(cmd.Context(), args[0], revision, api.UpdatePhotoAlbumRequest{Name: &args[1]})
	})
	starred := true
	star := photoAlbumWrite("star <album-id>", "Star or unstar an album", "  docbank photos albums star <album-id> --starred=false", cobra.ExactArgs(1), func(cmd *cobra.Command, c *daemonconn.Connection, args []string, revision int64) (api.PhotoAlbum, error) {
		return c.UpdatePhotoAlbum(cmd.Context(), args[0], revision, api.UpdatePhotoAlbumRequest{Starred: &starred})
	})
	star.Flags().BoolVar(&starred, "starred", true, "star the album")
	cover := photoAlbumWrite("cover <album-id> [asset-id]", "Choose a cover or reset to newest ready member", "  docbank photos albums cover <album-id> <asset-id>", cobra.RangeArgs(1, 2), func(cmd *cobra.Command, c *daemonconn.Connection, args []string, revision int64) (api.PhotoAlbum, error) {
		var id *string
		if len(args) == 2 {
			id = &args[1]
		}
		return c.SetPhotoAlbumCover(cmd.Context(), args[0], revision, id)
	})
	duplicate := photoAlbumWrite("duplicate <album-id> <name>", "Copy an album and its member order", "  docbank photos albums duplicate <album-id> 'Acme copy'", cobra.ExactArgs(2), func(cmd *cobra.Command, c *daemonconn.Connection, args []string, revision int64) (api.PhotoAlbum, error) {
		return c.DuplicatePhotoAlbum(cmd.Context(), args[0], revision, args[1])
	})
	deleteAlbum := photoAlbumWrite("delete <album-id>", "Delete an album and keep its photos", "  docbank photos albums delete <album-id>", cobra.ExactArgs(1), func(cmd *cobra.Command, c *daemonconn.Connection, args []string, revision int64) (api.PhotoAlbum, error) {
		return c.DeletePhotoAlbum(cmd.Context(), args[0], revision)
	})
	for _, add := range []bool{true, false} {
		name := "remove"
		if add {
			name = "add"
		}
		var raw, coverage, profile string
		command := photoAlbumWrite(name+" <album-id> [asset-id ...]", strings.ToUpper(name[:1])+name[1:]+" selected photos or complete search results", "  docbank photos albums "+name+" <album-id> <asset-id>", cobra.MinimumNArgs(1), func(cmd *cobra.Command, c *daemonconn.Connection, args []string, revision int64) (api.PhotoAlbum, error) {
			if (raw == "") == (len(args) == 1) {
				return api.PhotoAlbum{}, usageError(errors.New("provide asset IDs or --query"))
			}
			body := api.PhotoAlbumMembersRequest{AssetIDs: args[1:], Coverage: api.WorkspaceQueryCoverage{Configuration: coverage, ProfileFingerprint: profile}}
			if raw != "" {
				if _, err := query.Parse([]byte(raw)); err != nil {
					return api.PhotoAlbum{}, usageError(err)
				}
				payload := api.QueryPayload(raw)
				body.Query = &payload
			}
			if add {
				return c.AddPhotoAlbumMembers(cmd.Context(), args[0], revision, body)
			}
			return c.RemovePhotoAlbumMembers(cmd.Context(), args[0], revision, body)
		})
		command.Flags().StringVar(&raw, "query", "", "strict QueryV1 JSON selecting the complete result")
		command.Flags().StringVar(&coverage, "coverage", "", "coverage configuration: configured or unconfigured")
		command.Flags().StringVar(&profile, "profile-fingerprint", "", "configured coverage profile fingerprint")
		albums.AddCommand(command)
	}
	var sort, direction, cursor string
	var pageSize int
	members := &cobra.Command{
		Example: `  docbank photos albums members <album-id>`,
		Use:     "members <album-id>",
		Short:   "Browse album members by added, import, or capture date",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if sort != "added_time" && sort != "import_time" && sort != "capture_time" {
				return usageError(errors.New("sort must be added_time, import_time, or capture_time"))
			}
			value := query.Query{V: 1, Syntax: "simple", Mode: "lexical", Filters: query.Filters{SetIDs: []string{args[0]}}, Sort: query.Sort{Field: sort, Direction: direction}}
			canonical, err := query.Canonical(value)
			if err != nil {
				return err
			}
			c, err := daemonconn.Ensure(cmd.Context())
			if err != nil {
				return err
			}
			out, err := c.BrowsePhotoAlbum(cmd.Context(), api.PhotoBrowseRequest{Query: api.QueryPayload(canonical), PageSize: pageSize, Cursor: cursor})
			if err != nil {
				return err
			}
			return writeCLIJSON(cmd.OutOrStdout(), out)
		}}
	members.Flags().StringVar(&sort, "sort", "added_time", "added_time, import_time, or capture_time")
	members.Flags().StringVar(&direction, "direction", "desc", "asc or desc")
	members.Flags().StringVar(&cursor, "cursor", "", "opaque next cursor")
	members.Flags().IntVar(&pageSize, "page-size", 50, "photos per page, 1 to 250")
	albums.AddCommand(list, show, create, rename, star, cover, duplicate, deleteAlbum, members)
	photosCmd.AddCommand(albums)
}
