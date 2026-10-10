package processing

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document/bundle"
	"go.kenn.io/docbank/document/media/mediatest"
	"go.kenn.io/docbank/internal/store"
)

func photoRenderInput(data []byte, mediaType string) store.PhotoExportInput {
	h := sha256.Sum256(data)
	return store.PhotoExportInput{Member: bundle.Member{NodeID: 1, VersionID: "11111111-1111-4111-8111-111111111111", SHA256: hex.EncodeToString(h[:]), Size: int64(len(data))}, MediaType: mediaType, Keywords: []string{}}
}

func TestPhotoExportOrientationAndAuthoredRotation(t *testing.T) {
	t.Parallel()
	frame := image.NewNRGBA(image.Rect(0, 0, 3, 2))
	for y := range 2 {
		for x := range 3 {
			frame.SetNRGBA(x, y, color.NRGBA{R: uint8(30 + x*50), G: uint8(50 + y*80), B: uint8(x + y*3), A: 255})
		}
	}
	var pixels bytes.Buffer
	require.NoError(t, png.Encode(&pixels, frame))
	// Each row describes the source pixel indices in the exported frame.
	orders := [][]int{{0, 1, 2, 3, 4, 5}, {2, 1, 0, 5, 4, 3}, {5, 4, 3, 2, 1, 0}, {3, 4, 5, 0, 1, 2}, {0, 3, 1, 4, 2, 5}, {3, 0, 4, 1, 5, 2}, {5, 2, 4, 1, 3, 0}, {2, 5, 1, 4, 0, 3}}
	for orientation := 1; orientation <= 8; orientation++ {
		var source bytes.Buffer
		source.Write(pixels.Bytes()[:33])
		writePhotoPNGChunk(&source, "eXIf", syntheticTIFF(42, []syntheticTIFFEntry{tiffShort(0x0112, uint16(orientation))}, nil))
		source.Write(pixels.Bytes()[33:])
		for _, rotation := range []int{0, 90, 180, 270} {
			t.Run(fmt.Sprintf("%d/%d", orientation, rotation), func(t *testing.T) {
				input := photoRenderInput(source.Bytes(), "image/png")
				input.Authored.Rotation = rotation
				out, receipt, err := RenderPhotoExport(t.Context(), bytes.NewReader(source.Bytes()), input, bundle.PhotoRenderProfile{Format: "png", Quality: 90})
				require.NoError(t, err)
				decoded, err := png.Decode(bytes.NewReader(out))
				require.NoError(t, err)
				w, h := 3, 2
				if orientation >= 5 {
					w, h = 2, 3
				}
				order := append([]int(nil), orders[orientation-1]...)
				for turn := 0; turn < rotation/90; turn++ {
					next := make([]int, len(order))
					for y := range w {
						for x := range h {
							next[y*h+x] = order[(h-1-x)*w+y]
						}
					}
					order = next
					w, h = h, w
				}
				assert.Equal(t, w, receipt.Width)
				assert.Equal(t, h, receipt.Height)
				for index, sourceIndex := range order {
					assert.Equal(t, color.NRGBAModel.Convert(frame.At(sourceIndex%3, sourceIndex/3)), color.NRGBAModel.Convert(decoded.At(index%w, index/w)))
				}
			})
		}
	}
}

func TestPhotoExportSizeQualityAndMetadataOff(t *testing.T) {
	t.Parallel()
	frame := image.NewNRGBA(image.Rect(0, 0, 5000, 2))
	for x := range 5000 {
		frame.Set(x, 0, color.NRGBA{R: uint8(x % 255), G: uint8(x * 7 % 255), B: uint8(x * 19 % 255), A: 255})
	}
	var source bytes.Buffer
	require.NoError(t, png.Encode(&source, frame))
	input := photoRenderInput(source.Bytes(), "image/png")
	for _, test := range []struct{ edge, want int }{{0, 5000}, {6000, 5000}, {1000, 1000}} {
		out, receipt, err := RenderPhotoExport(t.Context(), bytes.NewReader(source.Bytes()), input, bundle.PhotoRenderProfile{Format: "png", Quality: 90, LongEdge: test.edge})
		require.NoError(t, err)
		assert.Equal(t, test.want, receipt.Width)
		exif, xmp, err := photoSourcePackets(out)
		require.NoError(t, err)
		assert.Empty(t, exif)
		assert.Empty(t, xmp)
	}
	var outputs [][]byte
	for _, quality := range []int{10, 95} {
		out, _, err := RenderPhotoExport(t.Context(), bytes.NewReader(source.Bytes()), input, bundle.PhotoRenderProfile{Format: "jpeg", Quality: quality})
		require.NoError(t, err)
		_, err = jpeg.Decode(bytes.NewReader(out))
		require.NoError(t, err)
		outputs = append(outputs, out)
	}
	assert.Less(t, len(outputs[0]), len(outputs[1]))
}

func TestPhotoExportMetadataPreservesClearsAndRemovesGPSPayloads(t *testing.T) {
	t.Parallel()
	// The GPS directory is unreachable after stripping, and its marker must disappear too.
	exif := syntheticTIFF(42, []syntheticTIFFEntry{tiffShort(0x0112, 6), tiffASCII(0x010f, "Synthetic Camera"), tiffLong(0x8825, 0)}, []syntheticTIFFEntry{tiffLong(0xa002, 6000), tiffLong(0xa003, 4000), tiffRational(0x829a, 1, 250, false)})
	r, _ := newExifReader(exif)
	count, _ := r.u16(8)
	gpsPointer := 0
	for index := range count {
		base := 10 + int(index)*12
		if r.order.Uint16(exif[base:]) == 0x8825 {
			gpsPointer = base + 8
		}
	}
	require.NotZero(t, gpsPointer)
	gpsOffset := len(exif)
	exif = append(exif, make([]byte, 18+len("GPS-PAYLOAD")+1)...)
	binary.LittleEndian.PutUint32(exif[gpsPointer:], uint32(gpsOffset))
	external := gpsOffset + 18
	writeSyntheticTIFFIFD(exif, gpsOffset, []syntheticTIFFEntry{tiffASCII(0x001b, "GPS-PAYLOAD")}, &external)
	packet := []byte(photoSidecarHeader + ` xmp:Rating="5" dc:description="Old caption" xmlns:exif="http://ns.adobe.com/exif/1.0/" exif:GPSLatitude="12,34N" xmlns:keep="https://example.org/photo/" keep:Lens="Synthetic lens"><exif:Orientation>6</exif:Orientation>` + photoSidecarFooter)
	input := store.PhotoExportInput{Authored: store.PhotoAuthored{Flag: "reject"}, Keywords: []string{"landscape", "reviewed"}}
	receipt := bundle.PhotoRenderReceipt{Profile: bundle.PhotoRenderProfile{RemoveGPS: true}, Width: 2, Height: 3}
	merged, err := mergePhotoXMP(t.Context(), packet, input, receipt)
	require.NoError(t, err)
	assert.NotContains(t, string(merged), "GPSLatitude")
	assert.Contains(t, string(merged), "Synthetic lens")
	assert.NotContains(t, string(merged), "Old caption")
	values, err := ReadPhotoSidecar(t.Context(), merged)
	require.NoError(t, err)
	assert.Equal(t, input.Authored, values)
	assert.Contains(t, string(merged), "landscape")
	assert.Contains(t, string(merged), "reviewed")
	for _, strip := range []bool{false, true} {
		out, err := rewritePhotoEXIF(exif, 2, 3, strip)
		require.NoError(t, err)
		reader, ok := newExifReader(out)
		require.True(t, ok)
		root := reader.entries(reader.u32(4))
		assert.Equal(t, "Synthetic Camera", exifASCII(root[0x010f]))
		assert.Equal(t, uint16(1), reader.order.Uint16(root[0x0112]))
		details := reader.entries(reader.order.Uint32(root[0x8769]))
		assert.Equal(t, uint32(2), reader.order.Uint32(details[0xa002]))
		assert.Equal(t, !strip, strings.Contains(string(out), "GPS-PAYLOAD"))
	}
	for _, format := range []string{"jpeg", "png"} {
		data := syntheticJPEGSegment(t, mediatest.JPEG(3, 2, color.White), 0xe1, append([]byte("Exif\x00\x00"), exif...))
		data = syntheticJPEGSegment(t, data, 0xe1, append([]byte(photoXMPJPEGPrefix), packet...))
		in := photoRenderInput(data, "image/jpeg")
		in.Authored = input.Authored
		in.Keywords = input.Keywords
		out, receipt, err := RenderPhotoExport(t.Context(), bytes.NewReader(data), in, bundle.PhotoRenderProfile{Format: format, Quality: 90, IncludeMetadata: true, RemoveGPS: true})
		require.NoError(t, err)
		assert.Equal(t, 2, receipt.Width)
		outExif, outXMP, err := photoSourcePackets(out)
		require.NoError(t, err)
		assert.NotContains(t, string(outExif), "GPS-PAYLOAD")
		actual, err := ReadPhotoSidecar(t.Context(), outXMP)
		require.NoError(t, err)
		assert.Equal(t, in.Authored, actual)
	}
}

func TestPhotoExportRAWAndMalformedMetadata(t *testing.T) {
	t.Parallel()
	preview := mediatest.JPEG(3, 2, color.White)
	data := syntheticRAWPreviewTIFF(6, preview)
	in := photoRenderInput(data, "image/x-adobe-dng")
	out, receipt, err := RenderPhotoExport(t.Context(), bytes.NewReader(data), in, bundle.PhotoRenderProfile{Format: "png", Quality: 90, IncludeMetadata: true})
	require.NoError(t, err)
	assert.True(t, receipt.EmbeddedPreview)
	assert.Equal(t, 2, receipt.Width)
	assert.Equal(t, 3, receipt.Height)
	exif, _, err := photoSourcePackets(out)
	require.NoError(t, err)
	reader, ok := newExifReader(exif)
	require.True(t, ok)
	assert.Equal(t, uint16(1), reader.order.Uint16(reader.entries(reader.u32(4))[0x0112]))
	missing := syntheticRAWPreviewTIFF(1)
	_, _, err = RenderPhotoExport(t.Context(), bytes.NewReader(missing), photoRenderInput(missing, "image/x-adobe-dng"), bundle.PhotoRenderProfile{Format: "jpeg", Quality: 90})
	require.ErrorIs(t, err, bundle.ErrUnavailable)
	bad := syntheticJPEGSegment(t, preview, 0xe1, append([]byte(photoXMPJPEGPrefix), []byte(`<x:xmpmeta xmlns:x="adobe:ns:meta/"><broken>`)...))
	input := photoRenderInput(bad, "image/jpeg")
	_, _, err = RenderPhotoExport(t.Context(), bytes.NewReader(bad), input, bundle.PhotoRenderProfile{Format: "jpeg", Quality: 90, IncludeMetadata: true})
	require.Error(t, err)
	_, _, err = RenderPhotoExport(t.Context(), bytes.NewReader(bad), input, bundle.PhotoRenderProfile{Format: "jpeg", Quality: 90})
	require.NoError(t, err)
	input.Member.SHA256 = strings.Repeat("0", 64)
	_, _, err = RenderPhotoExport(t.Context(), bytes.NewReader(bad), input, bundle.PhotoRenderProfile{Format: "jpeg", Quality: 90})
	require.Error(t, err)
}
