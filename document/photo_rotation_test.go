package document

import (
	"math"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestPhotoRotationFromRolls(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		degrees []float64
		want    *int
	}{
		{"missing", nil, nil}, {"upright", []float64{0}, nil},
		{"clockwise", []float64{-90}, new(90)}, {"counterclockwise", []float64{90}, new(270)},
		{"upside down", []float64{180}, new(180)}, {"wraparound", []float64{450}, new(270)},
		{"boundary", []float64{-65}, new(90)}, {"outside boundary", []float64{-64.99}, nil},
		{"diagonal", []float64{50}, nil}, {"agree", []float64{80, 100}, new(270)},
		{"disagree", []float64{90, 180}, nil}, {"upright dissents", []float64{90, 0}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rolls := []*float64{nil}
			for _, degrees := range tc.degrees {
				rolls = append(rolls, new(degrees*math.Pi/180))
			}
			require.Equal(t, tc.want, PhotoRotationFromRolls(rolls))
		})
	}
	description := PhotoDescription{Rotation: new(90)}
	require.NoError(t, ValidatePhotoDescription(description))
	description.Rotation = new(45)
	require.Error(t, ValidatePhotoDescription(description))
}
