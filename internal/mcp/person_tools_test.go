package mcp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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
	readOnly := catalogMap(toolCatalog(false, false, false))
	for _, name := range []string{"get_person", "list_person_custodians"} {
		tool := readOnly[name]
		require.NotNil(t, tool, name)
		assert.True(t, tool.Annotations.ReadOnlyHint, name)
	}
	withWrites := catalogMap(toolCatalog(true, true, true))
	for _, name := range []string{"create_person", "rename_person", "retire_person", "merge_people", "split_person"} {
		assert.NotContains(t, withWrites, name)
	}
	assertSchemaAccepts(t, readOnly["list_person_custodians"].OutputSchema, map[string]any{
		"items": []any{
			map[string]any{"assignment_id": "00000000-0000-4000-8000-000000000001", "scope_kind": "collection", "ingest_id": "00000000-0000-4000-8000-000000000011", "raw_label": "Collection", "rank": "primary", "basis": "operator_assigned", "source_ref": "synthetic", "revision": 1, "recorded_at": "2026-09-29T00:00:00Z"},
			map[string]any{"assignment_id": "00000000-0000-4000-8000-000000000002", "scope_kind": "package", "package_id": "00000000-0000-4000-8000-000000000012", "raw_label": "Package", "rank": "primary", "basis": "package_column", "source_ref": "synthetic", "revision": 1, "recorded_at": "2026-09-29T00:00:00Z"},
			map[string]any{"assignment_id": "00000000-0000-4000-8000-000000000003", "scope_kind": "document", "node_id": 1, "content_version_id": "00000000-0000-4000-8000-000000000013", "raw_label": "Document", "rank": "primary", "basis": "operator_assigned", "source_ref": "synthetic", "revision": 1, "recorded_at": "2026-09-29T00:00:00Z"},
		}, "total": 3, "ttlMs": 0, "cacheScope": "private",
	})
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

	collection, err := catalog.BeginIngest(t.Context(), "mcp", "synthetic collection")
	require.NoError(t, err)
	node, err := catalog.IngestFileExact(t.Context(), collection, catalog.RootID(), "mcp-document.txt", strings.Repeat("a", 64), 16, "text/plain", "mcp-document.txt", "")
	require.NoError(t, err)
	_, err = catalog.SetCustodian(t.Context(), store.CustodianRequest{
		Scope: store.CustodianScope{Kind: "collection", IngestID: collection.ID()}, PersonID: survivor.PersonID,
		RawLabel: "Collection owner", Rank: "primary", Basis: "operator_assigned", SourceRef: "mcp-collection", IfMatchRevision: 1,
	})
	require.NoError(t, err)
	profileJSON := "{}"
	profileDigest := sha256.Sum256([]byte(profileJSON))
	manifestHash, manifestSize, err := blobs.Write(strings.NewReader("synthetic manifest"))
	require.NoError(t, err)
	require.NoError(t, catalog.RecordBlob(t.Context(), manifestHash, manifestSize, store.BlobPhysical{Encoding: "raw", StoredBytes: manifestSize}))
	pkg, err := catalog.CreatePackage(t.Context(), store.PackageRequest{
		PackageID: "00000000-0000-4000-8000-000000000101", Direction: "received", PackageName: "mcp-package",
		ProfileSHA256: hex.EncodeToString(profileDigest[:]), ProfileJSON: profileJSON,
		MappingSHA256: hex.EncodeToString(profileDigest[:]), MappingJSON: profileJSON,
		ManifestSHA256: manifestHash, ManifestBlobSHA256: manifestHash,
		IngestID: collection.ID(), State: "importing",
	})
	require.NoError(t, err)
	_, err = catalog.SetCustodian(t.Context(), store.CustodianRequest{
		Scope: store.CustodianScope{Kind: "package", PackageID: pkg.PackageID}, PersonID: survivor.PersonID,
		RawLabel: "Package owner", Rank: "primary", Basis: "operator_assigned", SourceRef: "mcp-package", IfMatchRevision: 1,
	})
	require.NoError(t, err)
	_, err = catalog.SetCustodian(t.Context(), store.CustodianRequest{
		Scope: store.CustodianScope{Kind: "document", NodeID: node.ID, ContentVersionID: node.CurrentVersionID}, PersonID: survivor.PersonID,
		RawLabel: "Document owner", Rank: "primary", Basis: "operator_assigned", SourceRef: "mcp-document", IfMatchRevision: 1,
	})
	require.NoError(t, err)

	_, err = catalog.MergePersons(t.Context(), survivor.PersonID, absorbed.PersonID, "00000000-0000-4000-8000-000000000004",
		survivor.Revision, int64(absorbedRevision))
	require.NoError(t, err)
	merged := call("get_person", map[string]any{"person_id": survivor.PersonID})
	assert.EqualValues(t, survivor.Revision+1, merged["revision"])
	mergedIdentities, ok := merged["identities"].([]any)
	require.True(t, ok)
	require.Len(t, mergedIdentities, 1)

	page := call("list_person_custodians", map[string]any{"person_id": survivor.PersonID, "limit": 2})
	assert.EqualValues(t, 3, page["total"])
	items, ok := page["items"].([]any)
	require.True(t, ok)
	require.Len(t, items, 2)
	cursor, ok := page["next_cursor"].(string)
	require.True(t, ok)
	rest := call("list_person_custodians", map[string]any{"person_id": survivor.PersonID, "cursor": cursor})
	restItems, ok := rest["items"].([]any)
	require.True(t, ok)
	items = append(items, restItems...)
	require.Len(t, items, 3)
	bySource := make(map[string]map[string]any, len(items))
	for _, raw := range items {
		item, ok := raw.(map[string]any)
		require.True(t, ok)
		sourceRef, ok := item["source_ref"].(string)
		require.True(t, ok)
		bySource[sourceRef] = item
	}
	collectionOutput := bySource["mcp-collection"]
	require.Equal(t, collection.ID(), collectionOutput["ingest_id"])
	require.NotContains(t, collectionOutput, "package_id")
	require.NotContains(t, collectionOutput, "node_id")
	packageOutput := bySource["mcp-package"]
	require.Equal(t, pkg.PackageID, packageOutput["package_id"])
	require.NotContains(t, packageOutput, "ingest_id")
	require.NotContains(t, packageOutput, "node_id")
	documentOutput := bySource["mcp-document"]
	require.EqualValues(t, node.ID, documentOutput["node_id"])
	require.Equal(t, node.CurrentVersionID, documentOutput["content_version_id"])
	require.NotContains(t, documentOutput, "ingest_id")
	require.NotContains(t, documentOutput, "package_id")
}
