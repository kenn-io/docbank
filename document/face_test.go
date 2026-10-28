package document

import (
	"encoding/json/v2"
	"math"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestFaceBoxJSONCoordinates(t *testing.T) {
	t.Parallel()
	var box FaceBox
	require.NoError(t, json.Unmarshal([]byte(`{"x":0,"y":0,"width":0.2,"height":0.3}`), &box))
	require.Equal(t, FaceBox{Width: .2, Height: .3}, box)
	for _, raw := range []string{
		`{"y":0,"width":0.2,"height":0.3}`,
		`{"x":0,"width":0.2,"height":0.3}`,
		`{"x":0,"y":0,"height":0.3}`,
		`{"x":0,"y":0,"width":0.2}`,
	} {
		require.Error(t, json.Unmarshal([]byte(raw), &box), raw)
	}
}

func TestFaceBoxValidation(t *testing.T) {
	t.Parallel()
	for _, box := range []FaceBox{{Width: 0.2, Height: 0.2}, {X: 0.8, Y: 0.8, Width: 0.2, Height: 0.2}} {
		require.NoError(t, box.Validate())
	}
	for _, box := range []FaceBox{{}, {X: -0.1, Width: 0.2, Height: 0.2}, {Width: 1.1, Height: 0.2}, {Width: math.NaN(), Height: 0.2}, {Width: 0.2, Height: math.Inf(1)}} {
		require.Error(t, box.Validate())
	}
}
