package main

import (
	"archive/zip"
	"bytes"
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"uuid"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/canonical"
	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/docbank/internal/pdfproduction"
	"go.kenn.io/docbank/internal/processing"
	"go.kenn.io/docbank/internal/store"
)

func TestNumberedDownloadMaskVerification(t *testing.T) {
	f := newRedactionVerifyFixture(t)
	ns := store.BatesNamespace{NamespaceID: uuid.New().String(), Prefix: "Q", Padding: 6}
	recipe, err := processing.BuildNumberedRecipe(uuid.New(), ns, 1, f.review)
	require.NoError(t, err)
	presentation, err := recipe.Presentation()
	require.NoError(t, err)
	for _, scenario := range []string{"valid", "unmasked PDF"} {
		pdf := redactionVerifyPDF(t, f.review, presentation, scenario)
		receipt := numberedDownloadReceipt(t, recipe, pdf)
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/api/v1/bates/exports/"+receipt.AllocationID {
				_ = json.MarshalWrite(w, receipt)
			} else {
				_, _ = w.Write(pdf)
			}
		}))
		t.Cleanup(server.Close)
		client := daemonconn.New(server.URL, "synthetic-key")
		t.Cleanup(func() { _ = client.Close() })
		stage, err := os.Create(filepath.Join(t.TempDir(), "download.pdf"))
		require.NoError(t, err)
		t.Cleanup(func() { _ = stage.Close() })
		// Both scopes agree on the retained identity; only the full scope promises
		// to detect a changed mask when that identity itself has been substituted.
		reduced, err := client.DownloadBatesExportTo(t.Context(), receipt, stage, nil)
		require.NoError(t, err)
		require.Equal(t, "recorded_bytes_and_numbers", reduced.Verification)
		_, full, err := client.DownloadBatesExport(t.Context(), receipt, &f.review)
		if scenario == "unmasked PDF" {
			require.ErrorIs(t, err, pdfproduction.ErrPDFIntegrity)
			require.Empty(t, full.Verification)
		} else {
			require.NoError(t, err)
			require.Equal(t, "masks_text_and_numbers", full.Verification)
		}
		require.NoError(t, stage.Close())
		require.NoError(t, client.Close())
		server.Close()
	}
}

func TestNumberedArchiveVisibleNumberVerification(t *testing.T) {
	f := newRedactionVerifyFixture(t)
	ns := store.BatesNamespace{NamespaceID: uuid.New().String(), Prefix: "Q", Padding: 6}
	recipe, err := processing.BuildNumberedRecipe(uuid.New(), ns, 1, f.review)
	require.NoError(t, err)
	presentation, err := recipe.Presentation()
	require.NoError(t, err)
	for _, scenario := range []string{"valid", "wrong visible number"} {
		t.Run(scenario, func(t *testing.T) {
			// Only the raster changes to Q000002. The PDF text, review and
			// reservation still name Q000001; all unsigned hashes are recomputed.
			pdf := redactionVerifyPDF(t, f.review, presentation, scenario)
			export := numberedDownloadReceipt(t, recipe, pdf)
			r := processing.NumberedReservation{Contract: "native-pdf-numbered-reservation/v1",
				Recipe: recipe, RecipeSHA256: export.RecipeSHA256, AllocationID: uuid.MustParse(export.AllocationID),
				SnapshotID: uuid.MustParse(export.Allocation.SnapshotID), RequestSHA256: export.Allocation.RequestSHA256,
				State: "committed", Pages: export.Pages}
			delivery, err := processing.NumberedArtifactDelivery(r, store.BatesArtifact{
				ArtifactID: export.ArtifactID, AllocationID: export.AllocationID, BlobSHA256: export.BlobSHA256,
				Size: export.Size, PageCount: 1, MediaType: "application/pdf", RecipeJSON: export.Recipe,
				RecipeSHA256: export.RecipeSHA256, ManifestSHA256: export.ManifestSHA256, Pages: export.Pages})
			require.NoError(t, err)
			text := []byte("[REDACTED] KEEP SECRET\f")
			receipt := processing.RedactionReceipt{Contract: "native-pdf-numbered-result/v1",
				Selection: f.review.Selection, PreparationSHA256: f.review.PreparationSHA256,
				DecisionsSHA256: f.review.DecisionsSHA256, ReviewSHA256: f.review.SHA256,
				ResolvedSHA256: f.review.Resolved.SHA256, Recipe: f.review.Recipe, PageCount: 1,
				PDF: redactionFixtureDigest(pdf), Text: redactionFixtureDigest(text), Numbered: delivery}
			encoded, err := json.Marshal(receipt)
			require.NoError(t, err)
			var archive bytes.Buffer
			writer := zip.NewWriter(&archive)
			for i, name := range []string{"redacted.pdf", "redacted.txt", "receipt.json"} {
				entry, err := writer.CreateHeader(&zip.FileHeader{Name: name, Method: zip.Store})
				require.NoError(t, err)
				_, err = entry.Write([][]byte{pdf, text, encoded}[i])
				require.NoError(t, err)
			}
			require.NoError(t, writer.Close())
			_, err = daemonconn.VerifyRedactionArchive(t.Context(), bytes.NewReader(archive.Bytes()), int64(archive.Len()), f.review)
			require.NoError(t, err, "receipt bindings, archive hashes and CRCs agree")
			require.NoError(t, os.WriteFile(f.args[5], archive.Bytes(), 0600))
			output, err := runCLI(t, append(f.args, "--json")...)
			if scenario == "valid" {
				require.NoError(t, err)
			} else {
				require.Equal(t, 6, commandExitCode(err, true))
				require.ErrorIs(t, err, pdfproduction.ErrPDFIntegrity)
				require.Empty(t, output)
			}
		})
	}
}

func numberedDownloadReceipt(t *testing.T, recipe processing.NumberedRecipe, data []byte) api.BatesExport {
	t.Helper()
	encoded, err := canonical.Marshal(recipe)
	require.NoError(t, err)
	request, err := recipe.PlanRequest(uuid.New())
	require.NoError(t, err)
	requestSHA, err := store.BatesRequestDigest(request)
	require.NoError(t, err)
	id := uuid.New().String()
	digest := redactionFixtureDigest(data)
	artifact := store.BatesArtifact{ArtifactID: id, AllocationID: id, BlobSHA256: digest.SHA256,
		Size: digest.Size, MediaType: "application/pdf", PageCount: 1, RecipeSHA256: request.RecipeSHA256,
		Pages: []store.BatesArtifactPage{{Ordinal: 1, OccurrenceID: request.Pages[0].OccurrenceID,
			SourceBlobSHA256: recipe.Selection.Source.SHA256, SourcePage: 1, OutputPage: 1, Label: "Q000001"}}}
	manifest, err := store.BatesArtifactManifestSHA(artifact)
	require.NoError(t, err)
	return api.BatesExport{ArtifactID: id, AllocationID: id, BlobSHA256: digest.SHA256,
		Size: digest.Size, MediaType: artifact.MediaType, PageCount: 1, RecipeSHA256: request.RecipeSHA256,
		ManifestSHA256: manifest, State: "verified", Recipe: encoded, Pages: artifact.Pages,
		Namespace: &api.BatesNamespace{NamespaceID: recipe.Namespace.ID.String(), Prefix: "Q", Padding: 6},
		Allocation: &api.BatesAllocation{AllocationID: id, NamespaceID: recipe.Namespace.ID.String(),
			SnapshotID: request.SnapshotID, RecipeSHA256: request.RecipeSHA256, RequestSHA256: requestSHA,
			State: "committed", StartSequence: 1, EndSequence: 1, Labels: []api.BatesPageLabel{{
				Ordinal: 1, OccurrenceID: request.Pages[0].OccurrenceID, SourcePage: 1, OutputPage: 1, Label: "Q000001"}}},
	}
}
