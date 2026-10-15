package processing

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"io"
	"log/slog"
	"runtime/debug"

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

func decodeVisualPixels(ctx context.Context, source io.ReadSeeker, format string, allowReducedJPEG bool) (result visualPixels, err error) {
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
	if !visualPreviewDimensionsAllowed(config.Width, config.Height, visualPreviewMaxSourcePixels) {
		if allowReducedJPEG && format == visualFormatJPEG &&
			visualPreviewDimensionsAllowed(config.Width, config.Height, visualPreviewMaxDCSourcePixels) {
			// Decode only the DC coefficients: the image comes back at one
			// eighth width and height, so the bounds check below is skipped.
			if _, err = source.Seek(0, io.SeekStart); err != nil {
				result.readErr = err
				result.seekFailed = true
				return result, err
			}
			result.header = false
			result.image, err = decodeVisualPreviewJPEGDCSafely(ctx, bufio.NewReaderSize(reader, 64*1024))
			if errors.Is(err, errVisualDimensions) {
				// Baseline JPEGs have a lower reduced-decode ceiling.
				result.header = true
				return result, errVisualDimensions
			}
			if err != nil {
				return result, fmt.Errorf("decoding image pixels: %w", err)
			}
			if bounds := result.image.Bounds(); bounds.Dx() != (config.Width+7)/8 || bounds.Dy() != (config.Height+7)/8 {
				return result, errVisualDecodeBounds
			}
			return result, ctx.Err()
		}
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

// decodeVisualPreviewJPEGDCSafely turns a decoder panic into a durable JPEG
// format error so one malformed original fails its own preview once instead of
// stopping the worker or retrying forever. The stored failure is generic, so the
// panic and its stack go to the log for diagnosis.
func decodeVisualPreviewJPEGDCSafely(ctx context.Context, reader *bufio.Reader) (img image.Image, err error) {
	defer func() {
		if r := recover(); r != nil {
			slog.ErrorContext(ctx, "reduced JPEG preview decoder panicked",
				"panic", r, "stack", string(debug.Stack()))
			img, err = nil, jpeg.FormatError(fmt.Sprintf("decoder panic: %v", r))
		}
	}()
	return decodeVisualPreviewJPEGDC(ctx, reader)
}
