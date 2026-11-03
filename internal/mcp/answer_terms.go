package mcp

import (
	"reflect"
	"slices"

	"go.kenn.io/docbank/internal/api"
)

type answerSearchHit struct {
	result    api.DocumentSearchResult
	relevance *float64
}

func answerSearchHits(report api.DocumentSearchReport) []answerSearchHit {
	hits := make([]answerSearchHit, 0, len(report.Results))
	for index, result := range report.Results {
		hit := answerSearchHit{result: result, relevance: searchRerankRelevance(result, index, report.Reranking)}
		hits = append(hits, hit)
	}
	return hits
}

func answerContentEvidence(result api.DocumentSearchResult) []api.DocumentEvidenceReference {
	evidence := make([]api.DocumentEvidenceReference, 0, len(result.Evidence))
	for _, item := range result.Evidence {
		if answerContentKind(item.Kind) {
			evidence = append(evidence, item)
		}
	}
	return evidence
}

func sameAnswerLead(a, b api.DocumentSearchResult) bool {
	return a.NodeID == b.NodeID && a.ContentVersionID == b.ContentVersionID && a.Path == b.Path
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

func mergeAnswerLeadHits(existing, candidate answerSearchHit) answerSearchHit {
	if existing.relevance == nil {
		existing.relevance = candidate.relevance
	}
	evidence := slices.Clone(existing.result.Evidence)
	for _, item := range candidate.result.Evidence {
		if !slices.ContainsFunc(evidence, func(existing api.DocumentEvidenceReference) bool {
			return reflect.DeepEqual(existing, item)
		}) {
			evidence = append(evidence, item)
		}
	}
	existing.result.Evidence = evidence
	return existing
}

func mergeAnswerSearchResults(questionHits []answerSearchHit, terms []api.DocumentSearchReport, limit int) ([]answerSearchHit, bool) {
	questionHits = slices.Clone(questionHits)
	maxResults := 0
	for _, report := range terms {
		maxResults = max(maxResults, len(report.Results))
	}
	var merged []*answerSearchHit
	seen := make([]*answerSearchHit, 0, len(questionHits))
	questionHitIndexes := make(map[*answerSearchHit]int, len(questionHits))
	questionEmitted := make([]bool, len(questionHits))
	for index := range questionHits {
		hit := &questionHits[index]
		seen = append(seen, hit)
		questionHitIndexes[hit] = index
	}
	questionIndex := 0
	emitNextQuestion := func() {
		for questionIndex < len(questionHits) {
			index := questionIndex
			questionIndex++
			if questionEmitted[index] {
				continue
			}
			merged = append(merged, seen[index])
			questionEmitted[index] = true
			return
		}
	}
	for index := range maxResults {
		for _, report := range terms {
			if index >= len(report.Results) {
				continue
			}
			emitNextQuestion()
			candidate := &answerSearchHit{result: report.Results[index]}
			var duplicate *answerSearchHit
			for _, existing := range seen {
				if sameAnswerSearchResult(existing.result, candidate.result) {
					duplicate = existing
					break
				}
			}
			if duplicate != nil {
				if !answerContentExcerpt(candidate.result) {
					*duplicate = mergeAnswerLeadHits(*duplicate, *candidate)
				}
				if index, isQuestionHit := questionHitIndexes[duplicate]; isQuestionHit && !questionEmitted[index] {
					merged = append(merged, duplicate)
					questionEmitted[index] = true
				}
				continue
			}
			seen = append(seen, candidate)
			merged = append(merged, candidate)
		}
	}
	for ; questionIndex < len(questionHits); questionIndex++ {
		if questionEmitted[questionIndex] {
			continue
		}
		merged = append(merged, seen[questionIndex])
		questionEmitted[questionIndex] = true
	}

	truncated := len(merged) > limit
	if truncated {
		merged = merged[:limit]
	}
	hits := make([]answerSearchHit, len(merged))
	for index, hit := range merged {
		hits[index] = *hit
		hits[index].result.Rank = index + 1
	}
	return hits, truncated
}
