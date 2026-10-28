package main

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/docbank/internal/store"
)

const (
	defaultSearchLimit = 50
	maxSearchLimit     = 1000
)

var (
	searchLimit          int
	searchJSON           bool
	searchAgent          bool
	searchFuzzy          bool
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
	searchSimilarTo      string
	searchVersion        string
	searchRerank         bool
)

type documentSearchCLIOptions struct {
	Mode              string
	Profile           string
	BindingID         string
	ContentVersionIDs []string
	Limit             int
	Explain           bool
	Rerank            bool
	JSON              bool
	Agent             bool
}

var searchCmd = &cobra.Command{
	Example: `  docbank search invoice 2026 --agent
  docbank search merger plan --under /cases/acme --json
  docbank search --tag urgent
  docbank search invocie --fuzzy --modified-since 2026-01-01T00:00:00Z`,
	Long: `Query words are joined. Lexical search matches word prefixes, names before
content, on current versions of live nodes.
With a query embedding binding, plain text search runs hybrid retrieval over
live files and appends matching folder names. Filters, --fuzzy, --limit above
100, and --mode lexical use lexical search. A failed hybrid attempt prints
"degraded: REASON" to stderr and returns lexical results.
--json prints lexical {hits, limit, truncated}, or for hybrid the processing
report (actual_mode, coverage, results) plus folders.
--agent prints an OK header and one line per hit.
--fuzzy appends filename/path matches with one typo.
Explicit processing modes need --profile and --source-version UUIDs.
See docs/usage/search.md.`,
	GroupID: groupSearch,
	Use:     "search [<query>...]",
	Short:   "Find files and folders by name, text, tag, type, folder, or date",
	RunE: func(cmd *cobra.Command, args []string) (retErr error) {
		if searchAgent && searchJSON {
			return usageError(errors.New("--agent and --json are mutually exclusive"))
		}
		if searchFuzzy {
			for _, flag := range []string{
				"mode", "profile", "binding", "source-version", "explain", "rerank", "similar-to",
			} {
				if cmd.Flags().Changed(flag) {
					return usageError(fmt.Errorf("--fuzzy cannot be combined with --%s", flag))
				}
			}
			if strings.TrimSpace(strings.Join(args, " ")) == "" {
				return usageError(errors.New("--fuzzy requires query text"))
			}
		}
		if cmd.Flags().Changed("similar-to") {
			if len(args) != 0 {
				return usageError(errors.New("--similar-to does not accept query text"))
			}
			for _, flag := range []string{"mode", "explain", "rerank", "tag", "mime-type", "under", "modified-since", "modified-before"} {
				if cmd.Flags().Changed(flag) {
					return usageError(fmt.Errorf("--similar-to cannot be combined with --%s", flag))
				}
			}
			options := documentSearchCLIOptions{Mode: "semantic", Profile: searchProfile, BindingID: searchBinding, ContentVersionIDs: searchSourceVersions, Limit: searchLimit, JSON: searchJSON, Agent: searchAgent}
			if !cmd.Flags().Changed("limit") {
				options.Limit = 20
			}
			if err := validateDocumentSearchOptions("stored source", options); err != nil {
				return err
			}
			selector, err := parseNodeSelector(searchSimilarTo)
			if err != nil {
				return err
			}
			if searchVersion != "" && !daemonconn.IsCanonicalUUIDv4(searchVersion) {
				return usageError(errors.New("--version must be a canonical UUIDv4"))
			}
			c, err := daemonconn.Ensure(cmd.Context())
			if err != nil {
				return err
			}
			return runSimilarSearch(cmd, c, selector, searchVersion, options)
		}
		if cmd.Flags().Changed("version") {
			return usageError(errors.New("--version requires --similar-to"))
		}
		if documentSearchFlagsChanged(cmd) {
			if searchTag != "" || searchMIME != "" || searchUnder != "" || searchSince != "" || searchBefore != "" {
				return usageError(errors.New("--mode search cannot be combined with tag, MIME, directory, or time filters"))
			}
			options := documentSearchCLIOptions{
				Mode: searchMode, Profile: searchProfile, BindingID: searchBinding,
				ContentVersionIDs: searchSourceVersions, Limit: searchLimit,
				Explain: searchExplain, Rerank: searchRerank, JSON: searchJSON, Agent: searchAgent,
			}
			if err := validateDocumentSearchOptions(strings.Join(args, " "), options); err != nil {
				return err
			}
			c, err := daemonconn.Ensure(cmd.Context())
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
		if searchTag == "" && store.SearchNeedsQuery(strings.Join(args, " "), store.SearchOptions{
			MIMEType: mimeType, ModifiedSince: modifiedSince, ModifiedBefore: modifiedBefore,
		}) {
			return usageError(store.ErrSearchQueryRequired)
		}
		var underSelector nodeSelector
		if searchUnder != "" {
			underSelector, err = parseNodeSelector(searchUnder)
			if err != nil {
				return err
			}
		}
		c, err := daemonconn.Ensure(cmd.Context())
		if err != nil {
			return err
		}
		defer func() {
			if !isClosedOutputPipe(retErr) {
				reportFailure(cmd, c, "search", retErr, searchAgent)
			}
		}()
		opts := daemonconn.SearchOptions{
			Fuzzy:    searchFuzzy,
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
		query := strings.Join(args, " ")
		var automatic automaticSearchOutcome
		if !cmd.Flags().Changed("mode") && automaticSearchEligible(query, searchLimit, opts) {
			// Name matches decide whether the document request can record a miss.
			nameOpts := opts
			nameOpts.SkipMissLog = true
			names, nameErr := c.SearchWithOptions(cmd.Context(), query, searchLimit, nameOpts)
			if nameErr != nil {
				return nameErr
			}
			opts.SkipMissLog = slices.ContainsFunc(names.Hits, func(hit api.SearchHit) bool { return hit.Match == "name" })
			automatic, err = tryAutomaticSearch(cmd.Context(), c, query, searchLimit, opts)
			if err != nil {
				return err
			}
		}
		// Lexical search runs after query embedding, so folder hits added to a
		// hybrid report are as current as its revalidated results.
		opts.SkipMissLog = automatic.report != nil
		rep, err := c.SearchWithOptions(cmd.Context(), query, searchLimit, opts)
		if err != nil {
			return err
		}
		searchType := "text"
		if automatic.report != nil {
			searchType = automatic.report.ActualMode
		}
		empty := len(rep.Hits) == 0
		if automatic.report != nil {
			empty = len(automatic.report.Results) == 0 && !slices.ContainsFunc(rep.Hits, func(hit api.SearchHit) bool { return hit.Node.Kind == "dir" })
		}
		defer func() {
			if retErr == nil {
				reportSearch(cmd, c, searchType, empty, searchAgent)
			}
		}()
		if automatic.report != nil {
			return writeAutomaticSearch(cmd, query, *automatic.report, rep)
		}
		if err := writeSearchDegradation(cmd, automatic); err != nil {
			return err
		}
		if searchJSON {
			return writeCLIJSON(cmd.OutOrStdout(), rep)
		}
		if searchAgent {
			return writeLexicalSearchAgent(cmd, query, rep, automatic)
		}
		return writeSearchReport(cmd, rep, tagName, underPath)
	},
}

func writeSearchReport(cmd *cobra.Command, rep api.SearchReport, tagName, underPath string) error {
	if rep.Fuzzy != nil && rep.Fuzzy.Status != store.FuzzyStatusComplete {
		_, err := fmt.Fprintf(cmd.OutOrStdout(), "incomplete fuzzy search (%s: %s); "+
			"use a more specific ASCII query or --under; "+
			"increase --limit if ordinary hits filled the page\n",
			rep.Fuzzy.Status, strings.Join(rep.Fuzzy.Reasons, ", "))
		if err != nil {
			return fmt.Errorf("writing fuzzy coverage: %w", err)
		}
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
}

func documentSearchFlagsChanged(cmd *cobra.Command) bool {
	return (cmd.Flags().Changed("mode") && searchMode != "lexical") || cmd.Flags().Changed("profile") ||
		cmd.Flags().Changed("binding") || cmd.Flags().Changed("source-version") ||
		cmd.Flags().Changed("explain") || searchRerank
}

func runSimilarSearch(cmd *cobra.Command, c *daemonconn.Connection, selector nodeSelector, version string, options documentSearchCLIOptions) (retErr error) {
	defer func() {
		if !isClosedOutputPipe(retErr) {
			reportFailure(cmd, c, "search", retErr, searchAgent)
		}
	}()
	node, err := selector.resolve(cmd.Context(), c)
	if err != nil {
		return err
	}
	if version == "" {
		version = node.CurrentVersionID
	}
	if node.Kind != "file" || version != node.CurrentVersionID {
		return usageError(errors.New("similar source must be a current live file version"))
	}
	if !slices.Contains(options.ContentVersionIDs, version) {
		return usageError(errors.New("the selected version must occur in --source-version"))
	}
	profiles, err := c.API().ListDocumentProcessingProfiles(cmd.Context())
	if err != nil {
		return err
	}
	profile, found := findProcessingProfile(*profiles, options.Profile)
	if !found {
		return usageError(errors.New("processing profile is unavailable"))
	}
	binding, err := selectDocumentSearchBinding("semantic", options.BindingID, profile.EmbeddingBindings)
	if err != nil {
		return usageError(err)
	}
	info, err := c.API().VaultInfo(cmd.Context())
	if err != nil {
		return err
	}
	report, err := c.SimilarDocuments(cmd.Context(), api.DocumentSimilarRequest{
		Selector: api.ProcessingSelector{NodeID: node.ID, ContentVersionID: version, Profile: options.Profile}, BindingID: binding, Limit: options.Limit,
		Fence: api.DocumentSourceFence{VaultUID: info.VaultID, ContentVersionIDs: options.ContentVersionIDs}})
	if err != nil {
		return err
	}
	defer func() {
		if retErr == nil && report.State != "unavailable" {
			reportSearch(cmd, c, "semantic", len(report.Results) == 0, searchAgent)
		}
	}()
	if options.JSON {
		return writeCLIJSON(cmd.OutOrStdout(), report)
	}
	if options.Agent {
		return writeSimilarSearchAgent(cmd, report)
	}
	return writeSimilarSearchReport(cmd, report)
}

func writeSimilarSearchReport(cmd *cobra.Command, report api.DocumentSimilarReport) error {
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "similar to: id:%d (version %s)\nbinding: %s\n", report.Source.NodeID, report.Source.ContentVersionID, report.BindingID)
	if report.State == "unavailable" {
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "unavailable: no current embedding for binding %q on version %s\n", report.BindingID, report.Source.ContentVersionID)
		return nil
	}
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "coverage: %s (%d/%d source documents complete)\n", report.Coverage.State, report.Coverage.CompleteDocuments, report.Coverage.ScopedDocuments)
	if len(report.Results) == 0 {
		_, _ = fmt.Fprintln(cmd.OutOrStdout(), "no similar documents inside the source fence")
		return nil
	}
	w := tabwriter.NewWriter(cmd.OutOrStdout(), 2, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "RANK\tSCORE\tSELECTOR\tVERSION\tDUPLICATES\tPATH")
	for _, result := range report.Results {
		_, _ = fmt.Fprintf(w, "%d\t%.6g\tid:%d\t%s\t%d\t%s\n", result.Rank, result.Score, result.NodeID, result.ContentVersionID, result.DuplicateCount, result.Path)
	}
	if err := w.Flush(); err != nil {
		return fmt.Errorf("writing similar results: %w", err)
	}
	if report.Truncated {
		_, _ = fmt.Fprintln(cmd.OutOrStdout(), "results truncated at the requested limit")
	}
	return nil
}

func runDocumentSearch(cmd *cobra.Command, c *daemonconn.Connection, query string, options documentSearchCLIOptions) (retErr error) {
	defer func() {
		if !isClosedOutputPipe(retErr) {
			reportFailure(cmd, c, "search", retErr, searchAgent)
		}
	}()
	if err := validateDocumentSearchOptions(query, options); err != nil {
		return err
	}

	profiles, err := c.API().ListDocumentProcessingProfiles(cmd.Context())

	if err != nil {
		return err
	}
	profile, found := findProcessingProfile(*profiles, options.Profile)
	if !found {
		return usageError(fmt.Errorf("processing profile %q is not executable on this daemon", options.Profile))
	}
	bindingID, err := selectDocumentSearchBinding(options.Mode, options.BindingID, profile.EmbeddingBindings)
	if err != nil {
		return usageError(err)
	}
	info, err := c.API().VaultInfo(cmd.Context())

	if err != nil {
		return err
	}
	report, err := c.SearchDocuments(cmd.Context(), api.DocumentSearchRequest{
		Query: query, Mode: options.Mode, Limit: options.Limit, Profile: options.Profile,
		BindingID: bindingID, Explain: options.Explain, Rerank: options.Rerank, SkipMissLog: true,
		Fence: api.DocumentSourceFence{VaultUID: info.VaultID, ContentVersionIDs: options.ContentVersionIDs},
	})
	if err != nil {
		return err
	}
	defer func() {
		if retErr == nil {
			reportSearch(cmd, c, report.ActualMode, len(report.Results) == 0, searchAgent)
		}
	}()
	if options.JSON {
		return writeCLIJSON(cmd.OutOrStdout(), report)
	}
	if options.Agent {
		return writeDocumentSearchAgent(cmd, query, report, nil)
	}
	return writeDocumentSearchReport(cmd, report, options.Explain, nil)
}

func validateDocumentSearchOptions(query string, options documentSearchCLIOptions) error {
	if !validDocumentSearchMode(options.Mode) {
		if options.Mode == "" {
			return usageError(errors.New("--mode is required with processing search options"))
		}
		return usageError(errors.New("--mode must be lexical, semantic, hybrid, or auto"))
	}
	if options.Profile == "" {
		return usageError(errors.New("--profile is required for processing search; list profiles with \"docbank processing profiles\""))
	}
	if options.Limit < 1 || options.Limit > 100 {
		return usageError(errors.New("--limit must be between 1 and 100 for processing search"))
	}
	if strings.TrimSpace(query) == "" {
		return usageError(errors.New("search query must not be empty"))
	}
	if len(options.ContentVersionIDs) == 0 {
		return usageError(errors.New("at least one --source-version is required; use current_version_id from \"docbank stat <path-or-id> --json\""))
	}
	if len(options.ContentVersionIDs) > 4096 {
		return usageError(errors.New("at most 4096 --source-version values are allowed"))
	}
	seen := make(map[string]struct{}, len(options.ContentVersionIDs))
	for _, versionID := range options.ContentVersionIDs {
		if !daemonconn.IsCanonicalUUIDv4(versionID) {
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
	if mode == "lexical" || mode == "auto" {
		if requested != "" {
			return "", errors.New("--binding is only used by semantic or hybrid search")
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

// writeDocumentSearchReport prints a processing report. Automatic search
// passes the folder name hits it adds; explicit processing search passes nil.
func writeDocumentSearchReport(cmd *cobra.Command, report api.DocumentSearchReport, explain bool,
	folders []api.SearchHit,
) error {
	writeDocumentSearchHeader(cmd, report)
	out := cmd.OutOrStdout()
	if len(report.Results) == 0 && len(folders) == 0 {
		if _, err := fmt.Fprintln(out, "no matches inside the source fence"); err != nil {
			return fmt.Errorf("writing empty document search result: %w", err)
		}
	}
	if len(report.Results) > 0 {
		w := tabwriter.NewWriter(out, 2, 4, 2, ' ', 0)
		_, _ = fmt.Fprintln(w, "RANK\tSCORE\tSELECTOR\tVERSION\tPATH\tEXCERPT")
		for _, result := range report.Results {
			_, _ = fmt.Fprintf(w, "%d\t%.6g\t%s\t%s\t%s\t%s\n", result.Rank, result.Score,
				formatNodeSelector(result.NodeID), result.ContentVersionID, result.Path, result.Excerpt)
		}
		if err := w.Flush(); err != nil {
			return fmt.Errorf("writing document search results: %w", err)
		}
	}
	if err := writeFolderSearchHits(cmd, folders, len(report.Results) > 0); err != nil {
		return err
	}
	if explain {
		for _, event := range report.Trace {
			_, _ = fmt.Fprintf(out, "trace: %s=%d\n", event.Code, event.Count)
		}
	}
	if report.Truncated {
		_, _ = fmt.Fprintln(out, "results truncated at the requested limit")
	}
	return nil
}

func writeDocumentSearchHeader(cmd *cobra.Command, report api.DocumentSearchReport) {
	out := cmd.OutOrStdout()
	_, _ = fmt.Fprintf(out, "mode: %s", report.ActualMode)
	if report.RequestedMode != report.ActualMode {
		_, _ = fmt.Fprintf(out, " (requested %s)", report.RequestedMode)
	}
	_, _ = fmt.Fprintf(out, "\ncoverage: %s (%d/%d source document(s) complete)\n",
		report.Coverage.State, report.Coverage.CompleteDocuments, report.Coverage.ScopedDocuments)
	for _, degradation := range report.Degradations {
		_, _ = fmt.Fprintf(out, "degraded: %s\n", degradation)
	}
	if r := report.Reranking; r != nil {
		if r.Cause == "" {
			_, _ = fmt.Fprintf(out, "reranking: %s (%d candidates)\n", r.Outcome, r.CandidateCount)
		} else {
			_, _ = fmt.Fprintf(out, "reranking: %s (%s, %d candidates)\n", r.Outcome, r.Cause, r.CandidateCount)
		}
	}
}

func writeFolderSearchHits(cmd *cobra.Command, folders []api.SearchHit, separate bool) error {
	if len(folders) == 0 {
		return nil
	}
	if separate {
		_, _ = fmt.Fprintln(cmd.OutOrStdout())
	}
	w := tabwriter.NewWriter(cmd.OutOrStdout(), 2, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "SELECTOR\tMATCH\tPATH")
	for _, hit := range folders {
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\n", formatNodeSelector(hit.Node.ID), hit.Match, hit.Path)
	}
	if err := w.Flush(); err != nil {
		return fmt.Errorf("writing folder search results: %w", err)
	}
	return nil
}

func init() {
	searchCmd.Flags().StringVar(&searchSimilarTo, "similar-to", "", "find similar files using the stored embedding of id:N or /path")
	searchCmd.Flags().StringVar(&searchVersion, "version", "", "exact current source version for --similar-to")
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
		"mode: lexical (ordinary alone), semantic, hybrid, or auto")
	searchCmd.Flags().StringVar(&searchProfile, "profile", "",
		"named executable processing profile for --mode search")
	searchCmd.Flags().StringVar(&searchBinding, "binding", "",
		"embedding binding for semantic or hybrid search")
	searchCmd.Flags().StringSliceVar(&searchSourceVersions, "source-version", nil,
		"allowed content-version UUID (repeat for a bounded source fence)")
	searchCmd.Flags().BoolVar(&searchExplain, "explain", false,
		"show bounded retrieval stages without raw similarities or vectors")
	searchCmd.Flags().BoolVar(&searchRerank, "rerank", false,
		"rerank the source-fenced processing search results")
	searchCmd.Flags().BoolVar(&searchAgent, "agent", false, "print concise agent-readable text")
	searchCmd.Flags().BoolVar(&searchFuzzy, "fuzzy", false, "append bounded ASCII filename/path matches with at most one typo")
	searchCmd.Flags().BoolVar(&searchJSON, "json", false, "print JSON to stdout")
	rootCmd.AddCommand(searchCmd)
}
