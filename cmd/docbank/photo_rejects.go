package main

import (
	"encoding/json/v2"
	"errors"
	"fmt"
	"os"

	"github.com/spf13/cobra"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/daemonconn"
)

func init() {
	var raw, confirm, coverage, profile string
	command := &cobra.Command{Use: "rejects", Short: "Preview rejected photos or move previewed photos to trash",
		Long:    "Preview photos whose originals all have reject flags in the query scope.\nMixed-flag photos stay in Docbank. Save the preview JSON, then use --confirm\nto move its targets and sidecars to recoverable trash. Confirmation refuses\nphotos changed since preview and moves at most 1,000 photos and 1,000 live\nfiles.",
		Example: "  docbank photos rejects > preview.json\n  docbank photos rejects --confirm preview.json",
		Args:    cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := daemonconn.Ensure(cmd.Context())
			if err != nil {
				return err
			}
			return runPhotoRejects(cmd, c, api.PhotoRejectsRequest{Query: api.QueryPayload(raw), Coverage: api.WorkspaceQueryCoverage{Configuration: coverage, ProfileFingerprint: profile}}, confirm)
		}}
	command.Flags().StringVar(&raw, "query", "{}", "query JSON selecting the preview scope")
	command.Flags().StringVar(&confirm, "confirm", "", "preview JSON file, or - for stdin")
	command.Flags().StringVar(&coverage, "coverage", "", "coverage configuration: configured or unconfigured")
	command.Flags().StringVar(&profile, "profile-fingerprint", "", "configured coverage profile fingerprint")
	command.MarkFlagsMutuallyExclusive("query", "confirm")
	command.MarkFlagsMutuallyExclusive("coverage", "confirm")
	command.MarkFlagsMutuallyExclusive("profile-fingerprint", "confirm")
	photosCmd.AddCommand(command)
}

func runPhotoRejects(cmd *cobra.Command, c *daemonconn.Connection, request api.PhotoRejectsRequest, confirm string) error {
	if confirm != "" {
		reader := cmd.InOrStdin()
		if confirm != "-" {
			file, err := os.Open(confirm)
			if err != nil {
				return fmt.Errorf("opening rejects preview: %w", err)
			}
			defer func() { _ = file.Close() }()
			reader = file
		}
		var preview api.PhotoRejectsPreflight
		if err := json.UnmarshalRead(reader, &preview, json.RejectUnknownMembers(true)); err != nil {
			return fmt.Errorf("decoding rejects preview: %w", err)
		}
		if preview.Targets == nil {
			return errors.New("decoding rejects preview: missing targets")
		}
		result, err := c.MovePhotoRejects(cmd.Context(), api.MovePhotoRejectsRequest{Targets: preview.Targets})
		if err != nil {
			return err
		}
		return writeCLIJSON(cmd.OutOrStdout(), result)
	}
	result, err := c.PhotoRejects(cmd.Context(), request)
	if err != nil {
		return err
	}
	return writeCLIJSON(cmd.OutOrStdout(), result)
}
