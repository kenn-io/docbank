package main

import (
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"
	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/docbank/internal/processing"
)

func newRedactionResolveCommand() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{Use: "resolve <preparation.json> <masks.json>", Short: "Review explicit PDF masks before rendering", Args: cobra.ExactArgs(2),
		Long:    "Masks select UTF-8 byte spans, rectangles, or whole pages.\nReview the effective masks and retained text before rendering.",
		Example: "  docbank redaction resolve preparation.json masks.json --json > review.json",
		RunE: func(cmd *cobra.Command, args []string) error {
			request, err := readRedactionInputs(args[0], args[1])
			if err != nil {
				return err
			}
			connection, err := daemonconn.Ensure(cmd.Context())
			if err != nil {
				return err
			}
			defer func() { _ = connection.Close() }()
			review, err := connection.ResolveNativePDF(cmd.Context(), request)
			if err != nil {
				return redactionCLIError(writeRedactionProposal(cmd.ErrOrStderr(), err))
			}
			if asJSON {
				return writeCLIJSON(cmd.OutOrStdout(), review)
			}
			return writeRedactionReview(cmd.OutOrStdout(), review)
		}}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print JSON for saving and rendering")
	return cmd
}

func writeRedactionReview(out io.Writer, review processing.RedactionReview) error {
	if _, err := fmt.Fprintf(out, "Review SHA-256: %s\n", review.SHA256); err != nil {
		return fmt.Errorf("writing redaction review: %w", err)
	}
	for _, page := range review.Resolved.Pages {
		if _, err := fmt.Fprintf(out, "Page %d: %d × %d, frame %s\n", page.Number, page.Width, page.Height, page.FrameSHA256); err != nil {
			return fmt.Errorf("writing redaction review: %w", err)
		}
		for _, box := range review.Resolved.RedactBoxes {
			if box.Page == page.Number {
				if _, err := fmt.Fprintf(out, "  Mask: (%d,%d)–(%d,%d)\n", box.X0, box.Y0, box.X1, box.Y1); err != nil {
					return fmt.Errorf("writing redaction review: %w", err)
				}
			}
		}
		var text strings.Builder
		for _, run := range review.Resolved.Runs {
			if run.Page == page.Number {
				text.WriteString(run.Text)
			}
		}
		if _, err := fmt.Fprintf(out, "  Retained text: %q\n", text.String()); err != nil {
			return fmt.Errorf("writing redaction review: %w", err)
		}
	}
	for _, span := range review.Resolved.Removed {
		if _, err := fmt.Fprintf(out, "Removed bytes: [%d,%d)\n", span.Start, span.End); err != nil {
			return fmt.Errorf("writing redaction review: %w", err)
		}
	}
	return nil
}
