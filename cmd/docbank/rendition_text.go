package main

import (
	"errors"
	"fmt"
	"uuid"

	"github.com/spf13/cobra"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/daemonconn"
)

var (
	renditionTextVaultID      string
	renditionTextNodeID       int64
	renditionTextVersionID    string
	renditionTextAttachmentID string
	renditionTextOffset       int
	renditionTextMaxChars     int
	renditionTextJSON         bool
)

var renditionTextCmd = &cobra.Command{
	Use: "text", Short: "Read a bounded Unicode window from one exact rendition",
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		vault, err := uuid.Parse(renditionTextVaultID)
		if err != nil || vault.String() != renditionTextVaultID {
			return usageError(errors.New("--vault-id must be a canonical UUID"))
		}
		if renditionTextNodeID < 1 {
			return usageError(errors.New("--node-id must be positive"))
		}
		version, err := uuid.Parse(renditionTextVersionID)
		if err != nil || version.String() != renditionTextVersionID {
			return usageError(errors.New("--source-version must be a canonical UUID"))
		}
		if !canonicalSHA256(renditionTextAttachmentID) {
			return usageError(errors.New("--attachment-id must be lowercase SHA-256"))
		}
		if renditionTextOffset < 0 || renditionTextOffset > 1<<31-1 {
			return usageError(errors.New("--offset must be between 0 and 2147483647"))
		}
		if renditionTextMaxChars < 1 || renditionTextMaxChars > 16000 {
			return usageError(errors.New("--max-chars must be between 1 and 16000"))
		}
		c, err := daemonconn.Ensure(cmd.Context())
		if err != nil {
			return err
		}
		window, err := c.RenditionTextWindow(cmd.Context(), api.RenditionWindowRequest{
			VaultID: renditionTextVaultID, NodeID: renditionTextNodeID,
			ContentVersionID: renditionTextVersionID, AttachmentID: renditionTextAttachmentID,
			Offset: renditionTextOffset, MaxChars: renditionTextMaxChars,
		})
		if err != nil {
			return err
		}
		if renditionTextJSON {
			return writeCLIJSON(cmd.OutOrStdout(), window)
		}
		_, err = fmt.Fprint(cmd.OutOrStdout(), window.Text)
		if err != nil {
			return fmt.Errorf("writing rendition text: %w", err)
		}
		return nil
	},
}

func init() {
	renditionTextCmd.Flags().StringVar(&renditionTextVaultID, "vault-id", "", "exact vault ID")
	renditionTextCmd.Flags().Int64Var(&renditionTextNodeID, "node-id", 0, "exact document node ID")
	renditionTextCmd.Flags().StringVar(&renditionTextVersionID, "source-version", "", "exact content version ID")
	renditionTextCmd.Flags().StringVar(&renditionTextAttachmentID, "attachment-id", "", "exact rendition attachment ID")
	renditionTextCmd.Flags().IntVar(&renditionTextOffset, "offset", 0, "Unicode character offset")
	renditionTextCmd.Flags().IntVar(&renditionTextMaxChars, "max-chars", 16000, "maximum Unicode characters (1-16000)")
	renditionTextCmd.Flags().BoolVar(&renditionTextJSON, "json", false, "emit the complete window as JSON")
	renditionCmd.AddCommand(renditionTextCmd)
}
