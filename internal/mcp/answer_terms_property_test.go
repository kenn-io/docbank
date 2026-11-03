//go:build !darwin || arm64

package mcp

import (
	"reflect"
	"strconv"
	"testing"

	"go.kenn.io/docbank/internal/api"
	"hegel.dev/go/hegel"
)

func TestMergeAnswerSearchResultsEmitsLaterQuestionHitAtTermPositionProperty(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		trailingQuestionHits := hegel.Draw(ht, hegel.Integers(0, 4))
		questionRelevance := 0.7
		passage := func(id int) api.DocumentSearchResult {
			return api.DocumentSearchResult{
				NodeID:           int64(id),
				ContentVersionID: "version-" + strconv.Itoa(id),
				Path:             "/document-" + strconv.Itoa(id) + ".txt",
				Excerpt:          "passage-" + strconv.Itoa(id),
				Evidence:         []api.DocumentEvidenceReference{{Kind: "content_blob"}},
			}
		}
		questionHits := []answerSearchHit{
			{result: passage(1), relevance: &questionRelevance},
			{result: passage(2), relevance: &questionRelevance},
			{result: passage(3), relevance: &questionRelevance},
		}
		for offset := range trailingQuestionHits {
			questionHits = append(questionHits, answerSearchHit{result: passage(4 + offset)})
		}
		termReports := []api.DocumentSearchReport{
			{Results: []api.DocumentSearchResult{questionHits[2].result}},
			{Results: []api.DocumentSearchResult{passage(100)}},
		}

		got, truncated := mergeAnswerSearchResults(questionHits, termReports, 3)
		if !truncated {
			ht.Fatal("expected the combined result set to be truncated")
		}
		if len(got) != 3 {
			ht.Fatalf("got %d hits, want 3", len(got))
		}
		want := []api.DocumentSearchResult{
			questionHits[0].result,
			questionHits[2].result,
			questionHits[1].result,
		}
		for index := range want {
			if got[index].result.ContentVersionID != want[index].ContentVersionID {
				ht.Fatalf("hit %d has version %q, want %q", index, got[index].result.ContentVersionID, want[index].ContentVersionID)
			}
		}
		if got[1].relevance == nil || *got[1].relevance != questionRelevance {
			ht.Fatalf("matched question hit lost relevance: got %v, want %v", got[1].relevance, questionRelevance)
		}
		if !reflect.DeepEqual(got[1].result.Evidence, want[1].Evidence) {
			ht.Fatalf("matched question hit changed evidence: got %v, want %v", got[1].result.Evidence, want[1].Evidence)
		}
	}, hegel.WithDatabase(t.TempDir()))
}
