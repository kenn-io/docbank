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
	"go.kenn.io/docbank/internal/store"
)

func TestFaceProfileKeepsSourceEligibility(t *testing.T) {
	t.Parallel()
	for _, source := range []string{"hidden", "raw"} {
		t.Run(source, func(t *testing.T) {
			t.Parallel()
			ctx := t.Context()
			faces := newFaceEmbeddingFixture(t, false)
			f := faces.f
			faces.enable(t)
			if source == "hidden" {
				require.NoError(t, f.catalog.SetupPhotoHidden(ctx, "synthetic-passcode"))
				_, err := f.catalog.SetPhotoAssetHidden(ctx, f.asset.ID, f.asset.Revision, true)
				require.NoError(t, err)
			}
			rawVersion := ""
			if source == "raw" {
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
			worker := faces.worker
			for range 4 {
				worker.reconcileAt = time.Time{}
				_, err := worker.ScanOnce(ctx)
				require.NoError(t, err)
			}
			wantCalls := 1
			if source == "raw" {
				wantCalls = 2
			}
			require.Equal(t, wantCalls, faces.calls, "face eligibility includes Hidden and paired RAW members")
			if rawVersion != "" {
				sets, err := f.catalog.FaceMatchingInputs(ctx)
				require.NoError(t, err)
				var published []string
				for _, set := range sets.Sets {
					published = append(published, set.Set.InputGeneration.SourceVersionID)
				}
				require.Contains(t, published, rawVersion, "paired RAW must publish face vectors while JPEG is display")
			}
		})
	}
}
