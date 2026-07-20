package main

import (
	"fmt"

	"github.com/spf13/cobra"

	"go.kenn.io/docbank/internal/client"
	"go.kenn.io/docbank/internal/store"
)

var revertJSON bool

var revertCmd = &cobra.Command{
	Use:   "revert <vault-path-or-id> <version-id>",
	Short: "Create a new current version from an immutable prior version",
	Args:  cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		selector, err := parseNodeSelector(args[0])
		if err != nil {
			return err
		}
		if !client.IsCanonicalUUIDv4(args[1]) {
			return usageError(fmt.Errorf(
				"reversion source %q must be a canonical UUIDv4", args[1]))
		}
		c, err := client.Ensure(cmd.Context())
		if err != nil {
			return err
		}
		target, err := selector.resolve(cmd.Context(), c)
		if err != nil {
			return err
		}
		if target.Kind != "file" {
			return fmt.Errorf("%q: %w", args[0], store.ErrNotFile)
		}
		receipt, err := c.RevertContent(
			cmd.Context(), target.ID, target.Revision, args[1],
		)
		if err != nil {
			return fmt.Errorf("reverting %q: %w", args[0], err)
		}
		if revertJSON {
			return writeVersionJSON(cmd.OutOrStdout(), receipt)
		}
		_, err = fmt.Fprintf(cmd.OutOrStdout(),
			"reverted %s to source version %s as new version %s (revision %d, %s, sha256 %s)\n",
			target.Path, receipt.SourceVersion.ID, receipt.Version.ID, receipt.Node.Revision,
			formatBackupBytes(receipt.Version.Size), receipt.Version.BlobHash)
		if err != nil {
			return fmt.Errorf("writing reversion receipt: %w", err)
		}
		return nil
	},
}

func init() {
	revertCmd.Flags().BoolVar(&revertJSON, "json", false, "emit a machine-readable reversion receipt")
	rootCmd.AddCommand(revertCmd)
}
