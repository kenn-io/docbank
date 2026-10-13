package main

import (
	"cmp"
	"slices"
	"unicode/utf8"

	"github.com/spf13/cobra"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/daemonconn"
)

// tryAutomaticSearch returns nil when the caller should use ordinary lexical
// search. All scope and semantic readiness decisions stay with the daemon.
func tryAutomaticSearch(cmd *cobra.Command, c *daemonconn.Connection, query string, limit int, opts daemonconn.SearchOptions) (*api.DocumentSearchReport, string, error) {
	profiles, err := c.API().ListDocumentProcessingProfiles(cmd.Context())
	if err != nil {
		if code, _ := daemonconn.ProblemCode(err); code == "processing_unavailable" {
			return nil, "", nil
		}
		return nil, "", err
	}
	configured := false
	for _, profile := range *profiles {
		configured = configured || len(profile.EmbeddingBindings) > 0
	}
	if !configured {
		return nil, "", nil
	}
	if opts.TagID != "" || opts.MIMEType != "" || opts.UnderNodeID != 0 || opts.ModifiedSince != "" || opts.ModifiedBefore != "" {
		return nil, "filters_require_lexical", nil
	}
	if limit > 100 {
		return nil, "limit_requires_lexical", nil
	}
	// Processing accepts at most 8192 Unicode characters; ordinary lexical
	// search does not share that request limit.
	if utf8.RuneCountInString(query) > 8192 {
		return nil, "query_requires_lexical", nil
	}
	type candidate struct{ profile, binding string }
	var candidates []candidate
	ordered := slices.Clone(*profiles)
	slices.SortFunc(ordered, func(a, b api.ProcessingProfileSummary) int { return cmp.Compare(a.Name, b.Name) })
	for _, profile := range ordered {
		bindings := slices.Clone(profile.QueryEmbeddingBindings)
		slices.Sort(bindings)
		for _, binding := range bindings {
			candidates = append(candidates, candidate{profile.Name, binding})
		}
	}
	if len(candidates) == 0 {
		return nil, "query_embedding_unavailable", nil
	}
	scope, err := c.ResolveDocumentSourceFence(cmd.Context(), api.DocumentSourceFenceResolveRequest{Filters: &api.DocumentSourceFenceFilters{}})
	if err != nil {
		return automaticSearchFallback(cmd, err)
	}
	if len(scope.Fence.ContentVersionIDs) == 0 {
		return nil, "empty_source_scope", nil
	}
	var reason string
	for _, candidate := range candidates {
		report, err := c.SearchDocuments(cmd.Context(), api.DocumentSearchRequest{Query: query, Mode: "hybrid", Profile: candidate.profile, BindingID: candidate.binding, Limit: limit, Fence: api.DocumentSourceFence{VaultUID: scope.Fence.VaultUID, ContentVersionIDs: scope.Fence.ContentVersionIDs}})
		if err == nil {
			return &report, "", nil
		}
		_, candidateReason, err := automaticSearchFallback(cmd, err)
		if err != nil {
			return nil, "", err
		}
		if reason == "" {
			reason = candidateReason
		}
	}
	return nil, reason, nil
}

func automaticSearchFallback(cmd *cobra.Command, err error) (*api.DocumentSearchReport, string, error) {
	if contextErr := cmd.Context().Err(); contextErr != nil {
		return nil, "", contextErr
	}
	code, _ := daemonconn.ProblemCode(err)
	switch code {
	case "processing_unavailable", "processing_profile_unavailable", "processing_failed",
		"processing_consent_required", "processing_consent_expired", "processing_consent_revoked",
		"stale_version", "stale_index", "scope_too_large", "not_found":
		return nil, code, nil
	default:
		return nil, "", err
	}
}
