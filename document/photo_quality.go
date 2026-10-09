package document

import (
	"errors"
	"fmt"
	"math"
	"sync"
)

const (
	photoQualityRevision     = "v2"
	PhotoQualityFocusCeiling = 0.15
)

// PhotoQualitySignals measures preview pixels on a calibrated 0..1 scale.
type PhotoQualitySignals struct {
	Focus      float64 `json:"focus" minimum:"0" maximum:"1"`
	Blur       float64 `json:"blur" minimum:"0" maximum:"1"`
	Brightness float64 `json:"brightness" minimum:"0" maximum:"1"`
	ColorRed   float64 `json:"color_red" minimum:"0" maximum:"1"`
	ColorGreen float64 `json:"color_green" minimum:"0" maximum:"1"`
	ColorBlue  float64 `json:"color_blue" minimum:"0" maximum:"1"`
	Framing    float64 `json:"framing" minimum:"0" maximum:"1"`
	Aesthetics float64 `json:"aesthetics" minimum:"0" maximum:"1"`
}

func ValidatePhotoQualitySignals(s PhotoQualitySignals) error {
	for _, v := range []float64{
		s.Focus, s.Blur, s.Brightness, s.ColorRed, s.ColorGreen, s.ColorBlue, s.Framing, s.Aesthetics,
	} {
		if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 || v > 1 {
			return errors.New("photo quality scores must be finite and within 0..1")
		}
	}
	return nil
}

// PhotoQualityFingerprints identifies the grid preview the evaluator reads and
// the evaluator itself, which includes every choice that affects measurements.
type PhotoQualityFingerprints struct {
	GridRecipe string
	Evaluator  string
}

var photoQualityFingerprints = sync.OnceValues(func() (PhotoQualityFingerprints, error) {
	recipe, err := BuiltInVisualPreviewRecipe("grid")
	if err != nil {
		return PhotoQualityFingerprints{}, err
	}
	_, grid, err := MarshalVisualPreviewRecipeV1(recipe)
	if err != nil {
		return PhotoQualityFingerprints{}, fmt.Errorf("fingerprinting photo quality grid recipe: %w", err)
	}
	evaluator := sha256Hex(fmt.Appendf(nil, "photo-quality:%s:16x16:catmull-rom:focus-ceiling=%g:%s", photoQualityRevision, PhotoQualityFocusCeiling, grid))
	return PhotoQualityFingerprints{GridRecipe: grid, Evaluator: evaluator}, nil
})

// CurrentPhotoQualityFingerprints returns the fingerprints computed once per process.
func CurrentPhotoQualityFingerprints() (PhotoQualityFingerprints, error) {
	return photoQualityFingerprints()
}
