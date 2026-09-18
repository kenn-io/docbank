package document

import (
	"math"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func pageSource() PageSource {
	return PageSource{VersionID: "00000000-0000-4000-8000-000000000001", SHA256: strings.Repeat("a", 64), Size: 123}
}

func TestPageFrameRotationsPreservePhysicalCropAndMapCorners(t *testing.T) {
	// Crop is one inch wide, two inches tall, offset by (10,20) points.
	for _, tc := range []struct {
		rotation      int
		width, height int64
		transform     [6]PageRational
	}{
		{0, 10000, 20000, [6]PageRational{{1, 72}, {0, 1}, {0, 1}, {-1, 72}, {-12500, 9}, {205000, 9}}},
		{90, 20000, 10000, [6]PageRational{{0, 1}, {1, 72}, {1, 72}, {0, 1}, {-25000, 9}, {-12500, 9}}},
		{180, 10000, 20000, [6]PageRational{{-1, 72}, {0, 1}, {0, 1}, {1, 72}, {102500, 9}, {-25000, 9}}},
		{270, 20000, 10000, [6]PageRational{{0, 1}, {-1, 72}, {-1, 72}, {0, 1}, {205000, 9}, {102500, 9}}},
	} {
		f, err := NewPDFPageFrame(pageSource(), 1, [4]float64{-10, -20, 612, 792}, [4]float64{10, 20, 82, 164}, tc.rotation)
		require.NoError(t, err)
		require.Equal(t, tc.width, f.Width)
		require.Equal(t, tc.height, f.Height)
		require.Equal(t, tc.transform, f.Transform)
		encoded, digest, err := MarshalPageFrameV1(f)
		require.NoError(t, err)
		got, gotDigest, err := DecodePageFrameV1(encoded)
		require.NoError(t, err)
		require.Equal(t, f, got)
		require.Equal(t, digest, gotDigest)
		got.Width++
		_, _, err = MarshalPageFrameV1(got)
		require.Error(t, err, "forged geometry must not canonicalize")
	}
}

func TestPageFrameQuantizationAndInvalidBounds(t *testing.T) {
	_, err := NewPDFPageFrame(pageSource(), 1, [4]float64{-0.00005, -1, 612, 792}, [4]float64{0, 0, 595.2756, 841.8898}, 0)
	require.Error(t, err, "crop outside media")
	f, err := NewPDFPageFrame(pageSource(), 1, [4]float64{-0.00005, -1, 600, 850}, [4]float64{0, 0, 595.2756, 841.8898}, 0)
	require.NoError(t, err)
	require.Equal(t, int64(-1), f.MediaBox[0])
	require.Equal(t, int64(82677), f.Width)
	require.Equal(t, int64(116929), f.Height)
	for _, box := range [][4]float64{{0, 0, 0, 10}, {0, 0, math.Inf(1), 10}, {0, 0, math.NaN(), 10}, {0, 0, 1e20, 10}} {
		_, err = NewPDFPageFrame(pageSource(), 1, box, box, 0)
		require.Error(t, err)
	}
	_, err = NewPDFPageFrame(pageSource(), 1, [4]float64{0, 0, 72, 72}, [4]float64{0, 0, 72, 72}, 45)
	require.Error(t, err)
}

func TestPNGPageFrameUsesExactDensity(t *testing.T) {
	f, err := NewPNGPageFrame(pageSource(), 254, 508, 10000, 10000)
	require.NoError(t, err)
	require.Equal(t, int64(10000), f.Width)
	require.Equal(t, int64(20000), f.Height)
	require.Equal(t, [6]PageRational{{5000, 127}, {0, 1}, {0, 1}, {5000, 127}, {0, 1}, {0, 1}}, f.Transform)
	require.Equal(t, "pixel", f.InputUnits)
	_, err = NewPNGPageFrame(pageSource(), 254, 508, 0, 10000)
	require.Error(t, err)
	_, err = NewPNGPageFrame(pageSource(), 254, 508, 10000, 9999)
	require.Error(t, err)
}
