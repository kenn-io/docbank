package mcp

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/blob"
	"go.kenn.io/docbank/internal/config"
	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/docbank/internal/store"
)

func TestPeopleMCPToolsAreReads(t *testing.T) {
	readOnly := catalogMap(toolCatalog(ServerOptions{}))
	tool := readOnly["get_person"]
	require.NotNil(t, tool)
	assert.True(t, tool.Annotations.ReadOnlyHint)
	withWrites := catalogMap(toolCatalog(ServerOptions{
		AllowProcessing: true, AllowPackageWrites: true, AllowPhotoEdits: true,
	}))
	for _, name := range []string{"create_person", "rename_person", "retire_person", "merge_people", "split_person"} {
		assert.NotContains(t, withWrites, name)
	}
}

// peopleMCPServer serves MCP person tools from a real daemon over a fresh vault.
func peopleMCPServer(t *testing.T) (*store.Store, func(string, map[string]any) map[string]any) {
	t.Helper()
	vault := t.TempDir()
	catalog, err := store.Open(filepath.Join(vault, "docbank.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, catalog.Close()) })
	blobs, err := blob.New(store.NewPackCatalog(catalog), filepath.Join(vault, "blobs"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, blobs.Close()) })
	cfg := config.Default()
	cfg.Server.APIKey = "synthetic-person-mcp-key"
	daemon := api.NewServer(api.Deps{Store: catalog, Blobs: blobs, VaultRoot: vault, Cfg: cfg})
	t.Cleanup(daemon.Close)
	httpServer := httptest.NewServer(daemon.Handler())
	t.Cleanup(httpServer.Close)
	connection := daemonconn.New(httpServer.URL, cfg.Server.APIKey)
	t.Cleanup(func() { require.NoError(t, connection.Close()) })
	lease := newDaemonLeaseWith(func(context.Context) (*daemonconn.Connection, error) {
		return connection, nil
	}, func(*daemonconn.Connection) error { return nil })
	server := newServerWithOptionsAndDaemon(testImplementation(), ServerOptions{}, lease)

	call := func(name string, arguments map[string]any) map[string]any {
		t.Helper()
		result := decodeResult(t, exchangeRaw(t, server, requestFor("tools/call", map[string]any{
			"name": name, "arguments": arguments,
		})))
		require.NotEqual(t, true, result["isError"], "%s returned an error: %v", name, result)
		return objectField(t, result, "structuredContent")
	}
	return catalog, call
}

func TestPeopleMCPReads(t *testing.T) {
	catalog, call := peopleMCPServer(t)
	survivor, err := catalog.CreatePerson(t.Context(), "Survivor", "operator")
	require.NoError(t, err)
	absorbed, err := catalog.CreatePerson(t.Context(), "Absorbed", "operator")
	require.NoError(t, err)
	identity, err := catalog.AddPersonIdentity(t.Context(), absorbed.PersonID, absorbed.Revision, store.PersonIdentity{
		Kind: "email", ValueDisplay: "absorbed@example.test", Origin: "operator",
		EvidenceKind: "operator_assertion", EvidenceID: "mcp-workflow", Confidence: "operator_asserted",
	})
	require.NoError(t, err)
	people := call("find_people", map[string]any{"query": "Absorbed"})
	items, ok := people["items"].([]any)
	require.True(t, ok)
	require.Len(t, items, 1)
	person, ok := items[0].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, absorbed.PersonID, person["person_id"])
	assert.Equal(t, "Absorbed", person["display_name"])
	assert.Equal(t, "absorbed@example.test", person["identity_label"])
	absorbedDetail := call("get_person", map[string]any{"person_id": absorbed.PersonID})
	assert.Equal(t, "Absorbed", absorbedDetail["display_name"])
	absorbedIdentities, ok := absorbedDetail["identities"].([]any)
	require.True(t, ok)
	require.Len(t, absorbedIdentities, 1)
	absorbedIdentity, ok := absorbedIdentities[0].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, identity.IdentityID, absorbedIdentity["identity_id"])
	absorbedRevision, ok := absorbedDetail["revision"].(float64)
	require.True(t, ok)

	_, err = catalog.MergePersons(t.Context(), survivor.PersonID, absorbed.PersonID, "00000000-0000-4000-8000-000000000004",
		survivor.Revision, int64(absorbedRevision))
	require.NoError(t, err)
	merged := call("get_person", map[string]any{"person_id": survivor.PersonID})
	assert.EqualValues(t, survivor.Revision+1, merged["revision"])
	mergedIdentities, ok := merged["identities"].([]any)
	require.True(t, ok)
	require.Len(t, mergedIdentities, 1)
}

func TestGetPersonMCPFitsAssertionEnvelope(t *testing.T) {
	t.Parallel()
	const stamp = "2026-10-01T00:00:00Z"
	detail := api.PersonDetail{
		PersonID: "00000000-0000-4000-8000-000000000001", Revision: 1, DisplayName: "Ada", Origin: "operator", State: "curated", CreatedAt: stamp, UpdatedAt: stamp,
		Identities: []api.PersonIdentity{}, ExternalIdentities: []api.PersonExternalIdentity{},
	}
	for i := range 1000 {
		detail.Assertions = append(detail.Assertions, api.PhotoPersonAssertion{AssertionID: fmt.Sprintf("00000000-0000-4000-8000-%012d", i+10), ContentVersionID: fmt.Sprintf("00000000-0000-4000-9000-%012d", i+10), PersonID: detail.PersonID, Role: "author", Action: "assert", Note: strings.Repeat("\x01", 1000), Revision: 1, RecordedAt: detail.CreatedAt})
	}
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		page := detail
		cursor := r.URL.Query().Get("assertion_cursor")
		start := 0
		for start < len(detail.Assertions) && detail.Assertions[start].AssertionID <= cursor {
			start++
		}
		page.Assertions = detail.Assertions[start:]
		w.Header().Set("ETag", `"1"`)
		writeDaemonJSON(t, w, page)
	}))
	t.Cleanup(daemon.Close)
	lease := newDaemonLeaseWith(func(context.Context) (*daemonconn.Connection, error) {
		return daemonconn.New(daemon.URL, "synthetic-key"), nil
	}, func(*daemonconn.Connection) error { return nil })
	var all []api.PhotoPersonAssertion
	cursor := ""
	for {
		result, err := invokeReadTool(t.Context(), lease, "get_person", map[string]any{"person_id": detail.PersonID, "assertion_cursor": cursor})
		require.NoError(t, err)
		wire, err := json.Marshal(result)
		require.NoError(t, err)
		require.Less(t, len(wire), maxToolResponseBytes)
		raw, err := json.Marshal(result.StructuredContent)
		require.NoError(t, err)
		var page personDetailToolOutput
		require.NoError(t, json.Unmarshal(raw, &page))
		require.NotEmpty(t, page.Assertions)
		all = append(all, page.Assertions...)
		if !page.AssertionsTruncated {
			break
		}
		require.Greater(t, page.AssertionsNextCursor, cursor)
		require.Equal(t, page.Assertions[len(page.Assertions)-1].AssertionID, page.AssertionsNextCursor)
		cursor = page.AssertionsNextCursor
	}
	require.Equal(t, detail.Assertions, all)
}

// TestGetPersonMCPFitsUnpageableIdentitiesUnderEnvelopeBudget covers a person
// whose identities (not paginated, unlike assertions) alone exceed a
// fraction of the response budget while the complete MCP envelope still
// fits: get_person must not reject a response the real 1 MiB ceiling would
// have carried. Every identity and external identity below is a value the
// store would actually accept: distinct per-row identifiers (the store
// rejects duplicate identity and external-identity tuples), and evidence
// kind, confidence, and UID classification drawn from the store's real
// enumerations, so the fixture reflects a response the daemon could produce.
func TestGetPersonMCPFitsUnpageableIdentitiesUnderEnvelopeBudget(t *testing.T) {
	t.Parallel()
	const stamp = "2026-10-01T00:00:00Z"
	detail := api.PersonDetail{
		PersonID: "00000000-0000-4000-8000-000000000002", Revision: 1, DisplayName: "Long Identities", Origin: "operator", State: "curated", CreatedAt: stamp, UpdatedAt: stamp,
		Assertions: []api.PhotoPersonAssertion{{
			AssertionID: "00000000-0000-4000-8000-000000000003", ContentVersionID: "00000000-0000-4000-9000-000000000003",
			PersonID: "00000000-0000-4000-8000-000000000002", Role: "author", Action: "assert", Revision: 1, RecordedAt: stamp,
		}},
	}
	for i := range 200 {
		value := strings.Repeat("a", 297) + fmt.Sprintf("%03d", i) + "@example.test"
		detail.Identities = append(detail.Identities, api.PersonIdentity{
			IdentityID: fmt.Sprintf("00000000-0000-4000-8000-%012d", i+100),
			Kind:       "email", ValueDisplay: value, ValueNormalized: value,
			ScopeKind: strings.Repeat("k", 64), ScopeValue: strings.Repeat("s", 300),
			Normalization: "email_v1", Origin: "operator", EvidenceKind: "operator_assertion",
			EvidenceID: strings.Repeat("i", 256), Confidence: "operator_asserted", RecordedAt: stamp,
		})
	}
	for i := range 64 {
		detail.ExternalIdentities = append(detail.ExternalIdentities, api.PersonExternalIdentity{
			System:    "msgvault",
			ArchiveID: strings.Repeat("r", 120) + fmt.Sprintf("%08d", i),
			UID:       strings.Repeat("u", 248) + fmt.Sprintf("%08d", i),
			UIDKind:   "vcard_uid", UIDState: "current",
			DisplayNameSnapshot: strings.Repeat("n", 200), LinkedAt: stamp, UpdatedAt: stamp,
		})
	}
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"1"`)
		writeDaemonJSON(t, w, detail)
	}))
	t.Cleanup(daemon.Close)
	lease := newDaemonLeaseWith(func(context.Context) (*daemonconn.Connection, error) {
		return daemonconn.New(daemon.URL, "synthetic-key"), nil
	}, func(*daemonconn.Connection) error { return nil })
	result, err := invokeReadTool(t.Context(), lease, "get_person", map[string]any{"person_id": detail.PersonID})
	require.NoError(t, err)
	require.False(t, result.IsError, "%v", result.StructuredContent)
	wire, err := json.Marshal(result)
	require.NoError(t, err)
	require.Less(t, len(wire), maxToolResponseBytes)
	raw, err := json.Marshal(result.StructuredContent)
	require.NoError(t, err)
	var page personDetailToolOutput
	require.NoError(t, json.Unmarshal(raw, &page))
	require.Len(t, page.Identities, 200)
	require.Len(t, page.ExternalIdentities, 64)
	require.False(t, page.AssertionsTruncated)
	require.Len(t, page.Assertions, 1)
}

func TestFindPeoplePagesWithinResponseLimit(t *testing.T) {
	catalog, call := peopleMCPServer(t)
	// Each control character escapes to six bytes in the structured copy and
	// seven in its text copy, so 250 of these names exceed the response limit.
	name := strings.Repeat("\x01", document.MaxPersonDisplayNameBytes-1) + "A"
	want := map[string]bool{}
	for range 250 {
		person, err := catalog.CreatePerson(t.Context(), name, "operator")
		require.NoError(t, err)
		want[person.PersonID] = true
	}
	seen := map[string]bool{}
	arguments := map[string]any{"limit": 250}
	for pages := 0; ; pages++ {
		require.Less(t, pages, 10)
		page := call("find_people", arguments)
		items, ok := page["items"].([]any)
		require.True(t, ok)
		require.NotEmpty(t, items)
		for _, item := range items {
			person, ok := item.(map[string]any)
			require.True(t, ok)
			id, ok := person["person_id"].(string)
			require.True(t, ok)
			require.False(t, seen[id], "person %s repeated", id)
			seen[id] = true
		}
		cursor, _ := page["next_cursor"].(string)
		if cursor == "" {
			require.Positive(t, pages, "a full page should not fit in one response")
			break
		}
		arguments = map[string]any{"limit": 250, "cursor": cursor}
	}
	assert.Equal(t, want, seen)
}
