package mcp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"go.kenn.io/docbank/internal/answer"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/store"
)

type answerTermsClient struct{ *answerFakeClient }

func answerTermsSearchReport(
	req api.DocumentSearchRequest,
	coverage api.DocumentSearchCoverage,
	results ...api.DocumentSearchResult,
) api.DocumentSearchReport {
	report := api.DocumentSearchReport{
		RequestedMode: req.Mode,
		ActualMode:    req.Mode,
		Coverage:      coverage,
		Results:       slices.Clone(results),
	}
	for i := range report.Results {
		result := &report.Results[i]
		result.VaultUID = req.Fence.VaultUID
		if result.ContentVersionID == "" && i < len(req.Fence.ContentVersionIDs) {
			result.ContentVersionID = req.Fence.ContentVersionIDs[i]
		}
		result.Rank = i + 1
		switch req.Mode {
		case "lexical":
			result.SemanticRank = 0
			if result.LexicalRank == 0 {
				result.LexicalRank = i + 1
			}
		case "semantic":
			result.LexicalRank = 0
			if result.SemanticRank == 0 {
				result.SemanticRank = i + 1
			}
		case "hybrid":
			if result.LexicalRank == 0 && result.SemanticRank == 0 {
				result.LexicalRank = i + 1
			}
		}
	}
	return report
}

func (f *answerTermsClient) ResolveDocumentSummaries(ctx context.Context, req api.DocumentSummaryResolveRequest) ([]api.DocumentSummary, error) {
	seen := map[api.DocumentIdentity]bool{}
	for _, identity := range req.Identities {
		if seen[identity] {
			return nil, store.ErrInvalidDocumentQuery
		}
		seen[identity] = true
	}
	return f.answerFakeClient.ResolveDocumentSummaries(ctx, req)
}

// Dropping the terms pass leaves a natural question with only a semantic lead.
func TestAnswerQuestionTermsRecoverLexicalPassage(t *testing.T) {
	catalog, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "search.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = catalog.Close() })
	node, err := catalog.CreateFile(t.Context(), catalog.RootID(), "lease.txt", strings.Repeat("b", 64), 1, "text/plain")
	if err != nil {
		t.Fatal(err)
	}
	if err := catalog.RecordExtraction(t.Context(), store.ExtractionResult{BlobHash: node.BlobHash, Extractor: "plain-text", ExtractorVersion: 1, Status: store.ExtractionOK, Text: "Lease renewal is 1 March."}); err != nil {
		t.Fatal(err)
	}
	f := &answerTermsClient{answerFakeClient: &answerFakeClient{}}
	f.fenceVersions = []string{node.CurrentVersionID}
	f.documents = []api.DocumentSummary{{NodeID: node.ID, ContentVersionID: node.CurrentVersionID, Name: "lease.txt", ActiveRenditions: []api.DocumentRenditionIdentity{{AttachmentID: strings.Repeat("c", 64)}}}}
	embedding := api.DocumentEvidenceReference{Kind: "embedding", VectorSpaceID: strings.Repeat("c", 64), EmbeddingSetID: strings.Repeat("d", 64), InputGenerationID: strings.Repeat("e", 64), InputID: "input-a", InputKind: "rendition_chunk", BuildID: strings.Repeat("a", 64), SourceManifestChecksum: strings.Repeat("f", 64)}
	f.searchReport = func(req api.DocumentSearchRequest) (api.DocumentSearchReport, error) {
		coverage := api.DocumentSearchCoverage{State: "incomplete", ScopedDocuments: 1}
		if req.Mode == "hybrid" {
			report := answerTermsSearchReport(req, coverage, api.DocumentSearchResult{
				NodeID: node.ID, ContentVersionID: node.CurrentVersionID, Path: "/lease.txt", Score: 0.7,
				SemanticRank: 1, Evidence: []api.DocumentEvidenceReference{embedding},
			})
			if req.Rerank {
				report.Reranking = &api.DocumentSearchRerankingReceipt{Outcome: "applied", CandidateCount: 1}
			}
			return report, nil
		}
		hits, _, err := catalog.SearchExplainedLexicalCandidates(t.Context(), req.Query, req.Limit, store.SearchOptions{ContentVersionIDs: []string{node.CurrentVersionID}}, req.ContentFirst)
		if err != nil {
			return api.DocumentSearchReport{}, err
		}
		results := make([]api.DocumentSearchResult, 0, len(hits))
		for i, hit := range hits {
			results = append(results, api.DocumentSearchResult{NodeID: hit.Node.ID, ContentVersionID: hit.Node.CurrentVersionID, Path: hit.Path, LexicalRank: i + 1, Excerpt: hit.Excerpt, Evidence: []api.DocumentEvidenceReference{{Kind: hit.EvidenceKind}}})
		}
		return answerTermsSearchReport(req, coverage, results...), nil
	}
	session := func(ctx context.Context, fn func(context.Context, answerClient) error) error { return fn(ctx, f) }
	without, _, err := answerQuestion(t.Context(), session, []byte(`{"question":"what is the renewal date of the lease"}`))
	if err != nil || len(without.Leads) != 1 || len(without.Passages) != 0 {
		t.Fatalf("without=%+v err=%v", without, err)
	}
	with, links, err := answerQuestion(t.Context(), session, []byte(`{"question":"what is the renewal date of the lease","terms":["lease","renewal"],"rerank":true}`))
	if err != nil || with.Status != answer.Answered || len(with.Passages) != 1 || len(links) != 1 || with.CoverageComplete {
		t.Fatalf("with=%+v err=%v", with, err)
	}
	if len(f.requests) != 3 {
		t.Fatalf("requests=%+v", f.requests)
	}
	question, terms := f.requests[1], f.requests[2]
	if terms.Query != "lease renewal" || terms.Mode != "lexical" || terms.Rerank || terms.BindingID != "" || !terms.ContentFirst || terms.Profile != question.Profile || !reflect.DeepEqual(terms.Fence, question.Fence) {
		t.Fatalf("question=%+v terms=%+v", question, terms)
	}
	encoded, err := json.Marshal(with)
	if err != nil {
		t.Fatal(err)
	}
	var wire struct {
		Receipt struct {
			TermsSearched bool   `json:"terms_searched"`
			TermsSHA256   string `json:"terms_sha256"`
		} `json:"receipt"`
	}
	if err := json.Unmarshal(encoded, &wire); err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256([]byte("lease renewal"))
	if !wire.Receipt.TermsSearched || wire.Receipt.TermsSHA256 != hex.EncodeToString(hash[:]) || strings.Contains(string(encoded), `"terms":`) {
		t.Fatalf("receipt=%+v", wire.Receipt)
	}
	_, output := answerQuestionSchemas()
	var normalized any
	if err := json.Unmarshal(encoded, &normalized); err != nil {
		t.Fatal(err)
	}
	assertSchemaAccepts(t, output, normalized)
}

func TestAnswerMergeTermsPreservesProvenanceAndContentIdentity(t *testing.T) {
	passage := func(version, segment string, rank int) api.DocumentSearchResult {
		return api.DocumentSearchResult{ContentVersionID: version, Rank: rank, LexicalRank: rank, Excerpt: "text", Score: 0.7, Evidence: []api.DocumentEvidenceReference{{Kind: "rendition_segment", BuildID: "build", SegmentID: segment}}}
	}
	a, b, c := passage("v1", "a", 1), passage("v2", "b", 2), passage("v3", "c", 2)
	duplicate := a
	a.Evidence = append(slices.Clone(a.Evidence), api.DocumentEvidenceReference{Kind: "embedding", VectorSpaceID: "extra"})
	duplicate.Rank = 1
	question := api.DocumentSearchReport{Results: []api.DocumentSearchResult{a, b}, Reranking: &api.DocumentSearchRerankingReceipt{Outcome: "applied", CandidateCount: 2}}
	terms := api.DocumentSearchReport{Results: []api.DocumentSearchResult{duplicate, c}}
	hits, truncated := mergeAnswerSearchResults(question, terms, 3)
	if truncated || len(hits) != 3 {
		t.Fatalf("hits=%+v truncated=%t", hits, truncated)
	}
	for i, want := range []string{"v1", "v2", "v3"} {
		if hits[i].result.ContentVersionID != want || hits[i].result.Rank != i+1 {
			t.Fatalf("hit %d=%+v", i, hits[i])
		}
	}
	if hits[0].relevance == nil || hits[1].relevance == nil || hits[2].relevance != nil {
		t.Fatalf("incorrect relevance: %+v", hits)
	}
	hits, truncated = mergeAnswerSearchResults(question, terms, 2)
	if !truncated || len(hits) != 2 {
		t.Fatalf("hits=%+v truncated=%t", hits, truncated)
	}
	terms.Results = []api.DocumentSearchResult{c}
	hits, truncated = mergeAnswerSearchResults(question, terms, 3)
	if truncated || len(hits) != 3 || hits[1].result.ContentVersionID != "v3" || hits[1].relevance != nil || hits[2].result.ContentVersionID != "v2" || hits[2].relevance == nil {
		t.Fatalf("relevance lost its search provenance after interleaving: %+v", hits)
	}
}

func TestAnswerMergeTermsKeepsQuestionPassageProvenance(t *testing.T) {
	first := api.DocumentSearchResult{NodeID: 1, ContentVersionID: "v1", Rank: 1, LexicalRank: 1, Excerpt: "first", Evidence: []api.DocumentEvidenceReference{{Kind: "content_blob"}}}
	passage := api.DocumentSearchResult{NodeID: 2, ContentVersionID: "v2", Rank: 2, LexicalRank: 2, Excerpt: "question excerpt", Score: 0.7, Evidence: []api.DocumentEvidenceReference{{Kind: "rendition_segment", BuildID: "b", SegmentID: "s"}, {Kind: "embedding", VectorSpaceID: "vs"}}}
	lexical := passage
	lexical.Excerpt = "terms excerpt"
	lexical.Evidence = slices.Clone(passage.Evidence[:1])
	question := api.DocumentSearchReport{Results: []api.DocumentSearchResult{first, passage}, Reranking: &api.DocumentSearchRerankingReceipt{Outcome: "applied", CandidateCount: 2}}
	terms := api.DocumentSearchReport{Results: []api.DocumentSearchResult{lexical}}
	hits, truncated := mergeAnswerSearchResults(question, terms, 2)
	if truncated || len(hits) != 2 || hits[1].relevance == nil || *hits[1].relevance != 0.7 || hits[1].result.Excerpt != "question excerpt" || len(hits[1].result.Evidence) != 2 || hits[1].result.Rank != 2 {
		t.Fatalf("question provenance lost: hits=%+v truncated=%t", hits, truncated)
	}
}

func TestAnswerMergeTermsDeduplicatesIdenticalLeads(t *testing.T) {
	lead := api.DocumentSearchResult{NodeID: 7, ContentVersionID: "v", Path: "/lease.pdf", Rank: 1, Evidence: []api.DocumentEvidenceReference{{Kind: "node_name"}}}
	other := lead
	other.NodeID = 9
	other.ContentVersionID = "v3"
	other.Path = "/other.pdf"
	other.Evidence = slices.Clone(lead.Evidence)
	passage := lead
	passage.NodeID = 8
	passage.ContentVersionID = "v2"
	passage.LexicalRank = 1
	passage.Excerpt = "content"
	passage.Evidence = []api.DocumentEvidenceReference{{Kind: "content_blob"}}
	question := api.DocumentSearchReport{Results: []api.DocumentSearchResult{lead, passage}}
	terms := api.DocumentSearchReport{Results: []api.DocumentSearchResult{lead, other}}
	hits, truncated := mergeAnswerSearchResults(question, terms, 3)
	if truncated || len(hits) != 3 || hits[0].result.NodeID != 7 || !answerContentExcerpt(hits[1].result) || hits[2].result.NodeID != 9 {
		t.Fatalf("hits=%+v truncated=%t", hits, truncated)
	}
	first := api.DocumentSearchResult{NodeID: 8, ContentVersionID: "v0", Rank: 1}
	lead.Rank, lead.Score = 2, 0.7
	question = api.DocumentSearchReport{Results: []api.DocumentSearchResult{first, lead}, Reranking: &api.DocumentSearchRerankingReceipt{Outcome: "applied", CandidateCount: 2}}
	terms = api.DocumentSearchReport{Results: []api.DocumentSearchResult{lead}}
	hits, truncated = mergeAnswerSearchResults(question, terms, 2)
	if truncated || len(hits) != 2 || hits[1].relevance == nil || *hits[1].relevance != 0.7 {
		t.Fatalf("question relevance lost on duplicate: hits=%+v truncated=%t", hits, truncated)
	}
}

func FuzzAnswerMergeIdenticalLeads(f *testing.F) {
	f.Add([]byte{0, 1, 0, 2})
	f.Add([]byte{0, 0, 0})
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > 30 {
			data = data[:30]
		}
		question := api.DocumentSearchReport{RequestedMode: "hybrid", ActualMode: "hybrid", Coverage: api.DocumentSearchCoverage{State: "complete", ScopedDocuments: 12, CompleteDocuments: 12}}
		terms := api.DocumentSearchReport{RequestedMode: "lexical", ActualMode: "lexical", Coverage: api.DocumentSearchCoverage{State: "complete", ScopedDocuments: 12, CompleteDocuments: 12}}
		seen := map[byte]bool{}
		questionVersions := map[byte]bool{}
		termsVersions := map[byte]bool{}
		for i, value := range data {
			seen[value] = true
			lead := api.DocumentSearchResult{NodeID: int64(value) + 1, ContentVersionID: fmt.Sprintf("version-%d", value), Evidence: []api.DocumentEvidenceReference{{Kind: "node_name"}}}
			if i%2 == 0 {
				if questionVersions[value] {
					continue
				}
				questionVersions[value] = true
				question.Results = append(question.Results, lead)
			} else {
				if termsVersions[value] {
					continue
				}
				termsVersions[value] = true
				terms.Results = append(terms.Results, lead)
			}
		}
		hits, truncated := mergeAnswerSearchResults(question, terms, 50)
		if truncated || len(hits) != len(seen) {
			t.Fatalf("distinct leads=%d hits=%d truncated=%t", len(seen), len(hits), truncated)
		}
		for _, hit := range hits {
			key := byte(hit.result.NodeID - 1)
			if !seen[key] {
				t.Fatalf("unexpected or repeated lead %d", key)
			}
			delete(seen, key)
		}
	})
}

func TestAnswerMergeTermsKeepsDistinctContentIdentities(t *testing.T) {
	question := api.DocumentSearchReport{Results: []api.DocumentSearchResult{{NodeID: 1, ContentVersionID: "v", Rank: 1, LexicalRank: 1, Excerpt: "text", Evidence: []api.DocumentEvidenceReference{{Kind: "content_blob"}, {Kind: "embedding"}}}}}
	terms := api.DocumentSearchReport{Results: []api.DocumentSearchResult{
		{NodeID: 1, ContentVersionID: "v", Rank: 1, LexicalRank: 1, Excerpt: "text", Evidence: []api.DocumentEvidenceReference{{Kind: "content_blob"}}},
		{NodeID: 2, ContentVersionID: "v2", Rank: 2, LexicalRank: 2, Excerpt: "other text", Evidence: []api.DocumentEvidenceReference{{Kind: "rendition_segment", BuildID: "one", SegmentID: "segment"}}},
		{NodeID: 3, ContentVersionID: "v3", Rank: 3, LexicalRank: 3, Excerpt: "other build", Evidence: []api.DocumentEvidenceReference{{Kind: "rendition_segment", BuildID: "two", SegmentID: "segment"}}},
	}}
	hits, truncated := mergeAnswerSearchResults(question, terms, 10)
	if truncated || len(hits) != 3 || hits[0].result.Excerpt != "text" || hits[1].result.Excerpt != "other text" || hits[2].result.Excerpt != "other build" {
		t.Fatalf("hits=%+v truncated=%t", hits, truncated)
	}
}

func FuzzAnswerMergeTerms(f *testing.F) {
	f.Add([]byte{0, 1, 2, 3, 4, 5}, uint8(4))
	f.Add([]byte{0, 0, 0, 0}, uint8(1))
	f.Fuzz(func(t *testing.T, data []byte, capByte uint8) {
		if len(data) > 100 {
			data = data[:100]
		}
		limit := int(capByte%50) + 1
		question := api.DocumentSearchReport{}
		terms := api.DocumentSearchReport{}
		questionRelevance := map[int64]float64{}
		allVersions := map[string]bool{}
		questionVersions := map[string]bool{}
		termsVersions := map[string]bool{}
		for i, id := range data {
			identity := int(id%12) + 1
			version := fmt.Sprintf("00000000-0000-4000-8000-%012d", identity)
			r := api.DocumentSearchResult{VaultUID: "00000000-0000-4000-8000-000000000001", NodeID: int64(identity), ContentVersionID: version, Path: fmt.Sprintf("/fixture-%d", identity), Excerpt: strings.Repeat("界", int(id%8)+1), Score: 0.9, Evidence: []api.DocumentEvidenceReference{{Kind: "rendition_segment", BuildID: strings.Repeat("a", 64), SegmentID: fmt.Sprintf("segment-%d", identity)}}}
			if i%2 == 0 {
				if questionVersions[version] || len(question.Results) == limit {
					continue
				}
				questionVersions[version] = true
				r.Rank = len(question.Results) + 1
				r.LexicalRank = r.Rank
				question.Results = append(question.Results, r)
				allVersions[version] = true
				if r.Rank <= 2 {
					questionRelevance[r.NodeID] = r.Score
				}
			} else {
				if termsVersions[version] || len(terms.Results) == limit {
					continue
				}
				termsVersions[version] = true
				r.Rank = len(terms.Results) + 1
				r.LexicalRank = r.Rank
				terms.Results = append(terms.Results, r)
				allVersions[version] = true
			}
		}
		if len(question.Results) == 0 {
			question.Reranking = &api.DocumentSearchRerankingReceipt{Outcome: "skipped"}
		} else {
			question.Reranking = &api.DocumentSearchRerankingReceipt{Outcome: "applied", CandidateCount: min(2, len(question.Results))}
		}
		if len(question.Results) > limit || len(terms.Results) > limit {
			t.Fatal("search report exceeded its requested result limit")
		}
		hits, truncated := mergeAnswerSearchResults(question, terms, limit)
		again, againTruncated := mergeAnswerSearchResults(question, terms, limit)
		if !reflect.DeepEqual(hits, again) || truncated != againTruncated {
			t.Fatal("nondeterministic merge")
		}
		if len(hits) != min(limit, len(allVersions)) || truncated != (len(allVersions) > limit) {
			t.Fatalf("limit=%d unique=%d hits=%d truncated=%t", limit, len(allVersions), len(hits), truncated)
		}
		seen := map[string]bool{}
		for i, hit := range hits {
			v := hit.result.ContentVersionID
			if seen[v] || hit.result.Rank != i+1 {
				t.Fatal("duplicate or nonsequential rank")
			}
			seen[v] = true
			wantRelevance, reranked := questionRelevance[hit.result.NodeID]
			if (hit.relevance != nil) != reranked || (reranked && *hit.relevance != wantRelevance) {
				t.Fatal("relevance lost its original question prefix provenance")
			}
		}
		if len(question.Results) > 0 && (len(hits) == 0 || hits[0].result.ContentVersionID != question.Results[0].ContentVersionID) {
			t.Fatal("question did not lead")
		}
		passages := make([]answer.Passage, len(hits))
		for i, hit := range hits {
			passages[i] = answer.Passage{Text: hit.result.Excerpt, Rank: hit.result.Rank}
		}
		budget := int(capByte)
		included, used, omitted := answer.Budget(passages, budget)
		if used > budget || len(included)+omitted != len(passages) || !reflect.DeepEqual(included, passages[:len(included)]) {
			t.Fatal("budget broke prefix or bound")
		}
	})
}

func TestAnswerQuestionTermsFailureIsReturned(t *testing.T) {
	failure := errors.New("terms search failed")
	f := &answerTermsClient{answerFakeClient: &answerFakeClient{searchReport: func(req api.DocumentSearchRequest) (api.DocumentSearchReport, error) {
		if req.Mode == "lexical" {
			return api.DocumentSearchReport{}, failure
		}
		return answerTermsSearchReport(req, api.DocumentSearchCoverage{State: "complete", ScopedDocuments: 1, CompleteDocuments: 1}), nil
	}}}
	session := func(ctx context.Context, fn func(context.Context, answerClient) error) error { return fn(ctx, f) }
	_, _, err := answerQuestion(t.Context(), session, []byte(`{"question":"question","terms":["secret"]}`))
	if !errors.Is(err, failure) || len(f.requests) != 2 {
		t.Fatalf("err=%v requests=%+v", err, f.requests)
	}
}

func TestAnswerQuestionTermsSchema(t *testing.T) {
	input, _ := answerQuestionSchemas()
	for _, terms := range []any{[]any{"lease"}, []any{strings.Repeat("界", 64)}, []any{"a", "b", "c", "d", "e", "f", "g", "h"}} {
		assertSchemaAccepts(t, input, map[string]any{"question": "q", "terms": terms})
	}
	for _, terms := range []any{nil, []any{}, []any{""}, []any{strings.Repeat("界", 65)}, []any{"a", "b", "c", "d", "e", "f", "g", "h", "i"}} {
		assertSchemaRejects(t, input, map[string]any{"question": "q", "terms": terms})
	}
}

func TestAnswerQuestionTermsEmptyFence(t *testing.T) {
	f := &answerFakeClient{empty: true}
	session := func(ctx context.Context, fn func(context.Context, answerClient) error) error { return fn(ctx, f) }
	out, _, err := answerQuestion(t.Context(), session, []byte(`{"question":"q","terms":["term"]}`))
	if err != nil || len(f.requests) != 0 || f.validations != 2 || out.Status != answer.NoMatch {
		t.Fatalf("out=%+v err=%v calls=%d validations=%d", out, err, len(f.requests), f.validations)
	}
}

func TestAnswerQuestionTermsTruncated(t *testing.T) {
	f := &answerTermsClient{answerFakeClient: &answerFakeClient{
		fenceVersions: []string{"00000000-0000-4000-8000-000000000002", "00000000-0000-4000-8000-000000000003"},
		searchReport: func(req api.DocumentSearchRequest) (api.DocumentSearchReport, error) {
			coverage := api.DocumentSearchCoverage{State: "complete", ScopedDocuments: 2, CompleteDocuments: 2}
			if req.Mode != "lexical" {
				return answerTermsSearchReport(req, coverage), nil
			}
			report := answerTermsSearchReport(req, coverage, api.DocumentSearchResult{
				NodeID: 2, ContentVersionID: req.Fence.ContentVersionIDs[0], Path: "/fixture-terms",
				Excerpt: "term passage", Evidence: []api.DocumentEvidenceReference{{Kind: "content_blob"}},
			})
			report.Truncated = true
			return report, nil
		},
	}}
	session := func(ctx context.Context, fn func(context.Context, answerClient) error) error { return fn(ctx, f) }
	out, _, err := answerQuestion(t.Context(), session, []byte(`{"question":"q","terms":["term"],"limit":1}`))
	if err != nil || out.Status != answer.Answered || !out.CoverageComplete || len(out.Passages) != 1 || out.Receipt.ObservedScopeCount != 2 || !slices.Contains(out.CoverageReasons, "retrieval_truncated") {
		t.Fatalf("out=%+v err=%v", out, err)
	}
}

func TestAnswerQuestionTermsLimitAndPassageBudget(t *testing.T) {
	fenceVersions := []string{
		"00000000-0000-4000-8000-000000000001",
		"00000000-0000-4000-8000-000000000002",
		"00000000-0000-4000-8000-000000000003",
		"00000000-0000-4000-8000-000000000004",
	}
	f := &answerTermsClient{answerFakeClient: &answerFakeClient{
		fenceVersions: fenceVersions,
		searchReport: func(req api.DocumentSearchRequest) (api.DocumentSearchReport, error) {
			results := make([]api.DocumentSearchResult, 0, 2)
			for i := 1; i <= 2; i++ {
				nodeID := int64(i)
				if req.Mode == "lexical" {
					nodeID += 2
				}
				version := fmt.Sprintf("00000000-0000-4000-8000-%012d", nodeID)
				results = append(results, api.DocumentSearchResult{NodeID: nodeID, ContentVersionID: version, Path: fmt.Sprintf("/fixture-%d", nodeID), LexicalRank: i, Excerpt: strings.Repeat("界", 512), Evidence: []api.DocumentEvidenceReference{{Kind: "rendition_segment", BuildID: strings.Repeat("a", 64), SegmentID: fmt.Sprintf("segment-%d", nodeID)}}})
			}
			coverage := api.DocumentSearchCoverage{State: "complete", ScopedDocuments: 4, CompleteDocuments: 4}
			return answerTermsSearchReport(req, coverage, results...), nil
		},
	}}
	session := func(ctx context.Context, fn func(context.Context, answerClient) error) error { return fn(ctx, f) }
	out, _, err := answerQuestion(t.Context(), session, []byte(`{"question":"q","terms":["term"],"limit":3,"max_passage_bytes":2048}`))
	if err != nil || len(out.Passages) != 1 || out.Passages[0].NodeID != 1 || out.OmittedPassages != 2 || out.PassageBytes != 1536 || !slices.Contains(out.CoverageReasons, "retrieval_truncated") {
		t.Fatalf("out=%+v err=%v", out, err)
	}
}

func TestAnswerQuestionTermsReceiptHashesExecutedQueryWithoutEcho(t *testing.T) {
	f := &answerTermsClient{answerFakeClient: &answerFakeClient{searchReport: func(req api.DocumentSearchRequest) (api.DocumentSearchReport, error) {
		return answerTermsSearchReport(req, api.DocumentSearchCoverage{State: "complete", ScopedDocuments: 1, CompleteDocuments: 1}), nil
	}}}
	session := func(ctx context.Context, fn func(context.Context, answerClient) error) error { return fn(ctx, f) }
	out, _, err := answerQuestion(t.Context(), session, []byte(`{"question":"q","terms":[" classified "," token "]}`))
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256([]byte("classified   token"))
	if len(f.requests) != 2 || f.requests[1].Query != "classified   token" || !out.Receipt.TermsSearched || out.Receipt.TermsSHA256 != hex.EncodeToString(hash[:]) || strings.Contains(string(encoded), "classified") || strings.Contains(string(encoded), "token") {
		t.Fatalf("out=%+v requests=%+v", out, f.requests)
	}
}
