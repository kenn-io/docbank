package main

import (
	"errors"
	"fmt"
	"io"
	"syscall"

	"github.com/spf13/cobra"

	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/docbank/internal/store"
)

var catCmd = &cobra.Command{
	Example: `  docbank cat /cases/acme/notes.txt
  docbank cat id:12 > invoice.pdf`,
	Long: `Streams the current version's original bytes. For binary files prefer
"docbank get" (verified copy on disk); for extracted text of PDFs and Office
files use "docbank rendition window"; for an older version use
"docbank versions cat".`,
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
			reportFailure(cmd, c, "open", err, false)
			return err
		}
		if n.Kind == "dir" {
			err := fmt.Errorf("%q: %w", args[0], store.ErrNotFile)
			reportFailure(cmd, c, "open", err, false)
			return err
		}
		// Read the immutable version selected by Stat. A concurrent replacement
		// may advance the node, but it cannot make this stream silently switch
		// to different bytes.
		rc, err := c.VersionContent(cmd.Context(), n.CurrentVersionID)
		if err != nil {
			reportFailure(cmd, c, "open", err, false)
			return err
		}
		defer func() { _ = rc.Close() }()
		output := &catOutput{Writer: cmd.OutOrStdout()}
		if _, err := rc.CopyVerified(output); err != nil {
			if !errors.Is(output.err, io.ErrClosedPipe) && !errors.Is(output.err, syscall.EPIPE) {
				reportFailure(cmd, c, "open", err, false)
			}
			return fmt.Errorf("streaming %q: %w", args[0], err)
		}
		reportAction(cmd, c, "document_opened", "", "", false)
		return nil
	},
}

type catOutput struct {
	io.Writer

	err error
}

func (w *catOutput) Write(p []byte) (int, error) {
	n, err := w.Writer.Write(p)
	w.err = err
	return n, err
}

func init() { rootCmd.AddCommand(catCmd) }
