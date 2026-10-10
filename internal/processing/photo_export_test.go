package processing

import (
	"bytes"
	"compress/zlib"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"io"
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

func TestPhotoExportTrailingEXIFAndICC(t *testing.T) {
	t.Parallel()
	var source bytes.Buffer
	require.NoError(t, png.Encode(&source, image.NewNRGBA(image.Rect(0, 0, 3, 2))))
	var late bytes.Buffer
	late.Write(source.Bytes()[:source.Len()-12])
	writePhotoPNGChunk(&late, "eXIf", syntheticTIFF(42, []syntheticTIFFEntry{tiffShort(0x0112, 6)}, nil))
	late.Write(source.Bytes()[source.Len()-12:])
	out, receipt, err := renderPhotoExport(t.Context(), late.Bytes(), photoRenderInput(late.Bytes(), "image/png"), bundle.PhotoRenderProfile{Format: "png", Quality: 90, IncludeMetadata: true}, nil)
	require.NoError(t, err)
	assert.Equal(t, 2, receipt.Width)
	decoded, err := png.Decode(bytes.NewReader(out))
	require.NoError(t, err)
	assert.Equal(t, image.Rect(0, 0, 2, 3), decoded.Bounds())
	packet := []byte(photoSidecarHeader + ` dc:creator="Embedded credit" dc:rights="Embedded rights" dc:description="Embedded caption" dc:subject="Embedded keyword" xmlns:keep="https://example.org/photo/" keep:Orientation="77"><keep:ImageWidth>12345</keep:ImageWidth>` + photoSidecarFooter)
	var compressed, tagged bytes.Buffer
	writer := zlib.NewWriter(&compressed)
	_, err = writer.Write(packet)
	require.NoError(t, err)
	require.NoError(t, writer.Close())
	tagged.Write(source.Bytes()[:source.Len()-12])
	writePhotoPNGChunk(&tagged, "iTXt", append([]byte(photoXMPPNGKeyword+"\x00\x01\x00\x00\x00"), compressed.Bytes()...))
	tagged.Write(source.Bytes()[source.Len()-12:])
	compressedPackets, err := photoSourcePackets(t.Context(), tagged.Bytes(), true)
	require.NoError(t, err)
	require.Equal(t, packet, compressedPackets.xmp)
	profile := syntheticPhotoICC()

	jpegSource := syntheticJPEGSegment(t, mediatest.JPEG(3, 2, color.White), 0xe2, append([]byte("ICC_PROFILE\x00\x01\x01"), profile...))
	for _, format := range []string{"jpeg", "png"} {
		out, _, err := renderPhotoExport(t.Context(), jpegSource, photoRenderInput(jpegSource, "image/jpeg"), bundle.PhotoRenderProfile{Format: format, Quality: 90}, nil)
		require.NoError(t, err)
		packets, err := photoSourcePackets(t.Context(), out, true)
		require.NoError(t, err)
		assert.Equal(t, profile, packets.icc)
	}
	exifSource := syntheticJPEGSegment(t, jpegSource, 0xe1, append([]byte("Exif\x00\x00"), syntheticTIFF(42, []syntheticTIFFEntry{tiffShort(0x0112, 1)}, nil)...))
	out, _, err = renderPhotoExport(t.Context(), exifSource, photoRenderInput(exifSource, "image/jpeg"), bundle.PhotoRenderProfile{Format: "jpeg", Quality: 90, IncludeMetadata: true}, nil)
	require.NoError(t, err)
	require.Equal(t, []byte{0xff, 0xe1}, out[2:4])
	require.Equal(t, "Exif\x00\x00", string(out[6:12]))
	packets, err := photoSourcePackets(t.Context(), out, true)
	require.NoError(t, err)
	require.Equal(t, profile, packets.icc)
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
				if _, _, err := renderPhotoExport(context.Background(), source.Bytes(), input, bundle.PhotoRenderProfile{Format: setting.format, Quality: 90, LongEdge: setting.edge}, nil); err != nil {
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
				out, receipt, err := renderPhotoExport(t.Context(), source.Bytes(), input, bundle.PhotoRenderProfile{Format: "png", Quality: 90}, nil)
				require.NoError(t, err)
				decoded, err := png.Decode(bytes.NewReader(out))
				require.NoError(t, err)
				w, h := 3, 2
				if orientation >= 5 {
					w, h = 2, 3
				}
				order := append([]int(nil), orders[orientation-1]...)
				for range rotation / 90 {
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

func TestPhotoExportSizeAndQuality(t *testing.T) {
	t.Parallel()
	frame := image.NewNRGBA(image.Rect(0, 0, 5000, 2))
	for x := range 5000 {
		frame.Set(x, 0, color.NRGBA{R: uint8(x % 255), G: uint8(x * 7 % 255), B: uint8(x * 19 % 255), A: 255})
	}
	var source bytes.Buffer
	require.NoError(t, png.Encode(&source, frame))
	input := photoRenderInput(source.Bytes(), "image/png")
	for _, test := range []struct{ edge, want int }{{0, 5000}, {6000, 5000}, {1000, 1000}} {
		_, receipt, err := renderPhotoExport(t.Context(), source.Bytes(), input, bundle.PhotoRenderProfile{Format: "png", Quality: 90, LongEdge: test.edge}, nil)
		require.NoError(t, err)
		assert.Equal(t, test.want, receipt.Width)
	}
	var outputs [][]byte
	for _, quality := range []int{10, 95} {
		out, _, err := renderPhotoExport(t.Context(), source.Bytes(), input, bundle.PhotoRenderProfile{Format: "jpeg", Quality: quality}, nil)
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
	packet := []byte("\xef\xbb\xbf" + photoSidecarHeader + ` xmp:Rating="5" dc:description="Old caption" xmlns:exif="http://ns.adobe.com/exif/1.0/" exif:GPSLatitude="12,34N" xmlns:keep="https://example.org/photo/" keep:Lens="Synthetic lens"><exif:Orientation>6</exif:Orientation>` + photoSidecarFooter)
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
	creditPacket := []byte(photoSidecarHeader + ` dc:creator="Embedded credit" dc:rights="Embedded rights" dc:description="Embedded caption" dc:subject="Embedded keyword" xmlns:keep="https://example.org/photo/" keep:Orientation="77"><keep:ImageWidth>12345</keep:ImageWidth>` + photoSidecarFooter)
	creditInput := store.PhotoExportInput{Authored: store.PhotoAuthored{Rating: 4, Confirmed: store.PhotoConfirmedRating}}
	merged, err = mergePhotoXMP(t.Context(), creditPacket, creditInput, receipt)
	require.NoError(t, err)
	for _, value := range []string{"Embedded credit", "Embedded rights", "Embedded caption", "77", "12345"} {
		assert.Contains(t, string(merged), value)
	}
	assert.NotContains(t, string(merged), "Embedded keyword")
	assert.Contains(t, string(merged), "Bag")
	creditInput.Authored.Confirmed |= store.PhotoConfirmedCaption
	merged, err = mergePhotoXMP(t.Context(), creditPacket, creditInput, receipt)
	require.NoError(t, err)
	assert.NotContains(t, string(merged), "Embedded caption")
	assert.Contains(t, string(merged), "Embedded credit")
	largeXMP := []byte(photoSidecarHeader + ` xmlns:keep="https://example.org/photo/" xmlns:lr="http://ns.adobe.com/lightroom/1.0/" xmlns:pdf="http://ns.adobe.com/pdf/1.3/" lr:hierarchicalSubject="Removed attribute tag" pdf:Keywords="Removed PDF tag"><lr:hierarchicalSubject><rdf:Bag><rdf:li>Removed collection tag</rdf:li></rdf:Bag></lr:hierarchicalSubject><pdf:Keywords>Removed scalar tag</pdf:Keywords>` + strings.Repeat(`<keep:History rdf:parseType="Resource"><keep:Step keep:Type="keep:Type">kept &amp; safe</keep:Step></keep:History>`, 360) + `<keep:Scoped xmlns:keep="https://example.org/scoped/" keep:Type="keep:Type"><keep:Value>keep:Literal</keep:Value></keep:Scoped><Plain xmlns="https://example.org/default/" xmlns:q="https://example.org/default/" q:Value="q:Literal"><Child/></Plain><keep:Reset xmlns=""><Child/></keep:Reset>` + photoSidecarFooter)
	require.Greater(t, len(largeXMP), 40000)
	merged, err = mergePhotoXMP(t.Context(), largeXMP, input, receipt)
	require.NoError(t, err)
	require.Less(t, len(merged), len(largeXMP)+1000)
	require.NotContains(t, string(merged), "Removed")
	counts := map[xml.Name]int{}
	decoder := xml.NewDecoder(bytes.NewReader(merged))
	for {
		token, err := decoder.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		require.NoError(t, err)
		if element, ok := token.(xml.StartElement); ok {
			counts[element.Name]++
			for _, attr := range element.Attr {
				if attr.Name.Local == "Type" {
					require.Equal(t, element.Name.Space, attr.Name.Space)
					require.Equal(t, "keep:Type", attr.Value)
				}
			}
		}
	}
	require.Equal(t, 360, counts[xml.Name{Space: "https://example.org/photo/", Local: "Step"}])
	require.Equal(t, 1, counts[xml.Name{Space: "https://example.org/scoped/", Local: "Value"}])
	require.Equal(t, 1, counts[xml.Name{Space: "https://example.org/default/", Local: "Child"}])
	require.Equal(t, 1, counts[xml.Name{Local: "Child"}])
	require.Equal(t, 1, counts[xml.Name{Space: xmpDublinCoreNamespace, Local: "subject"}])
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
	pixels := mediatest.JPEG(3, 2, color.White)
	render := func(exif, packet []byte, authored store.PhotoAuthored, keywords []string, format string, removeGPS bool) (photoPackets, bundle.PhotoRenderReceipt) {
		t.Helper()
		data := pixels
		if len(exif) > 0 {
			data = syntheticJPEGSegment(t, data, 0xe1, append([]byte("Exif\x00\x00"), exif...))
		}
		if len(packet) > 0 {
			data = syntheticJPEGSegment(t, data, 0xe1, append([]byte(photoXMPJPEGPrefix), packet...))
		}
		in := photoRenderInput(data, "image/jpeg")
		in.Authored, in.Keywords = authored, keywords
		out, receipt, err := renderPhotoExport(t.Context(), data, in, bundle.PhotoRenderProfile{Format: format, Quality: 90, IncludeMetadata: true, RemoveGPS: removeGPS}, nil)
		require.NoError(t, err)
		packets, err := photoSourcePackets(t.Context(), out, true)
		require.NoError(t, err)
		return packets, receipt
	}
	for _, format := range []string{"jpeg", "png"} {
		packets, _ := render(exif, packet, input.Authored, input.Keywords, format, true)
		require.NotEmpty(t, packets.exif)
		require.NotEmpty(t, packets.xmp)
	}
	for _, flag := range []string{"pick", "", "reject"} {
		for _, property := range []string{` xmp:Rating="-1">`, `><xmp:Rating>-<!--split-->1</xmp:Rating>`, `><xmp:Rating><rdf:value>-1</rdf:value></xmp:Rating>`, ` xmp:Rating="4">`, `><xmp:Rating>4</xmp:Rating>`, `><xmp:Rating><rdf:value>4</rdf:value></xmp:Rating>`, `><xmp:Rating xmlns:exif="http://ns.adobe.com/exif/1.0/" exif:GPSLatitude="12,30N">4</xmp:Rating>`, `><xmp:Rating xmlns:exif="http://ns.adobe.com/exif/1.0/"><rdf:value>4</rdf:value><exif:GPSLatitude>12,30N</exif:GPSLatitude></xmp:Rating>`, `><xmp:Rating xmlns:exif="http://ns.adobe.com/exif/1.0/"><rdf:value>-1</rdf:value><exif:GPSLatitude>12,30N</exif:GPSLatitude></xmp:Rating>`} {
			for _, spelling := range []string{"-1", "-01"} {
				property := strings.ReplaceAll(property, "-1", spelling)
				property = strings.ReplaceAll(property, "-<!--split-->1", "-<!--split-->"+spelling[1:])
				for _, confirmed := range []store.PhotoAuthoredFields{0, store.PhotoConfirmedFlag, store.PhotoConfirmedRating, store.PhotoConfirmedFlag | store.PhotoConfirmedRating} {
					packet := []byte(photoSidecarHeader + property + photoSidecarFooter)
					merged, err := mergePhotoXMP(t.Context(), packet, store.PhotoExportInput{Authored: store.PhotoAuthored{Confirmed: confirmed, Rating: 3, Flag: flag}}, receipt)
					require.NoError(t, err)
					require.NotContains(t, string(merged), "GPSLatitude")
					actual, err := ReadPhotoSidecar(t.Context(), merged)
					require.NoError(t, err)
					wantFlag, wantRating := "", 0
					if strings.Contains(property, "4") {
						wantRating = 4
					} else {
						wantFlag = "reject"
					}
					if confirmed&store.PhotoConfirmedFlag != 0 {
						wantFlag = flag
					}
					if confirmed&store.PhotoConfirmedRating != 0 {
						wantRating = 3
					}
					require.Equal(t, wantFlag, actual.Flag)
					require.Equal(t, wantRating, actual.Rating)
				}
			}
		}
	}
	for _, property := range []string{` ts:Pick="pick"><xmp:Rating>-01</xmp:Rating>`, `><ts:Pick>pick</ts:Pick><xmp:Rating>-01</xmp:Rating>`, `><xmp:Rating>-01</xmp:Rating><ts:Pick>pick</ts:Pick>`, ` ts:Pick="pick"><xmp:Rating>4</xmp:Rating>`} {
		packet := []byte(photoSidecarHeader + property + photoSidecarFooter)
		merged, err := mergePhotoXMP(t.Context(), packet, store.PhotoExportInput{Authored: store.PhotoAuthored{Confirmed: store.PhotoConfirmedRating, Rating: 3}}, receipt)
		require.NoError(t, err)
		actual, err := ReadPhotoSidecar(t.Context(), merged)
		require.NoError(t, err)
		flag := "reject"
		if strings.Contains(property, ">4<") {
			flag = "pick"
		}
		require.Equal(t, flag, actual.Flag)
		require.Equal(t, 3, actual.Rating)
	}
	var value []byte
	for _, ch := range "Synthetic embedded credit" {
		value = append(value, byte(ch), 0)
	}
	value = append(value, 0, 0)
	var entries []syntheticTIFFEntry
	for _, tag := range []uint16{0x9c9b, 0x9c9c, 0x9c9d, 0x9c9e, 0x9c9f, 0x9c90} {
		entries = append(entries, syntheticTIFFEntry{tag: tag, kind: 1, value: value})
	}
	entries = append(entries, tiffShort(0x4746, 5), tiffShort(0x4749, 99), syntheticTIFFEntry{tag: 0x83bb, kind: 7, value: append([]byte{0x1c, 2, 25, 0, 16}, []byte("Embedded keyword")...)})
	aliasEXIF := syntheticTIFF(42, entries, nil)
	for _, confirmed := range []store.PhotoAuthoredFields{0, store.PhotoConfirmedCaption | store.PhotoConfirmedCreator | store.PhotoConfirmedRating} {
		for _, format := range []string{"jpeg", "png"} {
			packets, _ := render(aliasEXIF, nil, store.PhotoAuthored{Confirmed: confirmed}, nil, format, false)
			r, ok := newExifReader(packets.exif)
			require.True(t, ok)
			root := r.entries(r.u32(4))
			require.NotContains(t, root, uint16(0x9c9e))
			require.NotContains(t, root, uint16(0x83bb))
			require.NotContains(t, string(packets.exif), "Embedded keyword")
			for _, tag := range []uint16{0x4746, 0x4749} {
				if confirmed == 0 {
					require.Contains(t, root, tag)
				} else {
					require.NotContains(t, root, tag)
				}
			}
			if confirmed&store.PhotoConfirmedRating != 0 {
				actual, err := ReadPhotoSidecar(t.Context(), packets.xmp)
				require.NoError(t, err)
				require.Zero(t, actual.Rating)
			}
			require.Equal(t, value, root[0x9c90])
			for _, tag := range []uint16{0x9c9b, 0x9c9c, 0x9c9d, 0x9c9f} {
				if confirmed == 0 {
					require.Equal(t, value, root[tag])
				} else {
					require.NotContains(t, root, tag)
				}
			}
		}
	}
	_, err = rewritePhotoEXIF(syntheticTIFF(42, []syntheticTIFFEntry{{tag: 0x010e, kind: 2}}, nil), 65, 64, false, store.PhotoAuthored{})
	require.NoError(t, err)
}

func TestPhotoExportFailedDecodePreservesPixelBudget(t *testing.T) {
	t.Parallel()
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
	budget.pixels = 5
	_, _, err = renderPhotoExport(t.Context(), preview, photoRenderInput(preview, "image/jpeg"), bundle.PhotoRenderProfile{Format: "png"}, budget)
	require.ErrorIs(t, err, bundle.ErrLimit)
}

func TestPhotoExportRAWAndMalformedMetadata(t *testing.T) {
	t.Parallel()
	preview := mediatest.JPEG(3, 2, color.White)
	rawData := func(extra ...syntheticTIFFEntry) []byte {
		entries := []syntheticTIFFEntry{tiffShort(0x0112, 6), tiffASCII(0x010f, "Synthetic Camera"), tiffShort(0x0102, 16), tiffShort(0x0103, 7), tiffShort(0x0106, 32803), tiffLong(0xc612, 0x00000401), tiffShort(0x828e, 1), tiffLong(0x0201, 0), tiffLong(0x0202, uint32(len(preview)))}
		data := syntheticTIFF(42, append(entries, extra...), nil)
		for index := range int(binary.LittleEndian.Uint16(data[8:])) {
			entry := 10 + index*12
			if binary.LittleEndian.Uint16(data[entry:]) == 0x0201 {
				binary.LittleEndian.PutUint32(data[entry+8:], uint32(len(data)))
			}
		}
		return append(data, preview...)
	}
	data := rawData()
	in := photoRenderInput(data, "image/x-adobe-dng")
	out, receipt, err := renderPhotoExport(t.Context(), data, in, bundle.PhotoRenderProfile{Format: "png", Quality: 90, IncludeMetadata: true}, nil)
	require.NoError(t, err)
	assert.True(t, receipt.EmbeddedPreview)
	assert.Equal(t, 2, receipt.Width)
	assert.Equal(t, 3, receipt.Height)
	packets, err := photoSourcePackets(t.Context(), out, true)
	require.NoError(t, err)
	reader, ok := newExifReader(packets.exif)
	require.True(t, ok)
	assert.Equal(t, uint16(1), reader.order.Uint16(reader.entries(reader.u32(4))[0x0112]))
	root := reader.entries(reader.u32(4))
	for _, tag := range []uint16{0x0102, 0x0103, 0x0106, 0xc612, 0x828e, 0x0201, 0x0202} {
		assert.NotContains(t, root, tag)
	}
	assert.Equal(t, "Synthetic Camera", exifASCII(root[0x010f]))
	rootICC := syntheticPhotoICC()
	previewICC := append([]byte(nil), rootICC...)
	whitePoint := int(binary.BigEndian.Uint32(previewICC[136:]))
	previewICC[whitePoint+19] ^= 1
	originalPreview := preview
	preview = syntheticJPEGSegment(t, preview, 0xe2, append([]byte("ICC_PROFILE\x00\x01\x01"), previewICC...))
	profiledRAW := rawData(syntheticTIFFEntry{tag: 34675, kind: 7, value: rootICC})
	for _, format := range []string{"jpeg", "png"} {
		for _, metadata := range []bool{true, false} {
			out, _, err := renderPhotoExport(t.Context(), profiledRAW, photoRenderInput(profiledRAW, "image/x-adobe-dng"), bundle.PhotoRenderProfile{Format: format, Quality: 90, IncludeMetadata: metadata}, nil)
			require.NoError(t, err)
			packets, err := photoSourcePackets(t.Context(), out, true)
			require.NoError(t, err)
			require.Equal(t, previewICC, packets.icc)
			if metadata {
				reader, ok := newExifReader(packets.exif)
				require.True(t, ok)
				require.NotContains(t, reader.entries(reader.u32(4)), uint16(34675))
			}
		}
	}
	preview = originalPreview
	ambiguousRAW := rawData(syntheticTIFFEntry{tag: 34675, kind: 7, value: rootICC})
	for _, metadata := range []bool{true, false} {
		_, _, err := renderPhotoExport(t.Context(), ambiguousRAW, photoRenderInput(ambiguousRAW, "image/x-adobe-dng"), bundle.PhotoRenderProfile{Format: "jpeg", Quality: 90, IncludeMetadata: metadata}, nil)
		require.ErrorIs(t, err, bundle.ErrUnavailable)
		require.ErrorContains(t, err, "not associated")
	}
	missing := syntheticRAWPreviewTIFF(1)
	_, _, err = renderPhotoExport(t.Context(), missing, photoRenderInput(missing, "image/x-adobe-dng"), bundle.PhotoRenderProfile{Format: "jpeg", Quality: 90}, nil)
	require.ErrorIs(t, err, bundle.ErrUnavailable)
	bad := syntheticJPEGSegment(t, preview, 0xe1, append([]byte(photoXMPJPEGPrefix), []byte(`<x:xmpmeta xmlns:x="adobe:ns:meta/"><broken>`)...))
	input := photoRenderInput(bad, "image/jpeg")
	_, _, err = renderPhotoExport(t.Context(), bad, input, bundle.PhotoRenderProfile{Format: "jpeg", Quality: 90, IncludeMetadata: true}, nil)
	require.Error(t, err)
	_, _, err = renderPhotoExport(t.Context(), bad, input, bundle.PhotoRenderProfile{Format: "jpeg", Quality: 90}, nil)
	require.NoError(t, err)
	oversized := bytes.Repeat([]byte{'x'}, maxPhotoSidecarBytes+1)
	for _, tag := range []uint16{34675, 0x010e} {
		raw := rawData(syntheticTIFFEntry{tag: tag, kind: 7, value: oversized})
		// Ordinary extraction remains lenient while export refuses discarded values.
		reader, ok := newExifReader(raw)
		require.True(t, ok)
		_, ok = reader.typedEntries(reader.u32(4))
		require.True(t, ok)
		for _, metadata := range []bool{true, false} {
			_, _, err = renderPhotoExport(t.Context(), raw, photoRenderInput(raw, "image/x-adobe-dng"), bundle.PhotoRenderProfile{Format: "png", Quality: 90, IncludeMetadata: metadata}, nil)
			require.ErrorContains(t, err, "invalid RAW EXIF directory")
		}
	}
	var compressed bytes.Buffer
	writer := zlib.NewWriter(&compressed)
	_, err = writer.Write(oversized)
	require.NoError(t, err)
	require.NoError(t, writer.Close())
	for _, test := range []struct {
		format, kind string
		payload      []byte
		skipped      bool
	}{
		{"png", "eXIf", oversized, false},
		{"png", "iTXt", append([]byte(photoXMPPNGKeyword+"\x00\x00\x00\x00\x00"), oversized...), true},
		{"png", "iTXt", append([]byte(photoXMPPNGKeyword+"\x00\x01\x00\x00\x00"), compressed.Bytes()...), true},
		{"png", "iTXt", append([]byte(photoXMPPNGKeyword+"\x00\x00\x00"), oversized...), true},
		{"png", "iCCP", append([]byte("Profile\x00\x00"), compressed.Bytes()...), false},
		{"webp", "EXIF", oversized, false},
		{"webp", "ICCP", oversized, false},
		{"webp", "XMP ", oversized, true},
	} {
		var container bytes.Buffer
		if test.format == "png" {
			container.WriteString("\x89PNG\r\n\x1a\n")
			writePhotoPNGChunk(&container, test.kind, test.payload)
			writePhotoPNGChunk(&container, "IEND", nil)
		} else {
			container.WriteString("RIFF")
			padding := len(test.payload) % 2
			require.NoError(t, binary.Write(&container, binary.LittleEndian, uint32(12+len(test.payload)+padding)))
			container.WriteString("WEBP" + test.kind)
			require.NoError(t, binary.Write(&container, binary.LittleEndian, uint32(len(test.payload))))
			container.Write(test.payload)
			container.Write(make([]byte, padding))
		}
		_, err = photoSourcePackets(t.Context(), container.Bytes(), true)
		require.ErrorIs(t, err, errVisualMetadataLimit, test.kind)
		_, err = photoSourcePackets(t.Context(), container.Bytes(), false)
		if test.skipped {
			require.NoError(t, err, test.kind)
		} else {
			require.ErrorIs(t, err, errVisualMetadataLimit, test.kind)
		}
	}
}

func syntheticPhotoICC() []byte {
	tags := []string{"wtpt", "rXYZ", "gXYZ", "bXYZ", "rTRC", "gTRC", "bTRC"}
	profile := make([]byte, 0)
	profile = append(profile, make([]byte, 132+12*len(tags))...)
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
func TestPhotoExportErrorPreservesCategory(t *testing.T) {
	t.Parallel()
	input := store.PhotoExportInput{Member: bundle.Member{NodeID: 1}}
	for _, category := range []error{bundle.ErrLimit, bundle.ErrConflict, bundle.ErrUnavailable} {
		err := photoExportError(input, category)
		require.ErrorIs(t, err, category)
		require.Contains(t, err.Error(), "photo 1")
	}
}

func TestPhotoExportGIFCanvas(t *testing.T) {
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
	output, receipt, err := renderPhotoExport(t.Context(), source, photoRenderInput(source, "image/gif"), bundle.PhotoRenderProfile{Format: "png", Quality: 90}, nil)
	require.NoError(t, err)
	assert.Equal(t, 64, receipt.Width)
	assert.Equal(t, 32, receipt.Height)
	rendered, err := png.Decode(bytes.NewReader(output))
	require.NoError(t, err)
	assert.Equal(t, image.Rect(0, 0, 64, 32), rendered.Bounds())
	red, green, blue, alpha := rendered.At(8, 4).RGBA()
	assert.Equal(t, [4]uint32{}, [4]uint32{red, green, blue, alpha})
	red, green, blue, alpha = rendered.At(32, 16).RGBA()
	assert.Equal(t, [4]uint32{0, 0, 0, 65535}, [4]uint32{red, green, blue, alpha})
}

func TestPhotoExportPNGPixelChunksDoNotConsumeMetadataBudget(t *testing.T) {
	t.Parallel()
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
	_, err := photoSourcePackets(t.Context(), source.Bytes(), false)
	require.NoError(t, err)
	input := photoRenderInput(source.Bytes(), "image/png")
	output, receipt, err := renderPhotoExport(t.Context(), source.Bytes(), input, bundle.PhotoRenderProfile{Format: "png", Quality: 90}, nil)
	require.NoError(t, err)
	require.Equal(t, 65, receipt.Width)
	require.NotEmpty(t, output)
}

func TestPhotoExportPNGRejectsTooManyNonPixelChunks(t *testing.T) {
	t.Parallel()
	var source bytes.Buffer
	source.WriteString("\x89PNG\r\n\x1a\n")
	for range visualPreviewMaxPNGChunks + 1 {
		writePhotoPNGChunk(&source, "tEXt", []byte("synthetic\x00metadata"))
	}
	writePhotoPNGChunk(&source, "IEND", nil)
	_, err := photoSourcePackets(t.Context(), source.Bytes(), false)
	require.ErrorIs(t, err, errVisualMetadataLimit)
}
