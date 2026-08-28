package media

import (
	"encoding/binary"
	"math"
)

type mp4CodecConfiguration struct {
	codec          string
	nalLengthBytes int
}

// visualCodecDimensions reads dimensions from an out-of-band codec
// configuration. avc3/hev1 and the other recognized visual sample-entry kinds
// can carry configuration changes in media samples, which this metadata-only
// detector cannot bound, so they fail closed.
func visualCodecDimensions(kind string, entry []byte) (mp4CodecConfiguration, int64, int64, bool) {
	if len(entry) < 78 {
		return mp4CodecConfiguration{}, 0, 0, false
	}
	var configuration mp4CodecConfiguration
	var configKind string
	var parse func([]byte) (int64, int64, bool)
	switch kind {
	case "avc1":
		configuration.codec, configKind, parse = "h264", "avcC", avcConfigDimensions
	case "hvc1":
		configuration.codec, configKind, parse = "h265", "hvcC", hevcConfigDimensions
	case "vp09":
		configuration.codec, configKind, parse = "vp9", "vpcC", visualEntryDimensions
	case "av01":
		configuration.codec, configKind, parse = "av1", "av1C", visualEntryDimensions
	default:
		return mp4CodecConfiguration{}, 0, 0, false
	}
	children := entry[78:]
	var width, height int64
	configs := 0
	for offset := 0; offset < len(children); {
		headerLen, size, ok := mp4BoxHeader(children, offset)
		if !ok {
			return mp4CodecConfiguration{}, 0, 0, false
		}
		if string(children[offset+4:offset+8]) == configKind {
			configs++
			if configs > 1 {
				return mp4CodecConfiguration{}, 0, 0, false
			}
			config := children[offset+headerLen : offset+size]
			if (kind == "vp09" && !validVP9Config(config)) || (kind == "av01" && !validAV1Config(config)) {
				return mp4CodecConfiguration{}, 0, 0, false
			}
			codecWidth, codecHeight, ok := parse(config)
			if !ok {
				return mp4CodecConfiguration{}, 0, 0, false
			}
			switch kind {
			case "avc1":
				configuration.nalLengthBytes = int(config[4]&0x03) + 1
			case "hvc1":
				configuration.nalLengthBytes = int(config[21]&0x03) + 1
			}
			if kind == "vp09" || kind == "av01" {
				codecWidth = int64(binary.BigEndian.Uint16(entry[24:26]))
				codecHeight = int64(binary.BigEndian.Uint16(entry[26:28]))
			}
			width, height = codecWidth, codecHeight
		}
		offset += size
	}
	return configuration, width, height, configs == 1 && width > 0 && height > 0
}

func visualEntryDimensions(config []byte) (int64, int64, bool) {
	return 1, 1, len(config) > 0
}

func validVP9Config(config []byte) bool {
	if len(config) < 12 || config[0] != 1 || config[1] != 0 || config[2] != 0 || config[3] != 0 ||
		config[4] > 3 {
		return false
	}
	bitDepth := config[6] >> 4
	if bitDepth != 8 && bitDepth != 10 && bitDepth != 12 || config[6]>>1&0x07 > 3 {
		return false
	}
	initializationDataSize := int(binary.BigEndian.Uint16(config[10:12]))
	return len(config) == 12+initializationDataSize
}

func validAV1Config(config []byte) bool {
	if len(config) < 4 || config[0] != 0x81 || config[3]&0xe0 != 0 ||
		config[3]&0x10 == 0 && config[3]&0x0f != 0 {
		return false
	}
	return len(config) == 4 || validAV1OBUs(config[4:], false, true)
}

func validMP4CodecSample(configuration mp4CodecConfiguration, sample []byte) bool {
	switch configuration.codec {
	case "h264":
		return validLengthPrefixedNALSample(sample, configuration.nalLengthBytes, false)
	case "h265":
		return validLengthPrefixedNALSample(sample, configuration.nalLengthBytes, true)
	case "vp9":
		return len(sample) > 0 && sample[0]&0x03 == 0x02
	case "av1":
		return validAV1OBUs(sample, true, false)
	default:
		return false
	}
}

func validLengthPrefixedNALSample(sample []byte, lengthBytes int, hevc bool) bool {
	if lengthBytes < 1 || lengthBytes > 4 {
		return false
	}
	hasPicture := false
	for offset := 0; offset < len(sample); {
		if offset+lengthBytes > len(sample) {
			return false
		}
		size := uint64(0)
		for _, value := range sample[offset : offset+lengthBytes] {
			size = size<<8 | uint64(value)
		}
		offset += lengthBytes
		remaining := uint64(len(sample) - offset) //nolint:gosec // offset is bounded by the sample length
		if size == 0 || size > remaining {
			return false
		}
		nal := sample[offset : offset+int(size)]
		if hevc {
			if len(nal) < 2 || nal[0]&0x80 != 0 || nal[1]&0x07 == 0 {
				return false
			}
			hasPicture = hasPicture || (nal[0]>>1)&0x3f <= 31
		} else {
			if nal[0]&0x80 != 0 || nal[0]&0x1f == 0 || nal[0]&0x1f > 23 {
				return false
			}
			nalType := nal[0] & 0x1f
			hasPicture = hasPicture || nalType >= 1 && nalType <= 5
		}
		offset += int(size)
	}
	return len(sample) > 0 && hasPicture
}

func validAV1OBUs(data []byte, requireFrame, requireSequence bool) bool {
	hasFrame, hasSequence := false, false
	for offset := 0; offset < len(data); {
		header := data[offset]
		offset++
		obuType := header >> 3 & 0x0f
		if header&0x81 != 0 || header&0x02 == 0 || obuType == 0 || obuType >= 9 && obuType <= 14 {
			return false
		}
		if header&0x04 != 0 {
			if offset >= len(data) || data[offset]&0x07 != 0 {
				return false
			}
			offset++
		}
		size, sizeBytes, ok := readAV1LEB128(data[offset:])
		remaining := uint64(len(data) - offset - sizeBytes) //nolint:gosec // offsets are bounded by data above
		if !ok || size > remaining {
			return false
		}
		offset += sizeBytes
		if size == 0 && obuType != 2 && obuType != 15 {
			return false
		}
		hasSequence = hasSequence || obuType == 1
		hasFrame = hasFrame || obuType == 3 || obuType == 6 || obuType == 7
		offset += int(size) //nolint:gosec // size is bounded by the remaining data above
	}
	return len(data) > 0 && (!requireFrame || hasFrame) && (!requireSequence || hasSequence)
}

func readAV1LEB128(data []byte) (uint64, int, bool) {
	var value uint64
	for index := 0; index < len(data) && index < 8; index++ {
		value |= uint64(data[index]&0x7f) << (index * 7)
		if data[index]&0x80 == 0 {
			return value, index + 1, true
		}
	}
	return 0, 0, false
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
