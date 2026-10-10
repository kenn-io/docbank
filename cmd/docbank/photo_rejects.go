package main

import (
	"encoding/json/v2"
	"errors"
	"fmt"
	"os"

	"github.com/spf13/cobra"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/docbank/internal/store"
)

func init() {
	var raw, confirm string
	command := &cobra.Command{Use: "rejects", Short: "Preview rejects or move previewed photos to trash", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		c, err := daemonconn.Ensure(cmd.Context())
		if err != nil {
			return err
		}
		return runPhotoRejects(cmd, c, api.PhotoRejectsRequest{Query: api.QueryPayload(raw)}, confirm)
	}}
	command.Flags().StringVar(&raw, "query", "{}", "QueryV1 JSON selecting the scope")
	command.Flags().StringVar(&confirm, "confirm", "", "preview JSON file, or - for stdin")
	command.MarkFlagsMutuallyExclusive("query", "confirm")
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
		var preview struct {
			Targets []store.PhotoRejectTarget `json:"targets"`
		}
		if err := json.UnmarshalRead(reader, &preview); err != nil {
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
