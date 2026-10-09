package main

import (
	"fmt"

	"github.com/spf13/cobra"

	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/docbank/internal/store"
)

var catCmd = &cobra.Command{
	Example: `  docbank cat /cases/acme/notes.txt
  docbank cat id:12 > invoice.pdf`,
	Long: `Streams the current version's original bytes. For binary files prefer
"docbank get" (verified copy on disk); for extracted text of PDFs and Office
files use "docbank rendition window"; for an older version "docbank versions cat".`,
	GroupID: groupDocuments,
	Use:     "cat <path-or-id>",
	Short:   "Print a file's original bytes to stdout",
	Args:    cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		selector, err := parseNodeSelector(args[0])
		if err != nil {
			return err
		}
		c, err := daemonconn.Ensure(cmd.Context())
		if err != nil {
			return err
		}
		n, err := selector.resolveIncludingTrash(cmd.Context(), c)
		if err != nil {
			return err
		}
		if n.Kind == "dir" {
			return fmt.Errorf("%q: %w", args[0], store.ErrNotFile)
		}
		// Read the immutable version selected by Stat. A concurrent replacement
		// may advance the node, but it cannot make this stream silently switch
		// to different bytes.
		rc, err := c.VersionContent(cmd.Context(), n.CurrentVersionID)
		if err != nil {
			return err
		}
		defer func() { _ = rc.Close() }()
		if _, err := rc.CopyVerified(cmd.OutOrStdout()); err != nil {
			return fmt.Errorf("streaming %q: %w", args[0], err)
		}
		return nil
	},
}

func init() { rootCmd.AddCommand(catCmd) }
