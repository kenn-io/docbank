package main

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/client"
	"go.kenn.io/docbank/internal/store"
)

const (
	defaultSearchLimit = 50
	maxSearchLimit     = 1000
)

var (
	searchLimit          int
	searchJSON           bool
	searchTag            string
	searchMIME           string
	searchUnder          string
	searchSince          string
	searchBefore         string
	searchMode           string
	searchProfile        string
	searchBinding        string
	searchSourceVersions []string
	searchExplain        bool
)

type documentSearchCLIOptions struct {
	Mode              string
	Profile           string
	BindingID         string
	ContentVersionIDs []string
	Limit             int
	Explain           bool
	JSON              bool
}

var searchCmd = &cobra.Command{
	Use:   "search <query>...",
	Short: "Search document names and extracted text",
	Args:  cobra.MinimumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if documentSearchFlagsChanged(cmd) {
			if searchTag != "" || searchMIME != "" || searchUnder != "" || searchSince != "" || searchBefore != "" {
				return usageError(errors.New("--mode search cannot be combined with tag, MIME, directory, or time filters"))
			}
			options := documentSearchCLIOptions{
				Mode: searchMode, Profile: searchProfile, BindingID: searchBinding,
				ContentVersionIDs: searchSourceVersions, Limit: searchLimit,
				Explain: searchExplain, JSON: searchJSON,
			}
			if err := validateDocumentSearchOptions(strings.Join(args, " "), options); err != nil {
				return err
			}
			c, err := client.Ensure(cmd.Context())
			if err != nil {
				return err
			}
			return runDocumentSearch(cmd, c, strings.Join(args, " "), options)
		}
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

func documentSearchFlagsChanged(cmd *cobra.Command) bool {
	return cmd.Flags().Changed("mode") || cmd.Flags().Changed("profile") ||
		cmd.Flags().Changed("binding") || cmd.Flags().Changed("source-version") ||
		cmd.Flags().Changed("explain")
}

func runDocumentSearch(cmd *cobra.Command, c *client.Client, query string, options documentSearchCLIOptions) error {
	if err := validateDocumentSearchOptions(query, options); err != nil {
		return err
	}

	profiles, err := c.ProcessingProfiles(cmd.Context())
	if err != nil {
		return err
	}
	profile, found := findProcessingProfile(profiles, options.Profile)
	if !found {
		return usageError(fmt.Errorf("processing profile %q is not executable on this daemon", options.Profile))
	}
	bindingID, err := selectDocumentSearchBinding(options.Mode, options.BindingID, profile.EmbeddingBindings)
	if err != nil {
		return usageError(err)
	}
	info, err := c.Info(cmd.Context())
	if err != nil {
		return err
	}
	report, err := c.SearchDocuments(cmd.Context(), api.DocumentSearchRequest{
		Query: query, Mode: options.Mode, Limit: options.Limit, Profile: options.Profile,
		BindingID: bindingID, Explain: options.Explain,
		Fence: api.DocumentSourceFence{VaultUID: info.VaultID, ContentVersionIDs: options.ContentVersionIDs},
	})
	if err != nil {
		return err
	}
	if options.JSON {
		return writeCLIJSON(cmd.OutOrStdout(), report)
	}
	return writeDocumentSearchReport(cmd, report, options.Explain)
}

func validateDocumentSearchOptions(query string, options documentSearchCLIOptions) error {
	if !validDocumentSearchMode(options.Mode) {
		if options.Mode == "" {
			return usageError(errors.New("--mode is required with processing search options"))
		}
		return usageError(errors.New("--mode must be lexical, semantic, hybrid, or auto"))
	}
	if options.Profile == "" {
		return usageError(errors.New("--profile is required for processing search"))
	}
	if options.Limit < 1 || options.Limit > 100 {
		return usageError(errors.New("--limit must be between 1 and 100 for processing search"))
	}
	if strings.TrimSpace(query) == "" {
		return usageError(errors.New("search query must not be empty"))
	}
	if len(options.ContentVersionIDs) == 0 {
		return usageError(errors.New("at least one --source-version is required"))
	}
	if len(options.ContentVersionIDs) > 4096 {
		return usageError(errors.New("at most 4096 --source-version values are allowed"))
	}
	seen := make(map[string]struct{}, len(options.ContentVersionIDs))
	for _, versionID := range options.ContentVersionIDs {
		if !client.IsCanonicalUUIDv4(versionID) {
			return usageError(fmt.Errorf("source version %q must be a canonical UUIDv4", versionID))
		}
		if _, exists := seen[versionID]; exists {
			return usageError(fmt.Errorf("source version %q is duplicated", versionID))
		}
		seen[versionID] = struct{}{}
	}
	return nil
}

func validDocumentSearchMode(mode string) bool {
	switch mode {
	case "lexical", "semantic", "hybrid", "auto":
		return true
	default:
		return false
	}
}

func findProcessingProfile(profiles []api.ProcessingProfileSummary, name string) (api.ProcessingProfileSummary, bool) {
	for _, profile := range profiles {
		if profile.Name == name {
			return profile, true
		}
	}
	return api.ProcessingProfileSummary{}, false
}

func selectDocumentSearchBinding(mode, requested string, bindings []string) (string, error) {
	if mode == "lexical" {
		if requested != "" {
			return "", errors.New("--binding is not used by lexical search")
		}
		return "", nil
	}
	if requested != "" {
		if slices.Contains(bindings, requested) {
			return requested, nil
		}
		return "", fmt.Errorf("--binding %q is not available in this profile", requested)
	}
	if len(bindings) == 1 {
		return bindings[0], nil
	}
	if len(bindings) > 1 {
		return "", errors.New("--binding is required because this profile has multiple embedding bindings")
	}
	if mode == "semantic" || mode == "hybrid" {
		return "", errors.New("selected profile has no embedding binding for semantic search")
	}
	return "", nil
}

func writeDocumentSearchReport(cmd *cobra.Command, report api.DocumentSearchReport, explain bool) error {
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "mode: %s", report.ActualMode)
	if report.RequestedMode != report.ActualMode {
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), " (requested %s)", report.RequestedMode)
	}
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "\ncoverage: %s (%d/%d source document(s) complete)\n",
		report.Coverage.State, report.Coverage.CompleteDocuments, report.Coverage.ScopedDocuments)
	for _, degradation := range report.Degradations {
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "degraded: %s\n", degradation)
	}
	if len(report.Results) == 0 {
		_, err := fmt.Fprintln(cmd.OutOrStdout(), "no matches inside the source fence")
		if err != nil {
			return fmt.Errorf("writing empty document search result: %w", err)
		}
		return nil
	}
	w := tabwriter.NewWriter(cmd.OutOrStdout(), 2, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "RANK\tSCORE\tSELECTOR\tVERSION\tPATH\tEXCERPT")
	for _, result := range report.Results {
		_, _ = fmt.Fprintf(w, "%d\t%.6g\t%s\t%s\t%s\t%s\n", result.Rank, result.Score,
			formatNodeSelector(result.NodeID), result.ContentVersionID, result.Path, result.Excerpt)
	}
	if err := w.Flush(); err != nil {
		return fmt.Errorf("writing document search results: %w", err)
	}
	if explain {
		for _, event := range report.Trace {
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "trace: %s=%d\n", event.Code, event.Count)
		}
	}
	if report.Truncated {
		_, _ = fmt.Fprintln(cmd.OutOrStdout(), "results truncated at the requested limit")
	}
	return nil
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
	searchCmd.Flags().StringVar(&searchMode, "mode", "",
		"processing search mode: lexical, semantic, hybrid, or auto")
	searchCmd.Flags().StringVar(&searchProfile, "profile", "",
		"named executable processing profile for --mode search")
	searchCmd.Flags().StringVar(&searchBinding, "binding", "",
		"embedding binding for semantic or hybrid search")
	searchCmd.Flags().StringSliceVar(&searchSourceVersions, "source-version", nil,
		"allowed content-version UUID (repeat for a bounded source fence)")
	searchCmd.Flags().BoolVar(&searchExplain, "explain", false,
		"show bounded retrieval stages without raw similarities or vectors")
	searchCmd.Flags().BoolVar(&searchJSON, "json", false, "emit machine-readable JSON")
	rootCmd.AddCommand(searchCmd)
}
