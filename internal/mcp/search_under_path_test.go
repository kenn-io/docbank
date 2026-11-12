package mcp

import (
	"context"
	"errors"
	"strings"
	"testing"

	apiruntime "github.com/doordash-oss/oapi-codegen-dd/v3/pkg/runtime"
	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/docbank/internal/processing"
	"go.kenn.io/docbank/internal/store"
)

func TestSearchUnderPathToolsScope(t *testing.T) {
	f := newNativeExportFixture(t)
	dir, err := f.catalog.Mkdir(t.Context(), f.catalog.RootID(), "Café")
	require.NoError(t, err)
	for index, text := range []string{"The cooling period is 7 days.", "The cooling period is 15 days.", "The cooling period is 30 days."} {
		member := f.addFile(t, string(rune('a'+index))+".txt", text)
		require.NoError(t, f.catalog.RecordExtraction(t.Context(), store.ExtractionResult{
			BlobHash: member.SHA256, Extractor: "plain-text", ExtractorVersion: 1, Status: store.ExtractionOK, Text: text,
		}))
		if index < 2 {
			node, err := f.catalog.NodeByID(t.Context(), member.NodeID)
			require.NoError(t, err)
			_, _, err = f.catalog.Move(t.Context(), node.ID, dir.ID, node.Name, node.Revision)
			require.NoError(t, err)
		}
	}
	server, _, profile := newProcessingDocumentServer(t, f)
	for _, tool := range []struct{ name, query, results string }{
		{"search_documents", "query", "results"},
		{"answer_question", "question", "passages"},
		{"find_conflicts", "query", "passages"},
	} {
		t.Run(tool.name, func(t *testing.T) {
			args := map[string]any{tool.query: "cooling period", "profile": profile, "filters": map[string]any{"under_path": "/Café"}}
			byPath := exportCall(t, server, tool.name, args)
			require.NotContains(t, byPath, "code")
			require.Len(t, byPath[tool.results], 2)
		})
	}
}

func TestSearchUnderPathInvalidScopesNeverCallProvider(t *testing.T) {
	f := newNativeExportFixture(t)
	f.addFile(t, "mercury.txt", "mercury")
	dir, err := f.catalog.Mkdir(t.Context(), f.catalog.RootID(), "trashed")
	require.NoError(t, err)
	_, _, err = f.catalog.Trash(t.Context(), dir.ID, dir.Revision)
	require.NoError(t, err)
	profile := newSearchTestEmbeddingProfile(t, f, document.EmbeddingInputOriginalFile, false)
	provider := &searchQueryCounter{EmbeddingProvider: profile.EmbeddingProviders["query"]}
	profile.EmbeddingProviders["query"] = provider
	service, err := processing.NewService(t.Context(), processing.ServiceConfig{Catalog: f.catalog, Blobs: f.blobs,
		Gate: api.NewOperationGate(), SpoolDirectory: t.TempDir(), Profiles: map[string]processing.ProfileConfig{"text": profile}})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, service.Shutdown(context.WithoutCancel(t.Context()))) })
	server := newSearchProcessingServer(t, f, service)
	for _, tool := range []string{"search_documents", "answer_question"} {
		for _, tc := range []struct {
			name, path, code string
			id               int64
		}{
			{"missing", "/missing", "not_found", 0},
			{"trashed", "/trashed", "not_found", 0},
			{"file", "/mercury.txt", "not_dir", 0},
			{"dotdot", "/..", "validation", 0},
			{"path and node id", "/", "validation", f.catalog.RootID()},
		} {
			t.Run(tool+"/"+tc.name, func(t *testing.T) {
				t.Logf("scope diagnostic tool=%s path=%q node_id=%d expected=%s", tool, tc.path, tc.id, tc.code)
				if tool == "answer_question" {
					_, serviceErr := service.ResolveSourceFence(t.Context(), processing.SourceFenceResolveRequest{
						Filters: &store.SearchOptions{UnderPath: tc.path, UnderNodeID: tc.id},
					})
					t.Logf("source fence service error=%T: %v invalid_scope=%t invalid_fence=%t not_found=%t not_dir=%t", serviceErr, serviceErr,
						errors.Is(serviceErr, store.ErrInvalidSearchScope), errors.Is(serviceErr, store.ErrInvalidProcessingSourceFence),
						errors.Is(serviceErr, store.ErrNotFound), errors.Is(serviceErr, store.ErrNotDir))
					client, err := server.daemon.acquire(t.Context())
					require.NoError(t, err)
					_, httpErr := client.daemonconn.ResolveDocumentSourceFence(t.Context(), api.DocumentSourceFenceResolveRequest{
						Filters: &api.DocumentSourceFenceFilters{UnderPath: tc.path, UnderNodeID: tc.id},
					})
					status, received := daemonconn.ResponseStatus(httpErr)
					facts, hasFacts := daemonconn.ExtractProblemFacts(httpErr)
					t.Logf("source fence daemon before MCP error=%T: %v status=%d received=%t problem=%+v has_problem=%t decode_error=%t",
						httpErr, httpErr, status, received, facts, hasFacts, daemonconn.IsResponseDecodeError(httpErr))
					for cause := httpErr; cause != nil; cause = errors.Unwrap(cause) {
						t.Logf("source fence daemon error chain=%T: %v", cause, cause)
					}
					if decoded, ok := errors.AsType[*apiruntime.ResponseDecodeError](httpErr); ok {
						t.Logf("source fence daemon response decode status=%d body=%s", decoded.StatusCode, decoded.Body)
					}
				}
				filters := map[string]any{"under_path": tc.path}
				if tc.id != 0 {
					filters["under_node_id"] = tc.id
				}
				query := "query"
				if tool == "answer_question" {
					query = "question"
				}
				args := map[string]any{query: "mercury", "profile": "text", "filters": filters}
				if tool == "search_documents" {
					args["mode"] = "semantic"
				}
				raw := exchangeRaw(t, server, requestFor("tools/call", map[string]any{"name": tool, "arguments": args}))
				t.Logf("scope diagnostic MCP envelope=%s", raw)
				out := objectField(t, decodeResult(t, raw), "structuredContent")
				require.Equal(t, tc.code, out["code"], "%s %s", tool, tc.path)
				require.Zero(t, provider.queries.Load())
			})
		}
	}
}

func TestSearchUnderPathRejectsInvalidToolPaths(t *testing.T) {
	for _, tool := range []string{"search_documents", "answer_question", "find_conflicts"} {
		for _, path := range []string{"relative", "/" + strings.Repeat("é", 8192)} {
			query := "query"
			if tool == "answer_question" {
				query = "question"
			}
			wire := callToolWire(t, tool, map[string]any{query: "mercury", "profile": "text", "filters": map[string]any{"under_path": path}})
			require.EqualValues(t, jsonrpc.CodeInvalidParams, decodeWireError(t, wire).Code, tool)
		}
	}
}
