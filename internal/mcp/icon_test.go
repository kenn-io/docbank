package mcp

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	jsonv2 "encoding/json/v2"
	"fmt"
	"image/png"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/docbank/internal/version"
)

// Check the client-visible identity independently of the implementation builder.
func assertMCPServerInfo(t *testing.T, raw json.RawMessage) []byte {
	t.Helper()
	var info sdkmcp.Implementation
	require.NoError(t, json.Unmarshal(raw, &info))
	assert.Equal(t, "docbank", info.Name)
	assert.Equal(t, "Docbank", info.Title)
	assert.Equal(t, "Self-sovereign document system", info.Description)
	assert.Equal(t, version.Version, info.Version)
	assert.Equal(t, "https://docbank.ai", info.WebsiteURL)
	require.Len(t, info.Icons, 1)
	icon := info.Icons[0]
	assert.Equal(t, "image/png", icon.MIMEType)
	assert.Equal(t, []string{"256x256"}, icon.Sizes)
	const prefix = "data:image/png;base64,"
	require.True(t, strings.HasPrefix(icon.Source, prefix))
	data, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(icon.Source, prefix))
	require.NoError(t, err)
	require.Less(t, len(data), 16*1024, "keep discovery metadata small")
	img, err := png.Decode(bytes.NewReader(data))
	require.NoError(t, err)
	assert.Equal(t, 256, img.Bounds().Dx())
	assert.Equal(t, 256, img.Bounds().Dy())
	return data
}

func TestMCPServerIconHTTP(t *testing.T) {
	handler, err := NewServer().HTTPTransportHandler(HTTPOptions{BearerToken: testMCPBearer})
	require.NoError(t, err)
	request := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "http://127.0.0.1/mcp",
		bytes.NewReader(fixture(t, "discover.json")))
	request.Header = protocolHeaders("server/discover", "")
	request.Header.Set("Authorization", "Bearer "+testMCPBearer)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	var wire struct {
		Result struct {
			Meta map[string]json.RawMessage `json:"_meta"`
		} `json:"result"`
	}
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &wire))
	declared := assertMCPServerInfo(t, wire.Result.Meta[sdkmcp.MetaKeyServerInfo])
	for _, path := range []string{"/icon.png", "/favicon.ico"} {
		for _, method := range []string{http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodOptions} {
			t.Run(method+path, func(t *testing.T) {
				request := httptest.NewRequestWithContext(t.Context(), method, "http://127.0.0.1"+path, nil)
				request.Header.Set("Origin", "https://client.example")
				response := httptest.NewRecorder()
				handler.ServeHTTP(response, request)
				if method != http.MethodGet && method != http.MethodHead {
					assert.Equal(t, http.StatusMethodNotAllowed, response.Code)
					assert.Equal(t, "GET, HEAD", response.Header().Get("Allow"))
					return
				}
				require.Equal(t, http.StatusOK, response.Code)
				assert.Equal(t, "image/png", response.Header().Get("Content-Type"))
				assert.Equal(t, "nosniff", response.Header().Get("X-Content-Type-Options"))
				assert.Equal(t, "public, max-age=86400", response.Header().Get("Cache-Control"))
				assert.Equal(t, strconv.Itoa(len(declared)), response.Header().Get("Content-Length"))
				assert.Empty(t, response.Header().Get("WWW-Authenticate"))
				if method == http.MethodHead {
					assert.Empty(t, response.Body.Bytes())
				} else {
					assert.Equal(t, declared, response.Body.Bytes())
				}
			})
		}
	}
	for _, bearer := range []string{"", "synthetic-wrong-token"} {
		request := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "http://127.0.0.1/mcp",
			bytes.NewReader(fixture(t, "discover.json")))
		request.Header = protocolHeaders("server/discover", "")
		if bearer != "" {
			request.Header.Set("Authorization", "Bearer "+bearer)
		}
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		assert.Equal(t, http.StatusUnauthorized, response.Code)
		assert.Equal(t, "no-store", response.Header().Get("Cache-Control"))
	}
}

func TestMCPServerIconRoutesPreserveGuardedTransportPaths(t *testing.T) {
	handler, err := NewServer().HTTPTransportHandler(HTTPOptions{BearerToken: testMCPBearer})
	require.NoError(t, err)
	for _, path := range []string{"/mcp", "//mcp", "/mcp/../mcp", "/./icon.png"} {
		for _, bearer := range []string{"", testMCPBearer} {
			t.Run(path+"/"+strconv.FormatBool(bearer != ""), func(t *testing.T) {
				request := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "http://127.0.0.1"+path,
					bytes.NewReader(fixture(t, "discover.json")))
				request.Header = protocolHeaders("server/discover", "")
				if bearer != "" {
					request.Header.Set("Authorization", "Bearer "+bearer)
				}
				response := httptest.NewRecorder()
				handler.ServeHTTP(response, request)
				assert.Empty(t, response.Header().Get("Location"), "preserve transport handling without a redirect")
				assert.Equal(t, "no-store", response.Header().Get("Cache-Control"))
				if bearer == "" {
					require.Equal(t, http.StatusUnauthorized, response.Code)
					return
				}
				require.Equal(t, http.StatusOK, response.Code, response.Body.String())
				var wire struct {
					Result struct {
						Meta map[string]json.RawMessage `json:"_meta"`
					} `json:"result"`
				}
				require.NoError(t, json.Unmarshal(response.Body.Bytes(), &wire))
				assertMCPServerInfo(t, wire.Result.Meta[sdkmcp.MetaKeyServerInfo])
			})
		}
	}
}

func TestMCPServerIconPreservesNearLimitDocumentPage(t *testing.T) {
	// Catalog pages have no pagination-specific metadata reserve. These legal
	// paths produce a result that fits with main's identity but not the icon.
	page := api.DocumentPage{PathPrefix: "/", Sort: "path", Direction: "asc", PageSize: 128}
	prefix := "/" + strings.Repeat(strings.Repeat("a", 190)+"/", 20)
	for i := range page.PageSize {
		name := fmt.Sprintf("synthetic-%03d.md", i)
		page.Items = append(page.Items, api.DocumentSummary{
			NodeID: int64(i + 1), ContentVersionID: fmt.Sprintf("22222222-2222-4222-8222-%012d", i+1),
			Path: prefix + name, Name: name, MediaType: "text/markdown", Size: 12,
			ModifiedAt: "2026-08-28T00:00:00Z", ActiveRenditions: []api.DocumentRenditionIdentity{},
		})
	}
	output := listDocumentsOutput{DocumentPage: page, privateCache: newPrivateCache()}
	envelope, err := toolEnvelopeBytes(output, nil)
	require.NoError(t, err)
	const target = maxToolResponseBytes - 1500
	require.Less(t, envelope, target)
	// Each added ASCII byte occurs in both the structured and text copies.
	extra := (target - envelope) / 2
	padding := strings.Repeat(strings.Repeat("b", 190)+"/", extra/191) + strings.Repeat("b", extra%191)
	last := &page.Items[len(page.Items)-1]
	last.Path = "/" + padding + strings.TrimPrefix(last.Path, "/")
	require.Less(t, len(last.Path), maxPathCharacters)

	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/info":
			writeDaemonJSON(t, w, api.VaultInfo{VaultID: testVaultID})
		case "/api/v1/vault":
			writeDaemonJSON(t, w, map[string]string{"vault_id": testVaultID})
		case "/api/v1/documents":
			writeDaemonJSON(t, w, page)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(daemon.Close)
	for _, test := range []struct {
		name           string
		implementation *sdkmcp.Implementation
	}{
		{name: "main-sized identity", implementation: testImplementation()},
		{name: "icon identity", implementation: defaultImplementation()},
	} {
		t.Run(test.name, func(t *testing.T) {
			lease := newDaemonLeaseWith(func(context.Context) (*daemonconn.Connection, error) {
				return daemonconn.New(daemon.URL, "synthetic-key"), nil
			}, func(*daemonconn.Connection) error { return nil })
			server := newServerWithOptionsAndDaemon(test.implementation, ServerOptions{}, lease)
			raw := exchangeRaw(t, server, requestFor("tools/call", map[string]any{
				"name": "list_documents", "arguments": map[string]any{"page_size": page.PageSize},
			}))
			var wire struct {
				Result json.RawMessage `json:"result"`
				Error  json.RawMessage `json:"error"`
			}
			require.NoError(t, json.Unmarshal(raw, &wire))
			require.Empty(t, wire.Error, string(raw))
			require.LessOrEqual(t, len(wire.Result), maxToolResponseBytes)
			var result struct {
				StructuredContent listDocumentsOutput `json:"structuredContent"`
			}
			require.NoError(t, jsonv2.Unmarshal(wire.Result, &result))
			assert.Equal(t, page.Items, result.StructuredContent.Items)
			t.Logf("complete result: %d bytes", len(wire.Result))
		})
	}
}
