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
	if a.ContentVersionID != b.ContentVersionID {
		return false
	}
	return reflect.DeepEqual(answerContentEvidence(a), answerContentEvidence(b))
}

func mergeAnswerSearchResults(question, terms api.DocumentSearchReport, limit int) ([]answerSearchHit, bool) {
	questionHits := answerSearchHits(question, true)
	termsHits := answerSearchHits(terms, false)
	merged := make([]answerSearchHit, 0, len(questionHits)+len(termsHits))

	for questionIndex, termsIndex := 0, 0; questionIndex < len(questionHits) || termsIndex < len(termsHits); {
		if questionIndex < len(questionHits) {
			merged = append(merged, questionHits[questionIndex])
			questionIndex++
		}
		if termsIndex < len(termsHits) {
			candidate := termsHits[termsIndex]
			if !answerSearchHitExists(questionHits, candidate) && !answerSearchHitExists(merged, candidate) {
				merged = append(merged, candidate)
			}
			termsIndex++
		}
	}

	truncated := len(merged) > limit
	if truncated {
		merged = merged[:limit]
	}
	for index := range merged {
		merged[index].result.Rank = index + 1
	}
	return merged, truncated
}

func interleaveAnswerTerms(reports []api.DocumentSearchReport) api.DocumentSearchReport {
	var combined api.DocumentSearchReport
	maxResults := 0
	for _, report := range reports {
		combined.Truncated = combined.Truncated || report.Truncated
		maxResults = max(maxResults, len(report.Results))
	}
	for index := range maxResults {
		for _, report := range reports {
			if index < len(report.Results) {
				combined.Results = append(combined.Results, report.Results[index])
			}
		}
	}
	return combined
}

func answerSearchHitExists(hits []answerSearchHit, candidate answerSearchHit) bool {
	for _, existing := range hits {
		if sameAnswerSearchResult(existing.result, candidate.result) {
			return true
		}
	}
	return false
}
