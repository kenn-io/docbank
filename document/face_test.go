package document

import (
	"math"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestFaceBoxValidation(t *testing.T) {
	t.Parallel()
	for _, box := range []FaceBox{{Width: 0.2, Height: 0.2}, {X: 0.8, Y: 0.8, Width: 0.2, Height: 0.2}} {
		require.NoError(t, box.Validate())
	}
	for _, box := range []FaceBox{{}, {X: -0.1, Width: 0.2, Height: 0.2}, {Width: 1.1, Height: 0.2}, {Width: math.NaN(), Height: 0.2}, {Width: 0.2, Height: math.Inf(1)}} {
		require.Error(t, box.Validate())
	}
}
