package main

import (
	"context"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/spf13/cobra"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/daemonconn"
)

// Processing search requests accept at most this many results and query
// characters; ordinary lexical search accepts more of both.
const (
	maxAutomaticSearchLimit      = 100
	maxAutomaticSearchQueryRunes = 8192
)

// automaticSearchOutcome is a successful hybrid report, or the reason a hybrid
// attempt fell back to lexical search. An empty reason means hybrid search was
// not attempted. err keeps a failure that has no stable daemon problem code.
type automaticSearchOutcome struct {
	report *api.DocumentSearchReport
	reason string
	err    error
}

// automaticSearchJSON adds folder name matches to the hybrid report, because
// the hybrid source fence holds file content versions only.
type automaticSearchJSON struct {
	api.DocumentSearchReport `json:",inline"`

	Folders []api.SearchHit `json:"folders"`
}

// tryAutomaticSearch runs hybrid search when a profile has a query embedding
// binding. Scope and semantic readiness decisions stay with the daemon.
func tryAutomaticSearch(ctx context.Context, c *daemonconn.Connection, query string, limit int,
	opts daemonconn.SearchOptions,
) (automaticSearchOutcome, error) {
	if !automaticSearchEligible(query, limit, opts) {
		return automaticSearchOutcome{}, nil
	}
	profiles, err := c.API().ListDocumentProcessingProfiles(ctx)
	if err != nil {
		if code, _ := daemonconn.ProblemCode(err); code == "processing_unavailable" {
			return automaticSearchOutcome{}, nil
		}
		return automaticSearchFallback(ctx, err)
	}
	candidates := daemonconn.AutomaticSearchCandidates(*profiles)
	if len(candidates) == 0 {
		return automaticSearchOutcome{}, nil
	}
	scope, err := c.ResolveDocumentSourceFence(ctx, api.DocumentSourceFenceResolveRequest{
		Filters: &api.DocumentSourceFenceFilters{},
	})
	if err != nil {
		return automaticSearchFallback(ctx, err)
	}
	if len(scope.Fence.ContentVersionIDs) == 0 {
		return automaticSearchOutcome{}, nil
	}
	fence := api.DocumentSourceFence{VaultUID: scope.Fence.VaultUID, ContentVersionIDs: scope.Fence.ContentVersionIDs}
	var first automaticSearchOutcome
	for _, candidate := range candidates {
		report, searchErr := c.SearchDocuments(ctx, api.DocumentSearchRequest{Query: query, Mode: "hybrid",
			Profile: candidate.Profile, BindingID: candidate.Binding, Limit: limit, Fence: fence})
		if searchErr == nil {
			return automaticSearchOutcome{report: &report}, nil
		}
		outcome, err := automaticSearchFallback(ctx, searchErr)
		if err != nil {
			return automaticSearchOutcome{}, err
		}
		if first.reason == "" {
			first = outcome
		}
		if !daemonconn.BindingSearchFailure(outcome.reason) {
			break
		}
	}
	return first, nil
}

// automaticSearchEligible reports whether a request can use hybrid search.
// Filters, fuzzy discovery, large pages, and long queries are explicit
// lexical requests, so they skip hybrid search without a degradation.
func automaticSearchEligible(query string, limit int, opts daemonconn.SearchOptions) bool {
	filtered := opts.Fuzzy || opts.TagID != "" || opts.MIMEType != "" || opts.UnderNodeID != 0 ||
		opts.ModifiedSince != "" || opts.ModifiedBefore != ""
	return !filtered && limit <= maxAutomaticSearchLimit && strings.TrimSpace(query) != "" &&
		utf8.RuneCountInString(query) <= maxAutomaticSearchQueryRunes
}

// automaticSearchFallback keeps lexical search available when the optional
// hybrid path fails. Only cancellation and authentication stop the command.
func automaticSearchFallback(ctx context.Context, err error) (automaticSearchOutcome, error) {
	if contextErr := ctx.Err(); contextErr != nil {
		return automaticSearchOutcome{}, contextErr
	}
	code, _ := daemonconn.ProblemCode(err)
	switch code {
	case "unauthorized":
		return automaticSearchOutcome{}, err
	case "":
		return automaticSearchOutcome{reason: "semantic_search_failed", err: err}, nil
	default:
		return automaticSearchOutcome{reason: code}, nil
	}
}

// writeAutomaticSearch adds folder name hits to a hybrid report within one
// --limit page. Folders rank ahead of the lowest hybrid results, as ordinary
// search ranks name matches ahead of content. The page is truncated when
// either search omitted matches.
func writeAutomaticSearch(cmd *cobra.Command, query string, report api.DocumentSearchReport,
	lexical api.SearchReport,
) error {
	folders := []api.SearchHit{}
	for _, hit := range lexical.Hits {
		if hit.Node.Kind == "dir" {
			folders = append(folders, hit)
		}
	}
	if keep := searchLimit - len(folders); len(report.Results) > keep {
		report.Results = report.Results[:keep]
		report.Truncated = true
	}
	report.Truncated = report.Truncated || lexical.Truncated
	if searchJSON {
		return writeCLIJSON(cmd.OutOrStdout(), automaticSearchJSON{DocumentSearchReport: report, Folders: folders})
	}
	if searchAgent {
		return writeDocumentSearchAgent(cmd, query, report, folders)
	}
	return writeDocumentSearchReport(cmd, report, false, folders)
}

// writeSearchDegradation reports a failed hybrid attempt on stderr so stdout
// keeps the lexical table or JSON unchanged. Agent output reports it in the
// header instead.
func writeSearchDegradation(cmd *cobra.Command, outcome automaticSearchOutcome) error {
	if outcome.reason == "" || searchAgent {
		return nil
	}
	line := "degraded: " + outcome.reason
	if outcome.err != nil {
		line += ": " + outcome.err.Error()
	}
	if _, err := fmt.Fprintln(cmd.ErrOrStderr(), line); err != nil {
		return fmt.Errorf("writing search degradation: %w", err)
	}
	return nil
}
