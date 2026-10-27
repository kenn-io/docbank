package main

import (
	"fmt"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"
	"go.kenn.io/docbank/internal/answercard"
	"go.kenn.io/docbank/internal/daemonconn"
)

var cardsRoot string
var cardsDeep, cardsJSON bool
var cardsCmd = &cobra.Command{Use: "cards", Short: "Manage cards", Long: "Inspect answer cards and report whether their cited sources remain current.", GroupID: groupDocuments}
var cardsCheckCmd = &cobra.Command{
	Use: "check", Short: "Report answer card states", Args: cobra.NoArgs,
	Example: "  docbank cards check --root /answer-cards --json",
	RunE: func(cmd *cobra.Command, _ []string) error {
		c, err := daemonconn.Ensure(cmd.Context())
		if err != nil {
			return err
		}
		report, err := answercard.Check(cmd.Context(), c, cardsRoot, cardsDeep)
		if err != nil {
			return err
		}
		if cardsJSON {
			return writeCLIJSON(cmd.OutOrStdout(), report)
		}
		w := tabwriter.NewWriter(cmd.OutOrStdout(), 2, 4, 2, ' ', 0)
		_, _ = fmt.Fprintln(w, "STATE\tREASON\tPATH")
		for _, row := range report.Items {
			_, _ = fmt.Fprintf(w, "%s\t%s\t%s\n", row.State, strings.Join(strings.Fields(row.Reason), " "), row.Path)
			for _, issue := range row.CitationIssues {
				_, _ = fmt.Fprintf(w, "  citation %d\t%s\t%s\n", issue.Citation, strings.Join(strings.Fields(issue.Reason), " "), row.Path)
			}
		}
		return w.Flush()
	},
}

func init() {
	cardsCheckCmd.Flags().StringVar(&cardsRoot, "root", "/answer-cards", "card folder")
	cardsCheckCmd.Flags().BoolVar(&cardsDeep, "deep", false, "resolve cited quotations")
	cardsCheckCmd.Flags().BoolVar(&cardsJSON, "json", false, "print JSON to stdout")
	cardsCmd.AddCommand(cardsCheckCmd)
	rootCmd.AddCommand(cardsCmd)
}
