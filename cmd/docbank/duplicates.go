package main

import (
	"errors"
	"fmt"
	"text/tabwriter"

	"github.com/spf13/cobra"
	"go.kenn.io/docbank/internal/daemonconn"
)

var duplicatesCmd = &cobra.Command{
	Use: "duplicates", GroupID: groupSearch,
	Short: "Find matching documents",
	Long: `Review duplicate documents without changing their originals.
Use "duplicates docx" to compare complete DOCX package contents.`,
}

func init() {
	var asJSON bool
	var limit, offset int
	cmd := &cobra.Command{
		Use: "docx", Short: "Find DOCX files with identical package contents", Args: cobra.NoArgs,
		Long: `Ignore ZIP timestamps, compression, and entry order when comparing DOCX files.
Every entry name and its complete content must match. Results use cached
metadata and show how many current DOCX files lack a fingerprint.
Original files and their raw SHA-256 hashes remain unchanged.`,
		Example: `  docbank duplicates docx
  docbank duplicates docx --limit 20 --offset 0 --json`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if limit < 1 || limit > 100 || offset < 0 {
				return usageError(errors.New("limit must be 1..100 and offset nonnegative"))
			}
			connection, err := daemonconn.Ensure(cmd.Context())
			if err != nil {
				return err
			}
			page, err := connection.DOCXDuplicates(cmd.Context(), limit, offset)
			if err != nil {
				return err
			}
			if asJSON {
				return writeCLIJSON(cmd.OutOrStdout(), page)
			}
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "%d groups, %d documents; %d DOCX documents without fingerprints\n", page.Total, page.TotalReferences, page.UnfingerprintedReferences)
			w := tabwriter.NewWriter(cmd.OutOrStdout(), 2, 4, 2, ' ', 0)
			for _, group := range page.Items {
				_, _ = fmt.Fprintf(w, "\n%s (%d documents)\n", group.Fingerprint, group.ReferenceCount)
				for _, ref := range group.References {
					_, _ = fmt.Fprintf(w, "%s\t%s\t%d bytes\t%s\n", formatNodeSelector(ref.ID), ref.BlobHash, ref.Size, ref.Path)
				}
				if group.ReferencesTruncated {
					_, _ = fmt.Fprintln(w, "  Additional references omitted (preview limit: 16).")
				}
			}
			if offset+len(page.Items) < page.Total {
				_, _ = fmt.Fprintf(w, "\nMore groups: docbank duplicates docx --limit %d --offset %d\n", limit, offset+len(page.Items))
			}
			return w.Flush()
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print JSON to stdout")
	cmd.Flags().IntVar(&limit, "limit", 50, "maximum groups (1..100)")
	cmd.Flags().IntVar(&offset, "offset", 0, "skip this many groups")
	duplicatesCmd.AddCommand(cmd)
	rootCmd.AddCommand(duplicatesCmd)
}
