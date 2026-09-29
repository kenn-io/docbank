package mcp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
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

func TestPeopleMCPWriteOptIn(t *testing.T) {
	readOnly := catalogMap(toolCatalog(false, false, false, false))
	for _, name := range []string{"get_person", "list_person_custodians"} {
		assert.Contains(t, readOnly, name)
	}
	for _, name := range []string{"create_person", "rename_person", "retire_person", "merge_people", "split_person"} {
		assert.NotContains(t, readOnly, name)
	}

	withWrites := catalogMap(toolCatalog(false, false, false, true))
	for _, name := range []string{"create_person", "rename_person", "retire_person", "merge_people", "split_person"} {
		tool := withWrites[name]
		require.NotNil(t, tool)
		require.NotNil(t, tool.Annotations)
		assert.False(t, tool.Annotations.ReadOnlyHint)
		assertSchemaContract(t, tool.InputSchema)
		assertSchemaContract(t, tool.OutputSchema)
	}
	assertSchemaAccepts(t, readOnly["list_person_custodians"].OutputSchema, map[string]any{
		"items": []any{
			map[string]any{"assignment_id": "00000000-0000-4000-8000-000000000001", "scope_kind": "collection", "ingest_id": "00000000-0000-4000-8000-000000000011", "raw_label": "Collection", "rank": "primary", "basis": "operator_assigned", "source_ref": "synthetic", "revision": 1, "recorded_at": "2026-09-29T00:00:00Z"},
			map[string]any{"assignment_id": "00000000-0000-4000-8000-000000000002", "scope_kind": "package", "package_id": "00000000-0000-4000-8000-000000000012", "raw_label": "Package", "rank": "primary", "basis": "package_column", "source_ref": "synthetic", "revision": 1, "recorded_at": "2026-09-29T00:00:00Z"},
			map[string]any{"assignment_id": "00000000-0000-4000-8000-000000000003", "scope_kind": "document", "node_id": 1, "content_version_id": "00000000-0000-4000-8000-000000000013", "raw_label": "Document", "rank": "primary", "basis": "operator_assigned", "source_ref": "synthetic", "revision": 1, "recorded_at": "2026-09-29T00:00:00Z"},
		}, "total": 3, "ttlMs": 0, "cacheScope": "private",
	})
	server := newServerWithOptions(testImplementation(), ServerOptions{AllowPersonEdits: true})
	discovery := decodeResult(t, exchangeRaw(t, server, requestFor("server/discover", nil)))
	assert.Equal(t, catalogInstructions(false, false, false, true), discovery["instructions"])
}

func TestPeopleMCPWorkflow(t *testing.T) {
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
	server := newServerWithOptionsAndDaemon(testImplementation(), ServerOptions{AllowPersonEdits: true}, lease)

	call := func(name string, arguments map[string]any) map[string]any {
		t.Helper()
		result := decodeResult(t, exchangeRaw(t, server, requestFor("tools/call", map[string]any{
			"name": name, "arguments": arguments,
		})))
		require.NotEqual(t, true, result["isError"], "%s returned an error: %v", name, result)
		return objectField(t, result, "structuredContent")
	}
	callError := func(name string, arguments map[string]any, code string) {
		t.Helper()
		result := decodeResult(t, exchangeRaw(t, server, requestFor("tools/call", map[string]any{
			"name": name, "arguments": arguments,
		})))
		require.Equal(t, true, result["isError"], "%s unexpectedly succeeded: %v", name, result)
		assert.Equal(t, code, objectField(t, result, "structuredContent")["code"])
	}
	created := call("create_person", map[string]any{"display_name": "Survivor"})
	survivorID, ok := created["person_id"].(string)
	require.True(t, ok)
	assert.Equal(t, "Survivor", created["display_name"])
	assert.EqualValues(t, 1, created["revision"])
	absorbed := call("create_person", map[string]any{"display_name": "Absorbed"})
	absorbedID, ok := absorbed["person_id"].(string)
	require.True(t, ok)
	assert.Equal(t, "Absorbed", absorbed["display_name"])
	assert.EqualValues(t, 1, absorbed["revision"])
	collection, err := catalog.BeginIngest(t.Context(), "mcp", "synthetic collection")
	require.NoError(t, err)
	node, err := catalog.IngestFileExact(t.Context(), collection, catalog.RootID(), "mcp-document.txt", strings.Repeat("a", 64), 16, "text/plain", "mcp-document.txt", "")
	require.NoError(t, err)
	_, err = catalog.SetCustodian(t.Context(), store.CustodianRequest{
		Scope: store.CustodianScope{Kind: "collection", IngestID: collection.ID()}, PersonID: survivorID,
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
		Scope: store.CustodianScope{Kind: "package", PackageID: pkg.PackageID}, PersonID: survivorID,
		RawLabel: "Package owner", Rank: "primary", Basis: "operator_assigned", SourceRef: "mcp-package", IfMatchRevision: 1,
	})
	require.NoError(t, err)
	_, err = catalog.SetCustodian(t.Context(), store.CustodianRequest{
		Scope: store.CustodianScope{Kind: "document", NodeID: node.ID, ContentVersionID: node.CurrentVersionID}, PersonID: survivorID,
		RawLabel: "Document owner", Rank: "primary", Basis: "operator_assigned", SourceRef: "mcp-document", IfMatchRevision: 1,
	})
	require.NoError(t, err)
	renamed := call("rename_person", map[string]any{"person_id": absorbedID, "if_match_revision": 1, "display_name": "Absorbed Renamed"})
	assert.Equal(t, absorbedID, renamed["person_id"])
	assert.Equal(t, "Absorbed Renamed", renamed["display_name"])
	assert.EqualValues(t, 2, renamed["revision"])
	found := call("find_people", map[string]any{"query": "Survivor", "limit": 10})
	people, ok := found["items"].([]any)
	require.True(t, ok)
	require.Len(t, people, 1)
	personSummary, ok := people[0].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, survivorID, personSummary["person_id"])
	assert.Equal(t, "Survivor", personSummary["display_name"])

	identity, err := catalog.AddPersonIdentity(t.Context(), absorbedID, 2, store.PersonIdentity{
		Kind: "email", ValueDisplay: "absorbed@example.test", Origin: "operator",
		EvidenceKind: "operator_assertion", EvidenceID: "mcp-workflow", Confidence: "operator_asserted",
	})
	require.NoError(t, err)
	absorbedDetail := call("get_person", map[string]any{"person_id": absorbedID})
	assert.Equal(t, absorbedID, absorbedDetail["person_id"])
	assert.Equal(t, "Absorbed Renamed", absorbedDetail["display_name"])
	assert.EqualValues(t, 3, absorbedDetail["revision"])
	absorbedIdentities, ok := absorbedDetail["identities"].([]any)
	require.True(t, ok)
	require.Len(t, absorbedIdentities, 1)
	absorbedIdentity, ok := absorbedIdentities[0].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, identity.IdentityID, absorbedIdentity["identity_id"])

	mergeID := "00000000-0000-4000-8000-000000000004"
	merged := call("merge_people", map[string]any{
		"survivor_person_id": survivorID, "survivor_revision": 1,
		"absorbed_person_id": absorbedID, "absorbed_revision": 3, "operation_id": mergeID,
	})
	assert.Equal(t, survivorID, merged["survivor_person_id"])
	assert.Equal(t, absorbedID, merged["absorbed_person_id"])
	assert.Equal(t, "Absorbed Renamed", merged["absorbed_display_name"])
	assert.EqualValues(t, 1, merged["survivor_revision_before"])
	assert.EqualValues(t, 2, merged["survivor_revision_after"])
	moved := objectField(t, merged, "moved")
	assert.EqualValues(t, 1, moved["identities"])
	callError("rename_person", map[string]any{"person_id": survivorID, "if_match_revision": 1, "display_name": "Rejected"}, "stale_revision")

	survivor := call("get_person", map[string]any{"person_id": survivorID})
	assert.Equal(t, survivorID, survivor["person_id"])
	assert.EqualValues(t, 2, survivor["revision"])
	identities, ok := survivor["identities"].([]any)
	require.True(t, ok)
	require.Len(t, identities, 1)
	survivorIdentity, ok := identities[0].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, identity.IdentityID, survivorIdentity["identity_id"])

	splitID := "00000000-0000-4000-8000-000000000005"
	split := call("split_person", map[string]any{
		"person_id": survivorID, "if_match_revision": 2, "operation_id": splitID,
		"display_name": "Separated", "identity_ids": []string{identity.IdentityID},
	})
	assert.Equal(t, splitID, split["operation_id"])
	assert.Equal(t, survivorID, split["source_person_id"])
	assert.EqualValues(t, 3, split["source_revision_after"])
	newPersonID, ok := split["new_person_id"].(string)
	require.True(t, ok)
	assert.NotEqual(t, survivorID, newPersonID)
	assert.Equal(t, []any{identity.IdentityID}, split["moved_identity_ids"])
	sourceRename := call("rename_person", map[string]any{"person_id": survivorID, "if_match_revision": 3, "display_name": "Survivor after split"})
	assert.EqualValues(t, 4, sourceRename["revision"])

	separated := call("get_person", map[string]any{"person_id": newPersonID})
	assert.Equal(t, "Separated", separated["display_name"])
	assert.EqualValues(t, 1, separated["revision"])
	separatedIdentities, ok := separated["identities"].([]any)
	require.True(t, ok)
	require.Len(t, separatedIdentities, 1)
	separatedIdentity, ok := separatedIdentities[0].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, identity.IdentityID, separatedIdentity["identity_id"])
	callError("split_person", map[string]any{
		"person_id": survivorID, "if_match_revision": 3, "operation_id": "00000000-0000-4000-8000-000000000006",
		"display_name": "Empty",
	}, "invalid_person")
	retired := call("retire_person", map[string]any{"person_id": newPersonID, "if_match_revision": 1})
	assert.Equal(t, newPersonID, retired["person_id"])
	assert.Equal(t, "retired", retired["state"])
	assert.EqualValues(t, 2, retired["revision"])
	callError("get_person", map[string]any{"person_id": newPersonID}, "not_found")
	page := call("list_person_custodians", map[string]any{"person_id": survivorID, "limit": 10})
	assert.EqualValues(t, 3, page["total"])
	items, ok := page["items"].([]any)
	require.True(t, ok)
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

func TestPersonWriteTreatsMalformedSuccessAsUnknown(t *testing.T) {
	const personID = "00000000-0000-4000-8000-000000000001"
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.MarshalWrite(w, api.Person{PersonID: personID, DisplayName: "Synthetic", Origin: "operator", State: "curated", Revision: 1,
			CreatedAt: "2026-09-28T00:00:00Z", UpdatedAt: "2026-09-28T00:00:00Z"})
	}))
	t.Cleanup(server.Close)
	lease := newDaemonLeaseWith(func(context.Context) (*daemonconn.Connection, error) {
		return daemonconn.New(server.URL, "synthetic-key"), nil
	}, func(*daemonconn.Connection) error { return nil })
	validator := mustResolveSchema(catalogMap(toolCatalog(false, false, false, true))["create_person"].OutputSchema)
	_, err := executePersonWriteTool(t.Context(), lease, "create_person", validator, []byte(`{"display_name":"Synthetic"}`))
	require.ErrorIs(t, err, errProcessingOutcomeUnknown)
	assert.Equal(t, int32(1), requests.Load())
}

func TestPersonSplitWriteTreatsMalformedFenceAsUnknown(t *testing.T) {
	const (
		personID    = "00000000-0000-4000-8000-000000000001"
		newPersonID = "00000000-0000-4000-8000-000000000002"
		operationID = "00000000-0000-4000-8000-000000000003"
	)
	for _, test := range []struct {
		name, etag string
		revision   int64
	}{
		{name: "missing revision", etag: `"2"`},
		{name: "missing etag", revision: 2},
		{name: "mismatched etag", revision: 2, etag: `"1"`},
	} {
		t.Run(test.name, func(t *testing.T) {
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				requests.Add(1)
				w.Header().Set("Content-Type", "application/json")
				if test.etag != "" {
					w.Header().Set("ETag", test.etag)
				}
				_ = json.MarshalWrite(w, api.PersonSplitReceipt{OperationID: operationID, SourcePersonID: personID, NewPersonID: newPersonID,
					SourceRevisionAfter: test.revision, MovedIdentityIDs: []string{}, CreatedAt: "2026-09-28T00:00:00Z"})
			}))
			t.Cleanup(server.Close)
			lease := newDaemonLeaseWith(func(context.Context) (*daemonconn.Connection, error) {
				return daemonconn.New(server.URL, "synthetic-key"), nil
			}, func(*daemonconn.Connection) error { return nil })
			validator := mustResolveSchema(catalogMap(toolCatalog(false, false, false, true))["split_person"].OutputSchema)
			_, err := executePersonWriteTool(t.Context(), lease, "split_person", validator, []byte(`{"person_id":"00000000-0000-4000-8000-000000000001","if_match_revision":1,"operation_id":"00000000-0000-4000-8000-000000000003","display_name":"Synthetic split"}`))
			require.ErrorIs(t, err, errProcessingOutcomeUnknown)
			require.Equal(t, int32(1), requests.Load())
		})
	}
}
