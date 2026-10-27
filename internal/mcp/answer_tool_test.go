package mcp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"testing"

	"go.kenn.io/docbank/internal/answer"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/processing"
)

func TestAnswerContentExcerpt(t *testing.T) {
	tests := []struct {
		name   string
		result api.DocumentSearchResult
		want   bool
	}{
		{"rendition content", api.DocumentSearchResult{Excerpt: "words", LexicalRank: 1, Evidence: []api.DocumentEvidenceReference{{Kind: "rendition_segment"}}}, true},
		{"blob content", api.DocumentSearchResult{Excerpt: "words", LexicalRank: 1, Evidence: []api.DocumentEvidenceReference{{Kind: "content_blob"}}}, true},
		{"name", api.DocumentSearchResult{Excerpt: "file", LexicalRank: 1, Evidence: []api.DocumentEvidenceReference{{Kind: "node_name"}}}, false},
		{"name and embedding", api.DocumentSearchResult{Excerpt: "file", LexicalRank: 1, Evidence: []api.DocumentEvidenceReference{{Kind: "node_name"}, {Kind: "embedding"}}}, false},
		{"semantic", api.DocumentSearchResult{Excerpt: "words", Evidence: []api.DocumentEvidenceReference{{Kind: "rendition_segment"}}}, false},
		{"empty", api.DocumentSearchResult{LexicalRank: 1, Evidence: []api.DocumentEvidenceReference{{Kind: "rendition_segment"}}}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := answerContentExcerpt(tt.result); got != tt.want {
				t.Fatalf("got %v want %v", got, tt.want)
			}
		})
	}
}

type answerFakeClient struct {
	documentClient

	results       []api.DocumentSearchResult
	report        api.DocumentSearchReport
	requests      []api.DocumentSearchRequest
	searchErr     error
	profiles      []api.ProcessingProfileSummary
	bindingErrors map[string]error
	documents     []api.DocumentSummary
	fenceVersions []string
	empty         bool
	validations   int
}

func (f *answerFakeClient) ListDocumentProcessingProfiles(context.Context) ([]api.ProcessingProfileSummary, error) {
	if f.profiles != nil {
		return f.profiles, nil
	}
	return []api.ProcessingProfileSummary{{Name: "default", Fingerprint: strings.Repeat("a", 64), QueryEmbeddingBindings: []string{"query"}, RerankingAvailable: true}}, nil
}
func (f *answerFakeClient) ResolveDocumentSourceFence(context.Context, api.DocumentSourceFenceResolveRequest) (api.DocumentSourceFenceResolution, error) {
	versions := f.fenceVersions
	if versions == nil {
		versions = []string{"00000000-0000-4000-8000-000000000002"}
	}
	if f.empty {
		versions = []string{}
	}
	fence := api.ResolvedDocumentSourceFence{VaultUID: "00000000-0000-4000-8000-000000000001", ContentVersionIDs: versions}
	fingerprint, err := processing.SourceFenceFingerprint(processing.SourceFence{VaultUID: fence.VaultUID, ContentVersionIDs: versions})
	return api.DocumentSourceFenceResolution{Fence: fence, FenceFingerprint: fingerprint, ObservedScopeCount: len(versions)}, err
}

func (f *answerFakeClient) ValidateDocumentSearch(context.Context, api.DocumentSearchValidationRequest) error {
	f.validations++
	return nil
}
func (f *answerFakeClient) SearchDocuments(_ context.Context, req api.DocumentSearchRequest) (api.DocumentSearchReport, error) {
	f.requests = append(f.requests, req)
	if err := f.bindingErrors[req.BindingID]; err != nil {
		return api.DocumentSearchReport{}, err
	}
	if f.searchErr != nil {
		return api.DocumentSearchReport{}, f.searchErr
	}
	report := f.report
	report.Results = f.results
	return report, nil
}
func (f *answerFakeClient) ResolveDocumentSummaries(context.Context, api.DocumentSummaryResolveRequest) ([]api.DocumentSummary, error) {
	return f.documents, nil
}
func TestAnswerQuestionEvidenceAndCoverage(t *testing.T) {
	f := &answerFakeClient{report: api.DocumentSearchReport{RequestedMode: "hybrid", ActualMode: "hybrid", Coverage: api.DocumentSearchCoverage{State: "complete", ScopedDocuments: 1, CompleteDocuments: 1}}}
	session := func(ctx context.Context, fn func(context.Context, documentClient) error) error { return fn(ctx, f) }
	out, _, err := answerQuestion(context.Background(), session, []byte(`{"question":"example"}`))
	if err != nil || out.Status != answer.NoMatch {
		t.Fatalf("out=%+v err=%v", out, err)
	}
	if len(f.requests) != 1 || !f.requests[0].ContentFirst {
		t.Fatal("content-first search not requested")
	}
	f.results = []api.DocumentSearchResult{{NodeID: 1, ContentVersionID: "00000000-0000-4000-8000-000000000002", Path: "/example", Rank: 1, LexicalRank: 1, Excerpt: "example", Evidence: []api.DocumentEvidenceReference{{Kind: "node_name"}, {Kind: "embedding"}}}}
	out, _, err = answerQuestion(context.Background(), session, []byte(`{"question":"example"}`))
	if err != nil || out.Status != answer.IncompleteCoverage || len(out.Leads) != 1 {
		t.Fatalf("out=%+v err=%v", out, err)
	}
	for _, kind := range []string{"rendition_segment", "content_blob"} {
		f.results[0].Evidence = []api.DocumentEvidenceReference{{Kind: kind}}
		out, _, err = answerQuestion(context.Background(), session, []byte(`{"question":"example"}`))
		if err != nil || out.Status != answer.Answered || len(out.Passages) != 1 || out.Passages[0].Text != "example" || len(out.Passages[0].EvidenceIDs) != 1 || out.Passages[0].EvidenceIDs[0] != fmt.Sprintf(`{"kind":%q}`, kind) {
			t.Fatalf("kind=%s out=%+v err=%v", kind, out, err)
		}
	}
}

func TestAnswerQuestionEnvelopeBound(t *testing.T) {
	f := &answerFakeClient{report: api.DocumentSearchReport{RequestedMode: "hybrid", ActualMode: "hybrid", Coverage: api.DocumentSearchCoverage{State: "complete", ScopedDocuments: 50, CompleteDocuments: 50}}}
	pathPrefix := "/" + strings.Repeat(strings.Repeat("p", 254)+"/", 63)
	for i := 1; i <= 50; i++ {
		version := fmt.Sprintf("00000000-0000-4000-8000-%012d", i+1)
		name := strings.Repeat("n", 251) + fmt.Sprintf("%04d", i)
		f.fenceVersions = append(f.fenceVersions, version)
		renditions := make([]api.DocumentRenditionIdentity, 64)
		for j := range renditions {
			renditions[j] = api.DocumentRenditionIdentity{ProfileFingerprint: fmt.Sprintf("%064x", j+1),
				AttachmentID: fmt.Sprintf("%064x", i*64+j+1), BuildID: fmt.Sprintf("%064x", 10000+i*64+j+1)}
		}
		f.documents = append(f.documents, api.DocumentSummary{NodeID: int64(i), ContentVersionID: version, Path: pathPrefix + name, Name: name, ActiveRenditions: renditions})
		f.results = append(f.results, api.DocumentSearchResult{NodeID: int64(i), ContentVersionID: version, Path: pathPrefix + name, Rank: i, LexicalRank: i, Score: 1 / float64(60+i), Excerpt: strings.Repeat("x", 512), Evidence: []api.DocumentEvidenceReference{{Kind: "rendition_segment", BuildID: strings.Repeat("a", 64), SegmentID: strings.Repeat("b", 64)}}})
	}
	session := func(ctx context.Context, fn func(context.Context, documentClient) error) error { return fn(ctx, f) }
	out, links, err := answerQuestion(context.Background(), session, []byte(`{"question":"example","limit":50,"max_passage_bytes":65536}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(links) == 0 {
		t.Fatal("expected retained resource links")
	}
	if out.OmittedLinks == 0 {
		t.Fatal("expected link omission")
	}
	if out.OmittedPassages == 0 {
		t.Fatal("expected envelope passage omission")
	}
	_, schema := answerQuestionSchemas()
	if _, err := boundedToolSuccess(mustResolveSchema(schema), out, links); err != nil {
		t.Fatal(err)
	}
}

func TestAnswerQuestionDoesNotHideSearchFailure(t *testing.T) {
	for _, tc := range []struct {
		name    string
		failure error
		rerank  bool
	}{
		{name: "generic", failure: errors.New("search failed")},
		{name: "processing failed", failure: api.NewError(500, "processing_failed", "generic failure")},
		{name: "reranking unavailable", failure: api.NewError(503, "reranking_unavailable", "private provider details"), rerank: true},
		{name: "reranking failed", failure: api.NewError(503, "reranking_failed", "private provider details"), rerank: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &answerFakeClient{searchErr: tc.failure, profiles: []api.ProcessingProfileSummary{
				{Name: "a", QueryEmbeddingBindings: []string{"first"}, RerankingAvailable: true},
				{Name: "z", QueryEmbeddingBindings: []string{"second"}, RerankingAvailable: true},
			}}
			session := func(ctx context.Context, fn func(context.Context, documentClient) error) error { return fn(ctx, f) }
			_, _, err := answerQuestion(t.Context(), session, []byte(fmt.Sprintf(`{"question":"example","rerank":%t}`, tc.rerank)))
			if !errors.Is(err, tc.failure) || len(f.requests) != 1 {
				t.Fatalf("got %v after %d calls", err, len(f.requests))
			}
		})
	}
}

func TestAnswerQuestionRetriesBindings(t *testing.T) {
	f := &answerFakeClient{profiles: []api.ProcessingProfileSummary{{Name: "z", QueryEmbeddingBindings: []string{"b"}}, {Name: "a", QueryEmbeddingBindings: []string{"c", "a"}}}, bindingErrors: map[string]error{"a": api.NewError(404, "not_found", "missing"), "c": api.NewError(409, "stale_index", "failed")}, report: api.DocumentSearchReport{RequestedMode: "hybrid", ActualMode: "hybrid", Coverage: api.DocumentSearchCoverage{State: "complete", ScopedDocuments: 1, CompleteDocuments: 1}}}
	session := func(ctx context.Context, fn func(context.Context, documentClient) error) error { return fn(ctx, f) }
	_, _, err := answerQuestion(context.Background(), session, []byte(`{"question":"example"}`))
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, r := range f.requests {
		got = append(got, r.Profile+":"+r.BindingID)
	}
	if !slices.Equal(got, []string{"a:a", "a:c", "z:b"}) {
		t.Fatalf("unexpected retry order: %v", got)
	}
}

func TestAnswerQuestionRelevanceOnlyForRerankedPrefix(t *testing.T) {
	f := &answerFakeClient{report: api.DocumentSearchReport{RequestedMode: "hybrid", ActualMode: "hybrid", Coverage: api.DocumentSearchCoverage{State: "complete", ScopedDocuments: 2, CompleteDocuments: 2}, Reranking: &api.DocumentSearchRerankingReceipt{Outcome: "applied", CandidateCount: 1}}}
	const providerScore = 2.7
	for i, score := range []float64{providerScore, 1.0 / 62} {
		i++
		version := fmt.Sprintf("00000000-0000-4000-8000-%012d", i+1)
		f.fenceVersions = append(f.fenceVersions, version)
		f.results = append(f.results, api.DocumentSearchResult{NodeID: int64(i), ContentVersionID: version, Path: fmt.Sprintf("/example-%d", i), Rank: i, LexicalRank: i, Excerpt: "example", Score: score, Evidence: []api.DocumentEvidenceReference{{Kind: "rendition_segment"}}})
	}
	session := func(ctx context.Context, fn func(context.Context, documentClient) error) error { return fn(ctx, f) }
	out, _, err := answerQuestion(context.Background(), session, []byte(`{"question":"example","rerank":true}`))
	if err != nil || len(out.Passages) != 2 || out.Passages[0].Relevance == nil || *out.Passages[0].Relevance != providerScore || out.Passages[1].Relevance != nil {
		t.Fatalf("out=%+v err=%v", out, err)
	}
}

func TestAnswerQuestionRetryExhaustionFallsBackLexically(t *testing.T) {
	f := &answerFakeClient{bindingErrors: map[string]error{"query": api.NewError(404, "not_found", "missing")}, report: api.DocumentSearchReport{RequestedMode: "lexical", ActualMode: "lexical", Coverage: api.DocumentSearchCoverage{State: "complete", ScopedDocuments: 1, CompleteDocuments: 1}}}
	session := func(ctx context.Context, fn func(context.Context, documentClient) error) error { return fn(ctx, f) }
	out, _, err := answerQuestion(context.Background(), session, []byte(`{"question":"example"}`))
	if err != nil || out.Status != answer.IncompleteCoverage || out.SemanticFailure != "not_found" || len(f.requests) != 2 || f.requests[1].Mode != "lexical" {
		t.Fatalf("out=%+v err=%v requests=%+v", out, err, f.requests)
	}
}

func TestAnswerQuestionRecordsExecutedQueryHash(t *testing.T) {
	f := &answerFakeClient{report: api.DocumentSearchReport{RequestedMode: "hybrid", ActualMode: "hybrid", Coverage: api.DocumentSearchCoverage{State: "complete", ScopedDocuments: 1, CompleteDocuments: 1}}}
	session := func(ctx context.Context, fn func(context.Context, documentClient) error) error { return fn(ctx, f) }
	out, _, err := answerQuestion(context.Background(), session, []byte(`{"question":" example "}`))
	if err != nil {
		t.Fatal(err)
	}
	raw, executed := sha256.Sum256([]byte(" example ")), sha256.Sum256([]byte("example"))
	if out.Receipt.QuestionSHA256 != hex.EncodeToString(raw[:]) || out.Receipt.ExecutedQuerySHA256 != hex.EncodeToString(executed[:]) {
		t.Fatalf("incorrect hashes: %+v", out.Receipt)
	}
}

func TestAnswerQuestionEmptyFenceValidatesWithoutSearch(t *testing.T) {
	for _, tc := range []struct{ binding, rerank bool }{{true, false}, {false, false}, {true, true}} {
		t.Run(fmt.Sprintf("binding=%t/rerank=%t", tc.binding, tc.rerank), func(t *testing.T) {
			f := &answerFakeClient{empty: true}
			if !tc.binding {
				f.profiles = []api.ProcessingProfileSummary{{Name: "local"}}
			}
			session := func(ctx context.Context, fn func(context.Context, documentClient) error) error { return fn(ctx, f) }
			out, _, err := answerQuestion(t.Context(), session, []byte(fmt.Sprintf(`{"question":"example","rerank":%t}`, tc.rerank)))
			if err != nil || f.validations != 1 || len(f.requests) != 0 || out.Receipt.SearchExecuted || out.Receipt.ExecutedQuerySHA256 != "" || out.Status != answer.NoMatch || !out.CoverageComplete || len(out.CoverageReasons) != 0 {
				t.Fatalf("out=%+v err=%v", out, err)
			}
			if tc.rerank && (out.Receipt.Reranking == nil || out.Receipt.Reranking.Outcome != "not_reported") {
				t.Fatalf("receipt=%+v", out.Receipt.Reranking)
			}
		})
	}
}

func TestAnswerQuestionSchemaRejectsUnknownCoverageReason(t *testing.T) {
	_, output := answerQuestionSchemas()
	reasons := schemaMap(t, output["properties"])["coverage_reasons"]
	for _, reason := range []string{"scope_too_large", "semantic_unavailable", "embeddings_incomplete", "passage_text_unavailable", "retrieval_truncated"} {
		assertSchemaAccepts(t, reasons, []any{reason})
	}
	assertSchemaRejects(t, reasons, []any{"invented_reason"})
}

func TestAnswerQuestionSchemaPreservesProviderScoreScale(t *testing.T) {
	_, output := answerQuestionSchemas()
	for _, name := range []string{"passages", "leads"} {
		t.Run(name, func(t *testing.T) {
			item := schemaMap(t, schemaMap(t, schemaMap(t, output["properties"])[name])["items"])
			relevance := schemaMap(t, item["properties"])["relevance"]
			for _, score := range []float64{-2, 0, 0.5, 1, 2} {
				assertSchemaAccepts(t, relevance, score)
			}
		})
	}
}

func TestAnswerProcessingErrorsStayScopedToAnswerTool(t *testing.T) {
	if _, mapped := domainToolError(api.NewError(409, "stale_index", "stale")); mapped {
		t.Fatal("stale index became a domain tool error")
	}
	for _, code := range []string{"processing_unavailable", "processing_profile_unavailable", "reranking_unavailable", "reranking_failed"} {
		t.Run(code, func(t *testing.T) {
			err := api.NewError(503, code, "private provider details")
			if _, mapped := domainToolError(err); mapped {
				t.Fatal("answer-specific mapping changed another tool's error contract")
			}
			result, callErr := readToolFailure(slog.Default(), "answer_question", err)
			if callErr != nil || result == nil || !result.IsError {
				t.Fatalf("result=%+v err=%v", result, callErr)
			}
			output, ok := result.StructuredContent.(toolErrorOutput)
			if !ok || output.Code != code || strings.Contains(output.Message, "private") {
				t.Fatalf("output=%+v", result.StructuredContent)
			}
		})
	}
}

func TestAnswerQuestionIncompleteAndTruncated(t *testing.T) {
	f := &answerFakeClient{report: api.DocumentSearchReport{RequestedMode: "hybrid", ActualMode: "hybrid", Coverage: api.DocumentSearchCoverage{State: "incomplete", ScopedDocuments: 1}}}
	session := func(ctx context.Context, fn func(context.Context, documentClient) error) error { return fn(ctx, f) }
	out, _, err := answerQuestion(context.Background(), session, []byte(`{"question":"example"}`))
	if err != nil || out.Status != answer.IncompleteCoverage || !slices.Contains(out.CoverageReasons, "embeddings_incomplete") {
		t.Fatalf("out=%+v err=%v", out, err)
	}
	f.report.Coverage = api.DocumentSearchCoverage{State: "complete", ScopedDocuments: 1, CompleteDocuments: 1}
	f.report.Truncated = true
	out, _, err = answerQuestion(context.Background(), session, []byte(`{"question":"example"}`))
	if err != nil || out.Status != answer.IncompleteCoverage || !slices.Contains(out.CoverageReasons, "retrieval_truncated") {
		t.Fatalf("out=%+v err=%v", out, err)
	}
}

func TestAnswerQuestionExplicitProfileSelection(t *testing.T) {
	primaryFailure := api.NewError(409, "stale_index", "primary binding unavailable")
	for _, tc := range []struct {
		name, arguments, binding, code string
		failPrimary                    bool
		wantPrimaryFailure             bool
	}{
		{name: "primary in name order", arguments: `{"question":"example","profile":"chosen"}`, binding: "a"},
		{name: "primary failure is returned", arguments: `{"question":"example","profile":"chosen"}`, binding: "a", failPrimary: true, wantPrimaryFailure: true},
		{name: "explicit binding", arguments: `{"question":"example","profile":"chosen","binding_id":"z"}`, binding: "z", failPrimary: true},
		{name: "missing profile", arguments: `{"question":"example","profile":"missing"}`, code: "processing_profile_unavailable"},
		{name: "missing binding", arguments: `{"question":"example","profile":"chosen","binding_id":"missing"}`, code: "processing_profile_unavailable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &answerFakeClient{
				profiles: []api.ProcessingProfileSummary{
					{Name: "other", Fingerprint: strings.Repeat("a", 64), QueryEmbeddingBindings: []string{"healthy"}},
					{Name: "chosen", Fingerprint: strings.Repeat("b", 64), QueryEmbeddingBindings: []string{"z", "a"}},
				},
				report: api.DocumentSearchReport{RequestedMode: "hybrid", ActualMode: "hybrid", Coverage: api.DocumentSearchCoverage{State: "complete", ScopedDocuments: 1, CompleteDocuments: 1}},
			}
			if tc.failPrimary {
				f.bindingErrors = map[string]error{"a": primaryFailure}
			}
			session := func(ctx context.Context, fn func(context.Context, documentClient) error) error { return fn(ctx, f) }
			out, _, err := answerQuestion(t.Context(), session, []byte(tc.arguments))
			if tc.code != "" {
				native, ok := errors.AsType[*api.Error](err)
				if !ok || native.Code != tc.code || len(f.requests) != 0 {
					t.Fatalf("err=%v requests=%+v", err, f.requests)
				}
				return
			}
			if tc.wantPrimaryFailure {
				if !errors.Is(err, primaryFailure) {
					t.Fatalf("expected selected binding failure, got %v", err)
				}
			} else if err != nil || out.Receipt.BindingID != tc.binding {
				t.Fatalf("receipt=%+v err=%v", out.Receipt, err)
			}
			if len(f.requests) != 1 || f.requests[0].Profile != "chosen" || f.requests[0].BindingID != tc.binding {
				t.Fatalf("explicit selection retried or escaped: %+v", f.requests)
			}
		})
	}
}

func TestAnswerQuestionAutomaticRerankingProfiles(t *testing.T) {
	for _, tc := range []struct {
		name           string
		bindings       []string
		bindingFailure error
		wantMode       string
		wantCalls      int
	}{
		{name: "hybrid", bindings: []string{"supported"}, wantMode: "hybrid", wantCalls: 1},
		{name: "lexical without bindings", wantMode: "lexical", wantCalls: 1},
		{name: "lexical after retry exhaustion", bindings: []string{"supported"}, bindingFailure: api.NewError(404, "not_found", "missing binding"), wantMode: "lexical", wantCalls: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &answerFakeClient{
				profiles: []api.ProcessingProfileSummary{
					{Name: "a", QueryEmbeddingBindings: []string{"unsupported"}},
					{Name: "z", QueryEmbeddingBindings: tc.bindings, RerankingAvailable: true},
				},
				bindingErrors: map[string]error{"unsupported": api.NewError(503, "reranking_unavailable", "no reranker"), "supported": tc.bindingFailure},
				report:        api.DocumentSearchReport{RequestedMode: tc.wantMode, ActualMode: tc.wantMode, Coverage: api.DocumentSearchCoverage{State: "complete", ScopedDocuments: 1, CompleteDocuments: 1}, Reranking: &api.DocumentSearchRerankingReceipt{Outcome: "skipped"}},
			}
			session := func(ctx context.Context, fn func(context.Context, documentClient) error) error { return fn(ctx, f) }
			out, _, err := answerQuestion(t.Context(), session, []byte(`{"question":"example","rerank":true}`))
			if err != nil || out.Receipt.Profile == nil || out.Receipt.Profile.Name != "z" || out.Receipt.ActualMode != tc.wantMode {
				t.Fatalf("receipt=%+v err=%v requests=%+v", out.Receipt, err, f.requests)
			}
			if len(f.requests) != tc.wantCalls || f.requests[len(f.requests)-1].Mode != tc.wantMode {
				t.Fatalf("unexpected search attempts: %+v", f.requests)
			}
			for _, req := range f.requests {
				if req.Profile != "z" || !req.Rerank {
					t.Fatalf("selected incompatible profile: %+v", req)
				}
			}
		})
	}
}
