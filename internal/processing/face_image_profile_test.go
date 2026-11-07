package processing

import (
	"bytes"
	"image/color"
	"testing"
	"time"
	"uuid"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/media/mediatest"
	"go.kenn.io/docbank/internal/config"
	"go.kenn.io/docbank/internal/store"
)

func TestMixedFaceImageProfileKeepsBindingEligibility(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name              string
		source            string
		images, faceCalls int
	}{
		{"mixed faceless", "faceless", 1, 0},
		{"mixed Hidden", "hidden", 0, 1},
		{"mixed paired RAW", "raw", 1, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			faces := newFaceEmbeddingFixture(t, false)
			f := faces.f
			if tc.source == "faceless" {
				f = newPhotoDescriptionFixture(t)
			}
			images := newImageEmbeddingService(t, f)
			portable := images.portable
			providers := map[string]document.EmbeddingProvider{images.binding.Name: images.provider}
			descriptors := []string{images.descriptor.Fingerprint}
			faceProfile := faces.service.profiles[config.LocalFacesProfile]
			portable.Embeddings = append(portable.Embeddings, faceProfile.portable.Embeddings[0])
			providers[config.LocalFacesProfile] = faceProfile.embedders[config.LocalFacesProfile]
			descriptors = append(descriptors, providers[config.LocalFacesProfile].Descriptor().Fingerprint)
			service, err := NewService(ctx, ServiceConfig{Catalog: f.catalog, Blobs: f.blobs, Gate: newTestOperationGate(), SpoolDirectory: t.TempDir(), Lifecycle: ctx, Principal: "synthetic-operator", Scope: "document-processing", Profiles: map[string]ProfileConfig{"mixed": {Profile: portable, EmbeddingProviders: providers}}})
			require.NoError(t, err)
			t.Cleanup(service.Stop)
			f.grant(t, service, "mixed")
			if tc.source == "hidden" {
				require.NoError(t, f.catalog.SetupPhotoHidden(ctx, "synthetic-passcode"))
				_, err = f.catalog.SetPhotoAssetHidden(ctx, f.asset.ID, f.asset.Revision, true)
				require.NoError(t, err)
			}
			rawVersion := ""
			if tc.source == "raw" {
				rawBytes := mediatest.TIFFWithEmbeddedJPEG(1, mediatest.TIFFJPEGPreview{Data: mediatest.JPEG(1024, 768, color.White)})
				written, err := f.blobs.WriteDetailedContext(ctx, bytes.NewReader(rawBytes))
				require.NoError(t, err)
				run, err := f.catalog.BeginIngest(ctx, "cli", "Synthetic paired source")
				require.NoError(t, err)
				raw, err := f.catalog.IngestFileExact(ctx, run, f.catalog.RootID(), "paired.nef", written.Hash, written.Size, "image/x-nikon-nef", "paired.nef", "", processingBlobPhysical(t, written))
				require.NoError(t, err)
				rawVersion = raw.CurrentVersionID
				asset, err := f.catalog.PhotoAssetForNode(ctx, raw.ID)
				require.NoError(t, err)
				_, err = f.catalog.DetachPhotoFile(ctx, asset.ID, asset.Revision, asset.Files[0].ID, store.PhotoDetachOptions{})
				require.NoError(t, err)
				asset, err = f.catalog.AttachPhotoFile(ctx, f.asset.ID, f.asset.Revision, raw.ID, store.PhotoRoleRAW, nil)
				require.NoError(t, err)
				for _, file := range asset.Files {
					if file.NodeID == f.node.ID {
						asset, err = f.catalog.SetPhotoDisplay(ctx, asset.ID, asset.Revision, &file.ID)
						require.NoError(t, err)
					}
				}
				_, _, err = f.catalog.CreatePhotoFace(ctx, raw.CurrentVersionID, uuid.New().String(), document.FaceBox{Width: .2, Height: .2}, "")
				require.NoError(t, err)
				_, err = EnsureVisualPreview(ctx, f.catalog, f.blobs, raw.CurrentVersionID, f.recipe)
				require.NoError(t, err)
			}
			worker, err := NewEmbeddingWorker(EmbeddingWorkerConfig{Catalog: f.catalog, Authority: f.catalog, Blobs: f.blobs, GenerationBlobs: f.blobs, Runtime: service.EmbeddingRuntimes(), Gate: newTestOperationGate(), Owner: "mixed-test", RetryLimit: 1, RetryBaseDelay: time.Millisecond, MaxRetryDelay: time.Second, AttemptLifetime: time.Minute, MaxRows: 100, MaxDimensions: 512, MaxVectorBlobBytes: 1 << 20, LeaseDuration: time.Minute, IdleDelay: time.Millisecond, DescriptorFingerprints: descriptors})
			require.NoError(t, err)
			for range 4 {
				worker.reconcileAt = time.Time{}
				_, err = worker.ScanOnce(ctx)
				require.NoError(t, err)
			}
			require.Equal(t, tc.images, images.fake.runtime.callCount(images.binding.Name), "image eligibility must stay independent of face bindings")
			require.Equal(t, tc.faceCalls, faces.calls, "face eligibility includes Hidden and paired RAW members")
			if rawVersion != "" {
				sets, err := f.catalog.FaceMatchingInputs(ctx)
				require.NoError(t, err)
				var published []string
				for _, set := range sets.Sets {
					published = append(published, set.Set.InputGeneration.SourceVersionID)
				}
				require.Contains(t, published, rawVersion, "paired RAW must publish face vectors while JPEG is display")
			}
			coverage, err := service.Coverage(ctx, "mixed", SourceFence{VaultUID: f.catalog.VaultID(), ContentVersionIDs: []string{f.node.CurrentVersionID}})
			require.NoError(t, err)
			for _, binding := range coverage.Embeddings {
				if binding.Name == images.binding.Name {
					require.Equal(t, tc.images, binding.Complete, "eligible image work must publish a vector head")
				}
			}
		})
	}
}
