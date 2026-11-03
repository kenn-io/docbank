package mcp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"testing"

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
		Coverage: answerTermsSearchCoverage(req),
		Results:  slices.Clone(results),
	}
	for i := range report.Results {
		r := &report.Results[i]
		r.VaultUID, r.Rank = req.Fence.VaultUID, i+1
		r.LexicalRank = 0
		for _, evidence := range r.Evidence {
			if evidence.Kind == "embedding" {
				r.SemanticRank = i + 1
			} else {
				r.LexicalRank = i + 1
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

func answerTermsSearchCoverage(req api.DocumentSearchRequest) api.DocumentSearchCoverage {
	if req.Mode == "lexical" {
		return api.DocumentSearchCoverage{State: "unknown"}
	}
	return api.DocumentSearchCoverage{State: "complete", ScopedDocuments: len(req.Fence.ContentVersionIDs), CompleteDocuments: len(req.Fence.ContentVersionIDs)}
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
	require.True(t, with.CoverageComplete)
	require.Equal(t, "complete", with.Receipt.Coverage.State)
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
}

func TestAnswerQuestionTermsMatchIndependently(t *testing.T) {
	f := newNativeExportFixture(t)
	member := f.addFile(t, "agreement.txt", "Lease renewal is 1 March.")
	require.NoError(t, f.catalog.RecordExtraction(t.Context(), store.ExtractionResult{
		BlobHash: member.SHA256, Extractor: "plain-text", ExtractorVersion: 1, Status: store.ExtractionOK, Text: "Lease renewal is 1 March.",
	}))
	server, _, _ := newProcessingDocumentServer(t, f)
	for _, tc := range []struct {
		terms []string
		want  int
	}{{[]string{"lease"}, 1}, {[]string{"lease nonexistentword"}, 0}} {
		out := exportCall(t, server, "answer_question", map[string]any{"question": "when must I renew", "terms": tc.terms})
		passages, ok := out["passages"].([]any)
		require.True(t, ok)
		require.Len(t, passages, tc.want)
		if tc.want > 0 {
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
	lead := results[0]
	lead.Excerpt = ""
	lead.Evidence = []api.DocumentEvidenceReference{{Kind: "node_name"}}
	results = append(results, lead)
	for _, tc := range []struct {
		name                         string
		question, first, second      []int
		limit                        int
		termTruncated, wantTruncated bool
		wantLeads                    int
		want                         []string
	}{
		{"round robin", []int{0, 1}, []int{2, 3}, []int{4, 2}, 5, false, false, 0, []string{"a", "c", "b", "e", "d"}},
		{"cross-search duplicates", []int{0, 1}, []int{1, 2}, []int{2, 3}, 5, false, false, 0, []string{"a", "b", "c", "d"}},
		{"combined limit", []int{0, 1}, []int{2, 3}, []int{4, 2}, 3, false, true, 0, []string{"a", "c", "b"}},
		{"term truncated", nil, nil, []int{2}, 1, true, true, 0, []string{"c"}},
		{"duplicate leads", []int{5}, []int{5}, []int{5}, 5, false, false, 1, nil},
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
				report.Truncated = tc.termTruncated && req.Query == "second"
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
					require.InDelta(t, 0.7, *passage.Relevance, 1e-12)
				} else {
					require.Nil(t, passage.Relevance)
				}
			}
			require.Len(t, out.Leads, tc.wantLeads)
			require.Equal(t, tc.want, texts)
			require.Equal(t, tc.wantTruncated, slices.Contains(out.CoverageReasons, "retrieval_truncated"))
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
	for _, terms := range []any{nil, []any{}, []any{""}, []any{"lease", "lease"}, []any{strings.Repeat("界", 65)}, []any{"a", "b", "c", "d", "e", "f", "g", "h", "i"}} {
		assertSchemaRejects(t, input, map[string]any{"question": "q", "terms": terms})
	}
}

func TestAnswerQuestionTermsRetainedReceipt(t *testing.T) {
	fixture := newNativeExportFixture(t)
	member := fixture.addFile(t, "lease.txt", "Lease renewal and termination.")
	leadMember := fixture.addFile(t, "index.txt", "Index")
	for _, budget := range []int{2048, 4096} {
		t.Run(strconv.Itoa(budget), func(t *testing.T) {
			f := &answerTermsClient{answerFakeClient: &answerFakeClient{fenceVersions: []string{member.VersionID, leadMember.VersionID}}, summaries: daemonDocumentClient{fixture.connection}}
			space := strings.Repeat("c", 64)
			f.searchReport = func(req api.DocumentSearchRequest) (api.DocumentSearchReport, error) {
				result := api.DocumentSearchResult{NodeID: member.NodeID, ContentVersionID: member.VersionID, Path: "/lease.txt", Excerpt: strings.Repeat("𐐀", 512), Evidence: []api.DocumentEvidenceReference{{Kind: "content_blob"}}}
				if req.Mode == "hybrid" {
					require.True(t, req.Rerank, "reranking places the lead before the passage found in both lanes")
					result.Score = 0.7
					result.Evidence = append(result.Evidence, api.DocumentEvidenceReference{Kind: "embedding", VectorSpaceID: space, EmbeddingSetID: strings.Repeat("d", 64), InputGenerationID: strings.Repeat("e", 64), InputID: "input-a", InputKind: "rendition_chunk", BuildID: strings.Repeat("a", 64), SourceManifestChecksum: strings.Repeat("f", 64)})
					lead := api.DocumentSearchResult{NodeID: leadMember.NodeID, ContentVersionID: leadMember.VersionID, Path: "/index.txt", Score: 0.9, Evidence: []api.DocumentEvidenceReference{{Kind: "node_name"}}}
					return answerTermsSearchReport(req, lead, result), nil
				}
				result.Excerpt = "Lease"
				return answerTermsSearchReport(req, result), nil
			}
			session := func(ctx context.Context, fn func(context.Context, answerClient) error) error { return fn(ctx, f) }
			raw := fmt.Appendf(nil, `{"question":"renewal","terms":["lease"],"rerank":true,"max_passage_bytes":%d}`, budget)
			out, _, err := answerQuestion(t.Context(), session, raw)
			require.NoError(t, err)
			if budget == 2048 {
				require.Len(t, out.Passages, 1)
				require.Equal(t, "Lease", out.Passages[0].Text)
				require.Empty(t, out.Receipt.VectorSpaceIDs)
			} else {
				require.Len(t, out.Passages, 2)
				require.Equal(t, []string{space}, out.Receipt.VectorSpaceIDs)
			}

			encoded, err := json.Marshal(out)
			require.NoError(t, err)
			var normalized any
			require.NoError(t, json.Unmarshal(encoded, &normalized))
			_, output := answerQuestionSchemas()
			assertSchemaAccepts(t, output, normalized)
			require.Equal(t, []answerCitation{{NodeID: member.NodeID, ContentVersionID: member.VersionID}}, out.Receipt.CitedVersions)
		})
	}
}

func TestAnswerQuestionTermsReceiptWithoutEcho(t *testing.T) {
	f := &answerFakeClient{searchReport: func(req api.DocumentSearchRequest) (api.DocumentSearchReport, error) {
		return answerTermsSearchReport(req), nil
	}}
	session := func(ctx context.Context, fn func(context.Context, answerClient) error) error { return fn(ctx, f) }
	out, _, err := answerQuestion(t.Context(), session, []byte(`{"question":"q","terms":["classified","token"]}`))
	require.NoError(t, err)
	hash := sha256.Sum256([]byte(`["classified","token"]`))
	require.Equal(t, hex.EncodeToString(hash[:]), out.Receipt.TermsSHA256)
	encoded, err := json.Marshal(out)
	require.NoError(t, err)
	require.NotContains(t, string(encoded), "classified")
	require.NotContains(t, string(encoded), "token")
	for _, tc := range []struct {
		terms string
		same  bool
	}{
		{`["classified","token"]`, true},
		{`["classified token"]`, false},
		{`["token","classified"]`, false},
	} {
		other, _, err := answerQuestion(t.Context(), session, []byte(`{"question":"q","terms":`+tc.terms+`}`))
		require.NoError(t, err)
		if tc.same {
			require.Equal(t, out.Receipt.TermsSHA256, other.Receipt.TermsSHA256)
		} else {
			require.NotEqual(t, out.Receipt.TermsSHA256, other.Receipt.TermsSHA256)
		}
	}
}
