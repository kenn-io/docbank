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

func TestPeopleMCPReads(t *testing.T) {
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
	survivor, err := catalog.CreatePerson(t.Context(), "Survivor", "operator")
	require.NoError(t, err)
	absorbed, err := catalog.CreatePerson(t.Context(), "Absorbed", "operator")
	require.NoError(t, err)
	identity, err := catalog.AddPersonIdentity(t.Context(), absorbed.PersonID, absorbed.Revision, store.PersonIdentity{
		Kind: "email", ValueDisplay: "absorbed@example.test", Origin: "operator",
		EvidenceKind: "operator_assertion", EvidenceID: "mcp-workflow", Confidence: "operator_asserted",
	})
	require.NoError(t, err)
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
