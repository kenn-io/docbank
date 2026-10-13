package document

import (
	"math"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestFaceBoxRotations(t *testing.T) {
	t.Parallel()
	box := FaceBox{X: 0.1, Y: 0.2, Width: 0.3, Height: 0.4}
	for _, tc := range []struct {
		rotation int
		want     FaceBox
	}{{0, box}, {90, FaceBox{0.4, 0.1, 0.4, 0.3}}, {180, FaceBox{0.6, 0.4, 0.3, 0.4}}, {270, FaceBox{0.2, 0.6, 0.4, 0.3}}} {
		got, err := box.Rotate(tc.rotation)
		require.NoError(t, err)
		require.InDelta(t, tc.want.X, got.X, 1e-12)
		require.InDelta(t, tc.want.Y, got.Y, 1e-12)
		require.InDelta(t, tc.want.Width, got.Width, 1e-12)
		require.InDelta(t, tc.want.Height, got.Height, 1e-12)
	}
	edge := FaceBox{X: 0.8, Y: 0.8, Width: 0.2, Height: 0.2}
	for _, rotation := range []int{0, 90, 180, 270} {
		rotated, err := edge.Rotate(rotation)
		require.NoError(t, err)
		require.NoError(t, rotated.Validate())
	}
	_, err := box.Rotate(45)
	require.Error(t, err)
}

func TestFaceBoxValidationAndOverlap(t *testing.T) {
	t.Parallel()
	for _, box := range []FaceBox{{Width: 0.2, Height: 0.2}, {X: 0.8, Y: 0.8, Width: 0.2, Height: 0.2}} {
		require.NoError(t, box.Validate())
		require.InDelta(t, 1.0, box.Overlap(box), 1e-12)
	}
	for _, box := range []FaceBox{{}, {X: -0.1, Width: 0.2, Height: 0.2}, {Width: 1.1, Height: 0.2}, {Width: math.NaN(), Height: 0.2}, {Width: 0.2, Height: math.Inf(1)}} {
		require.Error(t, box.Validate())
	}
	require.Zero(t, (FaceBox{Width: 0.1, Height: 0.1}).Overlap(FaceBox{X: 0.5, Width: 0.1, Height: 0.1}))
}
