package mcp

import (
	"reflect"

	"go.kenn.io/docbank/internal/api"
)

type answerSearchHit struct {
	result    api.DocumentSearchResult
	relevance *float64
}

func answerSearchHits(report api.DocumentSearchReport, allowRelevance bool) []answerSearchHit {
	hits := make([]answerSearchHit, 0, len(report.Results))
	for index, result := range report.Results {
		hit := answerSearchHit{result: result}
		if allowRelevance {
			hit.relevance = searchRerankRelevance(result, index, report.Reranking)
		}
		hits = append(hits, hit)
	}
	return hits
}

func answerContentEvidence(result api.DocumentSearchResult) []api.DocumentEvidenceReference {
	evidence := make([]api.DocumentEvidenceReference, 0, len(result.Evidence))
	for _, item := range result.Evidence {
		if item.Kind == "rendition_segment" || item.Kind == "content_blob" {
			evidence = append(evidence, item)
		}
	}
	return evidence
}

func sameAnswerLead(a, b api.DocumentSearchResult) bool {
	return a.NodeID == b.NodeID && a.ContentVersionID == b.ContentVersionID && a.Path == b.Path && reflect.DeepEqual(a.Evidence, b.Evidence)
}

func sameAnswerSearchResult(a, b api.DocumentSearchResult) bool {
	aHasContent := answerContentExcerpt(a)
	bHasContent := answerContentExcerpt(b)
	if aHasContent != bHasContent {
		return false
	}
	if !aHasContent {
		return sameAnswerLead(a, b)
	}
	if a.ContentVersionID != b.ContentVersionID || a.Excerpt != b.Excerpt {
		return false
	}
	return reflect.DeepEqual(answerContentEvidence(a), answerContentEvidence(b))
}

func mergeAnswerSearchResults(questionHits []answerSearchHit, terms []api.DocumentSearchReport, limit int) ([]answerSearchHit, bool) {
	maxResults := 0
	for _, report := range terms {
		maxResults = max(maxResults, len(report.Results))
	}
	var merged []answerSearchHit
	seen := append([]answerSearchHit(nil), questionHits...)
	questionIndex := 0
	for index := range maxResults {
		for _, report := range terms {
			if index >= len(report.Results) {
				continue
			}
			if questionIndex < len(questionHits) {
				merged = append(merged, questionHits[questionIndex])
				questionIndex++
			}
			candidate := answerSearchHit{result: report.Results[index]}
			if !answerSearchHitExists(seen, candidate) {
				merged = append(merged, candidate)
				seen = append(seen, candidate)
			}
		}
	}
	merged = append(merged, questionHits[questionIndex:]...)

	truncated := len(merged) > limit
	if truncated {
		merged = merged[:limit]
	}
	for index := range merged {
		merged[index].result.Rank = index + 1
	}
	return merged, truncated
}

func answerSearchHitExists(hits []answerSearchHit, candidate answerSearchHit) bool {
	for _, existing := range hits {
		if sameAnswerSearchResult(existing.result, candidate.result) {
			return true
		}
	}
	return false
}
