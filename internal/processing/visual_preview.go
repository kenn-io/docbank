package processing

import (
	"bytes"
	"compress/flate"
	"compress/zlib"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/gif"
	"image/jpeg"
	"image/png"
	"io"
	"mime"

	xdraw "golang.org/x/image/draw"

	"go.kenn.io/docbank/document"
)

const (
	visualPreviewProcessorDescriptor = document.VisualPreviewProcessorDescriptor
	visualPreviewMaxEdgePixels       = document.VisualPreviewMaxEdgePixels
	visualPreviewMaxSourcePixels     = document.VisualPreviewMaxSourcePixels
	visualPreviewJPEGQuality         = document.VisualPreviewJPEGQuality
	visualPreviewMaxJPEGSegments     = 1024
	visualPreviewMaxPNGChunks        = 1024
	visualPreviewMaxWebPChunks       = 1024
	visualPreviewMaxEXIFBytes        = 1 << 20
	visualPreviewMaxICCBytes         = 1 << 20
	visualPreviewWebPAnimation       = 1 << 1
	visualPreviewWebPEXIF            = 1 << 3
	visualPreviewWebPICCProfile      = 1 << 5
)

var visualPreviewRecipe = func() document.VisualPreviewRecipeV1 {
	recipe, err := document.BuiltInVisualPreviewRecipe("large")
	if err != nil {
		panic(err)
	}
	return recipe
}()

// VisualPreviewRecipeForSize returns a canonical built-in size recipe.
func VisualPreviewRecipeForSize(size string) (document.VisualPreviewRecipeV1, error) {
	return document.BuiltInVisualPreviewRecipe(size)
}

func validateBuiltInVisualPreviewRecipe(recipe document.VisualPreviewRecipeV1) error {
	for _, size := range []string{"grid", "fit", "large"} {
		builtIn, _ := VisualPreviewRecipeForSize(size)
		if recipe == builtIn {
			return nil
		}
	}
	return errors.New("visual preview recipe is not a canonical built-in")
}

// VisualPreviewSupportsMediaType reports whether a decoder path exists.
func VisualPreviewSupportsMediaType(mediaType string) bool {
	return visualPreviewFormat(mediaType) != ""
}

func visualPreviewFormat(mediaType string) string {
	switch visualPreviewSourceMediaType(mediaType) {
	case "image/jpeg":
		return "jpeg"
	case "image/png":
		return "png"
	case "image/gif":
		return "gif"
	case "image/webp":
		return "webp"
	case "image/x-sony-arw", "image/x-adobe-dng", "image/x-canon-cr2", "image/x-nikon-nef", "image/x-fuji-raf":
		return "raw"
	default:
		return ""
	}
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

// CurrentVisualPreviewRecipe returns the legacy large preview identity.
func CurrentVisualPreviewRecipe() document.VisualPreviewRecipeV1 {
	return visualPreviewRecipe
}

// ProduceVisualPreview verifies and processes one exact source. Deterministic
// source failures become terminal results; storage and verification failures
// remain retryable errors.
func ProduceVisualPreview(
	ctx context.Context, source io.ReadSeeker, target VisualPreviewTarget,
) (VisualPreviewProduct, error) {
	return ProduceVisualPreviewForRecipe(ctx, source, target, CurrentVisualPreviewRecipe())
}

// ProduceVisualPreviewForRecipe verifies the source and produces the selected built-in recipe.
func ProduceVisualPreviewForRecipe(
	ctx context.Context, source io.ReadSeeker, target VisualPreviewTarget, recipe document.VisualPreviewRecipeV1,
) (VisualPreviewProduct, error) {
	if err := validateBuiltInVisualPreviewRecipe(recipe); err != nil {
		return VisualPreviewProduct{}, err
	}
	base := document.VisualPreviewV1{
		ContractVersion: document.VisualPreviewContractV1,
		SourceSHA256:    target.SourceSHA256,
		Recipe:          recipe,
	}
	if err := verifySeekableSource(ctx, source, target.SourceSHA256, target.Size); err != nil {
		return VisualPreviewProduct{}, sourceContentUnavailable(
			fmt.Errorf("verifying visual preview source: %w", err))
	}
	mediaType := visualPreviewSourceMediaType(target.MediaType)
	switch visualPreviewFormat(mediaType) {
	case "jpeg":
		return produceVisualPreviewJPEG(ctx, source, base)
	case "png":
		return produceVisualPreviewPNG(ctx, source, target.Size, base)
	case "gif":
		return decodeAndEncodeVisualPreview(ctx, source, base, "gif", 1, visualPreviewColorSpace{})
	case "webp":
		return produceVisualPreviewWebP(ctx, source, target.Size, base)
	case "raw":
		return produceVisualPreviewCameraRAW(ctx, source, target.Size, mediaType, base)
	default:
		base.State = document.VisualPreviewUnsupported
		base.Failure = &document.VisualPreviewFailureV1{
			Code:   "unsupported_media_type",
			Detail: "the built-in preview producer does not support this original's media type",
		}
		return VisualPreviewProduct{Preview: base}, nil
	}
}

func produceVisualPreviewWebP(
	ctx context.Context, source io.ReadSeeker, sourceSize int64, base document.VisualPreviewV1,
) (VisualPreviewProduct, error) {
	orientation, colorSpace, unsupportedMetadata, animated, malformed, err :=
		inspectVisualPreviewWebP(ctx, source, sourceSize)
	if err != nil {
		return VisualPreviewProduct{}, sourceContentUnavailable(
			fmt.Errorf("inspecting visual preview WebP: %w", err))
	}
	if malformed {
		return failedVisualPreview(base, "decode_failed", "the verified WebP container is malformed"), nil
	}
	if animated {
		base.State = document.VisualPreviewUnsupported
		base.Failure = &document.VisualPreviewFailureV1{
			Code: "unsupported_webp_animation", Detail: "the built-in preview producer requires a still WebP original",
		}
		return VisualPreviewProduct{Preview: base}, nil
	}
	if unsupportedMetadata {
		base.State = document.VisualPreviewUnsupported
		base.Failure = &document.VisualPreviewFailureV1{
			Code: "unsupported_webp_metadata", Detail: "the WebP metadata exceeds the built-in preview limit",
		}
		return VisualPreviewProduct{Preview: base}, nil
	}
	return decodeAndEncodeVisualPreview(ctx, source, base, "webp", orientation, colorSpace)
}

func produceVisualPreviewJPEG(
	ctx context.Context, source io.ReadSeeker, base document.VisualPreviewV1,
) (VisualPreviewProduct, error) {
	return produceVisualPreviewJPEGWithOrientation(ctx, source, base, 0)
}

func produceVisualPreviewJPEGWithOrientation(
	ctx context.Context, source io.ReadSeeker, base document.VisualPreviewV1, containerOrientation int,
) (VisualPreviewProduct, error) {
	orientation, colorSpace, malformed, err := inspectVisualPreviewJPEG(ctx, source)
	if err != nil {
		return VisualPreviewProduct{}, sourceContentUnavailable(
			fmt.Errorf("inspecting visual preview JPEG: %w", err))
	}
	if malformed {
		return failedVisualPreview(base, "decode_failed", "the verified JPEG header is malformed"), nil
	}
	if containerOrientation >= 1 && containerOrientation <= 8 {
		orientation = containerOrientation
	}
	return decodeAndEncodeVisualPreview(ctx, source, base, "jpeg", orientation, colorSpace)
}

func produceVisualPreviewPNG(
	ctx context.Context, source io.ReadSeeker, sourceSize int64, base document.VisualPreviewV1,
) (VisualPreviewProduct, error) {
	orientation, colorSpace, unsupportedMetadata, malformed, trailingOffset, err :=
		inspectVisualPreviewPNG(ctx, source, sourceSize)
	if err != nil {
		return VisualPreviewProduct{}, sourceContentUnavailable(
			fmt.Errorf("inspecting visual preview PNG: %w", err))
	}
	if malformed {
		return failedVisualPreview(base, "decode_failed", "the verified PNG header is malformed"), nil
	}
	if unsupportedMetadata {
		base.State = document.VisualPreviewUnsupported
		base.Failure = &document.VisualPreviewFailureV1{
			Code: "unsupported_png_metadata", Detail: "the PNG EXIF metadata exceeds the built-in preview limit",
		}
		return VisualPreviewProduct{Preview: base}, nil
	}
	pixels, err := decodeVisualPixels(ctx, source, visualFormatPNG)
	if err != nil {
		return visualPreviewDecodeResult(base, visualFormatPNG, pixels, err)
	}
	if trailingOffset != 0 {
		orientation, err = trailingVisualPreviewPNGOrientation(ctx, source, sourceSize, trailingOffset)
		if err != nil {
			return VisualPreviewProduct{}, sourceContentUnavailable(fmt.Errorf("inspecting visual preview PNG: %w", err))
		}
	}
	return encodeVisualPreview(ctx, base, pixels.image, orientation, colorSpace)
}

func encodeVisualPreview(
	ctx context.Context,
	base document.VisualPreviewV1,
	decoded image.Image,
	orientation int,
	colorSpace visualPreviewColorSpace,
) (VisualPreviewProduct, error) {
	preview, err := transformPhotoPixels(ctx, decoded, orientation, base.Recipe.MaxEdgePixels)
	if err != nil {
		return VisualPreviewProduct{}, err
	}
	if err := applyVisualPreviewColor(ctx, preview, colorSpace.transform()); err != nil {
		return VisualPreviewProduct{}, err
	}
	width, height := preview.Bounds().Dx(), preview.Bounds().Dy()
	matte, err := whitePhotoMatte(ctx, preview)
	if err != nil {
		return VisualPreviewProduct{}, err
	}
	var encoded bytes.Buffer
	if err := jpeg.Encode(&encoded, matte, &jpeg.Options{Quality: visualPreviewJPEGQuality}); err != nil {
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

func visualPreviewSourceMediaType(value string) string {
	mediaType, _, err := mime.ParseMediaType(value)
	if err != nil {
		return ""
	}
	return mediaType
}

func inspectVisualPreviewPNG(ctx context.Context, source io.ReadSeeker, sourceSize int64) (orientation int, colorSpace visualPreviewColorSpace, unsupportedMetadata, malformed bool, trailingOffset int64, err error) {
	orientation, colorSpace, unsupportedMetadata, _, malformed, trailingOffset, err = inspectVisualPreviewContainer(ctx, source, visualFormatPNG, sourceSize)
	return
}

// Trailing EXIF supplies only orientation; the verified source size bounds the scan.
func trailingVisualPreviewPNGOrientation(
	ctx context.Context, source io.ReadSeeker, sourceSize, offset int64,
) (int, error) {
	if _, err := source.Seek(offset, io.SeekStart); err != nil {
		return 0, err
	}
	for offset <= sourceSize-12 {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		var header [8]byte
		if _, err := io.ReadFull(source, header[:]); err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				return 1, nil
			}
			return 0, err
		}
		length := int64(binary.BigEndian.Uint32(header[:4]))
		chunkType := string(header[4:])
		if length > sourceSize-offset-12 || chunkType == "IEND" {
			return 1, nil
		}
		if chunkType == "eXIf" {
			return visualPreviewPNGEXIFOrientation(source, length)
		}
		if _, err := source.Seek(length+4, io.SeekCurrent); err != nil {
			return 0, err
		}
		offset += 12 + length
	}
	return 1, nil
}

func visualPreviewPNGEXIFOrientation(source io.Reader, length int64) (int, error) {
	if length > visualPreviewMaxEXIFBytes {
		return 1, nil
	}
	payload := make([]byte, length)
	if _, err := io.ReadFull(source, payload); err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return 1, nil
		}
		return 0, err
	}
	fields, _ := visualPreviewEXIF(payload)
	return fields.orientation, nil
}

func inspectVisualPreviewWebP(ctx context.Context, source io.ReadSeeker, sourceSize int64) (orientation int, colorSpace visualPreviewColorSpace, unsupportedMetadata, animated, malformed bool, err error) {
	orientation, colorSpace, unsupportedMetadata, animated, malformed, _, err = inspectVisualPreviewContainer(ctx, source, visualFormatWebP, sourceSize)
	return
}

func inspectVisualPreviewJPEG(ctx context.Context, source io.ReadSeeker) (orientation int, colorSpace visualPreviewColorSpace, malformed bool, err error) {
	orientation, colorSpace, _, _, malformed, _, err = inspectVisualPreviewContainer(ctx, source, visualFormatJPEG, -1) //nolint:dogsled // JPEG only consumes orientation, color, and validity.
	return
}

var errVisualInspectionDone = errors.New("preview inspection complete")

func inspectVisualPreviewContainer(ctx context.Context, source io.ReadSeeker, format string, size int64) (orientation int, colorSpace visualPreviewColorSpace, unsupportedMetadata, animated, malformed bool, trailingOffset int64, err error) {
	orientation = 1
	chunks := 0
	sawPNGEXIF := false
	webpICCFlag := false
	var jpegICC visualPreviewJPEGICC
	err = walkVisualContainer(ctx, source, format, size, func(kind string, reader io.Reader, length, offset int64) error {
		chunks++
		if format == visualFormatPNG && chunks > visualPreviewMaxPNGChunks {
			return errVisualContainer
		}
		if format == visualFormatPNG && kind == "IDAT" {
			if !sawPNGEXIF {
				trailingOffset = offset
			}
			return errVisualInspectionDone
		}
		if format == visualFormatPNG && kind == "IEND" {
			return errVisualContainer
		}
		if format == visualFormatPNG && kind == "iCCP" || format == visualFormatWebP && kind == "ICCP" && webpICCFlag {
			profile, err := readVisualPreviewICCChunk(reader, length)
			if err != nil {
				return err
			}
			if format == visualFormatPNG {
				profile = visualPreviewPNGICC(profile)
			}
			colorSpace.profile = profile
			return nil
		}
		if format == visualFormatWebP {
			switch kind {
			case "VP8X":
				if length != 10 {
					return errVisualContainer
				}
				var flags [1]byte
				if _, err := io.ReadFull(reader, flags[:]); err != nil {
					return err
				}
				colorProfile, animation := visualPreviewWebPFlags(flags[0])
				webpICCFlag = colorProfile
				animated = animated || animation
			case "ANIM", "ANMF":
				animated = true
			}
		}
		if format == visualFormatJPEG && (kind == "\xe1" || kind == "\xe2") || format == visualFormatPNG && kind == "eXIf" || format == visualFormatWebP && kind == "EXIF" {
			firstEXIF := !sawPNGEXIF
			if format == visualFormatPNG {
				sawPNGEXIF = true
			}
			if length > visualPreviewMaxEXIFBytes {
				unsupportedMetadata = true
				return nil
			}
			payload, err := io.ReadAll(reader)
			if err != nil {
				return err
			}
			if int64(len(payload)) != length {
				return io.ErrUnexpectedEOF
			}
			if format == visualFormatJPEG && kind == "\xe2" {
				if bytes.HasPrefix(payload, []byte("ICC_PROFILE\x00")) {
					jpegICC.add(payload)
				}
				return nil
			}
			if format == visualFormatJPEG && !bytes.HasPrefix(payload, []byte("Exif\x00\x00")) {
				return nil
			}
			if format != visualFormatPNG {
				payload = bytes.TrimPrefix(payload, []byte("Exif\x00\x00"))
			}
			if fields, found := visualPreviewEXIF(payload); found && (format != visualFormatPNG || firstEXIF) {
				orientation = fields.orientation
				colorSpace.adobeRGB = fields.adobeRGB
			}
		}
		return nil
	})
	if errors.Is(err, errVisualInspectionDone) {
		err = nil
	}
	if errors.Is(err, errVisualMetadataLimit) && format == visualFormatWebP {
		return 0, visualPreviewColorSpace{}, true, false, false, 0, nil
	}
	if errors.Is(err, errVisualContainer) || errors.Is(err, errVisualMetadataLimit) {
		return 0, visualPreviewColorSpace{}, false, false, true, 0, nil
	}
	if format == visualFormatJPEG {
		colorSpace.profile = jpegICC.profile()
	}
	return orientation, colorSpace, unsupportedMetadata, animated, malformed, trailingOffset, err
}

func decodeAndEncodeVisualPreview(ctx context.Context, source io.ReadSeeker, base document.VisualPreviewV1, format string, orientation int, colorSpace visualPreviewColorSpace) (VisualPreviewProduct, error) {
	pixels, err := decodeVisualPixels(ctx, source, format)
	if err == nil {
		return encodeVisualPreview(ctx, base, pixels.image, orientation, colorSpace)
	}
	return visualPreviewDecodeResult(base, format, pixels, err)
}

func visualPreviewDecodeResult(base document.VisualPreviewV1, format string, pixels visualPixels, err error) (VisualPreviewProduct, error) {
	name := map[string]string{visualFormatJPEG: "JPEG", visualFormatPNG: "PNG", visualFormatWebP: "WebP", "gif": "GIF"}[format]
	if errors.Is(err, errVisualColor) {
		base.State = document.VisualPreviewUnsupported
		base.Failure = &document.VisualPreviewFailureV1{Code: "unsupported_color_profile", Detail: "CMYK and YCCK JPEGs are unsupported by the built-in preview producer"}
		return VisualPreviewProduct{Preview: base}, nil
	}
	if errors.Is(err, errVisualDimensions) {
		return failedVisualPreview(base, "source_dimensions_exceed_limit", "the "+name+" dimensions exceed the built-in preview limit"), nil
	}
	if errors.Is(err, errVisualDecodeBounds) {
		return failedVisualPreview(base, "decode_failed", "the "+name+" dimensions changed during decoding"), nil
	}
	if pixels.seekFailed {
		return VisualPreviewProduct{}, sourceContentUnavailable(fmt.Errorf("seeking visual preview source: %w", err))
	}
	detail := "the verified " + name + " cannot be decoded"
	if pixels.header {
		detail = "the verified " + name + " header is malformed"
	}
	switch format {
	case visualFormatJPEG:
		return visualPreviewJPEGDecodeResult(base, detail, err)
	case visualFormatPNG:
		return visualPreviewPNGDecodeResult(base, detail, err)
	case "gif":
		return visualPreviewGIFDecodeResult(base, detail, pixels.readErr)
	default:
		return visualPreviewWebPDecodeResult(base, detail, pixels.readErr)
	}
}

type visualPreviewEXIFFields struct {
	orientation int
	colorSpace  int
	// adobeRGB reports the DCF Adobe RGB marker: ColorSpace Uncalibrated
	// (0xffff) with InteropIndex R03.
	adobeRGB bool
}

func visualPreviewEXIF(data []byte) (fields visualPreviewEXIFFields, found bool) {
	fields.orientation = 1
	reader, root, ok := sourceMetadataTIFFRoot(data)
	if !ok {
		return fields, false
	}
	if value, ok := exifUnsigned(reader, root[0x0112]); ok && value >= 1 && value <= 8 {
		fields.orientation = int(value)
	}
	if raw := root[0x8769]; len(raw) >= 4 {
		exif := reader.entries(reader.order.Uint32(raw))
		if value, ok := exifUnsigned(reader, exif[0xa001]); ok {
			fields.colorSpace = int(value)
		}
		if raw := exif[0xa005]; fields.colorSpace == 0xffff && len(raw) >= 4 {
			interop := reader.entries(reader.order.Uint32(raw))
			fields.adobeRGB = exifASCII(interop[0x0001]) == "R03"
		}
	}
	return fields, true
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

func visualPreviewPNGDecodeResult(
	base document.VisualPreviewV1, malformedDetail string, err error,
) (VisualPreviewProduct, error) {
	if _, ok := errors.AsType[png.UnsupportedError](err); ok {
		base.State = document.VisualPreviewUnsupported
		base.Failure = &document.VisualPreviewFailureV1{
			Code: "unsupported_png_feature", Detail: "the PNG uses a feature unavailable to the built-in decoder",
		}
		return VisualPreviewProduct{Preview: base}, nil
	}
	if _, ok := errors.AsType[png.FormatError](err); ok ||
		errors.Is(err, zlib.ErrHeader) || errors.Is(err, zlib.ErrDictionary) ||
		errors.Is(err, zlib.ErrChecksum) ||
		errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return failedVisualPreview(base, "decode_failed", malformedDetail), nil
	}
	if _, ok := errors.AsType[flate.CorruptInputError](err); ok {
		return failedVisualPreview(base, "decode_failed", malformedDetail), nil
	}
	return VisualPreviewProduct{}, sourceContentUnavailable(
		fmt.Errorf("reading visual preview PNG: %w", err))
}

func visualPreviewGIFDecodeResult(
	base document.VisualPreviewV1, malformedDetail string, readErr error,
) (VisualPreviewProduct, error) {
	if readErr != nil {
		return VisualPreviewProduct{}, sourceContentUnavailable(
			fmt.Errorf("reading visual preview GIF: %w", readErr))
	}
	return failedVisualPreview(base, "decode_failed", malformedDetail), nil
}

func visualPreviewWebPDecodeResult(
	base document.VisualPreviewV1, malformedDetail string, readErr error,
) (VisualPreviewProduct, error) {
	if readErr != nil {
		return VisualPreviewProduct{}, sourceContentUnavailable(
			fmt.Errorf("reading visual preview WebP: %w", readErr))
	}
	return failedVisualPreview(base, "decode_failed", malformedDetail), nil
}

type visualPreviewReadErrorRecorder struct {
	reader io.Reader
	err    error
}

func (reader *visualPreviewReadErrorRecorder) Read(target []byte) (int, error) {
	read, err := reader.reader.Read(target)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) && reader.err == nil {
		reader.err = err
	}
	return read, err
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

func applyVisualPreviewOrientation(ctx context.Context, source *image.NRGBA, orientation int) (*image.NRGBA, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if orientation == 1 {
		return source, nil
	}
	width, height := source.Bounds().Dx(), source.Bounds().Dy()
	outputWidth, outputHeight := visualPreviewOrientedDimensions(width, height, orientation)
	output := image.NewNRGBA(image.Rect(0, 0, outputWidth, outputHeight))
	for y := range outputHeight {
		for x := range outputWidth {
			if x%4096 == 0 {
				if err := ctx.Err(); err != nil {
					return nil, err
				}
			}
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
	return output, ctx.Err()
}

func failedVisualPreview(
	base document.VisualPreviewV1, code, detail string,
) VisualPreviewProduct {
	base.State = document.VisualPreviewFailed
	base.Failure = &document.VisualPreviewFailureV1{Code: code, Detail: detail}
	return VisualPreviewProduct{Preview: base}
}

func visualPreviewDimensionsAllowed(width, height int, limit int64) bool {
	return width > 0 && height > 0 &&
		int64(width) <= limit/int64(height)
}

func boundedVisualPreviewDimensionsForEdge(width, height, edge int) (int, int) {
	if width <= edge && height <= edge {
		return width, height
	}
	if width >= height {
		return edge,
			max(1, (height*edge+width/2)/width)
	}
	return max(1, (width*edge+height/2)/height),
		edge
}

func transformPhotoPixels(ctx context.Context, decoded image.Image, orientation, edge int) (*image.NRGBA, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	width, height := decoded.Bounds().Dx(), decoded.Bounds().Dy()
	orientedWidth, orientedHeight := visualPreviewOrientedDimensions(width, height, orientation)
	if edge == 0 {
		edge = max(orientedWidth, orientedHeight)
	}
	w, h := boundedVisualPreviewDimensionsForEdge(orientedWidth, orientedHeight, edge)
	if visualPreviewOrientationSwapsDimensions(orientation) {
		w, h = h, w
	}
	resized := image.NewNRGBA(image.Rect(0, 0, w, h))
	if w == width && h == height {
		if err := drawPhotoPixels(ctx, resized, decoded, draw.Src); err != nil {
			return nil, err
		}
	} else {
		if err := scalePhotoPixels(ctx, resized, decoded); err != nil {
			return nil, err
		}
	}
	return applyVisualPreviewOrientation(ctx, resized, orientation)
}

func whitePhotoMatte(ctx context.Context, pixels image.Image) (*image.RGBA, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	matte := image.NewRGBA(pixels.Bounds())
	if err := drawPhotoPixels(ctx, matte, image.NewUniform(color.White), draw.Src); err != nil {
		return nil, err
	}
	if err := drawPhotoPixels(ctx, matte, pixels, draw.Over); err != nil {
		return nil, err
	}
	return matte, nil
}

func drawPhotoPixels(ctx context.Context, dst draw.Image, src image.Image, op draw.Op) error {
	bounds := dst.Bounds()
	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		for x := bounds.Min.X; x < bounds.Max.X; x += 4096 {
			if err := ctx.Err(); err != nil {
				return err
			}
			r := image.Rect(x, y, min(x+4096, bounds.Max.X), y+1)
			draw.Draw(dst, r, src, src.Bounds().Min.Add(r.Min.Sub(bounds.Min)), op)
		}
	}
	return ctx.Err()
}

// Image callbacks cannot return errors. Unwind only our own cancellation
// sentinel so pixel work stops synchronously; unrelated panics still propagate.
func recoverPhotoCancellation(err *error) {
	if p := recover(); p != nil {
		if canceled, ok := p.(photoPixelsCanceled); ok {
			*err = canceled.err
		} else {
			panic(p)
		}
	}
}

func scalePhotoPixels(ctx context.Context, dst *image.NRGBA, src image.Image) (err error) {
	defer recoverPhotoCancellation(&err)
	kernel := *xdraw.CatmullRom
	kernel.At = func(t float64) float64 {
		if err := ctx.Err(); err != nil {
			panic(photoPixelsCanceled{err})
		}
		return xdraw.CatmullRom.At(t)
	}
	kernel.Scale(photoScaleDestination{dst, ctx}, dst.Bounds(), photoContextImage{src, ctx}, src.Bounds(), draw.Src, nil)
	return ctx.Err()
}

type photoPixelsCanceled struct{ err error }

type photoContextImage struct {
	image.Image

	ctx context.Context
}

func (p photoContextImage) At(x, y int) color.Color {
	if err := p.ctx.Err(); err != nil {
		panic(photoPixelsCanceled{err})
	}
	return p.Image.At(x, y)
}

func (p photoContextImage) RGBA64At(x, y int) color.RGBA64 {
	if err := p.ctx.Err(); err != nil {
		panic(photoPixelsCanceled{err})
	}
	if src, ok := p.Image.(image.RGBA64Image); ok {
		return src.RGBA64At(x, y)
	}
	converted, _ := color.RGBA64Model.Convert(p.Image.At(x, y)).(color.RGBA64)
	return converted
}

func (p photoContextImage) Opaque() bool {
	bounds := p.Bounds()
	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		for x := bounds.Min.X; x < bounds.Max.X; x++ {
			if p.RGBA64At(x, y).A != 0xffff {
				return false
			}
		}
	}
	return true
}

type photoScaleDestination struct {
	*image.NRGBA

	ctx context.Context
}

func (p photoScaleDestination) SetRGBA64(x, y int, c color.RGBA64) {
	if err := p.ctx.Err(); err != nil {
		panic(photoPixelsCanceled{err})
	}
	p.NRGBA.SetRGBA64(x, y, c)
}

func decodeGIFCanvas(source io.Reader, config image.Config) (image.Image, error) {
	decoded, err := gif.Decode(source)
	if err != nil {
		return nil, fmt.Errorf("decoding GIF canvas: %w", err)
	}
	canvas := image.NewNRGBA(image.Rect(0, 0, config.Width, config.Height))
	draw.Draw(canvas, decoded.Bounds(), decoded, decoded.Bounds().Min, draw.Src)
	return canvas, nil
}
