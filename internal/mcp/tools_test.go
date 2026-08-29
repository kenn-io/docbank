package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/client"
	"go.kenn.io/docbank/internal/store"
)

func TestDefaultToolCatalogIsFixedBoundedAndReadOnly(t *testing.T) {
	tools := toolCatalog(false)
	wantNames := []string{
		"get_vault_info", "list_documents", "search_documents", "get_document",
		"list_document_versions", "read_rendition_text", "get_processing_plan",
		"get_processing_status", "get_processing_coverage",
	}
	require.Len(t, tools, len(wantNames))
	for index, tool := range tools {
		assert.Equal(t, wantNames[index], tool.Name)
		require.NotNil(t, tool.Annotations)
		assert.True(t, tool.Annotations.ReadOnlyHint)
		assert.True(t, tool.Annotations.IdempotentHint)
		assert.Equal(t, new(false), tool.Annotations.DestructiveHint)
		assert.Equal(t, new(false), tool.Annotations.OpenWorldHint)
		assertSchemaContract(t, tool.InputSchema, true)
		assertSchemaContract(t, tool.OutputSchema, true)
		assert.Equal(t, map[string]any{"maxResponseBytes": maxToolResponseBytes}, tool.Meta["io.docbank/bounds"])
	}
	assert.NotContains(t, catalogNames(tools), "start_processing")
}

func TestProcessingToolIsConstructionTimeOptIn(t *testing.T) {
	readOnly := catalogNames(toolCatalog(false))
	enabledTools := toolCatalog(true)
	enabled := catalogNames(enabledTools)
	require.Equal(t, append(append([]string{}, readOnly...), "start_processing"), enabled)

	write := enabledTools[len(enabledTools)-1]
	require.NotNil(t, write.Annotations)
	assert.False(t, write.Annotations.ReadOnlyHint)
	assert.False(t, write.Annotations.IdempotentHint)
	assert.Equal(t, new(false), write.Annotations.DestructiveHint)
	assert.Equal(t, new(true), write.Annotations.OpenWorldHint)

	for _, allowProcessing := range []bool{false, true} {
		server := newServerWithOptions(testImplementation(), ServerOptions{AllowProcessing: allowProcessing})
		discovery := decodeResult(t, exchangeRaw(t, server, requestFor("server/discover", nil)))
		capabilities := objectField(t, discovery, "capabilities")
		assert.Equal(t, map[string]any{}, objectField(t, capabilities, "tools"))
		assert.NotContains(t, objectField(t, capabilities, "tools"), "listChanged")
		assert.Equal(t, catalogInstructions(allowProcessing), discovery["instructions"])

		listed := decodeResult(t, exchangeRaw(t, server, requestFor("tools/list", map[string]any{})))
		wantListed := catalogNames(toolCatalog(allowProcessing))
		slices.Sort(wantListed)
		assert.Equal(t, wantListed, listedToolNames(t, listed))
		assert.EqualValues(t, toolCatalogTTLMs, listed["ttlMs"])
		assert.Equal(t, "public", listed["cacheScope"])
		assert.Equal(t, "complete", listed["resultType"])
		assert.Empty(t, listed["nextCursor"])
	}
}

func TestToolsListTransmitsRegisteredSchemasAnnotationsAndBounds(t *testing.T) {
	server := newServerWithOptions(testImplementation(), ServerOptions{AllowProcessing: true})
	listed := decodeResult(t, exchangeRaw(t, server, requestFor("tools/list", map[string]any{})))
	assert.EqualValues(t, toolCatalogTTLMs, listed["ttlMs"])
	assert.Equal(t, "public", listed["cacheScope"])
	assert.Equal(t, "complete", listed["resultType"])
	assert.Empty(t, listed["nextCursor"])
	wireTools := listedToolsByName(t, listed)
	registered := catalogMap(toolCatalog(true))
	require.Len(t, wireTools, len(registered))

	for name, want := range registered {
		got := wireTools[name]
		require.NotNil(t, got, "tools/list omitted %s", name)
		assertJSONValueEqual(t, want.InputSchema, got["inputSchema"], name+" input schema")
		assertJSONValueEqual(t, want.OutputSchema, got["outputSchema"], name+" output schema")
		assertJSONValueEqual(t, want.Annotations, got["annotations"], name+" annotations")
		assertJSONValueEqual(t, want.Meta, got["_meta"], name+" metadata")
		meta := objectField(t, got, "_meta")
		assert.EqualValues(t, maxToolResponseBytes,
			objectField(t, meta, "io.docbank/bounds")["maxResponseBytes"])
	}
}

func TestRegisteredToolsRejectUnknownArgumentsBeforeHandlers(t *testing.T) {
	response := exchangeRaw(t, newTestServer(), requestFor("tools/call", map[string]any{
		"name": "list_documents", "arguments": map[string]any{"unknown": "synthetic"},
	}))
	wireErr := decodeWireError(t, response)
	assert.Equal(t, int64(jsonrpc.CodeInvalidParams), wireErr.Code)
	assert.Equal(t, "invalid tool arguments", wireErr.Message)
}

func TestRegisteredToolsEnforceDaemonIdentityAndTimeGrammars(t *testing.T) {
	validUUID := "00000000-0000-4000-8000-000000000007"
	invalidUUIDs := []string{
		"00000000-0000-5000-8000-000000000007",
		"00000000-0000-4000-7000-000000000007",
		"00000000-0000-4000-8000-00000000000G",
		"00000000-0000-4000-8000-00000000007",
	}
	for _, value := range invalidUUIDs {
		t.Run("invalid UUID "+value, func(t *testing.T) {
			wireErr := callToolWireError(t, "get_document", map[string]any{
				"node_id": 7, "content_version_id": value,
			})
			assert.Equal(t, int64(jsonrpc.CodeInvalidParams), wireErr.Code)
		})
	}
	validIdentity := callToolWireError(t, "get_document", map[string]any{
		"node_id": 7, "content_version_id": validUUID,
	})
	assert.Equal(t, int64(jsonrpc.CodeInternalError), validIdentity.Code,
		"valid identity must reach the Task 6 handler boundary")

	for _, value := range []string{"not-a-date", "2026-02-30T00:00:00Z", "2026-08-28T00:00:00"} {
		t.Run("invalid date "+value, func(t *testing.T) {
			wireErr := callToolWireError(t, "search_documents", map[string]any{
				"query": "synthetic", "profile": "local",
				"filters": map[string]any{"modified_since": value},
			})
			assert.Equal(t, int64(jsonrpc.CodeInvalidParams), wireErr.Code)
		})
	}
	validTime := callToolWireError(t, "search_documents", map[string]any{
		"query": "synthetic", "profile": "local",
		"filters": map[string]any{"modified_since": "2026-08-28T01:02:03.123456789+02:30"},
	})
	assert.Equal(t, int64(jsonrpc.CodeInternalError), validTime.Code,
		"absolute RFC3339Nano input must reach the Task 6 handler boundary")
}

func TestRegisteredToolsEnforceDaemonByteBounds(t *testing.T) {
	tooManyPathBytes := "/" + strings.Repeat("é", maxPathCharacters/2)
	require.LessOrEqual(t, len([]rune(tooManyPathBytes)), maxPathCharacters)
	require.Greater(t, len(tooManyPathBytes), maxPathBytes)
	pathErr := callToolWireError(t, "list_documents", map[string]any{"path_prefix": tooManyPathBytes})
	assert.Equal(t, int64(jsonrpc.CodeInvalidParams), pathErr.Code)

	validPath := "/" + strings.Repeat("x", maxPathBytes-1)
	validPathErr := callToolWireError(t, "list_documents", map[string]any{"path_prefix": validPath})
	assert.Equal(t, int64(jsonrpc.CodeInternalError), validPathErr.Code)

	tooManyCursorBytes := strings.Repeat("é", maxCursorCharacters/2+1)
	require.LessOrEqual(t, len([]rune(tooManyCursorBytes)), maxCursorCharacters)
	require.Greater(t, len(tooManyCursorBytes), maxCursorBytes)
	cursorErr := callToolWireError(t, "list_documents", map[string]any{"cursor": tooManyCursorBytes})
	assert.Equal(t, int64(jsonrpc.CodeInvalidParams), cursorErr.Code)

	validCursorErr := callToolWireError(t, "list_documents", map[string]any{
		"cursor": strings.Repeat("a", maxCursorBytes-2) + ".a",
	})
	assert.Equal(t, int64(jsonrpc.CodeInternalError), validCursorErr.Code)
}

func TestToolSchemasPinInputsBoundsAndStableIdentities(t *testing.T) {
	tools := catalogMap(toolCatalog(true))

	assertSchemaAccepts(t, tools["get_vault_info"].InputSchema, map[string]any{})
	assertSchemaRejects(t, tools["get_vault_info"].InputSchema, map[string]any{"extra": true})

	list := tools["list_documents"]
	assertSchemaAccepts(t, list.InputSchema, map[string]any{"path_prefix": "/synthetic", "page_size": 250})
	assertSchemaRejects(t, list.InputSchema, map[string]any{"page_size": 251})
	assertSchemaRejects(t, list.InputSchema, map[string]any{"cursor": strings.Repeat("c", 2049)})
	assertSchemaRejects(t, list.InputSchema, map[string]any{"unknown": "value"})
	assertSchemaRejects(t, list.OutputSchema, map[string]any{
		"path_prefix": "/", "sort": "path", "direction": "asc", "page_size": 50,
		"items": []any{}, "next_cursor": "é.a", "ttlMs": 0, "cacheScope": "private",
	})
	assertSchemaRejects(t, tools["get_document"].InputSchema, map[string]any{
		"node_id": 7, "content_version_id": "00000000-0000-5000-8000-000000000007",
	})
	assertSchemaAccepts(t, list.OutputSchema, map[string]any{
		"path_prefix": "/", "sort": "path", "direction": "asc", "page_size": 50,
		"items": []any{map[string]any{
			"node_id": 7, "content_version_id": "00000000-0000-4000-8000-000000000007",
			"path": "/synthetic.txt", "name": "synthetic.txt", "media_type": "text/plain",
			"size": 12, "modified_at": "2026-08-28T00:00:00Z", "active_renditions": []any{},
		}}, "ttlMs": 0, "cacheScope": "private",
	})

	search := tools["search_documents"]
	assertSchemaAccepts(t, search.InputSchema, map[string]any{
		"query": "synthetic", "profile": "local", "content_version_ids": []any{"00000000-0000-4000-8000-000000000007"},
	})
	assertSchemaRejects(t, search.InputSchema, map[string]any{
		"query": strings.Repeat("q", 8193), "profile": "local", "content_version_ids": []any{"00000000-0000-4000-8000-000000000007"},
	})
	assertSchemaRejects(t, search.InputSchema, map[string]any{
		"query": "synthetic", "profile": "local", "content_version_ids": make([]any, 4097),
	})
	assertSchemaRejects(t, search.InputSchema, map[string]any{
		"query": "synthetic", "profile": "local", "filters": map[string]any{"modified_since": "not-a-date"},
	})

	read := tools["read_rendition_text"]
	assertSchemaAccepts(t, read.InputSchema, map[string]any{
		"vault_id": "00000000-0000-4000-8000-000000000001", "node_id": 7,
		"content_version_id": "00000000-0000-4000-8000-000000000007",
		"attachment_id":      strings.Repeat("a", 64), "offset": 0, "max_chars": 16000,
	})
	assertSchemaRejects(t, read.InputSchema, map[string]any{
		"vault_id": "00000000-0000-4000-8000-000000000001", "node_id": 7,
		"content_version_id": "00000000-0000-4000-8000-000000000007",
		"attachment_id":      strings.Repeat("a", 64), "max_chars": 16001,
	})
	assertSchemaAccepts(t, read.OutputSchema, map[string]any{
		"vault_id": "00000000-0000-4000-8000-000000000001", "node_id": 7,
		"content_version_id": "00000000-0000-4000-8000-000000000007",
		"attachment_id":      strings.Repeat("a", 64), "build_id": strings.Repeat("b", 64),
		"profile_fingerprint": strings.Repeat("c", 64), "text": "synthetic", "media_type": "text/markdown",
		"checksum": strings.Repeat("d", 64), "requested_offset": 0, "actual_start": 0,
		"actual_end": 9, "next_offset": 9, "eof": true, "response_bytes": 9,
		"ttlMs": 0, "cacheScope": "private",
	})
	assertSchemaRejects(t, read.OutputSchema, map[string]any{
		"vault_id": "00000000-0000-4000-8000-000000000001", "node_id": 7,
		"content_version_id": "00000000-0000-4000-8000-000000000007",
		"attachment_id":      strings.Repeat("a", 64), "build_id": strings.Repeat("b", 64),
		"profile_fingerprint": strings.Repeat("c", 64), "text": strings.Repeat("x", 16001),
		"media_type": "text/markdown", "checksum": strings.Repeat("d", 64),
		"requested_offset": 0, "actual_start": 0, "actual_end": 16001,
		"next_offset": 16001, "eof": false, "response_bytes": 16001,
		"ttlMs": 0, "cacheScope": "private",
	})

	start := tools["start_processing"]
	assertSchemaAccepts(t, start.InputSchema, map[string]any{
		"content_version_id": "00000000-0000-4000-8000-000000000007",
		"plan_fingerprint":   strings.Repeat("e", 64),
	})
	assertSchemaRejects(t, start.InputSchema, map[string]any{
		"content_version_id": "00000000-0000-4000-8000-000000000007",
		"plan_fingerprint":   strings.Repeat("e", 64), "consent": true,
	})

	plan := tools["get_processing_plan"]
	assertSchemaAccepts(t, plan.OutputSchema, authoritativeProcessingPlanFixture())
	missingDisclosure := authoritativeProcessingPlanFixture()
	delete(missingDisclosure, "backup_consequence")
	assertSchemaRejects(t, plan.OutputSchema, missingDisclosure)

	for name, tool := range tools {
		assertOutputIdentityProperties(t, name, tool.OutputSchema)
	}
}

func TestExpectedDomainErrorsAreBoundedToolResults(t *testing.T) {
	tests := []struct {
		name      string
		err       error
		code      string
		redaction string
	}{
		{name: "not found", err: store.ErrNotFound, code: "not_found"},
		{name: "stale version", err: store.ErrProcessingSourceFenceStaleVersion, code: "stale_version"},
		{name: "consent", err: client.ErrProcessingConsent, code: "consent_required"},
		{name: "cursor", err: store.ErrDocumentCursorExpired, code: "cursor_expired"},
		{name: "cursor capacity", err: fmt.Errorf("private cursor capacity detail: %w", store.ErrDocumentCursorCapacity),
			code: "cursor_capacity", redaction: "private cursor capacity detail"},
		{name: "invalid cursor", err: fmt.Errorf("private cursor detail: %w", store.ErrInvalidDocumentCursor),
			code: "invalid_document_cursor", redaction: "private cursor detail"},
		{name: "scope", err: &client.SourceFenceScopeTooLargeError{ObservedScopeCount: 4097}, code: "scope_too_large"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result, ok := domainToolError(test.err)
			require.True(t, ok)
			require.NotNil(t, result)
			assert.True(t, result.IsError)
			structured, structuredOK := result.StructuredContent.(toolErrorOutput)
			require.True(t, structuredOK)
			assert.Equal(t, test.code, structured.Code)
			assert.NotContains(t, structured.Message, "/home/private")
			encoded, err := json.Marshal(result)
			require.NoError(t, err)
			assert.LessOrEqual(t, len(encoded), maxToolErrorBytes)
			if test.redaction != "" {
				assert.NotContains(t, string(encoded), test.redaction)
			}
		})
	}

	secret := errors.New("unexpected /home/private key=secret document text")
	_, ok := domainToolError(secret)
	assert.False(t, ok)
	rpcErr := sanitizedRPCError(secret)
	assert.Equal(t, int64(jsonrpc.CodeInternalError), rpcErr.Code)
	assert.Equal(t, "internal Docbank MCP error", rpcErr.Message)
	assert.NotContains(t, fmt.Sprint(rpcErr.Data), "private")
	assert.NotContains(t, fmt.Sprint(rpcErr.Data), "secret")
}

func authoritativeProcessingPlanFixture() map[string]any {
	plan := api.ProcessingPlan{
		Fingerprint: strings.Repeat("a", 64), VaultUID: "00000000-0000-4000-8000-000000000001",
		Selector: api.ProcessingSelector{NodeID: 7,
			ContentVersionID: "00000000-0000-4000-8000-000000000007", Profile: "local"},
		ProfileFingerprint: strings.Repeat("b", 64),
		Flow: []api.ProcessingFlowHop{{
			Capability: "rendition", ProviderID: "synthetic-provider", TrustBoundary: "local_process",
			InputClasses: []string{"original_file"}, RuntimeDisclosure: api.ProcessingRuntimeDisclosure{
				ImmediateProcessor: "local-worker", UltimateProcessor: "local-runtime",
				Endpoint: "in-process", Deployment: "synthetic", Model: "synthetic-model",
				ModelRevision: "v1", VectorSpace: "synthetic-space",
				MetadataClasses: []string{"filename"}, RetainedArtifactRoles: []string{"sanitized_markdown"},
			},
		}},
		DisclosedClasses: []string{"original_file"}, RetainedClasses: []string{"sanitized_markdown"},
		Estimate:        api.ProcessingEstimate{SourceBytes: 12, ProviderCalls: 1, VectorSpaces: 1},
		ConsentRequired: true, ConsentState: "required",
		BackupConsequence: "retained derivatives are included in backups",
	}
	data, err := json.Marshal(plan)
	if err != nil {
		panic(err)
	}
	var result map[string]any
	if err := json.Unmarshal(data, &result); err != nil {
		panic(err)
	}
	result["ttlMs"], result["cacheScope"] = 0, "private"
	return result
}

func assertSchemaContract(t *testing.T, raw any, object bool) {
	t.Helper()
	schema := schemaMap(t, raw)
	draft, ok := schema["$schema"].(string)
	require.True(t, ok)
	if draft != jsonSchemaDraft {
		t.Errorf("schema draft = %q, want %q", draft, jsonSchemaDraft)
	}
	if object {
		assert.Equal(t, "object", schema["type"])
		assert.Equal(t, false, schema["additionalProperties"])
	}
}

func assertSchemaAccepts(t *testing.T, raw any, value any) {
	t.Helper()
	resolved := resolvedSchema(t, raw)
	require.NoError(t, resolved.Validate(&value))
}

func assertSchemaRejects(t *testing.T, raw any, value any) {
	t.Helper()
	resolved := resolvedSchema(t, raw)
	require.Error(t, resolved.Validate(&value))
}

func resolvedSchema(t *testing.T, raw any) *jsonschema.Resolved {
	t.Helper()
	data, err := json.Marshal(raw)
	require.NoError(t, err)
	var schema jsonschema.Schema
	require.NoError(t, json.Unmarshal(data, &schema))
	resolved, err := schema.Resolve(&jsonschema.ResolveOptions{ValidateDefaults: true})
	require.NoError(t, err)
	return resolved
}

func schemaMap(t *testing.T, raw any) map[string]any {
	t.Helper()
	data, err := json.Marshal(raw)
	require.NoError(t, err)
	var schema map[string]any
	require.NoError(t, json.Unmarshal(data, &schema))
	return schema
}

func assertOutputIdentityProperties(t *testing.T, name string, raw any) {
	t.Helper()
	data, err := json.Marshal(raw)
	require.NoError(t, err)
	text := string(data)
	wants := map[string][]string{
		"get_vault_info":          {"vault_id"},
		"list_documents":          {"node_id", "content_version_id", "attachment_id"},
		"search_documents":        {"fence_fingerprint", "content_version_ids", "node_id"},
		"get_document":            {"node_id", "content_version_id", "attachment_id"},
		"list_document_versions":  {"node_id", "content_version_id"},
		"read_rendition_text":     {"node_id", "content_version_id", "attachment_id", "build_id"},
		"get_processing_plan":     {"content_version_id", "fingerprint"},
		"get_processing_status":   {"job_id"},
		"get_processing_coverage": {"vault_id", "content_version_ids", "profile_fingerprint"},
		"start_processing":        {"job_id", "content_version_id", "profile_fingerprint"},
	}
	for _, identity := range wants[name] {
		assert.Contains(t, text, `"`+identity+`"`, "%s output omits %s", name, identity)
	}
}

func catalogNames(tools []*sdkmcp.Tool) []string {
	names := make([]string, len(tools))
	for index, tool := range tools {
		names[index] = tool.Name
	}
	return names
}

func catalogMap(tools []*sdkmcp.Tool) map[string]*sdkmcp.Tool {
	result := make(map[string]*sdkmcp.Tool, len(tools))
	for _, tool := range tools {
		result[tool.Name] = tool
	}
	return result
}

func requestFor(method string, arguments map[string]any) []byte {
	params := map[string]any{
		"_meta": map[string]any{
			sdkmcp.MetaKeyProtocolVersion:    ProtocolVersion,
			sdkmcp.MetaKeyClientCapabilities: map[string]any{},
		},
	}
	maps.Copy(params, arguments)
	wire := map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params}
	data, err := json.Marshal(wire)
	if err != nil {
		panic(err)
	}
	return data
}

func decodeResult(t *testing.T, response []byte) map[string]any {
	t.Helper()
	var wire struct {
		Result map[string]any `json:"result"`
	}
	require.NoError(t, json.Unmarshal(response, &wire))
	require.NotNil(t, wire.Result, "response did not contain a result: %s", response)
	return wire.Result
}

func objectField(t *testing.T, object map[string]any, key string) map[string]any {
	t.Helper()
	value, ok := object[key].(map[string]any)
	require.True(t, ok, "%s is %T", key, object[key])
	return value
}

func listedToolNames(t *testing.T, result map[string]any) []string {
	t.Helper()
	values, ok := result["tools"].([]any)
	require.True(t, ok)
	names := make([]string, 0, len(values))
	for _, value := range values {
		tool, ok := value.(map[string]any)
		require.True(t, ok)
		name, ok := tool["name"].(string)
		require.True(t, ok)
		names = append(names, name)
	}
	return names
}

func listedToolsByName(t *testing.T, result map[string]any) map[string]map[string]any {
	t.Helper()
	values, ok := result["tools"].([]any)
	require.True(t, ok)
	tools := make(map[string]map[string]any, len(values))
	for _, value := range values {
		tool, ok := value.(map[string]any)
		require.True(t, ok)
		name, ok := tool["name"].(string)
		require.True(t, ok)
		tools[name] = tool
	}
	return tools
}

func assertJSONValueEqual(t *testing.T, want, got any, label string) {
	t.Helper()
	wantJSON, err := json.Marshal(want)
	require.NoError(t, err)
	gotJSON, err := json.Marshal(got)
	require.NoError(t, err)
	assert.JSONEq(t, string(wantJSON), string(gotJSON), label)
}

func callToolWireError(t *testing.T, name string, arguments map[string]any) *jsonrpc.Error {
	t.Helper()
	lease := newDaemonLeaseWith(func(context.Context) (*client.Client, error) {
		return nil, errors.New("synthetic daemon unavailable")
	}, func(*client.Client) error { return nil })
	response := exchangeRaw(t, newServerWithOptionsAndDaemon(testImplementation(),
		ServerOptions{AllowProcessing: true}, lease),
		requestFor("tools/call", map[string]any{"name": name, "arguments": arguments}))
	return decodeWireError(t, response)
}
