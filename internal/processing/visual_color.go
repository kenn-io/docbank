package processing

import (
	"bytes"
	"compress/zlib"
	"context"
	"encoding/binary"
	"image"
	"io"
	"math"
)

type visualPreviewJPEGICC struct {
	parts       [256][]byte
	count, size int
	invalid     bool
}

func (icc *visualPreviewJPEGICC) add(payload []byte) {
	if icc.invalid {
		return
	}
	if len(payload) < 14 {
		icc.invalid = true
		return
	}
	sequence, count := int(payload[12]), int(payload[13])
	if sequence == 0 || count == 0 || sequence > count ||
		icc.count != 0 && icc.count != count || icc.parts[sequence] != nil ||
		len(payload)-14 > visualPreviewMaxICCBytes-icc.size {
		icc.invalid = true
		return
	}
	icc.count = count
	icc.size += len(payload) - 14
	icc.parts[sequence] = payload[14:]
}

func (icc *visualPreviewJPEGICC) profile() []byte {
	if icc.invalid || icc.count == 0 {
		return nil
	}
	profile := make([]byte, 0, icc.size)
	for i := 1; i <= icc.count; i++ {
		if icc.parts[i] == nil {
			return nil
		}
		profile = append(profile, icc.parts[i]...)
	}
	return profile
}

// readVisualPreviewICCChunk reads a PNG or WebP profile chunk. A chunk over the
// profile limit is skipped, so the image renders without conversion.
func readVisualPreviewICCChunk(reader io.Reader, length int64) ([]byte, error) {
	if length > visualPreviewMaxICCBytes {
		return nil, nil
	}
	payload := make([]byte, length)
	if _, err := io.ReadFull(reader, payload); err != nil {
		return nil, err
	}
	return payload, nil
}

func visualPreviewPNGICC(payload []byte) []byte {
	end := bytes.IndexByte(payload, 0)
	if end < 1 || end > 79 || end+2 > len(payload) || payload[end+1] != 0 {
		return nil
	}
	reader, err := zlib.NewReader(bytes.NewReader(payload[end+2:]))
	if err != nil {
		return nil
	}
	profile, readErr := io.ReadAll(io.LimitReader(reader, visualPreviewMaxICCBytes+1))
	closeErr := reader.Close()
	if readErr != nil || closeErr != nil || len(profile) > visualPreviewMaxICCBytes {
		return nil
	}
	return profile
}

// visualPreviewColorSpace records how a source describes its RGB values. An
// embedded profile wins over the EXIF Adobe RGB marker.
type visualPreviewColorSpace struct {
	profile []byte
	// adobeRGB is set by EXIF ColorSpace Uncalibrated with DCF InteropIndex R03,
	// which cameras write for Adobe RGB photos without an ICC profile.
	adobeRGB bool
}

func (space visualPreviewColorSpace) transform() *visualPreviewColorTransform {
	if transform, ok := parseVisualPreviewICC(space.profile); ok {
		return transform
	}
	if space.adobeRGB {
		return visualPreviewAdobeRGB
	}
	return nil
}

type visualPreviewColorTransform struct {
	input  [3][256]float64
	matrix [3][3]float64
}

// ICC colorants already use the D50 connection space, including chromatic adaptation.
var visualPreviewXYZToSRGB = [3][3]float64{
	{3.1338561, -1.6168667, -0.4906146},
	{-0.9787684, 1.9161415, 0.0334540},
	{0.0719453, -0.2289914, 1.4052427},
}

// Vendor sRGB profiles round the D50 white differently and land up to about
// 0.0003 from visualPreviewXYZToSRGB. A 0.001 linear difference is invisible.
const (
	visualPreviewSRGBMatrixTolerance = 0.001
	visualPreviewSRGBCurveTolerance  = 0.0003
)

var visualPreviewSRGBEncode = func() [4097]byte {
	var lut [4097]byte
	for i := range lut {
		x := float64(i) / 4096
		y := 12.92 * x
		if x > 0.0031308 {
			y = 1.055*math.Pow(x, 1/2.4) - 0.055
		}
		lut[i] = byte(math.Round(y * 255))
	}
	return lut
}()

// visualPreviewAdobeRGB uses the Adobe RGB (1998) D50 colorants and its
// 563/256 gamma.
var visualPreviewAdobeRGB = func() *visualPreviewColorTransform {
	var curve [256]float64
	for i := range curve {
		curve[i] = math.Pow(float64(i)/255, 563.0/256)
	}
	transform, _ := newVisualPreviewColorTransform([3][3]float64{
		{0.6097559, 0.3111242, 0.0194811},
		{0.2052401, 0.6256560, 0.0608902},
		{0.1492240, 0.0632197, 0.7448387},
	}, [3][256]float64{curve, curve, curve})
	return transform
}()

// parseVisualPreviewICC reads an RGB matrix/TRC profile. ok is false when the
// profile is unusable; a nil transform with ok means the profile is sRGB.
func parseVisualPreviewICC(profile []byte) (*visualPreviewColorTransform, bool) {
	tags, ok := visualPreviewICCTags(profile)
	if !ok {
		return nil, false
	}
	var columns [3][3]float64
	var curves [3][256]float64
	for channel, name := range []string{"r", "g", "b"} {
		xyz := tags[name+"XYZ"]
		if len(xyz) < 20 || string(xyz[:4]) != "XYZ " {
			return nil, false
		}
		for k := range 3 {
			columns[channel][k] = visualPreviewICCFixed(xyz[8+k*4:])
		}
		if curves[channel], ok = visualPreviewICCCurve(tags[name+"TRC"]); !ok {
			return nil, false
		}
	}
	return newVisualPreviewColorTransform(columns, curves)
}

// visualPreviewICCTags validates an RGB display profile header and returns
// its tag table. Version 2 and 4 profiles are accepted.
func visualPreviewICCTags(profile []byte) (map[string][]byte, bool) {
	if len(profile) < 132 ||
		profile[8] != 2 && profile[8] != 4 || string(profile[36:40]) != "acsp" ||
		string(profile[16:20]) != "RGB " || string(profile[20:24]) != "XYZ " {
		return nil, false
	}
	size := uint64(binary.BigEndian.Uint32(profile))
	if size < 132 || size > uint64(len(profile)) {
		return nil, false
	}
	profile = profile[:size]
	count := int(binary.BigEndian.Uint32(profile[128:132]))
	if count > (len(profile)-132)/12 {
		return nil, false
	}
	tableEnd := uint64(132 + count*12)
	tags := make(map[string][]byte, count)
	for i := range count {
		entry := profile[132+i*12 : 144+i*12]
		name := string(entry[:4])
		offset := uint64(binary.BigEndian.Uint32(entry[4:8]))
		length := uint64(binary.BigEndian.Uint32(entry[8:12]))
		if offset < tableEnd || offset > size || length > size-offset || tags[name] != nil {
			return nil, false
		}
		tags[name] = profile[offset : offset+length]
	}
	return tags, true
}

// newVisualPreviewColorTransform combines per-channel colorants (D50 XYZ
// columns) and linearizing curves. It returns nil with ok for sRGB.
func newVisualPreviewColorTransform(
	columns [3][3]float64, curves [3][256]float64,
) (*visualPreviewColorTransform, bool) {
	transform := &visualPreviewColorTransform{input: curves}
	for row := range 3 {
		for column := range 3 {
			for k := range 3 {
				transform.matrix[row][column] += visualPreviewXYZToSRGB[row][k] * columns[column][k]
			}
		}
	}
	m := transform.matrix
	determinant := m[0][0]*(m[1][1]*m[2][2]-m[1][2]*m[2][1]) -
		m[0][1]*(m[1][0]*m[2][2]-m[1][2]*m[2][0]) +
		m[0][2]*(m[1][0]*m[2][1]-m[1][1]*m[2][0])
	if math.Abs(determinant) < 1e-8 {
		return nil, false
	}
	if visualPreviewTransformIsSRGB(transform) {
		return nil, true
	}
	return transform, true
}

func visualPreviewTransformIsSRGB(transform *visualPreviewColorTransform) bool {
	for row := range 3 {
		for column := range 3 {
			expected := 0.0
			if row == column {
				expected = 1
			}
			if math.Abs(transform.matrix[row][column]-expected) >= visualPreviewSRGBMatrixTolerance {
				return false
			}
		}
	}
	for _, curve := range &transform.input {
		for i, value := range &curve {
			x := float64(i) / 255
			expected := x / 12.92
			if x > 0.04045 {
				expected = math.Pow((x+0.055)/1.055, 2.4)
			}
			if math.Abs(value-expected) >= visualPreviewSRGBCurveTolerance {
				return false
			}
		}
	}
	return true
}

func visualPreviewICCFixed(data []byte) float64 {
	value := int64(binary.BigEndian.Uint32(data))
	if value >= 1<<31 {
		value -= 1 << 32
	}
	return float64(value) / 65536
}

func visualPreviewICCCurve(data []byte) (lut [256]float64, ok bool) {
	if len(data) < 12 {
		return lut, false
	}
	var evaluate func(float64) float64
	switch string(data[:4]) {
	case "curv":
		count := int(binary.BigEndian.Uint32(data[8:12]))
		if count > (len(data)-12)/2 || count == 1 && binary.BigEndian.Uint16(data[12:14]) == 0 {
			return lut, false
		}
		evaluate = func(x float64) float64 {
			switch count {
			case 0:
				return x
			case 1:
				return math.Pow(x, float64(binary.BigEndian.Uint16(data[12:14]))/256)
			default:
				position := x * float64(count-1)
				index := min(int(position), count-2)
				a := float64(binary.BigEndian.Uint16(data[12+index*2:])) / 65535
				b := float64(binary.BigEndian.Uint16(data[14+index*2:])) / 65535
				return a + (b-a)*(position-float64(index))
			}
		}
	case "para":
		kind := int(binary.BigEndian.Uint16(data[8:10]))
		counts := [5]int{1, 3, 4, 5, 7}
		if kind >= len(counts) || len(data) < 12+counts[kind]*4 {
			return lut, false
		}
		var p [7]float64
		for i := range counts[kind] {
			p[i] = visualPreviewICCFixed(data[12+i*4:])
		}
		if p[0] <= 0 || kind > 0 && p[1] <= 0 {
			return lut, false
		}
		evaluate = func(x float64) float64 {
			switch kind {
			case 0:
				return math.Pow(x, p[0])
			case 1, 2:
				y := 0.0
				if x >= -p[2]/p[1] {
					y = math.Pow(p[1]*x+p[2], p[0])
				}
				if kind == 2 {
					y += p[3]
				}
				return y
			default:
				if x >= p[4] {
					return math.Pow(p[1]*x+p[2], p[0]) + p[5]
				}
				return p[3]*x + p[6]
			}
		}
	default:
		return lut, false
	}
	previous := 0.0
	for i := range lut {
		value := evaluate(float64(i) / 255)
		if math.IsNaN(value) || math.IsInf(value, 0) {
			return lut, false
		}
		lut[i] = max(0, min(1, value))
		if i > 0 && lut[i] < previous {
			return lut, false
		}
		previous = lut[i]
	}
	return lut, true
}

func applyVisualPreviewColor(
	ctx context.Context, pixels *image.NRGBA, transform *visualPreviewColorTransform,
) error {
	if transform == nil {
		return nil
	}
	width := pixels.Bounds().Dx()
	for y := range pixels.Bounds().Dy() {
		if err := ctx.Err(); err != nil {
			return err
		}
		row := pixels.Pix[y*pixels.Stride:][:width*4]
		for i := 0; i < len(row); i += 4 {
			linear := [3]float64{
				transform.input[0][row[i]], transform.input[1][row[i+1]], transform.input[2][row[i+2]],
			}
			for channel, coefficients := range transform.matrix {
				value := coefficients[0]*linear[0] + coefficients[1]*linear[1] + coefficients[2]*linear[2]
				row[i+channel] = visualPreviewSRGBEncode[int(math.Round(max(0, min(1, value))*4096))]
			}
		}
	}
	return nil
}
