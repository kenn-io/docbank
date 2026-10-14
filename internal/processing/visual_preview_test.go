package processing

import (
	"bufio"
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
	"os"
	"path/filepath"
	"strings"
	"runtime"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/bundle"
	"go.kenn.io/docbank/document/media"
	"go.kenn.io/docbank/document/media/mediatest"
	"go.kenn.io/docbank/internal/blob"
	"go.kenn.io/docbank/internal/ingest"
	"go.kenn.io/docbank/internal/store"
)

func TestByteFirstPNGRefinementKeepsAnimatedPNGPreviewable(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	catalog, err := store.Open(filepath.Join(root, "docbank.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, catalog.Close()) })
	blobs, err := blob.New(store.NewPackCatalog(catalog), filepath.Join(root, "blobs"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, blobs.Close()) })
	source := syntheticAPNG(t, mediatest.PNG(4, 3, color.White))
	ing := &ingest.Ingester{Store: catalog, Blobs: blobs}
	for _, suffix := range []string{".png", ".apng"} {
		t.Run(suffix, func(t *testing.T) {
			sourcePath := filepath.Join(root, "animation"+suffix)
			require.NoError(t, os.WriteFile(sourcePath, source, 0o600))

			result, err := ing.AddPaths(t.Context(), []string{sourcePath}, "/inbox")
			require.NoError(t, err)
			require.Equal(t, 1, result.Added)
			node, err := catalog.NodeByPath(t.Context(), "/inbox/animation"+suffix)
			require.NoError(t, err)
			require.Equal(t, "image/png", node.MimeType)
			stored, err := blobs.Open(node.BlobHash)
			require.NoError(t, err)
			storedBytes, err := io.ReadAll(stored)
			require.NoError(t, err)
			require.NoError(t, stored.Close())
			require.Equal(t, source, storedBytes)

			metadata, err := media.DetectBytes(storedBytes, node.MimeType)
			require.NoError(t, err)
			assert.True(t, metadata.Animated)

			digest := sha256.Sum256(storedBytes)
			product, err := ProduceVisualPreview(t.Context(), bytes.NewReader(storedBytes), VisualPreviewTarget{
				SourceSHA256: hex.EncodeToString(digest[:]), Size: node.Size, MediaType: node.MimeType,
			})
			require.NoError(t, err)
			assert.Equal(t, document.VisualPreviewReady, product.Preview.State)
			require.NotNil(t, product.Preview.Output)
			require.NotEmpty(t, product.Output)
		})
	}
}

func TestProduceVisualPreviewAppliesEXIFOrientation(t *testing.T) {
	t.Parallel()
	tiff := syntheticTIFF(42,
		[]syntheticTIFFEntry{tiffShort(0x0112, 6)},
		nil,
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
	t.Parallel()
	recipe := CurrentVisualPreviewRecipe()
	assert.Equal(t, 4096, recipe.MaxEdgePixels)
	width, height := boundedVisualPreviewDimensionsForEdge(6000, 4000, visualPreviewMaxEdgePixels)
	assert.Equal(t, 4096, width)
	assert.Equal(t, 2731, height)
}

func TestProduceVisualPreviewAcceptsJPEGMediaTypeParameters(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
	frame := image.NewNRGBA(image.Rect(0, 0, 3, 2))
	for index := range 6 {
		frame.SetNRGBA(index%3, index/3, color.NRGBA{R: uint8(index * 40), G: uint8(200 - index*30), A: 255})
	}
	var encoded bytes.Buffer
	require.NoError(t, png.Encode(&encoded, frame))
	original := encoded.Bytes()
	untilIEND := func(source []byte) []byte { return bytes.Clone(source[:len(source)-12]) }
	exif := syntheticTIFF(42, []syntheticTIFFEntry{tiffShort(0x0112, 6)}, nil)
	leading := syntheticPNGChunk(t, original, "eXIf", exif)
	trailing := appendSyntheticPNGChunk(untilIEND(original), "eXIf", exif)
	trailing = appendSyntheticPNGChunk(trailing, "IEND", nil)
	unusableTrailing := appendSyntheticPNGChunk(untilIEND(original), "eXIf", nil)
	unusableTrailing = appendSyntheticPNGChunk(unusableTrailing, "eXIf", exif)
	unusableTrailing = appendSyntheticPNGChunk(unusableTrailing, "IEND", nil)
	malformedTrailing := appendSyntheticPNGChunk(untilIEND(original), "eXIf",
		[]byte{0x49, 0x49, 0x2a, 0x00, 0xff, 0xff, 0xff, 0xff})
	malformedTrailing = appendSyntheticPNGChunk(malformedTrailing, "eXIf", exif)
	malformedTrailing = appendSyntheticPNGChunk(malformedTrailing, "IEND", nil)
	unusableLeading := syntheticPNGChunk(t, original, "eXIf", nil)
	unusableLeading = appendSyntheticPNGChunk(untilIEND(unusableLeading), "eXIf", exif)
	unusableLeading = appendSyntheticPNGChunk(unusableLeading, "IEND", nil)
	rotate180 := syntheticTIFF(42, []syntheticTIFFEntry{tiffShort(0x0112, 3)}, nil)
	twoLeading := syntheticPNGChunk(t, syntheticPNGChunk(t, original, "eXIf", rotate180), "eXIf", exif)
	unusableThenLeading := syntheticPNGChunk(t, syntheticPNGChunk(t, original, "eXIf", exif), "eXIf", nil)
	duplicate := appendSyntheticPNGChunk(untilIEND(leading), "eXIf", rotate180)
	duplicate = appendSyntheticPNGChunk(duplicate, "IEND", nil)
	oversized := appendSyntheticPNGChunk(untilIEND(original), "eXIf", make([]byte, visualPreviewMaxEXIFBytes+1))
	oversized = appendSyntheticPNGChunk(oversized, "eXIf", exif)
	oversized = appendSyntheticPNGChunk(oversized, "IEND", nil)
	manyText := untilIEND(original)
	for range visualPreviewMaxPNGChunks + 1 {
		manyText = appendSyntheticPNGChunk(manyText, "tEXt", []byte("comment\x00text"))
	}
	manyText = appendSyntheticPNGChunk(manyText, "eXIf", exif)
	manyText = appendSyntheticPNGChunk(manyText, "IEND", nil)
	for name, test := range map[string]struct {
		source        []byte
		width, height int
	}{
		"before IDAT":                      {leading, 2, 3},
		"after IDAT":                       {trailing, 2, 3},
		"leading EXIF wins over trailing":  {duplicate, 2, 3},
		"first of two leading EXIF wins":   {twoLeading, 2, 3},
		"unusable first leading EXIF wins": {unusableThenLeading, 3, 2},
		"unusable leading EXIF wins":       {unusableLeading, 3, 2},
		"empty first trailing EXIF wins":   {unusableTrailing, 3, 2},
		"malformed first trailing wins":    {malformedTrailing, 3, 2},
		"oversized first trailing wins":    {oversized, 3, 2},
		"many trailing text chunks":        {manyText, 2, 3},
	} {
		t.Run(name, func(t *testing.T) {
			digest := sha256.Sum256(test.source)
			product, err := ProduceVisualPreview(t.Context(), bytes.NewReader(test.source), VisualPreviewTarget{
				SourceSHA256: hex.EncodeToString(digest[:]), Size: int64(len(test.source)), MediaType: "image/png",
			})
			require.NoError(t, err)
			assert.Equal(t, document.VisualPreviewReady, product.Preview.State)
			require.NotNil(t, product.Preview.Output)
			assert.Equal(t, test.width, product.Preview.Output.Width)
			assert.Equal(t, test.height, product.Preview.Output.Height)
			if strings.Contains(name, "wins") && name != "oversized first trailing wins" {
				input := photoRenderInput(test.source, "image/png")
				output, receipt, err := renderPhotoExport(t.Context(), test.source, input, bundle.PhotoRenderProfile{Format: "png"})
				require.NoError(t, err)
				require.Equal(t, test.width, receipt.Width)
				require.Equal(t, test.height, receipt.Height)
				decoded, err := png.Decode(bytes.NewReader(output))
				require.NoError(t, err)
				order := []int{0, 1, 2, 3, 4, 5}
				if test.width == 2 {
					order = []int{3, 0, 4, 1, 5, 2}
				}
				for index, sourceIndex := range order {
					require.Equal(t, frame.At(sourceIndex%3, sourceIndex/3), color.NRGBAModel.Convert(decoded.At(index%test.width, index/test.width)))
				}
				_, _, err = renderPhotoExport(t.Context(), test.source, input, bundle.PhotoRenderProfile{Format: "png", IncludeMetadata: true})
				require.ErrorIs(t, err, bundle.ErrUnavailable)
			}
		})
	}
}

func TestVisualPreviewPNGScansSkipIDATPayload(t *testing.T) {
	t.Parallel()
	original := mediatest.PNG(3, 2, color.White)
	idatStart := int64(len(original) - 12)
	source := appendSyntheticPNGChunk(bytes.Clone(original[:idatStart]), "IDAT", make([]byte, 64*1024))
	idatEnd := int64(len(source))
	source = appendSyntheticPNGChunk(source, "eXIf", syntheticTIFF(42, []syntheticTIFFEntry{tiffShort(0x0112, 6)}, nil))
	source = appendSyntheticPNGChunk(source, "IEND", nil)
	reader := &recordingVisualPreviewReadSeeker{
		Reader: bytes.NewReader(source), payloadStart: idatStart + 8, payloadEnd: idatEnd - 4,
	}

	_, colorSpace, unsupportedMetadata, malformed, trailingOffset, err :=
		inspectVisualPreviewPNG(t.Context(), reader, int64(len(source)))
	require.NoError(t, err)
	assert.Empty(t, colorSpace)
	assert.False(t, unsupportedMetadata)
	assert.False(t, malformed)
	require.NotZero(t, trailingOffset)

	orientation, err := trailingVisualPreviewPNGOrientation(t.Context(), reader, int64(len(source)), trailingOffset)
	require.NoError(t, err)
	assert.Equal(t, 6, orientation)
	assert.Zero(t, reader.payloadBytes, "PNG scans must seek past IDAT payload")
}

func TestProduceVisualPreviewUsesGIFPrimaryFrame(t *testing.T) {
	t.Parallel()
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
	decoded, err := decodeVisualPixels(t.Context(), bytes.NewReader(source), "gif")
	require.NoError(t, err)
	red, green, blue, alpha := decoded.image.At(8, 4).RGBA()
	assert.Equal(t, [4]uint32{}, [4]uint32{red, green, blue, alpha})
	red, green, blue, alpha = decoded.image.At(32, 16).RGBA()
	assert.Equal(t, [4]uint32{0, 0, 0, 65535}, [4]uint32{red, green, blue, alpha})
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
	t.Parallel()
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
	t.Parallel()
	preview := mediatest.JPEG(3, 2, color.White)
	source := syntheticRAWPreviewTIFF(6, preview)
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

func TestProduceVisualPreviewAcceptsSingleStripDNGPreview(t *testing.T) {
	t.Parallel()
	preview := mediatest.JPEG(3, 2, color.White)
	source := syntheticRAWPreviewTIFFCandidates(1, syntheticRAWPreviewCandidate{
		data: preview, singleStrip: true,
	})
	digest := sha256.Sum256(source)

	product, err := ProduceVisualPreview(t.Context(), bytes.NewReader(source), VisualPreviewTarget{
		SourceSHA256: hex.EncodeToString(digest[:]), Size: int64(len(source)), MediaType: "image/x-adobe-dng",
	})
	require.NoError(t, err)
	assert.Equal(t, document.VisualPreviewReady, product.Preview.State)
	require.NotNil(t, product.Preview.Output)
	assert.Equal(t, 3, product.Preview.Output.Width)
	assert.Equal(t, 2, product.Preview.Output.Height)
}

func TestProduceVisualPreviewFallsBackToSmallerUsableCameraRAWPreview(t *testing.T) {
	t.Parallel()
	preview := mediatest.JPEG(3, 2, color.White)
	invalid := make([]byte, len(preview)+1)
	source := syntheticRAWPreviewTIFF(1, preview, invalid)
	digest := sha256.Sum256(source)

	product, err := ProduceVisualPreview(t.Context(), bytes.NewReader(source), VisualPreviewTarget{
		SourceSHA256: hex.EncodeToString(digest[:]), Size: int64(len(source)), MediaType: "image/x-nikon-nef",
	})
	require.NoError(t, err)
	assert.Equal(t, document.VisualPreviewReady, product.Preview.State)
	require.NotNil(t, product.Preview.Output)
	assert.Equal(t, 3, product.Preview.Output.Width)
	assert.Equal(t, 2, product.Preview.Output.Height)
}

func TestProduceVisualPreviewUsesCandidateCameraRAWOrientation(t *testing.T) {
	t.Parallel()
	preview := mediatest.JPEG(4, 3, color.White)
	source := syntheticRAWPreviewTIFFCandidates(6, syntheticRAWPreviewCandidate{
		data: preview, orientation: 1,
	})
	digest := sha256.Sum256(source)

	product, err := ProduceVisualPreview(t.Context(), bytes.NewReader(source), VisualPreviewTarget{
		SourceSHA256: hex.EncodeToString(digest[:]), Size: int64(len(source)), MediaType: "image/x-nikon-nef",
	})
	require.NoError(t, err)
	assert.Equal(t, document.VisualPreviewReady, product.Preview.State)
	require.NotNil(t, product.Preview.Output)
	assert.Equal(t, 4, product.Preview.Output.Width)
	assert.Equal(t, 3, product.Preview.Output.Height)
}

func TestProduceVisualPreviewAcceptsRAF(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
	source := syntheticRAWPreviewTIFF(1, mediatest.JPEG(3, 2, color.White))
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
	t.Parallel()
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

func TestProduceVisualPreviewRecordsTruncatedPNGFailure(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
	source := mediatest.PNG(1, 1, color.White)
	binary.BigEndian.PutUint32(source[16:20], 10001)
	binary.BigEndian.PutUint32(source[20:24], 10001)
	binary.BigEndian.PutUint32(source[29:33], crc32.ChecksumIEEE(source[12:29]))
	source = appendSyntheticPNGChunk(bytes.Clone(source[:len(source)-12]), "tEXt", make([]byte, 64*1024))
	tailStart := int64(len(source))
	source = appendSyntheticPNGChunk(source, "eXIf", syntheticTIFF(42, []syntheticTIFFEntry{tiffShort(0x0112, 6)}, nil))
	source = appendSyntheticPNGChunk(source, "IEND", nil)
	reader := &recordingVisualPreviewReadSeeker{Reader: bytes.NewReader(source)}
	product, err := produceVisualPreviewPNG(t.Context(), reader, int64(len(source)), document.VisualPreviewV1{})
	require.NoError(t, err)
	assert.Equal(t, document.VisualPreviewFailed, product.Preview.State)
	require.NotNil(t, product.Preview.Failure)
	assert.Equal(t, "source_dimensions_exceed_limit", product.Preview.Failure.Code)
	assert.Less(t, reader.maxReadEnd, tailStart)
}

func TestProduceVisualPreviewRecordsMalformedJPEGFailure(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
	assert.True(t, visualPreviewJPEGColorModelSupported(color.GrayModel))
	assert.True(t, visualPreviewJPEGColorModelSupported(color.YCbCrModel))
	assert.True(t, visualPreviewJPEGColorModelSupported(color.RGBAModel))
	assert.False(t, visualPreviewJPEGColorModelSupported(color.CMYKModel))
}

func TestProduceVisualPreviewKeepsImageReadErrorsRetryable(t *testing.T) {
	t.Parallel()
	readErr := errors.New("injected read failure")
	pngSource := mediatest.PNG(3, 2, color.White)
	trailingPNG := appendSyntheticPNGChunk(bytes.Clone(pngSource[:len(pngSource)-12]), "IDAT", make([]byte, 64*1024))
	failReadAt := int64(len(trailingPNG))
	trailingPNG = appendSyntheticPNGChunk(trailingPNG, "eXIf", syntheticTIFF(42, []syntheticTIFFEntry{tiffShort(0x0112, 6)}, nil))
	trailingPNG = appendSyntheticPNGChunk(trailingPNG, "IEND", nil)
	sources := []struct {
		name, mediaType string
		data            []byte
		failAtSeeks     []int
		failReadAt      int64
	}{
		{name: "jpeg", mediaType: "image/jpeg", data: mediatest.JPEG(3, 2, color.White), failAtSeeks: []int{3, 4}},
		{name: "oversize jpeg", mediaType: "image/jpeg", data: syntheticVisualJPEGDC(t, 10008, 10008, true, 0), failAtSeeks: []int{3, 5}},
		{name: "png", mediaType: "image/png", data: pngSource, failAtSeeks: []int{3, 4}},
		{name: "png after IDAT", mediaType: "image/png", data: trailingPNG, failAtSeeks: []int{4}, failReadAt: failReadAt},
		{name: "gif", mediaType: "image/gif", data: mediatest.GIF(3, 2, 1), failAtSeeks: []int{2, 3}},
		{name: "webp", mediaType: "image/webp", data: mustDecodeWebP(t), failAtSeeks: []int{3, 4}},
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
			for index, failAtSeek := range source.failAtSeeks {
				phase := phases[index].name
				if source.failReadAt != 0 {
					phase = "after IDAT"
				}
				t.Run(phase, func(t *testing.T) {
					reader := &failingVisualPreviewReadSeeker{
						Reader: bytes.NewReader(source.data), failAtSeek: failAtSeek, failReadAt: source.failReadAt, err: readErr,
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
	t.Parallel()
	readErr := errors.New("injected read failure")
	source := syntheticRAWPreviewTIFF(1, mediatest.JPEG(3, 2, color.White))
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
	t.Parallel()
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

func syntheticRAWPreviewTIFF(orientation uint16, previews ...[]byte) []byte {
	candidates := make([]syntheticRAWPreviewCandidate, len(previews))
	for index, preview := range previews {
		candidates[index].data = preview
	}
	return syntheticRAWPreviewTIFFCandidates(orientation, candidates...)
}

type syntheticRAWPreviewCandidate struct {
	data        []byte
	orientation uint16
	singleStrip bool
}

func syntheticRAWPreviewTIFFCandidates(
	rootOrientation uint16, previews ...syntheticRAWPreviewCandidate,
) []byte {
	const (
		headerSize     = 8
		rootEntries    = 1
		rootIFDSize    = 2 + rootEntries*12 + 4
		firstIFDOffset = headerSize + rootIFDSize
	)
	ifdOffsets := make([]int, len(previews))
	previewOffset := firstIFDOffset
	for index, preview := range previews {
		ifdOffsets[index] = previewOffset
		entries := 2
		if preview.singleStrip {
			entries++
		}
		if preview.orientation != 0 {
			entries++
		}
		previewOffset += 2 + entries*12 + 4
	}
	totalSize := previewOffset
	for _, preview := range previews {
		totalSize += len(preview.data)
	}
	source := make([]byte, totalSize)
	copy(source, "II")
	binary.LittleEndian.PutUint16(source[2:4], 42)
	binary.LittleEndian.PutUint32(source[4:8], headerSize)
	externalOffset := totalSize
	writeSyntheticTIFFIFD(source, headerSize,
		[]syntheticTIFFEntry{tiffShort(visualPreviewRAWOrientationTag, rootOrientation)}, &externalOffset)
	if len(ifdOffsets) > 0 {
		binary.LittleEndian.PutUint32(source[22:26], uint32(ifdOffsets[0]))
	}
	for index, preview := range previews {
		ifdOffset := ifdOffsets[index]
		offsetTag := uint16(visualPreviewRAWOffsetTag)
		lengthTag := uint16(visualPreviewRAWLengthTag)
		var entries []syntheticTIFFEntry
		if preview.singleStrip {
			offsetTag = visualPreviewRAWStripOffsetsTag
			lengthTag = visualPreviewRAWStripByteCountsTag
			entries = append(entries, tiffShort(visualPreviewRAWCompressionTag, visualPreviewRAWJPEGCompression))
		}
		entries = append(entries,
			tiffLong(offsetTag, uint32(previewOffset)),
			tiffLong(lengthTag, uint32(len(preview.data))),
		)
		if preview.orientation != 0 {
			entries = append(entries, tiffShort(visualPreviewRAWOrientationTag, preview.orientation))
		}
		writeSyntheticTIFFIFD(source, ifdOffset, entries, &externalOffset)
		if index+1 < len(previews) {
			nextOffset := ifdOffset + 2 + len(entries)*12
			binary.LittleEndian.PutUint32(source[nextOffset:nextOffset+4], uint32(ifdOffsets[index+1]))
		}
		copy(source[previewOffset:], preview.data)
		previewOffset += len(preview.data)
	}
	return source
}

func syntheticPNGChunk(t *testing.T, source []byte, chunkType string, payload []byte) []byte {
	t.Helper()
	require.Len(t, chunkType, 4)
	require.GreaterOrEqual(t, len(source), 33)
	var result bytes.Buffer
	result.Write(source[:33])
	writePhotoPNGChunk(&result, chunkType, payload)
	result.Write(source[33:])
	return result.Bytes()
}

func syntheticAPNG(t *testing.T, source []byte) []byte {
	t.Helper()
	require.GreaterOrEqual(t, len(source), 33)
	var idatPayloads [][]byte
	for offset := 33; offset+12 <= len(source); {
		length := int(binary.BigEndian.Uint32(source[offset : offset+4]))
		end := offset + 12 + length
		require.LessOrEqual(t, end, len(source))
		switch string(source[offset+4 : offset+8]) {
		case "IDAT":
			idatPayloads = append(idatPayloads, append([]byte(nil), source[offset+8:offset+8+length]...))
		case "IEND":
			offset = len(source)
			continue
		}
		offset = end
	}
	require.NotEmpty(t, idatPayloads)

	output := append([]byte(nil), source[:33]...)
	actl := make([]byte, 8)
	binary.BigEndian.PutUint32(actl[:4], 2)
	output = appendSyntheticPNGChunk(output, "acTL", actl)
	sequence := uint32(0)
	for frame := range 2 {
		fctl := make([]byte, 26)
		binary.BigEndian.PutUint32(fctl[:4], sequence)
		sequence++
		copy(fctl[4:12], source[16:24])
		fctl[23] = 10
		output = appendSyntheticPNGChunk(output, "fcTL", fctl)
		for _, payload := range idatPayloads {
			if frame == 0 {
				output = appendSyntheticPNGChunk(output, "IDAT", payload)
				continue
			}
			frameData := binary.BigEndian.AppendUint32(nil, sequence)
			sequence++
			frameData = append(frameData, payload...)
			output = appendSyntheticPNGChunk(output, "fdAT", frameData)
		}
	}
	return appendSyntheticPNGChunk(output, "IEND", nil)
}

func appendSyntheticPNGChunk(output []byte, chunkType string, payload []byte) []byte {
	result := bytes.NewBuffer(output)
	writePhotoPNGChunk(result, chunkType, payload)
	return result.Bytes()
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
	if chunkType == "ICCP" {
		body = append(body, chunk...)
		body = append(body, source[12:]...)
	} else {
		body = append(body, source[12:]...)
		body = append(body, chunk...)
	}
	result := make([]byte, 12, 12+len(body))
	copy(result[:4], "RIFF")
	binary.LittleEndian.PutUint32(result[4:8], uint32(4+len(body)))
	copy(result[8:12], "WEBP")
	return append(result, body...)
}

type recordingVisualPreviewReadSeeker struct {
	*bytes.Reader

	payloadStart int64
	payloadEnd   int64
	payloadBytes int64
	maxReadEnd   int64
}

func (r *recordingVisualPreviewReadSeeker) Read(target []byte) (int, error) {
	position := r.Size() - int64(r.Len())
	n, err := r.Reader.Read(target)
	end := position + int64(n)
	r.payloadBytes += max(int64(0), min(end, r.payloadEnd)-max(position, r.payloadStart))
	r.maxReadEnd = max(r.maxReadEnd, end)
	return n, err //nolint:wrapcheck // The test double preserves Reader error identity.
}

type failingVisualPreviewReadSeeker struct {
	*bytes.Reader

	err        error
	failAtSeek int
	failReadAt int64
	startSeeks int
}

func (r *failingVisualPreviewReadSeeker) Read(target []byte) (int, error) {
	if r.startSeeks == r.failAtSeek && r.Size()-int64(r.Len()) >= r.failReadAt {
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

//nolint:paralleltest // Measures allocations without concurrent preview producers.
func TestProduceVisualPreviewDownscalesOversizedJPEG(t *testing.T) {
	for _, progressive := range []bool{true, false} {
		t.Run(strconv.FormatBool(progressive), func(t *testing.T) {
			source := syntheticVisualJPEGDC(t, 10008, 10008, progressive, 0)
			digest := sha256.Sum256(source)
			recipe, err := VisualPreviewRecipeForSize("grid")
			require.NoError(t, err)
			var before, after runtime.MemStats
			runtime.ReadMemStats(&before)
			product, err := ProduceVisualPreviewForRecipe(t.Context(), bytes.NewReader(source), VisualPreviewTarget{
				SourceSHA256: hex.EncodeToString(digest[:]), Size: int64(len(source)), MediaType: "image/jpeg",
			}, recipe)
			runtime.ReadMemStats(&after)
			require.NoError(t, err)
			require.Equal(t, document.VisualPreviewReady, product.Preview.State)
			require.NotNil(t, product.Preview.Output)
			assert.Equal(t, 512, product.Preview.Output.Width)
			assert.Equal(t, 512, product.Preview.Output.Height)
			assert.Less(t, after.TotalAlloc-before.TotalAlloc, uint64(32<<20))
			decoded, err := jpeg.Decode(bytes.NewReader(product.Output))
			require.NoError(t, err)
			assert.Equal(t, image.Rect(0, 0, 512, 512), decoded.Bounds())
		})
	}
}

func TestVisualPreviewJPEGDCBlockMeans(t *testing.T) {
	t.Parallel()
	gray := image.NewGray(image.Rect(0, 0, 37, 29))
	ycbcr := image.NewYCbCr(gray.Bounds(), image.YCbCrSubsampleRatio420)
	for y := range 29 {
		for x := range 37 {
			gray.Pix[gray.PixOffset(x, y)] = byte(40 + 17*(x/8) + 23*(y/8))
			ycbcr.Y[ycbcr.YOffset(x, y)] = gray.GrayAt(x, y).Y
			i := ycbcr.COffset(x, y)
			ycbcr.Cb[i], ycbcr.Cr[i] = byte(100+9*(x/16)), byte(100+13*(y/16))
		}
	}
	progressive, err := os.ReadFile("testdata/visual-preview-progressive.jpg")
	require.NoError(t, err)
	for _, test := range []struct {
		name string
		img  image.Image
		data []byte
	}{
		{name: "gray", img: gray},
		{name: "420", img: ycbcr},
		{name: "progressive refinement", data: progressive},
	} {
		t.Run(test.name, func(t *testing.T) {
			source := test.data
			if test.img != nil {
				var buffer bytes.Buffer
				require.NoError(t, jpeg.Encode(&buffer, test.img, &jpeg.Options{Quality: 90}))
				source = buffer.Bytes()
			}
			full, err := jpeg.Decode(bytes.NewReader(source))
			require.NoError(t, err)
			reduced, err := decodeVisualPreviewJPEGDC(bufio.NewReader(bytes.NewReader(source)))
			require.NoError(t, err)
			assert.Equal(t, image.Rect(0, 0, 5, 4), reduced.Bounds())
			if fullGray, ok := full.(*image.Gray); ok {
				reducedGray, ok := reduced.(*image.Gray)
				require.True(t, ok)
				assertVisualJPEGDCMeans(t, fullGray.Pix, fullGray.Stride, 37, 29, reducedGray.Pix)
			} else {
				fullYCbCr, ok := full.(*image.YCbCr)
				require.True(t, ok)
				reducedYCbCr, ok := reduced.(*image.YCbCr)
				require.True(t, ok)
				assertVisualJPEGDCMeans(t, fullYCbCr.Y, fullYCbCr.YStride, 37, 29, reducedYCbCr.Y)
				cw, ch := 37, 29
				if fullYCbCr.SubsampleRatio == image.YCbCrSubsampleRatio420 {
					cw, ch = 19, 15
				}
				assertVisualJPEGDCMeans(t, fullYCbCr.Cb, fullYCbCr.CStride, cw, ch, reducedYCbCr.Cb)
				assertVisualJPEGDCMeans(t, fullYCbCr.Cr, fullYCbCr.CStride, cw, ch, reducedYCbCr.Cr)
			}
		})
	}
}

func assertVisualJPEGDCMeans(t *testing.T, full []byte, stride, width, height int, reduced []byte) {
	t.Helper()
	for y := range (height + 7) / 8 {
		for x := range (width + 7) / 8 {
			sum, count := 0, 0
			for yy := y * 8; yy < min(y*8+8, height); yy++ {
				for xx := x * 8; xx < min(x*8+8, width); xx++ {
					sum += int(full[yy*stride+xx])
					count++
				}
			}
			assert.InDelta(t, float64(sum)/float64(count), reduced[y*((width+7)/8)+x], 2)
		}
	}
}

func TestVisualPreviewJPEGDCRestartsAndFailures(t *testing.T) {
	t.Parallel()
	for _, progressive := range []bool{false, true} {
		source := syntheticVisualJPEGDC(t, 37, 29, progressive, 3)
		decoded, err := decodeVisualPreviewJPEGDC(bufio.NewReader(bytes.NewReader(source)))
		require.NoError(t, err)
		gray, ok := decoded.(*image.Gray)
		require.True(t, ok)
		assert.Equal(t, bytes.Repeat([]byte{128}, 20), gray.Pix)
	}
	source := syntheticVisualJPEGDC(t, 10008, 10008, true, 0)
	truncated := source[:len(source)/3]
	arithmetic := bytes.Clone(source)
	frame := bytes.Index(arithmetic, []byte{0xff, 0xc2})
	require.Positive(t, frame)
	arithmetic[frame+1] = 0xc9
	for _, test := range []struct {
		name  string
		data  []byte
		state document.VisualPreviewState
		code  string
	}{
		{"truncated", truncated, document.VisualPreviewFailed, "decode_failed"},
		{"arithmetic", arithmetic, document.VisualPreviewUnsupported, "unsupported_jpeg_feature"},
	} {
		t.Run(test.name, func(t *testing.T) {
			digest := sha256.Sum256(test.data)
			product, err := ProduceVisualPreview(t.Context(), bytes.NewReader(test.data), VisualPreviewTarget{
				SourceSHA256: hex.EncodeToString(digest[:]), Size: int64(len(test.data)), MediaType: "image/jpeg",
			})
			require.NoError(t, err)
			assert.Equal(t, test.state, product.Preview.State)
			require.NotNil(t, product.Preview.Failure)
			assert.Equal(t, test.code, product.Preview.Failure.Code)
		})
	}
}

func syntheticVisualJPEGDC(t *testing.T, width, height int, progressive bool, restart int) []byte {
	t.Helper()
	source := []byte{0xff, 0xd8}
	segment := func(marker byte, payload []byte) {
		source = append(source, 0xff, marker, byte((len(payload)+2)>>8), byte(len(payload)+2))
		source = append(source, payload...)
	}
	quant := append([]byte{0}, bytes.Repeat([]byte{1}, 64)...)
	segment(0xdb, quant)
	frame := []byte{8, byte(height >> 8), byte(height), byte(width >> 8), byte(width), 1, 1, 0x11, 0}
	marker := byte(0xc0)
	if progressive {
		marker = 0xc2
	}
	segment(marker, frame)
	for _, table := range []byte{0, 0x10} {
		// One one-bit code, zero, represents a zero DC difference or AC EOB.
		segment(0xc4, append([]byte{table, 1}, make([]byte, 16)...))
	}
	if restart > 0 {
		segment(0xdd, []byte{byte(restart >> 8), byte(restart)})
	}
	blocks := ((width + 7) / 8) * ((height + 7) / 8)
	entropy := func(bitsPerBlock int) {
		remaining, rst := blocks, byte(0)
		for remaining > 0 {
			n := remaining
			if restart > 0 {
				n = min(n, restart)
			}
			bits := n * bitsPerBlock
			source = append(source, make([]byte, bits/8)...)
			if bits%8 != 0 {
				source = append(source, byte((1<<(8-bits%8))-1))
			}
			remaining -= n
			if remaining > 0 {
				source = append(source, 0xff, 0xd0+rst)
				rst = (rst + 1) % 8
			}
		}
	}
	if progressive {
		segment(0xda, []byte{1, 1, 0, 0, 0, 0})
		entropy(1)
		segment(0xda, []byte{1, 1, 0, 1, 63, 0})
		entropy(1)
	} else {
		segment(0xda, []byte{1, 1, 0, 0, 63, 0})
		entropy(2)
	}
	return append(source, 0xff, 0xd9)
}
