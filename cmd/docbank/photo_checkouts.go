package main

import (
	"errors"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/docbank/internal/store"
)

const photoCheckoutListCommand = "list"

func checkoutSelection(cmd *cobra.Command) (store.PhotoCheckoutSelection, error) {
	assets, _ := cmd.Flags().GetStringSlice("asset")
	albums, _ := cmd.Flags().GetStringSlice("album")
	all, _ := cmd.Flags().GetBool("all")
	years, _ := cmd.Flags().GetString("years")
	selection := store.PhotoCheckoutSelection{AssetIDs: assets, AlbumIDs: albums, All: all}
	if years != "" {
		from, to, found := strings.Cut(years, ":")
		if !found {
			to = from
		}
		start, err := strconv.Atoi(from)
		if err != nil {
			return selection, errors.New("years must be YYYY or YYYY:YYYY")
		}
		end, err := strconv.Atoi(to)
		if err != nil {
			return selection, errors.New("years must be YYYY or YYYY:YYYY")
		}
		selection.Years = &store.PhotoCheckoutYears{From: start, To: end}
	}
	return selection, store.ValidatePhotoCheckoutSelection(selection)
}

func newPhotoCheckoutCmd() *cobra.Command {
	command := &cobra.Command{Use: "checkout", Short: "Create writable photo copies", Long: `Estimate a selection, then create verified writable copies in an empty folder.
The folder must be on the daemon's computer. List or inspect saved checkouts.`, Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error { return cmd.Help() }}
	for _, create := range []bool{false, true} {
		use, short, args := "estimate", "Estimate selected files and bytes", cobra.NoArgs
		example := "  docbank photos checkout estimate --years 2024:2025"
		if create {
			example = "  docbank photos checkout create ./photos --all --byte-limit 1000000000"
			use, short, args = "create <destination>", "Copy selected photos to an empty daemon-host folder", cobra.ExactArgs(1)
		}
		child := &cobra.Command{Use: use, Short: short, Example: example, Args: args, RunE: func(cmd *cobra.Command, args []string) error {
			selection, err := checkoutSelection(cmd)
			if err != nil {
				return usageError(err)
			}
			var request api.PhotoCheckoutCreateRequest
			if create {
				if args[0] == "" {
					return usageError(errors.New("destination must not be empty"))
				}
				destination, err := filepath.Abs(args[0])
				if err != nil {
					return err
				}
				limit, _ := cmd.Flags().GetInt64("byte-limit")
				if limit < 0 {
					return usageError(errors.New("--byte-limit must be nonnegative"))
				}
				if selection.All && limit == 0 {
					return usageError(errors.New("all requires a positive --byte-limit"))
				}
				request = api.PhotoCheckoutCreateRequest{Selection: selection, Destination: destination, ByteLimit: limit}
			}
			connection, err := daemonconn.Ensure(cmd.Context())
			if err != nil {
				return err
			}
			if create {
				result, err := connection.CreatePhotoCheckout(cmd.Context(), request)
				if err != nil {
					return err
				}
				return writeCLIJSON(cmd.OutOrStdout(), result)
			}
			estimate, err := connection.EstimatePhotoCheckout(cmd.Context(), selection)
			if err != nil {
				return err
			}
			return writeCLIJSON(cmd.OutOrStdout(), estimate)
		}}
		child.Flags().StringSlice("asset", nil, "asset IDs")
		child.Flags().StringSlice("album", nil, "album IDs")
		child.Flags().String("years", "", "capture year or inclusive YYYY:YYYY range")
		child.Flags().Bool("all", false, "select the full visible library")
		if create {
			child.Flags().Int64("byte-limit", 0, "maximum copied bytes; required with --all")
		}
		command.AddCommand(child)
	}
	command.AddCommand(&cobra.Command{Use: photoCheckoutListCommand, Short: "List photo checkouts", Example: "  docbank photos checkout list", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		connection, err := daemonconn.Ensure(cmd.Context())
		if err != nil {
			return err
		}
		out, err := connection.PhotoCheckouts(cmd.Context())
		if err != nil {
			return err
		}
		return writeCLIJSON(cmd.OutOrStdout(), out)
	}})
	command.AddCommand(&cobra.Command{Use: "status <checkout-id>", Short: "Inspect photo checkout progress", Example: "  docbank photos checkout status 00000000-0000-4000-8000-000000000001", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		connection, err := daemonconn.Ensure(cmd.Context())
		if err != nil {
			return err
		}
		out, err := connection.PhotoCheckoutStatus(cmd.Context(), args[0])
		if err != nil {
			return err
		}
		return writeCLIJSON(cmd.OutOrStdout(), out)
	}})
	return command
}

func init() { photosCmd.AddCommand(newPhotoCheckoutCmd()) }
