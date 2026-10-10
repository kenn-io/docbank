package processing

import (
	"bytes"
	"context"
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

func TestPhotoExportTrailingEXIFCreditsNamespacesAndICC(t *testing.T) {
	t.Parallel()
	var source bytes.Buffer
	require.NoError(t, png.Encode(&source, image.NewNRGBA(image.Rect(0, 0, 3, 2))))
	var late bytes.Buffer
	late.Write(source.Bytes()[:source.Len()-12])
	writePhotoPNGChunk(&late, "eXIf", syntheticTIFF(42, []syntheticTIFFEntry{tiffShort(0x0112, 6)}, nil))
	late.Write(source.Bytes()[source.Len()-12:])
	out, receipt, err := RenderPhotoExport(t.Context(), bytes.NewReader(late.Bytes()), photoRenderInput(late.Bytes(), "image/png"), bundle.PhotoRenderProfile{Format: "png", Quality: 90, IncludeMetadata: true})
	require.NoError(t, err)
	assert.Equal(t, 2, receipt.Width)
	decoded, err := png.Decode(bytes.NewReader(out))
	require.NoError(t, err)
	assert.Equal(t, image.Rect(0, 0, 2, 3), decoded.Bounds())
	packet := []byte(photoSidecarHeader + ` dc:creator="Embedded credit" dc:rights="Embedded rights" dc:description="Embedded caption" xmlns:keep="https://example.org/photo/" keep:Orientation="77"><keep:ImageWidth>12345</keep:ImageWidth>` + photoSidecarFooter)
	input := store.PhotoExportInput{Authored: store.PhotoAuthored{Rating: 4, Confirmed: store.PhotoConfirmedRating}}
	merged, err := mergePhotoXMP(t.Context(), packet, input, receipt)
	require.NoError(t, err)
	for _, value := range []string{"Embedded credit", "Embedded rights", "Embedded caption", "77", "12345"} {
		assert.Contains(t, string(merged), value)
	}
	input.Authored.Confirmed |= store.PhotoConfirmedCaption
	merged, err = mergePhotoXMP(t.Context(), packet, input, receipt)
	require.NoError(t, err)
	assert.NotContains(t, string(merged), "Embedded caption")
	assert.Contains(t, string(merged), "Embedded credit")
	profile := syntheticPhotoICC()

	jpegSource := syntheticJPEGSegment(t, mediatest.JPEG(3, 2, color.White), 0xe2, append([]byte("ICC_PROFILE\x00\x01\x01"), profile...))
	for _, format := range []string{"jpeg", "png"} {
		out, _, err := RenderPhotoExport(t.Context(), bytes.NewReader(jpegSource), photoRenderInput(jpegSource, "image/jpeg"), bundle.PhotoRenderProfile{Format: format, Quality: 90})
		require.NoError(t, err)
		packets, err := photoSourcePackets(t.Context(), out, true)
		require.NoError(t, err)
		assert.Equal(t, profile, packets.icc)
		assert.Empty(t, packets.xmp)
		assert.Empty(t, packets.exif)
	}
	budget := photoExportBudget{pixels: 5}
	_, _, err = renderPhotoExport(t.Context(), bytes.NewReader(jpegSource), photoRenderInput(jpegSource, "image/jpeg"), bundle.PhotoRenderProfile{Format: "png", Quality: 90}, &budget)
	require.ErrorIs(t, err, bundle.ErrLimit)
}

func BenchmarkPhotoExport24MP(b *testing.B) {
	frame := image.NewNRGBA(image.Rect(0, 0, 6000, 4000))
	var seed uint32 = 1
	for i := 0; i < len(frame.Pix); i += 4 {
		seed = seed*1664525 + 1013904223
		frame.Pix[i], frame.Pix[i+1], frame.Pix[i+2], frame.Pix[i+3] = byte(seed>>24), byte(seed>>16), byte(seed>>8), 255
	}
	var source bytes.Buffer
	if err := jpeg.Encode(&source, frame, &jpeg.Options{Quality: 90}); err != nil {
		b.Fatal(err)
	}
	input := photoRenderInput(source.Bytes(), "image/jpeg")
	for _, setting := range []struct {
		name, format string
		edge         int
	}{{"JPEG_original", "jpeg", 0}, {"JPEG_2048", "jpeg", 2048}, {"PNG_original", "png", 0}} {
		b.Run(setting.name, func(b *testing.B) {
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				if _, _, err := RenderPhotoExport(context.Background(), bytes.NewReader(source.Bytes()), input, bundle.PhotoRenderProfile{Format: setting.format, Quality: 90, LongEdge: setting.edge}); err != nil {
					b.Fatal(err)
				}
			}
			b.StopTimer()
			b.ReportMetric(b.Elapsed().Seconds()*bundle.MaxPhotoExportMembers/float64(b.N), "s/16photos")
		})
	}
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
		packets, err := photoSourcePackets(t.Context(), out, true)
		require.NoError(t, err)
		assert.Empty(t, packets.exif)
		assert.Empty(t, packets.xmp)
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
	input := store.PhotoExportInput{Authored: store.PhotoAuthored{Confirmed: store.PhotoConfirmedAll, Flag: "reject"}, Keywords: []string{"landscape", "reviewed"}}
	receipt := bundle.PhotoRenderReceipt{Profile: bundle.PhotoRenderProfile{RemoveGPS: true}, Width: 2, Height: 3}
	merged, err := mergePhotoXMP(t.Context(), packet, input, receipt)
	require.NoError(t, err)
	assert.NotContains(t, string(merged), "GPSLatitude")
	assert.Contains(t, string(merged), "Synthetic lens")
	assert.NotContains(t, string(merged), "Old caption")
	values, err := ReadPhotoSidecar(t.Context(), merged)
	require.NoError(t, err)
	expected := input.Authored
	assert.Equal(t, expected, values)
	assert.Contains(t, string(merged), "landscape")
	assert.Contains(t, string(merged), "reviewed")
	for _, strip := range []bool{false, true} {
		out, err := rewritePhotoEXIF(exif, 2, 3, strip, input.Authored)
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
		packets, err := photoSourcePackets(t.Context(), out, true)
		require.NoError(t, err)
		assert.NotContains(t, string(packets.exif), "GPS-PAYLOAD")
		actual, err := ReadPhotoSidecar(t.Context(), packets.xmp)
		require.NoError(t, err)
		expected := in.Authored
		assert.Equal(t, expected, actual)
	}
}

func TestPhotoExportConfirmedFlagClearsLegacyReject(t *testing.T) {
	t.Parallel()
	for _, flag := range []string{"pick", ""} {
		for _, property := range []string{` xmp:Rating="-1">`, `><xmp:Rating>-<!--split-->1</xmp:Rating>`, `><xmp:Rating><rdf:value>-1</rdf:value></xmp:Rating>`, ` xmp:Rating="4">`, `><xmp:Rating>4</xmp:Rating>`, `><xmp:Rating><rdf:value>4</rdf:value></xmp:Rating>`} {
			for _, format := range []string{"jpeg", "png"} {
				packet := []byte(photoSidecarHeader + property + photoSidecarFooter)
				data := syntheticJPEGSegment(t, mediatest.JPEG(3, 2, color.White), 0xe1, append([]byte(photoXMPJPEGPrefix), packet...))
				input := photoRenderInput(data, "image/jpeg")
				input.Authored = store.PhotoAuthored{Confirmed: store.PhotoConfirmedFlag, Flag: flag}
				out, _, err := RenderPhotoExport(t.Context(), bytes.NewReader(data), input, bundle.PhotoRenderProfile{Format: format, Quality: 90, IncludeMetadata: true})
				require.NoError(t, err)
				packets, err := photoSourcePackets(t.Context(), out, true)
				require.NoError(t, err)
				actual, err := ReadPhotoSidecar(t.Context(), packets.xmp)
				require.NoError(t, err)
				require.Equal(t, flag, actual.Flag)
				if strings.Contains(property, "4") {
					require.Equal(t, 4, actual.Rating)
				} else {
					require.Zero(t, actual.Rating)
				}
			}
		}
	}
}

func TestPhotoExportFailedDecodePreservesPixelBudget(t *testing.T) {
	preview := mediatest.JPEG(3, 2, color.White)
	sos := bytes.Index(preview, []byte{0xff, 0xda})
	require.Positive(t, sos)
	broken := preview[:sos+2+int(binary.BigEndian.Uint16(preview[sos+2:]))]
	config, _, err := image.DecodeConfig(bytes.NewReader(broken))
	require.NoError(t, err)
	require.Equal(t, 3, config.Width)
	budget := &photoExportBudget{pixels: 6}
	_, _, err = decodePhotoExport(t.Context(), bytes.NewReader(broken), "jpeg", 1, photoPackets{}, budget)
	require.Error(t, err)
	require.EqualValues(t, 6, budget.pixels)
	_, _, err = decodePhotoExport(t.Context(), bytes.NewReader(preview), "jpeg", 1, photoPackets{}, budget)
	require.NoError(t, err)
	require.Zero(t, budget.pixels)
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
	packets, err := photoSourcePackets(t.Context(), out, true)
	require.NoError(t, err)
	reader, ok := newExifReader(packets.exif)
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

func syntheticPhotoICC() []byte {
	tags := []string{"wtpt", "rXYZ", "gXYZ", "bXYZ", "rTRC", "gTRC", "bTRC"}
	profile := make([]byte, 132+12*len(tags))
	copy(profile[12:16], "mntr")
	copy(profile[16:20], "RGB ")
	copy(profile[20:24], "XYZ ")
	copy(profile[36:40], "acsp")
	binary.BigEndian.PutUint32(profile[128:], uint32(len(tags)))
	for i, tag := range tags {
		entry := profile[132+i*12:]
		copy(entry, tag)
		binary.BigEndian.PutUint32(entry[4:], uint32(len(profile)))
		var value []byte
		if i < 4 {
			value = make([]byte, 20)
			copy(value, "XYZ ")
			for j, n := range [3]uint32{0xf6d6, 0x10000, 0xd32d} {
				binary.BigEndian.PutUint32(value[8+j*4:], n)
			}
		} else {
			value = make([]byte, 16)
			copy(value, "curv")
			binary.BigEndian.PutUint32(value[8:], 1)
			binary.BigEndian.PutUint16(value[12:], 563)
		}
		binary.BigEndian.PutUint32(entry[8:], uint32(len(value)))
		profile = append(profile, value...)
	}
	binary.BigEndian.PutUint32(profile, uint32(len(profile)))
	return profile
}
func TestPhotoExportPNGPixelChunksDoNotConsumeMetadataBudget(t *testing.T) {
	frame := image.NewNRGBA(image.Rect(0, 0, 65, 64))
	seed := uint32(1)
	for i := range frame.Pix {
		seed = seed*1664525 + 1013904223
		frame.Pix[i] = byte(seed >> 24)
	}
	var encoded, source bytes.Buffer
	require.NoError(t, png.Encode(&encoded, frame))
	source.Write(encoded.Bytes()[:8])
	chunks := 0
	for data := encoded.Bytes()[8:]; len(data) > 0; {
		n := int(binary.BigEndian.Uint32(data))
		kind := string(data[4:8])
		payload := data[8 : 8+n]
		if kind == "IDAT" {
			for len(payload) > 0 {
				size := min(8, len(payload))
				writePhotoPNGChunk(&source, kind, payload[:size])
				payload = payload[size:]
				chunks++
			}
		} else {
			source.Write(data[:n+12])
		}
		data = data[n+12:]
	}
	require.Greater(t, chunks, 1024)
	input := photoRenderInput(source.Bytes(), "image/png")
	output, receipt, err := RenderPhotoExport(t.Context(), bytes.NewReader(source.Bytes()), input, bundle.PhotoRenderProfile{Format: "png", Quality: 90})
	require.NoError(t, err)
	require.Equal(t, 65, receipt.Width)
	require.NotEmpty(t, output)
	preview, err := ProduceVisualPreview(t.Context(), bytes.NewReader(source.Bytes()), VisualPreviewTarget{SourceSHA256: input.Member.SHA256, Size: input.Member.Size, MediaType: "image/png"})
	require.NoError(t, err)
	require.NotNil(t, preview.Preview.Output)
	require.Equal(t, 65, preview.Preview.Output.Width)
	for _, category := range []error{bundle.ErrLimit, bundle.ErrConflict, bundle.ErrUnavailable} {
		err := photoExportError(input, category)
		require.ErrorIs(t, err, category)
		require.Contains(t, err.Error(), "photo 1")
	}
	exif := syntheticTIFF(42, []syntheticTIFFEntry{{tag: 0x010e, kind: 2}}, nil)
	_, err = rewritePhotoEXIF(exif, 65, 64, false, store.PhotoAuthored{})
	require.NoError(t, err)
}
