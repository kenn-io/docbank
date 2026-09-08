package retrieval

import (
	"errors"
	"fmt"
	"math"
	"slices"
)

func mergeVariantReports(reports []Report, limit int) (Report, error) {
	if len(reports) == 0 {
		return Report{}, errors.New("retrieval requires one query report")
	}
	if limit < 1 || limit > MaxCandidateLimit {
		return Report{}, fmt.Errorf("retrieval candidate limit must be between 1 and %d", MaxCandidateLimit)
	}
	if len(reports) == 1 {
		return reports[0], nil
	}
	merged := reports[0]
	merged.Coverage = conservativeCoverage(reports)
	byDocument := make(map[DocumentIdentity]*Result)
	for _, report := range reports {
		if report.RequestedMode != merged.RequestedMode || report.ActualMode != merged.ActualMode {
			return Report{}, errors.New("query variants returned inconsistent retrieval modes")
		}
		if report.Degradation != merged.Degradation || !slices.Equal(report.Degradations, merged.Degradations) {
			return Report{}, errors.New("query variants returned incompatible degradations")
		}
		merged.Truncated = merged.Truncated || report.Truncated
		for _, result := range report.Results {
			score, err := resultContributionScore(result)
			if err != nil {
				return Report{}, err
			}
			current := byDocument[result.Document]
			if current == nil {
				mergedResult := result
				mergedResult.Score = score
				mergedResult.Explanation = slices.Clone(result.Explanation)
				mergedResult.Evidence = slices.Clone(result.Evidence)
				byDocument[result.Document] = &mergedResult
				continue
			}
			if !sameResultAuthority(*current, result) {
				return Report{}, errors.New("query variants returned incompatible document authority")
			}
			current.Score += score
			current.Explanation = append(current.Explanation, result.Explanation...)
			current.Evidence = append(current.Evidence, result.Evidence...)
		}
	}
	merged.Results = make([]Result, 0, len(byDocument))
	for _, result := range byDocument {
		merged.Results = append(merged.Results, *result)
	}
	slices.SortFunc(merged.Results, compareResults)
	if len(merged.Results) > limit {
		merged.Results = merged.Results[:limit]
		merged.Truncated = true
	}
	for index := range merged.Results {
		merged.Results[index].Rank = index + 1
	}
	return merged, nil
}

func resultContributionScore(result Result) (float64, error) {
	if len(result.Explanation) == 0 {
		return 0, errors.New("query variant result lacks rank contributions")
	}
	var score float64
	for _, contribution := range result.Explanation {
		if contribution.Rank < 1 || contribution.Lane != LaneLexical && contribution.Lane != LaneSemantic ||
			math.IsNaN(contribution.Contribution) || math.IsInf(contribution.Contribution, 0) ||
			contribution.Contribution <= 0 {
			return 0, errors.New("query variant result has invalid rank contributions")
		}
		score += contribution.Contribution
	}
	return score, nil
}

func sameResultAuthority(left, right Result) bool {
	if left.Path != right.Path {
		return false
	}
	leftRevision, leftOK := resultNodeRevision(left)
	rightRevision, rightOK := resultNodeRevision(right)
	return leftOK && rightOK && leftRevision == rightRevision
}

func resultNodeRevision(result Result) (int64, bool) {
	var revision int64
	for _, evidence := range result.Evidence {
		if evidence.NodeID != result.Document.NodeID || evidence.ContentVersionID != result.Document.ContentVersionID ||
			evidence.NodeRevision <= 0 {
			return 0, false
		}
		if revision == 0 {
			revision = evidence.NodeRevision
		} else if revision != evidence.NodeRevision {
			return 0, false
		}
	}
	return revision, revision > 0
}

func conservativeCoverage(reports []Report) Coverage {
	coverage := reports[0].Coverage
	complete := coverage.CompleteDocuments
	unknown := coverage.State == CoverageUnknown
	incomplete := coverage.State == CoverageIncomplete
	for _, report := range reports[1:] {
		coverage.BindingRequired = coverage.BindingRequired || report.Coverage.BindingRequired
		coverage.ScopedDocuments = max(coverage.ScopedDocuments, report.Coverage.ScopedDocuments)
		complete = min(complete, report.Coverage.CompleteDocuments)
		unknown = unknown || report.Coverage.State == CoverageUnknown
		incomplete = incomplete || report.Coverage.State == CoverageIncomplete
	}
	if unknown {
		coverage.ScopedDocuments = 0
		coverage.CompleteDocuments = 0
		coverage.State = CoverageUnknown
		return coverage
	}
	coverage.CompleteDocuments = complete
	if incomplete || coverage.CompleteDocuments != coverage.ScopedDocuments {
		coverage.State = CoverageIncomplete
		return coverage
	}
	coverage.State = CoverageComplete
	return coverage
}
