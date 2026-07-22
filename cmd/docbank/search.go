package main

import (
	"fmt"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"go.kenn.io/docbank/internal/client"
	"go.kenn.io/docbank/internal/store"
)

const (
	defaultSearchLimit = 50
	maxSearchLimit     = 1000
)

var (
	searchLimit  int
	searchJSON   bool
	searchTag    string
	searchMIME   string
	searchUnder  string
	searchSince  string
	searchBefore string
)

var searchCmd = &cobra.Command{
	Use:   "search <query>...",
	Short: "Search document names and extracted text",
	Args:  cobra.MinimumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if searchLimit < 1 || searchLimit > maxSearchLimit {
			return usageError(fmt.Errorf("--limit must be between 1 and %d", maxSearchLimit))
		}
		mimeType, err := store.NormalizeSearchMIMEType(searchMIME)
		if err != nil {
			return usageError(err)
		}
		modifiedSince, modifiedBefore, err := store.NormalizeSearchTimeBounds(
			searchSince, searchBefore,
		)
		if err != nil {
			return usageError(err)
		}
		var underSelector nodeSelector
		if searchUnder != "" {
			underSelector, err = parseNodeSelector(searchUnder)
			if err != nil {
				return err
			}
		}
		c, err := client.Ensure(cmd.Context())
		if err != nil {
			return err
		}
		opts := client.SearchOptions{
			MIMEType: mimeType, ModifiedSince: modifiedSince, ModifiedBefore: modifiedBefore,
		}
		var tagName string
		if searchTag != "" {
			tag, resolveErr := resolveTag(cmd, c, searchTag)
			if resolveErr != nil {
				return resolveErr
			}
			opts.TagID = tag.ID
			tagName = tag.Name
		}
		var underPath string
		if searchUnder != "" {
			directory, resolveErr := underSelector.resolve(cmd.Context(), c)
			if resolveErr != nil {
				return resolveErr
			}
			if directory.Kind != "dir" {
				return fmt.Errorf("search scope %q: %w", searchUnder, store.ErrNotDir)
			}
			opts.UnderNodeID = directory.ID
			underPath = directory.Path
		}
		rep, err := c.SearchWithOptions(
			cmd.Context(), strings.Join(args, " "), searchLimit, opts,
		)
		if err != nil {
			return err
		}
		if searchJSON {
			return writeCLIJSON(cmd.OutOrStdout(), rep)
		}
		if len(rep.Hits) == 0 {
			var filters []string
			if tagName != "" {
				filters = append(filters, fmt.Sprintf("tag %q", tagName))
			}
			if rep.MIMEType != "" {
				filters = append(filters, fmt.Sprintf("media type %q", rep.MIMEType))
			}
			if underPath != "" {
				filters = append(filters, fmt.Sprintf("directory %q", underPath))
			}
			if rep.ModifiedSince != "" {
				filters = append(filters, fmt.Sprintf("modified since %q", rep.ModifiedSince))
			}
			if rep.ModifiedBefore != "" {
				filters = append(filters, fmt.Sprintf("modified before %q", rep.ModifiedBefore))
			}
			if len(filters) == 0 {
				_, _ = fmt.Fprintln(cmd.OutOrStdout(), "no matches")
			} else {
				_, _ = fmt.Fprintln(cmd.OutOrStdout(),
					"no matches with "+strings.Join(filters, " and "))
			}
			return nil
		}
		w := tabwriter.NewWriter(cmd.OutOrStdout(), 2, 4, 2, ' ', 0)
		_, _ = fmt.Fprintln(w, "SELECTOR\tMATCH\tPATH")
		for _, h := range rep.Hits {
			_, _ = fmt.Fprintf(w, "%s\t%s\t%s\n",
				formatNodeSelector(h.Node.ID), h.Match, h.Path)
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
	searchCmd.Flags().StringVar(&searchTag, "tag", "",
		"require one tag by name or stable ID")
	searchCmd.Flags().StringVar(&searchMIME, "mime-type", "",
		"require one current parameter-free media type")
	searchCmd.Flags().StringVar(&searchUnder, "under", "",
		"require descendants of one live directory path or id:N")
	searchCmd.Flags().StringVar(&searchSince, "modified-since", "",
		"require modification at or after an absolute RFC3339 timestamp")
	searchCmd.Flags().StringVar(&searchBefore, "modified-before", "",
		"require modification before an absolute RFC3339 timestamp")
	searchCmd.Flags().BoolVar(&searchJSON, "json", false, "emit machine-readable JSON")
	rootCmd.AddCommand(searchCmd)
}
