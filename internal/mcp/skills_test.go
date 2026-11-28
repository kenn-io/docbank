package mcp

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/daemonconn"
)

func skillsTestServer(t *testing.T, options ServerOptions) *Server {
	t.Helper()
	lease := newDaemonLeaseWith(func(context.Context) (*daemonconn.Connection, error) {
		t.Error("guide operation attempted daemon access")
		return nil, errors.New("synthetic unavailable daemon")
	}, func(*daemonconn.Connection) error { return nil })
	return newServerWithOptionsAndDaemon(testImplementation(), options, lease)
}

func skillsCall(t *testing.T, server *Server, arguments map[string]any) map[string]any {
	t.Helper()
	return decodeResult(t, exchangeRaw(t, server, requestFor("tools/call", map[string]any{
		"name": "docbank.skills", "arguments": arguments,
	})))
}

func TestSkillsCatalogAndGuidesWithoutDaemon(t *testing.T) {
	server := skillsTestServer(t, ServerOptions{})
	catalog := skillsCall(t, server, map[string]any{})
	entries, ok := objectField(t, catalog, "structuredContent")["skills"].([]any)
	require.True(t, ok)
	require.Len(t, entries, 3)
	for index, name := range []string{"workflow", "search", "processing"} {
		entry, ok := entries[index].(map[string]any)
		require.True(t, ok)
		require.Equal(t, name, entry["name"])
		require.NotEmpty(t, entry["title"])
		require.NotEmpty(t, entry["summary"])
		guide := skillsCall(t, server, map[string]any{"name": name})
		require.Equal(t, []any{entry}, objectField(t, guide, "structuredContent")["skills"])
		content, ok := guide["content"].([]any)
		require.True(t, ok)
		require.Len(t, content, 1)
		text, ok := content[0].(map[string]any)
		require.True(t, ok)
		require.Equal(t, "text", text["type"])
		require.Contains(t, text["text"], []string{CLIWorkflowHelp, CLISearchHelp, CLIProcessingHelp}[index])
		require.Contains(t, text["text"], "## CLI guidance")
		body, ok := text["text"].(string)
		require.True(t, ok)
		require.Less(t, len(body), maxToolResponseBytes)
	}
	withoutArguments := decodeResult(t, exchangeRaw(t, server, requestFor("tools/call", map[string]any{"name": "docbank.skills"})))
	require.Equal(t, catalog["structuredContent"], withoutArguments["structuredContent"])
}

func TestSkillsRegisteredAcrossEveryOptionCombination(t *testing.T) {
	for mask := range 64 {
		options := ServerOptions{
			AllowProcessing: mask&1 != 0, AllowPackageWrites: mask&2 != 0,
			AllowPhotoEdits: mask&4 != 0, AllowExportWrites: mask&8 != 0,
			AllowReportWrites: mask&16 != 0, AllowKataWrites: mask&32 != 0,
		}
		server := skillsTestServer(t, options)
		before := decodeResult(t, exchangeRaw(t, server, requestFor("tools/list", nil)))
		count := 0
		for _, name := range listedToolNames(t, before) {
			if name == "docbank.skills" {
				count++
			}
		}
		require.Equal(t, 1, count, "option mask %d", mask)
	}
}

func TestSkillsUnknownName(t *testing.T) {
	server := skillsTestServer(t, ServerOptions{})
	result := skillsCall(t, server, map[string]any{"name": "unknown"})
	require.Equal(t, true, result["isError"])
}

func TestSkillsRejectMalformedArguments(t *testing.T) {
	server := skillsTestServer(t, ServerOptions{})
	for _, args := range []map[string]any{{"name": false}, {"path": "workflow"}} {
		response := exchangeRaw(t, server, requestFor("tools/call", map[string]any{"name": "docbank.skills", "arguments": args}))
		require.EqualValues(t, jsonrpc.CodeInvalidParams, decodeWireError(t, response).Code)
	}
}

func TestSkillsHTTPAuthenticatedCall(t *testing.T) {
	server := skillsTestServer(t, ServerOptions{})
	handler, err := server.HTTPTransportHandler(HTTPOptions{BearerToken: testMCPBearer})
	require.NoError(t, err)
	request := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "http://127.0.0.1/mcp",
		bytes.NewReader(requestFor("tools/call", map[string]any{"name": "docbank.skills", "arguments": map[string]any{}})))
	request.Header = protocolHeaders("tools/call", "docbank.skills")
	request.Header.Set("Authorization", "Bearer "+testMCPBearer)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	result := decodeResult(t, response.Body.Bytes())
	entries, ok := objectField(t, result, "structuredContent")["skills"].([]any)
	require.True(t, ok)
	require.Len(t, entries, 3)
}
