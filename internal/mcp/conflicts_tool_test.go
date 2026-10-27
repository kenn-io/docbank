package mcp

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"maps"
	"strconv"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/conflicts"
	"go.kenn.io/docbank/internal/store"
)

func TestFindConflictsRegistered(t *testing.T) {
	tool, err := ReadTool("find_conflicts")
	require.NoError(t, err)
	require.Equal(t, "find_conflicts", tool.Name)
}

type conflictTestClient struct {
	results      []api.DocumentSearchResult
	summaryCalls int
	stale        bool
	limit        int
	truncated    bool
	searchErr    error
	summaryErr   error
	badSummary   bool
}

func (c *conflictTestClient) VaultInfo(context.Context) (api.VaultInfo, error) {
	return api.VaultInfo{}, nil
}
func (c *conflictTestClient) ListDocuments(context.Context, api.DocumentQuery) (api.DocumentPage, error) {
	return api.DocumentPage{}, nil
}
func (c *conflictTestClient) ResolveDocumentSourceFence(context.Context, api.DocumentSourceFenceResolveRequest) (api.DocumentSourceFenceResolution, error) {
	return api.DocumentSourceFenceResolution{Fence: api.ResolvedDocumentSourceFence{VaultUID: testVaultID, ContentVersionIDs: []string{testVersionID}}, FenceFingerprint: testFenceFingerprint}, nil
}
func (c *conflictTestClient) ValidateDocumentSearch(context.Context, api.DocumentSearchValidationRequest) error {
	return nil
}
func (c *conflictTestClient) SearchDocuments(_ context.Context, req api.DocumentSearchRequest) (api.DocumentSearchReport, error) {
	c.limit = req.Limit
	return api.DocumentSearchReport{ActualMode: "lexical", Coverage: api.DocumentSearchCoverage{State: "complete"}, Results: c.results, Truncated: c.truncated}, c.searchErr
}
func (c *conflictTestClient) ResolveDocumentSummaries(_ context.Context, req api.DocumentSummaryResolveRequest) ([]api.DocumentSummary, error) {
	c.summaryCalls++
	if c.summaryCalls == 2 && c.summaryErr != nil {
		return nil, c.summaryErr
	}
	if c.stale && c.summaryCalls == 2 {
		return nil, errors.New("stale version")
	}
	out := make([]api.DocumentSummary, len(req.Identities))
	for i, id := range req.Identities {
		out[i] = api.DocumentSummary{NodeID: id.NodeID, ContentVersionID: id.ContentVersionID, Path: id.Path, ModifiedAt: "2026-01-01T00:00:00Z"}
		if c.badSummary && c.summaryCalls == 2 {
			out[i].Path += "-moved"
		}
	}
	return out, nil
}

func TestFindConflictsContentBlobAndUnicodeOffsets(t *testing.T) {
	c := &conflictTestClient{results: []api.DocumentSearchResult{
		conflictHit(1, "🍂 The café cooling period is 7 days.", "content_blob"),
		conflictHit(2, "🍂 The café cooling period is 15 days.", "rendition_segment"),
	}}
	out, err := findConflicts(t.Context(), c.session, []byte(`{"query":"cooling period","profile":"local","filters":{}}`))
	require.NoError(t, err)
	require.Equal(t, conflicts.Conflicting, out.Status)
	require.Len(t, out.Conflicts, 1)
	assertConflictCitations(t, out)
}

func TestFindConflictsExpandedNormalizationConformsToSchema(t *testing.T) {
	ligatures := strings.Repeat("ﬃ", 180)
	normalized := strings.Repeat("ffi", 180)
	for _, tc := range []struct {
		name, query, first, second string
	}{
		{"identifier", "policy", "Policy: " + ligatures + "1.", "Policy: " + ligatures + "2."},
		{"subject", "fee", "The " + ligatures + " fee is $10.", "The " + ligatures + " fee is $20."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Less(t, len([]rune(tc.first)), 512)
			require.Less(t, len([]rune(tc.second)), 512)
			c := &conflictTestClient{results: []api.DocumentSearchResult{
				conflictHit(1, tc.first, "content_blob"), conflictHit(2, tc.second, "rendition_segment"),
			}}
			raw, err := json.Marshal(searchDocumentsInput{Query: tc.query, Profile: "local", Filters: &api.DocumentSourceFenceFilters{}})
			require.NoError(t, err)
			out, err := findConflicts(t.Context(), c.session, raw)
			require.NoError(t, err)
			require.Equal(t, conflicts.Conflicting, out.Status)
			require.Len(t, out.Conflicts, 1)
			assertConflictCitations(t, out)
			if tc.name == "identifier" {
				require.Equal(t, normalized+"1", out.Conflicts[0].Sides[0].Value)
				require.Equal(t, normalized+"2", out.Conflicts[0].Sides[1].Value)
			} else {
				require.Contains(t, out.Conflicts[0].Subject, normalized)
			}
			_, output := findConflictsSchemas()
			result, err := boundedToolSuccess(mustResolveSchema(output), out, nil)
			require.NoError(t, err)
			wire, err := json.Marshal(result)
			require.NoError(t, err)
			require.LessOrEqual(t, len(wire), maxToolResponseBytes)
		})
	}
}

func assertConflictCitations(t *testing.T, out conflictsOutput) {
	t.Helper()
	if out.Status == conflicts.Conflicting {
		require.NotEmpty(t, out.Conflicts)
	}
	for _, edge := range out.Conflicts {
		require.Len(t, edge.Sides, 2)
		for _, side := range edge.Sides {
			require.GreaterOrEqual(t, side.Passage, 0)
			require.Less(t, side.Passage, len(out.Passages))
			p := out.Passages[side.Passage]
			require.True(t, p.Compared)
			require.NotEmpty(t, p.EvidenceIDs)
			runes := []rune(p.Excerpt)
			require.Equal(t, side.Quote, string(runes[side.QuoteStart:side.QuoteEnd]))
			require.Equal(t, side.Figure, string(runes[side.FigureStart:side.FigureEnd]))
		}
	}
}

func TestFindConflictsIncompleteComparison(t *testing.T) {
	c := &conflictTestClient{}
	// Twenty short excerpts still exceed the library's 10,000-pair work bound.
	for i := range 20 {
		c.results = append(c.results, conflictHit(int64(i+1), strings.Repeat("The fee is $10. ", 9), "rendition_segment"))
	}
	out, err := findConflicts(t.Context(), c.session, []byte(`{"query":"fee","profile":"local","filters":{}}`))
	require.NoError(t, err)
	require.Equal(t, conflicts.NotComparable, out.Status)
	require.True(t, out.ConflictsTruncated, "an unfinished scan must be disclosed")
}

func TestFindConflictsResponseBounds(t *testing.T) {
	for _, different := range []bool{false, true} {
		t.Run(strconv.FormatBool(different), func(t *testing.T) {
			c := &conflictTestClient{}
			for i := range 20 {
				text := "The cooling period is 7 days."
				if different && i%2 == 1 {
					text = "The cooling period is 15 days."
				}
				hit := conflictHit(int64(i+1), text, "rendition_segment")
				// Schema-valid Unicode paths and 64 opaque evidence identities force
				// pruning only when the duplicated escaped MCP envelope is measured.
				hit.Path = "/" + strings.Repeat("é\\\"\n", 2500)
				evidence := api.DocumentEvidenceReference{Kind: "rendition_segment", BuildID: strings.Repeat("a", 64), SegmentID: strings.Repeat("b", 64), SourceManifestChecksum: strings.Repeat("c", 64)}
				hit.Evidence = make([]api.DocumentEvidenceReference, 64)
				for j := range hit.Evidence {
					hit.Evidence[j] = evidence
				}
				c.results = append(c.results, hit)
			}
			out, err := findConflicts(t.Context(), c.session, []byte(`{"query":"cooling period","profile":"local","filters":{}}`))
			require.NoError(t, err)
			require.True(t, out.PassagesTruncated)
			require.Equal(t, conflicts.NotComparable, out.Status, "omitted evidence cannot assert a complete result")
			if different {
				require.True(t, out.ConflictsTruncated)
			}
			assertConflictCitations(t, out)
			_, output := findConflictsSchemas()
			result, err := boundedToolSuccess(mustResolveSchema(output), out, nil)
			require.NoError(t, err)
			wire, err := json.Marshal(result)
			require.NoError(t, err)
			require.LessOrEqual(t, len(wire), maxToolResponseBytes)
		})
	}
}

func TestFindConflictsPropagatesDomainFailures(t *testing.T) {
	for _, atSearch := range []bool{false, true} {
		c := &conflictTestClient{results: []api.DocumentSearchResult{conflictHit(1, "The fee is $10.", "rendition_segment")}}
		if atSearch {
			c.searchErr = store.ErrProcessingSourceFenceStaleVersion
		} else {
			c.summaryErr = store.ErrProcessingSourceFenceStaleVersion
		}
		_, err := findConflicts(t.Context(), c.session, []byte(`{"query":"fee","profile":"local","filters":{}}`))
		require.ErrorIs(t, err, store.ErrProcessingSourceFenceStaleVersion)
		result, ok := domainToolError(err)
		require.True(t, ok)
		require.True(t, result.IsError)
		require.Equal(t, "stale_version", structuredMap(t, result.StructuredContent)["code"])
	}
	c := &conflictTestClient{results: []api.DocumentSearchResult{conflictHit(1, "The fee is $10.", "rendition_segment")}, badSummary: true}
	_, err := findConflicts(t.Context(), c.session, []byte(`{"query":"fee","profile":"local","filters":{}}`))
	require.ErrorContains(t, err, "summary identity mismatch")
}
func (c *conflictTestClient) session(ctx context.Context, fn func(context.Context, documentClient) error) error {
	return fn(ctx, c)
}
func conflictHit(id int64, text, kind string) api.DocumentSearchResult {
	return api.DocumentSearchResult{NodeID: id, ContentVersionID: testVersionID, Rank: int(id), Path: fmt.Sprintf("/doc%d", id), Excerpt: text, Evidence: []api.DocumentEvidenceReference{{Kind: kind}}}
}
func TestFindConflictsResults(t *testing.T) {
	c := &conflictTestClient{results: []api.DocumentSearchResult{conflictHit(1, "The cooling period is 7 days.", "rendition_segment"), conflictHit(2, "The cooling period is 15 days.", "rendition_segment"), conflictHit(3, "7 days", "node_name"), conflictHit(4, "", "embedding"), conflictHit(5, strings.Repeat("a", 512), "rendition_segment")}}
	out, err := findConflicts(t.Context(), c.session, []byte(`{"query":"cooling period","profile":"local","filters":{}}`))
	require.NoError(t, err)
	require.Equal(t, 20, c.limit)
	require.Equal(t, conflicts.Conflicting, out.Status)
	require.Len(t, out.Conflicts, 1)
	require.Equal(t, "no_content_evidence", out.Passages[2].SkipReason)
	require.Equal(t, "no_excerpt", out.Passages[3].SkipReason)
	require.Equal(t, "saturated_excerpt", out.Passages[4].SkipReason)
	for _, side := range out.Conflicts[0].Sides {
		r := []rune(out.Passages[side.Passage].Excerpt)
		require.Equal(t, side.Quote, string(r[side.QuoteStart:side.QuoteEnd]))
		require.Equal(t, side.Figure, string(r[side.FigureStart:side.FigureEnd]))
	}
	_, err = boundedToolSuccess(mustResolveSchema(func() schema { _, s := findConflictsSchemas(); return s }()), out, nil)
	require.NoError(t, err)
}
func TestFindConflictsEmptyAndStale(t *testing.T) {
	c := &conflictTestClient{}
	out, err := findConflicts(t.Context(), c.session, []byte(`{"query":"cooling","profile":"local","filters":{}}`))
	require.NoError(t, err)
	require.Equal(t, conflicts.NotComparable, out.Status)
	require.Zero(t, c.summaryCalls)
	c.results = []api.DocumentSearchResult{conflictHit(1, "The cooling period is 7 days.", "rendition_segment")}
	c.stale = true
	_, err = findConflicts(t.Context(), c.session, []byte(`{"query":"cooling","profile":"local","filters":{}}`))
	require.ErrorContains(t, err, "stale version")
}

func TestFindConflictsLimits(t *testing.T) {
	for _, limit := range []int{1, 20} {
		c := &conflictTestClient{}
		_, err := findConflicts(t.Context(), c.session, fmt.Appendf(nil, `{"query":"fee","profile":"local","filters":{},"limit":%d}`, limit))
		require.NoError(t, err)
		require.Equal(t, limit, c.limit)
	}
	for _, limit := range []int{-1, 21} {
		c := &conflictTestClient{}
		_, err := findConflicts(t.Context(), c.session, fmt.Appendf(nil, `{"query":"fee","profile":"local","filters":{},"limit":%d}`, limit))
		require.Error(t, err)
		require.Zero(t, c.limit)
	}
}

func TestFindConflictsSkipReasons(t *testing.T) {
	for _, tc := range []struct{ name, text, kind, reason string }{
		{"semantic", "", "embedding_input", "no_excerpt"},
		{"name", "The fee is $20.", "node_name", "no_content_evidence"},
		{"clipped_prefix", "…The fee is $20.", "rendition_segment", "saturated_excerpt"},
		{"clipped_excerpt", "The fee is $20. " + strings.Repeat("x", 480) + "…", "rendition_segment", "saturated_excerpt"},
		{"saturated_unicode", "The fee is $20. " + strings.Repeat("é", 497), "rendition_segment", "saturated_excerpt"},
		{"partial", " … The fee is $20.", "rendition_segment", "partial"},
		{"no_figures", "The fee depends on the contract.", "content_blob", "no_figures"},
		{"negated", "The fee is not $20.", "rendition_segment", "negated"},
		{"ambiguous", "The fee is $500 and $250 for students.", "rendition_segment", "ambiguous"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := &conflictTestClient{results: []api.DocumentSearchResult{conflictHit(1, "The fee is $10.", "rendition_segment"), conflictHit(2, tc.text, tc.kind), conflictHit(3, "The fee is $10.", "rendition_segment")}}
			out, err := findConflicts(t.Context(), c.session, []byte(`{"query":"fee","profile":"local","filters":{}}`))
			require.NoError(t, err)
			require.Equal(t, conflicts.NoConflict, out.Status)
			require.Empty(t, out.Conflicts)
			require.True(t, out.Passages[0].Compared)
			require.True(t, out.Passages[2].Compared)
			require.False(t, out.Passages[1].Compared)
			require.Equal(t, tc.reason, out.Passages[1].SkipReason)
		})
	}
}

func TestFindConflictsEdgeLimitAndSearchMetadata(t *testing.T) {
	c := &conflictTestClient{truncated: true}
	for i := range 8 {
		c.results = append(c.results, conflictHit(int64(i+1), fmt.Sprintf("The fee is $%d.", i+10), "rendition_segment"))
	}
	out, err := findConflicts(t.Context(), c.session, []byte(`{"query":"fee","profile":"local","filters":{}}`))
	require.NoError(t, err)
	require.Equal(t, conflicts.Conflicting, out.Status)
	require.Len(t, out.Conflicts, 16)
	require.True(t, out.ConflictsTruncated)
	require.False(t, out.PassagesTruncated)
	require.True(t, out.Truncated)
	require.Equal(t, "lexical", out.ActualMode)
	require.Equal(t, testVaultID, out.VaultID)
	require.Equal(t, testFenceFingerprint, out.FenceFingerprint)
	require.Equal(t, "complete", out.Coverage.State)
	assertConflictCitations(t, out)
	for _, p := range out.Passages {
		require.Equal(t, "2026-01-01T00:00:00Z", p.ModifiedAt)
	}
}

func TestFindConflictsRejectsInvalidArgumentsBeforeReading(t *testing.T) {
	for _, tc := range []struct {
		name string
		args map[string]any
	}{
		{"zero_limit", map[string]any{"limit": 0}},
		{"large_limit", map[string]any{"limit": 21}},
		{"unknown", map[string]any{"excerpts": []string{"invented evidence"}}},
		{"two_selectors", map[string]any{"content_version_ids": []string{testVersionID}}},
		{"invalid_since", map[string]any{"filters": map[string]any{"modified_since": "2026-02-30T00:00:00Z"}}},
		{"invalid_before", map[string]any{"filters": map[string]any{"modified_before": "2026-08-28T00:00:00"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args := map[string]any{"query": "fee", "profile": "local", "filters": map[string]any{}}
			maps.Copy(args, tc.args)
			wireErr := decodeWireError(t, callToolWire(t, "find_conflicts", args))
			require.Equal(t, int64(jsonrpc.CodeInvalidParams), wireErr.Code)
			raw, err := json.Marshal(args)
			require.NoError(t, err)
			// A nil reader proves embedded admission rejects before any read.
			_, err = CallDocumentReadTool(t.Context(), nil, &sdkmcp.CallToolRequest{Params: &sdkmcp.CallToolParamsRaw{Name: "find_conflicts", Arguments: raw}})
			require.Error(t, err)
		})
	}
	args := map[string]any{"query": "fee", "profile": "local", "filters": map[string]any{
		"modified_since":  "2026-08-28T01:02:03.123456789+02:30",
		"modified_before": "2026-09-28T01:02:03Z",
	}}
	response := decodeResult(t, callToolWire(t, "find_conflicts", args))
	require.Equal(t, "daemon_unavailable", objectField(t, response, "structuredContent")["code"])
}
