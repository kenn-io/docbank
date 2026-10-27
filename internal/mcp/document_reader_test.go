package mcp

import (
	"context"
	"encoding/json/v2"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/config"
	"go.kenn.io/docbank/internal/processing"
)

func TestEmbeddedDocumentToolsMatchDaemon(t *testing.T) {
	f := newNativeExportFixture(t)
	f.addFile(t, "alpha.txt", "observatory")
	profileName, profile, err := processing.NewSuppliedMediaProfile(f.catalog, f.blobs, "test:operator")
	require.NoError(t, err)
	service, err := processing.NewService(processing.ServiceConfig{Catalog: f.catalog, Blobs: f.blobs, Gate: api.NewOperationGate(),
		SpoolDirectory: filepath.Join(t.TempDir(), "spool"), Profiles: map[string]processing.ProfileConfig{profileName: profile}})
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, service.Shutdown(context.Background())) })
	cfg := config.Default()
	cfg.Server.APIKey = "synthetic-export-key"
	apiServer := api.NewServer(api.Deps{Store: f.catalog, Blobs: f.blobs, Processing: service, Cfg: cfg, VaultRoot: os.Getenv("DOCBANK_HOME")})
	t.Cleanup(apiServer.Close)
	httpServer := httptest.NewServer(apiServer.Handler())
	t.Cleanup(httpServer.Close)
	daemon := newServerWithOptionsAndDaemon(testImplementation(), ServerOptions{}, exportTestLease(t, httpServer.URL))
	reader := api.NewDocumentReader(f.catalog, f.blobs, os.Getenv("DOCBANK_HOME"), service)
	embedded := sdkmcp.NewServer(testImplementation(), nil)
	for _, name := range []string{"get_vault_info", "list_documents", "search_documents", "find_conflicts"} {
		tool, err := ReadTool(name)
		require.NoError(t, err)
		embedded.AddTool(tool, func(ctx context.Context, req *sdkmcp.CallToolRequest) (*sdkmcp.CallToolResult, error) {
			return CallDocumentReadTool(ctx, reader, req)
		})
	}
	connect := func(server *sdkmcp.Server) *sdkmcp.ClientSession {
		t.Helper()
		ct, st := sdkmcp.NewInMemoryTransports()
		ss, err := server.Connect(t.Context(), st, nil)
		require.NoError(t, err)
		t.Cleanup(func() { assert.NoError(t, ss.Close()) })
		cs, err := sdkmcp.NewClient(testImplementation(), nil).Connect(t.Context(), ct, nil)
		require.NoError(t, err)
		t.Cleanup(func() { assert.NoError(t, cs.Close()) })
		return cs
	}
	native, remote := connect(embedded), connect(daemon.sdk)
	for _, tc := range []struct {
		name string
		args map[string]any
	}{
		{"list_documents", map[string]any{}},
		{"get_vault_info", map[string]any{}},
		{"search_documents", map[string]any{"query": "alpha", "mode": "lexical", "profile": profileName, "filters": map[string]any{}}},
		{"find_conflicts", map[string]any{"query": "alpha", "mode": "lexical", "profile": profileName, "filters": map[string]any{}}},
		{"find_conflicts", map[string]any{"query": "unmatchedterm", "mode": "lexical", "profile": profileName, "filters": map[string]any{}}},
		{"find_conflicts", map[string]any{"query": "alpha", "profile": "missing", "filters": map[string]any{"mime_type": "image/png"}}},
		{"search_documents", map[string]any{"query": "alpha", "profile": "missing", "filters": map[string]any{"mime_type": "image/png"}}},
		{"list_documents", map[string]any{"cursor": "invalid"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			params := &sdkmcp.CallToolParams{Name: tc.name, Arguments: tc.args}
			expected, expectedErr := remote.CallTool(t.Context(), params)
			actual, actualErr := native.CallTool(t.Context(), params)
			require.Equal(t, expectedErr, actualErr)
			if actualErr != nil {
				return
			}
			require.NotNil(t, actual)
			assert.Equal(t, expected.IsError, actual.IsError)
			want, err := json.Marshal(expected.StructuredContent)
			require.NoError(t, err)
			got, err := json.Marshal(actual.StructuredContent)
			require.NoError(t, err)
			assert.JSONEq(t, string(want), string(got))
			assert.Equal(t, expected.Content, actual.Content)
		})
	}
	// Resolve the complete population and reject it; never silently truncate it.
	for i := range 4096 {
		f.addFile(t, "extra-"+strconv.Itoa(i)+".txt", "same")
	}
	result, err := native.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "search_documents", Arguments: map[string]any{
		"query": "alpha", "profile": profileName, "mode": "lexical", "filters": map[string]any{},
	}})
	require.NoError(t, err)
	require.True(t, result.IsError)
	encoded, err := json.Marshal(result.StructuredContent)
	require.NoError(t, err)
	var failure struct {
		Code     string `json:"code"`
		Observed int    `json:"observed_scope_count"`
	}
	require.NoError(t, json.Unmarshal(encoded, &failure))
	assert.Equal(t, "scope_too_large", failure.Code)
	assert.Equal(t, 4097, failure.Observed)
}
