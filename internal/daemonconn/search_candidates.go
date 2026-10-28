package daemonconn

import (
	"cmp"
	"slices"

	"go.kenn.io/docbank/internal/api"
)

type SearchCandidate struct{ Profile, Binding string }

// AutomaticSearchCandidates orders profiles and query bindings deterministically.
func AutomaticSearchCandidates(profiles []api.ProcessingProfileSummary) []SearchCandidate {
	ordered := slices.Clone(profiles)
	slices.SortFunc(ordered, func(a, b api.ProcessingProfileSummary) int { return cmp.Compare(a.Name, b.Name) })
	var candidates []SearchCandidate
	for _, profile := range ordered {
		bindings := slices.Clone(profile.QueryEmbeddingBindings)
		slices.Sort(bindings)
		for _, binding := range bindings {
			candidates = append(candidates, SearchCandidate{profile.Name, binding})
		}
	}
	return candidates
}

// BindingSearchFailure identifies failures that another binding may avoid.
func BindingSearchFailure(reason string) bool {
	switch reason {
	case "processing_profile_unavailable", "processing_failed", "stale_index", "not_found", "processing_consent_required", "processing_consent_expired", "processing_consent_revoked":
		return true
	default:
		return false
	}
}
