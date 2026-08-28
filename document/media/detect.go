package media

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"image"
	"io"
	"math"
	"math/bits"
	"sort"

	_ "image/jpeg" // Register the JPEG decoder used by image.DecodeConfig.
	_ "image/png"  // Register the PNG decoder used by image.DecodeConfig.
)

// MaxBytes is the largest input the package will read. Policy.MaxBytes cannot
// exceed it.
const MaxBytes = int64(20 << 20)

// ErrTooLarge reports an input longer than MaxBytes.
var ErrTooLarge = errors.New("media: input exceeds maximum size")

// Detect sniffs the container format and reads bounded metadata from reader.
// It reads at most MaxBytes and never decodes pixels. The declared media type
// is recorded verbatim in the result and is not used for detection.
func Detect(reader io.ReaderAt, size int64, declaredMediaType string) (Metadata, error) {
	if reader == nil {
		return Metadata{}, errors.New("media: reader is required")
	}
	if size < 0 || size > MaxBytes {
		return Metadata{}, ErrTooLarge
	}
	data, err := io.ReadAll(io.NewSectionReader(reader, 0, size))
	if err != nil {
		return Metadata{}, fmt.Errorf("media: read input: %w", err)
	}
	return DetectBytes(data, declaredMediaType)
}

// DetectBytes is Detect for in-memory input.
func DetectBytes(data []byte, declaredMediaType string) (Metadata, error) {
	if int64(len(data)) > MaxBytes {
		return Metadata{}, ErrTooLarge
	}
	metadata, err := sniff(data)
	if err != nil {
		return Metadata{}, err
	}
	metadata.DeclaredMediaType = declaredMediaType
	metadata.Size = int64(len(data))
	if metadata.Width <= 0 || metadata.Height <= 0 {
		return Metadata{}, ErrMalformedMedia
	}
	return metadata, nil
}

func sniff(data []byte) (Metadata, error) {
	switch {
	case bytes.HasPrefix(data, []byte("\xff\xd8\xff")):
		metadata, err := sniffImage(data, FormatJPEG, "image/jpeg")
		if err != nil {
			return Metadata{}, err
		}
		metadata.FrameCount = 1
		return metadata, nil
	case bytes.HasPrefix(data, []byte("\x89PNG\r\n\x1a\n")):
		metadata, err := sniffImage(data, FormatPNG, "image/png")
		if err != nil {
			return Metadata{}, err
		}
		frames, ok := apngFrames(data, metadata.Width, metadata.Height)
		if !ok {
			return Metadata{}, ErrMalformedMedia
		}
		metadata.FrameCount, metadata.Animated = frames, frames > 1
		return metadata, nil
	case len(data) >= 12 && string(data[:4]) == "RIFF" && string(data[8:12]) == "WEBP":
		width, height, frames, ok := webPMetadata(data)
		if !ok {
			return Metadata{}, ErrMalformedMedia
		}
		return Metadata{
			Format: FormatWebP, Kind: KindImage, MediaType: "image/webp",
			Width: width, Height: height, FrameCount: frames, Animated: frames > 1,
		}, nil
	case bytes.HasPrefix(data, []byte("GIF87a")) || bytes.HasPrefix(data, []byte("GIF89a")):
		width, height, frames, ok := gifMetadata(data)
		if !ok {
			return Metadata{}, ErrMalformedMedia
		}
		return Metadata{
			Format: FormatGIF, Kind: KindImage, MediaType: "image/gif",
			Width: width, Height: height, FrameCount: frames, Animated: frames > 1,
		}, nil
	case isMP4(data):
		info, ok := mp4Metadata(data)
		if !ok {
			return Metadata{}, ErrMalformedMedia
		}
		mediaType := "video/mp4"
		if info.container == "quicktime" {
			mediaType = "video/quicktime"
		}
		return Metadata{
			Format: FormatMP4, Kind: KindVideo, MediaType: mediaType,
			Container: info.container, Codec: info.codec,
			Width: info.width, Height: info.height,
			DurationMS: info.durationMS, DurationKnown: info.durationKnown,
		}, nil
	default:
		return Metadata{}, ErrUnsupportedMedia
	}
}

func sniffImage(data []byte, format Format, mediaType string) (Metadata, error) {
	config, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return Metadata{}, ErrMalformedMedia
	}
	return Metadata{
		Format: format, Kind: KindImage, MediaType: mediaType,
		Width: int64(config.Width), Height: int64(config.Height),
	}, nil
}

// gifMetadata walks container blocks without allocating or decoding pixels.
// It returns logical-screen dimensions and the number of image descriptors.
func gifMetadata(data []byte) (int64, int64, int, bool) {
	if len(data) < 13 {
		return 0, 0, 0, false
	}
	width := int64(binary.LittleEndian.Uint16(data[6:8]))
	height := int64(binary.LittleEndian.Uint16(data[8:10]))
	if width <= 0 || height <= 0 {
		return 0, 0, 0, false
	}
	index := 13
	if data[10]&0x80 != 0 {
		index += 3 * (1 << ((data[10] & 0x07) + 1))
	}
	frames := 0
	for index < len(data) {
		switch data[index] {
		case 0x3b:
			return width, height, frames, frames > 0
		case 0x2c:
			frames++
			if index+10 > len(data) {
				return 0, 0, 0, false
			}
			// Every frame must be non-empty and lie inside the logical screen;
			// a frame larger than the screen cannot hide behind the screen size.
			left := int64(binary.LittleEndian.Uint16(data[index+1 : index+3]))
			top := int64(binary.LittleEndian.Uint16(data[index+3 : index+5]))
			frameWidth := int64(binary.LittleEndian.Uint16(data[index+5 : index+7]))
			frameHeight := int64(binary.LittleEndian.Uint16(data[index+7 : index+9]))
			if frameWidth <= 0 || frameHeight <= 0 || left+frameWidth > width || top+frameHeight > height {
				return 0, 0, 0, false
			}
			packed := data[index+9]
			index += 10
			if packed&0x80 != 0 {
				index += 3 * (1 << ((packed & 0x07) + 1))
			}
			if index >= len(data) {
				return 0, 0, 0, false
			}
			index++ // LZW minimum code size.
		case 0x21:
			if index+2 > len(data) {
				return 0, 0, 0, false
			}
			index += 2 // Extension introducer and label.
		default:
			return 0, 0, 0, false
		}
		var ok bool
		index, ok = skipSubBlocks(data, index)
		if !ok {
			return 0, 0, 0, false
		}
	}
	return 0, 0, 0, false
}

func skipSubBlocks(data []byte, index int) (int, bool) {
	for {
		if index >= len(data) {
			return 0, false
		}
		size := int(data[index])
		index++
		if size == 0 {
			return index, true
		}
		if index+size > len(data) {
			return 0, false
		}
		index += size
	}
}

func webPMetadata(data []byte) (int64, int64, int, bool) {
	if len(data) < 20 || uint64(binary.LittleEndian.Uint32(data[4:8]))+8 != uint64(len(data)) {
		return 0, 0, 0, false
	}
	kind, payload, offset, ok := nextWebPChunk(data, 12)
	if !ok {
		return 0, 0, 0, false
	}
	if kind != "VP8X" {
		width, height, ok := webPCodedDimensions(kind, payload)
		if !ok || offset != len(data) {
			return 0, 0, 0, false
		}
		return width, height, 1, true
	}
	if len(payload) != 10 || payload[0]&0xc1 != 0 {
		return 0, 0, 0, false
	}
	width := 1 + littleEndianUint24(payload[4:7])
	height := 1 + littleEndianUint24(payload[7:10])
	animated := payload[0]&0x02 != 0
	frames, images := 0, 0
	for offset < len(data) {
		kind, payload, next, ok := nextWebPChunk(data, offset)
		if !ok {
			return 0, 0, 0, false
		}
		switch kind {
		case "ANMF":
			if !animated || !validWebPAnimationFrame(payload, width, height) {
				return 0, 0, 0, false
			}
			frames++
		case "VP8 ", "VP8L":
			codedWidth, codedHeight, ok := webPCodedDimensions(kind, payload)
			if animated || !ok || codedWidth > width || codedHeight > height {
				return 0, 0, 0, false
			}
			images++
		}
		offset = next
	}
	if animated {
		return width, height, frames, frames > 0 && images == 0
	}
	return width, height, 1, images == 1 && frames == 0
}

func nextWebPChunk(data []byte, offset int) (string, []byte, int, bool) {
	if offset+8 > len(data) {
		return "", nil, 0, false
	}
	size := int(binary.LittleEndian.Uint32(data[offset+4 : offset+8]))
	next := offset + 8 + size + size%2
	if size < 0 || next < offset || next > len(data) {
		return "", nil, 0, false
	}
	return string(data[offset : offset+4]), data[offset+8 : offset+8+size], next, true
}

func webPCodedDimensions(kind string, payload []byte) (int64, int64, bool) {
	switch kind {
	case "VP8L":
		if len(payload) < 5 || payload[0] != 0x2f {
			return 0, 0, false
		}
		packed := binary.LittleEndian.Uint32(payload[1:5])
		return int64(packed&0x3fff) + 1, int64((packed>>14)&0x3fff) + 1, true
	case "VP8 ":
		if len(payload) < 10 || !bytes.Equal(payload[3:6], []byte{0x9d, 0x01, 0x2a}) {
			return 0, 0, false
		}
		width := binary.LittleEndian.Uint16(payload[6:8]) & 0x3fff
		height := binary.LittleEndian.Uint16(payload[8:10]) & 0x3fff
		return int64(width), int64(height), width > 0 && height > 0
	default:
		return 0, 0, false
	}
}

func validWebPAnimationFrame(payload []byte, canvasWidth, canvasHeight int64) bool {
	if len(payload) < 24 {
		return false
	}
	x := littleEndianUint24(payload[0:3]) * 2
	y := littleEndianUint24(payload[3:6]) * 2
	width := littleEndianUint24(payload[6:9]) + 1
	height := littleEndianUint24(payload[9:12]) + 1
	if x+width > canvasWidth || y+height > canvasHeight {
		return false
	}
	images := 0
	for offset := 16; offset < len(payload); {
		kind, image, next, ok := nextWebPChunk(payload, offset)
		if !ok {
			return false
		}
		if kind == "VP8 " || kind == "VP8L" {
			codedWidth, codedHeight, ok := webPCodedDimensions(kind, image)
			if !ok || codedWidth > width || codedHeight > height {
				return false
			}
			images++
		}
		offset = next
	}
	return images == 1
}

func littleEndianUint24(data []byte) int64 {
	return int64(data[0]) | int64(data[1])<<8 | int64(data[2])<<16
}

// apngFrames validates PNG chunk integrity and APNG control/data sequencing,
// or returns one for a complete non-animated PNG.
func apngFrames(data []byte, canvasWidth, canvasHeight int64) (int, bool) {
	offset := 8
	declared, controls := 0, 0
	sequence := uint64(0)
	hasAnimation, sawIHDR, sawIDAT, idatEnded := false, false, false, false
	frameOpen, frameUsesIDAT := false, false
	frameDataBytes, idatDataBytes := uint64(0), uint64(0)
	for offset+12 <= len(data) {
		size64 := uint64(binary.BigEndian.Uint32(data[offset : offset+4]))
		end64 := uint64(offset) + 12 + size64 // #nosec G115 -- offset is non-negative and bounded by len(data).
		if end64 > uint64(len(data)) {        // #nosec G115 -- len(data) is non-negative.
			return 0, false
		}
		size := int(size64) // #nosec G115 -- end64 is bounded by the in-memory input length.
		end := int(end64)   // #nosec G115 -- end64 is bounded by the in-memory input length.
		kind := string(data[offset+4 : offset+8])
		payload := data[offset+8 : offset+8+size]
		if crc32.ChecksumIEEE(data[offset+4:offset+8+size]) != binary.BigEndian.Uint32(data[offset+8+size:end]) {
			return 0, false
		}
		if sawIDAT && kind != "IDAT" {
			idatEnded = true
		}
		switch kind {
		case "IHDR":
			if sawIHDR || offset != 8 || size != 13 {
				return 0, false
			}
			sawIHDR = true
		case "acTL":
			if !sawIHDR || hasAnimation || sawIDAT || size != 8 {
				return 0, false
			}
			declared = int(binary.BigEndian.Uint32(payload[0:4]))
			if declared <= 0 || controls > declared {
				return 0, false
			}
			hasAnimation = true
		case "fcTL":
			if size != 26 || frameOpen && frameDataBytes == 0 ||
				uint64(binary.BigEndian.Uint32(payload[0:4])) != sequence ||
				!hasAnimation && (sawIDAT || controls != 0) || hasAnimation && controls >= declared {
				return 0, false
			}
			sequence++
			width := int64(binary.BigEndian.Uint32(payload[4:8]))
			height := int64(binary.BigEndian.Uint32(payload[8:12]))
			x := int64(binary.BigEndian.Uint32(payload[12:16]))
			y := int64(binary.BigEndian.Uint32(payload[16:20]))
			frameUsesIDAT = !sawIDAT && controls == 0
			if width <= 0 || height <= 0 || x+width > canvasWidth || y+height > canvasHeight ||
				frameUsesIDAT && (width != canvasWidth || height != canvasHeight || x != 0 || y != 0) ||
				payload[24] > 2 || payload[25] > 1 {
				return 0, false
			}
			controls++
			frameOpen, frameDataBytes = true, 0
		case "fdAT":
			if !hasAnimation || size < 4 || !frameOpen || frameUsesIDAT ||
				uint64(binary.BigEndian.Uint32(payload[0:4])) != sequence {
				return 0, false
			}
			sequence++
			frameDataBytes += uint64(size - 4) // #nosec G115 -- size is non-negative and bounded by the input.
		case "IDAT":
			if !sawIHDR || idatEnded || frameOpen && !frameUsesIDAT || controls > 1 {
				return 0, false
			}
			sawIDAT = true
			idatDataBytes += size64
			if frameOpen {
				frameDataBytes += size64
			}
		case "IEND":
			if size != 0 || end != len(data) || !sawIHDR || !sawIDAT || idatDataBytes == 0 {
				return 0, false
			}
			if !hasAnimation {
				if controls != 0 {
					return 0, false
				}
				return 1, true
			}
			return declared, controls == declared && frameOpen && frameDataBytes > 0
		}
		offset = end
	}
	return 0, false
}

func isMP4(data []byte) bool {
	return len(data) >= 12 && string(data[4:8]) == "ftyp"
}

type mp4Info struct {
	width, height, durationMS int64
	frameCount                int64
	durationKnown             bool
	pictureTracks             int
	container, codec          string
	sampleAuthority           bool

	moovCount, mvhdCount int
	mdatCount            int
	timescale            uint64
	movieDuration        uint64
	trackDurations       []uint64
	mediaDurations       []mp4Duration
	mediaDataRanges      []mp4ByteRange
	sampleRanges         []mp4SampleRange
	source               []byte
	unknownDuration      bool
}

type mp4Duration struct {
	value, timescale uint64
}

type mp4ByteRange struct {
	start, end uint64
}

type mp4SampleRange struct {
	mp4ByteRange

	descriptionIndex uint32
}

type mp4SampleToChunk struct {
	firstChunk, samplesPerChunk, descriptionIndex uint32
}

func mp4Metadata(data []byte) (mp4Info, bool) {
	mediaDataRanges, ok := mp4MediaDataRanges(data)
	if !ok {
		return mp4Info{}, false
	}
	info := mp4Info{mediaDataRanges: mediaDataRanges, source: data}
	if !scanMP4Boxes(data, &info, nil, "", 0) || info.moovCount != 1 || info.mvhdCount > 1 ||
		info.pictureTracks == 0 || info.width <= 0 || info.height <= 0 || info.codec == "" {
		return mp4Info{}, false
	}
	info.container = canonicalMP4Container(data)
	info.sampleAuthority = info.sampleAuthority && info.mdatCount > 0 &&
		mp4SampleRangesDoNotOverlap(info.sampleRanges)
	info.resolveDuration()
	return info, true
}

func mp4MediaDataRanges(data []byte) ([]mp4ByteRange, bool) {
	var ranges []mp4ByteRange
	for offset := 0; offset < len(data); {
		headerLen, size, ok := mp4BoxHeader(data, offset)
		if !ok {
			return nil, false
		}
		if string(data[offset+4:offset+8]) == "mdat" {
			ranges = append(ranges, mp4ByteRange{
				start: uint64(offset + headerLen), //nolint:gosec // offsets are non-negative and bounded by len(data)
				end:   uint64(offset + size),      //nolint:gosec // offsets are non-negative and bounded by len(data)
			})
		}
		offset += size
	}
	return ranges, true
}

const maxMP4Depth = 6

type mp4TrackInfo struct {
	tkhdCount, edtsCount, elstCount                  int
	hdlrCount, mdhdCount                             int
	stsdCount, sttsCount, cttsCount, sampleSizeCount int
	stscCount                                        int
	chunkOffsetCount                                 int
	chunkOffsets                                     []uint64
	sampleToChunks                                   []mp4SampleToChunk
	sampleDescriptionCount                           uint32
	sampleDescriptions                               []mp4CodecConfiguration
	defaultSampleSize                                uint32
	sampleSizes                                      []uint32
	handlerType                                      string
	presentationWidth, presentationHeight            int64
	codedWidth, codedHeight                          int64
	mediaTimescale, mediaDuration                    uint64
	sampleDuration                                   uint64
	editDuration                                     uint64
	timedSamples, compositionSamples                 uint64
	sampleCount                                      uint64
	hasVisualSamples                                 bool
	codec                                            string
	unknownSampleEntries                             int
}

// mp4BoxHeader returns the header length and total size of the box at offset,
// supporting the 64-bit largesize form (size 1) and the to-end-of-parent form
// (size 0). It fails on truncated, undersized, or overflowing boxes.
func mp4BoxHeader(data []byte, offset int) (headerLen, size int, ok bool) {
	if offset+8 > len(data) {
		return 0, 0, false
	}
	remaining := len(data) - offset // Non-negative: offset+8 <= len(data).
	size32 := binary.BigEndian.Uint32(data[offset : offset+4])
	switch size32 {
	case 0:
		return 8, remaining, true
	case 1:
		if offset+16 > len(data) {
			return 0, 0, false
		}
		large := binary.BigEndian.Uint64(data[offset+8 : offset+16])
		if large < 16 || large > math.MaxInt32 || int(large) > remaining {
			return 0, 0, false
		}
		return 16, int(large), true
	default:
		if size32 < 8 || size32 > math.MaxInt32 || int(size32) > remaining {
			return 0, 0, false
		}
		return 8, int(size32), true
	}
}

// scanMP4Boxes walks the container tree it needs: moov at the top level, trak
// under moov, and mvhd and tkhd headers within them. Structural boxes are
// only recognized in their authoritative position so a misplaced header
// cannot override the real one.
func scanMP4Boxes(data []byte, info *mp4Info, track *mp4TrackInfo, parent string, depth int) bool {
	if depth > maxMP4Depth {
		return false
	}
	for offset := 0; offset < len(data); {
		headerLen, size, ok := mp4BoxHeader(data, offset)
		if !ok {
			return false
		}
		kind := string(data[offset+4 : offset+8])
		payload := data[offset+headerLen : offset+size]
		switch {
		case kind == "moov" && parent == "":
			info.moovCount++
			if info.moovCount > 1 || !scanMP4Boxes(payload, info, nil, kind, depth+1) {
				return false
			}
		case kind == "mdat" && parent == "":
			info.mdatCount++
		case kind == "trak" && parent == "moov":
			trackInfo := &mp4TrackInfo{}
			if !scanMP4Boxes(payload, info, trackInfo, kind, depth+1) || !trackInfo.finish(info) {
				return false
			}
		case kind == "mdia" && parent == "trak":
			if !scanMP4Boxes(payload, info, track, kind, depth+1) {
				return false
			}
		case kind == "edts" && parent == "trak":
			if track == nil {
				return false
			}
			track.edtsCount++
			if track.edtsCount > 1 || !scanMP4Boxes(payload, info, track, kind, depth+1) {
				return false
			}
		case kind == "minf" && parent == "mdia":
			if !scanMP4Boxes(payload, info, track, kind, depth+1) {
				return false
			}
		case kind == "stbl" && parent == "minf":
			if !scanMP4Boxes(payload, info, track, kind, depth+1) {
				return false
			}
		case kind == "mvhd" && parent == "moov":
			info.mvhdCount++
			if info.mvhdCount > 1 || !parseMVHD(payload, info) {
				return false
			}
		case kind == "mvex" && parent == "moov", kind == "moof":
			// Fragmented MP4: fragments extend the timeline beyond what the
			// movie header declares, so any fragment marker leaves the
			// duration indeterminate.
			info.unknownDuration = true
		case kind == "tkhd" && parent == "trak":
			if track == nil || !parseTKHD(payload, info, track) {
				return false
			}
		case kind == "stsd" && parent == "stbl":
			if track == nil || !parseSTSD(payload, track) {
				return false
			}
		case kind == "stts" && parent == "stbl":
			if track == nil || !parseSTTS(payload, track) {
				return false
			}
		case kind == "ctts" && parent == "stbl":
			if track == nil || !parseCTTS(payload, info, track) {
				return false
			}
		case kind == "elst" && parent == "edts":
			if track == nil || !parseELST(payload, track) {
				return false
			}
		case (kind == "stsz" || kind == "stz2") && parent == "stbl":
			if track == nil || !parseSampleSize(kind, payload, track) {
				return false
			}
		case kind == "stsc" && parent == "stbl":
			if track == nil || !parseSTSC(payload, track) {
				return false
			}
		case (kind == "stco" || kind == "co64") && parent == "stbl":
			if track == nil || !parseChunkOffsets(kind, payload, track) {
				return false
			}
		case kind == "hdlr" && parent == "mdia":
			if track == nil || !parseHDLR(payload, track) {
				return false
			}
		case kind == "hdlr" && parent == "minf":
			// QuickTime may carry a separate data-handler declaration here.
			// It is not the media handler that establishes track modality.
			if len(payload) < 12 || payload[0] != 0 {
				return false
			}
		case kind == "mdhd" && parent == "mdia":
			if track == nil || !parseMDHD(payload, info, track) {
				return false
			}
		case kind == "moov" || kind == "trak" || kind == "edts" || kind == "elst" || kind == "mdia" || kind == "minf" ||
			kind == "stbl" || kind == "stsd" || kind == "stts" || kind == "ctts" || kind == "stsz" || kind == "stz2" || kind == "stsc" || kind == "stco" || kind == "co64" || kind == "mvhd" ||
			kind == "tkhd" || kind == "hdlr" || kind == "mdhd":
			// A structural header outside its authoritative position is not a
			// file this package can bound.
			return false
		}
		offset += size
	}
	return true
}

func parseMVHD(payload []byte, info *mp4Info) bool {
	if len(payload) < 20 {
		return false
	}
	// Full box: version(1) flags(3) creation modification timescale duration.
	switch payload[0] {
	case 0:
		info.timescale = uint64(binary.BigEndian.Uint32(payload[12:16]))
		info.movieDuration = uint64(binary.BigEndian.Uint32(payload[16:20]))
		if info.movieDuration == math.MaxUint32 {
			info.unknownDuration = true
		}
		return info.timescale > 0
	case 1:
		if len(payload) < 32 {
			return false
		}
		info.timescale = uint64(binary.BigEndian.Uint32(payload[20:24]))
		info.movieDuration = binary.BigEndian.Uint64(payload[24:32])
		if info.movieDuration == math.MaxUint64 {
			info.unknownDuration = true
		}
		return info.timescale > 0
	default:
		return false
	}
}

// parseTKHD records a track's duration in movie-timescale units and its
// presentation dimensions. Audio, subtitle, and hint tracks carry zero
// dimensions.
func parseTKHD(payload []byte, info *mp4Info, track *mp4TrackInfo) bool {
	// Full box: version(1) flags(3) creation modification track_ID reserved
	// duration ... width height. Reject trailing payload so dimensions can only
	// come from the canonical offsets for the declared version.
	var duration uint64
	var widthOffset int
	switch {
	case len(payload) == 84 && payload[0] == 0:
		duration = uint64(binary.BigEndian.Uint32(payload[20:24]))
		widthOffset = 76
		if duration == math.MaxUint32 {
			info.unknownDuration = true
		}
	case len(payload) == 96 && payload[0] == 1:
		duration = binary.BigEndian.Uint64(payload[28:36])
		widthOffset = 88
		if duration == math.MaxUint64 {
			info.unknownDuration = true
		}
	default:
		return false
	}
	track.tkhdCount++
	if track.tkhdCount > 1 {
		return false
	}
	info.trackDurations = append(info.trackDurations, duration)
	width := mp4Fixed1616Ceil(binary.BigEndian.Uint32(payload[widthOffset : widthOffset+4]))
	height := mp4Fixed1616Ceil(binary.BigEndian.Uint32(payload[widthOffset+4 : widthOffset+8]))
	track.presentationWidth, track.presentationHeight = width, height
	return true
}

func mp4Fixed1616Ceil(value uint32) int64 {
	result := int64(value >> 16)
	if value&math.MaxUint16 != 0 {
		result++
	}
	return result
}

// parseSTSD reads coded dimensions from visual sample entries. The entry
// payload follows ISO/IEC 14496-12 VisualSampleEntry: width and height are
// 16-bit integers at offsets 24 and 26 after the sample-entry box header.
func parseSTSD(payload []byte, track *mp4TrackInfo) bool {
	if len(payload) < 8 || payload[0] != 0 {
		return false
	}
	track.stsdCount++
	if track.stsdCount > 1 {
		return false
	}
	want := binary.BigEndian.Uint32(payload[4:8])
	entries := payload[8:]
	var seen uint32
	for offset := 0; offset < len(entries); {
		headerLen, size, ok := mp4BoxHeader(entries, offset)
		if !ok || seen == math.MaxUint32 {
			return false
		}
		seen++
		kind := string(entries[offset+4 : offset+8])
		entry := entries[offset+headerLen : offset+size]
		if isVisualSampleEntry(kind) {
			if len(entry) < 78 {
				return false
			}
			width := int64(binary.BigEndian.Uint16(entry[24:26]))
			height := int64(binary.BigEndian.Uint16(entry[26:28]))
			configuration, codecWidth, codecHeight, ok := visualCodecDimensions(kind, entry)
			if width <= 0 || height <= 0 || !ok {
				return false
			}
			if track.codec != "" && track.codec != configuration.codec {
				return false
			}
			track.hasVisualSamples = true
			track.codec = configuration.codec
			track.codedWidth = max(track.codedWidth, width, codecWidth)
			track.codedHeight = max(track.codedHeight, height, codecHeight)
			track.sampleDescriptions = append(track.sampleDescriptions, configuration)
		} else {
			track.unknownSampleEntries++
			track.sampleDescriptions = append(track.sampleDescriptions, mp4CodecConfiguration{})
		}
		offset += size
	}
	track.sampleDescriptionCount = seen
	return seen == want
}

func parseMDHD(payload []byte, info *mp4Info, track *mp4TrackInfo) bool {
	track.mdhdCount++
	if track.mdhdCount > 1 {
		return false
	}
	switch {
	case len(payload) == 24 && payload[0] == 0:
		track.mediaTimescale = uint64(binary.BigEndian.Uint32(payload[12:16]))
		track.mediaDuration = uint64(binary.BigEndian.Uint32(payload[16:20]))
		if track.mediaDuration == math.MaxUint32 {
			info.unknownDuration = true
		}
	case len(payload) == 36 && payload[0] == 1:
		track.mediaTimescale = uint64(binary.BigEndian.Uint32(payload[20:24]))
		track.mediaDuration = binary.BigEndian.Uint64(payload[24:32])
		if track.mediaDuration == math.MaxUint64 {
			info.unknownDuration = true
		}
	default:
		return false
	}
	return track.mediaTimescale > 0
}

func parseSTTS(payload []byte, track *mp4TrackInfo) bool {
	if len(payload) < 8 || payload[0] != 0 {
		return false
	}
	track.sttsCount++
	if track.sttsCount > 1 {
		return false
	}
	count := binary.BigEndian.Uint32(payload[4:8])
	if count > math.MaxInt32 || len(payload) != 8+int(count)*8 {
		return false
	}
	var total uint64
	var samples uint64
	for offset := 8; offset < len(payload); offset += 8 {
		sampleCount := uint64(binary.BigEndian.Uint32(payload[offset : offset+4]))
		sampleDelta := uint64(binary.BigEndian.Uint32(payload[offset+4 : offset+8]))
		if sampleCount == 0 || sampleDelta == 0 {
			return false
		}
		high, low := bits.Mul64(sampleCount, sampleDelta)
		if high != 0 || math.MaxUint64-total < low {
			return false
		}
		total += low
		if math.MaxUint64-samples < sampleCount {
			return false
		}
		samples += sampleCount
	}
	track.sampleDuration = total
	track.timedSamples = samples
	return true
}

// parseCTTS validates composition-time sample coverage. Composition offsets
// interact with edit lists and may extend the presentation timeline beyond
// decode timing, so their presence leaves duration unknown and therefore
// fails closed under a configured duration cap.
func parseCTTS(payload []byte, info *mp4Info, track *mp4TrackInfo) bool {
	if len(payload) < 8 || payload[0] > 1 || payload[1] != 0 || payload[2] != 0 || payload[3] != 0 {
		return false
	}
	track.cttsCount++
	if track.cttsCount > 1 {
		return false
	}
	count := binary.BigEndian.Uint32(payload[4:8])
	if count > math.MaxInt32 || len(payload) != 8+int(count)*8 {
		return false
	}
	var samples uint64
	for offset := 8; offset < len(payload); offset += 8 {
		sampleCount := uint64(binary.BigEndian.Uint32(payload[offset : offset+4]))
		if sampleCount == 0 || math.MaxUint64-samples < sampleCount {
			return false
		}
		samples += sampleCount
	}
	track.compositionSamples = samples
	info.unknownDuration = true
	return true
}

// parseELST validates an edit list and records its total presentation length.
// Segment durations use the movie timescale, so they can be compared directly
// with mvhd and tkhd after the track is complete.
func parseELST(payload []byte, track *mp4TrackInfo) bool {
	if len(payload) < 8 || payload[0] > 1 || payload[1] != 0 || payload[2] != 0 || payload[3] != 0 {
		return false
	}
	track.elstCount++
	if track.elstCount > 1 {
		return false
	}
	count := binary.BigEndian.Uint32(payload[4:8])
	entrySize := 12
	if payload[0] == 1 {
		entrySize = 20
	}
	if count == 0 || count > math.MaxInt32 || len(payload) != 8+int(count)*entrySize {
		return false
	}
	var total uint64
	for offset := 8; offset < len(payload); offset += entrySize {
		var duration uint64
		var validMediaTime bool
		var rateOffset int
		if payload[0] == 1 {
			duration = binary.BigEndian.Uint64(payload[offset : offset+8])
			mediaTime := binary.BigEndian.Uint64(payload[offset+8 : offset+16])
			validMediaTime = mediaTime <= math.MaxInt64 || mediaTime == math.MaxUint64
			rateOffset = offset + 16
		} else {
			duration = uint64(binary.BigEndian.Uint32(payload[offset : offset+4]))
			mediaTime := binary.BigEndian.Uint32(payload[offset+4 : offset+8])
			validMediaTime = mediaTime <= math.MaxInt32 || mediaTime == math.MaxUint32
			rateOffset = offset + 8
		}
		if duration == 0 || !validMediaTime || binary.BigEndian.Uint16(payload[rateOffset:rateOffset+2]) != 1 ||
			binary.BigEndian.Uint16(payload[rateOffset+2:rateOffset+4]) != 0 || math.MaxUint64-total < duration {
			return false
		}
		total += duration
	}
	track.editDuration = total
	return true
}

func parseSampleSize(kind string, payload []byte, track *mp4TrackInfo) bool {
	if len(payload) < 12 || payload[0] != 0 || payload[1] != 0 || payload[2] != 0 || payload[3] != 0 {
		return false
	}
	track.sampleSizeCount++
	if track.sampleSizeCount > 1 {
		return false
	}
	count := binary.BigEndian.Uint32(payload[8:12])
	if count > math.MaxInt32 {
		return false
	}
	samples := int(count) // #nosec G115 -- capped at MaxInt32 above.
	switch kind {
	case "stsz":
		track.defaultSampleSize = binary.BigEndian.Uint32(payload[4:8])
		if track.defaultSampleSize == 0 {
			if len(payload) != 12+samples*4 {
				return false
			}
			track.sampleSizes = make([]uint32, 0, samples)
			for offset := 12; offset < len(payload); offset += 4 {
				track.sampleSizes = append(track.sampleSizes, binary.BigEndian.Uint32(payload[offset:offset+4]))
			}
		} else if len(payload) != 12 {
			return false
		}
	case "stz2":
		var tableBytes int
		switch payload[7] {
		case 4:
			tableBytes = (samples + 1) / 2
		case 8:
			tableBytes = samples
		case 16:
			tableBytes = samples * 2
		default:
			return false
		}
		if len(payload) != 12+tableBytes {
			return false
		}
		track.sampleSizes = make([]uint32, 0, samples)
		switch payload[7] {
		case 4:
			for index := range samples {
				value := payload[12+index/2]
				if index%2 == 0 {
					track.sampleSizes = append(track.sampleSizes, uint32(value>>4))
				} else {
					track.sampleSizes = append(track.sampleSizes, uint32(value&0x0f))
				}
			}
		case 8:
			for _, value := range payload[12:] {
				track.sampleSizes = append(track.sampleSizes, uint32(value))
			}
		case 16:
			for offset := 12; offset < len(payload); offset += 2 {
				track.sampleSizes = append(track.sampleSizes, uint32(binary.BigEndian.Uint16(payload[offset:offset+2])))
			}
		}
	default:
		return false
	}
	track.sampleCount = uint64(count)
	return true
}

func parseSTSC(payload []byte, track *mp4TrackInfo) bool {
	if len(payload) < 8 || payload[0] != 0 || payload[1] != 0 || payload[2] != 0 || payload[3] != 0 {
		return false
	}
	track.stscCount++
	if track.stscCount != 1 {
		return false
	}
	count := binary.BigEndian.Uint32(payload[4:8])
	if count > math.MaxInt32 || len(payload) != 8+int(count)*12 {
		return false
	}
	track.sampleToChunks = make([]mp4SampleToChunk, 0, count)
	var previousFirst uint32
	for offset := 8; offset < len(payload); offset += 12 {
		entry := mp4SampleToChunk{
			firstChunk:       binary.BigEndian.Uint32(payload[offset : offset+4]),
			samplesPerChunk:  binary.BigEndian.Uint32(payload[offset+4 : offset+8]),
			descriptionIndex: binary.BigEndian.Uint32(payload[offset+8 : offset+12]),
		}
		if entry.firstChunk == 0 || entry.samplesPerChunk == 0 || entry.descriptionIndex == 0 ||
			len(track.sampleToChunks) == 0 && entry.firstChunk != 1 || previousFirst >= entry.firstChunk {
			return false
		}
		track.sampleToChunks = append(track.sampleToChunks, entry)
		previousFirst = entry.firstChunk
	}
	return true
}

func parseChunkOffsets(kind string, payload []byte, track *mp4TrackInfo) bool {
	if len(payload) < 8 || payload[0] != 0 || payload[1] != 0 || payload[2] != 0 || payload[3] != 0 {
		return false
	}
	track.chunkOffsetCount++
	if track.chunkOffsetCount != 1 {
		return false
	}
	count := binary.BigEndian.Uint32(payload[4:8])
	entryBytes := 4
	if kind == "co64" {
		entryBytes = 8
	}
	if count > math.MaxInt32 || len(payload) != 8+int(count)*entryBytes {
		return false
	}
	track.chunkOffsets = make([]uint64, 0, count)
	for offset := 8; offset < len(payload); offset += entryBytes {
		if kind == "co64" {
			track.chunkOffsets = append(track.chunkOffsets, binary.BigEndian.Uint64(payload[offset:offset+8]))
		} else {
			track.chunkOffsets = append(track.chunkOffsets, uint64(binary.BigEndian.Uint32(payload[offset:offset+4])))
		}
	}
	return true
}

func parseHDLR(payload []byte, track *mp4TrackInfo) bool {
	if len(payload) < 12 || payload[0] != 0 {
		return false
	}
	track.hdlrCount++
	if track.hdlrCount > 1 {
		return false
	}
	track.handlerType = string(payload[8:12])
	return true
}

func isVisualSampleEntry(kind string) bool {
	switch kind {
	case "avc1", "hvc1", "vp09", "av01":
		return true
	default:
		return false
	}
}

func (track *mp4TrackInfo) finish(info *mp4Info) bool {
	if track.tkhdCount != 1 || track.hdlrCount != 1 || track.mdhdCount != 1 || track.sttsCount != 1 ||
		track.sampleSizeCount != 1 || track.timedSamples != track.sampleCount ||
		track.edtsCount != track.elstCount ||
		track.cttsCount == 1 && track.compositionSamples != track.sampleCount {
		return false
	}
	if track.editDuration > 0 {
		info.trackDurations = append(info.trackDurations, track.editDuration)
	}
	info.mediaDurations = append(info.mediaDurations,
		mp4Duration{value: track.mediaDuration, timescale: track.mediaTimescale},
		mp4Duration{value: track.sampleDuration, timescale: track.mediaTimescale},
	)
	presentation := track.presentationWidth > 0 && track.presentationHeight > 0
	if (track.presentationWidth > 0) != (track.presentationHeight > 0) {
		return false
	}
	if track.handlerType != "vide" {
		return isNonVisualHandler(track.handlerType) && !presentation && !track.hasVisualSamples
	}
	if !presentation || !track.hasVisualSamples || track.stsdCount != 1 || track.unknownSampleEntries != 0 ||
		track.codedWidth <= 0 || track.codedHeight <= 0 || track.codec == "" {
		return false
	}
	if track.sampleCount > math.MaxInt64 {
		return false
	}
	trackSampleAuthority := track.proveSampleRanges(info)
	if info.pictureTracks == 0 {
		info.sampleAuthority = trackSampleAuthority
	} else {
		info.sampleAuthority = info.sampleAuthority && trackSampleAuthority
	}
	sampleCount := int64(track.sampleCount) // #nosec G115 -- checked against MaxInt64 above.
	if sampleCount > math.MaxInt64-info.frameCount {
		return false
	}
	info.frameCount += sampleCount
	info.pictureTracks++
	if info.codec != "" && info.codec != track.codec {
		return false
	}
	info.codec = track.codec
	info.width = max(info.width, track.presentationWidth, track.codedWidth)
	info.height = max(info.height, track.presentationHeight, track.codedHeight)
	return true
}

const maxMP4MappedSamples = 10_000

func (track *mp4TrackInfo) proveSampleRanges(info *mp4Info) bool {
	if track.stscCount != 1 || track.chunkOffsetCount != 1 || len(track.sampleToChunks) == 0 ||
		len(track.chunkOffsets) == 0 || track.sampleCount == 0 || track.sampleCount > maxMP4MappedSamples ||
		uint64(track.sampleToChunks[len(track.sampleToChunks)-1].firstChunk) > uint64(len(track.chunkOffsets)) {
		return false
	}
	sampleIndex := uint64(0)
	toChunkIndex := 0
	for chunkIndex, chunkOffset := range track.chunkOffsets {
		chunkNumber := uint32(chunkIndex + 1)
		for toChunkIndex+1 < len(track.sampleToChunks) &&
			track.sampleToChunks[toChunkIndex+1].firstChunk <= chunkNumber {
			toChunkIndex++
		}
		mapping := track.sampleToChunks[toChunkIndex]
		if mapping.firstChunk > chunkNumber || mapping.descriptionIndex > track.sampleDescriptionCount ||
			uint64(mapping.samplesPerChunk) > track.sampleCount-sampleIndex {
			return false
		}
		configuration := track.sampleDescriptions[mapping.descriptionIndex-1]
		cursor := chunkOffset
		for range mapping.samplesPerChunk {
			size, ok := track.sampleSize(sampleIndex)
			if !ok || size == 0 || math.MaxUint64-cursor < size {
				return false
			}
			sampleRange := mp4SampleRange{
				start: cursor, end: cursor + size,
				descriptionIndex: mapping.descriptionIndex,
			}
			if !mp4RangeInsideMediaData(sampleRange.mp4ByteRange, info.mediaDataRanges) {
				return false
			}
			if sampleRange.end > uint64(len(info.source)) {
				return false
			}
			start := int(sampleRange.start) //nolint:gosec // proven no greater than the source length above
			end := int(sampleRange.end)     //nolint:gosec // proven no greater than the source length above
			if !validMP4CodecSample(configuration, info.source[start:end]) {
				return false
			}
			info.sampleRanges = append(info.sampleRanges, sampleRange)
			cursor += size
			sampleIndex++
		}
	}
	return sampleIndex == track.sampleCount
}

func (track *mp4TrackInfo) sampleSize(index uint64) (uint64, bool) {
	if index >= track.sampleCount {
		return 0, false
	}
	if track.defaultSampleSize != 0 {
		return uint64(track.defaultSampleSize), true
	}
	if index >= uint64(len(track.sampleSizes)) {
		return 0, false
	}
	return uint64(track.sampleSizes[index]), true
}

func mp4RangeInsideMediaData(sample mp4ByteRange, mediaData []mp4ByteRange) bool {
	if sample.start >= sample.end {
		return false
	}
	for _, bounds := range mediaData {
		if sample.start >= bounds.start && sample.end <= bounds.end {
			return true
		}
	}
	return false
}

func mp4SampleRangesDoNotOverlap(samples []mp4SampleRange) bool {
	if len(samples) == 0 {
		return false
	}
	sort.Slice(samples, func(left, right int) bool {
		if samples[left].start == samples[right].start {
			return samples[left].end < samples[right].end
		}
		return samples[left].start < samples[right].start
	})
	for index := 1; index < len(samples); index++ {
		if samples[index].start < samples[index-1].end {
			return false
		}
	}
	return true
}

func canonicalMP4Container(data []byte) string {
	if len(data) >= 12 && string(data[8:12]) == "qt  " {
		return "quicktime"
	}
	return "mp4"
}

func isNonVisualHandler(handler string) bool {
	switch handler {
	case "soun", "hint", "subt", "text", "sbtl", "clcp", "meta", "tmcd":
		return true
	default:
		return false
	}
}

// resolveDuration converts every movie, track, media, and sample-table duration
// into milliseconds and retains the longest. A track that declares its
// duration unknown or a fragmented layout keeps the result unknown so a
// duration cap refuses the file.
func (info *mp4Info) resolveDuration() {
	if info.unknownDuration || info.timescale == 0 {
		return
	}
	var milliseconds int64
	for _, duration := range append([]uint64{info.movieDuration}, info.trackDurations...) {
		value, ok := mp4DurationMilliseconds(duration, info.timescale)
		if !ok {
			return
		}
		milliseconds = max(milliseconds, value)
	}
	for _, duration := range info.mediaDurations {
		value, ok := mp4DurationMilliseconds(duration.value, duration.timescale)
		if !ok {
			return
		}
		milliseconds = max(milliseconds, value)
	}
	if milliseconds > 0 {
		info.durationMS = milliseconds
		info.durationKnown = true
	}
}

func mp4DurationMilliseconds(duration, timescale uint64) (int64, bool) {
	if duration == 0 {
		return 0, true
	}
	if timescale == 0 {
		return 0, false
	}
	whole := duration / timescale
	if whole > math.MaxInt64/1000 {
		return 0, false
	}
	high, low := bits.Mul64(duration%timescale, 1000)
	fractional, remainder := bits.Div64(high, low, timescale)
	milliseconds := whole*1000 + fractional
	if remainder > 0 {
		milliseconds++
	}
	if milliseconds > math.MaxInt64 {
		return 0, false
	}
	return int64(milliseconds), true
}
