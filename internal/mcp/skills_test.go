package mcp

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/agentguide"
	"go.kenn.io/docbank/internal/daemonconn"
)

var guideHashes = map[string]string{
	"workflow":   "caa78c99272ce8f666e78149fce86e67f0acef49ad491aa0f1f3b65b7b30fa59",
	"search":     "61808623b698b2a8ea2b626af5a02a020fa6b83f6f848898d46508113383718e",
	"processing": "705ad101f75783cdb3fc791a6231e74f46b23451231f93e80de7068bb12cfdc0",
}

// Every exchange owns and joins its connection, including property-test draws.
// It returns errors so Hegel can record and shrink unexpected protocol failures.
func skillsExchange(ctx context.Context, server *Server, request []byte) ([]byte, error) {
	serverReader, clientWriter := io.Pipe()
	clientReader, serverWriter := io.Pipe()
	ctx, cancel := context.WithCancel(ctx)
	closePipes := func() {
		_ = serverReader.Close()
		_ = clientWriter.Close()
		_ = clientReader.Close()
		_ = serverWriter.Close()
	}
	stop := context.AfterFunc(ctx, closePipes)
	done := make(chan error, 1)
	go func() { done <- server.Run(ctx, &sdkmcp.IOTransport{Reader: serverReader, Writer: serverWriter}) }()
	defer func() {
		cancel()
		closePipes()
		stop()
		<-done
	}()
	var compact bytes.Buffer
	if err := json.Compact(&compact, request); err != nil {
		return nil, err
	}
	compact.WriteByte('\n')
	if _, err := clientWriter.Write(compact.Bytes()); err != nil {
		return nil, err
	}
	response, err := bufio.NewReader(clientReader).ReadBytes('\n')
	return bytes.TrimSpace(response), err
}

func skillsRequest(ctx context.Context, t *testing.T, server *Server, request []byte) []byte {
	t.Helper()
	response, err := skillsExchange(ctx, server, request)
	require.NoError(t, err)
	return response
}

func skillsSuccess(ctx context.Context, server *Server, request []byte) (map[string]any, error) {
	response, err := skillsExchange(ctx, server, request)
	if err != nil {
		return nil, err
	}
	var wire struct {
		Result map[string]any `json:"result"`
		Error  *jsonrpc.Error `json:"error"`
	}
	if err := json.Unmarshal(response, &wire); err != nil {
		return nil, err
	}
	if wire.Error != nil || wire.Result == nil {
		return nil, fmt.Errorf("expected guide result: %s", response)
	}
	return wire.Result, nil
}

func skillsTestServer(t *testing.T, options ServerOptions) *Server {
	t.Helper()
	lease := newDaemonLeaseWith(func(context.Context) (*daemonconn.Connection, error) {
		t.Error("guide operation attempted daemon access")
		return nil, errors.New("synthetic unavailable daemon")
	}, func(*daemonconn.Connection) error { return nil })
	return newServerWithOptionsAndDaemon(testImplementation(), options, lease)
}

func skillsCall(ctx context.Context, t *testing.T, server *Server, arguments map[string]any) map[string]any {
	t.Helper()
	return decodeResult(t, skillsRequest(ctx, t, server, requestFor("tools/call", map[string]any{
		"name": "docbank.skills", "arguments": arguments,
	})))
}

func TestSkillsCatalogAndGuidesWithoutDaemon(t *testing.T) {
	server := skillsTestServer(t, ServerOptions{})
	catalog := skillsCall(t.Context(), t, server, map[string]any{})
	entries := objectField(t, catalog, "structuredContent")["skills"].([]any)
	wantEntries := []any{
		map[string]any{"name": "workflow", "title": "Docbank agent workflow", "summary": "Select exact document identities, find evidence and read verified content."},
		map[string]any{"name": "search", "title": "Find document evidence", "summary": "Search an explicit scope and interpret coverage before drawing conclusions."},
		map[string]any{"name": "processing", "title": "Review document processing", "summary": "Inspect provider disclosure and use existing consent for an exact reviewed plan."},
	}
	require.Equal(t, wantEntries, entries)
	require.Equal(t, "- workflow: Docbank agent workflow. Select exact document identities, find evidence and read verified content.\n"+
		"- search: Find document evidence. Search an explicit scope and interpret coverage before drawing conclusions.\n"+
		"- processing: Review document processing. Inspect provider disclosure and use existing consent for an exact reviewed plan.\n",
		catalog["content"].([]any)[0].(map[string]any)["text"])
	for index, name := range []string{"workflow", "search", "processing"} {
		entry := entries[index].(map[string]any)
		require.Equal(t, name, entry["name"])
		require.NotEmpty(t, entry["title"])
		require.NotEmpty(t, entry["summary"])
		guide := skillsCall(t.Context(), t, server, map[string]any{"name": name})
		require.Equal(t, []any{entry}, objectField(t, guide, "structuredContent")["skills"])
		content := guide["content"].([]any)
		require.Len(t, content, 1)
		text := content[0].(map[string]any)
		require.Equal(t, "text", text["type"])
		require.Contains(t, text["text"], "## MCP")
		require.Contains(t, text["text"], "## CLI guidance")
		require.Less(t, len(text["text"].(string)), maxToolResponseBytes)
		// Pin the reviewed authored bytes independently of catalog construction.
		require.Equal(t, guideHashes[name], fmt.Sprintf("%x", sha256.Sum256([]byte(text["text"].(string)))))
	}
	withoutArguments := decodeResult(t, skillsRequest(t.Context(), t, server, requestFor("tools/call", map[string]any{"name": "docbank.skills"})))
	require.Equal(t, catalog["structuredContent"], withoutArguments["structuredContent"])
}

func TestSkillsDefinitionIsIndependent(t *testing.T) {
	tool, err := ReadTool("docbank.skills")
	require.NoError(t, err)
	schemaMap(t, tool.InputSchema) // Also exercises the public JSON representation.
	tool.InputSchema.(map[string]any)["properties"].(map[string]any)["name"] = "changed"
	tool.OutputSchema.(map[string]any)["required"] = []string{"changed"}
	tool.Meta["io.docbank/bounds"].(map[string]any)["maxResponseBytes"] = 1
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
	require.EqualValues(t, maxToolResponseBytes, clean.Meta["io.docbank/bounds"].(map[string]any)["maxResponseBytes"])
	assertSchemaAccepts(t, clean.InputSchema, map[string]any{"name": "workflow"})
	assertSchemaRejects(t, clean.InputSchema, map[string]any{"name": ""})
	skillsCall(t.Context(), t, skillsTestServer(t, ServerOptions{}), map[string]any{"name": "workflow"})
}

func TestSkillsPreserveCatalogAcrossEveryOptionCombination(t *testing.T) {
	for mask := range 64 {
		options := ServerOptions{
			AllowProcessing: mask&1 != 0, AllowPackageWrites: mask&2 != 0,
			AllowPhotoEdits: mask&4 != 0, AllowExportWrites: mask&8 != 0,
			AllowReportWrites: mask&16 != 0, AllowKataWrites: mask&32 != 0,
		}
		server := skillsTestServer(t, options)
		before := decodeResult(t, skillsRequest(t.Context(), t, server, requestFor("tools/list", nil)))
		count := 0
		for _, name := range listedToolNames(t, before) {
			if name == "docbank.skills" {
				count++
			}
		}
		require.Equal(t, 1, count, "option mask %d", mask)
		for _, name := range []string{"workflow", "search", "processing"} {
			skillsCall(t.Context(), t, server, map[string]any{"name": name})
		}
		after := decodeResult(t, skillsRequest(t.Context(), t, server, requestFor("tools/list", nil)))
		require.Equal(t, before, after, "guide read changed option mask %d", mask)
		require.Empty(t, server.plans.entries, "guide read must not review a processing plan")
	}
}

func TestSkillsUnknownNameErrorIsBounded(t *testing.T) {
	server := skillsTestServer(t, ServerOptions{})
	response := skillsRequest(t.Context(), t, server, requestFor("tools/call", map[string]any{
		"name": "docbank.skills", "arguments": map[string]any{"name": strings.Repeat("a", 64<<10)},
	}))
	require.Equal(t, int64(jsonrpc.CodeInvalidParams), decodeWireError(t, response).Code)
	require.Less(t, len(response), maxToolErrorBytes)
}

func TestSkillsMappingsNameRegisteredTools(t *testing.T) {
	all := catalogMap(toolCatalog(ServerOptions{
		AllowProcessing: true, AllowPackageWrites: true, AllowPhotoEdits: true,
		AllowExportWrites: true, AllowReportWrites: true, AllowKataWrites: true,
	}))
	row := regexp.MustCompile("(?m)^\\|[^|]+\\| `([^`]+)` \\|")
	for _, guide := range agentguide.Guides() {
		for _, match := range row.FindAllStringSubmatch(guide.Markdown, -1) {
			require.Contains(t, all, match[1], "%s guide maps an unavailable native tool", guide.Name)
		}
	}
}

func TestSkillsRejectInvalidArguments(t *testing.T) {
	server := skillsTestServer(t, ServerOptions{})
	for _, raw := range []string{
		`null`, `[]`, `"workflow"`, `{"name":null}`, `{"name":false}`,
		`{"name":1}`, `{"name":""}`, `{"name":" workflow"}`,
		`{"name":"unknown"}`, `{"name":"../../guide.md"}`, `{"path":"workflow"}`,
		`{"name":"workflow","unknown":true}`,
	} {
		t.Run(raw, func(t *testing.T) {
			request := requestFor("tools/call", map[string]any{"name": "docbank.skills", "arguments": json.RawMessage(raw)})
			failure := decodeWireError(t, skillsRequest(t.Context(), t, server, request))
			require.Equal(t, int64(jsonrpc.CodeInvalidParams), failure.Code)
		})
	}
}

func TestSkillsRejectDuplicateJSONBeforeDispatch(t *testing.T) {
	server := skillsTestServer(t, ServerOptions{})
	request := requestFor("tools/call", map[string]any{
		"name": "docbank.skills", "arguments": json.RawMessage(`{"name":"workflow","name":"search"}`),
	})
	response := skillsRequest(t.Context(), t, server, request)
	// The existing exact-version gate cannot decode metadata from duplicate-key
	// JSON. It rejects the frame before the skills handler can inspect arguments.
	require.Equal(t, int64(sdkmcp.CodeUnsupportedProtocolVersion), decodeWireError(t, response).Code)
	require.NotContains(t, string(response), `"result"`)
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
