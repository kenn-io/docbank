package mcp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"testing"

	"go.kenn.io/docbank/internal/answer"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/daemonconn"
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
	profiles      []api.ProcessingProfileSummary
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
func (f *answerFakeClient) SearchDocuments(ctx context.Context, req api.DocumentSearchRequest) (api.DocumentSearchReport, error) {
	f.requests = append(f.requests, req)
	report := f.report
	report.Results = f.results
	encoded, err := json.Marshal(report)
	if err != nil {
		return api.DocumentSearchReport{}, err
	}
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/search" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(encoded)
	}))
	defer daemon.Close()
	return daemonconn.New(daemon.URL, "key").SearchDocuments(ctx, req)
}
func (f *answerFakeClient) ResolveDocumentSummaries(context.Context, api.DocumentSummaryResolveRequest) ([]api.DocumentSummary, error) {
	return f.documents, nil
}
func TestSelectAnswerSearchPreservesProfiles(t *testing.T) {
	profiles := []api.ProcessingProfileSummary{
		{Name: "a", Fingerprint: strings.Repeat("a", 64), QueryEmbeddingBindings: []string{"c", "a"}},
		{Name: "z", Fingerprint: strings.Repeat("b", 64), QueryEmbeddingBindings: []string{"b"}, RerankingAvailable: true},
	}
	original := slices.Clone(profiles)
	_, _, err := selectAnswerSearch(profiles, answerInput{Rerank: true})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(original, profiles) {
		t.Fatal("profile selection changed caller profiles")
	}
}

func TestAnswerQuestionEvidenceAndCoverage(t *testing.T) {
	f := &answerFakeClient{report: api.DocumentSearchReport{RequestedMode: "hybrid", ActualMode: "hybrid", Coverage: api.DocumentSearchCoverage{State: "complete", ScopedDocuments: 1, CompleteDocuments: 1}}}
	session := func(ctx context.Context, fn func(context.Context, answerClient) error) error { return fn(ctx, f) }
	for _, tc := range []struct {
		name, question, reason string
		incomplete, truncated  bool
	}{
		{name: "executed query hash", question: " example "},
		{name: "incomplete embeddings", question: "example", incomplete: true, reason: "embeddings_incomplete"},
		{name: "truncated retrieval", question: "example", truncated: true, reason: "retrieval_truncated"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f.requests = nil
			f.report.Coverage = api.DocumentSearchCoverage{State: "complete", ScopedDocuments: 1, CompleteDocuments: 1}
			if tc.incomplete {
				f.report.Coverage = api.DocumentSearchCoverage{State: "incomplete", ScopedDocuments: 1}
			}
			f.report.Truncated = tc.truncated
			out, _, err := answerQuestion(t.Context(), session, []byte(fmt.Sprintf(`{"question":%q}`, tc.question)))
			if err != nil || (tc.reason == "" && out.Status != answer.NoMatch) || (tc.reason != "" && !slices.Contains(out.CoverageReasons, tc.reason)) {
				t.Fatalf("out=%+v err=%v", out, err)
			}
			if len(f.requests) != 1 || !f.requests[0].ContentFirst {
				t.Fatal("content-first search not requested")
			}
			if tc.question == " example " {
				raw, executed := sha256.Sum256([]byte(" example ")), sha256.Sum256([]byte("example"))
				if out.Receipt.QuestionSHA256 != hex.EncodeToString(raw[:]) || out.Receipt.ExecutedQuerySHA256 != hex.EncodeToString(executed[:]) {
					t.Fatalf("incorrect hashes: %+v", out.Receipt)
				}
			}
		})
	}
	f.report.Truncated = false
	embedding := api.DocumentEvidenceReference{Kind: "embedding", VectorSpaceID: strings.Repeat("c", 64), EmbeddingSetID: strings.Repeat("d", 64), InputGenerationID: strings.Repeat("e", 64), InputID: "input-a", InputKind: "rendition_chunk", BuildID: strings.Repeat("a", 64), SourceManifestChecksum: strings.Repeat("f", 64)}
	f.results = []api.DocumentSearchResult{{VaultUID: "00000000-0000-4000-8000-000000000001", NodeID: 1, ContentVersionID: "00000000-0000-4000-8000-000000000002", Path: "/example", Rank: 1, LexicalRank: 1, Excerpt: "example", Evidence: []api.DocumentEvidenceReference{{Kind: "node_name"}, embedding}}}
	out, _, err := answerQuestion(context.Background(), session, []byte(`{"question":"example"}`))
	if err != nil || out.Status != answer.IncompleteCoverage || len(out.Leads) != 1 {
		t.Fatalf("out=%+v err=%v", out, err)
	}
	f.results[0].Evidence = []api.DocumentEvidenceReference{
		{Kind: "rendition_segment", BuildID: strings.Repeat("a", 64), SegmentID: "segment-a"},
		embedding,
		embedding,
	}
	f.results[0].Evidence[2].InputID = "input-b"
	out, _, err = answerQuestion(context.Background(), session, []byte(`{"question":"example"}`))
	if err != nil || out.Status != answer.Answered || len(out.Passages) != 1 || out.Passages[0].Text != "example" || len(out.Passages[0].EvidenceIDs) != 3 || out.Passages[0].EvidenceIDs[0] != `{"kind":"rendition_segment","build_id":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","segment_id":"segment-a"}` || out.Passages[0].EvidenceIDs[1] == out.Passages[0].EvidenceIDs[2] {
		t.Fatalf("out=%+v err=%v", out, err)
	}
	if !slices.Equal(out.Receipt.VectorSpaceIDs, []string{strings.Repeat("c", 64)}) {
		t.Fatalf("unexpected vector spaces: %v", out.Receipt.VectorSpaceIDs)
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
		f.results = append(f.results, api.DocumentSearchResult{VaultUID: "00000000-0000-4000-8000-000000000001", NodeID: int64(i), ContentVersionID: version, Path: pathPrefix + name, Rank: i, LexicalRank: i, Score: 1 / float64(60+i), Excerpt: strings.Repeat("x", 512), Evidence: []api.DocumentEvidenceReference{
			{Kind: "rendition_segment", BuildID: strings.Repeat("a", 64), SegmentID: "segment-a"},
			{Kind: "embedding", VectorSpaceID: fmt.Sprintf("%064x", i), EmbeddingSetID: strings.Repeat("d", 64), InputGenerationID: strings.Repeat("e", 64), InputID: "input-a", InputKind: "rendition_chunk", BuildID: strings.Repeat("a", 64), SourceManifestChecksum: strings.Repeat("f", 64)},
		}})
	}
	session := func(ctx context.Context, fn func(context.Context, answerClient) error) error { return fn(ctx, f) }
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
	if len(out.Receipt.VectorSpaceIDs) != len(out.Passages) || slices.Contains(out.Receipt.VectorSpaceIDs, fmt.Sprintf("%064x", 50)) {
		t.Fatalf("vector spaces include omitted evidence: %v", out.Receipt.VectorSpaceIDs)
	}
	_, schema := answerQuestionSchemas()
	if _, err := boundedToolSuccess(mustResolveSchema(schema), out, links); err != nil {
		t.Fatal(err)
	}
}

func TestAnswerQuestionRelevanceOnlyForRerankedPrefix(t *testing.T) {
	f := &answerFakeClient{report: api.DocumentSearchReport{RequestedMode: "hybrid", ActualMode: "hybrid", Coverage: api.DocumentSearchCoverage{State: "complete", ScopedDocuments: 2, CompleteDocuments: 2}, Reranking: &api.DocumentSearchRerankingReceipt{Outcome: "applied", CandidateCount: 1}}}
	const providerScore = 2.7
	for i, score := range []float64{providerScore, 1.0 / 62} {
		i++
		version := fmt.Sprintf("00000000-0000-4000-8000-%012d", i+1)
		f.fenceVersions = append(f.fenceVersions, version)
		f.results = append(f.results, api.DocumentSearchResult{VaultUID: "00000000-0000-4000-8000-000000000001", NodeID: int64(i), ContentVersionID: version, Path: fmt.Sprintf("/example-%d", i), Rank: i, LexicalRank: i, Excerpt: "example", Score: score, Evidence: []api.DocumentEvidenceReference{{Kind: "rendition_segment", BuildID: strings.Repeat("a", 64), SegmentID: "segment-a"}}})
	}
	session := func(ctx context.Context, fn func(context.Context, answerClient) error) error { return fn(ctx, f) }
	out, _, err := answerQuestion(context.Background(), session, []byte(`{"question":"example","rerank":true}`))
	if err != nil || len(out.Passages) != 2 || out.Passages[0].Relevance == nil || *out.Passages[0].Relevance != providerScore || out.Passages[1].Relevance != nil {
		t.Fatalf("out=%+v err=%v", out, err)
	}
}

func TestAnswerQuestionEmptyFenceValidatesWithoutSearch(t *testing.T) {
	for _, tc := range []struct{ binding, rerank bool }{{true, false}, {false, false}, {true, true}} {
		t.Run(fmt.Sprintf("binding=%t/rerank=%t", tc.binding, tc.rerank), func(t *testing.T) {
			f := &answerFakeClient{empty: true}
			if !tc.binding {
				f.profiles = []api.ProcessingProfileSummary{{Name: "local"}}
			}
			session := func(ctx context.Context, fn func(context.Context, answerClient) error) error { return fn(ctx, f) }
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
	for code, status := range map[string]int{"processing_unavailable": 503, "processing_profile_unavailable": 422, "reranking_unavailable": 422, "reranking_failed": 502, "stale_index": 409} {
		if _, mapped := domainToolError(api.NewError(status, code, "private provider details")); mapped {
			t.Fatalf("answer-specific mapping for %s changed another tool's error contract", code)
		}
	}
	for code, status := range map[string]int{"reranking_failed": 502, "stale_index": 409} {
		t.Run(code, func(t *testing.T) {
			err := api.NewError(status, code, "private provider details")
			result, callErr := readToolFailure(slog.Default(), "answer_question", err)
			if callErr != nil || result == nil || !result.IsError {
				t.Fatalf("result=%+v err=%v", result, callErr)
			}
			output, ok := result.StructuredContent.(toolErrorOutput)
			if !ok || output.Code != code || strings.Contains(output.Message, "private") {
				t.Fatalf("output=%+v", result.StructuredContent)
			}
			wantMessage := "The requested reranking could not be completed."
			if code == "stale_index" {
				wantMessage = "The selected index needs rebuilding."
			}
			if output.Message != wantMessage {
				t.Fatalf("guidance=%q, want %q", output.Message, wantMessage)
			}
		})
	}
}
