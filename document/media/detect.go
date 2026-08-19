package media

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"image"
	"io"
	"math"
	"math/bits"

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
		return Metadata{
			Format: FormatMP4, Kind: KindVideo, MediaType: "video/mp4",
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

// apngFrames verifies that an APNG's declared frame count matches its actual
// frame-control chunks, or returns one for a complete non-animated PNG.
func apngFrames(data []byte, canvasWidth, canvasHeight int64) (int, bool) {
	offset := 8
	declared, controls := 0, 0
	hasAnimation, sawImageData := false, false
	for offset+12 <= len(data) {
		size := int(binary.BigEndian.Uint32(data[offset : offset+4]))
		kind := string(data[offset+4 : offset+8])
		if size < 0 || offset+12+size > len(data) {
			return 0, false
		}
		switch kind {
		case "acTL":
			if hasAnimation || sawImageData || size != 8 {
				return 0, false
			}
			declared = int(binary.BigEndian.Uint32(data[offset+8 : offset+12]))
			if declared <= 0 {
				return 0, false
			}
			hasAnimation = true
		case "fcTL":
			if !hasAnimation || size != 26 {
				return 0, false
			}
			width := int64(binary.BigEndian.Uint32(data[offset+12 : offset+16]))
			height := int64(binary.BigEndian.Uint32(data[offset+16 : offset+20]))
			x := int64(binary.BigEndian.Uint32(data[offset+20 : offset+24]))
			y := int64(binary.BigEndian.Uint32(data[offset+24 : offset+28]))
			if width <= 0 || height <= 0 || x+width > canvasWidth || y+height > canvasHeight {
				return 0, false
			}
			controls++
		case "fdAT":
			if !hasAnimation {
				return 0, false
			}
		case "IDAT":
			sawImageData = true
		case "IEND":
			if size != 0 || offset+12 != len(data) {
				return 0, false
			}
			if !hasAnimation {
				return 1, true
			}
			return declared, controls == declared
		}
		offset += 12 + size
	}
	return 0, false
}

func isMP4(data []byte) bool {
	return len(data) >= 12 && string(data[4:8]) == "ftyp"
}

type mp4Info struct {
	width, height, durationMS int64
	durationKnown             bool
	pictureTracks             int

	moovCount, mvhdCount int
	timescale            uint64
	movieDuration        uint64
	trackDurations       []uint64
	unknownTrackDuration bool
}

func mp4Metadata(data []byte) (mp4Info, bool) {
	var info mp4Info
	if !scanMP4Boxes(data, &info, nil, "", 0) || info.moovCount != 1 || info.mvhdCount > 1 ||
		info.pictureTracks == 0 || info.width <= 0 || info.height <= 0 {
		return mp4Info{}, false
	}
	info.resolveDuration()
	return info, true
}

const maxMP4Depth = 6

type mp4TrackInfo struct {
	tkhdCount, hdlrCount, stsdCount       int
	handlerType                           string
	presentationWidth, presentationHeight int64
	codedWidth, codedHeight               int64
	hasVisualSamples                      bool
	unknownSampleEntries                  int
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
		case kind == "trak" && parent == "moov":
			trackInfo := &mp4TrackInfo{}
			if !scanMP4Boxes(payload, info, trackInfo, kind, depth+1) || !trackInfo.finish(info) {
				return false
			}
		case kind == "mdia" && parent == "trak":
			if !scanMP4Boxes(payload, info, track, kind, depth+1) {
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
		case kind == "mvex" && parent == "moov":
			// Fragmented MP4: the movie header does not describe the fragments
			// that follow, so the timeline is indeterminate.
			info.unknownTrackDuration = true
		case kind == "tkhd" && parent == "trak":
			if track == nil || !parseTKHD(payload, info, track) {
				return false
			}
		case kind == "stsd" && parent == "stbl":
			if track == nil || !parseSTSD(payload, track) {
				return false
			}
		case kind == "hdlr" && parent == "mdia":
			if track == nil || !parseHDLR(payload, track) {
				return false
			}
		case kind == "moov" || kind == "trak" || kind == "mdia" || kind == "minf" ||
			kind == "stbl" || kind == "stsd" || kind == "mvhd" || kind == "tkhd" || kind == "hdlr":
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
		return true
	case 1:
		if len(payload) < 32 {
			return false
		}
		info.timescale = uint64(binary.BigEndian.Uint32(payload[20:24]))
		info.movieDuration = binary.BigEndian.Uint64(payload[24:32])
		return true
	default:
		return false
	}
}

// parseTKHD records a track's duration in movie-timescale units and, for
// picture tracks, keeps the largest area, enabled or not, so a small leading
// track cannot hide a large one from the pixel bound. Audio, subtitle, and
// hint tracks carry zero dimensions.
func parseTKHD(payload []byte, info *mp4Info, track *mp4TrackInfo) bool {
	// Full box: version(1) flags(3) creation modification track_ID reserved
	// duration ... width height. Duration sits at 20 (v0, 32-bit) or 28 (v1,
	// 64-bit); width and height are the last eight bytes.
	var duration uint64
	switch {
	case len(payload) >= 84 && payload[0] == 0:
		duration = uint64(binary.BigEndian.Uint32(payload[20:24]))
		if duration == math.MaxUint32 {
			info.unknownTrackDuration = true
		}
	case len(payload) >= 96 && payload[0] == 1:
		duration = binary.BigEndian.Uint64(payload[28:36])
		if duration == math.MaxUint64 {
			info.unknownTrackDuration = true
		}
	default:
		return false
	}
	track.tkhdCount++
	if track.tkhdCount > 1 {
		return false
	}
	info.trackDurations = append(info.trackDurations, duration)
	width := int64(binary.BigEndian.Uint32(payload[len(payload)-8:len(payload)-4]) >> 16)
	height := int64(binary.BigEndian.Uint32(payload[len(payload)-4:]) >> 16)
	track.presentationWidth, track.presentationHeight = width, height
	return true
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
			if len(entry) < 28 {
				return false
			}
			width := int64(binary.BigEndian.Uint16(entry[24:26]))
			height := int64(binary.BigEndian.Uint16(entry[26:28]))
			if width <= 0 || height <= 0 {
				return false
			}
			track.hasVisualSamples = true
			if width*height > track.codedWidth*track.codedHeight {
				track.codedWidth, track.codedHeight = width, height
			}
		} else {
			track.unknownSampleEntries++
		}
		offset += size
	}
	return seen == want
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
	case "avc1", "avc2", "avc3", "avc4", "hvc1", "hev1", "vp08", "vp09", "av01", "mp4v":
		return true
	default:
		return false
	}
}

func (track *mp4TrackInfo) finish(info *mp4Info) bool {
	if track.tkhdCount != 1 || track.hdlrCount != 1 {
		return false
	}
	presentation := track.presentationWidth > 0 && track.presentationHeight > 0
	if (track.presentationWidth > 0) != (track.presentationHeight > 0) {
		return false
	}
	if track.handlerType != "vide" {
		return isNonVisualHandler(track.handlerType) && !presentation && !track.hasVisualSamples
	}
	if !presentation || !track.hasVisualSamples || track.stsdCount != 1 || track.unknownSampleEntries != 0 ||
		track.codedWidth <= 0 || track.codedHeight <= 0 {
		return false
	}
	info.pictureTracks++
	width, height := track.presentationWidth, track.presentationHeight
	if track.codedWidth*track.codedHeight > width*height {
		width, height = track.codedWidth, track.codedHeight
	}
	if width*height > info.width*info.height {
		info.width, info.height = width, height
	}
	return true
}

func isNonVisualHandler(handler string) bool {
	switch handler {
	case "soun", "hint", "subt", "text", "sbtl", "clcp", "meta", "tmcd":
		return true
	default:
		return false
	}
}

// resolveDuration converts the longest declared duration — the movie header
// or any track — into milliseconds. Without a movie timescale, with a track
// that declares its duration unknown, or with a fragmented layout, the
// duration stays unknown so a duration cap refuses the file.
func (info *mp4Info) resolveDuration() {
	if info.timescale == 0 || info.unknownTrackDuration {
		return
	}
	longest := info.movieDuration
	for _, duration := range info.trackDurations {
		longest = max(longest, duration)
	}
	if longest == 0 {
		return
	}
	whole := longest / info.timescale
	if whole > math.MaxInt64/1000 {
		return
	}
	high, low := bits.Mul64(longest%info.timescale, 1000)
	fractional, remainder := bits.Div64(high, low, info.timescale)
	milliseconds := whole*1000 + fractional
	if remainder > 0 {
		milliseconds++
	}
	if milliseconds <= math.MaxInt64 {
		info.durationMS = int64(milliseconds)
		info.durationKnown = true
	}
}
