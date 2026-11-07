package main

import (
	"errors"
	"fmt"
	"strings"

	"github.com/spf13/cobra"
	"go.kenn.io/docbank/internal/answercard"
	"go.kenn.io/docbank/internal/daemonconn"
)

var cardsMatchLimit int
var cardsMatchCmd = &cobra.Command{
	Use: "match <question...>", Short: "Find freshly verified approved answers", Args: cobra.MinimumNArgs(1),
	Example: "  docbank cards match when does the policy renew --json",
	RunE: func(cmd *cobra.Command, args []string) error {
		if cardsMatchLimit < 1 || cardsMatchLimit > 10 {
			return errors.New("limit must be between 1 and 10")
		}
		c, err := daemonconn.Ensure(cmd.Context())
		if err != nil {
			return err
		}
		result, err := answercard.NewMatcher().Match(cmd.Context(), c, strings.Join(args, " "), cardsRoot, cardsMatchLimit)
		if err != nil {
			return err
		}
		if cardsJSON {
			return writeCLIJSON(cmd.OutOrStdout(), result)
		}
		w := cmd.OutOrStdout()
		if result.Status == "no_match" {
			_, err = fmt.Fprintln(w, "No matching approved answer.")
			if err != nil {
				return fmt.Errorf("writing no-match result: %w", err)
			}
		}
		for _, match := range result.Matches {
			if _, err = fmt.Fprintf(w, "%s (%.2f)\n%s\n%s\n", match.Card.Path, match.Score, match.Question, match.Answer); err != nil {
				return fmt.Errorf("writing answer card match: %w", err)
			}
			for i, cite := range match.Citations {
				if _, err = fmt.Fprintf(w, "[%d] node %d: %s\n", i+1, cite.NodeID, cite.Quote); err != nil {
					return fmt.Errorf("writing answer card citation: %w", err)
				}
			}
		}
		for _, card := range result.StaleCards {
			if _, err = fmt.Fprintf(w, "Stale: %s (%s)\n", card.Path, card.Reason); err != nil {
				return fmt.Errorf("writing stale card notice: %w", err)
			}
		}
		return nil
	},
}

func init() {
	cardsMatchCmd.Flags().StringVar(&cardsRoot, "root", "/answer-cards", "card folder")
	cardsMatchCmd.Flags().IntVar(&cardsMatchLimit, "limit", 3, "maximum matches (1–10)")
	cardsMatchCmd.Flags().BoolVar(&cardsJSON, "json", false, "print JSON to stdout")
	cardsCmd.AddCommand(cardsMatchCmd)
}
