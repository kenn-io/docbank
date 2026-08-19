package media

import (
	"encoding/binary"
	"math"
)

// visualCodecDimensions reads dimensions from an out-of-band codec
// configuration. avc3/hev1 and the other recognized visual sample-entry kinds
// can carry configuration changes in media samples, which this metadata-only
// detector cannot bound, so they fail closed.
func visualCodecDimensions(kind string, entry []byte) (int64, int64, bool) {
	if len(entry) < 78 {
		return 0, 0, false
	}
	var configKind string
	var parse func([]byte) (int64, int64, bool)
	switch kind {
	case "avc1":
		configKind, parse = "avcC", avcConfigDimensions
	case "hvc1":
		configKind, parse = "hvcC", hevcConfigDimensions
	default:
		return 0, 0, false
	}
	children := entry[78:]
	var width, height int64
	configs := 0
	for offset := 0; offset < len(children); {
		headerLen, size, ok := mp4BoxHeader(children, offset)
		if !ok {
			return 0, 0, false
		}
		if string(children[offset+4:offset+8]) == configKind {
			configs++
			if configs > 1 {
				return 0, 0, false
			}
			codecWidth, codecHeight, ok := parse(children[offset+headerLen : offset+size])
			if !ok {
				return 0, 0, false
			}
			width, height = codecWidth, codecHeight
		}
		offset += size
	}
	return width, height, configs == 1 && width > 0 && height > 0
}

func avcConfigDimensions(config []byte) (int64, int64, bool) {
	if len(config) < 7 || config[0] != 1 {
		return 0, 0, false
	}
	offset := 6
	spsCount := int(config[5] & 0x1f)
	if spsCount == 0 {
		return 0, 0, false
	}
	var width, height int64
	for range spsCount {
		if offset+2 > len(config) {
			return 0, 0, false
		}
		size := int(binary.BigEndian.Uint16(config[offset : offset+2]))
		offset += 2
		if size == 0 || offset+size > len(config) {
			return 0, 0, false
		}
		spsWidth, spsHeight, ok := avcSPSDimensions(config[offset : offset+size])
		if !ok {
			return 0, 0, false
		}
		width, height = max(width, spsWidth), max(height, spsHeight)
		offset += size
	}
	if offset >= len(config) {
		return 0, 0, false
	}
	ppsCount := int(config[offset])
	offset++
	if ppsCount == 0 {
		return 0, 0, false
	}
	for range ppsCount {
		if offset+2 > len(config) {
			return 0, 0, false
		}
		size := int(binary.BigEndian.Uint16(config[offset : offset+2]))
		offset += 2
		if size == 0 || offset+size > len(config) {
			return 0, 0, false
		}
		offset += size
	}
	return width, height, width > 0 && height > 0
}

func avcSPSDimensions(nal []byte) (int64, int64, bool) {
	if len(nal) < 4 || nal[0]&0x1f != 7 {
		return 0, 0, false
	}
	reader := mp4BitReader{data: removeEmulationPrevention(nal[1:])}
	profile, ok := reader.readBits(8)
	if !ok || !reader.skipBits(16) {
		return 0, 0, false
	}
	if _, ok = reader.readUE(); !ok { // seq_parameter_set_id
		return 0, 0, false
	}
	chromaFormat := uint64(1)
	separateColourPlane := uint64(0)
	if avcHighProfile(profile) {
		if chromaFormat, ok = reader.readUE(); !ok || chromaFormat > 3 {
			return 0, 0, false
		}
		if chromaFormat == 3 {
			if separateColourPlane, ok = reader.readBits(1); !ok {
				return 0, 0, false
			}
		}
		if _, ok = reader.readUE(); !ok { // bit_depth_luma_minus8
			return 0, 0, false
		}
		if _, ok = reader.readUE(); !ok { // bit_depth_chroma_minus8
			return 0, 0, false
		}
		if !reader.skipBits(1) { // qpprime_y_zero_transform_bypass_flag
			return 0, 0, false
		}
		scalingPresent, ok := reader.readBits(1)
		if !ok {
			return 0, 0, false
		}
		if scalingPresent != 0 {
			count := 8
			if chromaFormat == 3 {
				count = 12
			}
			for index := range count {
				present, ok := reader.readBits(1)
				if !ok {
					return 0, 0, false
				}
				if present != 0 && !reader.skipAVCScalingList(16+48*min(index/6, 1)) {
					return 0, 0, false
				}
			}
		}
	}
	if _, ok = reader.readUE(); !ok { // log2_max_frame_num_minus4
		return 0, 0, false
	}
	picOrderCountType, ok := reader.readUE()
	if !ok {
		return 0, 0, false
	}
	switch picOrderCountType {
	case 0:
		if _, ok = reader.readUE(); !ok {
			return 0, 0, false
		}
	case 1:
		if !reader.skipBits(1) {
			return 0, 0, false
		}
		if _, ok = reader.readSE(); !ok {
			return 0, 0, false
		}
		if _, ok = reader.readSE(); !ok {
			return 0, 0, false
		}
		cycle, ok := reader.readUE()
		if !ok || cycle > 256 {
			return 0, 0, false
		}
		for range cycle {
			if _, ok = reader.readSE(); !ok {
				return 0, 0, false
			}
		}
	case 2:
	default:
		return 0, 0, false
	}
	if _, ok = reader.readUE(); !ok || !reader.skipBits(1) { // refs, gaps flag
		return 0, 0, false
	}
	widthMbs, ok := reader.readUE()
	if !ok {
		return 0, 0, false
	}
	heightMapUnits, ok := reader.readUE()
	if !ok {
		return 0, 0, false
	}
	frameMbsOnly, ok := reader.readBits(1)
	if !ok {
		return 0, 0, false
	}
	if frameMbsOnly == 0 && !reader.skipBits(1) {
		return 0, 0, false
	}
	if !reader.skipBits(1) { // direct_8x8_inference_flag
		return 0, 0, false
	}
	cropping, ok := reader.readBits(1)
	if !ok {
		return 0, 0, false
	}
	var left, right, top, bottom uint64
	if cropping != 0 {
		if left, ok = reader.readUE(); !ok {
			return 0, 0, false
		}
		if right, ok = reader.readUE(); !ok {
			return 0, 0, false
		}
		if top, ok = reader.readUE(); !ok {
			return 0, 0, false
		}
		if bottom, ok = reader.readUE(); !ok {
			return 0, 0, false
		}
	}
	frameFactor := uint64(2) - frameMbsOnly
	width := (widthMbs + 1) * 16
	height := frameFactor * (heightMapUnits + 1) * 16
	chromaArrayType := chromaFormat
	if separateColourPlane != 0 {
		chromaArrayType = 0
	}
	cropUnitX, cropUnitY := uint64(1), frameFactor
	switch chromaArrayType {
	case 1:
		cropUnitX, cropUnitY = 2, 2*frameFactor
	case 2:
		cropUnitX, cropUnitY = 2, frameFactor
	case 3:
		cropUnitX, cropUnitY = 1, frameFactor
	}
	cropWidth, cropHeight := cropUnitX*(left+right), cropUnitY*(top+bottom)
	if cropWidth >= width || cropHeight >= height {
		return 0, 0, false
	}
	// Cropping affects the visible window, not the frame a decoder allocates.
	// Keep the uncropped coded dimensions as the conservative policy bound.
	if width > math.MaxInt64 || height > math.MaxInt64 {
		return 0, 0, false
	}
	return int64(width), int64(height), true
}

func avcHighProfile(profile uint64) bool {
	switch profile {
	case 44, 83, 86, 100, 110, 118, 122, 128, 134, 135, 138, 139, 244:
		return true
	default:
		return false
	}
}

func hevcConfigDimensions(config []byte) (int64, int64, bool) {
	if len(config) < 23 || config[0] != 1 {
		return 0, 0, false
	}
	offset := 23
	arrayCount := int(config[22])
	var width, height int64
	spsCount := 0
	for range arrayCount {
		if offset+3 > len(config) {
			return 0, 0, false
		}
		nalType := config[offset] & 0x3f
		nalCount := int(binary.BigEndian.Uint16(config[offset+1 : offset+3]))
		offset += 3
		if nalCount == 0 {
			return 0, 0, false
		}
		for range nalCount {
			if offset+2 > len(config) {
				return 0, 0, false
			}
			size := int(binary.BigEndian.Uint16(config[offset : offset+2]))
			offset += 2
			if size == 0 || offset+size > len(config) {
				return 0, 0, false
			}
			if nalType == 33 {
				spsWidth, spsHeight, ok := hevcSPSDimensions(config[offset : offset+size])
				if !ok {
					return 0, 0, false
				}
				width, height = max(width, spsWidth), max(height, spsHeight)
				spsCount++
			}
			offset += size
		}
	}
	return width, height, offset == len(config) && spsCount > 0
}

func hevcSPSDimensions(nal []byte) (int64, int64, bool) {
	if len(nal) < 4 || (nal[0]>>1)&0x3f != 33 {
		return 0, 0, false
	}
	reader := mp4BitReader{data: removeEmulationPrevention(nal[2:])}
	if !reader.skipBits(4) { // sps_video_parameter_set_id
		return 0, 0, false
	}
	maxSubLayers, ok := reader.readBits(3)
	if !ok || !reader.skipBits(1) || !reader.skipHEVCProfileTierLevel(int(maxSubLayers)) { //nolint:gosec // three-bit field is at most seven
		return 0, 0, false
	}
	if _, ok = reader.readUE(); !ok { // sps_seq_parameter_set_id
		return 0, 0, false
	}
	chromaFormat, ok := reader.readUE()
	if !ok || chromaFormat > 3 {
		return 0, 0, false
	}
	separateColourPlane := uint64(0)
	if chromaFormat == 3 {
		if separateColourPlane, ok = reader.readBits(1); !ok {
			return 0, 0, false
		}
	}
	width, ok := reader.readUE()
	if !ok {
		return 0, 0, false
	}
	height, ok := reader.readUE()
	if !ok {
		return 0, 0, false
	}
	window, ok := reader.readBits(1)
	if !ok {
		return 0, 0, false
	}
	var left, right, top, bottom uint64
	if window != 0 {
		if left, ok = reader.readUE(); !ok {
			return 0, 0, false
		}
		if right, ok = reader.readUE(); !ok {
			return 0, 0, false
		}
		if top, ok = reader.readUE(); !ok {
			return 0, 0, false
		}
		if bottom, ok = reader.readUE(); !ok {
			return 0, 0, false
		}
	}
	chromaArrayType := chromaFormat
	if separateColourPlane != 0 {
		chromaArrayType = 0
	}
	subWidth, subHeight := uint64(1), uint64(1)
	switch chromaArrayType {
	case 1:
		subWidth, subHeight = 2, 2
	case 2:
		subWidth = 2
	}
	cropWidth, cropHeight := subWidth*(left+right), subHeight*(top+bottom)
	if width == 0 || height == 0 || cropWidth >= width || cropHeight >= height {
		return 0, 0, false
	}
	// The conformance window is display metadata. Decoder work is bounded by
	// the full coded frame, so return its uncropped dimensions.
	if width > math.MaxInt64 || height > math.MaxInt64 {
		return 0, 0, false
	}
	return int64(width), int64(height), true
}

func removeEmulationPrevention(data []byte) []byte {
	out := make([]byte, 0, len(data))
	zeros := 0
	for _, value := range data {
		if zeros >= 2 && value == 3 {
			zeros = 2
			continue
		}
		out = append(out, value)
		if value == 0 {
			zeros++
		} else {
			zeros = 0
		}
	}
	return out
}

type mp4BitReader struct {
	data []byte
	bit  int
}

func (r *mp4BitReader) readBits(count int) (uint64, bool) {
	if count < 0 || count > 64 || r.bit+count > len(r.data)*8 {
		return 0, false
	}
	var value uint64
	for range count {
		value = value<<1 | uint64((r.data[r.bit/8]>>(7-r.bit%8))&1)
		r.bit++
	}
	return value, true
}

func (r *mp4BitReader) skipBits(count int) bool {
	if count < 0 || r.bit+count > len(r.data)*8 {
		return false
	}
	r.bit += count
	return true
}

func (r *mp4BitReader) readUE() (uint64, bool) {
	zeros := 0
	for {
		bit, ok := r.readBits(1)
		if !ok || zeros > 31 {
			return 0, false
		}
		if bit != 0 {
			break
		}
		zeros++
	}
	suffix, ok := r.readBits(zeros)
	if !ok {
		return 0, false
	}
	return (uint64(1) << zeros) - 1 + suffix, true
}

func (r *mp4BitReader) readSE() (int64, bool) {
	value, ok := r.readUE()
	if !ok {
		return 0, false
	}
	if value&1 != 0 {
		return int64((value + 1) / 2), true //nolint:gosec // readUE caps the code to 32 prefix bits
	}
	return -int64(value / 2), true //nolint:gosec // readUE caps the code to 32 prefix bits
}

func (r *mp4BitReader) skipAVCScalingList(size int) bool {
	lastScale, nextScale := int64(8), int64(8)
	for range size {
		if nextScale != 0 {
			delta, ok := r.readSE()
			if !ok {
				return false
			}
			nextScale = (lastScale + delta + 256) % 256
		}
		if nextScale != 0 {
			lastScale = nextScale
		}
	}
	return true
}

func (r *mp4BitReader) skipHEVCProfileTierLevel(maxSubLayers int) bool {
	if maxSubLayers < 0 || maxSubLayers > 7 || !r.skipBits(96) {
		return false
	}
	profilePresent := make([]bool, maxSubLayers)
	levelPresent := make([]bool, maxSubLayers)
	for index := range maxSubLayers {
		profile, ok := r.readBits(1)
		if !ok {
			return false
		}
		level, ok := r.readBits(1)
		if !ok {
			return false
		}
		profilePresent[index], levelPresent[index] = profile != 0, level != 0
	}
	if maxSubLayers > 0 && !r.skipBits((8-maxSubLayers)*2) {
		return false
	}
	for index := range maxSubLayers {
		if profilePresent[index] && !r.skipBits(88) {
			return false
		}
		if levelPresent[index] && !r.skipBits(8) {
			return false
		}
	}
	return true
}
