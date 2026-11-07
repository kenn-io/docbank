package processing

import (
	"testing"
	"time"
	"uuid"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/config"
)

func TestFaceEmbeddingCrowdFitsDescriptorBudget(t *testing.T) {
	t.Parallel()
	// The Linux descriptor limit checks worker preparation; Kit owns request chunking.
	withFaceDescriptorBudget(t, func() {
		x := newFaceEmbeddingFixture(t, false)
		x.matchAll = true
		x.worker.maxRows = 128
		for i := len(x.inputs); i < 128; i++ {
			_, _, err := x.f.catalog.CreatePhotoFace(t.Context(), x.f.node.CurrentVersionID, uuid.New().String(), document.FaceBox{X: .2, Y: .2, Width: .2, Height: .2}, "")
			require.NoError(t, err)
		}
		x.f.grant(t, x.service, config.LocalFacesProfile)
		x.worker.reconcileAt = time.Time{}
		n, err := x.worker.ScanOnce(t.Context())
		require.NoError(t, err)
		require.Equal(t, 1, n)
		sets, err := x.f.catalog.FaceMatchingInputs(t.Context())
		require.NoError(t, err)
		require.Len(t, sets.Sets, 1)
		require.Equal(t, 128, sets.Sets[0].Set.VectorSet.RowCount)
		require.Len(t, x.boxes, 128)
		require.Equal(t, 16, x.calls)
	})
}
