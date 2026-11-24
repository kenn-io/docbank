package mcp

import (
	"bytes"
	"encoding/json/v2"
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
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document/redaction"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/pdfproduction"
	"go.kenn.io/docbank/internal/processing"
)

func TestMCPResolveNativePDFRefusals(t *testing.T) {
	t.Setenv("DOCBANK_HOME", filepath.Join(t.TempDir(), "vault"))
	const privateText = "SECRET KEEP SECRET"
	const vaultID = "00000000-0000-4000-8000-000000000056"
	preparationPath, err := filepath.Abs("../../cmd/docbank/testdata/redaction-preparation.json")
	require.NoError(t, err)
	data, err := os.ReadFile(preparationPath)
	require.NoError(t, err)
	var prep processing.RedactionPreparation
	require.NoError(t, json.Unmarshal(data, &prep))
	require.Equal(t, privateText, prep.Map.Text)
	box := prep.Map.Atoms[0].Boxes[0]
	box.X1 = box.X0 + (box.X1-box.X0)/2
	masks := []processing.RedactionMask{{ID: uuid.New(), Selector: redaction.Selector{Kind: "rectangle", MapSHA256: prep.Map.SHA256, Boxes: []redaction.Box{box}}}}
	_, err = processing.BuildRedactionReview(prep, masks, prep.Selection.Revision, pdfproduction.QualifiedRecipe())
	var expansion *redaction.Problem
	require.ErrorAs(t, err, &expansion)
	require.Equal(t, "selection_expansion_required", expansion.Code)

	for _, scenario := range []struct {
		name, daemonCode, expectedCode string
		posts                          int32
	}{
		{name: "opt-in required"},
		{name: "invalid masks"},
		{name: "wrong vault", expectedCode: "redaction_selection_changed"},
		{name: "busy", daemonCode: "redaction_busy", expectedCode: "redaction_busy", posts: 1},
		{name: "memory", daemonCode: "redaction_memory", expectedCode: "redaction_memory", posts: 1},
		{name: "missing proposal", daemonCode: "selection_expansion_required", expectedCode: "redaction_integrity", posts: 1},
		{name: "altered proposal", daemonCode: "selection_expansion_required", expectedCode: "redaction_integrity", posts: 1},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			dir := t.TempDir()
			masksPath := filepath.Join(dir, "masks.json")
			maskBytes, err := json.Marshal(masks)
			require.NoError(t, err)
			if scenario.name == "invalid masks" {
				maskBytes = []byte(privateText)
			}
			require.NoError(t, os.WriteFile(masksPath, maskBytes, 0600))
			destination := filepath.Join(dir, "resolution.json")
			var requests, posts atomic.Int32
			daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				switch r.URL.Path {
				case "/api/v1/vault":
					observed := vaultID
					if scenario.name == "wrong vault" {
						observed = uuid.New().String()
					}
					writeDaemonJSON(t, w, api.VaultIdentity{VaultID: observed})
				case "/api/v1/redaction/resolve":
					posts.Add(1)
					assert.Equal(t, http.MethodPost, r.Method)
					var request processing.RedactionResolveRequest
					assert.NoError(t, json.UnmarshalRead(r.Body, &request))
					assert.Equal(t, prep, request.Preparation)
					assert.Equal(t, masks, request.Masks)
					status := http.StatusServiceUnavailable
					if scenario.daemonCode == "selection_expansion_required" {
						status = http.StatusUnprocessableEntity
					}
					problem := api.NewError(status, scenario.daemonCode, privateText)
					if scenario.name == "altered proposal" {
						altered := *expansion
						altered.DecisionIDs = []string{uuid.New().String()}
						problem.Redaction = &altered
					}
					w.Header().Set("Content-Type", "application/problem+json")
					w.WriteHeader(status)
					assert.NoError(t, json.MarshalWrite(w, problem))
				default:
					t.Errorf("unexpected daemon request: %s %s", r.Method, r.URL.Path)
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			defer daemon.Close()
			var log bytes.Buffer
			server := newServerWithOptionsAndDaemon(testImplementation(), ServerOptions{
				AllowPackageWrites: scenario.name != "opt-in required", Logger: slog.New(slog.NewJSONHandler(&log, nil)),
			}, exportTestLease(t, daemon.URL))
			raw := exchangeRaw(t, server, requestFor("tools/call", map[string]any{"name": "resolve_native_pdf", "arguments": map[string]any{
				"vault_id": vaultID, "preparation_path": preparationPath, "masks_path": masksPath, "destination_path": destination,
			}}))
			require.NotContains(t, string(raw)+log.String(), privateText)
			require.NoFileExists(t, destination)
			require.Equal(t, scenario.posts, posts.Load())
			if scenario.expectedCode == "" {
				require.EqualValues(t, jsonrpc.CodeInvalidParams, decodeWireError(t, raw).Code)
				require.Zero(t, requests.Load())
				return
			}
			result := objectField(t, decodeResult(t, raw), "structuredContent")
			require.Equal(t, scenario.expectedCode, result["code"])
			if scenario.name == "busy" || scenario.name == "memory" {
				message, ok := result["message"].(string)
				require.True(t, ok)
				require.Contains(t, strings.ToLower(message), "the daemon")
			}
		})
	}
}
