package processing

import (
	"bytes"
	"fmt"
	"image"
	"image/draw"
	"image/jpeg"
	"math"

	"go.kenn.io/docbank/document"
)

// CropPhotoFace returns a padded square closeup in the preview's oriented coordinates.
func CropPhotoFace(data []byte, box document.FaceBox) ([]byte, error) {
	if err := box.Validate(); err != nil {
		return nil, err
	}
	config, err := jpeg.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("reading face preview: %w", err)
	}
	if config.Width <= 0 || config.Height <= 0 || config.Width > visualPreviewMaxEdgePixels || config.Height > visualPreviewMaxEdgePixels {
		return nil, fmt.Errorf("invalid face preview dimensions")
	}
	decoded, err := jpeg.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("decoding face preview: %w", err)
	}
	side := min(config.Width, config.Height, max(24, int(math.Ceil(1.6*max(box.Width*float64(config.Width), box.Height*float64(config.Height))))))
	x := max(0, min(config.Width-side, int(math.Round((box.X+box.Width/2)*float64(config.Width)-float64(side)/2))))
	y := max(0, min(config.Height-side, int(math.Round((box.Y+box.Height/2)*float64(config.Height)-float64(side)/2))))
	crop := image.NewRGBA(image.Rect(0, 0, side, side))
	draw.Draw(crop, crop.Bounds(), decoded, image.Pt(x, y), draw.Src)
	var output bytes.Buffer
	if err := jpeg.Encode(&output, crop, &jpeg.Options{Quality: 90}); err != nil {
		return nil, fmt.Errorf("encoding face crop: %w", err)
	}
	return output.Bytes(), nil
}
