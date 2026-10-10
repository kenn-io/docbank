package main

import (
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"go.kenn.io/docbank/internal/apiclient"
	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/docbank/internal/store"
	"go.kenn.io/docbank/report"
)

func newReportShowCommand() *cobra.Command {
	cmd := &cobra.Command{
		Example: `  docbank search-export show <report-id>`,
		Use:     "show <report-id>",
		Short:   "Inspect a live frozen report",
		Args:    cobra.ExactArgs(1)}
	var asJSON bool
	cmd.Flags().BoolVar(&asJSON, "json", false, "print JSON to stdout")
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		if !daemonconn.IsTermReportID(args[0]) {
			return usageError(errors.New("report ID must be 48 lowercase hexadecimal characters"))
		}
		connection, err := daemonconn.Ensure(cmd.Context())
		if err != nil {
			return err
		}
		summary, err := connection.GetTermReport(cmd.Context(), args[0])
		if err != nil {
			return err
		}
		if asJSON {
			return writeCLIJSON(cmd.OutOrStdout(), summary)
		}
		return writeReportSummary(cmd.OutOrStdout(), summary)
	}
	return cmd
}

func newReportHistoryCommand() *cobra.Command {
	cmd := &cobra.Command{
		Example: `  docbank search-export history`,
		Use:     "history",
		Short:   "List recorded report runs",
		Args:    cobra.NoArgs}
	var offset, limit int
	var asJSON bool
	cmd.Flags().IntVar(&offset, "offset", 0, "number of recorded runs to skip (0–100)")
	cmd.Flags().IntVar(&limit, "limit", 20, "maximum recorded runs on this page (1–50)")
	cmd.Flags().BoolVar(&asJSON, "json", false, "print JSON to stdout")
	cmd.RunE = func(cmd *cobra.Command, _ []string) error {
		if offset < 0 || offset > 100 || limit < 1 || limit > 50 {
			return usageError(errors.New("history offset must be 0–100 and limit must be 1–50"))
		}
		connection, err := daemonconn.Ensure(cmd.Context())
		if err != nil {
			return err
		}
		page, err := connection.API().ListTermReportHistory(cmd.Context(),
			&apiclient.ListTermReportHistoryRequestOptions{Query: &apiclient.ListTermReportHistoryQuery{
				Offset: new(int64(offset)), Limit: new(int64(limit)),
			}})
		if err != nil {
			return err
		}
		if asJSON {
			return writeCLIJSON(cmd.OutOrStdout(), page)
		}
		return writeReportHistory(cmd.OutOrStdout(), *page, offset)
	}
	return cmd
}

func writeReportSummary(output io.Writer, summary report.Summary) error {
	var text strings.Builder
	fmt.Fprintf(&text, "report %s · %s\nobserved: %s\nexpires: %s\n",
		summary.ID, summary.State, summary.ObservedAt.Format(time.RFC3339),
		summary.ExpiresAt.Format(time.RFC3339))
	if summary.ParentID != "" {
		fmt.Fprintf(&text, "parent: %s\n", summary.ParentID)
	}
	for index, term := range summary.Terms {
		fmt.Fprintf(&text, "term %d: %s · %s · %s through %s\n", term.Number,
			strconv.Quote(term.Expression), term.Syntax, term.Dates.Start, term.Dates.End)
		if summary.State == report.StateComplete {
			counts := summary.Counts[index]
			fmt.Fprintf(&text, "  Hits: %d · Hits + family: %d · Unique hits: %d · "+
				"Unique families: %d · Unique hits + family: %d\n", counts.Hits,
				counts.HitsPlusFamily, counts.UniqueHits, counts.UniqueFamilies, counts.UniqueHitsPlusFamily)
		}
	}
	if summary.State == "needs_review" {
		fmt.Fprintf(&text, "%d unresolved dates; counts and downloads are withheld.\n"+
			"Run docbank search-export dates %s, then docbank search-export revise %s "+
			"--choices choices.json --output <path>.\n",
			summary.UnresolvedDates, summary.ID, summary.ID)
	} else {
		fmt.Fprintf(&text, "bundle: %d bytes · SHA-256 %s\n", summary.BundleBytes, summary.BundleSHA256)
	}
	if _, err := io.WriteString(output, text.String()); err != nil {
		return fmt.Errorf("writing report summary: %w", err)
	}
	return writeReportCoverage(output, summary)
}

func writeReportHistory(output io.Writer, page store.TermReportHistoryPage, offset int) error {
	var text strings.Builder
	text.WriteString("History records past runs; it does not retain downloadable artifacts.\n")
	for _, item := range page.Items {
		summary := item.Summary
		fmt.Fprintf(&text, "report %s · recorded state: %s · %d terms\n",
			summary.ID, summary.State, len(summary.Terms))
		if summary.ParentID != "" {
			fmt.Fprintf(&text, "  parent: %s\n", summary.ParentID)
		}
		fmt.Fprintf(&text, "  observed: %s · expires: %s\n",
			summary.ObservedAt.Format(time.RFC3339), summary.ExpiresAt.Format(time.RFC3339))
	}
	fmt.Fprintf(&text, "%d returned · %d recorded runs total\n", len(page.Items), page.Total)
	if next := offset + len(page.Items); len(page.Items) > 0 && next < page.Total {
		fmt.Fprintf(&text, "Next page: docbank search-export history --offset %d\n", next)
	}
	if _, err := io.WriteString(output, text.String()); err != nil {
		return fmt.Errorf("writing report history: %w", err)
	}
	return nil
}
