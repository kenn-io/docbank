package processing

import (
	"context"
	"errors"
	"fmt"
	"image"
	"io"

	_ "golang.org/x/image/webp" // Register WebP for the shared image decoder.
)

var errVisualDimensions = errors.New("source dimensions exceed the built-in limit")
var errVisualColor = errors.New("unsupported JPEG color model")
var errVisualFormat = errors.New("unexpected image format")
var errVisualDecodeBounds = errors.New("image dimensions changed during decoding")

type visualPixels struct {
	image      image.Image
	header     bool
	seekFailed bool
	readErr    error
}

func decodeVisualPixels(ctx context.Context, source io.ReadSeeker, format string) (result visualPixels, err error) {
	if _, err = source.Seek(0, io.SeekStart); err != nil {
		result.readErr = err
		result.seekFailed = true
		return result, err
	}
	reader := &visualPreviewReadErrorRecorder{reader: &contextReader{ctx, source}}
	defer func() {
		if reader.err != nil {
			result.readErr = reader.err
			err = errors.Join(err, reader.err)
		}
	}()
	result.header = true
	config, actual, err := image.DecodeConfig(reader)
	if err != nil {
		return result, fmt.Errorf("reading image dimensions: %w", err)
	}
	if actual != format {
		return result, errVisualFormat
	}
	if format == visualFormatJPEG && !visualPreviewJPEGColorModelSupported(config.ColorModel) {
		return result, errVisualColor
	}
	if !visualPreviewDimensionsAllowed(config.Width, config.Height) {
		return result, errVisualDimensions
	}
	if _, err = source.Seek(0, io.SeekStart); err != nil {
		result.readErr = err
		result.seekFailed = true
		return result, err
	}
	result.header = false
	if format == "gif" {
		result.image, err = decodeGIFCanvas(reader, config)
	} else {
		result.image, _, err = image.Decode(reader)
	}
	if err != nil {
		return result, fmt.Errorf("decoding image pixels: %w", err)
	}
	if result.image.Bounds().Dx() != config.Width || result.image.Bounds().Dy() != config.Height {
		return result, errVisualDecodeBounds
	}
	return result, ctx.Err()
}
