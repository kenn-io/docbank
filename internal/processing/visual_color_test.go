package processing

import (
	"bytes"
	"compress/zlib"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"image"
	"image/color"
	"image/jpeg"
	"testing"

	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/media/mediatest"
)

var syntheticVisualPreviewP3 = [3][3]float64{
	{0.515102, 0.241182, -0.001049}, {0.291965, 0.692236, 0.041882}, {0.157153, 0.066581, 0.784378},
}

var syntheticVisualPreviewSRGB = [3][3]float64{
	{0.4360747, 0.2225045, 0.0139322}, {0.3850649, 0.7168786, 0.0971045}, {0.1430804, 0.0606169, 0.7141733},
}

// Expected sRGB values for RGB (200,100,50) were computed from the D65
// P3-to-sRGB and Adobe-RGB-to-sRGB matrices, independently of the D50 path.
var (
	visualPreviewP3Expected    = color.NRGBA{R: 215, G: 93, B: 31, A: 127}
	visualPreviewAdobeExpected = color.NRGBA{R: 227, G: 100, B: 42, A: 127}
	visualPreviewUnconverted   = color.NRGBA{R: 200, G: 100, B: 50, A: 127}
)

func TestVisualPreviewColorConversion(t *testing.T) {
	t.Parallel()
	p3 := syntheticVisualPreviewICC(syntheticVisualPreviewP3, true)
	adobe := syntheticVisualPreviewICC([3][3]float64{
		{0.6097559, 0.3111242, 0.0194811}, {0.2052401, 0.625656, 0.0608902}, {0.149224, 0.0632197, 0.7448387},
	}, false)
	binary.BigEndian.PutUint32(adobe[8:12], 0x02100000)
	hybrid := syntheticVisualPreviewICC(syntheticVisualPreviewP3, true,
		syntheticICCTag{"A2B0", []byte("mft2\x00\x00\x00\x00")})
	for _, test := range []struct {
		name  string
		space visualPreviewColorSpace
		want  color.NRGBA
	}{
		{"Display P3 v4 profile", visualPreviewColorSpace{profile: p3}, visualPreviewP3Expected},
		{"Adobe RGB v2 profile", visualPreviewColorSpace{profile: adobe}, visualPreviewAdobeExpected},
		{"Adobe RGB EXIF marker", visualPreviewColorSpace{adobeRGB: true}, visualPreviewAdobeExpected},
		{"profile with LUT and matrix", visualPreviewColorSpace{profile: hybrid}, visualPreviewP3Expected},
		{"profile wins over EXIF", visualPreviewColorSpace{profile: p3, adobeRGB: true}, visualPreviewP3Expected},
		{
			"sRGB profile wins over EXIF",
			visualPreviewColorSpace{profile: syntheticVisualPreviewICC(syntheticVisualPreviewSRGB, true), adobeRGB: true},
			visualPreviewUnconverted,
		},
		{"no color information", visualPreviewColorSpace{}, visualPreviewUnconverted},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			pixels := image.NewNRGBA(image.Rect(0, 0, 1, 1))
			pixels.SetNRGBA(0, 0, visualPreviewUnconverted)
			require.NoError(t, applyVisualPreviewColor(t.Context(), pixels, test.space.transform()))
			got := pixels.NRGBAAt(0, 0)
			if test.want == visualPreviewUnconverted {
				require.Equal(t, test.want, got)
				return
			}
			require.InDelta(t, test.want.R, got.R, 1)
			require.InDelta(t, test.want.G, got.G, 1)
			require.InDelta(t, test.want.B, got.B, 1)
			require.Equal(t, test.want.A, got.A)
		})
	}
}

func TestVisualPreviewColorStopsWhenCanceled(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	pixels := image.NewNRGBA(image.Rect(0, 0, 1, 1))
	pixels.SetNRGBA(0, 0, visualPreviewUnconverted)
	err := applyVisualPreviewColor(ctx, pixels, visualPreviewColorSpace{adobeRGB: true}.transform())
	require.ErrorIs(t, err, context.Canceled)
	require.Equal(t, visualPreviewUnconverted, pixels.NRGBAAt(0, 0))
}

func TestVisualPreviewVendorSRGBProfileIsSRGB(t *testing.T) {
	t.Parallel()
	// Colorants of the HP "sRGB IEC61966-2.1" profile, as stored s15Fixed16 values.
	hp := syntheticVisualPreviewICC([3][3]float64{
		{0x6fa2 / 65536.0, 0x38f5 / 65536.0, 0x0390 / 65536.0},
		{0x6299 / 65536.0, 0xb785 / 65536.0, 0x18da / 65536.0},
		{0x24a0 / 65536.0, 0x0f84 / 65536.0, 0xb6cf / 65536.0},
	}, true)
	transform, ok := parseVisualPreviewICC(hp)
	require.True(t, ok)
	require.Nil(t, transform, "an sRGB profile must leave pixels unchanged")
}

func TestProduceVisualPreviewConvertsEmbeddedColor(t *testing.T) {
	t.Parallel()
	pixel := color.NRGBA{R: 200, G: 100, B: 50, A: 255}
	profile := syntheticVisualPreviewICC(syntheticVisualPreviewP3, true)
	parts := [][]byte{profile[:100], profile[100:]}
	segment := func(sequence int) []byte {
		header := append([]byte("ICC_PROFILE\x00"), byte(sequence), byte(len(parts)))
		return append(header, parts[sequence-1]...)
	}
	jpegSource := mediatest.JPEG(3, 2, pixel)
	// syntheticJPEGSegment inserts after SOI, so the second insert comes first in the file.
	jpegProfile := func(first, second int) []byte {
		source := syntheticJPEGSegment(t, jpegSource, 0xe2, segment(second))
		return syntheticJPEGSegment(t, source, 0xe2, segment(first))
	}
	pngSource := mediatest.PNG(3, 2, pixel)
	webpSource, err := base64.StdEncoding.DecodeString("UklGRh4AAABXRUJQVlA4TBEAAAAvAkAAAAdQsiJXpv+BiOh/AAA=")
	require.NoError(t, err)
	oddProfile := append(bytes.Clone(profile), 0)
	adobeEXIF := syntheticInteropEXIF(0xffff, "R03")
	jpegEXIF := func(tiff []byte) []byte {
		return syntheticJPEGSegment(t, jpegSource, 0xe1, append([]byte("Exif\x00\x00"), tiff...))
	}
	const (
		unchanged = iota
		p3
		adobe
	)
	for _, test := range []struct {
		name, mediaType  string
		source, baseline []byte
		want             int
	}{
		{"jpeg profile", "image/jpeg", jpegProfile(1, 2), jpegSource, p3},
		{"jpeg profile segments out of order", "image/jpeg", jpegProfile(2, 1), jpegSource, p3},
		{"jpeg Adobe RGB EXIF", "image/jpeg", jpegEXIF(adobeEXIF), jpegSource, adobe},
		{"jpeg uncalibrated without R03", "image/jpeg", jpegEXIF(syntheticInteropEXIF(0xffff, "R98")), jpegSource, unchanged},
		{"jpeg sRGB ColorSpace with R03", "image/jpeg", jpegEXIF(syntheticInteropEXIF(1, "R03")), jpegSource, unchanged},
		{"png profile", "image/png", syntheticPNGChunk(t, pngSource, "iCCP", syntheticVisualPreviewICCP(t, profile)), pngSource, p3},
		{"png Adobe RGB EXIF", "image/png", syntheticPNGChunk(t, pngSource, "eXIf", adobeEXIF), pngSource, adobe},
		{"png malformed profile", "image/png", syntheticPNGChunk(t, pngSource, "iCCP", []byte("profile")), pngSource, unchanged},
		{
			"png oversized profile chunk", "image/png",
			syntheticPNGChunk(t, pngSource, "iCCP", make([]byte, visualPreviewMaxICCBytes+1)), pngSource, unchanged,
		},
		{"webp odd-length profile", "image/webp", syntheticExtendedWebP(t, webpSource, 3, 2, visualPreviewWebPICCProfile, "ICCP", oddProfile), webpSource, p3},
		{"webp profile without VP8X flag", "image/webp", syntheticExtendedWebP(t, webpSource, 3, 2, 0, "ICCP", profile), webpSource, unchanged},
		{
			"webp oversized profile chunk", "image/webp",
			syntheticExtendedWebP(t, webpSource, 3, 2, visualPreviewWebPICCProfile, "ICCP", make([]byte, visualPreviewMaxICCBytes+1)),
			webpSource, unchanged,
		},
		{"nef embedded jpeg profile", "image/x-nikon-nef", syntheticRAWPreviewTIFF(1, jpegProfile(1, 2)), syntheticRAWPreviewTIFF(1, jpegSource), p3},
		{"nef embedded jpeg Adobe RGB EXIF", "image/x-nikon-nef", syntheticRAWPreviewTIFF(1, jpegEXIF(adobeEXIF)), syntheticRAWPreviewTIFF(1, jpegSource), adobe},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			produce := func(source []byte) []byte {
				digest := sha256.Sum256(source)
				product, err := ProduceVisualPreview(t.Context(), bytes.NewReader(source), VisualPreviewTarget{
					SourceSHA256: hex.EncodeToString(digest[:]), Size: int64(len(source)), MediaType: test.mediaType,
				})
				require.NoError(t, err)
				require.Equal(t, document.VisualPreviewReady, product.Preview.State)
				return product.Output
			}
			output := produce(test.source)
			if test.want == unchanged {
				require.Equal(t, produce(test.baseline), output)
				return
			}
			want := map[int]color.NRGBA{p3: visualPreviewP3Expected, adobe: visualPreviewAdobeExpected}[test.want]
			decoded, err := jpeg.Decode(bytes.NewReader(output))
			require.NoError(t, err)
			r, g, b, _ := decoded.At(1, 1).RGBA()
			require.InDelta(t, want.R, r>>8, 4)
			require.InDelta(t, want.G, g>>8, 4)
			require.InDelta(t, want.B, b>>8, 4)
		})
	}
}

func TestVisualPreviewColorFallback(t *testing.T) {
	t.Parallel()
	valid := syntheticVisualPreviewICC(syntheticVisualPreviewP3, true)
	for _, test := range []struct {
		name    string
		profile []byte
	}{
		{"truncated", valid[:100]},
		{"zero version", mutateVisualPreviewICC(valid, func(p []byte) { binary.BigEndian.PutUint32(p[8:12], 0) })},
		{"old version", mutateVisualPreviewICC(valid, func(p []byte) { p[8] = 1 })},
		{"version 3", mutateVisualPreviewICC(valid, func(p []byte) { p[8] = 3 })},
		{"new version", mutateVisualPreviewICC(valid, func(p []byte) { p[8] = 5 })},
		{"gray", mutateVisualPreviewICC(valid, func(p []byte) { copy(p[16:20], "GRAY") })},
		{"CMYK", mutateVisualPreviewICC(valid, func(p []byte) { copy(p[16:20], "CMYK") })},
		{"LUT only", syntheticVisualPreviewICCTags([]syntheticICCTag{{"A2B0", []byte("mft2\x00\x00\x00\x00")}})},
		{"tag count", mutateVisualPreviewICC(valid, func(p []byte) { binary.BigEndian.PutUint32(p[128:], ^uint32(0)) })},
		{"tag offset", mutateVisualPreviewICC(valid, func(p []byte) { binary.BigEndian.PutUint32(p[136:], ^uint32(0)) })},
		{"tag size", mutateVisualPreviewICC(valid, func(p []byte) { binary.BigEndian.PutUint32(p[140:], ^uint32(0)) })},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			pixels := image.NewNRGBA(image.Rect(0, 0, 1, 1))
			pixels.SetNRGBA(0, 0, visualPreviewUnconverted)
			require.NoError(t, applyVisualPreviewColor(t.Context(), pixels, visualPreviewColorSpace{profile: test.profile}.transform()))
			require.Equal(t, visualPreviewUnconverted, pixels.NRGBAAt(0, 0))
		})
	}
	require.Nil(t, visualPreviewPNGICC(syntheticVisualPreviewICCP(t, make([]byte, visualPreviewMaxICCBytes+1))))
	const chunkSize = 65533 - 14
	const segmentCount = visualPreviewMaxICCBytes/chunkSize + 1
	var oversizedSegments [][]byte
	for sequence := 1; sequence <= segmentCount; sequence++ {
		header := append([]byte("ICC_PROFILE\x00"), byte(sequence), byte(segmentCount))
		oversizedSegments = append(oversizedSegments, append(header, make([]byte, chunkSize)...))
	}
	for _, segments := range [][][]byte{
		{[]byte("ICC_PROFILE\x00")},
		{[]byte("ICC_PROFILE\x00\x01\x02a")},
		{[]byte("ICC_PROFILE\x00\x01\x01a"), []byte("ICC_PROFILE\x00\x01\x01b")},
		{[]byte("ICC_PROFILE\x00\x01\x02a"), []byte("ICC_PROFILE\x00\x02\x03b")},
		oversizedSegments,
	} {
		var icc visualPreviewJPEGICC
		for _, segment := range segments {
			icc.add(segment)
		}
		require.Nil(t, icc.profile())
	}
}

func mutateVisualPreviewICC(profile []byte, mutate func([]byte)) []byte {
	profile = bytes.Clone(profile)
	mutate(profile)
	return profile
}

func TestVisualPreviewICCFixed(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		bits uint32
		want float64
	}{
		{0, 0},
		{0x00010000, 1},
		{0x7fffffff, 32768 - 1.0/65536},
		{0x80000000, -32768},
		{0xffffffff, -1.0 / 65536},
	} {
		require.InDelta(t, test.want, visualPreviewICCFixed(binary.BigEndian.AppendUint32(nil, test.bits)), 0)
	}
}

func TestVisualPreviewICCCurves(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name string
		data []byte
		want float64
	}{
		{"identity", []byte("curv\x00\x00\x00\x00\x00\x00\x00\x00"), 64.0 / 255},
		{"gamma", []byte("curv\x00\x00\x00\x00\x00\x00\x00\x01\x02\x00"), (64.0 / 255) * (64.0 / 255)},
		{"sampled", []byte("curv\x00\x00\x00\x00\x00\x00\x00\x03\x00\x00\x40\x00\xff\xff"), 0.1255},
	} {
		t.Run(test.name, func(t *testing.T) {
			lut, ok := visualPreviewICCCurve(test.data)
			require.True(t, ok)
			require.InDelta(t, test.want, lut[64], 0.0001)
		})
	}
	for kind, parameters := range [][]float64{{2}, {2, 1, 0}, {2, 1, 0, 0.1}, {2, 1, 0, 0.1, 0.5}, {2, 1, 0, 0.1, 0.5, 0.1, 0.02}} {
		data := make([]byte, 12+len(parameters)*4)
		copy(data, "para")
		binary.BigEndian.PutUint16(data[8:], uint16(kind))
		for i, value := range parameters {
			binary.BigEndian.PutUint32(data[12+i*4:], uint32(value*65536))
		}
		lut, ok := visualPreviewICCCurve(data)
		require.True(t, ok)
		wantLow, wantHigh := (64.0/255)*(64.0/255), (192.0/255)*(192.0/255)
		if kind == 2 {
			wantLow += 0.1
			wantHigh += 0.1
		}
		if kind >= 3 {
			wantLow = 0.1 * 64 / 255
		}
		if kind == 4 {
			wantLow += 0.02
			wantHigh += 0.1
		}
		require.InDelta(t, wantLow, lut[64], 0.0001)
		require.InDelta(t, wantHigh, lut[192], 0.0001)
		_, ok = visualPreviewICCCurve(data[:len(data)-1])
		require.False(t, ok)
	}
	_, ok := visualPreviewICCCurve([]byte("curv\x00\x00\x00\x00\x00\x00\x00\x02\xff\xff\x00\x00"))
	require.False(t, ok)
}

func syntheticVisualPreviewICCP(t *testing.T, profile []byte) []byte {
	t.Helper()
	var compressed bytes.Buffer
	writer := zlib.NewWriter(&compressed)
	_, err := writer.Write(profile)
	require.NoError(t, err)
	require.NoError(t, writer.Close())
	return append([]byte("synthetic\x00\x00"), compressed.Bytes()...)
}

type syntheticICCTag struct {
	name string
	data []byte
}

func syntheticVisualPreviewICC(columns [3][3]float64, srgb bool, extra ...syntheticICCTag) []byte {
	var tags []syntheticICCTag
	for channel, name := range []string{"r", "g", "b"} {
		xyz := make([]byte, 20)
		copy(xyz, "XYZ ")
		for row, value := range columns[channel] {
			binary.BigEndian.PutUint32(xyz[8+row*4:], uint32(int32(value*65536+0.5)))
		}
		curve := []byte("curv\x00\x00\x00\x00\x00\x00\x00\x01\x02\x33")
		if srgb {
			curve = make([]byte, 32)
			copy(curve, "para")
			binary.BigEndian.PutUint16(curve[8:10], 3)
			for i, value := range []float64{2.4, 1 / 1.055, 0.055 / 1.055, 1 / 12.92, 0.04045} {
				binary.BigEndian.PutUint32(curve[12+i*4:], uint32(int32(value*65536+0.5)))
			}
		}
		tags = append(tags, syntheticICCTag{name + "XYZ", xyz}, syntheticICCTag{name + "TRC", curve})
	}
	return syntheticVisualPreviewICCTags(append(tags, extra...))
}

func syntheticVisualPreviewICCTags(tags []syntheticICCTag) []byte {
	header := 132 + len(tags)*12
	profile := append(make([]byte, 0, header), make([]byte, header)...)
	binary.BigEndian.PutUint32(profile[8:12], 0x04300000)
	copy(profile[12:16], "mntr")
	copy(profile[16:20], "RGB ")
	copy(profile[20:24], "XYZ ")
	copy(profile[36:40], "acsp")
	for i, value := range []uint32{0x0000f6d6, 0x00010000, 0x0000d32d} {
		binary.BigEndian.PutUint32(profile[68+i*4:], value)
	}
	binary.BigEndian.PutUint32(profile[128:132], uint32(len(tags)))
	for i, tag := range tags {
		entry := 132 + i*12
		copy(profile[entry:], tag.name)
		binary.BigEndian.PutUint32(profile[entry+4:], uint32(len(profile)))
		binary.BigEndian.PutUint32(profile[entry+8:], uint32(len(tag.data)))
		profile = append(profile, tag.data...)
		for len(profile)%4 != 0 {
			profile = append(profile, 0)
		}
	}
	binary.BigEndian.PutUint32(profile, uint32(len(profile)))
	return profile
}

// syntheticInteropEXIF builds root, Exif, and Interop IFDs with the given
// ColorSpace and InteropIndex.
func syntheticInteropEXIF(colorSpace uint16, interopIndex string) []byte {
	const exifOffset, interopOffset, size = 26, 56, 74
	tiff := make([]byte, size)
	copy(tiff, "II")
	binary.LittleEndian.PutUint16(tiff[2:4], 42)
	binary.LittleEndian.PutUint32(tiff[4:8], 8)
	external := size
	writeSyntheticTIFFIFD(tiff, 8, []syntheticTIFFEntry{tiffLong(0x8769, exifOffset)}, &external)
	writeSyntheticTIFFIFD(tiff, exifOffset, []syntheticTIFFEntry{
		tiffShort(0xa001, colorSpace), tiffLong(0xa005, interopOffset),
	}, &external)
	writeSyntheticTIFFIFD(tiff, interopOffset, []syntheticTIFFEntry{tiffASCII(0x0001, interopIndex)}, &external)
	return tiff
}
