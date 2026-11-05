package processing

import (
	"bytes"
	"context"
	"errors"
	"image/color"
	"testing"
	"testing/synctest"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/media/mediatest"
	"go.kenn.io/docbank/internal/query"
	"go.kenn.io/docbank/internal/store"
)

func TestLocalTextMixedRecipeRetriesImageOutage(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		renderer := newWorkerProvider(t)
		renderer.descriptor.SupportedFormats = []document.RenditionFormatCapability{{MediaFamily: "image", MediaType: "image/jpeg", InputKind: document.RenditionInputOriginalFile}}
		renderer.descriptor.Fingerprint = ""
		var err error
		renderer.descriptor, err = document.NewRenditionDescriptor(renderer.descriptor)
		require.NoError(t, err)
		fixture, setup := newLocalTextMixedFixture(t, localTextImageRenderer{renderer})
		provider, ok := setup.Service.profiles["mixed"].embedders["photo"].(*localTextEmbeddingProvider)
		require.True(t, ok)
		provider.embed = func(context.Context) error { return errors.New("synthetic image provider outage") }
		written, err := fixture.blobs.WriteDetailedContext(t.Context(), bytes.NewReader(mediatest.JPEG(32, 24, color.White)))
		require.NoError(t, err)
		run, err := fixture.catalog.BeginIngest(t.Context(), "cli", "Synthetic recoverable image outage")
		require.NoError(t, err)
		node, err := fixture.catalog.IngestFileExact(t.Context(), run, fixture.catalog.RootID(), "outage.jpg", written.Hash, written.Size, "image/jpeg", "outage.jpg", "", processingBlobPhysical(t, written))
		require.NoError(t, err)
		recipe, _ := VisualPreviewRecipeForSize("grid")
		_, err = EnsureVisualPreview(t.Context(), fixture.catalog, fixture.blobs, node.CurrentVersionID, recipe)
		require.NoError(t, err)
		require.NoError(t, setup.Service.grantProfileConsent(t.Context(), setup.Service.profiles["mixed"], nil))
		item := store.DocumentSummary{NodeID: node.ID, ContentVersionID: node.CurrentVersionID, Name: node.Name}
		require.Error(t, setup.process(t.Context(), item))
		jobs, err := fixture.catalog.EmbeddingJobsForVersionProfile(t.Context(), node.CurrentVersionID, setup.Service.profiles["mixed"].record.Fingerprint)
		require.NoError(t, err)
		states := map[string]string{}
		for _, job := range jobs {
			states[job.BindingID] = job.State
			if job.BindingID == "photo" {
				require.Equal(t, store.EmbeddingFailureProviderUnavailable, job.FailureCode)
			}
		}
		require.Equal(t, map[string]string{"chunk": "completed", "photo": "failed"}, states)
		provider.embed = nil
		source := setup.Service.profiles["mixed"]
		setup.Service.Stop()
		restarted, err := NewService(t.Context(), ServiceConfig{Catalog: fixture.catalog, Blobs: fixture.blobs,
			Gate: newWorkerTestGate(), SpoolDirectory: t.TempDir(), Profiles: map[string]ProfileConfig{"mixed": {
				Profile: source.portable, RenditionProvider: source.provider, EmbeddingProviders: source.embedders,
				Tokenizers: source.tokenizers, EmbeddingSimilarityFloors: map[string]float64{"photo": 0.2},
			}}})
		require.NoError(t, err)
		t.Cleanup(restarted.Stop)
		setup = &LocalTextSetup{Service: restarted, Selected: "mixed", Save: func(string, string) error { return nil }}
		targets, err := setup.targets(t.Context(), "", 50)
		require.NoError(t, err)
		require.Contains(t, targets, item, "provider recovery must leave the unfinished image eligible for retry")
		require.NoError(t, setup.activate(t.Context(), restarted, LocalTextPlan{Profile: "mixed", Server: "local"}))
		jobs, err = fixture.catalog.EmbeddingJobsForVersionProfile(t.Context(), node.CurrentVersionID, source.record.Fingerprint)
		require.NoError(t, err)
		for _, job := range jobs {
			require.Equal(t, "completed", job.State, job.BindingID)
		}
		targets, err = setup.targets(t.Context(), "", 50)
		require.NoError(t, err)
		require.NotContains(t, targets, item)
		ranked, err := restarted.SearchPhotos(t.Context(), store.PhotoBrowseRequest{Query: query.Query{V: 1, Syntax: "simple", Mode: "semantic", Text: "white image", Sort: query.Sort{Field: "relevance", Direction: "desc"}}})
		require.NoError(t, err)
		require.Equal(t, "semantic", ranked.ActualMode)
		require.Len(t, ranked.Members, 1)
		require.Equal(t, node.CurrentVersionID, ranked.Members[0].ContentVersionID)
	})
}
