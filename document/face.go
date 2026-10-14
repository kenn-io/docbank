package document

import (
	"errors"
	"math"
)

// MaxPhotoRecordsPerVersion bounds both faces and depicted assertions on one version.
const MaxPhotoRecordsPerVersion = 1000

// FaceBox uses normalized coordinates in the EXIF-oriented preview before authored rotation.
type FaceBox struct {
	X      float64 `json:"x"`
	Y      float64 `json:"y"`
	Width  float64 `json:"width"`
	Height float64 `json:"height"`
}

func (b FaceBox) Validate() error {
	for _, v := range []float64{b.X, b.Y, b.Width, b.Height} {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return errors.New("invalid face box")
		}
	}
	if b.X < 0 || b.Y < 0 || b.Width <= 0 || b.Height <= 0 || b.X+b.Width > 1 || b.Y+b.Height > 1 {
		return errors.New("invalid face box")
	}
	return nil
}
