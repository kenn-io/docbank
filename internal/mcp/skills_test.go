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
	"go.kenn.io/docbank/internal/agentguide"
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
		require.Contains(t, text["text"], []string{agentguide.Workflow, agentguide.Search, agentguide.Processing}[index])
		require.Contains(t, text["text"], "## CLI guidance")
		body, ok := text["text"].(string)
		require.True(t, ok)
		require.Less(t, len(body), maxToolResponseBytes)
	}
	withoutArguments := decodeResult(t, exchangeRaw(t, server, requestFor("tools/call", map[string]any{"name": "docbank.skills"})))
	require.Equal(t, catalog["structuredContent"], withoutArguments["structuredContent"])
}

func TestSkillsDefinitionIsIndependent(t *testing.T) {
	tool, err := ReadTool("docbank.skills")
	require.NoError(t, err)
	schemaMap(t, tool.InputSchema) // Also exercises the public JSON representation.
	input, ok := tool.InputSchema.(map[string]any)
	require.True(t, ok)
	objectField(t, input, "properties")["name"] = "changed"
	output, ok := tool.OutputSchema.(map[string]any)
	require.True(t, ok)
	output["required"] = []string{"changed"}
	objectField(t, tool.Meta, "io.docbank/bounds")["maxResponseBytes"] = 1
	tool.Annotations.ReadOnlyHint = false
	clean, err := ReadTool("docbank.skills")
	require.NoError(t, err)
	assertSchemaContract(t, clean.InputSchema)
	assertSchemaContract(t, clean.OutputSchema)
	assertSchemaAccepts(t, clean.OutputSchema, map[string]any{"skills": []any{map[string]any{"name": "workflow", "title": "Workflow", "summary": "Read first."}}})
	assertSchemaRejects(t, clean.OutputSchema, map[string]any{})
	require.True(t, clean.Annotations.ReadOnlyHint)
	require.Equal(t, new(false), clean.Annotations.DestructiveHint)
	require.Equal(t, new(false), clean.Annotations.OpenWorldHint)
	require.EqualValues(t, maxToolResponseBytes, objectField(t, clean.Meta, "io.docbank/bounds")["maxResponseBytes"])
	assertSchemaAccepts(t, clean.InputSchema, map[string]any{"name": "workflow"})
	assertSchemaRejects(t, clean.InputSchema, map[string]any{"name": ""})
	skillsCall(t, skillsTestServer(t, ServerOptions{}), map[string]any{"name": "workflow"})
}

func TestSkillsPreserveCatalogAcrossEveryOptionCombination(t *testing.T) {
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
		for _, name := range []string{"workflow", "search", "processing"} {
			skillsCall(t, server, map[string]any{"name": name})
		}
		after := decodeResult(t, exchangeRaw(t, server, requestFor("tools/list", nil)))
		require.Equal(t, before, after, "guide read changed option mask %d", mask)
		require.Empty(t, server.plans.entries, "guide read must not review a processing plan")
	}
}

func TestSkillsUnknownName(t *testing.T) {
	server := skillsTestServer(t, ServerOptions{})
	result := skillsCall(t, server, map[string]any{"name": "unknown"})
	require.Equal(t, true, result["isError"])
	content, ok := result["content"].([]any)
	require.True(t, ok)
	require.Len(t, content, 1)
	item, ok := content[0].(map[string]any)
	require.True(t, ok)
	require.Contains(t, item["text"], "unknown guide")
}

func TestSkillsRejectMalformedArguments(t *testing.T) {
	server := skillsTestServer(t, ServerOptions{})
	for _, args := range []map[string]any{{"name": false}, {"path": "workflow"}} {
		response := exchangeRaw(t, server, requestFor("tools/call", map[string]any{"name": "docbank.skills", "arguments": args}))
		require.EqualValues(t, jsonrpc.CodeInvalidParams, decodeWireError(t, response).Code)
	}
}

func TestSkillsHTTPKeepsBearerBoundary(t *testing.T) {
	server := skillsTestServer(t, ServerOptions{})
	handler, err := server.HTTPTransportHandler(HTTPOptions{BearerToken: testMCPBearer})
	require.NoError(t, err)
	for _, authorized := range []bool{false, true} {
		request := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "http://127.0.0.1/mcp",
			bytes.NewReader(requestFor("tools/call", map[string]any{"name": "docbank.skills", "arguments": map[string]any{}})))
		request.Header = protocolHeaders("tools/call", "docbank.skills")
		if authorized {
			request.Header.Set("Authorization", "Bearer "+testMCPBearer)
		}
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if authorized {
			require.Equal(t, http.StatusOK, response.Code, response.Body.String())
			require.Contains(t, response.Body.String(), "workflow")
		} else {
			require.Equal(t, http.StatusUnauthorized, response.Code)
			require.NotContains(t, response.Body.String(), "workflow")
		}
	}
}
