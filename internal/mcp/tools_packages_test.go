package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"
	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/blob"
	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/docbank/internal/processing"
	"go.kenn.io/docbank/internal/store"
)

func TestPackageReadToolsArePublishedAsBoundedReads(t *testing.T) {
	catalog := toolCatalog(false)
	byName := make(map[string]bool, len(catalog))
	for _, tool := range catalog {
		byName[tool.Name] = true
		if tool.Name == "list_packages" || tool.Name == "get_package" ||
			tool.Name == "list_package_members" || tool.Name == "get_package_record" ||
			tool.Name == "lookup_bates_label" {
			require.NotNil(t, tool.Annotations, tool.Name)
			assert.True(t, tool.Annotations.ReadOnlyHint, tool.Name)
			assert.True(t, tool.Annotations.IdempotentHint, tool.Name)
		}
	}
	for _, name := range []string{"list_packages", "get_package", "list_package_members", "get_package_record", "lookup_bates_label"} {
		assert.True(t, byName[name], name)
	}
	input := mustResolveSchema(catalogTool(t, catalog, "list_packages").InputSchema)
	require.NoError(t, input.Validate(&map[string]any{"page_size": 250.0}))
	require.Error(t, input.Validate(&map[string]any{"page_size": 251.0}))
	require.Error(t, input.Validate(&map[string]any{"unexpected": true}))
}

func TestPreflightLoadFilePackageAndReadsUseTypedDaemonRoutes(t *testing.T) {
	const id = "11111111-1111-4111-8111-111111111111"
	root := t.TempDir()
	hash := strings.Repeat("a", 64)
	preflight := api.PackagePreflight{PreflightID: id, SourceKind: "root", SourceRef: root,
		ProfileSHA256: hash, MappingSHA256: hash, ManifestSHA256: hash,
		Volumes: []api.PackageVolume{}, Diagnostics: []api.PackageDiagnostic{}, CreatedAt: "2026-09-21T00:00:00Z",
		ExpiresAt: "2026-09-22T00:00:00Z"}
	var createRequest api.PackagePreflightRequest
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/packages/preflights":
			if !assert.NoError(t, json.NewDecoder(r.Body).Decode(&createRequest)) {
				return
			}
			assert.NoError(t, json.NewEncoder(w).Encode(preflight))
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/packages/preflights/"+id:
			assert.NoError(t, json.NewEncoder(w).Encode(preflight))
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/packages/preflights/"+id+"/diagnostics":
			assert.Equal(t, "7", r.URL.Query().Get("limit"))
			assert.NoError(t, json.NewEncoder(w).Encode(api.PackageDiagnosticPage{Diagnostics: []api.PackageDiagnostic{}, Total: 0}))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(daemon.Close)
	lease := newDaemonLeaseWith(func(context.Context) (*daemonconn.Connection, error) {
		return daemonconn.New(daemon.URL, "synthetic-key"), nil
	}, func(*daemonconn.Connection) error { return nil })

	created, err := preflightLoadFilePackage(t.Context(), lease, []byte(`{"source_path":`+mustJSONString(t, root)+`,"profile":"relativity-dat-v1","encoding":"utf-8"}`))
	require.NoError(t, err)
	assert.Equal(t, id, created.PreflightID)
	assert.Equal(t, root, createRequest.SourceRef)

	got, err := getPackagePreflight(t.Context(), lease, []byte(`{"preflight_id":"`+id+`"}`))
	require.NoError(t, err)
	assert.Equal(t, id, got.PreflightID)
	page, err := listPackagePreflightDiagnostics(t.Context(), lease, []byte(`{"preflight_id":"`+id+`","limit":7}`))
	require.NoError(t, err)
	assert.Empty(t, page.Diagnostics)
}

func TestPackagePreflightAcceptsFullDiagnosticSummaryBound(t *testing.T) {
	hash := strings.Repeat("a", 64)
	value := api.PackagePreflight{PreflightID: uuid.NewString(), SourceKind: "root", SourceRef: "synthetic",
		ProfileSHA256: hash, MappingSHA256: hash, ManifestSHA256: hash, DiagnosticCount: 250,
		Diagnostics: make([]api.PackageDiagnostic, 250), CreatedAt: "2026-09-21T00:00:00Z",
		ExpiresAt: "2026-09-22T00:00:00Z"}
	require.NoError(t, validatePackagePreflightResult(value))
	value.Diagnostics = append(value.Diagnostics, api.PackageDiagnostic{})
	require.Error(t, validatePackagePreflightResult(value))
}

func mustJSONString(t *testing.T, value string) string {
	t.Helper()
	encoded, err := json.Marshal(value)
	require.NoError(t, err)
	return string(encoded)
}

func TestPackagePreflightToolsAreCandidateReadsAndGatedWrite(t *testing.T) {
	readOnly := catalogNames(toolCatalog(false))
	assert.Contains(t, readOnly, "get_package_preflight")
	assert.Contains(t, readOnly, "list_package_preflight_diagnostics")
	assert.NotContains(t, readOnly, "preflight_load_file_package")

	enabled := catalogMap(toolCatalog(true))
	preflight := enabled["preflight_load_file_package"]
	require.NotNil(t, preflight)
	require.NotNil(t, preflight.Annotations)
	assert.False(t, preflight.Annotations.ReadOnlyHint)
	assert.False(t, preflight.Annotations.IdempotentHint)
	assert.Equal(t, new(false), preflight.Annotations.DestructiveHint)
	assert.Equal(t, new(true), preflight.Annotations.OpenWorldHint)
	assertSchemaAccepts(t, preflight.InputSchema, map[string]any{
		"source_path": "/tmp/production", "profile": "relativity-dat-v1", "encoding": "utf-8",
		"mapping_json": `{"contract":"loadfile-mapping/v1"}`,
	})
	assertSchemaRejects(t, preflight.InputSchema, map[string]any{
		"source_path": "/tmp/production", "profile": "relativity-dat-v1", "encoding": "utf-8", "unknown": true,
	})
}

func TestClassifyPackagePreflightSourceUsesAbsoluteDirectoriesAndZIPFiles(t *testing.T) {
	root := t.TempDir()
	directory, err := classifyPackagePreflightSource(root)
	require.NoError(t, err)
	assert.Equal(t, packagePreflightSource{kind: "root", path: root}, directory)

	zipPath := filepath.Join(root, "production.zip")
	require.NoError(t, os.WriteFile(zipPath, []byte("synthetic zip bytes"), 0o600))
	archive, err := classifyPackagePreflightSource(zipPath)
	require.NoError(t, err)
	assert.Equal(t, packagePreflightSource{kind: "zip", path: zipPath}, archive)

	_, err = classifyPackagePreflightSource(filepath.Join(root, "production.dat"))
	require.Error(t, err)
}

func TestPackageCustodianToolsExposeCandidatesAndExactWrites(t *testing.T) {
	readOnly := catalogMap(toolCatalog(false))
	for _, name := range []string{"list_package_custodians", "find_people"} {
		tool := readOnly[name]
		require.NotNil(t, tool, name)
		assert.True(t, tool.Annotations.ReadOnlyHint, name)
	}
	enabled := catalogMap(toolCatalog(true))
	for _, name := range []string{"resolve_package_custodian", "assign_package_custodian"} {
		tool := enabled[name]
		require.NotNil(t, tool, name)
		assert.False(t, tool.Annotations.ReadOnlyHint, name)
		assert.False(t, tool.Annotations.IdempotentHint, name)
	}
	assertSchemaRejects(t, enabled["list_package_custodians"].InputSchema, map[string]any{
		"package_id": "11111111-1111-4111-8111-111111111111", "row_id": "",
	})
	assertSchemaAccepts(t, enabled["resolve_package_custodian"].InputSchema, map[string]any{
		"assignment_id": "11111111-1111-4111-8111-111111111111",
		"person_id":     "22222222-2222-4222-8222-222222222222", "if_match_revision": 1,
	})
}

func TestExportLoadFilePackageIsAnExactGatedFileWrite(t *testing.T) {
	readOnly := catalogMap(toolCatalog(false))
	assert.NotContains(t, readOnly, "export_load_file_package")
	tool := catalogMap(toolCatalog(true))["export_load_file_package"]
	require.NotNil(t, tool)
	assert.False(t, tool.Annotations.ReadOnlyHint)
	assert.False(t, tool.Annotations.IdempotentHint)
	assert.Equal(t, new(true), tool.Annotations.DestructiveHint)
	assertSchemaAccepts(t, tool.InputSchema, map[string]any{
		"snapshot_id": "11111111-1111-4111-8111-111111111111", "source_package_id": "22222222-2222-4222-8222-222222222222",
		"bates_allocation_id": "33333333-3333-4333-8333-333333333333", "profile_id": "export-dat-pdf-v1",
		"destination_path": "/tmp/synthetic-production.zip", "overwrite": false,
	})
	assertSchemaRejects(t, tool.InputSchema, map[string]any{
		"snapshot_id": "11111111-1111-4111-8111-111111111111", "profile_id": "unknown",
		"destination_path": "/tmp/synthetic-production.zip", "overwrite": false,
	})
	lease := newDaemonLeaseWith(func(context.Context) (*daemonconn.Connection, error) {
		return nil, errors.New("daemon must not be acquired")
	}, func(*daemonconn.Connection) error { return nil })
	_, err := exportLoadFilePackage(t.Context(), lease, []byte(`{
		"snapshot_id":"11111111-1111-4111-8111-111111111111",
		"profile_id":"export-csv-natives-v1","destination_path":"relative.zip","overwrite":false}`))
	require.Error(t, err)
}

func TestExportLoadFilePackagePublishesIndependentlyVerifiedArchive(t *testing.T) {
	ctx := t.Context()
	root := t.TempDir()
	catalog, err := store.Open(filepath.Join(root, "docbank.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, catalog.Close()) })
	blobs, err := blob.New(store.NewPackCatalog(catalog), filepath.Join(root, "blobs"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, blobs.Close()) })
	written, err := blobs.WriteDetailedContext(ctx, strings.NewReader("synthetic native\n"))
	require.NoError(t, err)
	encoding, err := written.EncodingName()
	require.NoError(t, err)
	physical := store.BlobPhysical{Encoding: encoding, StoredBytes: written.StoredSize,
		PackEligible: written.PackEligible, MD5: written.MD5, Created: written.Created}
	node, err := catalog.CreateFile(ctx, catalog.RootID(), "synthetic.txt", written.Hash, written.Size, "text/plain", physical)
	require.NoError(t, err)
	occurrence := strings.Repeat("a", 32)
	snapshot, err := catalog.SealCollectionSnapshot(ctx, store.SnapshotSealRequest{SnapshotID: uuid.NewString(),
		Members: []store.CollectionSnapshotMember{{Ordinal: 1, OccurrenceID: occurrence, NodeID: node.ID,
			ContentVersionID: node.CurrentVersionID, BlobSHA256: written.Hash, Size: written.Size,
			FamilyID: occurrence, FamilyOrder: 1, DisplayName: node.Name, FrozenFieldsJSON: "{}", DocumentKind: "file",
			Representations: []store.CollectionSnapshotRepresentation{{OccurrenceID: occurrence, Role: "native",
				Ordinal: 1, BlobSHA256: written.Hash, Size: written.Size, MediaType: "text/plain", Status: "available",
				TextAuthority: "none"}}}}})
	require.NoError(t, err)
	var archive bytes.Buffer
	built, err := processing.WriteLoadFileExport(ctx, catalog, blobs, processing.LoadFileExportRequest{
		SnapshotID: snapshot.SnapshotID, ProfileID: "export-csv-natives-v1",
	}, &archive)
	require.NoError(t, err)
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/packages/exports":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			assert.NoError(t, json.NewEncoder(w).Encode(api.PackageExportTicket{URL: "/download", Name: "production.zip",
				SnapshotID: snapshot.SnapshotID, ProfileID: "export-csv-natives-v1",
				ArchiveSHA256: built.Receipt.ArchiveSHA256, ManifestSHA256: built.Receipt.ManifestSHA256,
				CrosswalkSHA256: built.Receipt.CrosswalkSHA256, Size: built.Receipt.Size,
				Records: built.Receipt.RecordCount, Pages: built.Receipt.PageCount}))
		case "/download":
			_, _ = w.Write(archive.Bytes())
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(daemon.Close)
	lease := newDaemonLeaseWith(func(context.Context) (*daemonconn.Connection, error) {
		return daemonconn.New(daemon.URL, "synthetic-key"), nil
	}, func(*daemonconn.Connection) error { return nil })
	destination := filepath.Join(t.TempDir(), "production.zip")
	result, err := exportLoadFilePackage(ctx, lease, []byte(`{"snapshot_id":"`+snapshot.SnapshotID+
		`","profile_id":"export-csv-natives-v1","destination_path":`+mustJSONString(t, destination)+`,"overwrite":false}`))
	require.NoError(t, err)
	require.Equal(t, "published", result.State)
	require.Equal(t, built.Receipt.ArchiveSHA256, result.ArchiveSHA256)
	published, err := os.ReadFile(destination)
	require.NoError(t, err)
	require.Equal(t, archive.Bytes(), published)
}

func catalogTool(t *testing.T, catalog []*sdkmcp.Tool, name string) *sdkmcp.Tool {
	t.Helper()
	for _, tool := range catalog {
		if tool.Name == name {
			return tool
		}
	}
	t.Fatalf("tool %s not found", name)
	return nil
}
