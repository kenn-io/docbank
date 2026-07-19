package main

import (
	"fmt"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"go.kenn.io/docbank/internal/client"
)

const (
	defaultSearchLimit = 50
	maxSearchLimit     = 1000
)

var (
	searchLimit int
	searchJSON  bool
)

var searchCmd = &cobra.Command{
	Use:   "search <query>...",
	Short: "Full-text search over node names",
	Args:  cobra.MinimumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if searchLimit < 1 || searchLimit > maxSearchLimit {
			return fmt.Errorf("--limit must be between 1 and %d", maxSearchLimit)
		}
		c, err := client.Ensure(cmd.Context())
		if err != nil {
			return err
		}
		rep, err := c.Search(cmd.Context(), strings.Join(args, " "), searchLimit)
		if err != nil {
			return err
		}
		if searchJSON {
			return writeCLIJSON(cmd.OutOrStdout(), rep)
		}
		if len(rep.Hits) == 0 {
			_, _ = fmt.Fprintln(cmd.OutOrStdout(), "no matches")
			return nil
		}
		w := tabwriter.NewWriter(cmd.OutOrStdout(), 2, 4, 2, ' ', 0)
		_, _ = fmt.Fprintln(w, "ID\tPATH")
		for _, h := range rep.Hits {
			_, _ = fmt.Fprintf(w, "%d\t%s\n", h.Node.ID, h.Path)
		}
		if err := w.Flush(); err != nil {
			return fmt.Errorf("writing search results: %w", err)
		}
		if rep.Truncated {
			noun := "results"
			if rep.Limit == 1 {
				noun = "result"
			}
			_, _ = fmt.Fprintf(cmd.OutOrStdout(),
				"more than %d %s; showing the first %d (increase --limit to see more)\n",
				rep.Limit, noun, rep.Limit)
		}
		return nil
	},
}

func init() {
	searchCmd.Flags().IntVar(&searchLimit, "limit", defaultSearchLimit,
		"maximum results to return (1-1000)")
	searchCmd.Flags().BoolVar(&searchJSON, "json", false, "emit machine-readable JSON")
	rootCmd.AddCommand(searchCmd)
}
