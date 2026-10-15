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
	"slices"
	"strings"
	"testing"
	"testing/synctest"
	"time"

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

// The image boundary lets cancellation happen after decoding, during real
// pixel work, without depending on machine speed or a large allocation.
type cancelingPhotoPixels struct {
	image.Image

	onRead func()
	reads  int
}

func (p *cancelingPhotoPixels) At(x, y int) color.Color {
	p.reads++
	if p.reads == 1 {
		p.onRead()
	}
	return p.Image.At(x, y)
}

func TestPhotoExportCancellationAfterDecode(t *testing.T) {
	t.Parallel()
	for _, edge := range []int{0, 32} {
		for _, deadline := range []bool{false, true} {
			t.Run(fmt.Sprintf("edge=%d/deadline=%t", edge, deadline), func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					data := mediatest.PNG(128, 128, color.White)
					decoded, orientation, err := decodePhotoExport(t.Context(), bytes.NewReader(data), "png", 0, photoPackets{orientation: 6})
					require.NoError(t, err)
					ctx, cancel := context.WithTimeout(t.Context(), time.Second)
					defer cancel()
					want := context.Canceled
					onRead := cancel
					if deadline {
						want = context.DeadlineExceeded
						onRead = func() { <-ctx.Done() }
					}
					pixels := &cancelingPhotoPixels{Image: decoded, onRead: onRead}
					out, _, err := encodePhotoExport(ctx, pixels, orientation, photoPackets{}, photoRenderInput(data, "image/png"), bundle.PhotoRenderReceipt{Profile: bundle.PhotoRenderProfile{Format: "png", LongEdge: edge}})
					require.ErrorIs(t, err, want)
					assert.Empty(t, out)
					assert.Less(t, pixels.reads, 128*128, "cancellation must stop pixel work before the entire source is consumed")
				})
			})
		}
	}
}

func TestPhotoExportCancellationDuringEncoding(t *testing.T) {
	t.Parallel()
	for _, format := range []string{"jpeg", "png"} {
		t.Run(format, func(t *testing.T) {
			// Gray skips PNG's opacity scan, so cancellation occurs during
			// pixel encoding rather than format selection.
			var source bytes.Buffer
			require.NoError(t, png.Encode(&source, image.NewGray(image.Rect(0, 0, 128, 128))))
			decoded, err := png.Decode(&source)
			require.NoError(t, err)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			pixels := &cancelingPhotoPixels{Image: decoded, onRead: cancel}
			var out bytes.Buffer
			err = writePhotoExport(ctx, &out, pixels, bundle.PhotoRenderProfile{Format: format, Quality: 90})
			require.ErrorIs(t, err, context.Canceled)
			assert.Less(t, pixels.reads, 128*128, "cancellation must stop encoding before it reads the entire image")
		})
	}
}

func TestPhotoExportContainerMetadataAndColor(t *testing.T) {
	t.Parallel()
	source := mediatest.PNG(3, 2, color.Transparent)
	var late bytes.Buffer
	late.Write(source[:len(source)-12])
	writePhotoPNGChunk(&late, "eXIf", syntheticTIFF(42, []syntheticTIFFEntry{tiffShort(0x0112, 6)}, nil))
	late.Write(source[len(source)-12:])
	_, receipt, err := renderPhotoExport(t.Context(), late.Bytes(), photoRenderInput(late.Bytes(), "image/png"), bundle.PhotoRenderProfile{Format: "png", Quality: 90, IncludeMetadata: true})
	require.NoError(t, err)
	assert.Equal(t, 2, receipt.Width)
	packet := []byte(photoSidecarHeader + ` dc:creator="Embedded credit" dc:rights="Embedded rights" dc:description="Embedded caption" dc:subject="Embedded keyword" xmlns:keep="https://example.org/photo/" keep:Orientation="77"><keep:ImageWidth>12345</keep:ImageWidth>` + photoSidecarFooter)
	var compressed, tagged bytes.Buffer
	writer := zlib.NewWriter(&compressed)
	_, err = writer.Write(packet)
	require.NoError(t, err)
	require.NoError(t, writer.Close())
	tagged.Write(source[:len(source)-12])
	writePhotoPNGChunk(&tagged, "iTXt", append([]byte(photoXMPPNGKeyword+"\x00\x01\x00\x00\x00"), compressed.Bytes()...))
	tagged.Write(source[len(source)-12:])
	compressedPackets, err := photoSourcePackets(t.Context(), tagged.Bytes(), true, true)
	require.NoError(t, err)
	require.Equal(t, packet, compressedPackets.xmp)
	profile := syntheticPhotoICC()

	jpegSource := syntheticJPEGSegment(t, mediatest.JPEG(3, 2, color.White), 0xe2, append([]byte("ICC_PROFILE\x00\x01\x01"), profile...))
	var pngICCSource []byte
	for _, format := range []string{"jpeg", "png"} {
		out, _, err := renderPhotoExport(t.Context(), jpegSource, photoRenderInput(jpegSource, "image/jpeg"), bundle.PhotoRenderProfile{Format: format, Quality: 90})
		require.NoError(t, err)
		packets, err := photoSourcePackets(t.Context(), out, true, true)
		require.NoError(t, err)
		assert.Equal(t, profile, packets.icc)
		if format == "png" {
			pngICCSource = out
		}
	}
	for _, declaration := range [][]byte{{12, 13, 0, 1}, {9, 16, 0, 1}} {
		for _, original := range [][]byte{source, pngICCSource} {
			data := syntheticPNGChunk(t, original, "cICP", declaration)
			for _, metadata := range []bool{false, true} {
				_, _, err := renderPhotoExport(t.Context(), data, photoRenderInput(data, "image/png"), bundle.PhotoRenderProfile{Format: "png", IncludeMetadata: metadata})
				require.ErrorIs(t, err, bundle.ErrUnavailable)
			}
		}
	}
}

func TestPhotoExportQualifiedRatingPreservesFlag(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, property, flag string
	}{
		{"literal", `<xmp:Rating exif:GPSLatitude="12,30N">-1</xmp:Rating>`, "reject"},
		{"qualified value", `<xmp:Rating rdf:parseType="Resource"><rdf:value>-1</rdf:value><exif:GPSLatitude>12,30N</exif:GPSLatitude></xmp:Rating>`, "reject"},
		{"qualified description", `<xmp:Rating><rdf:Description><rdf:value>-1</rdf:value><exif:GPSLatitude>12,30N</exif:GPSLatitude></rdf:Description></xmp:Rating>`, "reject"},
		{"nested qualifier value", `<xmp:Rating rdf:parseType="Resource"><rdf:value>-1</rdf:value><exif:GPSLatitude><rdf:value>12,30N</rdf:value></exif:GPSLatitude></xmp:Rating>`, "reject"},
		{"qualified pick", `<xmp:Rating>4</xmp:Rating><ts:Pick rdf:parseType="Resource"><rdf:value>pick</rdf:value><exif:GPSLatitude>12,30N</exif:GPSLatitude></ts:Pick>`, "pick"},
	} {
		for _, format := range []string{"jpeg", "png"} {
			for _, removeGPS := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/removeGPS=%t", tc.name, format, removeGPS), func(t *testing.T) {
					packet := []byte(photoSidecarHeader + ` xmlns:exif="http://ns.adobe.com/exif/1.0/">` + tc.property + photoSidecarFooter)
					data := syntheticPNGChunk(t, mediatest.PNG(3, 2, color.White), "iTXt", append([]byte(photoXMPPNGKeyword+"\x00\x00\x00\x00\x00"), packet...))
					input := photoRenderInput(data, "image/png")
					input.Authored = store.PhotoAuthored{Confirmed: store.PhotoConfirmedRating, Rating: 3}
					out, _, err := renderPhotoExport(t.Context(), data, input, bundle.PhotoRenderProfile{Format: format, Quality: 90, IncludeMetadata: true, RemoveGPS: removeGPS})
					require.NoError(t, err)
					packets, err := photoSourcePackets(t.Context(), out, true, true)
					require.NoError(t, err)
					actual, err := ReadPhotoSidecar(t.Context(), packets.xmp)
					require.NoError(t, err)
					assert.Equal(t, 3, actual.Rating)
					assert.Equal(t, tc.flag, actual.Flag)
				})
			}
		}
	}
}

func TestPhotoExportNormalizedXMPLiterals(t *testing.T) {
	t.Parallel()
	const tiffNS = "http://ns.adobe.com/tiff/1.0/"
	const exifNS = "http://ns.adobe.com/exif/1.0/"
	properties := []struct {
		space, name, source, want string
	}{
		{tiffNS, "Orientation", "6", "1"},
		{exifNS, "Orientation", "6", "1"},
		{tiffNS, "ImageWidth", "4", "1"},
		{tiffNS, "ImageLength", "2", "2"},
		{exifNS, "PixelXDimension", "4", "1"},
		{exifNS, "PixelYDimension", "2", "2"},
	}
	for _, form := range []struct{ name, attrs, contents string }{
		{"literal", "", "%s"},
		{"qualified resource", ` rdf:parseType="Resource"`, `<rdf:value>%s</rdf:value><keep:Note>source measurement</keep:Note>`},
		{"qualified description", "", `<rdf:Description><rdf:value>%s</rdf:value><keep:Note>source measurement</keep:Note></rdf:Description>`},
		{"resource reference", ` rdf:resource="urn:synthetic:measurement"`, ""},
		{"blank node", ` rdf:nodeID="measurement"`, ""},
	} {
		for _, format := range []string{"jpeg", "png"} {
			t.Run(form.name+"/"+format, func(t *testing.T) {
				var packet strings.Builder
				packet.WriteString(photoSidecarHeader + ` xmlns:keep="https://example.org/photo/" keep:Lens="Synthetic lens">`)
				for _, property := range properties {
					contents := form.contents
					if contents != "" {
						contents = fmt.Sprintf(contents, property.source)
					}
					// Bind the property's namespace locally so normalization must retain it.
					fmt.Fprintf(&packet, `<p:%s xmlns:p="%s"%s>%s</p:%s>`, property.name, property.space, form.attrs, contents, property.name)
				}
				packet.WriteString(photoSidecarFooter)
				data := syntheticPNGChunk(t, mediatest.PNG(4, 2, color.White), "iTXt", append([]byte(photoXMPPNGKeyword+"\x00\x00\x00\x00\x00"), packet.String()...))
				data = syntheticPNGChunk(t, data, "eXIf", syntheticTIFF(42, []syntheticTIFFEntry{tiffShort(0x0112, 6)}, nil))
				out, _, err := renderPhotoExport(t.Context(), data, photoRenderInput(data, "image/png"), bundle.PhotoRenderProfile{Format: format, Quality: 90, LongEdge: 2, IncludeMetadata: true})
				require.NoError(t, err)
				pixels, _, err := image.DecodeConfig(bytes.NewReader(out))
				require.NoError(t, err)
				require.Equal(t, 1, pixels.Width)
				require.Equal(t, 2, pixels.Height)
				packets, err := photoSourcePackets(t.Context(), out, true, true)
				require.NoError(t, err)
				var decoded struct {
					Description struct {
						Lens       string `xml:"https://example.org/photo/ Lens,attr"`
						Properties []struct {
							XMLName  xml.Name   `xml:""`
							Attrs    []xml.Attr `xml:",any,attr"`
							Value    string     `xml:",chardata"`
							Children []struct{} `xml:",any"`
						} `xml:",any"`
					} `xml:"RDF>Description"`
				}
				require.NoError(t, xml.Unmarshal(packets.xmp, &decoded))
				require.Equal(t, "Synthetic lens", decoded.Description.Lens)
				require.Len(t, decoded.Description.Properties, len(properties))
				for index, property := range decoded.Description.Properties {
					want := properties[index]
					assert.Equal(t, xml.Name{Space: want.space, Local: want.name}, property.XMLName)
					assert.Equal(t, want.want, property.Value)
					assert.Empty(t, property.Children)
					// RDF/XML literalPropertyElt allows ID/datatype, XML context, and
					// namespace declarations; resource and qualifier attributes change its meaning.
					for _, attr := range property.Attrs {
						valid := attr.Name.Space == "xmlns" || attr.Name == (xml.Name{Local: "xmlns"}) ||
							attr.Name.Space == xmlNamespace || attr.Name.Space == rdfNamespace && (attr.Name.Local == "ID" || attr.Name.Local == "datatype")
						assert.True(t, valid, "%s has non-literal RDF attribute %v", want.name, attr.Name)
					}
				}
			})
		}
	}
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
				if _, _, err := renderPhotoExport(context.Background(), source.Bytes(), input, bundle.PhotoRenderProfile{Format: setting.format, Quality: 90, LongEdge: setting.edge}); err != nil {
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
	for _, tc := range []struct {
		name, attrs, property  string
		source, rotation, want int
		refuse                 bool
	}{
		{"XMP-only", `tiff:Orientation="6"`, "", 1, 0, 1, true},
		{"matching with authored rotation", `tiff:Orientation="6"`, "", 6, 90, 3, false},
		{"conflicting element", "", "<exif:Orientation>8</exif:Orientation>", 6, 0, 6, true},
		{"matching element", "", "<tiff:Orientation> 6 </tiff:Orientation>", 6, 0, 6, false},
		{"conflicting qualified value", "", `<tiff:Orientation rdf:parseType="Resource"><rdf:value>8</rdf:value><exif:GPSLatitude>12,30N</exif:GPSLatitude></tiff:Orientation>`, 6, 0, 6, true},
		{"conflicting qualified description", "", `<exif:Orientation><rdf:Description><rdf:value>8</rdf:value><exif:GPSLatitude>12,30N</exif:GPSLatitude></rdf:Description></exif:Orientation>`, 6, 0, 6, true},
		{"matching qualified value", "", `<tiff:Orientation rdf:parseType="Resource"><rdf:value>6</rdf:value><exif:GPSLatitude><rdf:value>8</rdf:value></exif:GPSLatitude></tiff:Orientation>`, 6, 0, 6, false},
		{"identity", `exif:Orientation="1"`, "", 6, 0, 6, false},
		{"invalid", "", "<exif:Orientation>9</exif:Orientation>", 1, 0, 1, false},
		{"zero", `tiff:Orientation="0"`, "", 1, 0, 1, false},
		{"empty", "", "<tiff:Orientation/>", 1, 0, 1, false},
		{"nonnumeric", `tiff:Orientation="unknown"`, "", 1, 0, 1, false},
		{"both namespaces", `tiff:Orientation="6" exif:Orientation="8"`, "", 6, 0, 6, true},
	} {
		for _, metadata := range []bool{true, false} {
			t.Run(fmt.Sprintf("XMP/%s/metadata=%t", tc.name, metadata), func(t *testing.T) {
				packet := []byte(photoSidecarHeader + ` xmlns:tiff="http://ns.adobe.com/tiff/1.0/" xmlns:exif="http://ns.adobe.com/exif/1.0/" ` + tc.attrs + ">" + tc.property + photoSidecarFooter)
				data := syntheticPNGChunk(t, pixels.Bytes(), "iTXt", append([]byte(photoXMPPNGKeyword+"\x00\x00\x00\x00\x00"), packet...))
				if tc.source != 1 {
					data = syntheticPNGChunk(t, data, "eXIf", syntheticTIFF(42, []syntheticTIFFEntry{tiffShort(0x0112, uint16(tc.source))}, nil))
				}
				input := photoRenderInput(data, "image/png")
				input.Authored = store.PhotoAuthored{Rotation: tc.rotation, Confirmed: store.PhotoConfirmedRotation}
				out, receipt, err := renderPhotoExport(t.Context(), data, input, bundle.PhotoRenderProfile{Format: "png", IncludeMetadata: metadata})
				if metadata && tc.refuse {
					require.ErrorIs(t, err, bundle.ErrUnavailable)
					require.ErrorIs(t, err, errPhotoXMPOrientation)
					require.ErrorContains(t, err, "turn image metadata off")
					return
				}
				require.NoError(t, err)
				decoded, err := png.Decode(bytes.NewReader(out))
				require.NoError(t, err)
				w, h := 3, 2
				if tc.want >= 5 {
					w, h = 2, 3
				}
				require.Equal(t, w, receipt.Width)
				require.Equal(t, h, receipt.Height)
				for index, sourceIndex := range orders[tc.want-1] {
					require.Equal(t, color.NRGBAModel.Convert(frame.At(sourceIndex%3, sourceIndex/3)), color.NRGBAModel.Convert(decoded.At(index%w, index/w)))
				}
				if metadata {
					packets, err := photoSourcePackets(t.Context(), out, true, true)
					require.NoError(t, err)
					if tc.property != "" {
						require.Contains(t, string(packets.xmp), ">1<")
					} else {
						require.Contains(t, string(packets.xmp), `Orientation="1"`)
					}
				}
			})
		}
	}
	for orientation := 1; orientation <= 8; orientation++ {
		var source bytes.Buffer
		source.Write(pixels.Bytes()[:33])
		writePhotoPNGChunk(&source, "eXIf", syntheticTIFF(42, []syntheticTIFFEntry{tiffShort(0x0112, uint16(orientation))}, nil))
		source.Write(pixels.Bytes()[33:])
		for _, rotation := range []int{0, 90, 180, 270} {
			t.Run(fmt.Sprintf("%d/%d", orientation, rotation), func(t *testing.T) {
				input := photoRenderInput(source.Bytes(), "image/png")
				input.Authored.Rotation = rotation
				input.Authored.Confirmed = store.PhotoConfirmedRotation
				out, receipt, err := renderPhotoExport(t.Context(), source.Bytes(), input, bundle.PhotoRenderProfile{Format: "png", Quality: 90})
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
		out, receipt, err := renderPhotoExport(t.Context(), source.Bytes(), input, bundle.PhotoRenderProfile{Format: "png", Quality: 90, LongEdge: test.edge})
		require.NoError(t, err)
		assert.Equal(t, test.want, receipt.Width)
		if test.edge == 0 {
			rendered, err := png.Decode(bytes.NewReader(out))
			require.NoError(t, err)
			_, _, _, alpha := rendered.At(0, 1).RGBA()
			assert.Zero(t, alpha)
		}
	}
	var outputs [][]byte
	for _, quality := range []int{10, 95} {
		out, _, err := renderPhotoExport(t.Context(), source.Bytes(), input, bundle.PhotoRenderProfile{Format: "jpeg", Quality: quality})
		require.NoError(t, err)
		_, err = jpeg.Decode(bytes.NewReader(out))
		require.NoError(t, err)
		outputs = append(outputs, out)
	}
	assert.Less(t, len(outputs[0]), len(outputs[1]))
}

func TestPhotoExportMetadataPreservationAndAuthoredEdits(t *testing.T) {
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
	packet := []byte("\xef\xbb\xbf" + photoSidecarHeader + ` xmp:Rating="5" dc:description="Old caption" xmlns:exif="http://ns.adobe.com/exif/1.0/" exif:GPSLatitude="12,34N" xmlns:keep="https://example.org/photo/" keep:Lens="Synthetic lens" xmlns:crs="http://ns.adobe.com/camera-raw-settings/1.0/" xmlns:container="http://ns.google.com/photos/1.0/container/" xmlns:hdrgm="http://ns.adobe.com/hdr-gain-map/1.0/" crs:HasCrop="stale-crop" crs:Exposure="1.25" hdrgm:Version="stale-gain-map"><xmp:Thumbnails><rdf:Alt><rdf:li>stale-thumbnail</rdf:li></rdf:Alt></xmp:Thumbnails><crs:CropAngle>stale-crop</crs:CropAngle><container:Directory>stale-container</container:Directory><exif:Orientation>6</exif:Orientation>` + photoSidecarFooter)
	input := store.PhotoExportInput{Authored: store.PhotoAuthored{Confirmed: store.PhotoConfirmedAll, Flag: "reject"}, Keywords: []string{"landscape", "reviewed"}}
	receipt := bundle.PhotoRenderReceipt{Profile: bundle.PhotoRenderProfile{RemoveGPS: true}, Width: 2, Height: 3}
	merged, err := mergePhotoXMP(t.Context(), packet, input, receipt, 6, nil)
	require.NoError(t, err)
	assert.NotContains(t, string(merged), "GPSLatitude")
	assert.Contains(t, string(merged), "Synthetic lens")
	assert.NotContains(t, string(merged), "stale-")
	assert.Contains(t, string(merged), `crs:Exposure="1.25"`)
	assert.NotContains(t, string(merged), "Old caption")
	values, err := ReadPhotoSidecar(t.Context(), merged)
	require.NoError(t, err)
	expected := input.Authored
	expected.Confirmed = 0
	assert.Equal(t, expected, values)
	assert.Contains(t, string(merged), "landscape")
	assert.Contains(t, string(merged), "reviewed")
	largeXMP := []byte(photoSidecarHeader + ` xmlns:keep="https://example.org/photo/">` + strings.Repeat(`<keep:History rdf:parseType="Resource"><keep:Step keep:Type="keep:Type">kept &amp; safe</keep:Step></keep:History>`, 360) + `<keep:Scoped xmlns:keep="https://example.org/scoped/" keep:Type="keep:Type"><keep:Value>keep:Literal</keep:Value></keep:Scoped><Plain xmlns="https://example.org/default/" xmlns:q="https://example.org/default/" q:Value="q:Literal"><Child/></Plain><keep:Reset xmlns=""><Child/></keep:Reset>` + photoSidecarFooter)
	require.Greater(t, len(largeXMP), 40000)
	merged, err = mergePhotoXMP(t.Context(), largeXMP, input, receipt, 1, nil)
	require.NoError(t, err)
	require.Less(t, len(merged), len(largeXMP)+1000)
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
		out, err := rewritePhotoEXIF(exif, 2, 3, strip, input.Authored, len(input.Keywords) > 0)
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
		out, receipt, err := renderPhotoExport(t.Context(), data, in, bundle.PhotoRenderProfile{Format: format, Quality: 90, IncludeMetadata: true, RemoveGPS: removeGPS})
		require.NoError(t, err)
		packets, err := photoSourcePackets(t.Context(), out, true, true)
		require.NoError(t, err)
		return packets, receipt
	}
	nestedPacket := []byte(photoSidecarHeader + ` xmlns:keep="https://example.org/photo/" xmlns:tiff="http://ns.adobe.com/tiff/1.0/" dc:creator="Original photo creator" tiff:Orientation="6"><dc:creator>Original photo creator</dc:creator><keep:History rdf:parseType="Resource" dc:creator="Prior attribute" tiff:Orientation="7" dc:subject="History subject attribute" xmp:Rating="-1" ts:Pick="reject"><dc:creator>Prior editor</dc:creator><tiff:Orientation>8</tiff:Orientation><tiff:ImageWidth>12345</tiff:ImageWidth><dc:subject>History subject element</dc:subject><xmp:Rating>-1</xmp:Rating><ts:Pick>reject</ts:Pick><keep:GPSLatitude>Nested GPS</keep:GPSLatitude></keep:History>` + photoSidecarFooter)
	for _, bare := range []bool{false, true} {
		packet := nestedPacket
		if bare {
			packet = bytes.Replace(packet, []byte(`<x:xmpmeta xmlns:x="adobe:ns:meta/">`), nil, 1)
			packet = bytes.Replace(packet, []byte(`</x:xmpmeta>`), nil, 1)
		}
		for _, format := range []string{"jpeg", "png"} {
			packets, _ := render(syntheticTIFF(42, []syntheticTIFFEntry{tiffShort(0x0112, 6)}, nil), packet, store.PhotoAuthored{Confirmed: store.PhotoConfirmedAll, Creator: "Current photo creator", Rating: 3, Flag: "pick"}, []string{"Catalog keyword"}, format, true)
			for _, retained := range []string{`dc:creator="Prior attribute"`, ">Prior editor<", `tiff:Orientation="7"`, ">8<", ">12345<", `dc:subject="History subject attribute"`, ">History subject element<", `xmp:Rating="-1"`, ">-1<", `ts:Pick="reject"`, ">reject<"} {
				require.Contains(t, string(packets.xmp), retained, format)
			}
			require.NotContains(t, string(packets.xmp), "Original photo creator")
			require.NotContains(t, string(packets.xmp), "Nested GPS")
			require.Contains(t, string(packets.xmp), ">Current photo creator<")
			require.Contains(t, string(packets.xmp), ">Catalog keyword<")
			require.Contains(t, string(packets.xmp), `tiff:Orientation="1"`)
		}
	}
	for _, format := range []string{"jpeg", "png"} {
		data := mediatest.JPEG(3, 2, color.White)
		out, _, err := renderPhotoExport(t.Context(), data, photoRenderInput(data, "image/jpeg"), bundle.PhotoRenderProfile{Format: format, Quality: 90, IncludeMetadata: true})
		require.NoError(t, err)
		packets, err := photoSourcePackets(t.Context(), out, true, true)
		require.NoError(t, err)
		require.Empty(t, packets.xmp)
	}
	keywordPacket := []byte(photoSidecarHeader + ` dc:subject="Embedded subject attribute" xmlns:ms="http://ns.microsoft.com/photo/1.0" xmlns:msLegacy="http://ns.microsoft.com/photo/1.0/" ms:LastKeywordXMP="Embedded Microsoft XMP" msLegacy:LastKeywordIPTC="Embedded Microsoft IPTC" xmlns:lr="http://ns.adobe.com/lightroom/1.0/" xmlns:pdf="http://ns.adobe.com/pdf/1.3/" lr:hierarchicalSubject="Embedded hierarchy attribute" pdf:Keywords="Embedded PDF attribute"><ms:LastKeywordIPTC_TIFF_IRB>Embedded Microsoft TIFF</ms:LastKeywordIPTC_TIFF_IRB><dc:subject><rdf:Bag><rdf:li>Embedded subject element</rdf:li></rdf:Bag></dc:subject><lr:hierarchicalSubject><rdf:Bag><rdf:li>Embedded hierarchy element</rdf:li></rdf:Bag></lr:hierarchicalSubject><pdf:Keywords>Embedded PDF element</pdf:Keywords><keep:History xmlns:keep="https://example.org/photo/" rdf:parseType="Resource" dc:subject="Nested subject attribute" ms:Rating="99" ms:LastKeywordXMP="Nested Microsoft keyword"><dc:subject>Nested subject element</dc:subject></keep:History>` + photoSidecarFooter)
	keywordEXIF := syntheticTIFF(42, []syntheticTIFFEntry{tiffASCII(0x9c9e, "Embedded EXIF keyword")}, nil)

	for _, keywords := range [][]string{nil, {"catalog tag"}, {"alpha\uffffbeta"}} {
		merged, err := mergePhotoXMP(t.Context(), keywordPacket, store.PhotoExportInput{Keywords: keywords}, receipt, 1, nil)
		if len(keywords) > 0 && strings.Contains(keywords[0], "\uffff") {
			require.Error(t, err)
			require.Contains(t, err.Error(), "U+FFFF")
			continue
		}
		require.NoError(t, err)
		if len(keywords) == 0 {
			require.Equal(t, 1, strings.Count(string(merged), "<rdf:Description"))
		}
		for _, value := range []string{"Embedded subject attribute", "Embedded subject element", "Embedded hierarchy attribute", "Embedded hierarchy element", "Embedded PDF attribute", "Embedded PDF element", "Embedded Microsoft XMP", "Embedded Microsoft IPTC", "Embedded Microsoft TIFF"} {
			assert.Equal(t, len(keywords) == 0, strings.Contains(string(merged), value), value)
		}
		assert.Contains(t, string(merged), `ms:Rating="99"`)
		assert.Contains(t, string(merged), "Nested Microsoft keyword")
		rewritten, err := rewritePhotoEXIF(keywordEXIF, 3, 2, false, store.PhotoAuthored{}, len(keywords) > 0)
		require.NoError(t, err)
		r, ok := newExifReader(rewritten)
		require.True(t, ok)
		assert.Equal(t, len(keywords) == 0, len(r.entries(r.u32(4))[0x9c9e]) > 0)
	}
	for _, property := range []string{">", ` ts:Rotation="90">`, `><ts:Rotation>90</ts:Rotation>`} {
		for _, confirmed := range []store.PhotoAuthoredFields{0, store.PhotoConfirmedRotation} {
			rotationPacket := []byte(photoSidecarHeader + property + photoSidecarFooter)
			authored := store.PhotoAuthored{Confirmed: confirmed}
			if confirmed != 0 {
				authored.Rotation = 90
			}
			merged, err := mergePhotoXMP(t.Context(), rotationPacket, store.PhotoExportInput{Authored: authored}, receipt, 1, nil)
			require.NoError(t, err)
			actual, err := ReadPhotoSidecar(t.Context(), merged)
			require.NoError(t, err)
			assert.Equal(t, confirmed != 0 || property != ">", strings.Contains(string(merged), "Rotation"))
			rotation := 0
			if confirmed == 0 && property != ">" {
				rotation = 90
			}
			assert.Equal(t, rotation, actual.Rotation)
		}
	}
	for _, flag := range []string{"pick", "", "reject"} {
		for _, property := range []string{` xmp:Rating="-1">`, `><xmp:Rating>-<!--split-->1</xmp:Rating>`, `><xmp:Rating><rdf:value>-1</rdf:value></xmp:Rating>`, ` xmp:Rating="4">`, `><xmp:Rating>4</xmp:Rating>`, `><xmp:Rating><rdf:value>4</rdf:value></xmp:Rating>`, `><xmp:Rating xmlns:exif="http://ns.adobe.com/exif/1.0/" exif:GPSLatitude="12,30N">4</xmp:Rating>`, `><xmp:Rating xmlns:exif="http://ns.adobe.com/exif/1.0/"><rdf:value>4</rdf:value><exif:GPSLatitude>12,30N</exif:GPSLatitude></xmp:Rating>`, `><xmp:Rating xmlns:exif="http://ns.adobe.com/exif/1.0/"><rdf:value>-1</rdf:value><exif:GPSLatitude>12,30N</exif:GPSLatitude></xmp:Rating>`} {
			spellings := []string{"-1"}
			if strings.Contains(property, "-1") || strings.Contains(property, "-<!--split-->1") {
				spellings = append(spellings, "-01")
			}
			for _, spelling := range spellings {
				property := strings.ReplaceAll(property, "-1", spelling)
				property = strings.ReplaceAll(property, "-<!--split-->1", "-<!--split-->"+spelling[1:])
				for _, confirmed := range []store.PhotoAuthoredFields{0, store.PhotoConfirmedFlag, store.PhotoConfirmedRating, store.PhotoConfirmedFlag | store.PhotoConfirmedRating} {
					if confirmed&store.PhotoConfirmedFlag == 0 && flag != "" {
						continue
					}
					packet := []byte(photoSidecarHeader + property + photoSidecarFooter)
					authored := store.PhotoAuthored{Confirmed: confirmed}
					if confirmed&store.PhotoConfirmedFlag != 0 {
						authored.Flag = flag
					}
					if confirmed&store.PhotoConfirmedRating != 0 {
						authored.Rating = 3
					}
					merged, err := mergePhotoXMP(t.Context(), packet, store.PhotoExportInput{Authored: authored}, receipt, 1, nil)
					require.NoError(t, err)
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
		merged, err := mergePhotoXMP(t.Context(), packet, store.PhotoExportInput{Authored: store.PhotoAuthored{Confirmed: store.PhotoConfirmedRating, Rating: 3}}, receipt, 1, nil)
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
	entries = append(entries, syntheticTIFFEntry{tag: 0x8649, kind: 7, value: []byte("8BIM\x04\x09\x00\x00\x00\x00\x00\x10Embedded preview")}, tiffShort(0x4746, 5), tiffShort(0x4749, 99), syntheticTIFFEntry{tag: 0x83bb, kind: 7, value: append([]byte{0x1c, 2, 25, 0, 16}, []byte("Embedded keyword")...)})
	aliasEXIF := syntheticTIFF(42, entries, nil)
	for _, alias := range []struct {
		name string
		bit  store.PhotoAuthoredFields
	}{
		{"tiff:Artist", store.PhotoConfirmedCreator},
		{"tiff:Copyright", store.PhotoConfirmedCopyright},
		{"tiff:ImageDescription", store.PhotoConfirmedCaption},
		{"dc:creator", store.PhotoConfirmedCreator},
		{"dc:rights", store.PhotoConfirmedCopyright},
		{"dc:description", store.PhotoConfirmedCaption},
		{"ms:Rating", store.PhotoConfirmedRating},
		{"msLegacy:Rating", store.PhotoConfirmedRating},
	} {
		for _, edit := range []string{"", "Confirmed replacement"} {
			if alias.bit == store.PhotoConfirmedRating && edit != "" {
				continue
			}
			t.Run(alias.name+"/"+edit, func(t *testing.T) {
				packet := []byte(photoSidecarHeader + ` xmlns:tiff="http://ns.adobe.com/tiff/1.0/" xmlns:ms="http://ns.microsoft.com/photo/1.0" xmlns:msLegacy="http://ns.microsoft.com/photo/1.0/" xmlns:keep="https://example.org/photo/" keep:Lens="Synthetic lens" keep:Orientation="77" ` + alias.name + `="Old attribute"><` + alias.name + `>Old element</` + alias.name + `><keep:History ms:Rating="Nested rating">Unrelated history</keep:History><keep:ImageWidth>12345</keep:ImageWidth>` + photoSidecarFooter)
				for _, confirmed := range []store.PhotoAuthoredFields{0, alias.bit} {
					if confirmed == 0 && edit != "" {
						continue
					}
					values := store.PhotoAuthored{Confirmed: confirmed}
					switch confirmed {
					case store.PhotoConfirmedRating:
						values.Rating = 3
					case store.PhotoConfirmedCreator:
						values.Creator = edit
					case store.PhotoConfirmedCopyright:
						values.Copyright = edit
					case store.PhotoConfirmedCaption:
						values.Caption = edit
					default:
						require.Zero(t, confirmed)
					}
					merged, err := mergePhotoXMP(t.Context(), packet, store.PhotoExportInput{Authored: values}, receipt, 1, nil)
					require.NoError(t, err)
					assert.Equal(t, confirmed == 0, strings.Contains(string(merged), "Old attribute"))
					assert.Equal(t, confirmed == 0, strings.Contains(string(merged), "Old element"))
					assert.Contains(t, string(merged), "Synthetic lens")
					assert.Contains(t, string(merged), "Unrelated history")
					assert.Contains(t, string(merged), `ms:Rating="Nested rating"`)
					assert.Contains(t, string(merged), `keep:Orientation="77"`)
					assert.Contains(t, string(merged), ">12345<")
					if confirmed != 0 {
						actual, err := ReadPhotoSidecar(t.Context(), merged)
						require.NoError(t, err)
						if alias.bit == store.PhotoConfirmedRating {
							assert.Equal(t, 3, actual.Rating)
						} else {
							assert.Equal(t, edit, map[store.PhotoAuthoredFields]string{store.PhotoConfirmedCreator: actual.Creator, store.PhotoConfirmedCopyright: actual.Copyright, store.PhotoConfirmedCaption: actual.Caption}[alias.bit])
						}
					}
				}
			})
		}
	}
	for _, confirmed := range []store.PhotoAuthoredFields{0, store.PhotoConfirmedCaption | store.PhotoConfirmedCreator | store.PhotoConfirmedRating} {
		for _, format := range []string{"jpeg", "png"} {
			packets, _ := render(aliasEXIF, nil, store.PhotoAuthored{Confirmed: confirmed}, nil, format, false)
			r, ok := newExifReader(packets.exif)
			require.True(t, ok)
			root := r.entries(r.u32(4))
			require.NotContains(t, root, uint16(0x83bb))
			require.NotContains(t, root, uint16(0x8649))
			require.NotContains(t, string(packets.exif), "Embedded keyword")
			if confirmed != 0 {
				require.Contains(t, string(packets.xmp), "Embedded keyword")
			}
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
	dataset := func(record, number byte, value string) []byte {
		return append([]byte{0x1c, record, number, byte(len(value) >> 8), byte(len(value))}, []byte(value)...)
	}
	asciiIPTC := append(dataset(2, 25, "Embedded keyword"), dataset(2, 25, "Embedded subject element")...)
	asciiIPTC = append(asciiIPTC, dataset(2, 25, "Embedded keyword")...)
	for _, subject := range []struct {
		name, value string
		invalid     bool
	}{
		{"attribute", ` dc:subject="bird">`, false},
		{"literal", `><dc:subject>  bird  </dc:subject>`, false},
		{"Bag", `><dc:subject> <rdf:Bag xmlns:keep="https://example.org/photo/"> <rdf:li>  bird  </rdf:li> </rdf:Bag> </dc:subject>`, false},
		{"qualified", `><dc:subject><rdf:Bag><rdf:li rdf:parseType="Resource"> <rdf:value>bird</rdf:value> </rdf:li></rdf:Bag></dc:subject>`, true},
		{"resource", `><dc:subject><rdf:Bag><rdf:li rdf:resource="https://example.org/bird"/></rdf:Bag></dc:subject>`, true},
		{"language", `><dc:subject><rdf:Bag><rdf:li xml:lang="en">bird</rdf:li></rdf:Bag></dc:subject>`, true},
		{"nested", `><dc:subject><rdf:Bag><rdf:li><rdf:Description><rdf:value>bird</rdf:value></rdf:Description></rdf:li></rdf:Bag></dc:subject>`, true},
		{"property attribute", `><dc:subject rdf:parseType="Resource"><rdf:value>bird</rdf:value></dc:subject>`, true},
		{"Seq", `><dc:subject><rdf:Seq><rdf:li>bird</rdf:li></rdf:Seq></dc:subject>`, true},
		{"Alt", `><dc:subject><rdf:Alt><rdf:li>bird</rdf:li></rdf:Alt></dc:subject>`, true},
		{"leading text", `><dc:subject>bird<rdf:Bag/></dc:subject>`, true},
		{"trailing text", `><dc:subject><rdf:Bag/>bird</dc:subject>`, true},
		{"Bag text", `><dc:subject><rdf:Bag>bird</rdf:Bag></dc:subject>`, true},
		{"multiple Bags", `><dc:subject><rdf:Bag/><rdf:Bag><rdf:li>bird</rdf:li></rdf:Bag></dc:subject>`, true},
	} {
		t.Run("XMP union/"+subject.name, func(t *testing.T) {
			packet := []byte(photoSidecarHeader + subject.value + photoSidecarFooter)
			for _, format := range []string{"jpeg", "png"} {
				for _, alternative := range []string{"union", "catalog", "no IPTC", "metadata off"} {
					data := pixels
					if alternative != "no IPTC" {
						exif := syntheticTIFF(42, []syntheticTIFFEntry{{tag: 0x83bb, kind: 7, value: asciiIPTC}}, nil)
						data = syntheticJPEGSegment(t, data, 0xe1, append([]byte("Exif\x00\x00"), exif...))
					}
					data = syntheticJPEGSegment(t, data, 0xe1, append([]byte(photoXMPJPEGPrefix), packet...))
					input := photoRenderInput(data, "image/jpeg")
					if alternative == "catalog" {
						input.Keywords = []string{"catalog tag"}
					}
					profile := bundle.PhotoRenderProfile{Format: format, Quality: 90, IncludeMetadata: alternative != "metadata off"}
					out, _, err := renderPhotoExport(t.Context(), data, input, profile)
					if subject.invalid && alternative == "union" {
						require.ErrorIs(t, err, bundle.ErrUnavailable)
						require.ErrorIs(t, err, errPhotoXMPKeywords)
						continue
					}
					require.NoError(t, err, "%s/%s", format, alternative)
					packets, err := photoSourcePackets(t.Context(), out, true, true)
					require.NoError(t, err)
					switch alternative {
					case "union":
						require.Contains(t, string(packets.xmp), ">Embedded keyword<")
						want := "  bird  "
						if subject.name == "attribute" {
							want = "bird"
						}
						require.Contains(t, string(packets.xmp), ">"+want+"<")
					case "catalog":
						require.Contains(t, string(packets.xmp), ">catalog tag<")
						require.NotContains(t, string(packets.xmp), "bird")
					case "no IPTC":
						require.Contains(t, string(packets.xmp), "bird")
						if subject.name == "qualified" {
							require.Contains(t, string(packets.xmp), `rdf:parseType="Resource"`)
							require.Contains(t, string(packets.xmp), ">bird</rdf:value>")
						}
					case "metadata off":
						require.Empty(t, packets.xmp)
					}
				}
			}
		})
	}
	for _, tc := range []struct {
		name    string
		iptc    []byte
		packet  []byte
		want    string
		invalid bool
	}{
		{"IPTC only", asciiIPTC, nil, "Embedded keyword", false},
		{"coexisting XMP", asciiIPTC, keywordPacket, "Embedded keyword", false},
		{"UTF8", append(dataset(1, 90, "\x1b%G"), dataset(2, 25, "Café")...), nil, "Café", false},
		{"Latin1", append(dataset(1, 90, "\x1b-A"), dataset(2, 25, "Caf\xe9")...), nil, "Café", false},
		{"extended length", append([]byte{0x1c, 2, 25, 0x80, 1, 16}, []byte("Embedded keyword")...), nil, "Embedded keyword", false},
		{"truncated length", []byte{0x1c, 2, 25, 0, 10, 'x'}, nil, "", true},
		{"truncated extended length", []byte{0x1c, 2, 25, 0x80, 4, 1}, nil, "", true},
		{"undeclared nonASCII", dataset(2, 25, "Café"), nil, "", true},
		{"catalog override", dataset(2, 25, "Café"), keywordPacket, "Café", false},
		{"discarded malformed IPTC", []byte("PHUT"), keywordPacket, "PHUT", false},
		{"discarded Photoshop trailing NUL", asciiIPTC, keywordPacket, "Embedded keyword", false},
		{"discarded Photoshop PHUT", nil, keywordPacket, "PHUT", false},
		{"unknown charset", append(dataset(1, 90, "\x1b$B"), dataset(2, 25, "word")...), nil, "", true},
		{"unknown charset without keywords", dataset(1, 90, "\x1b$B"), keywordPacket, "Embedded subject element", false},
		{"invalid UTF8", append(dataset(1, 90, "\x1b%G"), dataset(2, 25, "Caf\xe9")...), nil, "", true},
		{"XML control", dataset(2, 25, "word\x01"), nil, "", true},
	} {
		containers := []string{"TIFF bytes"}
		switch tc.name {
		case "IPTC only":
			containers = append(containers, "TIFF longs", "Photoshop", "TIFF Photoshop")
		case "coexisting XMP":
			containers = append(containers, "TIFF Photoshop")
		case "discarded malformed IPTC":
			containers = append(containers, "TIFF Photoshop")
		case "discarded Photoshop trailing NUL", "discarded Photoshop PHUT":
			containers = []string{"Photoshop", "TIFF Photoshop"}
		}
		for _, container := range containers {
			var catalog []string
			if strings.HasPrefix(tc.name, "discarded ") || tc.name == "catalog override" || tc.name == "coexisting XMP" && container == "TIFF Photoshop" {
				catalog = []string{"catalog tag"}
			}
			t.Run(tc.name+"/"+container, func(t *testing.T) {
				data := pixels
				if container == "Photoshop" || container == "TIFF Photoshop" {
					var resource bytes.Buffer
					resource.WriteString("Photoshop 3.0\x008BIM\x04\x0f\x02ab\x00\x00\x00\x00\x01Z\x008BIM\x04\x04\x02ab\x00")
					require.NoError(t, binary.Write(&resource, binary.BigEndian, uint32(len(tc.iptc))))
					resource.Write(tc.iptc)
					if len(tc.iptc)%2 != 0 {
						resource.WriteByte(0)
					}
					switch tc.name {
					case "discarded Photoshop trailing NUL":
						resource.WriteByte(0)
					case "discarded Photoshop PHUT":
						resource.Reset()
						resource.WriteString("Photoshop 3.0\x00PHUT")
					}
					if container == "Photoshop" {
						data = syntheticJPEGSegment(t, data, 0xed, resource.Bytes())
					} else {
						exif := syntheticTIFF(42, []syntheticTIFFEntry{{tag: 0x8649, kind: 7, value: bytes.TrimPrefix(resource.Bytes(), []byte("Photoshop 3.0\x00"))}}, nil)
						data = syntheticJPEGSegment(t, data, 0xe1, append([]byte("Exif\x00\x00"), exif...))
					}
				} else {
					value, kind := bytes.Clone(tc.iptc), uint16(7)
					if container == "TIFF longs" {
						kind = 4
						for len(value)%4 != 0 {
							value = append(value, 0)
						}
						for i := 0; i < len(value); i += 4 {
							slices.Reverse(value[i : i+4])
						}
					}
					exif := syntheticTIFF(42, []syntheticTIFFEntry{{tag: 0x83bb, kind: kind, value: value}}, nil)
					data = syntheticJPEGSegment(t, data, 0xe1, append([]byte("Exif\x00\x00"), exif...))
				}
				if len(tc.packet) > 0 {
					data = syntheticJPEGSegment(t, data, 0xe1, append([]byte(photoXMPJPEGPrefix), tc.packet...))
				}
				input := photoRenderInput(data, "image/jpeg")
				input.Keywords = catalog
				profile := bundle.PhotoRenderProfile{Format: "jpeg", Quality: 90, IncludeMetadata: true}
				out, _, err := renderPhotoExport(t.Context(), data, input, profile)
				if tc.invalid {
					require.ErrorIs(t, err, bundle.ErrUnavailable)
					require.ErrorContains(t, err, "turn image metadata off")
					profile.IncludeMetadata = false
					_, _, err = renderPhotoExport(t.Context(), data, input, profile)
					require.NoError(t, err)
					return
				}
				require.NoError(t, err)
				packets, err := photoSourcePackets(t.Context(), out, true, true)
				require.NoError(t, err)
				if len(tc.packet) > 0 {
					for _, retained := range []string{`dc:subject="Nested subject attribute"`, ">Nested subject element<"} {
						require.Contains(t, string(packets.xmp), retained)
					}
				}
				if len(catalog) > 0 {
					require.Contains(t, string(packets.xmp), ">catalog tag<")
					require.NotContains(t, string(packets.xmp), tc.want)
					require.NotContains(t, string(packets.xmp), "Embedded hierarchy")
				} else {
					require.Equal(t, 1, strings.Count(string(packets.xmp), ">"+tc.want+"<"))
					if len(tc.packet) > 0 {
						require.Equal(t, 1, strings.Count(string(packets.xmp), ">Embedded subject element<"))
						for _, retained := range []string{"Embedded subject attribute", "Embedded hierarchy element", "Embedded PDF attribute"} {
							require.Contains(t, string(packets.xmp), retained)
						}
					}
				}
			})
		}
	}
}

func TestPhotoExportOversizedJPEGXMP(t *testing.T) {
	t.Parallel()
	padding := strings.Repeat(" \n", 50000)
	for _, packet := range []string{
		photoSidecarHeader + ` xmlns:keep="https://example.org/photo/" xmlns:tiff="http://ns.adobe.com/tiff/1.0/" xmp:Rating="1" tiff:Orientation="6" keep:Attribute="preserve"><dc:subject><rdf:Bag><rdf:li>  Embedded keyword  </rdf:li></rdf:Bag></dc:subject><keep:History>` + strings.Repeat("x", 100000) + `</keep:History>` + photoSidecarFooter,
		padding + photoSidecarHeader + ` xmlns:keep="https://example.org/photo/" xmlns:tiff="http://ns.adobe.com/tiff/1.0/" xmp:Rating="1" tiff:Orientation="6">` + padding + `<keep:Value>  </keep:Value><dc:subject><rdf:Bag><rdf:li>  Embedded keyword  </rdf:li></rdf:Bag></dc:subject>` + photoSidecarFooter + padding,
	} {
		data := syntheticPNGChunk(t, mediatest.PNG(3, 2, color.White), "iTXt", append([]byte(photoXMPPNGKeyword+"\x00\x00\x00\x00\x00"), packet...))
		data = syntheticPNGChunk(t, data, "eXIf", syntheticTIFF(42, []syntheticTIFFEntry{tiffShort(0x0112, 6)}, nil))
		input := photoRenderInput(data, "image/png")
		input.Authored = store.PhotoAuthored{Rating: 4, Caption: strings.Repeat("c", store.MaxPhotoAuthoredTextBytes), Confirmed: store.PhotoConfirmedRating | store.PhotoConfirmedCaption}
		for _, format := range []string{"jpeg", "png"} {
			out, _, err := renderPhotoExport(t.Context(), data, input, bundle.PhotoRenderProfile{Format: format, Quality: 90, IncludeMetadata: true})
			if format == "jpeg" && strings.Contains(packet, "History") {
				require.ErrorIs(t, err, bundle.ErrLimit)
				require.ErrorContains(t, err, "choose PNG or turn image metadata off")
				continue
			}
			require.NoError(t, err)
			packets, err := photoSourcePackets(t.Context(), out, true, true)
			require.NoError(t, err)
			if !strings.Contains(packet, "History") {
				require.Contains(t, string(packets.xmp), ">  </keep:Value>")
			}
			if strings.Contains(packet, "History") {
				require.Contains(t, string(packets.xmp), strings.Repeat("x", 100000))
				require.Contains(t, string(packets.xmp), `keep:Attribute="preserve"`)
			}
		}
	}
}

func TestPhotoExportRAWSelectionAndSourceMetadataFailures(t *testing.T) {
	t.Parallel()
	t.Run("ICC fallback", func(t *testing.T) {
		data := syntheticRAWPreviewTIFFCandidates(1,
			syntheticRAWPreviewCandidate{data: mediatest.JPEG(12, 8, color.White), orientation: 1},
			syntheticRAWPreviewCandidate{data: mediatest.JPEG(3, 2, color.Black)},
		)
		firstIFD := int(binary.LittleEndian.Uint32(data[22:26]))
		for index := range int(binary.LittleEndian.Uint16(data[firstIFD:])) {
			entry := firstIFD + 2 + index*12
			if binary.LittleEndian.Uint16(data[entry:]) == visualPreviewRAWOrientationTag {
				binary.LittleEndian.PutUint16(data[entry:], visualPreviewRAWICCTag)
			}
		}
		locations, malformed, err := visualPreviewRAWLocations(bytes.NewReader(data), "image/x-adobe-dng", int64(len(data)))
		require.NoError(t, err)
		require.False(t, malformed)
		require.Len(t, locations, 2)
		require.True(t, locations[0].previewICC)
		_, receipt, err := renderPhotoExport(t.Context(), data, photoRenderInput(data, "image/x-adobe-dng"), bundle.PhotoRenderProfile{Format: "jpeg", Quality: 90, IncludeMetadata: true})
		require.NoError(t, err)
		require.True(t, receipt.EmbeddedPreview)
		require.Equal(t, 3, receipt.Width)
		require.Equal(t, 2, receipt.Height)
	})
	preview := mediatest.JPEG(3, 2, color.White)
	rawData := func(extra ...syntheticTIFFEntry) []byte {
		entries := []syntheticTIFFEntry{tiffLong(0x00fe, 1), tiffShort(0x00ff, 2), tiffShort(0x0112, 6), tiffASCII(0x010f, "Synthetic Camera"), tiffShort(0x0102, 16), tiffShort(0x0103, 7), tiffShort(0x0106, 32803), tiffLong(0xc612, 0x00000401), tiffShort(0x828e, 1), tiffLong(0x0201, 0), tiffLong(0x0202, uint32(len(preview)))}
		data := syntheticTIFF(42, append(entries, extra...), nil)
		for index := range int(binary.LittleEndian.Uint16(data[8:])) {
			entry := 10 + index*12
			if binary.LittleEndian.Uint16(data[entry:]) == 0x0201 {
				binary.LittleEndian.PutUint32(data[entry+8:], uint32(len(data)))
			}
		}
		return append(data, preview...)
	}
	xmp := []byte(photoSidecarHeader + ` xmlns:tiff="http://ns.adobe.com/tiff/1.0/" tiff:Orientation="6">` + photoSidecarFooter)
	data := rawData(syntheticTIFFEntry{tag: 700, kind: 1, value: xmp})
	in := photoRenderInput(data, "image/x-adobe-dng")
	out, receipt, err := renderPhotoExport(t.Context(), data, in, bundle.PhotoRenderProfile{Format: "png", Quality: 90, IncludeMetadata: true})
	require.NoError(t, err)
	assert.True(t, receipt.EmbeddedPreview)
	assert.Equal(t, 2, receipt.Width)
	assert.Equal(t, 3, receipt.Height)
	packets, err := photoSourcePackets(t.Context(), out, true, true)
	require.NoError(t, err)
	reader, ok := newExifReader(packets.exif)
	require.True(t, ok)
	require.Contains(t, string(packets.xmp), `Orientation="1"`)
	assert.Equal(t, uint16(1), reader.order.Uint16(reader.entries(reader.u32(4))[0x0112]))
	root := reader.entries(reader.u32(4))
	for _, tag := range []uint16{0x00fe, 0x00ff, 0x0102, 0x0103, 0x0106, 0xc612, 0x828e, 0x0201, 0x0202} {
		assert.NotContains(t, root, tag)
	}
	assert.Equal(t, "Synthetic Camera", exifASCII(root[0x010f]))
	rootICC := syntheticPhotoICC()
	previewICC := append([]byte(nil), rootICC...)
	whitePoint := int(binary.BigEndian.Uint32(previewICC[136:]))
	previewICC[whitePoint+19] ^= 1
	originalPreview := preview
	preview = syntheticJPEGSegment(t, preview, 0xe2, append([]byte("ICC_PROFILE\x00\x01\x01"), previewICC...))
	type colorCase struct {
		name, failure string
		data          []byte
	}
	cases := []colorCase{
		{"embedded ICC", "", rawData(syntheticTIFFEntry{tag: 34675, kind: 7, value: rootICC})},
	}
	preview = originalPreview
	cases = append(cases, colorCase{"unassociated root ICC", "not associated", rawData(syntheticTIFFEntry{tag: 34675, kind: 7, value: rootICC})})
	for _, subIFD := range []bool{false, true} {
		for _, strip := range []bool{false, true} {
			previewProfileRAW := rawData(syntheticTIFFEntry{tag: 34675, kind: 7, value: rootICC})
			if strip {
				for index := range int(binary.LittleEndian.Uint16(previewProfileRAW[8:])) {
					entry := 10 + index*12
					switch binary.LittleEndian.Uint16(previewProfileRAW[entry:]) {
					case visualPreviewRAWOffsetTag:
						binary.LittleEndian.PutUint16(previewProfileRAW[entry:], visualPreviewRAWStripOffsetsTag)
					case visualPreviewRAWLengthTag:
						binary.LittleEndian.PutUint16(previewProfileRAW[entry:], visualPreviewRAWStripByteCountsTag)
					}
				}
			}
			root := syntheticTIFFRoot(nil)[8:]
			if subIFD {
				root = syntheticTIFFRoot([]syntheticTIFFEntry{tiffLong(visualPreviewRAWSubIFDsTag, 8)})[8:]
			} else {
				binary.LittleEndian.PutUint32(root[2:], 8)
			}
			binary.LittleEndian.PutUint32(previewProfileRAW[4:], uint32(len(previewProfileRAW)))
			previewProfileRAW = append(previewProfileRAW, root...)
			cases = append(cases, colorCase{fmt.Sprintf("preview ICC/subIFD=%t/strip=%t", subIFD, strip), "preview IFD color profile", previewProfileRAW})
		}
	}
	for _, test := range cases {
		if test.failure != "" {
			t.Run(test.name, func(t *testing.T) {
				_, _, err := renderPhotoExport(t.Context(), test.data, photoRenderInput(test.data, "image/x-adobe-dng"), bundle.PhotoRenderProfile{Format: "jpeg", Quality: 90})
				require.ErrorIs(t, err, bundle.ErrUnavailable)
				require.ErrorContains(t, err, test.failure)
			})
			continue
		}
		for _, format := range []string{"jpeg", "png"} {
			for _, metadata := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/metadata=%t", test.name, format, metadata), func(t *testing.T) {
					input := photoRenderInput(test.data, "image/x-adobe-dng")
					input.Name = "synthetic-preview.dng"
					out, _, err := renderPhotoExport(t.Context(), test.data, input, bundle.PhotoRenderProfile{Format: format, Quality: 90, IncludeMetadata: metadata})
					require.NoError(t, err)
					packets, err := photoSourcePackets(t.Context(), out, true, true)
					require.NoError(t, err)
					require.Equal(t, previewICC, packets.icc)
					if metadata {
						reader, ok := newExifReader(packets.exif)
						require.True(t, ok)
						require.NotContains(t, reader.entries(reader.u32(4)), uint16(34675))
					}
				})
			}
		}
	}
	missing := syntheticRAWPreviewTIFF(1)
	_, _, err = renderPhotoExport(t.Context(), missing, photoRenderInput(missing, "image/x-adobe-dng"), bundle.PhotoRenderProfile{Format: "jpeg", Quality: 90})
	require.ErrorIs(t, err, bundle.ErrUnavailable)
	for _, packet := range []string{`<x:xmpmeta xmlns:x="adobe:ns:meta/"><broken>`, photoSidecarHeader + `><xmp:Thumbnails><bad:Credit/></xmp:Thumbnails>` + photoSidecarFooter} {
		bad := syntheticJPEGSegment(t, preview, 0xe1, append([]byte(photoXMPJPEGPrefix), []byte(packet)...))
		input := photoRenderInput(bad, "image/jpeg")
		_, _, err = renderPhotoExport(t.Context(), bad, input, bundle.PhotoRenderProfile{Format: "jpeg", Quality: 90, IncludeMetadata: true})
		require.ErrorIs(t, err, bundle.ErrUnavailable)
		_, _, err = renderPhotoExport(t.Context(), bad, input, bundle.PhotoRenderProfile{Format: "jpeg", Quality: 90})
		require.NoError(t, err)
	}
	extensionPacket := []byte(`<x:xmpmeta xmlns:x="adobe:ns:meta/"><rdf:RDF xmlns:rdf="http://www.w3.org/1999/02/22-rdf-syntax-ns#"><rdf:Description xmlns:keep="https://example.org/photo/" keep:Credit="Extension credit"/></rdf:RDF></x:xmpmeta>`)
	const extensionID = "45A0694DACBE36A91C27BDAFB8FF161B"
	extension := append([]byte("http://ns.adobe.com/xmp/extension/\x00"+extensionID), make([]byte, 8)...)
	binary.BigEndian.PutUint32(extension[len(extension)-8:], uint32(len(extensionPacket)))
	extension = append(extension, extensionPacket...)
	for _, property := range []string{` xmpNote:HasExtendedXMP="` + extensionID + `">`, `><xmpNote:HasExtendedXMP>` + extensionID + `</xmpNote:HasExtendedXMP>`, `>`} {
		for _, segment := range []bool{false, true} {
			if property == ">" && !segment {
				continue
			}
			mainPacket := []byte(photoSidecarHeader + ` xmp:Rating="2" xmlns:xmpNote="http://ns.adobe.com/xmp/note/"` + property + photoSidecarFooter)
			extended := syntheticJPEGSegment(t, preview, 0xe1, append([]byte(photoXMPJPEGPrefix), mainPacket...))
			if segment {
				extended = syntheticJPEGSegment(t, extended, 0xe1, extension)
			}
			for _, metadata := range []bool{true, false} {
				_, _, err = renderPhotoExport(t.Context(), extended, photoRenderInput(extended, "image/jpeg"), bundle.PhotoRenderProfile{Format: "jpeg", Quality: 90, IncludeMetadata: metadata})
				if metadata {
					require.ErrorIs(t, err, bundle.ErrUnavailable)
					require.ErrorContains(t, err, "extended XMP cannot be retained; turn image metadata off")
				} else {
					require.NoError(t, err)
				}
			}
		}
	}
	oversized := bytes.Repeat([]byte{'x'}, maxPhotoSidecarBytes+1)
	for _, tag := range []uint16{34675, 0x010e} {
		raw := rawData(syntheticTIFFEntry{tag: tag, kind: 7, value: oversized})
		// Ordinary extraction remains lenient while export refuses retained values it cannot read.
		reader, ok := newExifReader(raw)
		require.True(t, ok)
		_, ok = reader.typedEntries(reader.u32(4))
		require.True(t, ok)
		for _, metadata := range []bool{true, false} {
			_, _, err = renderPhotoExport(t.Context(), raw, photoRenderInput(raw, "image/x-adobe-dng"), bundle.PhotoRenderProfile{Format: "png", Quality: 90, IncludeMetadata: metadata})
			if tag == 0x010e && !metadata {
				require.NoError(t, err)
			} else {
				require.ErrorIs(t, err, bundle.ErrLimit)
				require.ErrorContains(t, err, fmt.Sprintf("EXIF tag 0x%04x exceeds 1 MiB", tag))
			}
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
		chunks       int
	}{
		{"png", "eXIf", oversized, false, 1},
		{"png", "iTXt", append([]byte(photoXMPPNGKeyword+"\x00\x00\x00\x00\x00"), oversized...), true, 1},
		{"png", "iTXt", append([]byte(photoXMPPNGKeyword+"\x00\x01\x00\x00\x00"), compressed.Bytes()...), true, 1},
		{"png", "iTXt", append([]byte(photoXMPPNGKeyword+"\x00\x00\x00"), oversized...), true, 1},
		{"png", "iCCP", append([]byte("Profile\x00\x00"), compressed.Bytes()...), false, 1},
		{"png", "tEXt", []byte("synthetic\x00metadata"), false, visualPreviewMaxPNGChunks + 1},
		{"webp", "EXIF", oversized, false, 1},
		{"webp", "ICCP", oversized, false, 1},
		{"webp", "XMP ", oversized, true, 1},
	} {
		var container bytes.Buffer
		if test.format == "png" {
			container.WriteString("\x89PNG\r\n\x1a\n")
			for range test.chunks {
				writePhotoPNGChunk(&container, test.kind, test.payload)
			}
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
		_, err = photoSourcePackets(t.Context(), container.Bytes(), true, true)
		require.ErrorIs(t, err, errVisualMetadataLimit, test.kind)
		_, err = photoSourcePackets(t.Context(), container.Bytes(), false, false)
		if test.skipped {
			require.NoError(t, err, test.kind)
		} else {
			require.ErrorIs(t, err, errVisualMetadataLimit, test.kind)
		}
	}
}

func syntheticPhotoICC() []byte {
	return syntheticICC("RGB ", []string{"wtpt", "rXYZ", "gXYZ", "bXYZ", "rTRC", "gTRC", "bTRC"})
}

func syntheticGrayPhotoICC() []byte {
	return syntheticICC("GRAY", []string{"wtpt", "kTRC"})
}

func syntheticICC(space string, tags []string) []byte {
	profile := make([]byte, 0)
	profile = append(profile, make([]byte, 132+12*len(tags))...)
	copy(profile[12:16], "mntr")
	copy(profile[16:20], space)
	copy(profile[20:24], "XYZ ")
	copy(profile[36:40], "acsp")
	binary.BigEndian.PutUint32(profile[128:], uint32(len(tags)))
	for i, tag := range tags {
		entry := profile[132+i*12:]
		copy(entry, tag)
		binary.BigEndian.PutUint32(entry[4:], uint32(len(profile)))
		var value []byte
		if !strings.HasSuffix(tag, "TRC") {
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
	input := photoRenderInput(source.Bytes(), "image/png")
	output, receipt, err := renderPhotoExport(t.Context(), source.Bytes(), input, bundle.PhotoRenderProfile{Format: "png", Quality: 90})
	require.NoError(t, err)
	require.Equal(t, 65, receipt.Width)
	require.NotEmpty(t, output)
}

func TestPhotoExportGrayProfileKeepsGrayscaleOutput(t *testing.T) {
	t.Parallel()
	gray := image.NewGray(image.Rect(0, 0, 3, 2))
	for i := range gray.Pix {
		gray.Pix[i] = byte(40 * i)
	}
	var encoded bytes.Buffer
	require.NoError(t, jpeg.Encode(&encoded, gray, &jpeg.Options{Quality: 100}))
	profile := syntheticGrayPhotoICC()
	segment := append([]byte("ICC_PROFILE\x00\x01\x01"), profile...)
	source := syntheticJPEGSegment(t, encoded.Bytes(), 0xe2, segment)
	for _, format := range []string{"jpeg", "png"} {
		for _, metadata := range []bool{false, true} {
			profileSettings := bundle.PhotoRenderProfile{Format: format, Quality: 90, IncludeMetadata: metadata}
			out, _, err := renderPhotoExport(t.Context(), source, photoRenderInput(source, "image/jpeg"), profileSettings)
			require.NoError(t, err, "%s metadata=%t", format, metadata)
			packets, err := photoSourcePackets(t.Context(), out, true, true)
			require.NoError(t, err)
			assert.Equal(t, profile, packets.icc)
			decoded, _, err := image.Decode(bytes.NewReader(out))
			require.NoError(t, err)
			assert.IsType(t, &image.Gray{}, decoded, "%s metadata=%t", format, metadata)
		}
	}
	colored := syntheticJPEGSegment(t, mediatest.JPEG(3, 2, color.RGBA{R: 200, G: 20, B: 20, A: 255}), 0xe2, segment)
	_, _, err := renderPhotoExport(t.Context(), colored, photoRenderInput(colored, "image/jpeg"), bundle.PhotoRenderProfile{Format: "png"})
	require.ErrorIs(t, err, bundle.ErrUnavailable)
	require.ErrorContains(t, err, "gray color profile")
}

func TestPhotoExportDropsDNGCalibrationMetadata(t *testing.T) {
	t.Parallel()
	preview := mediatest.JPEG(3, 2, color.White)
	calibration := []syntheticTIFFEntry{
		{tag: 0xc621, kind: 10, value: make([]byte, 72)},
		{tag: 0xc623, kind: 10, value: make([]byte, 72)},
		{tag: 0xc628, kind: 5, value: make([]byte, 24)},
		{tag: 0xc65a, kind: 3, value: []byte{21, 0}},
		{tag: 0xc6f9, kind: 4, value: make([]byte, 12)},
		{tag: 0xc6fa, kind: 11, value: make([]byte, 40000)},
		{tag: 0xc714, kind: 10, value: make([]byte, 72)},
		{tag: 0xc725, kind: 4, value: make([]byte, 12)},
		{tag: 0xc726, kind: 11, value: make([]byte, 40000)},
		{tag: 0xc7a5, kind: 10, value: make([]byte, 8)},
		{tag: 0xcd33, kind: 10, value: make([]byte, 72)},
	}
	entries := append([]syntheticTIFFEntry{tiffASCII(0xc614, "Synthetic Camera"), tiffLong(0x0201, 0), tiffLong(0x0202, uint32(len(preview)))}, calibration...)
	data := syntheticTIFF(42, entries, nil)
	for index := range int(binary.LittleEndian.Uint16(data[8:])) {
		entry := 10 + index*12
		if binary.LittleEndian.Uint16(data[entry:]) == 0x0201 {
			binary.LittleEndian.PutUint32(data[entry+8:], uint32(len(data)))
		}
	}
	data = append(data, preview...)
	out, receipt, err := renderPhotoExport(t.Context(), data, photoRenderInput(data, "image/x-adobe-dng"), bundle.PhotoRenderProfile{Format: "jpeg", Quality: 90, IncludeMetadata: true})
	require.NoError(t, err)
	require.True(t, receipt.EmbeddedPreview)
	packets, err := photoSourcePackets(t.Context(), out, true, true)
	require.NoError(t, err)
	reader, ok := newExifReader(packets.exif)
	require.True(t, ok)
	root := reader.entries(reader.u32(4))
	for _, entry := range calibration {
		assert.NotContains(t, root, entry.tag, "tag 0x%04x", entry.tag)
	}
	assert.Equal(t, "Synthetic Camera", exifASCII(root[0xc614]))
}

func TestPhotoExportOversizedRAWTags(t *testing.T) {
	t.Parallel()
	preview := mediatest.JPEG(3, 2, color.White)
	raw := func(extra syntheticTIFFEntry) []byte {
		data := syntheticTIFF(42, []syntheticTIFFEntry{tiffLong(0x0201, 0), tiffLong(0x0202, uint32(len(preview))), extra}, nil)
		for index := range int(binary.LittleEndian.Uint16(data[8:])) {
			entry := 10 + index*12
			if binary.LittleEndian.Uint16(data[entry:]) == 0x0201 {
				binary.LittleEndian.PutUint32(data[entry+8:], uint32(len(data)))
			}
		}
		return append(data, preview...)
	}
	oversized := bytes.Repeat([]byte{'x'}, 1<<20+16)
	withMetadata := bundle.PhotoRenderProfile{Format: "jpeg", Quality: 90, IncludeMetadata: true}

	stripped := raw(syntheticTIFFEntry{tag: 0xc68c, kind: 7, value: oversized})
	out, _, err := renderPhotoExport(t.Context(), stripped, photoRenderInput(stripped, "image/x-adobe-dng"), withMetadata)
	require.NoError(t, err)
	packets, err := photoSourcePackets(t.Context(), out, true, true)
	require.NoError(t, err)
	reader, ok := newExifReader(packets.exif)
	require.True(t, ok)
	assert.NotContains(t, reader.entries(reader.u32(4)), uint16(0xc68c))

	retained := raw(syntheticTIFFEntry{tag: 0x0131, kind: 2, value: oversized})
	_, _, err = renderPhotoExport(t.Context(), retained, photoRenderInput(retained, "image/x-adobe-dng"), withMetadata)
	require.ErrorIs(t, err, bundle.ErrLimit)
	require.ErrorContains(t, err, "EXIF tag 0x0131 exceeds 1 MiB; turn image metadata off")
	_, _, err = renderPhotoExport(t.Context(), retained, photoRenderInput(retained, "image/x-adobe-dng"), bundle.PhotoRenderProfile{Format: "jpeg", Quality: 90})
	require.NoError(t, err, "metadata off ignores retained tags")

	iptc := raw(syntheticTIFFEntry{tag: 0x83bb, kind: 7, value: oversized})
	_, _, err = renderPhotoExport(t.Context(), iptc, photoRenderInput(iptc, "image/x-adobe-dng"), withMetadata)
	require.ErrorIs(t, err, bundle.ErrLimit, "embedded keywords would be lost")
	tagged := photoRenderInput(iptc, "image/x-adobe-dng")
	tagged.Keywords = []string{"catalog"}
	out, _, err = renderPhotoExport(t.Context(), iptc, tagged, withMetadata)
	require.NoError(t, err, "catalog tags replace the embedded keywords")
	packets, err = photoSourcePackets(t.Context(), out, true, true)
	require.NoError(t, err)
	reader, ok = newExifReader(packets.exif)
	require.True(t, ok)
	assert.NotContains(t, reader.entries(reader.u32(4)), uint16(0x83bb))
}

func TestPhotoExportXMPKeepsThePhotoSubject(t *testing.T) {
	t.Parallel()
	const rdfNS = `xmlns:rdf="http://www.w3.org/1999/02/22-rdf-syntax-ns#"`
	const xmpNS = `xmlns:xmp="http://ns.adobe.com/xap/1.0/"`
	packet := func(subject string) []byte {
		return []byte(`<x:xmpmeta xmlns:x="adobe:ns:meta/"><rdf:RDF ` + rdfNS + `>` +
			`<rdf:Description ` + subject + ` ` + xmpNS + ` xmp:Rating="2"/>` +
			`<rdf:Description rdf:about="uuid:other" ` + xmpNS + ` xmp:Rating="1"/></rdf:RDF></x:xmpmeta>`)
	}
	render := func(subject string) ([]byte, error) {
		source := syntheticJPEGSegment(t, mediatest.JPEG(3, 2, color.White), 0xe1, append([]byte(photoXMPJPEGPrefix), packet(subject)...))
		input := photoRenderInput(source, "image/jpeg")
		input.Authored = store.PhotoAuthored{Rating: 5, Confirmed: store.PhotoConfirmedRating}
		out, _, err := renderPhotoExport(t.Context(), source, input, bundle.PhotoRenderProfile{Format: "png", IncludeMetadata: true})
		if err != nil {
			return nil, err
		}
		packets, err := photoSourcePackets(t.Context(), out, true, true)
		require.NoError(t, err)
		return packets.xmp, nil
	}
	for _, subject := range []string{`rdf:about="uuid:photo"`, `rdf:nodeID="photo"`} {
		exported, err := render(subject)
		require.NoError(t, err, subject)
		authored, err := ReadPhotoSidecar(t.Context(), exported)
		require.NoError(t, err, subject)
		assert.Equal(t, 5, authored.Rating, "%s: the authored rating must describe the photo subject", subject)
		assert.Contains(t, string(exported), `uuid:other`)
		assert.Regexp(t, `about="uuid:other"[^>]*Rating="1"`, string(exported), "%s: another subject keeps its rating", subject)
	}
	_, err := render(`rdf:ID="photo"`)
	require.ErrorIs(t, err, bundle.ErrUnavailable)
	require.ErrorContains(t, err, "rdf:ID")
}

func TestPhotoExportCarriesGIFProfileAndXMP(t *testing.T) {
	t.Parallel()
	frame := image.NewPaletted(image.Rect(0, 0, 3, 2), color.Palette{color.Black, color.White})
	var encoded bytes.Buffer
	require.NoError(t, gif.Encode(&encoded, frame, nil))
	plain := encoded.Bytes()
	tableBytes := 0
	if plain[10]&0x80 != 0 {
		tableBytes = 3 << (plain[10]&0x07 + 1)
	}
	profile := syntheticPhotoICC()
	var extensions bytes.Buffer
	extensions.WriteString("\x21\xff\x0bICCRGBG1012")
	for rest := profile; len(rest) > 0; {
		size := min(255, len(rest))
		extensions.WriteByte(byte(size))
		extensions.Write(rest[:size])
		rest = rest[size:]
	}
	extensions.WriteByte(0)
	packet := photoSidecarHeader + ` dc:rights="GIF rights">` + photoSidecarFooter
	extensions.WriteString("\x21\xff\x0bXMP DataXMP")
	extensions.WriteString(packet)
	extensions.WriteByte(1)
	for value := 255; value >= 0; value-- {
		extensions.WriteByte(byte(value))
	}
	extensions.WriteByte(0)
	source := append(append(bytes.Clone(plain[:13+tableBytes]), extensions.Bytes()...), plain[13+tableBytes:]...)

	for _, metadata := range []bool{false, true} {
		out, _, err := renderPhotoExport(t.Context(), source, photoRenderInput(source, "image/gif"), bundle.PhotoRenderProfile{Format: "png", IncludeMetadata: metadata})
		require.NoError(t, err)
		packets, err := photoSourcePackets(t.Context(), out, true, true)
		require.NoError(t, err)
		assert.Equal(t, profile, packets.icc, "metadata=%t", metadata)
		if !metadata {
			assert.Empty(t, packets.xmp)
			continue
		}
		authored, err := ReadPhotoSidecar(t.Context(), packets.xmp)
		require.NoError(t, err)
		assert.Equal(t, "GIF rights", authored.Copyright)
	}
	damaged := append(bytes.Clone(plain[:len(plain)-1]), extensions.Bytes()[:20]...)
	_, _, err := renderPhotoExport(t.Context(), damaged, photoRenderInput(damaged, "image/gif"), bundle.PhotoRenderProfile{Format: "png"})
	require.ErrorIs(t, err, bundle.ErrUnavailable, "a truncated profile after a decodable frame must not be dropped silently")
	var oversized bytes.Buffer
	oversized.Write(plain[:13+tableBytes])
	oversized.WriteString("\x21\xff\x0bXMP DataXMP")
	oversized.Write(bytes.Repeat([]byte{' '}, maxPhotoSidecarBytes+1))
	oversized.Write(gifXMPTrailer)
	oversized.Write(plain[13+tableBytes:])
	_, _, err = renderPhotoExport(t.Context(), oversized.Bytes(), photoRenderInput(oversized.Bytes(), "image/gif"), bundle.PhotoRenderProfile{Format: "png"})
	require.NoError(t, err, "metadata off discards the XMP regardless of size")
	_, _, err = renderPhotoExport(t.Context(), oversized.Bytes(), photoRenderInput(oversized.Bytes(), "image/gif"), bundle.PhotoRenderProfile{Format: "png", IncludeMetadata: true})
	require.ErrorIs(t, err, bundle.ErrUnavailable)
	withoutTrailer := append(bytes.Clone(plain[:len(plain)-1]), "junk after the frame"...)
	_, receipt, err := renderPhotoExport(t.Context(), withoutTrailer, photoRenderInput(withoutTrailer, "image/gif"), bundle.PhotoRenderProfile{Format: "png"})
	require.NoError(t, err, "a GIF the decoder accepts stays exportable")
	assert.Equal(t, 3, receipt.Width)
}

func TestPhotoExportRefusesJPEGBeyondItsDimensionLimit(t *testing.T) {
	t.Parallel()
	var encoded bytes.Buffer
	require.NoError(t, png.Encode(&encoded, image.NewGray(image.Rect(0, 0, 65536, 1))))
	source := encoded.Bytes()
	input := photoRenderInput(source, "image/png")
	_, _, err := renderPhotoExport(t.Context(), source, input, bundle.PhotoRenderProfile{Format: "jpeg", Quality: 90})
	require.ErrorIs(t, err, bundle.ErrLimit)
	require.ErrorContains(t, err, "65,535 pixels")
	_, receipt, err := renderPhotoExport(t.Context(), source, input, bundle.PhotoRenderProfile{Format: "jpeg", Quality: 90, LongEdge: 65535})
	require.NoError(t, err)
	assert.Equal(t, 65535, receipt.Width)
	_, _, err = renderPhotoExport(t.Context(), source, input, bundle.PhotoRenderProfile{Format: "png"})
	require.NoError(t, err)
}
