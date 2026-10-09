package document

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestPhotoQualityFingerprintCompatibility(t *testing.T) {
	t.Parallel()
	fingerprints, err := CurrentPhotoQualityFingerprints()
	require.NoError(t, err)
	require.Equal(t, sha256Hex([]byte("photo-quality:v2:16x16:catmull-rom:x-image-draw-v0.44.0:focus-ceiling=0.15:"+fingerprints.GridRecipe)), fingerprints.Evaluator)
}
