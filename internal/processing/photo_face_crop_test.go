package processing

import (
	"bytes"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"image"
	"image/color"
	"image/jpeg"
	"testing"
)

func TestCropPhotoFace(t *testing.T) {
	t.Parallel()
	source := image.NewRGBA(image.Rect(0, 0, 100, 80))
	for y := range 80 {
		for x := range 100 {
			c := color.RGBA{B: 255, A: 255}
			if x >= 60 {
				c = color.RGBA{R: 255, A: 255}
			}
			source.Set(x, y, c)
		}
	}
	var encoded bytes.Buffer
	require.NoError(t, jpeg.Encode(&encoded, source, &jpeg.Options{Quality: 100}))
	for _, test := range []struct {
		name string
		box  document.FaceBox
		side int
	}{
		{"padded", document.FaceBox{X: .7, Y: .3, Width: .2, Height: .2}, 32},
		{"tiny at edge", document.FaceBox{X: .99, Y: .99, Width: .01, Height: .01}, 24},
		{"full frame", document.FaceBox{Width: 1, Height: 1}, 80},
	} {
		t.Run(test.name, func(t *testing.T) {
			data, err := CropPhotoFace(encoded.Bytes(), test.box)
			require.NoError(t, err)
			crop, err := jpeg.Decode(bytes.NewReader(data))
			require.NoError(t, err)
			require.Equal(t, image.Rect(0, 0, test.side, test.side), crop.Bounds())
			if test.name != "full frame" {
				r, _, b, _ := crop.At(test.side/2, test.side/2).RGBA()
				require.Greater(t, r, uint32(60000))
				require.Less(t, b, uint32(3000))
			}
		})
	}
	var gray bytes.Buffer
	require.NoError(t, jpeg.Encode(&gray, image.NewGray(image.Rect(0, 0, 30, 40)), nil))
	_, err := CropPhotoFace(gray.Bytes(), document.FaceBox{Width: .1, Height: .1})
	require.NoError(t, err)
	_, err = CropPhotoFace([]byte("invalid"), document.FaceBox{Width: .2, Height: .2})
	require.Error(t, err)
}
