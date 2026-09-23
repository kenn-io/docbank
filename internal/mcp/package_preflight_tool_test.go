package mcp

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/apiclient"
	"go.kenn.io/docbank/internal/blob"
	"go.kenn.io/docbank/internal/config"
	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/docbank/internal/store"
)

func TestPackagePreflightToolsReturnImageDiagnostics(t *testing.T) {
	for _, count := range []int{1, 250} {
		t.Run(strconv.Itoa(count), func(t *testing.T) {
			vault := t.TempDir()
			catalog, err := store.Open(filepath.Join(vault, "docbank.db"))
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, catalog.Close()) })
			blobs, err := blob.New(store.NewPackCatalog(catalog), filepath.Join(vault, "blobs"))
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, blobs.Close()) })
			cfg := config.Default()
			cfg.Server.APIKey = "synthetic-diagnostics-key"
			server := api.NewServer(api.Deps{Store: catalog, Blobs: blobs, VaultRoot: vault, Cfg: cfg})
			t.Cleanup(server.Close)
			httpServer := httptest.NewServer(server.Handler())
			t.Cleanup(httpServer.Close)
			lease := newDaemonLeaseWith(func(context.Context) (*daemonconn.Connection, error) {
				return daemonconn.New(httpServer.URL, cfg.Server.APIKey), nil
			}, func(*daemonconn.Connection) error { return nil })

			root := t.TempDir()
			require.NoError(t, os.Mkdir(filepath.Join(root, "VOL001"), 0o700))
			require.NoError(t, os.WriteFile(filepath.Join(root, "VOL001", "package.dat"),
				[]byte("þDOCIDþ\x14þNATIVEþ\r\nþDOC-Aþ\x14þA.txtþ\r\n"), 0o600))
			require.NoError(t, os.WriteFile(filepath.Join(root, "VOL001", "A.txt"), []byte("synthetic native"), 0o600))
			var pageMap strings.Builder
			for index := range count {
				boundary := ""
				if index == 0 {
					boundary = "Y"
				}
				fmt.Fprintf(&pageMap, "DOC-A,VOL001,MISSING/PAGE-%03d.tif,%s,,,\r\n", index, boundary)
			}
			require.NoError(t, os.WriteFile(filepath.Join(root, "VOL001", "pages.opt"), []byte(pageMap.String()), 0o600))

			args, err := json.Marshal(preflightLoadFilePackageInput{SourcePath: root, Profile: "dat-concordance-v1", Encoding: "utf-8"})
			require.NoError(t, err)
			handler := packagePreflightToolHandler(lease, mustResolveSchema(packagePreflightOutputSchema()), slog.Default())
			created, err := handler(t.Context(), &sdkmcp.CallToolRequest{Params: &sdkmcp.CallToolParamsRaw{Arguments: args}})
			require.NoError(t, err)
			require.False(t, created.IsError)
			preflight := structuredMap(t, created.StructuredContent)
			got, err := invokeReadTool(t.Context(), lease, "get_package_preflight", map[string]any{"preflight_id": preflight["preflight_id"]})
			require.NoError(t, err)
			pageArgs := map[string]any{"preflight_id": preflight["preflight_id"], "limit": float64(250)}
			input, _ := listPackagePreflightDiagnosticsSchemas()
			require.NoError(t, mustResolveSchema(input).Validate(&pageArgs))
			page, err := invokeReadTool(t.Context(), lease, "list_package_preflight_diagnostics", pageArgs)
			require.NoError(t, err)
			for _, result := range []*sdkmcp.CallToolResult{created, got, page} {
				require.False(t, result.IsError)
				output := structuredMap(t, result.StructuredContent)
				diagnostics, ok := output["diagnostics"].([]any)
				require.True(t, ok)
				require.Len(t, diagnostics, count)
				first, ok := diagnostics[0].(map[string]any)
				require.True(t, ok)
				require.Equal(t, "image_missing", first["code"])
				require.Equal(t, "blocking", first["severity"])
				require.Equal(t, "DOC-A", first["row_id"])
			}
		})
	}
}

func TestPackageDiagnosticSchemaAcceptsImagePageOrdinals(t *testing.T) {
	diagnostic := structuredMap(t, api.PackageDiagnostic{
		Code: "image_missing", Severity: "blocking", RowID: "DOC-A", RowOrdinal: 100_001,
		Detail: "declared image is absent or unsafe",
	})
	require.NoError(t, mustResolveSchema(packageDiagnosticSchema()).Validate(&diagnostic))
}

func TestPackageZIPPreflightRecoversAfterLostSealResponse(t *testing.T) {
	vault := t.TempDir()
	catalog, err := store.Open(filepath.Join(vault, "docbank.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, catalog.Close()) })
	blobs, err := blob.New(store.NewPackCatalog(catalog), filepath.Join(vault, "blobs"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, blobs.Close()) })
	cfg := config.Default()
	cfg.Server.APIKey = "synthetic-zip-key"
	daemon := api.NewServer(api.Deps{Store: catalog, Blobs: blobs, VaultRoot: vault, Cfg: cfg})
	t.Cleanup(daemon.Close)
	sealed := make(chan api.PackageContainer, 1)
	httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/seal") {
			daemon.Handler().ServeHTTP(w, r)
			return
		}
		result := httptest.NewRecorder()
		daemon.Handler().ServeHTTP(result, r)
		if !assert.Equal(t, http.StatusOK, result.Code, result.Body.String()) {
			http.Error(w, "seal failed", result.Code)
			return
		}
		var container api.PackageContainer
		if !assert.NoError(t, json.Unmarshal(result.Body.Bytes(), &container)) {
			return
		}
		sealed <- container
		hijacker, ok := w.(http.Hijacker)
		if !assert.True(t, ok) {
			return
		}
		conn, _, err := hijacker.Hijack()
		if assert.NoError(t, err) {
			assert.NoError(t, conn.Close())
		}
	}))
	t.Cleanup(httpServer.Close)
	connection := daemonconn.New(httpServer.URL, cfg.Server.APIKey)
	t.Cleanup(func() { require.NoError(t, connection.Close()) })
	lease := newDaemonLeaseWith(func(context.Context) (*daemonconn.Connection, error) {
		return connection, nil
	}, func(*daemonconn.Connection) error { return nil })
	var archive bytes.Buffer
	writer := zip.NewWriter(&archive)
	for _, entry := range []struct{ name, body string }{
		{"VOL001/package.dat", "þDOCIDþ\x14þNATIVEþ\r\nþDOC-Aþ\x14þA.txtþ\r\n"},
		{"VOL001/A.txt", "synthetic document"},
	} {
		file, err := writer.Create(entry.name)
		require.NoError(t, err)
		_, err = file.Write([]byte(entry.body))
		require.NoError(t, err)
	}
	require.NoError(t, writer.Close())
	path := filepath.Join(t.TempDir(), "package.zip")
	require.NoError(t, os.WriteFile(path, archive.Bytes(), 0o600))
	server := newServerWithOptionsAndDaemon(testImplementation(), ServerOptions{AllowPackageWrites: true}, lease)
	result := decodeResult(t, exchangeRaw(t, server, requestFor("tools/call", map[string]any{
		"name": "preflight_load_file_package", "arguments": map[string]any{
			"source_path": path, "profile": "dat-concordance-v1", "encoding": "utf-8",
		},
	})))
	require.NotEqual(t, true, result["isError"])
	preflight := objectField(t, result, "structuredContent")
	assert.EqualValues(t, 1, preflight["records"])
	assert.NotEmpty(t, preflight["preflight_id"])
	assert.Equal(t, "container", preflight["source_kind"])
	select {
	case uploaded := <-sealed:
		retained, err := connection.API().GetPackageContainer(t.Context(), &apiclient.GetPackageContainerRequestOptions{
			PathParams: &apiclient.GetPackageContainerPath{ID: uploaded.ContainerID},
		})
		require.NoError(t, err)
		assert.Equal(t, "sealed", retained.State)
		digest := sha256.Sum256(archive.Bytes())
		assert.Equal(t, hex.EncodeToString(digest[:]), retained.SHA256)
		assert.EqualValues(t, archive.Len(), retained.Size)
	default:
		t.Fatal("preflight did not seal the uploaded ZIP")
	}
}

func TestPackageZIPPreflightCleansUnsealedFailedUploads(t *testing.T) {
	for _, outcome := range []string{"unsealed", "canceled", "unreadable"} {
		t.Run(outcome, func(t *testing.T) {
			vault := t.TempDir()
			catalog, err := store.Open(filepath.Join(vault, "docbank.db"))
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, catalog.Close()) })
			blobs, err := blob.New(store.NewPackCatalog(catalog), filepath.Join(vault, "blobs"))
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, blobs.Close()) })
			cfg := config.Default()
			cfg.Server.APIKey = "synthetic-upload-key"
			server := api.NewServer(api.Deps{Store: catalog, Blobs: blobs, VaultRoot: vault, Cfg: cfg})
			t.Cleanup(server.Close)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			declared := make(chan string, 1)
			httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				begin := r.Method == http.MethodPost && r.URL.Path == "/api/v1/packages/containers"
				if outcome == "unreadable" && r.Method == http.MethodGet {
					http.Error(w, "synthetic readback unavailable", http.StatusServiceUnavailable)
					return
				}
				if !begin {
					server.Handler().ServeHTTP(w, r)
					return
				}
				result := httptest.NewRecorder()
				server.Handler().ServeHTTP(result, r)
				var container api.PackageContainer
				if !assert.NoError(t, json.Unmarshal(result.Body.Bytes(), &container)) {
					return
				}
				declared <- container.ContainerID
				if outcome == "canceled" {
					cancel()
				}
				hijacker, ok := w.(http.Hijacker)
				if !assert.True(t, ok) {
					return
				}
				conn, _, err := hijacker.Hijack()
				if assert.NoError(t, err) {
					_ = conn.Close()
				}
			}))
			t.Cleanup(httpServer.Close)
			lease := newDaemonLeaseWith(func(context.Context) (*daemonconn.Connection, error) {
				return daemonconn.New(httpServer.URL, cfg.Server.APIKey), nil
			}, func(*daemonconn.Connection) error { return nil })
			path := filepath.Join(t.TempDir(), "package.zip")
			require.NoError(t, os.WriteFile(path, []byte("synthetic ZIP bytes"), 0o600))
			_, err = preflightPackageZIP(ctx, lease, path, api.PackagePreflightRequest{Profile: "dat-concordance-v1", Encoding: "utf-8"})
			require.ErrorIs(t, err, errProcessingOutcomeUnknown)
			var containerID string
			select {
			case containerID = <-declared:
			case <-time.After(5 * time.Second):
				t.Fatal("container was not declared")
			}
			request := httptest.NewRequest(http.MethodGet, "/api/v1/packages/containers/"+containerID, nil)
			request.Header.Set("X-Api-Key", cfg.Server.APIKey)
			response := httptest.NewRecorder()
			server.Handler().ServeHTTP(response, request)
			require.Equal(t, http.StatusNotFound, response.Code, response.Body.String())
		})
	}
}
