package mcp

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/go-pdf/fpdf"
	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/docbank/internal/pdfstamp"
)

const (
	testBatesNamespaceID  = "11111111-1111-4111-8111-111111111111"
	testBatesSnapshotID   = "22222222-2222-4222-8222-222222222222"
	testBatesOperationID  = "33333333-3333-4333-8333-333333333333"
	testBatesAllocationID = "44444444-4444-4444-8444-444444444444"
)

func TestBatesReadToolsCallBoundedDaemonContracts(t *testing.T) {
	recipe := testProfileID
	namespace := api.BatesNamespace{NamespaceID: testBatesNamespaceID, Prefix: "OUR", Suffix: "", Padding: 6,
		CreatedAt: "2026-09-21T12:00:00Z"}
	label := api.BatesPageLabel{Ordinal: 1, OccurrenceID: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		SourcePage: 1, OutputPage: 1, Label: "OUR000041"}
	allocation := api.BatesAllocation{AllocationID: testBatesAllocationID, NamespaceID: testBatesNamespaceID,
		SnapshotID: testBatesSnapshotID, RecipeSHA256: recipe, State: "reserved", StartSequence: 41,
		EndSequence: 41, Labels: []api.BatesPageLabel{label}, CreatedAt: "2026-09-21T12:00:00Z"}
	var calls []string
	daemon := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		calls = append(calls, request.Method+" "+request.URL.RequestURI())
		switch request.URL.Path {
		case "/api/v1/bates/namespaces":
			assert.Equal(t, http.MethodGet, request.Method)
			writeDaemonJSON(t, response, api.BatesNamespacePage{Items: []api.BatesNamespace{namespace}, Total: 1})
		case "/api/v1/bates/preview":
			assert.Equal(t, http.MethodPost, request.Method)
			var input api.BatesPlanRequest
			if !assert.NoError(t, decodeDaemonJSON(request.Body, &input)) {
				http.Error(response, "invalid synthetic request", http.StatusBadRequest)
				return
			}
			assert.Equal(t, testBatesNamespaceID, input.NamespaceID)
			assert.Equal(t, testBatesSnapshotID, input.SnapshotID)
			assert.Equal(t, int64(41), input.StartAt)
			assert.Empty(t, input.Pages)
			writeDaemonJSON(t, response, api.BatesPlan{Namespace: namespace, StartSequence: 41,
				EndSequence: 41, Labels: []api.BatesPageLabel{label}, StampedNothing: true})
		case "/api/v1/bates/allocations/" + testBatesAllocationID:
			assert.Equal(t, http.MethodGet, request.Method)
			writeDaemonJSON(t, response, allocation)
		case "/api/v1/bates/exports/candidates":
			assert.Equal(t, "OUR000041", request.URL.Query().Get("bates_label"))
			writeDaemonJSON(t, response, api.BatesCandidatePage{Items: []api.BatesCandidate{{
				ArtifactID: testBatesAllocationID, AllocationID: testBatesAllocationID, SnapshotID: testBatesSnapshotID,
				BlobSHA256: testProfileID, Size: 4096, MediaType: "application/pdf", PageCount: 1,
				ManifestSHA256: testProfileID, State: "verified", CreatedAt: "2026-09-21T12:00:00Z",
				Evidence: []api.BatesCandidateEvidence{{Kind: "label", OccurrenceID: label.OccurrenceID,
					Label: label.Label, OutputPage: 1}},
			}}})
		default:
			http.NotFound(response, request)
		}
	}))
	t.Cleanup(daemon.Close)
	server := newBatesToolTestServer(t, daemon.URL, false)

	listed := callToolResult(t, server, "list_bates_namespaces", map[string]any{"limit": 25})
	assert.EqualValues(t, 1, objectField(t, listed, "structuredContent")["total"])
	previewed := callToolResult(t, server, "preview_bates_stamp", batesPreviewArguments())
	assert.Equal(t, true, objectField(t, previewed, "structuredContent")["stamped_nothing"])
	read := callToolResult(t, server, "get_bates_allocation", map[string]any{"allocation_id": testBatesAllocationID})
	assert.Equal(t, testBatesAllocationID, objectField(t, read, "structuredContent")["allocation_id"])
	found := callToolResult(t, server, "find_bates_exports", map[string]any{"bates_label": "OUR000041", "limit": 10})
	assert.Len(t, objectField(t, found, "structuredContent")["items"], 1)
	assert.Equal(t, []string{
		"GET /api/v1/bates/namespaces?limit=25",
		"POST /api/v1/bates/preview",
		"GET /api/v1/bates/allocations/" + testBatesAllocationID,
		"GET /api/v1/bates/exports/candidates?bates_label=OUR000041&limit=10",
	}, calls)
}

func TestBatesWriteToolsAreOptInAndBindResponses(t *testing.T) {
	recipe, err := syntheticBatesRecipe().SHA256()
	require.NoError(t, err)
	namespace := api.BatesNamespace{NamespaceID: testBatesNamespaceID, Prefix: "OUR", Suffix: "", Padding: 6,
		CreatedAt: "2026-09-21T12:00:00Z"}
	allocation := api.BatesAllocation{AllocationID: testBatesAllocationID, NamespaceID: testBatesNamespaceID,
		SnapshotID: testBatesSnapshotID, RecipeSHA256: recipe, State: "reserved", StartSequence: 41,
		EndSequence: 41, Labels: []api.BatesPageLabel{{Ordinal: 1, OccurrenceID: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			SourcePage: 1, OutputPage: 1, Label: "OUR000041"}}, CreatedAt: "2026-09-21T12:00:00Z"}
	readOnly := newBatesToolTestServer(t, "http://127.0.0.1:1", false)
	missing := exchangeRaw(t, readOnly, requestFor("tools/call", map[string]any{
		"name": "ensure_bates_namespace", "arguments": map[string]any{"prefix": "OUR", "padding": 6},
	}))
	assert.NotZero(t, decodeWireError(t, missing).Code)

	daemon := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/api/v1/bates/namespaces":
			assert.Equal(t, http.MethodPost, request.Method)
			response.WriteHeader(http.StatusCreated)
			writeDaemonJSON(t, response, namespace)
		case "/api/v1/bates/allocations":
			assert.Equal(t, http.MethodPost, request.Method)
			var input api.BatesReserveRequest
			if !assert.NoError(t, decodeDaemonJSON(request.Body, &input)) {
				http.Error(response, "invalid synthetic request", http.StatusBadRequest)
				return
			}
			assert.Equal(t, api.BatesReserveRequest{OperationID: testBatesOperationID, SnapshotID: testBatesSnapshotID,
				Recipe: syntheticBatesRecipe()}, input, "reserve must send the reviewed recipe itself")
			response.WriteHeader(http.StatusCreated)
			writeDaemonJSON(t, response, allocation)
		default:
			http.NotFound(response, request)
		}
	}))
	t.Cleanup(daemon.Close)
	server := newBatesToolTestServer(t, daemon.URL, true)

	ensured := callToolResult(t, server, "ensure_bates_namespace", map[string]any{"prefix": "OUR", "padding": 6})
	assert.Equal(t, testBatesNamespaceID, objectField(t, ensured, "structuredContent")["namespace_id"])
	reserved := callToolResult(t, server, "reserve_bates_range", batesReserveArguments(t))
	assert.Equal(t, testBatesAllocationID, objectField(t, reserved, "structuredContent")["allocation_id"])
}

func TestBatesToolSchemasRejectUnboundedAndUnstableInputs(t *testing.T) {
	tools := catalogMap(toolCatalog(false, true))
	assertSchemaAccepts(t, tools["list_bates_namespaces"].InputSchema, map[string]any{"limit": 250})
	assertSchemaRejects(t, tools["list_bates_namespaces"].InputSchema, map[string]any{"limit": 251})
	assertSchemaRejects(t, tools["ensure_bates_namespace"].InputSchema, map[string]any{
		"prefix": "OUR", "padding": 11,
	})
	args := batesPreviewArguments()
	assertSchemaAccepts(t, tools["preview_bates_stamp"].InputSchema, args)
	args["start_at"] = -1
	assertSchemaRejects(t, tools["preview_bates_stamp"].InputSchema, args)
	reserve := batesReserveArguments(t)
	assertSchemaAccepts(t, tools["reserve_bates_range"].InputSchema, reserve)
	objectField(t, reserve, "recipe")["start_at"] = 0
	assertSchemaRejects(t, tools["reserve_bates_range"].InputSchema, reserve)
	publish := map[string]any{"allocation_id": testBatesAllocationID, "recipe": syntheticBatesRecipeArguments(t, syntheticBatesRecipe())}
	assertSchemaAccepts(t, tools["publish_bates_export"].InputSchema, publish)
	invalidRecipe := syntheticBatesRecipe()
	invalidRecipe.EngineIdentity.Options = []string{"onTop=true"}
	publish["recipe"] = syntheticBatesRecipeArguments(t, invalidRecipe)
	assertSchemaRejects(t, tools["publish_bates_export"].InputSchema, publish)
	assertSchemaAccepts(t, tools["find_bates_exports"].InputSchema, map[string]any{"bates_label": "OUR000041", "limit": 10})
	assertSchemaRejects(t, tools["find_bates_exports"].InputSchema, map[string]any{"bates_label": "OUR000041", "person_id": testBatesAllocationID})
	assertSchemaAccepts(t, tools["export_bates_file"].InputSchema, map[string]any{
		"allocation_id": testBatesAllocationID, "destination_path": "/tmp/production.pdf", "overwrite": false,
	})
}

func TestExportBatesFileRejectsRelativeDestinationBeforeDaemonAccess(t *testing.T) {
	raw, err := json.Marshal(map[string]any{"allocation_id": testBatesAllocationID,
		"destination_path": "relative.pdf", "overwrite": false})
	require.NoError(t, err)
	_, err = exportBatesFile(t.Context(), nil, raw, nil)
	assert.Error(t, err)
}

func TestBatesWriteReportsUnknownAuthorityOutcomeWithoutBlindRetry(t *testing.T) {
	var calls int
	daemon := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		calls++
		response.WriteHeader(http.StatusCreated)
		_, _ = response.Write([]byte("{"))
	}))
	t.Cleanup(daemon.Close)
	server := newBatesToolTestServer(t, daemon.URL, true)

	result := callToolResult(t, server, "ensure_bates_namespace", map[string]any{
		"prefix": "OUR", "padding": 6,
	})
	assert.Equal(t, true, result["isError"])
	assert.Equal(t, "bates_outcome_unknown", objectField(t, result, "structuredContent")["code"])
	assert.Equal(t, 1, calls)
}

func TestBatesExportToolsPublishAndReadVerifiedArtifacts(t *testing.T) {
	recipe := syntheticBatesRecipe()
	receipt := api.BatesExport{
		ArtifactID: testBatesAllocationID, AllocationID: testBatesAllocationID,
		BlobSHA256: testProfileID, Size: 4096, MediaType: "application/pdf", PageCount: 1,
		RecipeSHA256: testProfileID, ManifestSHA256: testProfileID, State: "verified",
		CreatedAt: "2026-09-21T12:00:00Z", Pages: []api.BatesArtifactPage{{
			Ordinal: 1, OccurrenceID: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			SourceBlobSHA256: testProfileID, SourcePage: 3, OutputPage: 1, Label: "OUR000041",
		}},
	}
	var calls []string
	daemon := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		calls = append(calls, request.Method+" "+request.URL.RequestURI())
		switch request.URL.Path {
		case "/api/v1/bates/exports":
			if request.Method == http.MethodPost {
				var input api.BatesExportRequest
				assert.NoError(t, decodeDaemonJSON(request.Body, &input))
				assert.Equal(t, testBatesAllocationID, input.AllocationID)
				assert.Equal(t, recipe, input.Recipe)
				response.WriteHeader(http.StatusCreated)
				writeDaemonJSON(t, response, receipt)
				return
			}
			writeDaemonJSON(t, response, api.BatesExportPage{Items: []api.BatesExport{receipt}, Total: 1})
		case "/api/v1/bates/exports/" + testBatesAllocationID:
			assert.Equal(t, http.MethodGet, request.Method)
			writeDaemonJSON(t, response, receipt)
		default:
			http.NotFound(response, request)
		}
	}))
	t.Cleanup(daemon.Close)
	readOnly := newBatesToolTestServer(t, daemon.URL, false)

	listed := callToolResult(t, readOnly, "list_bates_exports", map[string]any{"limit": 25})
	assert.EqualValues(t, 1, objectField(t, listed, "structuredContent")["total"])
	read := callToolResult(t, readOnly, "get_bates_export", map[string]any{"allocation_id": testBatesAllocationID})
	assert.Equal(t, testBatesAllocationID, objectField(t, read, "structuredContent")["artifact_id"])

	writes := newBatesToolTestServer(t, daemon.URL, true)
	published := callToolResult(t, writes, "publish_bates_export", map[string]any{
		"allocation_id": testBatesAllocationID, "recipe": recipe,
	})
	assert.Equal(t, "verified", objectField(t, published, "structuredContent")["state"])
	assert.Equal(t, []string{
		"GET /api/v1/bates/exports?limit=25",
		"GET /api/v1/bates/exports/" + testBatesAllocationID,
		"POST /api/v1/bates/exports",
	}, calls)
}

func syntheticBatesRecipe() pdfstamp.Recipe {
	return pdfstamp.Recipe{
		Contract: pdfstamp.RecipeContractV1, NamespaceID: testBatesNamespaceID,
		Prefix: "OUR", Padding: 6, StartAt: 41, Position: "bottom-right", MarginPoints: 24,
		FontName: "Helvetica", FontSizePoints: 9, Color: "#000000", Opacity: 1,
		Units: "point", RotationPolicy: "follow_page", EngineIdentity: pdfstamp.EngineIdentity{
			Name: "pdfcpu", Version: "v0.15.0", API: "AddWatermarksMap",
			Options: []string{"onTop=true", "update=restamp"},
		},
	}
}

func syntheticBatesRecipeArguments(t *testing.T, recipe pdfstamp.Recipe) map[string]any {
	t.Helper()
	encoded, err := json.Marshal(recipe)
	assert.NoError(t, err)
	var result map[string]any
	assert.NoError(t, json.Unmarshal(encoded, &result))
	return result
}

func newBatesToolTestServer(t *testing.T, daemonURL string, allowWrites bool) *Server {
	t.Helper()
	lease := newDaemonLeaseWith(func(context.Context) (*daemonconn.Connection, error) {
		return daemonconn.New(daemonURL, "synthetic-key"), nil
	}, func(*daemonconn.Connection) error { return nil })
	return newServerWithOptionsAndDaemon(testImplementation(), ServerOptions{AllowPackageWrites: allowWrites}, lease)
}

func batesPreviewArguments() map[string]any {
	return map[string]any{"namespace_id": testBatesNamespaceID, "snapshot_id": testBatesSnapshotID, "start_at": 41}
}

func batesReserveArguments(t *testing.T) map[string]any {
	t.Helper()
	return map[string]any{"operation_id": testBatesOperationID, "snapshot_id": testBatesSnapshotID,
		"recipe": syntheticBatesRecipeArguments(t, syntheticBatesRecipe())}
}

func callToolResult(t *testing.T, server *Server, name string, arguments map[string]any) map[string]any {
	t.Helper()
	return decodeResult(t, exchangeRaw(t, server, requestFor("tools/call", map[string]any{
		"name": name, "arguments": arguments,
	})))
}

func decodeDaemonJSON(body io.Reader, target any) error {
	return json.UnmarshalRead(body, target)
}

func TestExportBatesFilePublishesVerifiedPDF(t *testing.T) {
	t.Setenv("DOCBANK_HOME", filepath.Join(t.TempDir(), "docbank-home"))
	pdf, receipt := syntheticStampedBatesExport(t)
	daemon := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/api/v1/bates/exports/" + testBatesAllocationID:
			writeDaemonJSON(t, response, receipt)
		case "/api/v1/bates/exports/" + testBatesAllocationID + "/content":
			response.Header().Set("Content-Type", "application/pdf")
			_, _ = response.Write(pdf)
		default:
			http.NotFound(response, request)
		}
	}))
	t.Cleanup(daemon.Close)
	server := newBatesToolTestServer(t, daemon.URL, true)
	destination := filepath.Join(t.TempDir(), "production.PDF")

	result := callToolResult(t, server, "export_bates_file", map[string]any{
		"allocation_id": testBatesAllocationID, "destination_path": destination, "overwrite": false,
	})

	assert.Equal(t, "published", objectField(t, result, "structuredContent")["state"])
	written, err := os.ReadFile(destination)
	require.NoError(t, err)
	assert.Equal(t, pdf, written)
	entries, err := os.ReadDir(filepath.Dir(destination))
	require.NoError(t, err)
	assert.Len(t, entries, 1, "staging directory must be removed after publication")
}

func TestExportBatesFileRejectsUnsafeDestinations(t *testing.T) {
	dataDir := filepath.Join(t.TempDir(), "docbank-home")
	require.NoError(t, os.MkdirAll(filepath.Join(dataDir, "exports"), 0o700))
	t.Setenv("DOCBANK_HOME", dataDir)
	outside := t.TempDir()
	existing := filepath.Join(outside, "existing.pdf")
	require.NoError(t, os.WriteFile(existing, []byte("keep me"), 0o600))
	server := newBatesToolTestServer(t, "http://127.0.0.1:1", true)
	type destinationCase struct {
		path      string
		overwrite bool
		reason    string
	}
	cases := map[string]destinationCase{
		"existing without overwrite": {existing, false, "destination exists"},
		"non-PDF extension":          {filepath.Join(outside, "authorized_keys"), true, "must end in .pdf"},
		"data directory":             {filepath.Join(dataDir, "docbank.pdf"), true, "outside the Docbank data directory"},
		"data subdirectory":          {filepath.Join(dataDir, "exports", "out.pdf"), true, "outside the Docbank data directory"},
		"relative destination":       {"relative.pdf", true, "absolute"},
	}
	linkToData := filepath.Join(outside, "into-data")
	symlinkDestination := filepath.Join(outside, "linked.pdf")
	if err := errors.Join(os.Symlink(dataDir, linkToData), os.Symlink(existing, symlinkDestination)); err != nil {
		t.Logf("symlink cases skipped: %v", err)
	} else {
		cases["symlinked parent in data dir"] = destinationCase{filepath.Join(linkToData, "out.pdf"), true, "outside the Docbank data directory"}
		cases["symlink destination"] = destinationCase{symlinkDestination, true, "regular file"}
	}
	for name, test := range cases {
		t.Run(name, func(t *testing.T) {
			response := exchangeRaw(t, server, requestFor("tools/call", map[string]any{
				"name": "export_bates_file", "arguments": map[string]any{
					"allocation_id": testBatesAllocationID, "destination_path": test.path, "overwrite": test.overwrite,
				},
			}))
			wireErr := decodeWireError(t, response)
			assert.EqualValues(t, jsonrpc.CodeInvalidParams, wireErr.Code)
			assert.Contains(t, wireErr.Message, test.reason)
		})
	}
	kept, err := os.ReadFile(existing)
	require.NoError(t, err)
	assert.Equal(t, "keep me", string(kept))
	entries, err := os.ReadDir(filepath.Join(dataDir, "exports"))
	require.NoError(t, err)
	assert.Empty(t, entries)
}

func syntheticStampedBatesExport(t *testing.T) ([]byte, api.BatesExport) {
	t.Helper()
	source := fpdf.NewCustom(&fpdf.InitType{UnitStr: "pt", Size: fpdf.SizeType{Wd: 612, Ht: 792}})
	source.SetFont("Helvetica", "", 16)
	source.AddPage()
	source.Cell(250, 25, "SYNTHETIC PAGE")
	var unstamped, stamped bytes.Buffer
	require.NoError(t, source.Output(&unstamped))
	labels := []pdfstamp.PageLabel{{SourcePage: 1, Label: "OUR000041"}}
	result, err := pdfstamp.Stamp(t.Context(), bytes.NewReader(unstamped.Bytes()), labels, syntheticBatesRecipe(), &stamped)
	require.NoError(t, err)
	return stamped.Bytes(), api.BatesExport{
		ArtifactID: testBatesAllocationID, AllocationID: testBatesAllocationID,
		BlobSHA256: result.SHA256, Size: result.Size, MediaType: "application/pdf", PageCount: 1,
		RecipeSHA256: testProfileID, ManifestSHA256: testProfileID, State: "verified",
		CreatedAt: "2026-09-21T12:00:00Z", Pages: []api.BatesArtifactPage{{
			Ordinal: 1, OccurrenceID: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			SourceBlobSHA256: testProfileID, SourcePage: 1, OutputPage: 1, Label: "OUR000041",
		}},
	}
}
