package processing

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/draw"
	"image/jpeg"
	"math"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/blob"
	"go.kenn.io/docbank/internal/store"
	xdraw "golang.org/x/image/draw"
)

// EvaluatePhotoQuality reads only the verified, oriented grid preview.
func EvaluatePhotoQuality(ctx context.Context, s *store.Store, blobs *blob.Store, target store.PhotoVisualPreviewTarget) error {
	recipe, _ := VisualPreviewRecipeForSize("grid")
	_, fingerprint, _ := document.MarshalVisualPreviewRecipeV1(recipe)
	view, err := s.ContentVersionVisualPreviewByRecipe(ctx, target.VersionID, fingerprint)
	if err != nil {
		return err
	}

	output := view.Generation.Preview.Output
	if view.Generation.Preview.State != document.VisualPreviewReady || output == nil {
		return errors.New("photo quality preview unavailable")
	}
	body, err := readExportBlob(ctx, blobs, output.BlobSHA256, output.Size, 4<<20)
	if err != nil {
		return fmt.Errorf("reading photo quality preview: %w", err)
	}
	config, err := jpeg.DecodeConfig(bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("reading photo quality preview dimensions: %w", err)
	}
	if config.Width != output.Width || config.Height != output.Height {
		return errors.New("photo quality preview dimensions mismatch")
	}
	decoded, err := jpeg.Decode(bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("decoding photo quality preview: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return s.PublishPhotoQualitySignals(ctx, target, measurePhotoQuality(decoded))
}

func measurePhotoQuality(source image.Image) document.PhotoQualitySignals {
	const edge = 16
	sampled := image.NewRGBA(image.Rect(0, 0, edge, edge))
	xdraw.CatmullRom.Scale(sampled, sampled.Bounds(), source, source.Bounds(), draw.Src, nil)
	var s document.PhotoQualitySignals
	var luminance [edge * edge]float64
	for y := range edge {
		for x := range edge {
			p := sampled.RGBAAt(x, y)
			r, g, b := float64(p.R)/255, float64(p.G)/255, float64(p.B)/255
			s.ColorRed += r / 256
			s.ColorGreen += g / 256
			s.ColorBlue += b / 256
			luminance[y*edge+x] = 0.2126*r + 0.7152*g + 0.0722*b
			s.Brightness += luminance[y*edge+x] / 256
		}
	}
	var delta, wx, wy, weight float64
	count := 0
	for y := range edge {
		for x := range edge {
			l := luminance[y*edge+x]
			if x+1 < edge {
				delta += math.Abs(l - luminance[y*edge+x+1])
				count++
			}
			if y+1 < edge {
				delta += math.Abs(l - luminance[(y+1)*edge+x])
				count++
			}
			w := math.Abs(l - s.Brightness)
			weight += w
			wx += (float64(x) + 0.5) / edge * w
			wy += (float64(y) + 0.5) / edge * w
		}
	}
	s.Focus = min(max(delta/float64(count)/0.15, 0), 1)
	s.Blur = 1 - s.Focus
	s.Framing = 0.5
	if weight > 0.0001 {
		distance := math.Inf(1)
		for _, x := range []float64{1.0 / 3, 2.0 / 3} {
			for _, y := range []float64{1.0 / 3, 2.0 / 3} {
				distance = min(distance, math.Hypot(wx/weight-x, wy/weight-y))
			}
		}
		s.Framing = min(max(1-distance/0.5, 0), 1)
	}
	balanced := 1 - min(math.Abs(s.Brightness-0.5)*2, 1)
	contrast := max(s.ColorRed, s.ColorGreen, s.ColorBlue) - min(s.ColorRed, s.ColorGreen, s.ColorBlue)
	s.Aesthetics = min(max(s.Focus*0.35+balanced*0.25+contrast*0.20+s.Framing*0.20, 0), 1)
	return s
}
