package daemonconn

import (
	"bytes"
	"crypto/sha256"
	"encoding/json/v2"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"uuid"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/canonical"
	"go.kenn.io/docbank/internal/pdfstamp"
	"go.kenn.io/docbank/internal/processing"
	"go.kenn.io/docbank/internal/store"
)

func TestNumberedDownloadVerificationScope(t *testing.T) {
	prep := redactionClientPreparation(t)
	review, err := processing.BuildRedactionReview(prep, redactionClientMasks(prep), 1)
	require.NoError(t, err)
	ns := store.BatesNamespace{NamespaceID: uuid.New().String(), Prefix: "Q", Padding: 6}
	recipe, err := processing.BuildNumberedRecipe(uuid.New(), ns, 1, review)
	require.NoError(t, err)
	encoded, err := canonical.Marshal(recipe)
	require.NoError(t, err)
	request, err := recipe.PlanRequest(uuid.New())
	require.NoError(t, err)
	requestSHA, err := store.BatesRequestDigest(request)
	require.NoError(t, err)
	id := uuid.New().String()
	// Reduced recovery trusts recorded bytes; it deliberately does not claim to
	// parse them. A full review must never silently select that narrower scope.
	data := []byte("synthetic recorded bytes without PDF structure")
	page := store.BatesArtifactPage{Ordinal: 1, OccurrenceID: request.Pages[0].OccurrenceID,
		SourceBlobSHA256: prep.Selection.Source.SHA256, SourcePage: 1, OutputPage: 1, Label: "Q000001"}
	artifact := store.BatesArtifact{ArtifactID: id, AllocationID: id, BlobSHA256: fmt.Sprintf("%x", sha256.Sum256(data)),
		Size: int64(len(data)), MediaType: "application/pdf", RecipeSHA256: request.RecipeSHA256, PageCount: 1, Pages: []store.BatesArtifactPage{page}}
	manifest, err := store.BatesArtifactManifestSHA(artifact)
	require.NoError(t, err)
	receipt := api.BatesExport{ArtifactID: id, AllocationID: id, BlobSHA256: artifact.BlobSHA256,
		Size: artifact.Size, MediaType: artifact.MediaType, RecipeSHA256: artifact.RecipeSHA256,
		ManifestSHA256: manifest, State: "verified", PageCount: 1, Pages: artifact.Pages, Recipe: encoded,
		Namespace: &api.BatesNamespace{NamespaceID: ns.NamespaceID, Prefix: ns.Prefix, Padding: ns.Padding},
		Allocation: &api.BatesAllocation{AllocationID: id, NamespaceID: ns.NamespaceID, SnapshotID: request.SnapshotID,
			RequestSHA256: requestSHA, RecipeSHA256: request.RecipeSHA256, State: "committed", StartSequence: 1, EndSequence: 1,
			Labels: []api.BatesPageLabel{{Ordinal: 1, OccurrenceID: page.OccurrenceID, SourcePage: 1, OutputPage: 1, Label: page.Label}}}}
	wire := bytes.Clone(data)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/bates/exports/"+id {
			_ = json.MarshalWrite(w, receipt)
			return
		}
		_, _ = w.Write(wire)
	}))
	defer server.Close()
	client := New(server.URL, "synthetic-key")
	defer func() { require.NoError(t, client.Close()) }()
	got, verified, err := client.DownloadBatesExport(t.Context(), receipt, nil)
	require.NoError(t, err)
	require.Equal(t, data, got)
	require.Equal(t, "recorded_bytes_and_numbers", verified.Verification)
	file, err := os.Create(filepath.Join(t.TempDir(), "stage.pdf"))
	require.NoError(t, err)
	defer func() { require.NoError(t, file.Close()) }()
	_, err = file.Write(bytes.Repeat([]byte("old"), len(data)))
	require.NoError(t, err)
	verified, err = client.DownloadBatesExportTo(t.Context(), receipt, file, nil)
	require.NoError(t, err)
	require.Equal(t, "recorded_bytes_and_numbers", verified.Verification)
	saved, err := io.ReadAll(file)
	require.NoError(t, err)
	require.Equal(t, data, saved)
	_, _, err = client.DownloadBatesExport(t.Context(), receipt, &review)
	require.Error(t, err)
	wire[0] ^= 1
	_, _, err = client.DownloadBatesExport(t.Context(), receipt, nil)
	require.ErrorIs(t, err, ErrIntegrity)
}

// A retained record remains readable when its qualified engine is no longer
// shipped. The record is self-consistent; only the local engine differs.
func TestBatesRecordedRecipeAfterUpgrade(t *testing.T) {
	prep := redactionClientPreparation(t)
	review, err := processing.BuildRedactionReview(prep, redactionClientMasks(prep), 1)
	require.NoError(t, err)
	ns := store.BatesNamespace{NamespaceID: uuid.New().String(), Prefix: "Q", Padding: 6}
	numbered, err := processing.BuildNumberedRecipe(uuid.New(), ns, 1, review)
	require.NoError(t, err)
	numbered.Renderer.RendererSHA256 = strings.Repeat("a", 64)
	numbered.Renderer.FontSHA256 = strings.Repeat("b", 64)
	numbered.Renderer.WriterVersion = "saved-writer"
	numbered.Footer.FontSHA256 = numbered.Renderer.FontSHA256
	numbered.Footer.InsetTop += 100
	stamp := pdfstamp.Recipe{Contract: pdfstamp.RecipeContractV1, NamespaceID: ns.NamespaceID,
		Prefix: "Q", Padding: 6, StartAt: 1, Position: "bottom-right", MarginPoints: 24,
		FontName: "Helvetica", FontSizePoints: 9, Color: "#000000", Opacity: 1,
		Units: "point", RotationPolicy: "follow_page", EngineIdentity: pdfstamp.EngineIdentity{
			Name: "pdfcpu", Version: "v0.14.0", API: "AddWatermarksMap", Options: []string{"onTop=true", "update=restamp"}}}
	for _, test := range []struct {
		name   string
		recipe any
	}{{"numbered", numbered}, {"stamped", stamp}} {
		t.Run(test.name, func(t *testing.T) {
			encoded, err := canonical.Marshal(test.recipe)
			require.NoError(t, err)
			request, err := numbered.PlanRequest(uuid.New())
			require.NoError(t, err)
			request.RecipeSHA256 = fmt.Sprintf("%x", sha256.Sum256(encoded))
			requestSHA, err := store.BatesRequestDigest(request)
			require.NoError(t, err)
			id := uuid.New().String()
			data := []byte("recorded bytes from another qualified engine")
			page := store.BatesArtifactPage{Ordinal: 1, OccurrenceID: request.Pages[0].OccurrenceID,
				SourceBlobSHA256: prep.Selection.Source.SHA256, SourcePage: 1, OutputPage: 1, Label: "Q000001"}
			artifact := store.BatesArtifact{ArtifactID: id, AllocationID: id, BlobSHA256: fmt.Sprintf("%x", sha256.Sum256(data)),
				Size: int64(len(data)), MediaType: "application/pdf", RecipeSHA256: request.RecipeSHA256, PageCount: 1, Pages: []store.BatesArtifactPage{page}}
			manifest, err := store.BatesArtifactManifestSHA(artifact)
			require.NoError(t, err)
			receipt := api.BatesExport{ArtifactID: id, AllocationID: id, BlobSHA256: artifact.BlobSHA256,
				Size: artifact.Size, MediaType: artifact.MediaType, RecipeSHA256: artifact.RecipeSHA256,
				ManifestSHA256: manifest, State: "verified", PageCount: 1, Pages: artifact.Pages, Recipe: encoded,
				Namespace: &api.BatesNamespace{NamespaceID: ns.NamespaceID, Prefix: ns.Prefix, Padding: ns.Padding},
				Allocation: &api.BatesAllocation{AllocationID: id, NamespaceID: ns.NamespaceID, SnapshotID: request.SnapshotID,
					RequestSHA256: requestSHA, RecipeSHA256: request.RecipeSHA256, State: "committed", StartSequence: 1, EndSequence: 1,
					Labels: []api.BatesPageLabel{{Ordinal: 1, OccurrenceID: page.OccurrenceID, SourcePage: 1, OutputPage: 1, Label: page.Label}}}}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/v1/bates/exports/"+id {
					_ = json.MarshalWrite(w, receipt)
					return
				}
				_, _ = w.Write(data)
			}))
			defer server.Close()
			client := New(server.URL, "synthetic-key")
			defer func() { require.NoError(t, client.Close()) }()
			_, err = client.BatesExport(t.Context(), id)
			require.NoError(t, err)
			got, verified, err := client.DownloadBatesExport(t.Context(), receipt, nil)
			require.NoError(t, err)
			require.Equal(t, data, got)
			require.Equal(t, "recorded_bytes_and_numbers", verified.Verification)
			if test.name == "numbered" {
				_, _, err = client.DownloadBatesExport(t.Context(), receipt, &review)
				require.ErrorContains(t, err, "verifier version differs")
				require.NotErrorIs(t, err, ErrIntegrity)
			}
		})
	}
}
