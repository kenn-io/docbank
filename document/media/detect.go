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
		frames, ok := apngFrames(data)
		if !ok {
			return Metadata{}, ErrMalformedMedia
		}
		metadata.FrameCount, metadata.Animated = frames, frames > 1
		return metadata, nil
	case len(data) >= 12 && string(data[:4]) == "RIFF" && string(data[8:12]) == "WEBP":
		width, height, ok := webPDimensions(data)
		if !ok {
			return Metadata{}, ErrMalformedMedia
		}
		frames, ok := webPFrames(data)
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

func webPDimensions(data []byte) (int64, int64, bool) {
	if len(data) < 21 {
		return 0, 0, false
	}
	switch string(data[12:16]) {
	case "VP8X":
		if len(data) < 30 {
			return 0, 0, false
		}
		width := 1 + int64(data[24]) + int64(data[25])<<8 + int64(data[26])<<16
		height := 1 + int64(data[27]) + int64(data[28])<<8 + int64(data[29])<<16
		return width, height, true
	case "VP8L":
		if len(data) < 25 || data[20] != 0x2f {
			return 0, 0, false
		}
		packed := binary.LittleEndian.Uint32(data[21:25])
		return int64(packed&0x3fff) + 1, int64((packed>>14)&0x3fff) + 1, true
	case "VP8 ":
		if len(data) < 30 || !bytes.Equal(data[23:26], []byte{0x9d, 0x01, 0x2a}) {
			return 0, 0, false
		}
		width := binary.LittleEndian.Uint16(data[26:28]) & 0x3fff
		height := binary.LittleEndian.Uint16(data[28:30]) & 0x3fff
		return int64(width), int64(height), width > 0 && height > 0
	default:
		return 0, 0, false
	}
}

// webPFrames counts ANMF chunks when the VP8X animation flag is set. A still
// WebP reports one frame. It fails on a truncated or unwalkable chunk list.
func webPFrames(data []byte) (int, bool) {
	if string(data[12:16]) != "VP8X" {
		return 1, true
	}
	if len(data) < 21 || data[20]&0x02 == 0 {
		return 1, true
	}
	frames := 0
	offset := 12
	for offset+8 <= len(data) {
		size := int(binary.LittleEndian.Uint32(data[offset+4 : offset+8]))
		if size < 0 || offset+8+size > len(data) {
			return 0, false
		}
		if string(data[offset:offset+4]) == "ANMF" {
			frames++
		}
		offset += 8 + size + size%2
	}
	if frames == 0 {
		return 0, false
	}
	return frames, true
}

// apngFrames returns the acTL frame count of an APNG, or one when the PNG has
// no animation control chunk before its first image data.
func apngFrames(data []byte) (int, bool) {
	offset := 8
	for offset+8 <= len(data) {
		size := int(binary.BigEndian.Uint32(data[offset : offset+4]))
		kind := string(data[offset+4 : offset+8])
		if size < 0 || offset+12+size > len(data) {
			return 0, false
		}
		switch kind {
		case "acTL":
			if size < 8 {
				return 0, false
			}
			frames := int(binary.BigEndian.Uint32(data[offset+8 : offset+12]))
			if frames <= 0 {
				return 0, false
			}
			return frames, true
		case "IDAT", "IEND":
			return 1, true
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
}

func mp4Metadata(data []byte) (mp4Info, bool) {
	var info mp4Info
	if !scanMP4Boxes(data, &info, 0) || info.width <= 0 || info.height <= 0 {
		return mp4Info{}, false
	}
	return info, true
}

const maxMP4Depth = 8

func scanMP4Boxes(data []byte, info *mp4Info, depth int) bool {
	if depth > maxMP4Depth {
		return false
	}
	for offset := 0; offset+8 <= len(data); {
		size := int(binary.BigEndian.Uint32(data[offset : offset+4]))
		if size < 8 || offset+size > len(data) {
			return false
		}
		kind := string(data[offset+4 : offset+8])
		payload := data[offset+8 : offset+size]
		switch kind {
		case "moov", "trak", "mdia", "minf", "stbl":
			if !scanMP4Boxes(payload, info, depth+1) {
				return false
			}
		case "mvhd":
			parseMVHD(payload, info)
		case "tkhd":
			// Audio, subtitle, and hint tracks carry zero dimensions. Among
			// picture tracks keep the largest area, enabled or not, so a small
			// leading track cannot hide a large one from the pixel bound.
			if len(payload) >= 8 {
				width := int64(binary.BigEndian.Uint32(payload[len(payload)-8:len(payload)-4]) >> 16)
				height := int64(binary.BigEndian.Uint32(payload[len(payload)-4:]) >> 16)
				if width > 0 && height > 0 && width*height > info.width*info.height {
					info.width, info.height = width, height
				}
			}
		}
		offset += size
	}
	return true
}

func parseMVHD(payload []byte, info *mp4Info) {
	if len(payload) < 20 {
		return
	}
	var timescale, duration uint64
	if payload[0] == 1 {
		if len(payload) < 32 {
			return
		}
		timescale = uint64(binary.BigEndian.Uint32(payload[20:24]))
		duration = binary.BigEndian.Uint64(payload[24:32])
	} else {
		timescale = uint64(binary.BigEndian.Uint32(payload[12:16]))
		duration = uint64(binary.BigEndian.Uint32(payload[16:20]))
	}
	if timescale == 0 || duration == 0 {
		return
	}
	whole := duration / timescale
	if whole > math.MaxInt64/1000 {
		return
	}
	high, low := bits.Mul64(duration%timescale, 1000)
	fractional, _ := bits.Div64(high, low, timescale)
	milliseconds := whole*1000 + fractional
	if milliseconds <= math.MaxInt64 {
		info.durationMS = int64(milliseconds)
		info.durationKnown = true
	}
}
