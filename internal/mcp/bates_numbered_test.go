package mcp

import (
	"crypto/sha256"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"uuid"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/redaction"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/canonical"
	"go.kenn.io/docbank/internal/pdfproduction"
	"go.kenn.io/docbank/internal/processing"
	"go.kenn.io/docbank/internal/store"
)

func TestExportBatesFileReportsNumberedRecoveryScope(t *testing.T) {
	t.Setenv("DOCBANK_HOME", filepath.Join(t.TempDir(), "home"))
	data := []byte("recorded bytes; recovery does not claim PDF parsing")
	recipe := processing.NumberedRecipe{
		Contract: processing.NumberedPDFContract, VaultUID: uuid.New(),
		Selection: processing.NativePDFSelection{NodeID: 1, Source: document.PageSource{
			VersionID: uuid.New().String(), SHA256: testProfileID, Size: 123}},
		PreparationSHA256: testProfileID, DecisionsSHA256: testProfileID,
		ReviewSHA256: testProfileID, ResolvedSHA256: testProfileID,
		Renderer: pdfproduction.QualifiedRecipe(), PageCount: 1,
		Namespace:         processing.NumberedNamespace{ID: uuid.MustParse(testBatesNamespaceID), Prefix: "OUR", Padding: 6, Start: 41},
		Pages:             []redaction.Page{{Number: 1, Width: 85000, Height: 110000, FrameSHA256: testProfileID, Span: redaction.Span{End: 1}}},
		OriginalRetention: "indefinite",
	}
	recipe.Footer = pdfproduction.NumberedFooter{StripHeight: 5000, InsetX: 2000, InsetTop: 1000, InsetBottom: 1000,
		FontSHA256: recipe.Renderer.FontSHA256, FontSizeMilliPoints: 10000}
	require.NoError(t, processing.ValidateNumberedRecipe(recipe))
	encoded, err := canonical.Marshal(recipe)
	require.NoError(t, err)
	request, err := recipe.PlanRequest(uuid.MustParse(testBatesSnapshotID))
	require.NoError(t, err)
	requestSHA, err := store.BatesRequestDigest(request)
	require.NoError(t, err)
	page := store.BatesArtifactPage{Ordinal: 1, OccurrenceID: request.Pages[0].OccurrenceID,
		SourceBlobSHA256: testProfileID, SourcePage: 1, OutputPage: 1, Label: "OUR000041"}
	artifact := store.BatesArtifact{ArtifactID: testBatesAllocationID, AllocationID: testBatesAllocationID,
		BlobSHA256: fmt.Sprintf("%x", sha256.Sum256(data)), Size: int64(len(data)), MediaType: "application/pdf",
		PageCount: 1, RecipeSHA256: request.RecipeSHA256, Pages: []store.BatesArtifactPage{page}}
	manifest, err := store.BatesArtifactManifestSHA(artifact)
	require.NoError(t, err)
	receipt := api.BatesExport{ArtifactID: artifact.ArtifactID, AllocationID: artifact.AllocationID,
		BlobSHA256: artifact.BlobSHA256, Size: artifact.Size, MediaType: artifact.MediaType, PageCount: 1,
		RecipeSHA256: request.RecipeSHA256, ManifestSHA256: manifest, State: "verified", CreatedAt: "2026-10-01T00:00:00Z",
		Recipe: encoded, Pages: artifact.Pages,
		Namespace: &api.BatesNamespace{NamespaceID: testBatesNamespaceID, Prefix: "OUR", Padding: 6, CreatedAt: "2026-10-01T00:00:00Z"},
		Allocation: &api.BatesAllocation{AllocationID: testBatesAllocationID, NamespaceID: testBatesNamespaceID,
			SnapshotID: testBatesSnapshotID, RequestSHA256: requestSHA, RecipeSHA256: request.RecipeSHA256,
			State: "committed", StartSequence: 41, EndSequence: 41, CreatedAt: "2026-10-01T00:00:00Z",
			Labels: []api.BatesPageLabel{{Ordinal: 1, OccurrenceID: page.OccurrenceID, SourcePage: 1, OutputPage: 1, Label: page.Label}}},
	}
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/bates/exports/" + testBatesAllocationID:
			writeDaemonJSON(t, w, receipt)
		case "/api/v1/bates/exports/" + testBatesAllocationID + "/content":
			_, _ = w.Write(data)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(daemon.Close)
	server := newBatesToolTestServer(t, daemon.URL, true)
	inspected := callToolResult(t, server, "get_bates_export", map[string]any{"allocation_id": testBatesAllocationID})
	require.NotEqual(t, true, inspected["isError"])
	destination := filepath.Join(t.TempDir(), "delivery.pdf")
	result := callToolResult(t, server, "export_bates_file", map[string]any{
		"allocation_id": testBatesAllocationID, "destination_path": destination, "overwrite": false,
	})
	content := objectField(t, result, "structuredContent")
	require.Equal(t, "recorded_bytes_and_numbers", content["verification"])
	require.Equal(t, "published", content["state"])
	saved, err := os.ReadFile(destination)
	require.NoError(t, err)
	require.Equal(t, data, saved)
}
