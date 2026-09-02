package processing

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"hash/crc32"
	"image"
	"image/color"
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
	}{
		{name: "jpeg", mediaType: "image/jpeg", data: mediatest.JPEG(3, 2, color.White)},
		{name: "png", mediaType: "image/png", data: mediatest.PNG(3, 2, color.White)},
	}
	phases := []struct {
		name       string
		failAtSeek int
	}{
		{name: "header", failAtSeek: 3},
		{name: "pixels", failAtSeek: 4},
	}
	for _, source := range sources {
		t.Run(source.name, func(t *testing.T) {
			digest := sha256.Sum256(source.data)
			for _, phase := range phases {
				t.Run(phase.name, func(t *testing.T) {
					reader := &failingVisualPreviewReadSeeker{
						Reader: bytes.NewReader(source.data), failAtSeek: phase.failAtSeek, err: readErr,
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
