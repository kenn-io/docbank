package processing

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"hash/crc32"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"io"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/media/mediatest"
)

func TestProduceVisualPreviewAppliesEXIFOrientation(t *testing.T) {
	tiff := syntheticTIFF(42,
		[]syntheticTIFFEntry{tiffShort(0x0112, 6)},
		[]syntheticTIFFEntry{tiffShort(0xa001, 1)},
	)
	source := syntheticJPEGSegment(t, mediatest.JPEG(3, 2, color.White), 0xe1,
		append([]byte("Exif\x00\x00"), tiff...))
	digest := sha256.Sum256(source)

	product, err := ProduceVisualPreview(t.Context(), bytes.NewReader(source), VisualPreviewTarget{
		SourceSHA256: hex.EncodeToString(digest[:]), Size: int64(len(source)), MediaType: "image/jpeg",
	})
	require.NoError(t, err)
	assert.Equal(t, document.VisualPreviewReady, product.Preview.State)
	require.NotNil(t, product.Preview.Output)
	assert.Equal(t, 2, product.Preview.Output.Width)
	assert.Equal(t, 3, product.Preview.Output.Height)
	decoded, err := jpeg.Decode(bytes.NewReader(product.Output))
	require.NoError(t, err)
	assert.Equal(t, 2, decoded.Bounds().Dx())
	assert.Equal(t, 3, decoded.Bounds().Dy())
}

func TestVisualPreviewPreservesHighDensityDetail(t *testing.T) {
	recipe := CurrentVisualPreviewRecipe()
	assert.Equal(t, 4096, recipe.MaxEdgePixels)
	width, height := boundedVisualPreviewDimensions(6000, 4000)
	assert.Equal(t, 4096, width)
	assert.Equal(t, 2731, height)
}

func TestProduceVisualPreviewRejectsEmbeddedICCProfile(t *testing.T) {
	tiff := syntheticTIFF(42, nil, []syntheticTIFFEntry{tiffShort(0xa001, 1)})
	source := syntheticJPEGSegment(t, mediatest.JPEG(3, 2, color.White), 0xe1,
		append([]byte("Exif\x00\x00"), tiff...))
	source = syntheticJPEGSegment(t, source, 0xe2,
		[]byte("ICC_PROFILE\x00\x01\x01profile"))
	digest := sha256.Sum256(source)

	product, err := ProduceVisualPreview(t.Context(), bytes.NewReader(source), VisualPreviewTarget{
		SourceSHA256: hex.EncodeToString(digest[:]), Size: int64(len(source)), MediaType: "image/jpeg",
	})
	require.NoError(t, err)
	assert.Equal(t, document.VisualPreviewUnsupported, product.Preview.State)
	require.NotNil(t, product.Preview.Failure)
	assert.Equal(t, "unsupported_color_profile", product.Preview.Failure.Code)
	assert.Empty(t, product.Output)
}

func TestProduceVisualPreviewAcceptsJPEGMediaTypeParameters(t *testing.T) {
	source := mediatest.JPEG(3, 2, color.White)
	digest := sha256.Sum256(source)

	product, err := ProduceVisualPreview(t.Context(), bytes.NewReader(source), VisualPreviewTarget{
		SourceSHA256: hex.EncodeToString(digest[:]), Size: int64(len(source)),
		MediaType: "IMAGE/JPEG; charset=utf-8",
	})
	require.NoError(t, err)
	assert.Equal(t, document.VisualPreviewReady, product.Preview.State)
	require.NotNil(t, product.Preview.Output)
	assert.Equal(t, "image/jpeg", product.Preview.Output.MediaType)
}

func TestProduceVisualPreviewAcceptsPNGAndFlattensTransparency(t *testing.T) {
	canvas := image.NewNRGBA(image.Rect(0, 0, 64, 32))
	for y := range 32 {
		for x := range 64 {
			if x < 32 {
				canvas.SetNRGBA(x, y, color.NRGBA{R: 255})
			} else {
				canvas.SetNRGBA(x, y, color.NRGBA{A: 255})
			}
		}
	}
	var encoded bytes.Buffer
	require.NoError(t, png.Encode(&encoded, canvas))
	source := encoded.Bytes()
	digest := sha256.Sum256(source)

	product, err := ProduceVisualPreview(t.Context(), bytes.NewReader(source), VisualPreviewTarget{
		SourceSHA256: hex.EncodeToString(digest[:]), Size: int64(len(source)),
		MediaType: "IMAGE/PNG; charset=utf-8",
	})
	require.NoError(t, err)
	assert.Equal(t, document.VisualPreviewReady, product.Preview.State)
	require.NotNil(t, product.Preview.Output)
	assert.Equal(t, 64, product.Preview.Output.Width)
	assert.Equal(t, 32, product.Preview.Output.Height)
	preview, err := jpeg.Decode(bytes.NewReader(product.Output))
	require.NoError(t, err)
	transparentRed, transparentGreen, transparentBlue, _ := preview.At(8, 16).RGBA()
	opaqueRed, opaqueGreen, opaqueBlue, _ := preview.At(56, 16).RGBA()
	assert.Greater(t, transparentRed, uint32(0xf000))
	assert.Greater(t, transparentGreen, uint32(0xf000))
	assert.Greater(t, transparentBlue, uint32(0xf000))
	assert.Less(t, opaqueRed, uint32(0x1000))
	assert.Less(t, opaqueGreen, uint32(0x1000))
	assert.Less(t, opaqueBlue, uint32(0x1000))
}

func TestProduceVisualPreviewAppliesPNGEXIFOrientation(t *testing.T) {
	source := syntheticPNGChunk(t, mediatest.PNG(3, 2, color.White), "eXIf",
		syntheticTIFF(42, []syntheticTIFFEntry{tiffShort(0x0112, 6)}, nil))
	digest := sha256.Sum256(source)

	product, err := ProduceVisualPreview(t.Context(), bytes.NewReader(source), VisualPreviewTarget{
		SourceSHA256: hex.EncodeToString(digest[:]), Size: int64(len(source)), MediaType: "image/png",
	})
	require.NoError(t, err)
	assert.Equal(t, document.VisualPreviewReady, product.Preview.State)
	require.NotNil(t, product.Preview.Output)
	assert.Equal(t, 2, product.Preview.Output.Width)
	assert.Equal(t, 3, product.Preview.Output.Height)
}

func TestProduceVisualPreviewUsesGIFPrimaryFrame(t *testing.T) {
	palette := color.Palette{color.Black, color.White}
	primary := image.NewPaletted(image.Rect(16, 8, 48, 24), palette)
	later := image.NewPaletted(image.Rect(0, 0, 64, 32), palette)
	for index := range later.Pix {
		later.Pix[index] = 1
	}
	var encoded bytes.Buffer
	require.NoError(t, gif.EncodeAll(&encoded, &gif.GIF{
		Image: []*image.Paletted{primary, later}, Delay: []int{10, 10},
		Config: image.Config{ColorModel: palette, Width: 64, Height: 32},
	}))
	source := encoded.Bytes()
	digest := sha256.Sum256(source)

	product, err := ProduceVisualPreview(t.Context(), bytes.NewReader(source), VisualPreviewTarget{
		SourceSHA256: hex.EncodeToString(digest[:]), Size: int64(len(source)),
		MediaType: "IMAGE/GIF; charset=utf-8",
	})
	require.NoError(t, err)
	assert.Equal(t, document.VisualPreviewReady, product.Preview.State)
	require.NotNil(t, product.Preview.Output)
	assert.Equal(t, 64, product.Preview.Output.Width)
	assert.Equal(t, 32, product.Preview.Output.Height)
	preview, err := jpeg.Decode(bytes.NewReader(product.Output))
	require.NoError(t, err)
	primaryRed, primaryGreen, primaryBlue, _ := preview.At(32, 16).RGBA()
	assert.Less(t, primaryRed, uint32(0x1000))
	assert.Less(t, primaryGreen, uint32(0x1000))
	assert.Less(t, primaryBlue, uint32(0x1000))
	canvasRed, canvasGreen, canvasBlue, _ := preview.At(8, 4).RGBA()
	assert.Greater(t, canvasRed, uint32(0xf000))
	assert.Greater(t, canvasGreen, uint32(0xf000))
	assert.Greater(t, canvasBlue, uint32(0xf000))
}

func TestProduceVisualPreviewAcceptsWebP(t *testing.T) {
	source := mustDecodeWebP(t)
	digest := sha256.Sum256(source)

	product, err := ProduceVisualPreview(t.Context(), bytes.NewReader(source), VisualPreviewTarget{
		SourceSHA256: hex.EncodeToString(digest[:]), Size: int64(len(source)),
		MediaType: "IMAGE/WEBP; charset=utf-8",
	})
	require.NoError(t, err)
	assert.Equal(t, document.VisualPreviewReady, product.Preview.State)
	require.NotNil(t, product.Preview.Output)
	assert.Equal(t, 75, product.Preview.Output.Width)
	assert.Equal(t, 100, product.Preview.Output.Height)
}

func TestProduceVisualPreviewAcceptsTIFFCameraRAW(t *testing.T) {
	preview := mediatest.JPEG(3, 2, color.White)
	source := syntheticRAWPreviewTIFF(preview, 6)
	digest := sha256.Sum256(source)

	for _, mediaType := range []string{
		"image/x-sony-arw",
		"image/x-adobe-dng",
		"image/x-canon-cr2",
		"image/x-nikon-nef",
	} {
		t.Run(mediaType, func(t *testing.T) {
			product, err := ProduceVisualPreview(t.Context(), bytes.NewReader(source), VisualPreviewTarget{
				SourceSHA256: hex.EncodeToString(digest[:]), Size: int64(len(source)), MediaType: mediaType,
			})
			require.NoError(t, err)
			assert.Equal(t, document.VisualPreviewReady, product.Preview.State)
			require.NotNil(t, product.Preview.Output)
			assert.Equal(t, 2, product.Preview.Output.Width)
			assert.Equal(t, 3, product.Preview.Output.Height)
		})
	}
}

func TestProduceVisualPreviewAcceptsRAF(t *testing.T) {
	source := syntheticRAF()
	digest := sha256.Sum256(source)

	product, err := ProduceVisualPreview(t.Context(), bytes.NewReader(source), VisualPreviewTarget{
		SourceSHA256: hex.EncodeToString(digest[:]), Size: int64(len(source)), MediaType: "image/x-fuji-raf",
	})
	require.NoError(t, err)
	assert.Equal(t, document.VisualPreviewReady, product.Preview.State)
	require.NotNil(t, product.Preview.Output)
	assert.Equal(t, 30, product.Preview.Output.Width)
	assert.Equal(t, 40, product.Preview.Output.Height)
}

func TestProduceVisualPreviewRecordsMissingCameraRAWPreview(t *testing.T) {
	source := syntheticTIFFRoot([]syntheticTIFFEntry{tiffShort(0x0112, 1)})
	digest := sha256.Sum256(source)

	product, err := ProduceVisualPreview(t.Context(), bytes.NewReader(source), VisualPreviewTarget{
		SourceSHA256: hex.EncodeToString(digest[:]), Size: int64(len(source)), MediaType: "image/x-nikon-nef",
	})
	require.NoError(t, err)
	assert.Equal(t, document.VisualPreviewUnsupported, product.Preview.State)
	require.NotNil(t, product.Preview.Failure)
	assert.Equal(t, "embedded_preview_unavailable", product.Preview.Failure.Code)
}

func TestProduceVisualPreviewRecordsInvalidCameraRAWPreviewRange(t *testing.T) {
	source := syntheticRAWPreviewTIFF(mediatest.JPEG(3, 2, color.White), 1)
	previewIFD := int(binary.LittleEndian.Uint32(source[22:26]))
	binary.LittleEndian.PutUint32(source[previewIFD+10:previewIFD+14], uint32(len(source)+1))
	digest := sha256.Sum256(source)

	product, err := ProduceVisualPreview(t.Context(), bytes.NewReader(source), VisualPreviewTarget{
		SourceSHA256: hex.EncodeToString(digest[:]), Size: int64(len(source)), MediaType: "image/x-nikon-nef",
	})
	require.NoError(t, err)
	assert.Equal(t, document.VisualPreviewFailed, product.Preview.State)
	require.NotNil(t, product.Preview.Failure)
	assert.Equal(t, "decode_failed", product.Preview.Failure.Code)
}

func TestProduceVisualPreviewAppliesWebPEXIFOrientation(t *testing.T) {
	source := mustDecodeWebP(t)
	source = syntheticExtendedWebP(t, source, 75, 100, visualPreviewWebPEXIF, "EXIF",
		syntheticTIFF(42, []syntheticTIFFEntry{tiffShort(0x0112, 6)}, nil))
	digest := sha256.Sum256(source)

	product, err := ProduceVisualPreview(t.Context(), bytes.NewReader(source), VisualPreviewTarget{
		SourceSHA256: hex.EncodeToString(digest[:]), Size: int64(len(source)), MediaType: "image/webp",
	})
	require.NoError(t, err)
	assert.Equal(t, document.VisualPreviewReady, product.Preview.State)
	require.NotNil(t, product.Preview.Output)
	assert.Equal(t, 100, product.Preview.Output.Width)
	assert.Equal(t, 75, product.Preview.Output.Height)
}

func TestProduceVisualPreviewRejectsWebPICCProfile(t *testing.T) {
	source, err := base64.StdEncoding.DecodeString(
		"UklGRiIAAABXRUJQVlA4IBYAAAAwAQCdASoBAAEADsD+JaQAA3AAAAAA",
	)
	require.NoError(t, err)
	source = syntheticExtendedWebP(t, source, 1, 1, visualPreviewWebPICCProfile, "ICCP", []byte("profile"))
	digest := sha256.Sum256(source)

	product, err := ProduceVisualPreview(t.Context(), bytes.NewReader(source), VisualPreviewTarget{
		SourceSHA256: hex.EncodeToString(digest[:]), Size: int64(len(source)), MediaType: "image/webp",
	})
	require.NoError(t, err)
	assert.Equal(t, document.VisualPreviewUnsupported, product.Preview.State)
	require.NotNil(t, product.Preview.Failure)
	assert.Equal(t, "unsupported_color_profile", product.Preview.Failure.Code)
}

func TestProduceVisualPreviewRejectsPNGICCProfile(t *testing.T) {
	source := syntheticPNGChunk(t, mediatest.PNG(3, 2, color.White), "iCCP", []byte("profile"))
	digest := sha256.Sum256(source)

	product, err := ProduceVisualPreview(t.Context(), bytes.NewReader(source), VisualPreviewTarget{
		SourceSHA256: hex.EncodeToString(digest[:]), Size: int64(len(source)), MediaType: "image/png",
	})
	require.NoError(t, err)
	assert.Equal(t, document.VisualPreviewUnsupported, product.Preview.State)
	require.NotNil(t, product.Preview.Failure)
	assert.Equal(t, "unsupported_color_profile", product.Preview.Failure.Code)
}

func TestProduceVisualPreviewRecordsTruncatedPNGFailure(t *testing.T) {
	complete := mediatest.PNG(64, 48, color.White)
	source := complete[:len(complete)-1]
	digest := sha256.Sum256(source)

	product, err := ProduceVisualPreview(t.Context(), bytes.NewReader(source), VisualPreviewTarget{
		SourceSHA256: hex.EncodeToString(digest[:]), Size: int64(len(source)), MediaType: "image/png",
	})
	require.NoError(t, err)
	assert.Equal(t, document.VisualPreviewFailed, product.Preview.State)
	require.NotNil(t, product.Preview.Failure)
	assert.Equal(t, "decode_failed", product.Preview.Failure.Code)
	assert.Empty(t, product.Output)
}

func TestProduceVisualPreviewRecordsCorruptPNGCompressionFailure(t *testing.T) {
	source := mediatest.PNG(4, 3, color.White)
	corrupted := false
	for offset := 8; offset+12 <= len(source); {
		length := int(binary.BigEndian.Uint32(source[offset : offset+4]))
		end := offset + 12 + length
		require.LessOrEqual(t, end, len(source))
		if string(source[offset+4:offset+8]) == "IDAT" {
			require.GreaterOrEqual(t, length, 2)
			source[offset+8], source[offset+9] = 0, 0
			binary.BigEndian.PutUint32(source[end-4:end], crc32.ChecksumIEEE(source[offset+4:end-4]))
			corrupted = true
			break
		}
		offset = end
	}
	require.True(t, corrupted)
	digest := sha256.Sum256(source)

	product, err := ProduceVisualPreview(t.Context(), bytes.NewReader(source), VisualPreviewTarget{
		SourceSHA256: hex.EncodeToString(digest[:]), Size: int64(len(source)), MediaType: "image/png",
	})
	require.NoError(t, err)
	assert.Equal(t, document.VisualPreviewFailed, product.Preview.State)
	require.NotNil(t, product.Preview.Failure)
	assert.Equal(t, "decode_failed", product.Preview.Failure.Code)
}

func TestProduceVisualPreviewRejectsOversizedPNGDimensionsBeforeDecode(t *testing.T) {
	source := mediatest.PNG(1, 1, color.White)
	binary.BigEndian.PutUint32(source[16:20], 10001)
	binary.BigEndian.PutUint32(source[20:24], 10001)
	binary.BigEndian.PutUint32(source[29:33], crc32.ChecksumIEEE(source[12:29]))
	digest := sha256.Sum256(source)

	product, err := ProduceVisualPreview(t.Context(), bytes.NewReader(source), VisualPreviewTarget{
		SourceSHA256: hex.EncodeToString(digest[:]), Size: int64(len(source)), MediaType: "image/png",
	})
	require.NoError(t, err)
	assert.Equal(t, document.VisualPreviewFailed, product.Preview.State)
	require.NotNil(t, product.Preview.Failure)
	assert.Equal(t, "source_dimensions_exceed_limit", product.Preview.Failure.Code)
}

func TestProduceVisualPreviewRecordsMalformedJPEGFailure(t *testing.T) {
	source := []byte{0xff, 0xd8, 0xff, 0xd9}
	digest := sha256.Sum256(source)

	product, err := ProduceVisualPreview(t.Context(), bytes.NewReader(source), VisualPreviewTarget{
		SourceSHA256: hex.EncodeToString(digest[:]), Size: int64(len(source)), MediaType: "image/jpeg",
	})
	require.NoError(t, err)
	assert.Equal(t, document.VisualPreviewFailed, product.Preview.State)
	require.NotNil(t, product.Preview.Failure)
	assert.Equal(t, "decode_failed", product.Preview.Failure.Code)
	assert.Empty(t, product.Output)
}

func TestProduceVisualPreviewRecordsTruncatedJPEGFailure(t *testing.T) {
	complete := mediatest.JPEG(64, 48, color.RGBA{R: 220, G: 40, B: 20, A: 255})
	source := complete[:len(complete)-1]
	digest := sha256.Sum256(source)

	product, err := ProduceVisualPreview(t.Context(), bytes.NewReader(source), VisualPreviewTarget{
		SourceSHA256: hex.EncodeToString(digest[:]), Size: int64(len(source)), MediaType: "image/jpeg",
	})
	require.NoError(t, err)
	assert.Equal(t, document.VisualPreviewFailed, product.Preview.State)
	require.NotNil(t, product.Preview.Failure)
	assert.Equal(t, "decode_failed", product.Preview.Failure.Code)
	assert.Empty(t, product.Output)
}

func TestProduceVisualPreviewRecordsMalformedGIFFailure(t *testing.T) {
	source := []byte("GIF89a")
	digest := sha256.Sum256(source)

	product, err := ProduceVisualPreview(t.Context(), bytes.NewReader(source), VisualPreviewTarget{
		SourceSHA256: hex.EncodeToString(digest[:]), Size: int64(len(source)), MediaType: "image/gif",
	})
	require.NoError(t, err)
	assert.Equal(t, document.VisualPreviewFailed, product.Preview.State)
	require.NotNil(t, product.Preview.Failure)
	assert.Equal(t, "decode_failed", product.Preview.Failure.Code)
	assert.Empty(t, product.Output)
}

func TestProduceVisualPreviewRecordsMalformedWebPFailure(t *testing.T) {
	source := []byte("RIFF\x04\x00\x00\x00WEBP")
	digest := sha256.Sum256(source)

	product, err := ProduceVisualPreview(t.Context(), bytes.NewReader(source), VisualPreviewTarget{
		SourceSHA256: hex.EncodeToString(digest[:]), Size: int64(len(source)), MediaType: "image/webp",
	})
	require.NoError(t, err)
	assert.Equal(t, document.VisualPreviewFailed, product.Preview.State)
	require.NotNil(t, product.Preview.Failure)
	assert.Equal(t, "decode_failed", product.Preview.Failure.Code)
}

func TestVisualPreviewJPEGColorPolicyRejectsCMYK(t *testing.T) {
	assert.True(t, visualPreviewJPEGColorModelSupported(color.GrayModel))
	assert.True(t, visualPreviewJPEGColorModelSupported(color.YCbCrModel))
	assert.True(t, visualPreviewJPEGColorModelSupported(color.RGBAModel))
	assert.False(t, visualPreviewJPEGColorModelSupported(color.CMYKModel))
}

func TestProduceVisualPreviewKeepsImageReadErrorsRetryable(t *testing.T) {
	readErr := errors.New("injected read failure")
	sources := []struct {
		name, mediaType string
		data            []byte
		failAtSeeks     [2]int
	}{
		{name: "jpeg", mediaType: "image/jpeg", data: mediatest.JPEG(3, 2, color.White), failAtSeeks: [2]int{3, 4}},
		{name: "png", mediaType: "image/png", data: mediatest.PNG(3, 2, color.White), failAtSeeks: [2]int{3, 4}},
		{name: "gif", mediaType: "image/gif", data: mediatest.GIF(3, 2, 1), failAtSeeks: [2]int{2, 3}},
		{name: "webp", mediaType: "image/webp", data: mustDecodeWebP(t), failAtSeeks: [2]int{3, 4}},
	}
	phases := []struct {
		name string
	}{
		{name: "header"},
		{name: "pixels"},
	}
	for _, source := range sources {
		t.Run(source.name, func(t *testing.T) {
			digest := sha256.Sum256(source.data)
			for index, phase := range phases {
				t.Run(phase.name, func(t *testing.T) {
					reader := &failingVisualPreviewReadSeeker{
						Reader: bytes.NewReader(source.data), failAtSeek: source.failAtSeeks[index], err: readErr,
					}

					_, err := ProduceVisualPreview(t.Context(), reader, VisualPreviewTarget{
						SourceSHA256: hex.EncodeToString(digest[:]), Size: int64(len(source.data)),
						MediaType: source.mediaType,
					})
					require.Error(t, err)
					assert.True(t, IsSourceContentUnavailable(err))
					assert.ErrorIs(t, err, readErr)
				})
			}
		})
	}
}

func TestProduceVisualPreviewKeepsCameraRAWReadErrorsRetryable(t *testing.T) {
	readErr := errors.New("injected read failure")
	source := syntheticRAWPreviewTIFF(mediatest.JPEG(3, 2, color.White), 1)
	digest := sha256.Sum256(source)
	reader := &failingVisualPreviewReadSeeker{
		Reader: bytes.NewReader(source), failAtSeek: 2, err: readErr,
	}

	_, err := ProduceVisualPreview(t.Context(), reader, VisualPreviewTarget{
		SourceSHA256: hex.EncodeToString(digest[:]), Size: int64(len(source)), MediaType: "image/x-nikon-nef",
	})
	require.Error(t, err)
	assert.True(t, IsSourceContentUnavailable(err))
	assert.ErrorIs(t, err, readErr)
}

func TestVisualPreviewJPEGUnsupportedFeatureIsTerminal(t *testing.T) {
	product, err := visualPreviewJPEGDecodeResult(
		document.VisualPreviewV1{}, "malformed", jpeg.UnsupportedError("test feature"),
	)
	require.NoError(t, err)
	assert.Equal(t, document.VisualPreviewUnsupported, product.Preview.State)
	require.NotNil(t, product.Preview.Failure)
	assert.Equal(t, "unsupported_jpeg_feature", product.Preview.Failure.Code)
}

func syntheticJPEGSegment(t *testing.T, source []byte, marker byte, payload []byte) []byte {
	t.Helper()
	require.LessOrEqual(t, len(payload)+2, int(^uint16(0)))
	header := []byte{0xff, marker, 0, 0}
	binary.BigEndian.PutUint16(header[2:], uint16(len(payload)+2))
	result := make([]byte, 0, len(source)+len(header)+len(payload))
	result = append(result, source[:2]...)
	result = append(result, header...)
	result = append(result, payload...)
	return append(result, source[2:]...)
}

func syntheticRAWPreviewTIFF(preview []byte, orientation uint16) []byte {
	const (
		headerSize       = 8
		rootEntries      = 1
		previewEntries   = 2
		rootIFDSize      = 2 + rootEntries*12 + 4
		previewIFDSize   = 2 + previewEntries*12 + 4
		previewIFDOffset = headerSize + rootIFDSize
		previewOffset    = previewIFDOffset + previewIFDSize
	)
	source := make([]byte, previewOffset+len(preview))
	copy(source, "II")
	binary.LittleEndian.PutUint16(source[2:4], 42)
	binary.LittleEndian.PutUint32(source[4:8], headerSize)
	binary.LittleEndian.PutUint16(source[8:10], rootEntries)
	binary.LittleEndian.PutUint16(source[10:12], visualPreviewRAWOrientationTag)
	binary.LittleEndian.PutUint16(source[12:14], 3)
	binary.LittleEndian.PutUint32(source[14:18], 1)
	binary.LittleEndian.PutUint16(source[18:20], orientation)
	binary.LittleEndian.PutUint32(source[22:26], previewIFDOffset)
	binary.LittleEndian.PutUint16(source[previewIFDOffset:previewIFDOffset+2], previewEntries)
	binary.LittleEndian.PutUint16(source[previewIFDOffset+2:previewIFDOffset+4], visualPreviewRAWOffsetTag)
	binary.LittleEndian.PutUint16(source[previewIFDOffset+4:previewIFDOffset+6], 4)
	binary.LittleEndian.PutUint32(source[previewIFDOffset+6:previewIFDOffset+10], 1)
	binary.LittleEndian.PutUint32(source[previewIFDOffset+10:previewIFDOffset+14], previewOffset)
	binary.LittleEndian.PutUint16(source[previewIFDOffset+14:previewIFDOffset+16], visualPreviewRAWLengthTag)
	binary.LittleEndian.PutUint16(source[previewIFDOffset+16:previewIFDOffset+18], 4)
	binary.LittleEndian.PutUint32(source[previewIFDOffset+18:previewIFDOffset+22], 1)
	binary.LittleEndian.PutUint32(source[previewIFDOffset+22:previewIFDOffset+26], uint32(len(preview)))
	copy(source[previewOffset:], preview)
	return source
}

func syntheticPNGChunk(t *testing.T, source []byte, chunkType string, payload []byte) []byte {
	t.Helper()
	require.Len(t, chunkType, 4)
	require.GreaterOrEqual(t, len(source), 33)
	chunk := make([]byte, 12+len(payload))
	binary.BigEndian.PutUint32(chunk[:4], uint32(len(payload)))
	copy(chunk[4:8], chunkType)
	copy(chunk[8:], payload)
	binary.BigEndian.PutUint32(chunk[len(chunk)-4:], crc32.ChecksumIEEE(chunk[4:len(chunk)-4]))
	result := append([]byte{}, source[:33]...)
	result = append(result, chunk...)
	return append(result, source[33:]...)
}

func mustDecodeWebP(t *testing.T) []byte {
	t.Helper()
	source, err := base64.StdEncoding.DecodeString(
		"UklGRrIBAABXRUJQVlA4TKUBAAAvSsAYAA8w//M///MfeJAkbXvaSG7m8Q3GfYSBJekwQztm/IcZlgwnmWImn2BK7aFmBtnVir6q//8VOkFE/xm4baTIu8c48ArEo6+B3zFKYln3pqClSCKX0begFTAXFOLXHSyF8cCNcZEG4OywuA4KVVfJCiArU7GAgJI8+lJP/OKMT/fBAjevg1cYB7YVkFuWga2lyPi5I0HFy5YTpWIHg0RZpkniRVW9odHAKOwosWuOGdxIyn2OvaCDvhg/we6TwadPBPbqBV58MsLmMJ8yZnOWk8SRz4N+QoyPL+MnamzMvcE1rHNEr91F9GKZPVUcS9w7PhhH36suB9qPeYb/oLk6cuTiJ0wOK3m5h1cKjW6EVZCYMK7dxcKCBdgP9HkKr9gkAO2P8GKZGWVdIAatQa+1IDpt6qyorVwdy01xdW8Jkfk6xjEXmVQQ+HQdFr6OKhIN34dXWq0+0qr6EJSCeeVLH9+gvGTLyqM65PQ44ihzlTXxQKjKbAvshXgir7Lil9w4L2bvMycmjQcqXaMCO6BlY28i+FOLzbfI1vEqxAhotocAAA==",
	)
	require.NoError(t, err)
	return source
}

func syntheticExtendedWebP(
	t *testing.T, source []byte, width, height int, flags byte, chunkType string, payload []byte,
) []byte {
	t.Helper()
	require.GreaterOrEqual(t, len(source), 12)
	require.Equal(t, "RIFF", string(source[:4]))
	require.Equal(t, "WEBP", string(source[8:12]))
	require.Len(t, chunkType, 4)

	vp8x := make([]byte, 18)
	copy(vp8x[:4], "VP8X")
	binary.LittleEndian.PutUint32(vp8x[4:8], 10)
	vp8x[8] = flags
	w, h := width-1, height-1
	vp8x[12], vp8x[13], vp8x[14] = byte(w), byte(w>>8), byte(w>>16)
	vp8x[15], vp8x[16], vp8x[17] = byte(h), byte(h>>8), byte(h>>16)
	chunk := make([]byte, 8+len(payload)+len(payload)%2)
	copy(chunk[:4], chunkType)
	binary.LittleEndian.PutUint32(chunk[4:8], uint32(len(payload)))
	copy(chunk[8:], payload)
	body := make([]byte, 0, len(vp8x)+len(source)-12+len(chunk))
	body = append(body, vp8x...)
	body = append(body, source[12:]...)
	body = append(body, chunk...)
	result := make([]byte, 12, 12+len(body))
	copy(result[:4], "RIFF")
	binary.LittleEndian.PutUint32(result[4:8], uint32(4+len(body)))
	copy(result[8:12], "WEBP")
	return append(result, body...)
}

type failingVisualPreviewReadSeeker struct {
	*bytes.Reader

	err        error
	failAtSeek int
	startSeeks int
}

func (r *failingVisualPreviewReadSeeker) Read(target []byte) (int, error) {
	if r.startSeeks == r.failAtSeek {
		return 0, r.err
	}
	return r.Reader.Read(target) //nolint:wrapcheck // The test double preserves Reader error identity.
}

func (r *failingVisualPreviewReadSeeker) Seek(offset int64, whence int) (int64, error) {
	position, err := r.Reader.Seek(offset, whence)
	if err == nil && offset == 0 && whence == io.SeekStart {
		r.startSeeks++
	}
	return position, err //nolint:wrapcheck // The test double preserves Seeker error identity.
}
