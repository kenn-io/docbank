package main

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"
	"text/tabwriter"
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

type automaticSearchCandidate struct{ profile, binding string }

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
	candidates := automaticSearchCandidates(*profiles)
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
			Profile: candidate.profile, BindingID: candidate.binding, Limit: limit, Fence: fence})
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
		if !bindingSearchFailure(outcome.reason) {
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

func automaticSearchCandidates(profiles []api.ProcessingProfileSummary) []automaticSearchCandidate {
	ordered := slices.Clone(profiles)
	slices.SortFunc(ordered, func(a, b api.ProcessingProfileSummary) int { return cmp.Compare(a.Name, b.Name) })
	var candidates []automaticSearchCandidate
	for _, profile := range ordered {
		bindings := slices.Clone(profile.QueryEmbeddingBindings)
		slices.Sort(bindings)
		for _, binding := range bindings {
			candidates = append(candidates, automaticSearchCandidate{profile.Name, binding})
		}
	}
	return candidates
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

// bindingSearchFailure reports whether another profile or binding could still
// succeed. Scope failures such as stale_version repeat for every binding.
func bindingSearchFailure(reason string) bool {
	switch reason {
	case "processing_profile_unavailable", "processing_failed", "stale_index",
		"processing_consent_required", "processing_consent_expired", "processing_consent_revoked":
		return true
	default:
		return false
	}
}

func writeAutomaticSearch(cmd *cobra.Command, query string, report api.DocumentSearchReport,
	lexical []api.SearchHit,
) error {
	folders := []api.SearchHit{}
	for _, hit := range lexical {
		if hit.Node.Kind == "dir" {
			folders = append(folders, hit)
		}
	}
	if searchJSON {
		return writeCLIJSON(cmd.OutOrStdout(), automaticSearchJSON{DocumentSearchReport: report, Folders: folders})
	}
	if searchAgent {
		return writeDocumentSearchAgent(cmd, query, report, folders)
	}
	if err := writeDocumentSearchReport(cmd, report, false); err != nil {
		return err
	}
	if len(folders) == 0 {
		return nil
	}
	w := tabwriter.NewWriter(cmd.OutOrStdout(), 2, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "\nSELECTOR\tMATCH\tPATH")
	for _, hit := range folders {
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\n", formatNodeSelector(hit.Node.ID), hit.Match, hit.Path)
	}
	if err := w.Flush(); err != nil {
		return fmt.Errorf("writing folder search results: %w", err)
	}
	return nil
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
