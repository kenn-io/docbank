package mcp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
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
	searchReport  func(api.DocumentSearchRequest) (api.DocumentSearchReport, error)
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
	if f.searchReport != nil {
		var err error
		report, err = f.searchReport(req)
		if err != nil {
			return api.DocumentSearchReport{}, err
		}
	}
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
	f.results = []api.DocumentSearchResult{{VaultUID: "00000000-0000-4000-8000-000000000001", NodeID: 1, ContentVersionID: "00000000-0000-4000-8000-000000000002", Path: "/example", Rank: 1, LexicalRank: 1, SemanticRank: 1, Excerpt: "example", Evidence: []api.DocumentEvidenceReference{{Kind: "node_name"}, embedding}}}
	out, _, err := answerQuestion(context.Background(), session, []byte(`{"question":"example"}`))
	if err != nil || out.Status != answer.IncompleteCoverage || len(out.Leads) != 1 {
		t.Fatalf("out=%+v err=%v", out, err)
	}
	f.results[0].Evidence = []api.DocumentEvidenceReference{
		{Kind: "rendition_segment", BuildID: strings.Repeat("a", 64), SegmentID: "segment-a"},
		embedding,
	}
	out, _, err = answerQuestion(context.Background(), session, []byte(`{"question":"example"}`))
	if err != nil || out.Status != answer.Answered || len(out.Passages) != 1 || out.Passages[0].Text != "example" || len(out.Passages[0].EvidenceIDs) != 2 || out.Passages[0].EvidenceIDs[0] != `{"kind":"rendition_segment","build_id":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","segment_id":"segment-a"}` {
		t.Fatalf("out=%+v err=%v", out, err)
	}
	if !slices.Equal(out.Receipt.VectorSpaceIDs, []string{strings.Repeat("c", 64)}) {
		t.Fatalf("unexpected vector spaces: %v", out.Receipt.VectorSpaceIDs)
	}
}

func TestAnswerQuestionEnvelopeBound(t *testing.T) {
	f := &answerFakeClient{report: api.DocumentSearchReport{RequestedMode: "hybrid", ActualMode: "hybrid", Coverage: api.DocumentSearchCoverage{State: "complete", ScopedDocuments: 50, CompleteDocuments: 50}, Reranking: &api.DocumentSearchRerankingReceipt{Outcome: "applied", CandidateCount: 50}}}
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
		f.results = append(f.results, api.DocumentSearchResult{VaultUID: "00000000-0000-4000-8000-000000000001", NodeID: int64(i), ContentVersionID: version, Path: pathPrefix + name, Rank: i, LexicalRank: i, Score: float64(51-i) / 50, Excerpt: strings.Repeat("x", 512), Evidence: []api.DocumentEvidenceReference{
			{Kind: "rendition_segment", BuildID: strings.Repeat("a", 64), SegmentID: "segment-a"},
		}})
	}
	vectorSpaceID := strings.Repeat("c", 64)
	last := &f.results[len(f.results)-1]
	last.SemanticRank = 1
	last.Evidence = append(last.Evidence, api.DocumentEvidenceReference{Kind: "embedding", VectorSpaceID: vectorSpaceID, EmbeddingSetID: strings.Repeat("d", 64), InputGenerationID: strings.Repeat("e", 64), InputID: "input-a", InputKind: "rendition_chunk", BuildID: strings.Repeat("a", 64), SourceManifestChecksum: strings.Repeat("f", 64)})
	session := func(ctx context.Context, fn func(context.Context, answerClient) error) error { return fn(ctx, f) }
	out, links, err := answerQuestion(context.Background(), session, []byte(`{"question":"example","limit":50,"max_passage_bytes":65536,"rerank":true}`))
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
	if slices.Contains(out.Receipt.VectorSpaceIDs, vectorSpaceID) {
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
	for _, tc := range []struct {
		binding, rerank bool
		terms           []string
	}{{true, false, nil}, {false, false, nil}, {true, true, nil}, {true, false, []string{"first", "second"}}} {
		t.Run(fmt.Sprintf("binding=%t/rerank=%t/terms=%d", tc.binding, tc.rerank, len(tc.terms)), func(t *testing.T) {
			f := &answerFakeClient{empty: true}
			if !tc.binding {
				f.profiles = []api.ProcessingProfileSummary{{Name: "local", Fingerprint: strings.Repeat("a", 64)}}
			}
			session := func(ctx context.Context, fn func(context.Context, answerClient) error) error { return fn(ctx, f) }
			args := map[string]any{"question": "example", "rerank": tc.rerank}
			if len(tc.terms) > 0 {
				args["terms"] = tc.terms
			}
			raw, err := json.Marshal(args)
			if err != nil {
				t.Fatal(err)
			}
			out, links, err := answerQuestion(t.Context(), session, raw)
			if err != nil || f.validations != 1+len(tc.terms) || len(f.requests) != 0 || out.Receipt.SearchExecuted || out.Receipt.ExecutedQuerySHA256 != "" || out.Receipt.TermsSHA256 != "" || out.Status != answer.NoMatch || !out.CoverageComplete || len(out.CoverageReasons) != 0 {
				t.Fatalf("out=%+v err=%v", out, err)
			}
			if tc.rerank && (out.Receipt.Reranking == nil || out.Receipt.Reranking.Outcome != "skipped") {
				t.Fatalf("receipt=%+v", out.Receipt.Reranking)
			}
			if !tc.rerank && (out.Receipt.Reranking == nil || out.Receipt.Reranking.Outcome != "not_requested") {
				t.Fatalf("receipt=%+v", out.Receipt.Reranking)
			}
			_, output := answerQuestionSchemas()
			if _, err := boundedToolSuccess(mustResolveSchema(output), out, links); err != nil {
				t.Fatal(err)
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
	for code, status := range map[string]int{"processing_unavailable": 503, "processing_profile_unavailable": 422, "stale_index": 409} {
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
			wantMessage := "Document reranking failed; check the daemon log before retrying."
			if code == "stale_index" {
				wantMessage = "The selected index needs rebuilding."
			}
			if output.Message != wantMessage {
				t.Fatalf("guidance=%q, want %q", output.Message, wantMessage)
			}
		})
	}
}

// Reranker identity belongs to applied scores, not merely a configured provider.
func TestAnswerQuestionRerankerIdentity(t *testing.T) {
	for _, outcome := range []string{"applied", "skipped", "degraded", "not_requested", "not_reported"} {
		t.Run(outcome, func(t *testing.T) {
			checkAnswerRerankerIdentity(t, "synthetic-reranker/模型:v2", outcome)
		})
	}
}

func FuzzAnswerQuestionRerankerIdentity(f *testing.F) {
	f.Add("synthetic-reranker", uint8(0))
	f.Add("模型/ reranker:v2\n", uint8(0))
	f.Add(strings.Repeat("r", 4097), uint8(0))
	f.Add("", uint8(0))
	f.Fuzz(func(t *testing.T, profileID string, choice uint8) {
		// Bound materialized payloads; retain generated cases as valid wire text.
		if len(profileID) > 16384 {
			profileID = profileID[:16384]
		}
		profileID = strings.ToValidUTF8(profileID, "�")
		outcomes := []string{"applied", "skipped", "degraded", "not_requested", "not_reported"}
		checkAnswerRerankerIdentity(t, profileID, outcomes[int(choice)%len(outcomes)])
	})
}

func checkAnswerRerankerIdentity(t *testing.T, profileID, outcome string) {
	t.Helper()
	f := rerankerIdentityClient(nil)
	// Decode the daemon wire shape before sending the receipt through answer_question.
	if outcome != "not_requested" && outcome != "not_reported" {
		data, err := json.Marshal(map[string]any{"outcome": outcome, "candidate_count": 1, "profile_id": profileID})
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(data, &f.answerFakeClient.report.Reranking); err != nil {
			t.Fatal(err)
		}
	}
	session := func(ctx context.Context, fn func(context.Context, answerClient) error) error { return fn(ctx, f) }
	args := []byte(`{"question":"example","rerank":true}`)
	if outcome == "not_requested" {
		args = []byte(`{"question":"example"}`)
	}
	out, _, err := answerQuestion(context.Background(), session, args)
	if err != nil {
		t.Fatal(err)
	}
	output := structuredMap(t, out)
	reranking := schemaMap(t, schemaMap(t, output["receipt"])["reranking"])
	id, present := reranking["reranker_id"]
	if outcome == "applied" && profileID != "" {
		if !present || id != profileID {
			t.Fatalf("reranker identity = %v (present %v), want %q", id, present, profileID)
		}
	} else if present {
		t.Fatalf("unexpected reranker identity for %s: %v", outcome, id)
	}
	_, outputSchema := answerQuestionSchemas()
	receiptSchema := schemaMap(t, schemaMap(t, outputSchema["properties"])["receipt"])
	rerankingSchema := schemaMap(t, receiptSchema["properties"])["reranking"]
	assertSchemaAccepts(t, rerankingSchema, reranking)
}

func TestAnswerQuestionOversizedRerankerReceipt(t *testing.T) {
	f := rerankerIdentityClient(&api.DocumentSearchRerankingReceipt{
		Outcome: "applied", ProfileID: strings.Repeat("r", maxToolResponseBytes), CandidateCount: 1,
	})
	session := func(ctx context.Context, fn func(context.Context, answerClient) error) error { return fn(ctx, f) }
	_, _, err := answerQuestion(context.Background(), session, []byte(`{"question":"example","rerank":true}`))
	if !errors.Is(err, errToolResultTooLarge) {
		t.Fatalf("oversized identity error = %v", err)
	}
}

func rerankerIdentityClient(receipt *api.DocumentSearchRerankingReceipt) *answerTermsClient {
	f := &answerTermsClient{answerFakeClient: &answerFakeClient{
		report: api.DocumentSearchReport{
			Reranking: receipt,
			Coverage:  api.DocumentSearchCoverage{State: "complete", ScopedDocuments: 1, CompleteDocuments: 1},
		},
		results: []api.DocumentSearchResult{{
			VaultUID: "00000000-0000-4000-8000-000000000001", NodeID: 1,
			ContentVersionID: "00000000-0000-4000-8000-000000000002", Path: "/example", Rank: 1,
		}},
	}}
	f.search = func(req api.DocumentSearchRequest) (api.DocumentSearchReport, error) {
		report := f.answerFakeClient.report
		report.RequestedMode = req.Mode
		report.ActualMode = req.Mode
		report.Results = f.answerFakeClient.results
		return report, nil
	}
	return f
}
