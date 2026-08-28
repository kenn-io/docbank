package main

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"go.kenn.io/docbank/internal/client"
)

var renditionMaxBytes int64

var renditionCmd = &cobra.Command{
	Use:   "rendition",
	Short: "Read verified retained document renditions",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		return cmd.Help()
	},
}

var renditionGetCmd = &cobra.Command{
	Use:   "get <attachment-id>",
	Short: "Write one verified self-describing Markdown rendition to stdout",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if renditionMaxBytes < 1 || renditionMaxBytes > 64<<20 {
			return usageError(errors.New("--max-bytes must be between 1 and 67108864"))
		}
		if !canonicalSHA256(args[0]) {
			return usageError(errors.New("attachment ID must be lowercase SHA-256"))
		}
		c, err := client.Ensure(cmd.Context())
		if err != nil {
			return err
		}
		return runRenditionGet(cmd, c, args[0], renditionMaxBytes)
	},
}

func runRenditionGet(cmd *cobra.Command, c *client.Client, attachmentID string, maxBytes int64) error {
	stream, err := c.Rendition(cmd.Context(), attachmentID, maxBytes)
	if err != nil {
		return err
	}
	_, copyErr := stream.CopyVerified(cmd.OutOrStdout())
	closeErr := stream.Close()
	if copyErr != nil {
		return fmt.Errorf("reading rendition %s: %w", attachmentID, copyErr)
	}
	if closeErr != nil {
		return fmt.Errorf("closing rendition %s: %w", attachmentID, closeErr)
	}
	return nil
}

func init() {
	renditionGetCmd.Flags().Int64Var(&renditionMaxBytes, "max-bytes", 64<<20,
		"maximum complete rendition bytes to accept (1-67108864)")
	renditionCmd.AddCommand(renditionGetCmd)
	rootCmd.AddCommand(renditionCmd)
}
