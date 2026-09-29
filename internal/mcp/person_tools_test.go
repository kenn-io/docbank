package mcp

import (
	"context"
	"net/http/httptest"
	"path/filepath"
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
	readOnly := catalogMap(toolCatalog(false, false, false))
	tool := readOnly["get_person"]
	require.NotNil(t, tool)
	assert.True(t, tool.Annotations.ReadOnlyHint)
	withWrites := catalogMap(toolCatalog(true, true, true))
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
