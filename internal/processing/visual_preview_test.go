package processing

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"image/color"
	"image/jpeg"
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

func TestProduceVisualPreviewKeepsJPEGReadErrorsRetryable(t *testing.T) {
	source := mediatest.JPEG(3, 2, color.White)
	digest := sha256.Sum256(source)
	readErr := errors.New("injected read failure")
	for _, test := range []struct {
		name       string
		failAtSeek int
	}{
		{name: "header", failAtSeek: 3},
		{name: "pixels", failAtSeek: 4},
	} {
		t.Run(test.name, func(t *testing.T) {
			reader := &failingVisualPreviewReadSeeker{
				Reader: bytes.NewReader(source), failAtSeek: test.failAtSeek, err: readErr,
			}

			_, err := ProduceVisualPreview(t.Context(), reader, VisualPreviewTarget{
				SourceSHA256: hex.EncodeToString(digest[:]), Size: int64(len(source)), MediaType: "image/jpeg",
			})
			require.Error(t, err)
			assert.True(t, IsSourceContentUnavailable(err))
			assert.ErrorIs(t, err, readErr)
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
