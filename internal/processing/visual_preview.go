package processing

import (
	"bufio"
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
	_ "golang.org/x/image/webp"

	"go.kenn.io/docbank/document"
)

const (
	visualPreviewProcessorDescriptor = document.VisualPreviewProcessorDescriptor
	visualPreviewMaxEdgePixels       = document.VisualPreviewMaxEdgePixels
	visualPreviewMaxSourcePixels     = 100_000_000
	visualPreviewJPEGQuality         = document.VisualPreviewJPEGQuality
	visualPreviewMaxJPEGSegments     = 1024
	visualPreviewMaxWebPChunks       = 1024
	visualPreviewMaxEXIFBytes        = 1 << 20
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
	case "jpeg", "png", "gif", "webp":
		return produceVisualPreviewImage(ctx, source, target.Size, visualPreviewFormat(mediaType), base, 0)
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

func produceVisualPreviewJPEGWithOrientation(ctx context.Context, source io.ReadSeeker, base document.VisualPreviewV1, orientation int) (VisualPreviewProduct, error) {
	return produceVisualPreviewImage(ctx, source, -1, "jpeg", base, orientation)
}

func produceVisualPreviewImage(ctx context.Context, source io.ReadSeeker, size int64, format string, base document.VisualPreviewV1, containerOrientation int) (VisualPreviewProduct, error) {
	orientation := 1
	if format != "gif" {
		value, color, metadata, animated, malformed, err := inspectVisualPreviewContainer(ctx, source, size, format)
		if err != nil {
			return VisualPreviewProduct{}, sourceContentUnavailable(fmt.Errorf("inspecting visual preview %s: %w", format, err))
		}
		if malformed {
			return failedVisualPreview(base, "decode_failed", "the verified image container is malformed"), nil
		}
		var failure *document.VisualPreviewFailureV1
		switch {
		case animated:
			failure = &document.VisualPreviewFailureV1{Code: "unsupported_webp_animation", Detail: "the built-in preview producer requires a still WebP original"}
		case color:
			failure = &document.VisualPreviewFailureV1{Code: "unsupported_color_profile", Detail: "the built-in preview producer requires sRGB originals"}
		case metadata:
			failure = &document.VisualPreviewFailureV1{Code: "unsupported_" + format + "_metadata", Detail: "the image metadata exceeds the built-in preview limit"}
		}
		if failure != nil {
			base.State, base.Failure = document.VisualPreviewUnsupported, failure
			return VisualPreviewProduct{Preview: base}, nil
		}
		orientation = value
	}
	if containerOrientation >= 1 && containerOrientation <= 8 {
		orientation = containerOrientation
	}
	return produceDecodedVisualPreview(ctx, source, format, base, orientation)
}

var errVisualDimensions = errors.New("source dimensions exceed the built-in limit")
var errVisualColor = errors.New("unsupported JPEG color model")
var errVisualFormat = errors.New("unexpected image format")
var errVisualDecodeBounds = errors.New("image dimensions changed during decoding")

type visualPreviewPixels struct {
	image   image.Image
	config  image.Config
	readErr error
}

func decodeVisualPreviewPixels(ctx context.Context, source io.ReadSeeker, format string, admit func(image.Config) error) (result visualPreviewPixels, err error) {
	if _, err = source.Seek(0, io.SeekStart); err != nil {
		result.readErr = err
		return
	}
	reader := &visualPreviewReadErrorRecorder{reader: visualPreviewContextReader{ctx, source}}
	defer func() {
		if reader.err != nil {
			result.readErr = reader.err
		}
	}()
	var actual string
	result.config, actual, err = image.DecodeConfig(reader)
	if err != nil {
		return
	}
	if actual != format {
		return result, errVisualFormat
	}
	if !visualPreviewDimensionsAllowed(result.config.Width, result.config.Height) {
		return result, errVisualDimensions
	}
	if format == "jpeg" && !visualPreviewJPEGColorModelSupported(result.config.ColorModel) {
		return result, errVisualColor
	}
	if admit != nil {
		if err = admit(result.config); err != nil {
			return
		}
	}
	if _, err = source.Seek(0, io.SeekStart); err != nil {
		result.readErr = err
		return
	}
	if format == "gif" {
		result.image, err = decodeGIFCanvas(reader, result.config)
	} else {
		result.image, _, err = image.Decode(reader)
	}
	if err != nil {
		return
	}
	if result.image.Bounds().Dx() != result.config.Width || result.image.Bounds().Dy() != result.config.Height {
		return result, errVisualDecodeBounds
	}
	return result, ctx.Err()
}

func produceDecodedVisualPreview(ctx context.Context, source io.ReadSeeker, format string, base document.VisualPreviewV1, orientation int) (VisualPreviewProduct, error) {
	pixels, err := decodeVisualPreviewPixels(ctx, source, format, nil)
	if err != nil {
		if pixels.readErr != nil {
			return VisualPreviewProduct{}, sourceContentUnavailable(fmt.Errorf("reading visual preview %s: %w", format, pixels.readErr))
		}
		if errors.Is(err, errVisualDimensions) {
			return failedVisualPreview(base, "source_dimensions_exceed_limit", err.Error()), nil
		}
		if errors.Is(err, errVisualColor) {
			base.State = document.VisualPreviewUnsupported
			base.Failure = &document.VisualPreviewFailureV1{Code: "unsupported_color_profile", Detail: "the built-in preview producer requires sRGB JPEG originals"}
			return VisualPreviewProduct{Preview: base}, nil
		}
		if errors.Is(err, errVisualFormat) || errors.Is(err, errVisualDecodeBounds) {
			return failedVisualPreview(base, "decode_failed", err.Error()), nil
		}
		detail := "the verified " + format + " cannot be decoded"
		switch format {
		case "jpeg":
			return visualPreviewJPEGDecodeResult(base, detail, err)
		case "png":
			return visualPreviewPNGDecodeResult(base, detail, err)
		default:
			return failedVisualPreview(base, "decode_failed", detail), nil
		}
	}
	return encodeVisualPreview(base, pixels.image, pixels.config.Width, pixels.config.Height, orientation)
}

func encodeVisualPreview(
	base document.VisualPreviewV1,
	decoded image.Image,
	sourceWidth, sourceHeight, orientation int,
) (VisualPreviewProduct, error) {
	preview := transformPhotoPixels(decoded, orientation, base.Recipe.MaxEdgePixels)
	width, height := preview.Bounds().Dx(), preview.Bounds().Dy()
	matte := whitePhotoMatte(preview)

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

type visualPreviewContextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r visualPreviewContextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
}

var errVisualContainer = errors.New("malformed image container")
var errVisualMetadataLimit = errors.New("image metadata exceeds inspection limit")

func walkVisualPreviewContainer(ctx context.Context, source io.ReadSeeker, format string, size int64, visit func(string, io.Reader, int64) error) (err error) {
	defer func() {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			err = errors.Join(errVisualContainer, err)
		}
	}()
	if size < 0 {
		size, err = source.Seek(0, io.SeekEnd)
		if err != nil {
			return err
		}
	}
	if _, err := source.Seek(0, io.SeekStart); err != nil {
		return err
	}
	r := bufio.NewReader(visualPreviewContextReader{ctx, io.LimitReader(source, size)})
	headerSize := map[string]int{"jpeg": 2, "png": 8, "webp": 12}[format]
	header := make([]byte, headerSize)
	if _, err := io.ReadFull(r, header); err != nil {
		return err
	}
	if format == "jpeg" && !bytes.Equal(header, []byte{0xff, 0xd8}) || format == "png" && string(header) != "\x89PNG\r\n\x1a\n" || format == "webp" && (string(header[:4]) != "RIFF" || string(header[8:]) != "WEBP" || uint64(binary.LittleEndian.Uint32(header[4:]))+8 != uint64(size)) {
		return errVisualContainer
	}
	offset := int64(headerSize)
	for count := 0; ; count++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		if format == "webp" && offset == size {
			return nil
		}
		if format == "jpeg" && count >= visualPreviewMaxJPEGSegments {
			return errVisualContainer
		}
		if format == "webp" && count >= visualPreviewMaxWebPChunks {
			return errVisualMetadataLimit
		}
		var kind string
		var length, overhead, padding int64
		switch format {
		case "jpeg":
			marker, err := readVisualPreviewJPEGMarker(r)
			if err != nil {
				return err
			}
			if marker == 0xda || marker == 0xd9 {
				return nil
			}
			if marker == 0x01 || marker >= 0xd0 && marker <= 0xd7 {
				continue
			}
			var h [2]byte
			if _, err := io.ReadFull(r, h[:]); err != nil {
				return err
			}
			length = int64(binary.BigEndian.Uint16(h[:])) - 2
			if length < 0 {
				return errVisualContainer
			}
			kind = string([]byte{marker})
		case "png", "webp":
			overhead = 8
			if format == "png" {
				overhead = 12
			}
			if offset > size-overhead {
				return errVisualContainer
			}
			var h [8]byte
			if _, err := io.ReadFull(r, h[:]); err != nil {
				return err
			}
			if format == "png" {
				length, kind, padding = int64(binary.BigEndian.Uint32(h[:4])), string(h[4:]), 4
			} else {
				length, kind = int64(binary.LittleEndian.Uint32(h[4:])), string(h[:4])
				padding = length % 2
			}
			if length > size-offset-overhead || format == "webp" && length+padding > size-offset-8 {
				return errVisualContainer
			}
		}
		payload := &io.LimitedReader{R: r, N: length}
		if err := visit(kind, payload, length); err != nil {
			return err
		}
		if _, err := io.Copy(io.Discard, payload); err != nil {
			return err
		}
		if payload.N != 0 {
			return io.ErrUnexpectedEOF
		}
		if _, err := io.CopyN(io.Discard, r, padding); err != nil {
			return err
		}
		offset += overhead + length
		if format == "webp" {
			offset += padding
		}
		if format == "png" && kind == "IEND" {
			if length != 0 {
				return errVisualContainer
			}
			return nil
		}
	}
}

func inspectVisualPreviewContainer(ctx context.Context, source io.ReadSeeker, size int64, format string) (orientation int, unsupportedColor, unsupportedMetadata, animated, malformed bool, err error) {
	orientation = 1
	err = walkVisualPreviewContainer(ctx, source, format, size, func(kind string, r io.Reader, n int64) error {
		if format == "jpeg" && kind != "\xe1" && kind != "\xe2" {
			return nil
		}
		if format == "png" && kind == "iCCP" || format == "webp" && kind == "ICCP" {
			unsupportedColor = true
			return nil
		}
		if format == "webp" && (kind == "ANIM" || kind == "ANMF") {
			animated = true
			return nil
		}
		if format == "webp" && kind == "VP8X" {
			if n != 10 {
				return errVisualContainer
			}
			var flags [1]byte
			if _, err := io.ReadFull(r, flags[:]); err != nil {
				return err
			}
			animated = animated || flags[0]&visualPreviewWebPAnimation != 0
			unsupportedColor = unsupportedColor || flags[0]&visualPreviewWebPICCProfile != 0
			return nil
		}
		if format != "jpeg" && kind != "eXIf" && kind != "EXIF" {
			return nil
		}
		if n > visualPreviewMaxEXIFBytes {
			unsupportedMetadata = true
			return nil
		}
		payload, err := io.ReadAll(r)
		if err != nil {
			return err
		}
		if format == "jpeg" {
			if kind == "\xe2" && bytes.HasPrefix(payload, []byte("ICC_PROFILE\x00")) {
				unsupportedColor = true
				return nil
			}
			if kind != "\xe1" || !bytes.HasPrefix(payload, []byte("Exif\x00\x00")) {
				return nil
			}
		}
		if value, colorSpace, found := visualPreviewEXIF(bytes.TrimPrefix(payload, []byte("Exif\x00\x00"))); found {
			orientation = value
			unsupportedColor = unsupportedColor || colorSpace != 0 && colorSpace != 1
		}
		return nil
	})
	if errors.Is(err, errVisualContainer) {
		malformed, err = true, nil
	}
	if errors.Is(err, errVisualMetadataLimit) {
		unsupportedMetadata, err = true, nil
	}
	return
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

func failedVisualPreview(
	base document.VisualPreviewV1, code, detail string,
) VisualPreviewProduct {
	base.State = document.VisualPreviewFailed
	base.Failure = &document.VisualPreviewFailureV1{Code: code, Detail: detail}
	return VisualPreviewProduct{Preview: base}
}

func visualPreviewDimensionsAllowed(width, height int) bool {
	return width > 0 && height > 0 &&
		int64(width) <= visualPreviewMaxSourcePixels/int64(height)
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

func transformPhotoPixels(decoded image.Image, orientation, edge int) *image.NRGBA {
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
		draw.Draw(resized, resized.Bounds(), decoded, decoded.Bounds().Min, draw.Src)
	} else {
		xdraw.CatmullRom.Scale(resized, resized.Bounds(), decoded, decoded.Bounds(), draw.Src, nil)
	}
	return applyVisualPreviewOrientation(resized, orientation)
}
func whitePhotoMatte(pixels image.Image) *image.RGBA {
	matte := image.NewRGBA(pixels.Bounds())
	draw.Draw(matte, matte.Bounds(), image.NewUniform(color.White), image.Point{}, draw.Src)
	draw.Draw(matte, matte.Bounds(), pixels, pixels.Bounds().Min, draw.Over)
	return matte
}

func decodeGIFCanvas(source io.Reader, config image.Config) (image.Image, error) {
	decoded, err := gif.Decode(source)
	if err != nil {
		return nil, err
	}
	canvas := image.NewNRGBA(image.Rect(0, 0, config.Width, config.Height))
	draw.Draw(canvas, decoded.Bounds(), decoded, decoded.Bounds().Min, draw.Src)
	return canvas, nil
}
