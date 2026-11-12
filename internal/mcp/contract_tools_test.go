package mcp

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/blob"
	"go.kenn.io/docbank/internal/config"
	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/docbank/internal/store"
)

func TestContractMCPCatalogOptIn(t *testing.T) {
	names := func(options ServerOptions) map[string]bool {
		result := map[string]bool{}
		for _, tool := range toolCatalog(options) {
			result[tool.Name] = true
		}
		return result
	}
	assert.Len(t, toolCatalog(ServerOptions{}), 45)
	assert.Len(t, toolCatalog(ServerOptions{AllowContractWrites: true}), 48)
	assert.Len(t, toolCatalog(ServerOptions{AllowContractWrites: true, AllowProcessing: true, AllowPackageWrites: true, AllowPhotoEdits: true, AllowExportWrites: true, AllowReportWrites: true, AllowKataWrites: true}), 73)
	read := names(ServerOptions{})
	assert.True(t, read["list_contracts"])
	assert.True(t, read["get_contract"])
	assert.False(t, read["create_contract"])
	writes := names(ServerOptions{AllowContractWrites: true})
	for _, tool := range toolCatalog(ServerOptions{AllowContractWrites: true}) {
		if contractWriteTool(tool.Name) {
			assert.True(t, writes[tool.Name])
			assert.False(t, tool.Annotations.ReadOnlyHint)
			assert.False(t, tool.Annotations.IdempotentHint)
			assert.Equal(t, new(tool.Name == "update_contract"), tool.Annotations.DestructiveHint)
		}
	}
}

func TestContractMCPPatchPreservesNull(t *testing.T) {
	patch, err := decodeContractPatch([]byte(`{"starts_on":null,"expires_on":"2026-12-01","summary":""}`))
	require.NoError(t, err)
	encoded, err := json.Marshal(patch)
	require.NoError(t, err)
	var fields map[string]any
	require.NoError(t, json.Unmarshal(encoded, &fields))
	assert.Contains(t, fields, "starts_on")
	assert.Nil(t, fields["starts_on"])
	assert.Equal(t, "2026-12-01", fields["expires_on"])
	assert.Equal(t, "", fields["summary"])
	assert.NotContains(t, fields, "signed_on")
}

func TestContractMCPInputSemanticsTermsByteLimit(t *testing.T) {
	assert.True(t, validToolSemantics("update_contract", map[string]any{"patch": map[string]any{"summary": strings.Repeat("é", 4096)}}))
	assert.False(t, validToolSemantics("update_contract", map[string]any{"patch": map[string]any{"summary": strings.Repeat("é", 4097)}}))
}

func TestContractMCPWorkflowAndWriteOptIn(t *testing.T) {
	vault := t.TempDir()
	catalog, err := store.Open(t.Context(), filepath.Join(vault, "docbank.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, catalog.Close()) })
	blobs, err := blob.New(store.NewPackCatalog(catalog), filepath.Join(vault, "blobs"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, blobs.Close()) })
	cfg := config.Default()
	cfg.Server.APIKey = "synthetic-contract-mcp-key"
	daemon := api.NewServer(api.Deps{Store: catalog, Blobs: blobs, VaultRoot: vault, Cfg: cfg})
	t.Cleanup(daemon.Close)
	httpServer := httptest.NewServer(daemon.Handler())
	t.Cleanup(httpServer.Close)
	conn := daemonconn.New(httpServer.URL, cfg.Server.APIKey)
	t.Cleanup(func() { require.NoError(t, conn.Close()) })
	lease := newDaemonLeaseWith(func(context.Context) (*daemonconn.Connection, error) { return conn, nil }, func(*daemonconn.Connection) error { return nil })
	server := newServerWithOptionsAndDaemon(testImplementation(), ServerOptions{AllowContractWrites: true}, lease)
	call := func(name string, args map[string]any) map[string]any {
		t.Helper()
		result := decodeResult(t, exchangeRaw(t, server, requestFor("tools/call", map[string]any{"name": name, "arguments": args})))
		require.NotEqual(t, true, result["isError"], "%v", result)
		return objectField(t, result, "structuredContent")
	}
	party, err := catalog.CreateParty(t.Context(), store.PartyInput{Kind: "organization", DisplayName: "Synthetic owner", Own: true})
	require.NoError(t, err)
	created := call("create_contract", map[string]any{"title": "Agreement %_", "starts_on": "2026-01-01", "expires_on": "2026-12-01", "summary": "Synthetic terms", "parties": []any{map[string]any{"party_id": party.PartyID, "role": "own"}}})
	id := created["contract_id"].(string)
	assert.EqualValues(t, 1, created["revision"])
	list := call("list_contracts", map[string]any{"q": "%_", "party_id": party.PartyID, "as_of": "2026-11-12"})
	assert.EqualValues(t, 50, list["limit"])
	assert.Equal(t, "2026-11-12", list["as_of"])
	assert.Equal(t, "private", list["cacheScope"])
	items := list["items"].([]any)
	require.Len(t, items, 1)
	item := items[0].(map[string]any)
	assert.Equal(t, id, item["contract_id"])
	assert.NotContains(t, item, "parties")
	assert.NotContains(t, item, "summary")
	detail := call("get_contract", map[string]any{"contract_id": id, "as_of": "2026-11-12"})
	assert.Equal(t, "Synthetic terms", detail["summary"])
	require.Len(t, detail["parties"], 1)
	updated := call("update_contract", map[string]any{"contract_id": id, "revision": 1, "patch": map[string]any{"starts_on": nil, "summary": ""}})
	assert.EqualValues(t, 2, updated["revision"])
	node, err := catalog.CreateFile(t.Context(), catalog.RootID(), "agreement.txt", strings.Repeat("a", 64), 12, "text/plain")
	require.NoError(t, err)
	linked := call("link_contract_document", map[string]any{"contract_id": id, "revision": 2, "node_id": node.ID, "content_version_id": node.CurrentVersionID, "role": "signed"})
	assert.EqualValues(t, 3, linked["revision"])
	detail = call("get_contract", map[string]any{"contract_id": id})
	assert.Equal(t, "draft", detail["status"])
	documents := detail["documents"].([]any)
	require.Len(t, documents, 1)
	document := documents[0].(map[string]any)
	assert.Equal(t, node.CurrentVersionID, document["content_version_id"])
	assert.Equal(t, strings.Repeat("a", 64), document["blob_sha256"])
	assert.EqualValues(t, 12, document["size"])
	assert.Equal(t, true, document["current"])
	assert.Equal(t, true, document["available"])
	assert.Nil(t, detail["starts_on"])
	assert.Equal(t, "2026-12-01", detail["expires_on"])
	assert.Equal(t, "", detail["summary"])
	stale := decodeResult(t, exchangeRaw(t, server, requestFor("tools/call", map[string]any{"name": "update_contract", "arguments": map[string]any{"contract_id": id, "revision": 1, "patch": map[string]any{"title": "Incorrect"}}})))
	assert.Equal(t, "stale_revision", objectField(t, stale, "structuredContent")["code"])
	disabled := newServerWithOptionsAndDaemon(testImplementation(), ServerOptions{}, lease)
	reply := exchangeRaw(t, disabled, requestFor("tools/call", map[string]any{"name": "create_contract", "arguments": map[string]any{"title": "Disabled"}}))
	assert.NotZero(t, decodeWireError(t, reply).Code)
}

func TestContractMCPClosedArgumentsAndBounds(t *testing.T) {
	tools := catalogMap(toolCatalog(ServerOptions{AllowContractWrites: true}))
	for _, tc := range []struct {
		name string
		args map[string]any
	}{
		{"list_contracts", map[string]any{"contract_type": ""}}, {"list_contracts", map[string]any{"offset": float64(9007199254740992)}},
		{"get_contract", map[string]any{"contract_id": "11111111-1111-4111-8111-111111111111", "As_of": "2026-01-01"}},
		{"create_contract", map[string]any{"title": nil}},
		{"update_contract", map[string]any{"contract_id": "11111111-1111-4111-8111-111111111111", "revision": 1, "patch": map[string]any{}}},
		{"update_contract", map[string]any{"contract_id": "11111111-1111-4111-8111-111111111111", "revision": 1, "patch": map[string]any{"parties": []any{}}}},
		{"link_contract_document", map[string]any{"contract_id": "11111111-1111-4111-8111-111111111111", "revision": 1, "node_id": 1, "role": "signed"}},
	} {
		t.Run(tc.name+fmt.Sprint(tc.args), func(t *testing.T) { assertSchemaRejects(t, tools[tc.name].InputSchema, tc.args) })
	}
}

func TestContractMCPProjectionAndEnvelope(t *testing.T) {
	var c api.Contract
	require.NoError(t, json.Unmarshal([]byte(`{"contract_id":"11111111-1111-4111-8111-111111111111","title":"Synthetic","revision":1,"signer_email":"private@example.test"}`), &c))
	out := contractDetail(c)
	encoded, err := json.Marshal(out)
	require.NoError(t, err)
	assert.NotContains(t, string(encoded), "private@example.test")
	out.Summary = strings.Repeat("\"", maxToolResponseBytes)
	assert.ErrorIs(t, contractResultFits(out), errContractResultTooLarge)
}

func TestContractMCPWriteOutcomeUnknown(t *testing.T) {
	attempts := 0
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"contract_id":"11111111-1111-4111-8111-111111111111","revision":1}`))
	}))
	t.Cleanup(daemon.Close)
	conn := daemonconn.New(daemon.URL, "synthetic-key")
	t.Cleanup(func() { require.NoError(t, conn.Close()) })
	lease := newDaemonLeaseWith(func(context.Context) (*daemonconn.Connection, error) { return conn, nil }, func(*daemonconn.Connection) error { return nil })
	server := newServerWithOptionsAndDaemon(testImplementation(), ServerOptions{AllowContractWrites: true}, lease)
	result := decodeResult(t, exchangeRaw(t, server, requestFor("tools/call", map[string]any{"name": "create_contract", "arguments": map[string]any{"title": "Synthetic"}})))
	assert.Equal(t, "contract_outcome_unknown", objectField(t, result, "structuredContent")["code"])
	assert.Equal(t, 1, attempts)
}

func TestContractMCPPatchDateRelay(t *testing.T) {
	const id = "11111111-1111-4111-8111-111111111111"
	attempts := 0
	var body map[string]any
	var match string
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		match = r.Header.Get("If-Match")
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read request: %v", err)
			return
		}
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Errorf("decode request: %v", err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("ETag", `"8"`)
		_, _ = w.Write([]byte(`{"contract_id":"` + id + `","revision":8}`))
	}))
	t.Cleanup(daemon.Close)
	conn := daemonconn.New(daemon.URL, "synthetic-key")
	t.Cleanup(func() { require.NoError(t, conn.Close()) })
	lease := newDaemonLeaseWith(func(context.Context) (*daemonconn.Connection, error) { return conn, nil }, func(*daemonconn.Connection) error { return nil })
	server := newServerWithOptionsAndDaemon(testImplementation(), ServerOptions{AllowContractWrites: true}, lease)
	response := decodeResult(t, exchangeRaw(t, server, requestFor("tools/call", map[string]any{"name": "update_contract", "arguments": map[string]any{"contract_id": id, "revision": 7, "patch": map[string]any{"starts_on": nil, "expires_on": "2026-12-01", "summary": ""}}})))
	require.NotEqual(t, true, response["isError"], "%v", response)
	assert.Equal(t, 1, attempts)
	assert.Equal(t, `"7"`, match)
	assert.Contains(t, body, "starts_on")
	assert.Nil(t, body["starts_on"])
	assert.Equal(t, "2026-12-01", body["expires_on"])
	assert.Equal(t, "", body["summary"])
	assert.NotContains(t, body, "signed_on")
}
