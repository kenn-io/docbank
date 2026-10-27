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
	if a.ContentVersionID != b.ContentVersionID || (a.ContentVersionID == "" && a.NodeID != b.NodeID) {
		return false
	}
	return reflect.DeepEqual(answerContentEvidence(a), answerContentEvidence(b))
}

func mergeAnswerSearchResults(question, terms api.DocumentSearchReport, limit int) ([]answerSearchHit, bool) {
	questionHits := uniqueAnswerSearchHits(answerSearchHits(question, true), nil)
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

	if limit < 0 {
		limit = 0
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

// uniqueAnswerSearchHits keeps the first representation of each result. In
// particular, callers pass question hits as prior results so a terms match
// cannot replace its excerpt, evidence, or reranker score.
func uniqueAnswerSearchHits(hits, prior []answerSearchHit) []answerSearchHit {
	unique := make([]answerSearchHit, 0, len(hits))
	for _, hit := range hits {
		duplicate := false
		for _, existing := range prior {
			if sameAnswerSearchResult(existing.result, hit.result) {
				duplicate = true
				break
			}
		}
		if duplicate {
			continue
		}
		unique = append(unique, hit)
		prior = append(prior, hit)
	}
	return unique
}

func answerSearchHitExists(hits []answerSearchHit, candidate answerSearchHit) bool {
	for _, existing := range hits {
		if sameAnswerSearchResult(existing.result, candidate.result) {
			return true
		}
	}
	return false
}
