package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"text/tabwriter"

	"github.com/spf13/cobra"
	"go.kenn.io/kit/packstore"

	"go.kenn.io/docbank/internal/client"
)

const maxReferencesLimit = 1000

var (
	referencesLimit  int
	referencesOffset int
	referencesJSON   bool
)

var referencesCmd = &cobra.Command{
	Use:   "refs <sha256>",
	Short: "Find document versions that retain a content hash",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if _, err := packstore.ParseHash(args[0]); err != nil {
			return usageError(errors.New("content hash must be canonical lowercase SHA-256"))
		}
		if referencesLimit < 1 || referencesLimit > maxReferencesLimit {
			return usageError(fmt.Errorf("--limit must be between 1 and %d", maxReferencesLimit))
		}
		if referencesOffset < 0 {
			return usageError(errors.New("--offset must not be negative"))
		}
		c, err := client.Ensure(cmd.Context())
		if err != nil {
			return err
		}
		page, err := c.ContentReferences(
			cmd.Context(), args[0], referencesLimit, referencesOffset,
		)
		if err != nil {
			return err
		}
		if referencesJSON {
			enc := json.NewEncoder(cmd.OutOrStdout())
			enc.SetEscapeHTML(false)
			if err := enc.Encode(page); err != nil {
				return fmt.Errorf("writing content-reference JSON: %w", err)
			}
			return nil
		}
		if len(page.Items) == 0 {
			_, _ = fmt.Fprintln(cmd.OutOrStdout(), "no authoritative references")
			return nil
		}
		w := tabwriter.NewWriter(cmd.OutOrStdout(), 2, 4, 2, ' ', 0)
		_, _ = fmt.Fprintln(w, "VERSION\tNODE SELECTOR\tNODE REV\tCURRENT\tSTATE\tSIZE\tRECORDED\tPATH")
		for _, ref := range page.Items {
			current := "no"
			if ref.IsCurrent {
				current = "yes"
			}
			state, path := "live", ref.Path
			if ref.Node.TrashedAt != "" {
				state, path = "trashed", "-"
			}
			_, _ = fmt.Fprintf(w, "%s\t%s\t%d\t%s\t%s\t%d\t%s\t%s\n",
				ref.Version.ID, formatNodeSelector(ref.Node.ID), ref.Version.NodeRevision, current,
				state, ref.Version.Size, ref.Version.RecordedAt, path)
		}
		if err := w.Flush(); err != nil {
			return fmt.Errorf("writing content references: %w", err)
		}
		if referencesOffset+len(page.Items) < page.Total {
			_, _ = fmt.Fprintf(cmd.OutOrStdout(),
				"showing %d-%d of %d (use --offset to continue)\n",
				referencesOffset+1, referencesOffset+len(page.Items), page.Total)
		}
		return nil
	},
}

func init() {
	referencesCmd.Flags().IntVar(&referencesLimit, "limit", 100,
		"maximum references to return (1-1000)")
	referencesCmd.Flags().IntVar(&referencesOffset, "offset", 0,
		"number of references to skip")
	referencesCmd.Flags().BoolVar(&referencesJSON, "json", false,
		"emit machine-readable JSON")
	rootCmd.AddCommand(referencesCmd)
}
