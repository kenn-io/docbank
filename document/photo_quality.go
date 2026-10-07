package document

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"math"
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
	for _, v := range []float64{s.Focus, s.Blur, s.Brightness, s.ColorRed, s.ColorGreen, s.ColorBlue, s.Framing, s.Aesthetics} {
		if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 || v > 1 {
			return errors.New("photo quality scores must be finite and within 0..1")
		}
	}
	return nil
}

// PhotoQualityEvaluatorFingerprint includes every choice that affects measurements.
func PhotoQualityEvaluatorFingerprint() string {
	recipe, _ := BuiltInVisualPreviewRecipe("grid")
	_, fingerprint, _ := MarshalVisualPreviewRecipeV1(recipe)
	digest := sha256.Sum256([]byte("photo-quality:v2:16x16:catmull-rom:focus-ceiling=0.15:" + fingerprint))
	return hex.EncodeToString(digest[:])
}
