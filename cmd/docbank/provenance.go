package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/client"
	"go.kenn.io/docbank/internal/store"
)

var (
	provenanceLimit  int
	provenanceOffset int
	provenanceJSON   bool
)

var provenanceCmd = &cobra.Command{
	Use:   "provenance <path-or-id>",
	Short: "Show where a document came from",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if provenanceLimit < 1 || provenanceLimit > store.MaxProvenancePageSize {
			return usageError(fmt.Errorf(
				"--limit must be between 1 and %d", store.MaxProvenancePageSize,
			))
		}
		if provenanceOffset < 0 {
			return usageError(errors.New("--offset must not be negative"))
		}
		selector, err := parseNodeSelector(args[0])
		if err != nil {
			return err
		}
		c, err := client.Ensure(cmd.Context())
		if err != nil {
			return err
		}
		node, err := selector.resolveIncludingTrash(cmd.Context(), c)
		if err != nil {
			return err
		}
		page, err := c.Provenance(cmd.Context(), node.ID, provenanceLimit, provenanceOffset)
		if err != nil {
			return err
		}
		if provenanceJSON {
			return writeProvenanceJSON(cmd, page)
		}
		return writeProvenance(cmd, page)
	},
}

func writeProvenanceJSON(cmd *cobra.Command, page api.ProvenancePage) error {
	enc := json.NewEncoder(cmd.OutOrStdout())
	enc.SetEscapeHTML(false)
	if err := enc.Encode(page); err != nil {
		return fmt.Errorf("writing provenance JSON: %w", err)
	}
	return nil
}

func writeProvenance(cmd *cobra.Command, page api.ProvenancePage) error {
	w := tabwriter.NewWriter(cmd.OutOrStdout(), 2, 4, 2, ' ', 0)
	_, _ = fmt.Fprintf(w, "Node:\t%s\n", formatNodeSelector(page.Node.ID))
	if page.Node.Path == "" {
		_, _ = fmt.Fprintln(w, "State:\ttrashed")
	} else {
		_, _ = fmt.Fprintf(w, "Path:\t%s\n", strconv.Quote(page.Node.Path))
	}
	if page.Total == 0 {
		_, _ = fmt.Fprintln(w, "Provenance:\tnone recorded")
	} else if len(page.Items) == 0 {
		_, _ = fmt.Fprintf(w, "Provenance:\tno facts at offset %d (%d total)\n",
			page.Offset, page.Total)
	} else {
		for i, fact := range page.Items {
			if i > 0 {
				_, _ = fmt.Fprintln(w)
			}
			state := "superseded"
			if fact.Active {
				state = "active"
			}
			_, _ = fmt.Fprintf(w, "Provenance:\t%s\n", fact.Identity)
			_, _ = fmt.Fprintf(w, "Status:\t%s\n", state)
			_, _ = fmt.Fprintf(w, "Ingest:\t%s at %s\n", fact.IngestID, fact.IngestStartedAt)
			_, _ = fmt.Fprintf(w, "Source:\t%s / %s\n",
				strconv.Quote(fact.SourceKind), strconv.Quote(fact.SourceDescription))
			_, _ = fmt.Fprintf(w, "Original path:\t%s\n", strconv.Quote(fact.OriginalPath))
			if fact.OriginalMTime != nil {
				_, _ = fmt.Fprintf(w, "Original modified:\t%s\n", *fact.OriginalMTime)
			}
			if fact.Supersedes != nil {
				_, _ = fmt.Fprintf(w, "Supersedes:\t%s\n", *fact.Supersedes)
			}
		}
	}
	if err := w.Flush(); err != nil {
		return fmt.Errorf("writing provenance: %w", err)
	}
	if page.Offset+len(page.Items) < page.Total {
		_, _ = fmt.Fprintf(cmd.OutOrStdout(),
			"showing %d-%d of %d (use --offset to continue)\n",
			page.Offset+1, page.Offset+len(page.Items), page.Total)
	}
	return nil
}

func init() {
	provenanceCmd.Flags().IntVar(&provenanceLimit, "limit", 100,
		"maximum provenance records to return (1-1000)")
	provenanceCmd.Flags().IntVar(&provenanceOffset, "offset", 0,
		"number of provenance records to skip")
	provenanceCmd.Flags().BoolVar(&provenanceJSON, "json", false,
		"emit machine-readable JSON")
	rootCmd.AddCommand(provenanceCmd)
}
