package processing

import (
	"bytes"
	"image/color"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document/media/mediatest"
	"go.kenn.io/docbank/internal/store"
)

func TestFaceWorkerAbandonsDisappearedSource(t *testing.T) {
	t.Parallel()
	for _, scenario := range []struct {
		change      string
		preparation bool
	}{{"excluded", false}, {"trashed", false}, {"replaced", false}, {"excluded", true}, {"trashed", true}} {
		name := scenario.change
		if scenario.preparation {
			name += " before preparation"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ctx := t.Context()
			x := newFaceEmbeddingFixture(t, false)
			x.enable(t)
			x.worker.catalog = &countedReconcileCatalog{Store: x.f.catalog, pauseClaims: true}
			_, err := x.worker.ScanOnce(ctx)
			require.NoError(t, err)
			at := time.Now()
			claim, _, found, err := x.f.catalog.ClaimNextEmbeddingWork(ctx, "synthetic-observer", at, time.Minute, x.worker.descriptorFingerprints)
			require.NoError(t, err)
			require.True(t, found)
			jobID := claim.AttemptID
			require.NoError(t, x.f.catalog.ReleaseEmbeddingWork(ctx, claim, at))
			x.worker.catalog = x.f.catalog
			change := func() {
				x.onRequest = nil
				switch scenario.change {
				case "excluded":
					asset, err := x.f.catalog.PhotoAssetForNode(ctx, x.f.node.ID)
					require.NoError(t, err)
					_, err = x.f.catalog.SetPhotoAssetExcluded(ctx, asset.ID, asset.Revision, true)
					require.NoError(t, err)
				case "trashed":
					_, _, err := x.f.catalog.Trash(ctx, x.f.node.ID, store.UnconditionalRev)
					require.NoError(t, err)
				case "replaced":
					written, err := x.f.blobs.WriteDetailedContext(ctx, bytes.NewReader(mediatest.JPEG(800, 600, color.White)))
					require.NoError(t, err)
					_, _, err = x.f.catalog.ReplaceContent(ctx, x.f.node.ID, store.UnconditionalRev, written.Hash, written.Size, "image/jpeg", processingBlobPhysical(t, written))
					require.NoError(t, err)
				}
			}
			expectedCalls := 1
			if scenario.preparation {
				x.worker.catalog = &countedReconcileCatalog{Store: x.f.catalog, afterValidation: func(EmbeddingWorkClaim) { change() }}
				expectedCalls = 0
			} else {
				x.onRequest = change
			}
			_, err = x.worker.ScanOnce(ctx)
			require.NoError(t, err, "source changes during inference must fence and retire disposable work")
			require.Equal(t, expectedCalls, x.calls)
			job, err := x.f.catalog.EmbeddingJobByID(ctx, jobID)
			require.NoError(t, err)
			assert.Equal(t, "abandoned", job.State, "obsolete work must be retired before lease expiry")
			at = time.Now().Add(2 * time.Minute)
			_, _, found, err = x.f.catalog.ClaimEmbeddingWork(ctx, jobID, "synthetic-successor", at, time.Minute, x.worker.descriptorFingerprints)
			require.NoError(t, err)
			assert.False(t, found, "obsolete work must not return after lease expiry")
		})
	}
}
