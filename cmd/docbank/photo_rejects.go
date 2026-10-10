package main

import (
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/daemonconn"
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
		raw, err := io.ReadAll(io.LimitReader(reader, (1<<20)+1))
		if err != nil {
			return fmt.Errorf("reading rejects preview: %w", err)
		}
		if len(raw) > 1<<20 {
			return errors.New("rejects preview exceeds 1 MiB")
		}
		var preview api.PhotoRejectsPreflight
		if err := json.Unmarshal(raw, &preview, json.RejectUnknownMembers(true)); err != nil {
			return fmt.Errorf("decoding rejects preview: %w", err)
		}
		result, err := c.MovePhotoRejects(cmd.Context(), api.MovePhotoRejectsRequest{Targets: preview.Targets}, "")
		if err != nil {
			return err
		}
		return writeCLIJSON(cmd.OutOrStdout(), result)
	}
	result, err := c.PhotoRejects(cmd.Context(), request, "")
	if err != nil {
		return err
	}
	return writeCLIJSON(cmd.OutOrStdout(), result)
}
