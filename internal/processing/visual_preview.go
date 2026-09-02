package processing

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
	"io"
	"runtime"

	xdraw "golang.org/x/image/draw"

	"go.kenn.io/docbank/document"
)

const (
	visualPreviewMaxEdgePixels   = 2048
	visualPreviewMaxSourcePixels = 100_000_000
	visualPreviewJPEGQuality     = 90
	visualPreviewMaxJPEGSegments = 1024
)

var visualPreviewRecipe = document.VisualPreviewRecipeV1{
	ContractVersion:   document.VisualPreviewContractV1,
	MaxEdgePixels:     visualPreviewMaxEdgePixels,
	OutputMediaType:   "image/jpeg",
	OrientationPolicy: "apply",
	ColorPolicy:       "srgb",
	FramePolicy:       "primary",
	ProcessorFingerprint: fingerprintVisualPreviewProcessor(
		"docbank-visual-preview:jpeg-stdlib-" + runtime.Version() +
			"+x-image-draw-v0.44.0:max-edge=2048:quality=90:v1"),
}

// VisualPreviewTarget identifies one exact immutable source to process.
type VisualPreviewTarget struct {
	SourceSHA256 string
	Size         int64
	MediaType    string
}

// VisualPreviewProduct is one canonical result and any ready output bytes.
type VisualPreviewProduct struct {
	Preview document.VisualPreviewV1
	Output  []byte
}

// CurrentVisualPreviewRecipe returns the complete built-in preview identity.
func CurrentVisualPreviewRecipe() document.VisualPreviewRecipeV1 {
	return visualPreviewRecipe
}

// ProduceVisualPreview verifies and processes one exact source. Deterministic
// source failures become terminal results; storage and verification failures
// remain retryable errors.
func ProduceVisualPreview(
	ctx context.Context, source io.ReadSeeker, target VisualPreviewTarget,
) (VisualPreviewProduct, error) {
	base := document.VisualPreviewV1{
		ContractVersion: document.VisualPreviewContractV1,
		SourceSHA256:    target.SourceSHA256,
		Recipe:          CurrentVisualPreviewRecipe(),
	}
	if err := verifySeekableSource(ctx, source, target.SourceSHA256, target.Size); err != nil {
		return VisualPreviewProduct{}, sourceContentUnavailable(
			fmt.Errorf("verifying visual preview source: %w", err))
	}
	if target.MediaType != "image/jpeg" {
		base.State = document.VisualPreviewUnsupported
		base.Failure = &document.VisualPreviewFailureV1{
			Code: "unsupported_media_type", Detail: "the built-in preview producer supports JPEG originals",
		}
		return VisualPreviewProduct{Preview: base}, nil
	}
	orientation, unsupportedColor, malformed, err := inspectVisualPreviewJPEG(ctx, source)
	if err != nil {
		return VisualPreviewProduct{}, sourceContentUnavailable(
			fmt.Errorf("inspecting visual preview JPEG: %w", err))
	}
	if malformed {
		return failedVisualPreview(base, "decode_failed", "the verified JPEG header is malformed"), nil
	}
	if unsupportedColor {
		base.State = document.VisualPreviewUnsupported
		base.Failure = &document.VisualPreviewFailureV1{
			Code: "unsupported_color_profile", Detail: "the built-in preview producer requires sRGB JPEG originals",
		}
		return VisualPreviewProduct{Preview: base}, nil
	}
	if _, err := source.Seek(0, io.SeekStart); err != nil {
		return VisualPreviewProduct{}, sourceContentUnavailable(
			fmt.Errorf("seeking visual preview source: %w", err))
	}
	config, err := jpeg.DecodeConfig(source)
	if err != nil {
		return visualPreviewJPEGDecodeResult(base, "the verified JPEG header is malformed", err)
	}
	if !visualPreviewJPEGColorModelSupported(config.ColorModel) {
		base.State = document.VisualPreviewUnsupported
		base.Failure = &document.VisualPreviewFailureV1{
			Code: "unsupported_color_profile", Detail: "the built-in preview producer requires sRGB JPEG originals",
		}
		return VisualPreviewProduct{Preview: base}, nil
	}
	if config.Width < 1 || config.Height < 1 ||
		int64(config.Width)*int64(config.Height) > visualPreviewMaxSourcePixels {
		return failedVisualPreview(base, "source_dimensions_exceed_limit",
			"the JPEG dimensions exceed the built-in preview limit"), nil
	}
	if _, err := source.Seek(0, io.SeekStart); err != nil {
		return VisualPreviewProduct{}, sourceContentUnavailable(
			fmt.Errorf("seeking visual preview source: %w", err))
	}
	decoded, err := jpeg.Decode(source)
	if err != nil {
		return visualPreviewJPEGDecodeResult(base, "the verified JPEG cannot be decoded", err)
	}
	if decoded.Bounds().Dx() != config.Width || decoded.Bounds().Dy() != config.Height {
		return failedVisualPreview(base, "decode_failed", "the JPEG dimensions changed during decoding"), nil
	}
	orientedWidth, orientedHeight := visualPreviewOrientedDimensions(config.Width, config.Height, orientation)
	width, height := boundedVisualPreviewDimensions(orientedWidth, orientedHeight)
	resizeWidth, resizeHeight := width, height
	if visualPreviewOrientationSwapsDimensions(orientation) {
		resizeWidth, resizeHeight = height, width
	}
	resized := image.NewNRGBA(image.Rect(0, 0, resizeWidth, resizeHeight))
	if resizeWidth == config.Width && resizeHeight == config.Height {
		draw.Draw(resized, resized.Bounds(), decoded, decoded.Bounds().Min, draw.Src)
	} else {
		xdraw.CatmullRom.Scale(resized, resized.Bounds(), decoded, decoded.Bounds(), draw.Src, nil)
	}
	preview := applyVisualPreviewOrientation(resized, orientation)
	var encoded bytes.Buffer
	if err := jpeg.Encode(&encoded, preview, &jpeg.Options{Quality: visualPreviewJPEGQuality}); err != nil {
		return VisualPreviewProduct{}, fmt.Errorf("encoding visual preview: %w", err)
	}
	output := encoded.Bytes()
	digest := sha256.Sum256(output)
	base.State = document.VisualPreviewReady
	base.Output = &document.VisualPreviewOutputV1{
		BlobSHA256: hex.EncodeToString(digest[:]), Size: int64(len(output)),
		MediaType: "image/jpeg", Width: width, Height: height,
	}
	return VisualPreviewProduct{Preview: base, Output: output}, nil
}

func inspectVisualPreviewJPEG(
	ctx context.Context, source io.ReadSeeker,
) (orientation int, unsupportedColor, malformed bool, err error) {
	if _, err := source.Seek(0, io.SeekStart); err != nil {
		return 0, false, false, err
	}
	var signature [2]byte
	if _, err := io.ReadFull(source, signature[:]); err != nil {
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			return 0, false, true, nil
		}
		return 0, false, false, err
	}
	if signature != [2]byte{0xff, 0xd8} {
		return 0, false, true, nil
	}
	orientation = 1
	for range visualPreviewMaxJPEGSegments {
		if err := ctx.Err(); err != nil {
			return 0, false, false, err
		}
		marker, markerErr := readVisualPreviewJPEGMarker(source)
		if markerErr != nil {
			if errors.Is(markerErr, io.EOF) || errors.Is(markerErr, io.ErrUnexpectedEOF) {
				return 0, false, true, nil
			}
			return 0, false, false, markerErr
		}
		if marker == 0xd9 || marker == 0xda {
			return orientation, unsupportedColor, false, nil
		}
		if marker == 0x01 || marker >= 0xd0 && marker <= 0xd7 {
			continue
		}
		var lengthBytes [2]byte
		if _, err := io.ReadFull(source, lengthBytes[:]); err != nil {
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				return 0, false, true, nil
			}
			return 0, false, false, err
		}
		length := int(binary.BigEndian.Uint16(lengthBytes[:]))
		if length < 2 {
			return 0, false, true, nil
		}
		payload := make([]byte, length-2)
		if _, err := io.ReadFull(source, payload); err != nil {
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				return 0, false, true, nil
			}
			return 0, false, false, err
		}
		switch {
		case marker == 0xe1 && bytes.HasPrefix(payload, []byte("Exif\x00\x00")):
			if exifOrientation, colorSpace, found := visualPreviewEXIF(payload[6:]); found {
				orientation = exifOrientation
				unsupportedColor = unsupportedColor || colorSpace != 0 && colorSpace != 1
			}
		case marker == 0xe2 && bytes.HasPrefix(payload, []byte("ICC_PROFILE\x00")):
			unsupportedColor = true
		}
	}
	return 0, false, true, nil
}

func readVisualPreviewJPEGMarker(source io.Reader) (byte, error) {
	var value [1]byte
	for {
		if _, err := io.ReadFull(source, value[:]); err != nil {
			return 0, err
		}
		if value[0] != 0xff {
			return 0, io.ErrUnexpectedEOF
		}
		for value[0] == 0xff {
			if _, err := io.ReadFull(source, value[:]); err != nil {
				return 0, err
			}
		}
		if value[0] != 0x00 {
			return value[0], nil
		}
	}
}

func visualPreviewEXIF(data []byte) (orientation, colorSpace int, found bool) {
	reader, root, ok := sourceMetadataTIFFRoot(data)
	if !ok {
		return 1, 0, false
	}
	orientation = 1
	if value, ok := exifUnsigned(reader, root[0x0112]); ok && value >= 1 && value <= 8 {
		orientation = int(value)
	}
	if raw := root[0x8769]; len(raw) >= 4 {
		exif := reader.entries(reader.order.Uint32(raw))
		if value, ok := exifUnsigned(reader, exif[0xa001]); ok {
			colorSpace = int(value)
		}
	}
	return orientation, colorSpace, true
}

func visualPreviewJPEGColorModelSupported(model color.Model) bool {
	_, cmyk := model.Convert(color.Black).(color.CMYK)
	return !cmyk
}

func visualPreviewJPEGDecodeResult(
	base document.VisualPreviewV1, malformedDetail string, err error,
) (VisualPreviewProduct, error) {
	if _, ok := errors.AsType[jpeg.UnsupportedError](err); ok {
		base.State = document.VisualPreviewUnsupported
		base.Failure = &document.VisualPreviewFailureV1{
			Code: "unsupported_jpeg_feature", Detail: "the JPEG uses a feature unavailable to the built-in decoder",
		}
		return VisualPreviewProduct{Preview: base}, nil
	}
	if _, ok := errors.AsType[jpeg.FormatError](err); ok ||
		errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return failedVisualPreview(base, "decode_failed", malformedDetail), nil
	}
	return VisualPreviewProduct{}, sourceContentUnavailable(
		fmt.Errorf("reading visual preview JPEG: %w", err))
}

func visualPreviewOrientationSwapsDimensions(orientation int) bool {
	return orientation >= 5 && orientation <= 8
}

func visualPreviewOrientedDimensions(width, height, orientation int) (int, int) {
	if visualPreviewOrientationSwapsDimensions(orientation) {
		return height, width
	}
	return width, height
}

func applyVisualPreviewOrientation(source *image.NRGBA, orientation int) *image.NRGBA {
	if orientation == 1 {
		return source
	}
	width, height := source.Bounds().Dx(), source.Bounds().Dy()
	outputWidth, outputHeight := visualPreviewOrientedDimensions(width, height, orientation)
	output := image.NewNRGBA(image.Rect(0, 0, outputWidth, outputHeight))
	for y := range outputHeight {
		for x := range outputWidth {
			sourceX, sourceY := x, y
			switch orientation {
			case 2:
				sourceX = width - 1 - x
			case 3:
				sourceX, sourceY = width-1-x, height-1-y
			case 4:
				sourceY = height - 1 - y
			case 5:
				sourceX, sourceY = y, x
			case 6:
				sourceX, sourceY = y, height-1-x
			case 7:
				sourceX, sourceY = width-1-y, height-1-x
			case 8:
				sourceX, sourceY = width-1-y, x
			}
			output.SetNRGBA(x, y, source.NRGBAAt(sourceX, sourceY))
		}
	}
	return output
}

func fingerprintVisualPreviewProcessor(descriptor string) string {
	digest := sha256.Sum256([]byte(descriptor))
	return hex.EncodeToString(digest[:])
}

func failedVisualPreview(
	base document.VisualPreviewV1, code, detail string,
) VisualPreviewProduct {
	base.State = document.VisualPreviewFailed
	base.Failure = &document.VisualPreviewFailureV1{Code: code, Detail: detail}
	return VisualPreviewProduct{Preview: base}
}

func boundedVisualPreviewDimensions(width, height int) (int, int) {
	if width <= visualPreviewMaxEdgePixels && height <= visualPreviewMaxEdgePixels {
		return width, height
	}
	if width >= height {
		return visualPreviewMaxEdgePixels,
			max(1, (height*visualPreviewMaxEdgePixels+width/2)/width)
	}
	return max(1, (width*visualPreviewMaxEdgePixels+height/2)/height),
		visualPreviewMaxEdgePixels
}
