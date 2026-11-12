package mcp

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
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
	var patch api.PatchContractRequest
	err := json.Unmarshal([]byte(`{"starts_on":null,"expires_on":"2026-12-01","summary":""}`), &patch)
	require.NoError(t, err)
	encoded, err := json.Marshal(patch)
	require.NoError(t, err)
	var fields map[string]any
	require.NoError(t, json.Unmarshal(encoded, &fields))
	assert.Contains(t, fields, "starts_on")
	assert.Nil(t, fields["starts_on"])
	assert.Equal(t, "2026-12-01", fields["expires_on"])
	assert.Contains(t, fields, "summary")
	assert.Empty(t, fields["summary"])
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
	id, ok := created["contract_id"].(string)
	require.True(t, ok)
	assert.EqualValues(t, 1, created["revision"])
	list := call("list_contracts", map[string]any{"q": "%_", "party_id": party.PartyID, "as_of": "2026-11-12"})
	assert.EqualValues(t, 50, list["limit"])
	assert.Equal(t, "2026-11-12", list["as_of"])
	assert.Equal(t, "private", list["cacheScope"])
	items, ok := list["items"].([]any)
	require.True(t, ok)
	require.Len(t, items, 1)
	item, ok := items[0].(map[string]any)
	require.True(t, ok)
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
	documents, ok := detail["documents"].([]any)
	require.True(t, ok)
	require.Len(t, documents, 1)
	document, ok := documents[0].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, node.CurrentVersionID, document["content_version_id"])
	assert.Equal(t, strings.Repeat("a", 64), document["blob_sha256"])
	assert.EqualValues(t, 12, document["size"])
	assert.Equal(t, true, document["current"])
	assert.Equal(t, true, document["available"])
	assert.Nil(t, detail["starts_on"])
	assert.Equal(t, "2026-12-01", detail["expires_on"])
	assert.Contains(t, detail, "summary")
	assert.Empty(t, detail["summary"])
	stale := decodeResult(t, exchangeRaw(t, server, requestFor("tools/call", map[string]any{"name": "update_contract", "arguments": map[string]any{"contract_id": id, "revision": 1, "patch": map[string]any{"title": "Incorrect"}}})))
	assert.Equal(t, "stale_revision", objectField(t, stale, "structuredContent")["code"])
	// Pin a retained historical version after the file's head has changed.
	oldVersion := node.CurrentVersionID
	node, _, err = catalog.ReplaceContent(t.Context(), node.ID, node.Revision, strings.Repeat("b", 64), 7, "text/plain")
	require.NoError(t, err)
	historical := call("link_contract_document", map[string]any{"contract_id": id, "revision": 3, "node_id": node.ID, "content_version_id": oldVersion, "role": "amendment"})
	assert.EqualValues(t, 4, historical["revision"])
	detail = call("get_contract", map[string]any{"contract_id": id})
	documents, ok = detail["documents"].([]any)
	require.True(t, ok)
	require.Len(t, documents, 2)
	for _, raw := range documents {
		doc, ok := raw.(map[string]any)
		require.True(t, ok)
		assert.Equal(t, oldVersion, doc["content_version_id"])
		assert.Equal(t, false, doc["current"])
		assert.Equal(t, true, doc["available"])
	}
	replay := call("link_contract_document", map[string]any{"contract_id": id, "revision": 4, "node_id": node.ID, "content_version_id": oldVersion, "role": "amendment"})
	assert.EqualValues(t, 4, replay["revision"])
	other, err := catalog.CreateFile(t.Context(), catalog.RootID(), "other.txt", strings.Repeat("c", 64), 1, "text/plain")
	require.NoError(t, err)
	for _, tc := range []struct {
		nodeID, revision int64
		version, code    string
	}{
		{other.ID, 4, oldVersion, "invalid_contract"},
		{catalog.RootID(), 4, oldVersion, "not_file"},
		{node.ID, 3, oldVersion, "stale_revision"},
	} {
		response := decodeResult(t, exchangeRaw(t, server, requestFor("tools/call", map[string]any{"name": "link_contract_document", "arguments": map[string]any{"contract_id": id, "revision": tc.revision, "node_id": tc.nodeID, "content_version_id": tc.version, "role": "attachment"}})))
		assert.Equal(t, tc.code, objectField(t, response, "structuredContent")["code"])
	}
	other, _, err = catalog.Trash(t.Context(), other.ID, other.Revision)
	require.NoError(t, err)
	unavailable := decodeResult(t, exchangeRaw(t, server, requestFor("tools/call", map[string]any{"name": "link_contract_document", "arguments": map[string]any{"contract_id": id, "revision": 4, "node_id": other.ID, "content_version_id": other.CurrentVersionID, "role": "attachment"}})))
	assert.Equal(t, "invalid_contract", objectField(t, unavailable, "structuredContent")["code"])
	// A successful write can still fail the final MCP envelope limit.
	implementation := testImplementation()
	implementation.Name = strings.Repeat("x", maxToolResponseBytes)
	largeServer := newServerWithOptionsAndDaemon(implementation, ServerOptions{AllowContractWrites: true}, lease)
	large := decodeResult(t, exchangeRaw(t, largeServer, requestFor("tools/call", map[string]any{"name": "create_contract", "arguments": map[string]any{"title": "Envelope committed"}})))
	assert.Equal(t, "contract_outcome_unknown", objectField(t, large, "structuredContent")["code"])
	page, err := catalog.ListContracts(t.Context(), store.ContractListOptions{AsOf: "2026-11-12", Limit: 100})
	require.NoError(t, err)
	assert.Len(t, page, 2)
	// Let the real API commit, then drop its response at the HTTP boundary.
	var attempts atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		recorded := httptest.NewRecorder()
		daemon.Handler().ServeHTTP(recorded, r)
		if recorded.Code != http.StatusCreated {
			t.Errorf("unexpected mutation status: %d", recorded.Code)
		}
		connection, _, hijackErr := http.NewResponseController(w).Hijack()
		if hijackErr != nil {
			t.Errorf("hijack: %v", hijackErr)
			return
		}
		_ = connection.Close()
	}))
	t.Cleanup(proxy.Close)
	proxyConn := daemonconn.New(proxy.URL, cfg.Server.APIKey)
	t.Cleanup(func() { require.NoError(t, proxyConn.Close()) })
	proxyLease := newDaemonLeaseWith(func(context.Context) (*daemonconn.Connection, error) { return proxyConn, nil }, func(*daemonconn.Connection) error { return nil })
	proxyServer := newServerWithOptionsAndDaemon(testImplementation(), ServerOptions{AllowContractWrites: true}, proxyLease)
	lost := decodeResult(t, exchangeRaw(t, proxyServer, requestFor("tools/call", map[string]any{"name": "create_contract", "arguments": map[string]any{"title": "Lost reply committed"}})))
	assert.Equal(t, "contract_outcome_unknown", objectField(t, lost, "structuredContent")["code"])
	assert.EqualValues(t, 1, attempts.Load())
	page, err = catalog.ListContracts(t.Context(), store.ContractListOptions{AsOf: "2026-11-12", Limit: 100})
	require.NoError(t, err)
	assert.Len(t, page, 3)
	plan, err := catalog.PreviewInitialAudit(t.Context(), catalog.RootID(), "cli", nil)
	require.NoError(t, err)
	_, err = catalog.EnableInitialAudit(t.Context(), plan)
	require.NoError(t, err)
	audited := decodeResult(t, exchangeRaw(t, server, requestFor("tools/call", map[string]any{"name": "update_contract", "arguments": map[string]any{"contract_id": id, "revision": 4, "patch": map[string]any{"title": "Forbidden"}}})))
	assert.Equal(t, "audit_mutation_unsupported", objectField(t, audited, "structuredContent")["code"])
	unchanged, err := catalog.GetContract(t.Context(), id, "2026-11-12")
	require.NoError(t, err)
	assert.Equal(t, "Agreement %_", unchanged.Title)
	assert.EqualValues(t, 4, unchanged.Revision)
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
	assert.Contains(t, body, "summary")
	assert.Empty(t, body["summary"])
	assert.NotContains(t, body, "signed_on")
}

func TestContractMCPDaemonErrorPrivacy(t *testing.T) {
	const secret = "private-signer@example.test"
	for _, code := range []string{"invalid_contract", "party_not_own", "party_retired", "not_file", "stale_revision", "unrecognized_contract_problem"} {
		t.Run(code, func(t *testing.T) {
			var attempts atomic.Int32
			daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				attempts.Add(1)
				w.Header().Set("Content-Type", "application/problem+json")
				w.WriteHeader(http.StatusConflict)
				_, _ = fmt.Fprintf(w, `{"status":409,"title":"Request failed","code":%q,"detail":%q}`, code, secret)
			}))
			t.Cleanup(daemon.Close)
			var logs bytes.Buffer
			server := newServerWithOptionsAndDaemon(testImplementation(), ServerOptions{AllowContractWrites: true, Logger: slog.New(slog.NewTextHandler(&logs, nil))}, exportTestLease(t, daemon.URL))
			wire := exchangeRaw(t, server, requestFor("tools/call", map[string]any{"name": "create_contract", "arguments": map[string]any{"title": "Synthetic"}}))
			if code == "unrecognized_contract_problem" {
				assert.NotZero(t, decodeWireError(t, wire).Code)
			} else {
				result := decodeResult(t, wire)
				detail := objectField(t, result, "structuredContent")
				assert.Equal(t, code, detail["code"])
				want := "Check the contract values and selected party or document identities."
				if code == "stale_revision" {
					want = "The revision is stale; read the current state and retry with its revision."
				}
				assert.Equal(t, want, detail["message"])
			}
			assert.EqualValues(t, 1, attempts.Load())
			assert.NotContains(t, string(wire), secret)
			assert.NotContains(t, logs.String(), secret)
			assert.Contains(t, logs.String(), "operation=create_contract")
		})
	}
}

func TestContractMCPDaemonProjectionPrivacy(t *testing.T) {
	const id = "11111111-1111-4111-8111-111111111111"
	const secret = "private-signer@example.test"
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("ETag", `"1"`)
		_, _ = fmt.Fprintf(w, `{"contract_id":%q,"title":"Synthetic","contract_type":"","status":"draft","effective_status":"draft","starts_on":null,"expires_on":null,"signed_on":null,"summary":"","purpose":"","consideration":"","termination":"","revision":1,"created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-01T00:00:00Z","parties":[],"documents":[],"signer_email":%q}`, id, secret)
	}))
	t.Cleanup(daemon.Close)
	var logs bytes.Buffer
	server := newServerWithOptionsAndDaemon(testImplementation(), ServerOptions{Logger: slog.New(slog.NewTextHandler(&logs, nil))}, exportTestLease(t, daemon.URL))
	wire := exchangeRaw(t, server, requestFor("tools/call", map[string]any{"name": "get_contract", "arguments": map[string]any{"contract_id": id}}))
	result := decodeResult(t, wire)
	assert.NotEqual(t, true, result["isError"])
	assert.Equal(t, id, objectField(t, result, "structuredContent")["contract_id"])
	assert.NotContains(t, string(wire), secret)
	assert.NotContains(t, logs.String(), secret)
}

func TestContractMCPGetContractTooLargeWire(t *testing.T) {
	const id = "11111111-1111-4111-8111-111111111111"
	hash := strings.Repeat("a", 64)
	documents := make([]api.ContractDocument, 3000)
	for i := range documents {
		documents[i] = api.ContractDocument{
			NodeID: int64(i + 1), ContentVersionID: fmt.Sprintf("22222222-2222-4222-8222-%012x", i+1),
			BlobSHA256: hash, Size: 12, Role: "signed", AddedAt: "2026-01-01T00:00:00Z", Current: true, Available: true,
		}
	}
	contract := api.Contract{
		ContractID: id, Title: "Synthetic", Status: "draft", EffectiveStatus: "draft", Revision: 1,
		CreatedAt: "2026-01-01T00:00:00Z", UpdatedAt: "2026-01-01T00:00:00Z",
		Parties: []api.ContractParty{}, Documents: documents,
	}
	payload, err := json.Marshal(contract)
	require.NoError(t, err)
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("ETag", `"1"`)
		_, _ = w.Write(payload)
	}))
	t.Cleanup(daemon.Close)
	server := newServerWithOptionsAndDaemon(testImplementation(), ServerOptions{}, exportTestLease(t, daemon.URL))
	wire := exchangeRaw(t, server, requestFor("tools/call", map[string]any{"name": "get_contract", "arguments": map[string]any{"contract_id": id}}))
	result := decodeResult(t, wire)
	assert.Equal(t, true, result["isError"])
	detail := objectField(t, result, "structuredContent")
	assert.Equal(t, "contract_result_too_large", detail["code"])
	assert.Equal(t, "The contract result exceeds the MCP response limit; use the CLI or HTTP API to inspect it.", detail["message"])
	assert.NotContains(t, string(wire), hash)
	assert.NotContains(t, string(wire), `"documents"`)
	assert.NotContains(t, string(wire), id)
}

func TestContractMCPWriteInvalidIntegerEncoding(t *testing.T) {
	var mutations atomic.Int32
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mutations.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(daemon.Close)
	server := newServerWithOptionsAndDaemon(testImplementation(), ServerOptions{AllowContractWrites: true}, exportTestLease(t, daemon.URL))
	args := map[string]any{"contract_id": "11111111-1111-4111-8111-111111111111", "revision": 1, "patch": map[string]any{"title": "Synthetic"}}
	args["revision"] = float64(1)
	assertSchemaAccepts(t, catalogMap(toolCatalog(ServerOptions{AllowContractWrites: true}))["update_contract"].InputSchema, args)
	args["revision"] = 1
	request := requestFor("tools/call", map[string]any{"name": "update_contract", "arguments": args})
	wireRequest := strings.Replace(string(request), `"revision":1`, `"revision":1.0`, 1)
	require.NotEqual(t, string(request), wireRequest)
	wire := exchangeRaw(t, server, []byte(wireRequest))
	assert.Zero(t, mutations.Load())
	assert.EqualValues(t, -32602, decodeWireError(t, wire).Code)
}
