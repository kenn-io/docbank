package mcp

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json/v2"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"uuid"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document/redaction"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/config"
	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/docbank/internal/processing"
)

func TestMCPPrepareNativePDF(t *testing.T) {
	t.Setenv("DOCBANK_HOME", filepath.Join(t.TempDir(), "vault"))
	encoded, err := os.ReadFile("../../cmd/docbank/testdata/redaction-preparation.json")
	require.NoError(t, err)
	var original processing.RedactionPreparation
	require.NoError(t, json.Unmarshal(encoded, &original))
	const sourceText = "SECRET KEEP SECRET"
	require.Equal(t, sourceText, original.Map.Text)
	const vaultID = "00000000-0000-4000-8000-000000000056"
	for _, scenario := range []string{"saved", "disabled", "wrong vault", "wrong hash", "stale revision", "invalid preparation", "changed response selection", "malformed response", "changed map", "destination conflict"} {
		t.Run(scenario, func(t *testing.T) {
			prep := original
			prep.Selection.Revision++
			selected := prep.Selection.Source
			destination := filepath.Join(t.TempDir(), "PRIVATE-preparation.json")
			args := map[string]any{"vault_id": vaultID, "node_id": prep.Selection.NodeID, "content_version_id": selected.VersionID,
				"blob_hash": selected.SHA256, "size": selected.Size, "destination_path": destination}
			switch scenario {
			case "wrong vault":
				args["vault_id"] = uuid.New().String()
			case "wrong hash":
				args["blob_hash"] = fmt.Sprintf("%x", sha256.Sum256([]byte("different PDF")))
			}
			if scenario == "changed map" {
				// Rehash the modified map so an envelope-only check still accepts it.
				changed := prep.Map
				changed.Atoms = append(changed.Atoms[:0:0], changed.Atoms...)
				box := changed.Atoms[0].Boxes[0]
				box.X0++
				changed.Atoms[0].Boxes = append(changed.Atoms[0].Boxes[:0:0], changed.Atoms[0].Boxes...)
				changed.Atoms[0].Boxes[0] = box
				_, hash, err := redaction.CanonicalTextMap(changed)
				require.NoError(t, err)
				changed.SHA256 = hash
				prep.Map = changed
				require.NoError(t, processing.ValidateRedactionPreparation(prep))
			}
			var requests, preparations atomic.Int32
			currentVersion := uuid.New().String()
			daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				switch r.URL.Path {
				case "/api/v1/vault":
					writeDaemonJSON(t, w, api.VaultIdentity{VaultID: vaultID})
				case fmt.Sprintf("/api/v1/nodes/%d", prep.Selection.NodeID):
					writeDaemonJSON(t, w, api.Node{ID: prep.Selection.NodeID, Revision: prep.Selection.Revision, Kind: "file", Path: "/test.pdf", CurrentVersionID: currentVersion})
				case "/api/v1/versions/" + selected.VersionID:
					writeDaemonJSON(t, w, api.ContentVersion{ID: selected.VersionID, NodeID: prep.Selection.NodeID, Size: selected.Size, BlobHash: selected.SHA256, MimeType: "application/pdf"})
				case "/api/v1/redaction/prepare":
					preparations.Add(1)
					var request processing.RedactionPreparationRequest
					assert.NoError(t, json.UnmarshalRead(r.Body, &request))
					assert.Equal(t, prep.Selection, request.Selection)
					assert.Equal(t, http.MethodPost, r.Method)
					switch scenario {
					case "stale revision":
						w.Header().Set("Content-Type", "application/problem+json")
						w.WriteHeader(http.StatusConflict)
						assert.NoError(t, json.MarshalWrite(w, api.NewError(409, "redaction_selection_changed", sourceText)))
					case "invalid preparation":
						w.Header().Set("Content-Type", "application/problem+json")
						w.WriteHeader(http.StatusUnprocessableEntity)
						assert.NoError(t, json.MarshalWrite(w, api.NewError(422, "invalid_redaction_preparation", sourceText)))
					case "changed response selection":
						reply := prep
						reply.Selection.Revision++
						writeDaemonJSON(t, w, reply)
					case "malformed response":
						w.Header().Set("Content-Type", "application/json")
						_, _ = fmt.Fprintf(w, `{"%s":`, sourceText)
					default:
						if scenario == "destination conflict" {
							assert.NoError(t, os.WriteFile(destination, []byte("existing file"), 0600))
						}
						writeDaemonJSON(t, w, prep)
					}
				default:
					t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			defer daemon.Close()

			var logs bytes.Buffer
			server := newServerWithOptionsAndDaemon(testImplementation(), ServerOptions{AllowPackageWrites: scenario != "disabled",
				Logger: slog.New(slog.NewJSONHandler(&logs, nil))}, exportTestLease(t, daemon.URL))
			request := requestFor("tools/call", map[string]any{"name": "prepare_native_pdf", "arguments": args})
			response := exchangeRaw(t, server, request)
			require.NotContains(t, string(response)+logs.String(), sourceText)
			if scenario == "disabled" {
				require.EqualValues(t, jsonrpc.CodeInvalidParams, decodeWireError(t, response).Code)
				require.Zero(t, requests.Load())
				require.NoFileExists(t, destination)
				return
			}
			if scenario == "invalid preparation" {
				require.EqualValues(t, jsonrpc.CodeInvalidParams, decodeWireError(t, response).Code)
				require.EqualValues(t, 1, preparations.Load())
				require.NoFileExists(t, destination)
				return
			}
			result := objectField(t, decodeResult(t, response), "structuredContent")
			switch scenario {
			case "saved":
				require.Equal(t, "published", result["state"], string(response))
				require.Equal(t, selected.VersionID, result["content_version_id"])
				require.Equal(t, destination, result["destination_path"])
				require.EqualValues(t, prep.Selection.Revision, result["revision"])
				saved, err := os.ReadFile(destination)
				require.NoError(t, err)
				require.EqualValues(t, len(saved), result["size"])
				require.Equal(t, fmt.Sprintf("%x", sha256.Sum256(saved)), result["sha256"])
				require.Contains(t, string(saved), sourceText)
				var savedPreparation processing.RedactionPreparation
				require.NoError(t, json.Unmarshal(saved, &savedPreparation))
				require.Equal(t, prep, savedPreparation)
				existing := exchangeRaw(t, server, requestFor("tools/call", map[string]any{"name": "prepare_native_pdf", "arguments": args}))
				require.EqualValues(t, jsonrpc.CodeInvalidParams, decodeWireError(t, existing).Code)
			case "wrong vault", "wrong hash", "stale revision":
				require.Equal(t, "redaction_selection_changed", result["code"])
				require.NoFileExists(t, destination)
			case "destination conflict":
				require.Equal(t, "redaction_preparation_local_io", result["code"])
				saved, err := os.ReadFile(destination)
				require.NoError(t, err)
				require.Equal(t, "existing file", string(saved))
			default:
				require.Equal(t, "redaction_preparation_integrity", result["code"])
				require.NoFileExists(t, destination)
			}
			if scenario == "wrong vault" || scenario == "wrong hash" {
				require.Zero(t, preparations.Load())
			} else {
				require.EqualValues(t, 1, preparations.Load())
			}
			stages, err := filepath.Glob(filepath.Join(filepath.Dir(destination), "docbank-preparation-*"))
			require.NoError(t, err)
			require.Empty(t, stages)
		})
	}
}

func TestMCPPrepareNativePDFReadDuringCancellation(t *testing.T) {
	f := newNativeExportFixture(t)
	selected := f.addFile(t, "selected.pdf", "synthetic content")
	root := os.Getenv("DOCBANK_HOME")
	cfg := config.Default()
	cfg.Server.APIKey = "synthetic-preparation-key"
	const token = "synthetic-preparation-proof"
	owner := api.NewServer(api.Deps{Cfg: cfg, Store: f.catalog, Blobs: f.blobs, VaultRoot: root, ShutdownToken: token})
	defer owner.Close()
	entered, canceled := make(chan struct{}), make(chan struct{})
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/redaction/prepare" {
			owner.Handler().ServeHTTP(w, r)
			return
		}
		assert.Equal(t, cfg.Server.APIKey, r.Header.Get("X-Api-Key"))
		_, _ = io.Copy(io.Discard, r.Body)
		close(entered)
		<-r.Context().Done()
		close(canceled)
	}))
	defer daemon.Close()
	record := daemonconn.NewRecord(strings.TrimPrefix(daemon.URL, "http://"), cfg.Server.APIKey, token, "")
	_, err := daemonconn.RuntimeStore(root).Write(record)
	require.NoError(t, err)
	connection, running, err := daemonconn.Running(t.Context())
	require.NoError(t, err)
	require.True(t, running)
	t.Cleanup(func() { require.NoError(t, connection.Close()) })
	server := NewServerWithConnection(connection, ServerOptions{AllowPackageWrites: true})
	destination := filepath.Join(t.TempDir(), "preparation.json")
	args, err := json.Marshal(map[string]any{"vault_id": f.catalog.VaultID(), "node_id": selected.NodeID,
		"content_version_id": selected.VersionID, "blob_hash": selected.SHA256, "size": selected.Size, "destination_path": destination})
	require.NoError(t, err)
	_, outputSchema := prepareNativePDFSchemas()
	handler := prepareNativePDFHandler(server.daemon, mustResolveSchema(outputSchema), slog.Default())
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan *sdkmcp.CallToolResult, 1)
	go func() {
		result, err := handler(ctx, &sdkmcp.CallToolRequest{Params: &sdkmcp.CallToolParamsRaw{Name: "prepare_native_pdf", Arguments: args}})
		assert.NoError(t, err)
		done <- result
	}()
	select {
	case <-entered:
	case premature := <-done:
		t.Fatalf("preparation returned before reaching the daemon: %#v", premature)
	}
	read := func() {
		result := exportCall(t, server, "list_document_versions", map[string]any{"node_id": selected.NodeID})
		require.EqualValues(t, 1, result["total"])
	}
	read()
	cancel()
	result := <-done
	require.NotNil(t, result)
	require.True(t, result.IsError)
	require.Equal(t, "redaction_preparation_canceled", structuredMap(t, result.StructuredContent)["code"])
	<-canceled
	read()
	entries, err := os.ReadDir(filepath.Dir(destination))
	require.NoError(t, err)
	require.Empty(t, entries)
}
