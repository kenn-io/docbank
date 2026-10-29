package document

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestPhotoQualityFingerprintCompatibility(t *testing.T) {
	t.Parallel()
	fingerprints, err := CurrentPhotoQualityFingerprints()
	require.NoError(t, err)
	require.Equal(t, sha256Hex([]byte("photo-quality:v3:16x16:catmull-rom:x-image-draw-v0.44.0:focus-ceiling=0.15:face-min-edge=8:face-light=balanced:prominence=box-area:face-inputs=json-array-v1:"+fingerprints.GridRecipe)), fingerprints.Evaluator)
}
