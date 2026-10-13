package document

import (
	"errors"
	"math"
)

// FaceBox uses normalized coordinates in the unrotated observation image.
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

// Rotate maps the observation box to the authored clockwise display rotation.
func (b FaceBox) Rotate(degrees int) (FaceBox, error) {
	if err := b.Validate(); err != nil {
		return FaceBox{}, err
	}
	switch degrees {
	case 0:
		return b, nil
	case 90:
		return FaceBox{1 - (b.Y + b.Height), b.X, b.Height, b.Width}, nil
	case 180:
		return FaceBox{1 - (b.X + b.Width), 1 - (b.Y + b.Height), b.Width, b.Height}, nil
	case 270:
		return FaceBox{b.Y, 1 - (b.X + b.Width), b.Height, b.Width}, nil
	default:
		return FaceBox{}, errors.New("invalid face rotation")
	}
}

func (b FaceBox) Overlap(other FaceBox) float64 {
	width := math.Max(0, math.Min(b.X+b.Width, other.X+other.Width)-math.Max(b.X, other.X))
	height := math.Max(0, math.Min(b.Y+b.Height, other.Y+other.Height)-math.Max(b.Y, other.Y))
	intersection := width * height
	return intersection / (b.Width*b.Height + other.Width*other.Height - intersection)
}
