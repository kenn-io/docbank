package mcp_test

import (
	"context"
	"sync/atomic"
	"testing"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	catalog "go.kenn.io/docbank/mcp"
)

func TestReadToolRejectsUnavailableOperations(t *testing.T) {
	for _, name := range []string{"", "unknown", "start_processing", "start_package_import", "create_photo_asset", "download_export"} {
		t.Run(name, func(t *testing.T) {
			tool, err := catalog.ReadTool(name)
			require.Error(t, err)
			assert.Nil(t, tool)
		})
	}
}

func TestReadToolIndependentSDKContract(t *testing.T) {
	// An embedder may customize its copy without changing other servers.
	customized, err := catalog.ReadTool("list_documents")
	require.NoError(t, err)
	input, ok := customized.InputSchema.(map[string]any)
	require.True(t, ok)
	properties, ok := input["properties"].(map[string]any)
	require.True(t, ok)
	limit, ok := properties["page_size"].(map[string]any)
	require.True(t, ok)
	limit["type"] = "boolean"
	output, ok := customized.OutputSchema.(map[string]any)
	require.True(t, ok)
	output["required"] = []string{"changed"}
	customized.Annotations.ReadOnlyHint = false

	tool, err := catalog.ReadTool("list_documents")
	require.NoError(t, err)
	server := sdkmcp.NewServer(&sdkmcp.Implementation{Name: "catalog-test", Version: "1"}, nil)
	var calls atomic.Int32
	sdkmcp.AddTool(server, tool, func(_ context.Context, _ *sdkmcp.CallToolRequest, args map[string]any) (*sdkmcp.CallToolResult, map[string]any, error) {
		calls.Add(1)
		if args["page_size"] == float64(3) {
			return nil, map[string]any{"invalid": true}, nil
		}
		return nil, map[string]any{"path_prefix": "/", "sort": "path", "direction": "asc", "page_size": 2, "items": []any{}, "ttlMs": 0, "cacheScope": "private"}, nil
	})

	clientTransport, serverTransport := sdkmcp.NewInMemoryTransports()
	serverSession, err := server.Connect(t.Context(), serverTransport, nil)
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, serverSession.Close()) })
	client := sdkmcp.NewClient(&sdkmcp.Implementation{Name: "catalog-client", Version: "1"}, nil)
	session, err := client.Connect(t.Context(), clientTransport, nil)
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, session.Close()) })

	listed, err := session.ListTools(t.Context(), nil)
	require.NoError(t, err)
	require.Len(t, listed.Tools, 1)
	assert.Equal(t, "list_documents", listed.Tools[0].Name)
	require.NotNil(t, listed.Tools[0].Annotations)
	assert.True(t, listed.Tools[0].Annotations.ReadOnlyHint)

	result, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{
		Name: "list_documents", Arguments: map[string]any{"page_size": 2},
	})
	require.NoError(t, err)
	require.False(t, result.IsError)
	require.Len(t, result.Content, 1)
	content, ok := result.Content[0].(*sdkmcp.TextContent)
	require.True(t, ok)
	assert.JSONEq(t, `{"path_prefix":"/","sort":"path","direction":"asc","page_size":2,"items":[],"ttlMs":0,"cacheScope":"private"}`, content.Text)
	assert.Equal(t, int32(1), calls.Load())

	result, err = session.CallTool(t.Context(), &sdkmcp.CallToolParams{
		Name: "list_documents", Arguments: map[string]any{"page_size": "invalid"},
	})
	require.NoError(t, err)
	assert.True(t, result.IsError)
	assert.Equal(t, int32(1), calls.Load(), "invalid input must not reach the handler")

	_, err = session.CallTool(t.Context(), &sdkmcp.CallToolParams{
		Name: "list_documents", Arguments: map[string]any{"page_size": 3},
	})
	require.ErrorContains(t, err, "validating tool output")
	assert.Equal(t, int32(2), calls.Load())
}
