package main

import (
	"bufio"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"
	"go.kenn.io/docbank/internal/answercard"
	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/docbank/internal/store"
)

var cardsRoot string
var cardsDeep, cardsJSON, cardsRun, cardsYes bool
var cardsCmd = &cobra.Command{Use: "cards", Short: "Manage cards", Long: "Inspect answer cards and report whether their cited sources remain current.", GroupID: groupDocuments}
var cardsCheckCmd = &cobra.Command{
	Use: "check", Short: "Report answer card states", Args: cobra.NoArgs,
	Example: "  docbank cards check --root /answer-cards --json",
	RunE: func(cmd *cobra.Command, _ []string) error {
		c, err := daemonconn.Ensure(cmd.Context())
		if err != nil {
			return err
		}
		var report answercard.Report
		if cardsRun {
			report, err = answercard.CheckRun(cmd.Context(), c, cardsRoot, cardsDeep)
		} else {
			report, err = answercard.Check(cmd.Context(), c, cardsRoot, cardsDeep)
		}
		if err != nil {
			return err
		}
		if cardsJSON {
			return writeCLIJSON(cmd.OutOrStdout(), report)
		}
		w := tabwriter.NewWriter(cmd.OutOrStdout(), 2, 4, 2, ' ', 0)
		_, _ = fmt.Fprintln(w, "STATE\tREASON\tPATH")
		for _, row := range report.Items {
			rowReason := strings.Join(strings.Fields(row.Reason), " ")
			displayReason := rowReason
			if row.Citation != nil && rowReason != "" {
				displayReason = fmt.Sprintf("citation %d: %s", *row.Citation, rowReason)
			}
			_, _ = fmt.Fprintf(w, "%s\t%s\t%s\n", row.State, displayReason, row.Path)
			for _, issue := range row.CitationIssues {
				issueReason := strings.Join(strings.Fields(issue.Reason), " ")
				if row.Citation != nil && issue.Citation == *row.Citation && issueReason == rowReason {
					continue
				}
				_, _ = fmt.Fprintf(w, "  citation %d\t%s\n", issue.Citation, issueReason)
			}
		}
		return w.Flush()
	},
}

var cardsApproveCmd = &cobra.Command{
	Example: "  docbank cards approve /answer-cards/policy.md",
	Use:     "approve <path-or-id>", Short: "Approve an answer card after checking its sources", Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		selector, err := parseNodeSelector(args[0])
		if err != nil {
			return err
		}
		c, err := daemonconn.Ensure(cmd.Context())
		if err != nil {
			return err
		}
		node, err := selector.resolve(cmd.Context(), c)
		if err != nil {
			return err
		}
		normalized, err := store.NormalizeDocumentCatalogQuery(store.DocumentCatalogQuery{PathPrefix: cardsRoot})
		if err != nil {
			return fmt.Errorf("invalid_card: invalid root: %w", err)
		}
		root := strings.TrimSuffix(normalized.PathPrefix, "/")
		if root == "" {
			root = "/"
		}
		if !strings.HasPrefix(node.Path, strings.TrimSuffix(root, "/")+"/") {
			return fmt.Errorf("invalid_card: card is outside root %s", root)
		}
		err = answercard.Approve(cmd.Context(), c, node, func(card answercard.Card, paths []string) (bool, error) {
			w := cmd.OutOrStdout()
			if _, err := fmt.Fprintf(w, "Question: %s\n", card.Question); err != nil {
				return false, fmt.Errorf("writing approval preview: %w", err)
			}
			for _, a := range card.Alternates {
				if _, err := fmt.Fprintf(w, "Alternate: %s\n", a); err != nil {
					return false, fmt.Errorf("writing approval preview: %w", err)
				}
			}
			if _, err := fmt.Fprintf(w, "Answer: %s\n", card.Answer); err != nil {
				return false, fmt.Errorf("writing approval preview: %w", err)
			}
			for i, cite := range card.Citations {
				if _, err := fmt.Fprintf(w, "[%d] %s: %s\n", i+1, paths[i], cite.Quote); err != nil {
					return false, fmt.Errorf("writing approval preview: %w", err)
				}
			}
			if cardsYes {
				return true, nil
			}
			if _, err := fmt.Fprint(w, "Approve this card? [y/N] "); err != nil {
				return false, fmt.Errorf("writing approval preview: %w", err)
			}
			line, err := bufio.NewReader(cmd.InOrStdin()).ReadString('\n')
			if err != nil && (err != io.EOF || len(line) == 0) {
				return false, fmt.Errorf("reading approval confirmation: %w", err)
			}
			line = strings.TrimSpace(line)
			return strings.EqualFold(line, "y") || strings.EqualFold(line, "yes"), nil
		})
		if err != nil {
			return err
		}
		_, err = fmt.Fprintln(cmd.OutOrStdout(), "Card approved.")
		if err != nil {
			return fmt.Errorf("writing approval result: %w", err)
		}
		return nil
	},
}

func init() {
	cardsCheckCmd.Flags().StringVar(&cardsRoot, "root", "/answer-cards", "card folder")
	cardsCheckCmd.Flags().BoolVar(&cardsRun, "run", false, "record newly stale cards")
	cardsApproveCmd.Flags().StringVar(&cardsRoot, "root", "/answer-cards", "card folder")
	cardsApproveCmd.Flags().BoolVar(&cardsYes, "yes", false, "approve without prompting")
	cardsCmd.AddCommand(cardsApproveCmd)
	cardsCheckCmd.Flags().BoolVar(&cardsDeep, "deep", false, "resolve cited quotations")
	cardsCheckCmd.Flags().BoolVar(&cardsJSON, "json", false, "print JSON to stdout")
	cardsCmd.AddCommand(cardsCheckCmd)
	rootCmd.AddCommand(cardsCmd)
}
