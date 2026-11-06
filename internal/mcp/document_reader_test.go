package mcp

import (
	"context"
	"encoding/json/v2"
	"fmt"
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

func newProcessingDocumentServer(t *testing.T, f *nativeExportFixture) (*Server, *processing.Service, string) {
	t.Helper()
	profileName, profile, err := processing.NewSuppliedMediaProfile(f.catalog, f.blobs, "test:operator")
	require.NoError(t, err)
	service, err := processing.NewService(t.Context(), processing.ServiceConfig{Catalog: f.catalog, Blobs: f.blobs, Gate: api.NewOperationGate(),
		SpoolDirectory: filepath.Join(t.TempDir(), "spool"), Profiles: map[string]processing.ProfileConfig{profileName: profile}})
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, service.Shutdown(context.Background())) })
	return newSearchProcessingServer(t, f, service), service, profileName
}

func newSearchProcessingServer(t *testing.T, f *nativeExportFixture, service *processing.Service) *Server {
	t.Helper()
	cfg := config.Default()
	cfg.Server.APIKey = "synthetic-export-key"
	apiServer := api.NewServer(api.Deps{Store: f.catalog, Blobs: f.blobs, Processing: service, Cfg: cfg, VaultRoot: os.Getenv("DOCBANK_HOME")})
	t.Cleanup(apiServer.Close)
	httpServer := httptest.NewServer(apiServer.Handler())
	t.Cleanup(httpServer.Close)
	daemon := newServerWithOptionsAndDaemon(testImplementation(), ServerOptions{}, exportTestLease(t, httpServer.URL))
	return daemon
}

func TestEmbeddedDocumentToolsMatchDaemon(t *testing.T) {
	f := newNativeExportFixture(t)
	f.addFile(t, "alpha.txt", "observatory")
	daemon, service, profileName := newProcessingDocumentServer(t, f)
	reader := api.NewDocumentReader(f.catalog, f.blobs, os.Getenv("DOCBANK_HOME"), service)
	embedded := sdkmcp.NewServer(testImplementation(), nil)
	for _, name := range []string{"get_vault_info", "list_documents", "search_documents"} {
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
	// Filter searches cover live populations larger than an explicit version list.
	for i := range 4096 {
		f.addFile(t, "extra-"+strconv.Itoa(i)+".txt", "same")
	}
	result, err := native.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "search_documents", Arguments: map[string]any{
		"query": "alpha", "profile": profileName, "mode": "lexical", "filters": map[string]any{},
	}})
	require.NoError(t, err)
	require.False(t, result.IsError)
	output := structuredMap(t, result.StructuredContent)
	hits, ok := output["results"].([]any)
	require.True(t, ok)
	require.Len(t, hits, 1)
	hit, ok := hits[0].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "/alpha.txt", hit["path"])
}

func TestSearchDocumentsProfileDiscovery(t *testing.T) {
	f := newNativeExportFixture(t)
	f.addFile(t, "refund-policy.txt", "Refunds take five days.")
	profile, captions, err := processing.NewSuppliedCaptionProfile(f.catalog, f.blobs, "test:operator")
	require.NoError(t, err)
	service, err := processing.NewService(t.Context(), processing.ServiceConfig{Catalog: f.catalog, Blobs: f.blobs, Gate: api.NewOperationGate(),
		SpoolDirectory: filepath.Join(t.TempDir(), "spool"), Profiles: map[string]processing.ProfileConfig{profile: captions}})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, service.Shutdown(context.WithoutCancel(t.Context()))) })
	daemon := newSearchProcessingServer(t, f, service)
	reader := api.NewDocumentReader(f.catalog, f.blobs, os.Getenv("DOCBANK_HOME"), service)
	t.Run("automatic", func(t *testing.T) {
		output := exportCall(t, daemon, "search_documents", map[string]any{"query": "refund", "filters": map[string]any{}, "mode": "auto"})
		require.Equal(t, profile, output["profile"])
		require.Equal(t, "auto", output["requested_mode"])
		require.Equal(t, "lexical", output["actual_mode"])
		hits, ok := output["results"].([]any)
		require.True(t, ok)
		require.Len(t, hits, 1)
		require.Equal(t, "/refund-policy.txt", schemaMap(t, hits[0])["path"])
		require.Contains(t, fmt.Sprint(output["skipped_reasons"]), "docbank processing local")
	})
	for _, embedded := range []bool{false, true} {
		t.Run(fmt.Sprintf("embedded=%t/hybrid supplied captions", embedded), func(t *testing.T) {
			output := callSearchDocumentReader(t, daemon, reader, embedded, map[string]any{"query": "refund", "filters": map[string]any{}, "mode": "hybrid", "profile": profile})
			require.Equal(t, profile, output["profile"])
			require.Equal(t, "hybrid", output["requested_mode"])
			require.Equal(t, "lexical", output["actual_mode"])
			hits, ok := output["results"].([]any)
			require.True(t, ok)
			require.Len(t, hits, 1)
			require.Equal(t, "/refund-policy.txt", schemaMap(t, hits[0])["path"])
			require.Contains(t, output["skipped_reasons"], "Profile \"supplied-captions\" has no available search by meaning. Run docbank processing local to add it.")
		})
		for _, tc := range []struct {
			name, mode, selected, code string
			missing                    bool
		}{
			{name: "missing version before unknown profile", mode: "hybrid", selected: "default", code: "not_found", missing: true},
			{name: "unknown", mode: "hybrid", selected: "default", code: "processing_profile_unavailable"},
			{name: "semantic", mode: "semantic", selected: profile, code: "semantic_unavailable"},
		} {
			t.Run(fmt.Sprintf("embedded=%t/%s", embedded, tc.name), func(t *testing.T) {
				args := map[string]any{"query": "refund", "filters": map[string]any{}, "mode": tc.mode, "profile": tc.selected}
				if tc.missing {
					delete(args, "filters")
					args["content_version_ids"] = []string{testVersionID}
				}
				output := callSearchDocumentReader(t, daemon, reader, embedded, args)
				require.Equal(t, tc.code, output["code"])
				if tc.code != "not_found" {
					require.Contains(t, output["message"], profile)
				}
				if tc.code == "semantic_unavailable" {
					require.Contains(t, output["message"], "docbank processing local")
				}
			})
		}
	}
}

func callSearchDocumentReader(t *testing.T, daemon *Server, reader *api.DocumentReader, embedded bool, args map[string]any) map[string]any {
	t.Helper()
	if !embedded {
		return exportCall(t, daemon, "search_documents", args)
	}
	raw, err := json.Marshal(args)
	require.NoError(t, err)
	result, err := CallDocumentReadTool(t.Context(), reader, &sdkmcp.CallToolRequest{Params: &sdkmcp.CallToolParamsRaw{Name: "search_documents", Arguments: raw}})
	require.NoError(t, err)
	return structuredMap(t, result.StructuredContent)
}
