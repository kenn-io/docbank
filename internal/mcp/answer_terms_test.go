package mcp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/answer"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/store"
)

// Search responses use answerFakeClient's real daemon decoder; summaries use
// the fixture daemon and its catalog validation.
type answerTermsClient struct {
	*answerFakeClient
	summaries documentClient
}

func (f *answerTermsClient) ResolveDocumentSummaries(ctx context.Context, req api.DocumentSummaryResolveRequest) ([]api.DocumentSummary, error) {
	return f.summaries.ResolveDocumentSummaries(ctx, req)
}

func answerTermsSearchReport(req api.DocumentSearchRequest, results ...api.DocumentSearchResult) api.DocumentSearchReport {
	report := api.DocumentSearchReport{
		RequestedMode: req.Mode, ActualMode: req.Mode,
		Coverage: api.DocumentSearchCoverage{State: "complete", ScopedDocuments: len(req.Fence.ContentVersionIDs), CompleteDocuments: len(req.Fence.ContentVersionIDs)},
		Results:  slices.Clone(results),
	}
	for i := range report.Results {
		r := &report.Results[i]
		r.VaultUID, r.Rank = req.Fence.VaultUID, i+1
		r.LexicalRank = i + 1
		for _, evidence := range r.Evidence {
			if evidence.Kind == "embedding" {
				r.LexicalRank, r.SemanticRank = 0, i+1
			}
		}
	}
	if req.Rerank {
		report.Reranking = &api.DocumentSearchRerankingReceipt{Outcome: "applied", CandidateCount: len(results)}
		if len(results) == 0 {
			report.Reranking.Outcome = "skipped"
		}
	}
	return report
}

func TestAnswerQuestionTermsRecoverLexicalPassage(t *testing.T) {
	fixture := newNativeExportFixture(t)
	member := fixture.addFile(t, "lease.txt", "Lease renewal is 1 March.")
	require.NoError(t, fixture.catalog.RecordExtraction(t.Context(), store.ExtractionResult{
		BlobHash: member.SHA256, Extractor: "plain-text", ExtractorVersion: 1, Status: store.ExtractionOK, Text: "Lease renewal is 1 March.",
	}))
	f := &answerTermsClient{answerFakeClient: &answerFakeClient{fenceVersions: []string{member.VersionID}}, summaries: daemonDocumentClient{fixture.connection}}
	embedding := api.DocumentEvidenceReference{Kind: "embedding", VectorSpaceID: strings.Repeat("c", 64), EmbeddingSetID: strings.Repeat("d", 64), InputGenerationID: strings.Repeat("e", 64), InputID: "input-a", InputKind: "rendition_chunk", BuildID: strings.Repeat("a", 64), SourceManifestChecksum: strings.Repeat("f", 64)}
	f.searchReport = func(req api.DocumentSearchRequest) (api.DocumentSearchReport, error) {
		if req.Mode == "hybrid" {
			return answerTermsSearchReport(req, api.DocumentSearchResult{
				NodeID: member.NodeID, ContentVersionID: member.VersionID, Path: "/lease.txt", Score: 0.7, Evidence: []api.DocumentEvidenceReference{embedding},
			}), nil
		}
		hits, _, err := fixture.catalog.SearchExplainedLexicalCandidates(t.Context(), req.Query, req.Limit, store.SearchOptions{ContentVersionIDs: req.Fence.ContentVersionIDs}, req.ContentFirst)
		if err != nil {
			return api.DocumentSearchReport{}, err
		}
		var results []api.DocumentSearchResult
		for _, hit := range hits {
			results = append(results, api.DocumentSearchResult{NodeID: hit.Node.ID, ContentVersionID: hit.Node.CurrentVersionID, Path: hit.Path, Excerpt: hit.Excerpt, Evidence: []api.DocumentEvidenceReference{{Kind: hit.EvidenceKind}}})
		}
		return answerTermsSearchReport(req, results...), nil
	}
	session := func(ctx context.Context, fn func(context.Context, answerClient) error) error { return fn(ctx, f) }
	without, _, err := answerQuestion(t.Context(), session, []byte(`{"question":"when must I renew"}`))
	require.NoError(t, err)
	require.Empty(t, without.Passages)
	require.Len(t, without.Leads, 1)
	with, _, err := answerQuestion(t.Context(), session, []byte(`{"question":"when must I renew","terms":["lease","renewal"],"rerank":true}`))
	require.NoError(t, err)
	require.Equal(t, answer.Answered, with.Status)
	require.Len(t, with.Passages, 1, "both terms find the same passage")
	require.Equal(t, member.VersionID, with.Passages[0].ContentVersionID)
	require.Nil(t, with.Passages[0].Relevance)
	require.Equal(t, 0.7, *with.Leads[0].Relevance)
	require.Len(t, f.requests, 4)
	for i, term := range []string{"lease", "renewal"} {
		req := f.requests[i+2]
		require.Equal(t, term, req.Query)
		require.Equal(t, "lexical", req.Mode)
		require.False(t, req.Rerank)
		require.Empty(t, req.BindingID)
		require.True(t, req.ContentFirst)
		require.Equal(t, f.requests[1].Profile, req.Profile)
		require.Equal(t, f.requests[1].Fence, req.Fence)
	}
	hash := sha256.Sum256([]byte("lease renewal"))
	require.True(t, with.Receipt.TermsSearched)
	require.Equal(t, hex.EncodeToString(hash[:]), with.Receipt.TermsSHA256)
	encoded, err := json.Marshal(with)
	require.NoError(t, err)
	var normalized any
	require.NoError(t, json.Unmarshal(encoded, &normalized))
	_, output := answerQuestionSchemas()
	assertSchemaAccepts(t, output, normalized)
}

func TestAnswerQuestionTermsMatchIndependently(t *testing.T) {
	f := newNativeExportFixture(t)
	member := f.addFile(t, "agreement.txt", "Lease renewal is 1 March.")
	require.NoError(t, f.catalog.RecordExtraction(t.Context(), store.ExtractionResult{
		BlobHash: member.SHA256, Extractor: "plain-text", ExtractorVersion: 1, Status: store.ExtractionOK, Text: "Lease renewal is 1 March.",
	}))
	server, _, _ := newProcessingDocumentServer(t, f)
	for _, terms := range [][]string{{"lease"}, {"lease", "nonexistentword"}, {"lease nonexistentword"}} {
		out := exportCall(t, server, "answer_question", map[string]any{"question": "when must I renew", "terms": terms})
		passages := out["passages"].([]any)
		if len(terms) == 1 && terms[0] == "lease nonexistentword" {
			require.Empty(t, passages)
		} else {
			require.Len(t, passages, 1)
			require.Equal(t, member.VersionID, schemaMap(t, passages[0])["content_version_id"])
		}
	}
}

func TestAnswerQuestionTermsMerge(t *testing.T) {
	fixture := newNativeExportFixture(t)
	var results []api.DocumentSearchResult
	var versions []string
	for _, name := range []string{"a", "b", "c", "d", "e"} {
		member := fixture.addFile(t, name+".txt", name)
		versions = append(versions, member.VersionID)
		results = append(results, api.DocumentSearchResult{NodeID: member.NodeID, ContentVersionID: member.VersionID, Path: "/" + name + ".txt", Excerpt: name, Score: 0.7, Evidence: []api.DocumentEvidenceReference{{Kind: "content_blob"}}})
	}
	for _, tc := range []struct {
		name                    string
		question, first, second []int
		limit                   int
		truncated               bool
		want                    []string
	}{
		{"round robin", []int{0, 1}, []int{2, 3}, []int{4, 2}, 5, false, []string{"a", "c", "b", "e", "d"}},
		{"cross-search duplicates", []int{0, 1}, []int{1, 2}, []int{2, 3}, 5, false, []string{"a", "b", "c", "d"}},
		{"combined limit", []int{0, 1}, []int{2, 3}, []int{4, 2}, 3, false, []string{"a", "c", "b"}},
		{"term truncated", nil, nil, []int{2}, 1, true, []string{"c"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &answerTermsClient{answerFakeClient: &answerFakeClient{fenceVersions: versions}, summaries: daemonDocumentClient{fixture.connection}}
			f.searchReport = func(req api.DocumentSearchRequest) (api.DocumentSearchReport, error) {
				ids := tc.question
				if req.Query == "first" {
					ids = tc.first
				}
				if req.Query == "second" {
					ids = tc.second
				}
				var selected []api.DocumentSearchResult
				for _, id := range ids {
					selected = append(selected, results[id])
				}
				report := answerTermsSearchReport(req, selected...)
				report.Truncated = tc.truncated && req.Query == "second"
				return report, nil
			}
			session := func(ctx context.Context, fn func(context.Context, answerClient) error) error { return fn(ctx, f) }
			raw, err := json.Marshal(map[string]any{"question": "question", "terms": []string{"first", "second"}, "rerank": true, "limit": tc.limit})
			require.NoError(t, err)
			out, _, err := answerQuestion(t.Context(), session, raw)
			require.NoError(t, err)
			var texts []string
			for i, passage := range out.Passages {
				texts = append(texts, passage.Text)
				require.Equal(t, i+1, passage.Rank)
				if passage.Text == "a" || passage.Text == "b" {
					require.Equal(t, 0.7, *passage.Relevance)
				} else {
					require.Nil(t, passage.Relevance)
				}
			}
			require.Equal(t, tc.want, texts)
			require.Equal(t, tc.truncated || tc.name == "combined limit", slices.Contains(out.CoverageReasons, "retrieval_truncated"))
		})
	}
}

func TestAnswerQuestionTermsFailureIsReturned(t *testing.T) {
	failure := errors.New("terms search failed")
	f := &answerFakeClient{searchReport: func(req api.DocumentSearchRequest) (api.DocumentSearchReport, error) {
		if req.Mode == "lexical" {
			return api.DocumentSearchReport{}, failure
		}
		return answerTermsSearchReport(req), nil
	}}
	session := func(ctx context.Context, fn func(context.Context, answerClient) error) error { return fn(ctx, f) }
	_, _, err := answerQuestion(t.Context(), session, []byte(`{"question":"question","terms":["term"]}`))
	require.ErrorIs(t, err, failure)
}

func TestAnswerQuestionTermsSchema(t *testing.T) {
	input, _ := answerQuestionSchemas()
	for _, terms := range []any{[]any{"lease"}, []any{strings.Repeat("界", 64)}, []any{"a", "b", "c", "d", "e", "f", "g", "h"}} {
		assertSchemaAccepts(t, input, map[string]any{"question": "q", "terms": terms})
	}
	for _, terms := range []any{nil, []any{}, []any{""}, []any{strings.Repeat("界", 65)}, []any{"a", "b", "c", "d", "e", "f", "g", "h", "i"}} {
		assertSchemaRejects(t, input, map[string]any{"question": "q", "terms": terms})
	}
	fixture := newNativeExportFixture(t)
	server := newServerWithOptionsAndDaemon(testImplementation(), ServerOptions{}, fixture.lease)
	for _, terms := range [][]string{{" "}, {"lease", "\t\n"}} {
		raw := exchangeRaw(t, server, requestFor("tools/call", map[string]any{"name": "answer_question", "arguments": map[string]any{"question": "q", "terms": terms}}))
		require.EqualValues(t, jsonrpc.CodeInvalidParams, decodeWireError(t, raw).Code)
	}
}

func TestAnswerQuestionTermsEmptyFence(t *testing.T) {
	f := &answerFakeClient{empty: true}
	session := func(ctx context.Context, fn func(context.Context, answerClient) error) error { return fn(ctx, f) }
	out, _, err := answerQuestion(t.Context(), session, []byte(`{"question":"q","terms":["first","second"]}`))
	require.NoError(t, err)
	require.Empty(t, f.requests)
	require.Equal(t, 3, f.validations)
	require.Equal(t, answer.NoMatch, out.Status)
	require.False(t, out.Receipt.TermsSearched)
	require.Empty(t, out.Receipt.TermsSHA256)
}

func TestAnswerQuestionTermsDuplicateLeads(t *testing.T) {
	fixture := newNativeExportFixture(t)
	member := fixture.addFile(t, "lease.txt", "text")
	f := &answerTermsClient{answerFakeClient: &answerFakeClient{fenceVersions: []string{member.VersionID}}, summaries: daemonDocumentClient{fixture.connection}}
	f.searchReport = func(req api.DocumentSearchRequest) (api.DocumentSearchReport, error) {
		return answerTermsSearchReport(req, api.DocumentSearchResult{NodeID: member.NodeID, ContentVersionID: member.VersionID, Path: "/lease.txt", Evidence: []api.DocumentEvidenceReference{{Kind: "node_name"}}}), nil
	}
	session := func(ctx context.Context, fn func(context.Context, answerClient) error) error { return fn(ctx, f) }
	out, _, err := answerQuestion(t.Context(), session, []byte(`{"question":"lease","terms":["lease","txt"]}`))
	require.NoError(t, err)
	require.Len(t, out.Leads, 1)
	require.Empty(t, out.Passages)
}

func TestAnswerQuestionTermsReceiptWithoutEcho(t *testing.T) {
	f := &answerFakeClient{searchReport: func(req api.DocumentSearchRequest) (api.DocumentSearchReport, error) {
		return answerTermsSearchReport(req), nil
	}}
	session := func(ctx context.Context, fn func(context.Context, answerClient) error) error { return fn(ctx, f) }
	out, _, err := answerQuestion(t.Context(), session, []byte(`{"question":"q","terms":[" classified "," token "]}`))
	require.NoError(t, err)
	require.Equal(t, "classified", f.requests[1].Query)
	require.Equal(t, "token", f.requests[2].Query)
	hash := sha256.Sum256([]byte("classified   token"))
	require.True(t, out.Receipt.TermsSearched)
	require.Equal(t, hex.EncodeToString(hash[:]), out.Receipt.TermsSHA256)
	encoded, err := json.Marshal(out)
	require.NoError(t, err)
	require.NotContains(t, string(encoded), "classified")
	require.NotContains(t, string(encoded), "token")
}
