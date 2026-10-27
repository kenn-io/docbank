package mcp

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/answercard"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/daemonconn"
)

func TestMatchAnswerCardsDefaultReadTool(t *testing.T) {
	tool := catalogMap(toolCatalog(ServerOptions{}))["match_answer_cards"]
	require.NotNil(t, tool)
	require.True(t, tool.Annotations.ReadOnlyHint)
	assertSchemaContract(t, tool.InputSchema)
	assertSchemaContract(t, tool.OutputSchema)
	input := mustResolveSchema(tool.InputSchema)
	require.NoError(t, input.Validate(map[string]any{"question": "library hours"}))
	require.Error(t, input.Validate(map[string]any{"question": "library hours", "approve": true}))
	require.Error(t, input.Validate(map[string]any{"question": "library hours", "limit": 11}))
	flood := false
	pages := 0
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/api/v1/info" {
			_ = json.NewEncoder(w).Encode(map[string]any{"vault_id": testVaultID})
			return
		}
		assert.Equal(t, "/api/v1/documents", r.URL.Path)
		assert.Equal(t, "/answer-cards", r.URL.Query().Get("path_prefix"))
		if flood {
			pages++
			items := make([]api.DocumentSummary, 250)
			for i := range items {
				items[i] = api.DocumentSummary{NodeID: int64(i + 1), ContentVersionID: testVersionID, Path: "/answer-cards/file.pdf", Name: "file.pdf", MediaType: "application/pdf", ModifiedAt: "2026-10-27T00:00:00Z"}
			}
			_ = json.NewEncoder(w).Encode(api.DocumentPage{PathPrefix: "/answer-cards", Sort: "path", Direction: "asc", PageSize: 250, Items: items, NextCursor: strconv.Itoa(pages)})
			return
		}
		_ = json.NewEncoder(w).Encode(api.DocumentPage{PathPrefix: "/answer-cards", Sort: "path", Direction: "asc", PageSize: 250, Items: []api.DocumentSummary{}})
	}))
	defer s.Close()
	lease := newDaemonLeaseWith(func(context.Context) (*daemonconn.Connection, error) {
		return daemonconn.New(s.URL, "synthetic-key"), nil
	}, func(*daemonconn.Connection) error { return nil })
	result, err := executeReadTool(t.Context(), lease, nil, "match_answer_cards", mustResolveSchema(tool.OutputSchema), []byte(`{"question":"library hours"}`))
	require.NoError(t, err)
	encoded, err := json.Marshal(result.StructuredContent)
	require.NoError(t, err)
	require.Contains(t, string(encoded), `"status":"no_match"`)
	require.Contains(t, string(encoded), `"cacheScope":"private"`)
	flood = true
	result, err = executeReadTool(t.Context(), lease, nil, "match_answer_cards", mustResolveSchema(tool.OutputSchema), []byte(`{"question":"library hours"}`))
	require.Nil(t, result)
	require.ErrorIs(t, err, answercard.ErrCardLimit)
	failure, ok := domainToolError(err)
	require.True(t, ok)
	require.True(t, failure.IsError)
	encoded, err = json.Marshal(failure.StructuredContent)
	require.NoError(t, err)
	require.Contains(t, string(encoded), `"code":"card_limit"`)
}

func TestMatchAnswerCardsPopulatedResults(t *testing.T) {
	for _, tc := range []struct {
		name, reason                        string
		wantMatches, wantStale, wantInvalid int
	}{
		{name: "approved", wantMatches: 1},
		{name: "stale", reason: "new_version", wantStale: 1},
		{name: "Unicode reason boundary", reason: strings.Repeat("界", 300), wantStale: 1},
		{name: "oversized reason", reason: strings.Repeat("界", 301), wantInvalid: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const cardVersion = "33333333-3333-4333-8333-333333333333"
			digest := strings.Repeat("a", 64)
			raw := fmt.Sprintf(`---
docbank_answer_card: 1
question: When does the policy renew?
citations:
  - version: 1
    vault_uid: %s
    node_id: 42
    content_version_id: %s
    content_sha256: %s
    rendition_attachment_id: %s
    build_id: %s
    rendition_sha256: %s
    start: 0
    end: 4
    quote: test
state: draft
---
The policy renews annually.
`, testVaultID, testVersionID, digest, digest, digest, digest)
			card, err := answercard.Parse([]byte(raw))
			require.NoError(t, err)
			state := "state: approved\napproved_hash: " + card.Hash()
			if tc.reason != "" {
				state = "state: stale\nstale:\n  reason: " + tc.reason + "\n  citation: 0"
			}
			raw = strings.Replace(raw, "state: draft", state, 1)
			hash := sha256.Sum256([]byte(raw))
			contentHash := hex.EncodeToString(hash[:])
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case "/api/v1/info":
					_ = json.NewEncoder(w).Encode(map[string]any{"vault_id": testVaultID})
				case "/api/v1/documents":
					_ = json.NewEncoder(w).Encode(api.DocumentPage{PathPrefix: "/answer-cards", Sort: "path", Direction: "asc", PageSize: 250, Items: []api.DocumentSummary{{NodeID: 7, ContentVersionID: cardVersion, Path: "/answer-cards/policy.md", Name: "policy.md", MediaType: "text/markdown", Size: int64(len(raw)), ModifiedAt: "2026-10-27T00:00:00Z"}}})
				case "/api/v1/versions/" + cardVersion + "/content":
					w.Header().Set(api.ContentVersionHeader, cardVersion)
					w.Header().Set(api.BlobHashHeader, contentHash)
					w.Header().Set(api.BlobSizeHeader, strconv.Itoa(len(raw)))
					w.Header().Set("Trailer", "Content-Digest")
					_, _ = w.Write([]byte(raw))
					w.Header().Set("Content-Digest", "sha-256=:"+base64.StdEncoding.EncodeToString(hash[:])+":")
				case "/api/v1/nodes/7":
					_ = json.NewEncoder(w).Encode(api.Node{ID: 7, Kind: "file", Path: "/answer-cards/policy.md", CurrentVersionID: cardVersion, BlobHash: contentHash, Size: int64(len(raw))})
				case "/api/v1/nodes/42":
					_ = json.NewEncoder(w).Encode(api.Node{ID: 42, Kind: "file", CurrentVersionID: testVersionID, BlobHash: digest})
				case "/api/v1/text-citations/resolve":
					quote := card.Citations[0].Quote
					sum := sha256.Sum256([]byte(quote))
					_ = json.NewEncoder(w).Encode(document.ResolvedTextCitation{Citation: card.Citations[0].TextCitation(), Text: quote, TextBytes: len(quote), TextSHA256: hex.EncodeToString(sum[:])})
				default:
					t.Errorf("unexpected route %s", r.URL.Path)
					http.Error(w, "unexpected", http.StatusInternalServerError)
				}
			}))
			defer server.Close()
			lease := newDaemonLeaseWith(func(context.Context) (*daemonconn.Connection, error) {
				return daemonconn.New(server.URL, "synthetic-key"), nil
			}, func(*daemonconn.Connection) error { return nil })
			tool := catalogMap(toolCatalog(ServerOptions{}))["match_answer_cards"]
			result, err := executeReadTool(t.Context(), lease, nil, "match_answer_cards", mustResolveSchema(tool.OutputSchema), []byte(`{"question":"When does the policy renew?"}`))
			require.NoError(t, err)
			encoded, err := json.Marshal(result.StructuredContent)
			require.NoError(t, err)
			var got struct {
				Status     string                 `json:"status"`
				Matches    []answercard.MatchItem `json:"matches"`
				StaleCards []answercard.StaleCard `json:"stale_cards"`
				Excluded   answercard.Excluded    `json:"excluded"`
			}
			require.NoError(t, json.Unmarshal(encoded, &got))
			require.Len(t, got.Matches, tc.wantMatches)
			require.Len(t, got.StaleCards, tc.wantStale)
			require.Equal(t, tc.wantInvalid, got.Excluded.Invalid)
			if tc.wantMatches > 0 {
				require.Equal(t, "matched", got.Status)
				require.Equal(t, card.Question, got.Matches[0].MatchedPhrasing)
				require.Equal(t, card.Citations, got.Matches[0].Citations)
			} else {
				require.Equal(t, "no_match", got.Status)
			}
			if tc.wantStale > 0 {
				require.Equal(t, tc.reason, got.StaleCards[0].Reason)
			}
		})
	}
}
