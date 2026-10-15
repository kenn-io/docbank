package processing

import (
	"bufio"
	"bytes"
	"compress/zlib"
	"context"
	"encoding/binary"
	"encoding/xml"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"maps"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"golang.org/x/text/encoding/charmap"

	"go.kenn.io/docbank/document/bundle"
	"go.kenn.io/docbank/internal/store"
)

// maxPhotoSidecarBytes bounds each metadata packet an export reads or writes.
const maxPhotoSidecarBytes = 1 << 20

const photoXMPJPEGPrefix = "http://ns.adobe.com/xap/1.0/\x00"
const photoXMPPNGKeyword = "XML:com.adobe.xmp"
const sourceMetadataCreatorField = "creator"
const microsoftPhotoXMPNamespace = "http://ns.microsoft.com/photo/1.0"

// Source layout and thumbnail offsets do not describe the rendered pixels.
var photoEXIFSourceLayoutTags = []uint16{0x00fe, 0x00ff, 0x0102, 0x0103, 0x0106, 0x010a, 0x0111, 0x0115, 0x0116, 0x0117, 0x011c, 0x013d, 0x0144, 0x0145, 0x014a, 0x0153, 0x0201, 0x0202}

// Mosaic geometry, camera calibration and profiles, raw digests, opcodes, and
// depth maps apply to the RAW source, not to rendered pixels. DNG camera model,
// serial number, lens, and original file name stay as descriptive metadata.
var photoEXIFRAWCalibrationTags = []uint16{
	0x828d, 0x828e, 0xc612, 0xc613, 0xc616, 0xc617, 0xc618, 0xc619, 0xc61a, 0xc61b,
	0xc61c, 0xc61d, 0xc61e, 0xc61f, 0xc620, 0xc621, 0xc622, 0xc623, 0xc624, 0xc625,
	0xc626, 0xc627, 0xc628, 0xc629, 0xc62a, 0xc62b, 0xc62c, 0xc62d, 0xc62e, 0xc631,
	0xc632, 0xc633, 0xc634, 0xc635, 0xc65a, 0xc65b, 0xc65c, 0xc65d, 0xc68c, 0xc68d,
	0xc68e, 0xc68f, 0xc690, 0xc691, 0xc692, 0xc6bf, 0xc6f3, 0xc6f4, 0xc6f5, 0xc6f6,
	0xc6f7, 0xc6f8, 0xc6f9, 0xc6fa, 0xc6fb, 0xc6fc, 0xc6fd, 0xc6fe, 0xc714, 0xc715,
	0xc716, 0xc717, 0xc718, 0xc719, 0xc71a, 0xc71b, 0xc71c, 0xc71d, 0xc71e, 0xc71f,
	0xc725, 0xc726, 0xc740, 0xc741, 0xc74e, 0xc761, 0xc791, 0xc792, 0xc793, 0xc7a3,
	0xc7a4, 0xc7a5, 0xc7a6, 0xc7a7, 0xc7a8, 0xc7b5, 0xc7e9, 0xc7ea, 0xc7eb, 0xc7ec,
	0xc7ed, 0xc7ee,
}

// photoEXIFRAWCalibrationTag also covers the DNG 1.6 and 1.7 tag block, which
// holds only raw-processing data.
func photoEXIFRAWCalibrationTag(tag uint16) bool {
	return slices.Contains(photoEXIFRAWCalibrationTags, tag) || tag >= 0xcd2d && tag <= 0xcd4b
}

// XMP, IPTC and ICC use separate packets; MakerNote offsets cannot be rewritten safely.
var photoEXIFEmbeddedMetadataTags = []uint16{700, 0x83bb, 0x8649, 0x8773, 0x927c}

var photoEXIFRatingTags = []uint16{0x4746, 0x4749}
var photoEXIFCaptionTags = []uint16{0x010e, 0x9c9b, 0x9c9c, 0x9c9f}
var photoEXIFCreatorTags = []uint16{0x013b, 0x9c9d}

var errPhotoExtendedXMP = errors.New("extended XMP cannot be retained; turn image metadata off")
var errPhotoXMPKeywords = errors.New("qualified XMP keywords cannot be merged with embedded IPTC keywords; add catalog tags or turn image metadata off")
var errPhotoXMPOrientation = errors.New("XMP orientation cannot be retained; turn image metadata off")

type photoPackets struct {
	exif, xmp, icc   []byte
	keywords         []string
	rawPreview       *visualPreviewRAWLocation
	unsupportedColor bool
	grayICC          bool
	orientation      int
	animated         bool
}

func photoExportMetadata(ctx context.Context, packets photoPackets, input store.PhotoExportInput, receipt bundle.PhotoRenderReceipt, sourceOrientation int, pixels []byte) ([]byte, error) {
	exif, packet := packets.exif, packets.xmp
	if !receipt.Profile.IncludeMetadata {
		exif, packet = nil, nil
	}
	var err error
	if receipt.Profile.IncludeMetadata {
		if len(exif) > 0 {
			exif, err = rewritePhotoEXIF(exif, receipt.Width, receipt.Height, receipt.Profile.RemoveGPS, input.Authored, len(input.Keywords) > 0)
			if err != nil {
				return nil, err
			}
		}
		packet, err = mergePhotoXMP(ctx, packet, input, receipt, sourceOrientation, packets.keywords)
		if err != nil {
			return nil, err
		}
	}
	var out bytes.Buffer
	if receipt.Profile.Format == visualFormatJPEG {
		out.Write(pixels[:2])
		for _, payload := range [][]byte{append([]byte("Exif\x00\x00"), exif...), append([]byte(photoXMPJPEGPrefix), packet...)} {
			if !receipt.Profile.IncludeMetadata || bytes.HasPrefix(payload, []byte("Exif")) && len(exif) == 0 || bytes.HasPrefix(payload, []byte(photoXMPJPEGPrefix)) && len(packet) == 0 {
				continue
			}
			if len(payload) > 65533 {
				return nil, fmt.Errorf("%w: retained image metadata exceeds JPEG capacity; choose PNG or turn image metadata off", bundle.ErrLimit)
			}
			out.Write([]byte{0xff, 0xe1, byte(((len(payload) + 2) >> 8) & 0xff), byte((len(payload) + 2) & 0xff)})
			out.Write(payload)
		}
		if len(packets.icc) > 0 {
			const size = 65519
			count := (len(packets.icc) + size - 1) / size
			for i := range count {
				payload := append([]byte("ICC_PROFILE\x00"), byte((i+1)&0xff), byte(count&0xff))
				payload = append(payload, packets.icc[i*size:min((i+1)*size, len(packets.icc))]...)
				out.Write([]byte{0xff, 0xe2, byte(((len(payload) + 2) >> 8) & 0xff), byte((len(payload) + 2) & 0xff)})
				out.Write(payload)
			}
		}
		out.Write(pixels[2:])
	} else {
		out.Write(pixels[:33])
		if len(packets.icc) > 0 {
			var compressed bytes.Buffer
			w := zlib.NewWriter(&compressed)
			_, err = w.Write(packets.icc)
			err = errors.Join(err, w.Close())
			if err != nil {
				return nil, err
			}
			writePhotoPNGChunk(&out, "iCCP", append([]byte("Profile\x00\x00"), compressed.Bytes()...))
		}
		if len(exif) > 0 {
			writePhotoPNGChunk(&out, "eXIf", exif)
		}
		if len(packet) > 0 {
			writePhotoPNGChunk(&out, "iTXt", append([]byte(photoXMPPNGKeyword+"\x00\x00\x00\x00\x00"), packet...))
		}
		out.Write(pixels[33:])
	}
	return out.Bytes(), nil
}

func writePhotoPNGChunk(out *bytes.Buffer, kind string, payload []byte) {
	var n [4]byte
	binary.BigEndian.PutUint32(n[:], uint32(uint64(len(payload))&0xffffffff))
	out.Write(n[:])
	out.WriteString(kind)
	out.Write(payload)
	crc := crc32.NewIEEE()
	_, _ = crc.Write([]byte(kind))
	_, _ = crc.Write(payload)
	binary.BigEndian.PutUint32(n[:], crc.Sum32())
	out.Write(n[:])
}

func readPhotoIPTCKeywords(data []byte) ([]string, error) {
	invalid := errors.New("malformed or unsupported IPTC keywords; turn image metadata off")
	if len(data) > maxPhotoSidecarBytes {
		return nil, errVisualMetadataLimit
	}
	var values [][]byte
	charset := ""
	for len(data) > 0 {
		if len(data) <= 3 && len(bytes.Trim(data, "\x00")) == 0 {
			break
		}
		if len(data) < 5 || data[0] != 0x1c {
			return nil, invalid
		}
		record, dataset := data[1], data[2]
		n := int(binary.BigEndian.Uint16(data[3:5]))
		data = data[5:]
		if n&0x8000 != 0 {
			width := n & 0x7fff
			if width < 1 || width > 4 || width > len(data) {
				return nil, invalid
			}
			length := uint64(0)
			for _, b := range data[:width] {
				length = length<<8 | uint64(b)
			}
			data = data[width:]
			if length > uint64(len(data)) {
				return nil, invalid
			}
			n = int(length)
		}
		if n > len(data) {
			return nil, invalid
		}
		value := data[:n]
		data = data[n:]
		if record == 1 && dataset == 90 {
			if charset != "" {
				return nil, invalid
			}
			charset = string(value)
		}
		if record == 2 && dataset == 25 {
			values = append(values, value)
		}
	}
	var keywords []string
	seen := map[string]bool{}
	for _, value := range values {
		keyword := string(value)
		switch charset {
		case "\x1b-A":
			decoded, err := charmap.ISO8859_1.NewDecoder().Bytes(value)
			if err != nil {
				return nil, invalid
			}
			keyword = string(decoded)
		case "\x1b%G":
		case "":
			for _, b := range value {
				if b >= utf8.RuneSelf {
					return nil, invalid
				}
			}
		default:
			return nil, invalid
		}
		if validatePhotoXMPText(keyword) != nil {
			return nil, invalid
		}
		if keyword != "" && !seen[keyword] {
			seen[keyword] = true
			keywords = append(keywords, keyword)
		}
	}
	return keywords, nil
}

func photoTIFFIPTCKeywords(reader exifReader, tag uint16, entry exifEntry) ([]string, error) {
	value := entry.value
	if entry.kind != 1 && entry.kind != 4 && entry.kind != 7 {
		return nil, errors.New("unsupported TIFF IPTC type; turn image metadata off")
	}
	if entry.kind == 4 && reader.order == binary.LittleEndian {
		value = bytes.Clone(value)
		for i := 0; i < len(value); i += 4 {
			slices.Reverse(value[i : i+4])
		}
	}
	if tag == 0x8649 {
		return photoPhotoshopKeywords(value)
	}
	return readPhotoIPTCKeywords(value)
}

func photoPhotoshopKeywords(data []byte) ([]string, error) {
	data = bytes.TrimPrefix(data, []byte("Photoshop 3.0\x00"))
	var keywords []string
	for len(data) > 0 {
		if len(data) < 7 || string(data[:4]) != "8BIM" {
			return nil, errors.New("malformed Photoshop IPTC resource; turn image metadata off")
		}
		id := binary.BigEndian.Uint16(data[4:6])
		nameBytes := (int(data[6]) + 2) &^ 1
		if len(data) < 6+nameBytes+4 {
			return nil, errors.New("malformed Photoshop IPTC resource name; turn image metadata off")
		}
		data = data[6+nameBytes:]
		n := uint64(binary.BigEndian.Uint32(data))
		data = data[4:]
		padded := n + n%2
		if padded > uint64(len(data)) {
			return nil, errors.New("malformed Photoshop IPTC resource length; turn image metadata off")
		}
		if id == 0x0404 {
			values, err := readPhotoIPTCKeywords(data[:n])
			if err != nil {
				return nil, err
			}
			keywords = append(keywords, values...)
		}
		data = data[padded:]
	}
	return keywords, nil
}

// photoSourcePackets bounds metadata independently of the source's pixel bytes.
func photoSourcePackets(ctx context.Context, data []byte, metadata, retainKeywords bool) (result photoPackets, err error) {
	result.orientation = 1
	keywordBytes := 0
	addKeywords := func(values []string) error {
		for _, value := range values {
			keywordBytes += len(value)
			if keywordBytes > maxPhotoSidecarBytes {
				return errVisualMetadataLimit
			}
		}
		result.keywords = append(result.keywords, values...)
		return nil
	}
	inspectIPTC := func(reader exifReader, root map[uint16]exifEntry) error {
		if !retainKeywords {
			return nil
		}
		for _, tag := range []uint16{0x83bb, 0x8649} {
			if entry, found := root[tag]; found {
				keywords, err := photoTIFFIPTCKeywords(reader, tag, entry)
				if err != nil {
					return err
				}
				if err := addKeywords(keywords); err != nil {
					return err
				}
			}
		}
		return nil
	}
	inspectEXIF := func(payload []byte) error {
		if len(payload) > visualPreviewMaxEXIFBytes {
			return errVisualMetadataLimit
		}
		if orientation, colorSpace, found := visualPreviewEXIF(payload); found {
			result.orientation = orientation
			result.unsupportedColor = result.unsupportedColor || colorSpace != 0 && colorSpace != 1
		}
		if metadata {
			reader, ok := newExifReader(payload)
			if !ok {
				return errors.New("malformed EXIF header")
			}
			root, ok := reader.readTypedEntries(reader.u32(4), true, 0x83bb, 0x8649)
			if !ok {
				return errors.New("malformed EXIF IPTC directory")
			}
			if err := oversizedPhotoEXIFTag(root, metadataOffRemedy); err != nil {
				return err
			}
			return inspectIPTC(reader, root)
		}
		return nil
	}
	var exif, packet []byte
	sawPNGEXIF := false
	var iccParts [][]byte
	iccBytes := 0
	finish := func() (photoPackets, error) {
		result.exif, result.xmp = exif, packet
		if len(iccParts) > 0 {
			for _, p := range iccParts {
				if len(p) == 0 {
					return result, errors.New("incomplete ICC profile")
				}
				result.icc = append(result.icc, p...)
			}
		}
		if len(result.icc) > 0 && (len(result.icc) < 132 || len(result.icc) > maxPhotoSidecarBytes || int(binary.BigEndian.Uint32(result.icc)) != len(result.icc) || string(result.icc[36:40]) != "acsp" || !slices.Contains([]string{"RGB ", "GRAY"}, string(result.icc[16:20]))) {
			return result, errors.New("unsupported ICC profile")
		}
		result.grayICC = len(result.icc) > 0 && string(result.icc[16:20]) == "GRAY"
		if len(result.icc) > 0 {
			count := int(binary.BigEndian.Uint32(result.icc[128:]))
			if count < 1 || count > (len(result.icc)-132)/12 {
				return result, errors.New("malformed ICC tag table")
			}
			for i := range count {
				entry := result.icc[132+i*12:]
				offset, size := uint64(binary.BigEndian.Uint32(entry[4:])), uint64(binary.BigEndian.Uint32(entry[8:]))
				if offset < uint64(132+12*count) || size < 4 || offset+size > uint64(len(result.icc)) {
					return result, errors.New("malformed ICC tag bounds")
				}
			}
		}
		return result, nil
	}
	set := func(destination *[]byte, value []byte) error {
		if len(*destination) != 0 || len(value) == 0 || len(value) > maxPhotoSidecarBytes {
			return errors.New("duplicate or oversized photo metadata")
		}
		*destination = value
		return nil
	}
	switch {
	case bytes.HasPrefix(data, []byte{0xff, 0xd8}), bytes.HasPrefix(data, []byte("\x89PNG\r\n\x1a\n")), bytes.HasPrefix(data, []byte("RIFF")):
		format := visualFormatWebP
		switch data[0] {
		case 0xff:
			format = visualFormatJPEG
		case 0x89:
			format = visualFormatPNG
		}
		err = walkVisualContainer(ctx, bytes.NewReader(data), format, int64(len(data)), func(kind string, r io.Reader, n, _ int64) error {
			if format == visualFormatPNG && kind == "cICP" {
				return errors.New("unsupported PNG cICP color profile")
			}
			if format == visualFormatJPEG && kind != "\xe1" && kind != "\xe2" && (kind != "\xed" || !retainKeywords) {
				return nil
			}
			if format == visualFormatPNG && !slices.Contains([]string{"iCCP", "gAMA", "cHRM", "acTL", "eXIf", "iTXt"}, kind) {
				return nil
			}
			if format == visualFormatWebP && !slices.Contains([]string{"VP8X", "ANIM", "ANMF", "ICCP", "EXIF", "XMP "}, kind) {
				return nil
			}
			if kind == "ANIM" || kind == "ANMF" || kind == "acTL" {
				result.animated = true
				return nil
			}
			if !metadata && (kind == "iTXt" || kind == "XMP ") {
				return nil
			}
			if format == visualFormatPNG && (kind == "iCCP" || kind == "iTXt") {
				text := bufio.NewReader(r)
				terminated := func(limit int) (int, error) {
					for count := range limit {
						b, e := text.ReadByte()
						if e != nil {
							return 0, fmt.Errorf("reading PNG text header: %w", e)
						}
						if b == 0 {
							return count + 1, nil
						}
					}
					return 0, errVisualMetadataLimit
				}
				compressed := true
				header := 0
				if kind == "iCCP" {
					length, e := terminated(80)
					method, methodErr := text.ReadByte()
					if e != nil || methodErr != nil || length < 2 || method != 0 || len(result.icc) > 0 {
						return errors.New("malformed PNG ICC profile")
					}
				} else {
					prefix := make([]byte, len(photoXMPPNGKeyword)+1)
					if n < int64(len(prefix)) {
						return nil
					}
					if _, e := io.ReadFull(text, prefix); e != nil {
						return fmt.Errorf("reading PNG text keyword: %w", e)
					}
					if string(prefix) != photoXMPPNGKeyword+"\x00" {
						return nil
					}
					var controls [2]byte
					if _, e := io.ReadFull(text, controls[:]); e != nil || controls[0] > 1 || controls[1] != 0 {
						return errors.New("malformed PNG XMP")
					}
					compressed = controls[0] == 1
					header = len(prefix) + len(controls)
					for range 2 {
						length, e := terminated(maxPhotoSidecarBytes - header)
						if e != nil {
							return e
						}
						header += length
					}
				}
				var value []byte
				var e error
				if compressed {
					value, e = readPhotoCompressedMetadata(text)
				} else if n-int64(header) > maxPhotoSidecarBytes {
					return errVisualMetadataLimit
				} else {
					value, e = io.ReadAll(text)
				}
				if e != nil {
					return e
				}
				if kind == "iCCP" {
					result.icc = value
					return nil
				}
				return set(&packet, value)
			}
			if kind == "gAMA" && n != 4 || kind == "cHRM" && n != 32 {
				result.unsupportedColor = true
				return nil
			}
			if kind == "VP8X" && n != 10 {
				return errors.New("malformed WebP flags")
			}
			limit := int64(maxPhotoSidecarBytes)
			if format == visualFormatWebP && kind == "EXIF" {
				limit += 6
			}
			if n > limit {
				return errVisualMetadataLimit
			}
			payload, e := io.ReadAll(r)
			if e != nil {
				return e
			}
			switch format {
			case visualFormatJPEG:
				marker := kind[0]
				if marker == 0xed {
					if !bytes.HasPrefix(payload, []byte("Photoshop 3.0\x00")) {
						return nil
					}
					keywords, err := photoPhotoshopKeywords(payload)
					if err != nil {
						return err
					}
					return addKeywords(keywords)
				}
				if metadata && marker == 0xe1 && bytes.HasPrefix(payload, []byte("http://ns.adobe.com/xmp/extension/\x00")) {
					return errPhotoExtendedXMP
				}
				if marker == 0xe2 && bytes.HasPrefix(payload, []byte("ICC_PROFILE\x00")) {
					if len(payload) < 14 || payload[12] == 0 || payload[13] == 0 || payload[12] > payload[13] {
						return errors.New("malformed JPEG ICC profile")
					}
					if len(iccParts) == 0 {
						iccParts = make([][]byte, int(payload[13]))
					}
					index := int(payload[12]) - 1
					if len(iccParts) != int(payload[13]) || iccParts[index] != nil {
						return errors.New("duplicate ICC profile segment")
					}
					iccBytes += len(payload) - 14
					if iccBytes > maxPhotoSidecarBytes {
						return errors.New("ICC profile exceeds metadata limit")
					}
					iccParts[index] = payload[14:]
				}
				if marker == 0xe1 && bytes.HasPrefix(payload, []byte("Exif\x00\x00")) {
					if e := inspectEXIF(payload[6:]); e != nil {
						return e
					}
				}
				if !metadata || marker != 0xe1 {
					return nil
				}
				switch {
				case bytes.HasPrefix(payload, []byte("Exif\x00\x00")):
					err = set(&exif, payload[6:])
				case bytes.HasPrefix(payload, []byte(photoXMPJPEGPrefix)):
					err = set(&packet, payload[len(photoXMPJPEGPrefix):])
				}
				return err
			case visualFormatPNG:
				if kind == "gAMA" && (len(payload) != 4 || binary.BigEndian.Uint32(payload) != 45455) {
					result.unsupportedColor = true
				}
				if kind == "cHRM" && !bytes.Equal(payload, []byte{0, 0, 122, 38, 0, 0, 128, 132, 0, 0, 250, 0, 0, 0, 128, 232, 0, 0, 117, 48, 0, 0, 234, 96, 0, 0, 58, 152, 0, 0, 23, 112}) {
					result.unsupportedColor = true
				}
				if kind == "eXIf" {
					firstEXIF := !sawPNGEXIF
					sawPNGEXIF = true
					orientation := result.orientation
					if e := inspectEXIF(payload); e != nil {
						return e
					}
					if !firstEXIF {
						result.orientation = orientation
					}
				}
				if metadata && kind == "eXIf" {
					err = set(&exif, payload)
				}

				if err != nil {
					return err
				}
			case visualFormatWebP:
				switch kind {
				case "VP8X":
					colorFlag, animationFlag := visualPreviewWebPFlags(payload[0])
					result.animated = result.animated || animationFlag
					result.unsupportedColor = result.unsupportedColor || colorFlag
				case "ICCP":
					if len(result.icc) > 0 {
						return errors.New("duplicate ICC profile")
					}
					result.icc = payload
				case "EXIF":
					if e := inspectEXIF(bytes.TrimPrefix(payload, []byte("Exif\x00\x00"))); e != nil {
						return e
					}
					if !metadata {
						break
					}
					err = set(&exif, bytes.TrimPrefix(payload, []byte("Exif\x00\x00")))
				case "XMP ":
					err = set(&packet, payload)
				}
				if err != nil {
					return err
				}
			}
			return err
		})
		if err != nil {
			return result, err
		}
	case exifTIFFSignature(data):
		reader, ok := newExifReader(data)
		if !ok {
			return result, errors.New("invalid RAW EXIF")
		}
		var tags []uint16
		if !metadata {
			tags = []uint16{visualPreviewRAWICCTag}
		}
		root, ok := reader.readTypedEntries(reader.u32(4), true, tags...)
		if !ok {
			return result, errors.New("invalid RAW EXIF directory")
		}
		// IPTC containers matter only while their keywords are kept; otherwise
		// the EXIF writer drops them and their size is irrelevant.
		retainedTags := []uint16{visualPreviewRAWICCTag, 700}
		if retainKeywords {
			retainedTags = append(retainedTags, 0x83bb, 0x8649)
		}
		packetTags := map[uint16]exifEntry{}
		for _, tag := range retainedTags {
			if entry, ok := root[tag]; ok {
				packetTags[tag] = entry
			}
		}
		remedy := ""
		if metadata {
			remedy = metadataOffRemedy
		}
		if err := oversizedPhotoEXIFTag(packetTags, remedy); err != nil {
			return result, err
		}
		if value, ok := root[34675]; ok {
			result.icc = value.value
		}
		if metadata {
			if err := inspectIPTC(reader, root); err != nil {
				return result, err
			}
			if value, ok := root[700]; ok {
				if err := set(&packet, value.value); err != nil {
					return result, err
				}
			}
			exif = data
		}
		return finish()
	case bytes.HasPrefix(data, []byte("GIF8")):
		icc, xmp, err := photoGIFMetadata(data, metadata)
		if err != nil {
			return result, err
		}
		if len(icc) > 0 {
			iccParts = [][]byte{icc}
		}
		if metadata && len(xmp) > 0 {
			if err := set(&packet, xmp); err != nil {
				return result, err
			}
		}
		return finish()
	case isSourceMetadataRAF(data):
		location, found, malformed, err := inspectVisualPreviewRAF(bytes.NewReader(data), int64(len(data)))
		if err != nil {
			return result, err
		}
		if malformed || !found {
			return result, errors.New("invalid RAF metadata")
		}
		packets, err := photoSourcePackets(ctx, data[location.offset:location.offset+location.length], metadata, retainKeywords)
		packets.rawPreview = &location
		return packets, err
	}
	return finish()
}

func readPhotoCompressedMetadata(source io.Reader) ([]byte, error) {
	reader, err := zlib.NewReader(source)
	if err != nil {
		return nil, fmt.Errorf("opening compressed photo metadata: %w", err)
	}
	value, err := io.ReadAll(io.LimitReader(reader, maxPhotoSidecarBytes+1))
	err = errors.Join(err, reader.Close())
	if len(value) > maxPhotoSidecarBytes {
		return nil, errVisualMetadataLimit
	}
	return value, err
}

// Rebuilding only reachable directories removes GPS payloads and stale thumbnails.
func rewritePhotoEXIF(data []byte, width, height int, removeGPS bool, authored store.PhotoAuthored, replaceKeywords bool) ([]byte, error) {
	r, ok := newExifReader(data)
	if !ok || r.format != "tiff" {
		return nil, errors.New("malformed EXIF header")
	}
	out := append([]byte(nil), data[:8]...)
	r.order.PutUint32(out[4:], 8)
	seen := map[uint32]bool{}
	var write func(uint32, int) (uint32, error)
	write = func(offset uint32, depth int) (uint32, error) {
		if depth > 4 || seen[offset] {
			return 0, errors.New("cyclic EXIF directory")
		}
		seen[offset] = true
		entries, ok := r.readTypedEntries(offset, true)
		if !ok {
			return 0, errors.New("malformed EXIF directory")
		}
		for tag, entry := range entries {
			if slices.Contains(photoEXIFSourceLayoutTags, tag) || photoEXIFRAWCalibrationTag(tag) || slices.Contains(photoEXIFEmbeddedMetadataTags, tag) || removeGPS && tag == 0x8825 {
				delete(entries, tag)
				continue
			}
			if slices.Contains(photoEXIFRatingTags, tag) && authored.Confirmed&store.PhotoConfirmedRating != 0 || tag == 0x9c9e && replaceKeywords || slices.Contains(photoEXIFCaptionTags, tag) && authored.Confirmed&store.PhotoConfirmedCaption != 0 || slices.Contains(photoEXIFCreatorTags, tag) && authored.Confirmed&store.PhotoConfirmedCreator != 0 || tag == 0x8298 && authored.Confirmed&store.PhotoConfirmedCopyright != 0 {
				delete(entries, tag)
				continue
			}
			if entry.oversized {
				return 0, oversizedPhotoEXIFTag(map[uint16]exifEntry{tag: entry}, metadataOffRemedy)
			}
			if len(entry.value) == 0 {
				delete(entries, tag)
				continue
			}
		}
		long := func(n int) exifEntry {
			b := make([]byte, 4)
			r.order.PutUint32(b, uint32(n&0x7fffffff))
			return exifEntry{kind: 4, value: b}
		}
		short := func() exifEntry {
			b := make([]byte, 2)
			r.order.PutUint16(b, 1)
			return exifEntry{kind: 3, value: b}
		}
		if depth == 0 {
			entries[0x0100], entries[0x0101], entries[0x0112] = long(width), long(height), short()
		}
		if _, exists := entries[0x0112]; exists {
			entries[0x0112] = short()
		}
		if _, exists := entries[0xa002]; exists {
			entries[0xa002] = long(width)
		}
		if _, exists := entries[0xa003]; exists {
			entries[0xa003] = long(height)
		}
		tags := make([]uint16, 0, len(entries))
		for tag := range entries {
			tags = append(tags, tag)
		}
		slices.Sort(tags)
		start := uint32(uint64(len(out)) & 0xffffffff)
		out = append(out, make([]byte, 2+12*len(tags)+4)...)
		r.order.PutUint16(out[start:], uint16(len(tags)&0xffff))
		for index, tag := range tags {
			e := entries[tag]
			if slices.Contains([]uint16{0x8769, 0x8825, 0xa005}, tag) {
				if e.kind != 4 || len(e.value) != 4 {
					return 0, errors.New("invalid EXIF directory pointer")
				}
				child, err := write(r.order.Uint32(e.value), depth+1)
				if err != nil {
					return 0, err
				}
				e = long(int(child))
			}
			base := int(start) + 2 + index*12
			r.order.PutUint16(out[base:], tag)
			r.order.PutUint16(out[base+2:], e.kind)
			size := int(exifTypeWidths[e.kind])
			if size == 0 || len(e.value)%size != 0 {
				return 0, errors.New("unsupported EXIF value")
			}
			r.order.PutUint32(out[base+4:], uint32((len(e.value)/size)&0x7fffffff))
			if len(e.value) <= 4 {
				copy(out[base+8:base+12], e.value)
			} else {
				r.order.PutUint32(out[base+8:], uint32(uint64(len(out))&0xffffffff))
				out = append(out, e.value...)
				if len(out)%2 != 0 {
					out = append(out, 0)
				}
			}
			if len(out) > maxPhotoSidecarBytes {
				return 0, bundle.ErrLimit
			}
		}
		return start, nil
	}
	_, err := write(r.u32(4), 0)
	return out, err
}

type photoXMPFrame struct {
	original, qualified xml.Name
	namespaces          map[string]string
}

type photoXMPEncoder struct {
	encoder *xml.Encoder
	frames  []photoXMPFrame
}

func validatePhotoXMPText(value string) error {
	if !utf8.ValidString(value) {
		return errors.New("XMP text contains invalid UTF-8")
	}
	for _, r := range value {
		if r == '\t' || r == '\n' || r == '\r' || r >= 0x20 && r <= 0xd7ff || r >= 0xe000 && r <= 0xfffd || r >= 0x10000 && r <= 0x10ffff {
			continue
		}
		return fmt.Errorf("XMP text contains character U+%04X that XML cannot represent", r)
	}
	return nil
}

func (w *photoXMPEncoder) EncodeToken(token xml.Token) error {
	switch t := token.(type) {
	case xml.CharData:
		if err := validatePhotoXMPText(string(t)); err != nil {
			return err
		}
	case xml.StartElement:
		namespaces := map[string]string{"xml": xmlNamespace}
		if len(w.frames) > 0 {
			namespaces = maps.Clone(w.frames[len(w.frames)-1].namespaces)
		}
		for _, attr := range t.Attr {
			if err := validatePhotoXMPText(attr.Value); err != nil {
				return err
			}
			if attr.Name.Space == xmlNamespacePrefix {
				namespaces[attr.Name.Local] = attr.Value
			} else if attr.Name.Space == "" && attr.Name.Local == xmlNamespacePrefix {
				namespaces[""] = attr.Value
			}
		}
		attrs := make([]xml.Attr, 0, len(t.Attr))
		qualify := func(name xml.Name, attribute bool) xml.Name {
			if name.Space == "" {
				return name
			}
			if !attribute && namespaces[""] == name.Space {
				return xml.Name{Local: name.Local}
			}
			for _, prefix := range slices.Sorted(maps.Keys(namespaces)) {
				if prefix != "" && namespaces[prefix] == name.Space {
					return xml.Name{Local: prefix + ":" + name.Local}
				}
			}
			prefix := "ns" + strconv.Itoa(len(namespaces))
			for namespaces[prefix] != "" {
				prefix += "_"
			}
			namespaces[prefix] = name.Space
			attrs = append(attrs, xml.Attr{Name: xml.Name{Local: "xmlns:" + prefix}, Value: name.Space})
			return xml.Name{Local: prefix + ":" + name.Local}
		}
		original := t.Name
		t.Name = qualify(t.Name, false)
		for _, attr := range t.Attr {
			if attr.Name.Space == xmlNamespacePrefix {
				attr.Name = xml.Name{Local: "xmlns:" + attr.Name.Local}
			} else {
				attr.Name = qualify(attr.Name, true)
			}
			attrs = append(attrs, attr)
		}
		t.Attr = attrs
		w.frames = append(w.frames, photoXMPFrame{original, t.Name, namespaces})
		token = t
	case xml.EndElement:
		if len(w.frames) == 0 || w.frames[len(w.frames)-1].original != t.Name {
			return errors.New("unbalanced XMP element")
		}
		t.Name = w.frames[len(w.frames)-1].qualified
		w.frames = w.frames[:len(w.frames)-1]
		token = t
	}
	if err := w.encoder.EncodeToken(token); err != nil {
		return fmt.Errorf("encoding XMP token: %w", err)
	}
	return nil
}

func (w *photoXMPEncoder) EncodeElement(value string, start xml.StartElement) error {
	for _, token := range []xml.Token{start, xml.CharData(value), start.End()} {
		if err := w.EncodeToken(token); err != nil {
			return err
		}
	}
	return nil
}

func mergePhotoXMP(ctx context.Context, packet []byte, input store.PhotoExportInput, receipt bundle.PhotoRenderReceipt, sourceOrientation int, embedded []string) ([]byte, error) {
	if len(packet) == 0 && input.Authored.Confirmed == 0 && len(input.Keywords) == 0 && len(embedded) == 0 {
		return nil, nil
	}
	if len(packet) == 0 {
		packet = []byte(`<x:xmpmeta xmlns:x="adobe:ns:meta/"><rdf:RDF xmlns:rdf="http://www.w3.org/1999/02/22-rdf-syntax-ns#"></rdf:RDF></x:xmpmeta>`)
	}
	if len(packet) > maxPhotoSidecarBytes {
		return nil, bundle.ErrLimit
	}
	unionKeywords := len(input.Keywords) == 0 && len(embedded) > 0
	var subjects []string
	var subjectText strings.Builder
	subjectDepth := 0
	subjectCollection := false
	plainSubject := xml.Name{Space: xmpDublinCoreNamespace, Local: "subject"}
	plainSubjectAttrs := func(attrs []xml.Attr) bool {
		for _, attr := range attrs {
			if attr.Name.Space != xmlNamespacePrefix && attr.Name != (xml.Name{Local: xmlNamespacePrefix}) {
				return false
			}
		}
		return true
	}
	collectSubject := func() {
		if subjectText.Len() > 0 {
			subjects = append(subjects, subjectText.String())
		}
		subjectText.Reset()
	}
	decoder := newPhotoXMLDecoder(packet)
	var out bytes.Buffer
	encoder := &photoXMPEncoder{encoder: xml.NewEncoder(&out)}
	ratingConfirmed := input.Authored.Confirmed&store.PhotoConfirmedRating != 0
	flagConfirmed := input.Authored.Confirmed&store.PhotoConfirmedFlag != 0
	embeddedReject := false
	saveFlag := func(value string) {
		if !embeddedReject {
			input.Authored.Flag = strings.TrimSpace(value)
		}
		input.Authored.Confirmed |= store.PhotoConfirmedFlag
	}
	legacyReject := func(value string) bool {
		flag, rating, valid := photoXMPFlag(value, input.Authored.Flag)
		if !valid || rating != -1 {
			return false
		}
		if ratingConfirmed && !flagConfirmed {
			embeddedReject = true
			input.Authored.Flag = flag
			input.Authored.Confirmed |= store.PhotoConfirmedFlag
		}
		return true
	}
	isRating := func(n xml.Name) bool { return n.Space == xmpBasicNamespace && n.Local == "Rating" }
	sourceFlag := func(n xml.Name) bool {
		return ratingConfirmed && !flagConfirmed && n.Space == teststripXMPNamespace && n.Local == "Pick"
	}
	extendedXMP := func(n xml.Name) bool {
		return n.Space == "http://ns.adobe.com/xmp/note/" && n.Local == "HasExtendedXMP"
	}
	remove := func(n xml.Name, photoProperty bool) bool {
		if n.Space == xmlNamespacePrefix || n.Space == "" && n.Local == xmlNamespacePrefix {
			return false
		}
		if receipt.Profile.RemoveGPS && strings.HasPrefix(strings.ToUpper(n.Local), "GPS") {
			return true
		}
		if !photoProperty {
			return false
		}
		if n.Space == xmpBasicNamespace && n.Local == "Thumbnails" || n.Space == "http://ns.adobe.com/camera-raw-settings/1.0/" && (n.Local == "HasCrop" || strings.HasPrefix(n.Local, "Crop")) ||
			n.Space == "http://ns.google.com/photos/1.0/container/" || n.Space == "http://ns.google.com/photos/1.0/container/item/" || n.Space == "http://ns.adobe.com/hdr-gain-map/1.0/" {
			return true
		}
		return photoXMPConfirmed(n, input)
	}
	depth, skip, roots, rdf := 0, 0, 0, 0
	rdfDepth := 2
	// The first top-level description names the photo, as readPhotoSidecar does.
	subject := photoXMPSubject{kind: "about"}
	subjectSeen, photoDescription := false, false
	var elements []xml.Name
	// Qualified properties carry their scalar in rdf:value. Other descendants
	// describe that value and must not contribute text to it.
	scalarText := func(propertyDepth int) bool {
		if depth == propertyDepth {
			return true
		}
		if elements[depth-1] != (xml.Name{Space: rdfNamespace, Local: "value"}) {
			return false
		}
		return depth == propertyDepth+1 || depth == propertyDepth+2 &&
			elements[propertyDepth] == (xml.Name{Space: rdfNamespace, Local: "Description"})
	}
	var ratingTokens []xml.Token
	var ratingValue strings.Builder
	ratingDepth := 0
	flagTokens := false
	emit := func(token xml.Token) error {
		if ratingTokens == nil {
			return encoder.EncodeToken(token)
		}
		ratingTokens = append(ratingTokens, xml.CopyToken(token))
		if text, ok := token.(xml.CharData); ok && scalarText(ratingDepth) {
			ratingValue.Write(text)
		}
		if _, end := token.(xml.EndElement); !end || depth != ratingDepth-1 {
			return nil
		}
		if flagTokens {
			saveFlag(ratingValue.String())
		}
		reject := !flagTokens && legacyReject(ratingValue.String())
		if !flagTokens && !ratingConfirmed && (!flagConfirmed || !reject) {
			for _, token := range ratingTokens {
				if err := encoder.EncodeToken(token); err != nil {
					return err
				}
			}
		}
		ratingTokens = nil
		ratingValue.Reset()
		return nil
	}
	var orientationText strings.Builder
	orientationDepth := 0
	checkOrientation := func(value string) error {
		orientation, err := strconv.Atoi(strings.TrimSpace(value))
		if err == nil && orientation >= 2 && orientation <= 8 && orientation != sourceOrientation {
			return fmt.Errorf("%w: XMP %d differs from source %d", errPhotoXMPOrientation, orientation, sourceOrientation)
		}
		return nil
	}
	normalized := func(name xml.Name, photoProperty bool) string {
		if !photoProperty {
			return ""
		}
		if name.Space != "http://ns.adobe.com/tiff/1.0/" && name.Space != "http://ns.adobe.com/exif/1.0/" {
			return ""
		}
		switch name.Local {
		case "Orientation":
			return "1"
		case "PixelXDimension", "ImageWidth":
			return strconv.Itoa(receipt.Width)
		case "PixelYDimension", "ImageLength":
			return strconv.Itoa(receipt.Height)
		}
		return ""
	}
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		token, err := decoder.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("decoding photo XMP: %w", err)
		}
		if subjectDepth > 0 {
			switch t := token.(type) {
			case xml.Directive:
				return nil, errors.New("XMP directives are unsupported")
			case xml.StartElement:
				if !plainSubjectAttrs(t.Attr) {
					return nil, errPhotoXMPKeywords
				}
				switch {
				case depth == subjectDepth && !subjectCollection && t.Name == (xml.Name{Space: rdfNamespace, Local: "Bag"}) && strings.TrimSpace(subjectText.String()) == "":
					subjectCollection = true
					subjectText.Reset()
				case depth == subjectDepth+1 && t.Name == (xml.Name{Space: rdfNamespace, Local: "li"}):
				default:
					return nil, errPhotoXMPKeywords
				}
				depth++
				elements = append(elements, t.Name)
				if depth > maxSourceMetadataXMLDepth {
					return nil, bundle.ErrLimit
				}
			case xml.CharData:
				if !subjectCollection && depth == subjectDepth || elements[len(elements)-1] == (xml.Name{Space: rdfNamespace, Local: "li"}) {
					subjectText.Write(t)
				} else if strings.TrimSpace(string(t)) != "" {
					return nil, errPhotoXMPKeywords
				}
			case xml.EndElement:
				if t.Name == (xml.Name{Space: rdfNamespace, Local: "li"}) || depth == subjectDepth {
					collectSubject()
				}
				if depth == subjectDepth {
					subjectDepth = 0
				}
				depth--
				elements = elements[:len(elements)-1]
			}
			continue
		}
		switch t := token.(type) {
		case xml.Directive:
			return nil, errors.New("XMP directives are unsupported")
		case xml.StartElement:
			if extendedXMP(t.Name) {
				return nil, errPhotoExtendedXMP
			}
			elements = append(elements, t.Name)
			depth++
			if depth > maxSourceMetadataXMLDepth {
				return nil, bundle.ErrLimit
			}
			if depth == 1 {
				roots++
				if t.Name == (xml.Name{Space: rdfNamespace, Local: "RDF"}) {
					rdfDepth = 1
				}
				if roots > 1 || !isXMPRoot(t.Name) && rdfDepth != 1 {
					return nil, errors.New("invalid XMP root")
				}
			}
			if skip > 0 {
				skip++
				continue
			}
			if depth == rdfDepth+1 && elements[rdfDepth-1] == (xml.Name{Space: rdfNamespace, Local: "RDF"}) && t.Name == (xml.Name{Space: rdfNamespace, Local: "Description"}) {
				current, err := readPhotoXMPSubject(t.Attr)
				if err != nil {
					return nil, err
				}
				if !subjectSeen {
					subject, subjectSeen = current, true
				}
				photoDescription = current == subject
			}
			photoObject := photoDescription && depth >= rdfDepth+1 && elements[rdfDepth-1] == (xml.Name{Space: rdfNamespace, Local: "RDF"}) && elements[rdfDepth] == (xml.Name{Space: rdfNamespace, Local: "Description"})
			photoProperty, photoAttributes := photoObject && depth == rdfDepth+2, photoObject && depth == rdfDepth+1
			if photoProperty && unionKeywords && t.Name == plainSubject {
				if !plainSubjectAttrs(t.Attr) {
					return nil, errPhotoXMPKeywords
				}
				subjectDepth, subjectCollection = depth, false
				continue
			}
			if remove(t.Name, photoProperty) && !isRating(t.Name) && !sourceFlag(t.Name) {
				skip = 1
				continue
			}
			if photoProperty && ((ratingConfirmed || flagConfirmed) && isRating(t.Name) || sourceFlag(t.Name)) && ratingTokens == nil {
				ratingTokens = []xml.Token{}
				ratingDepth = depth
				flagTokens = sourceFlag(t.Name)
			}
			attrs := []xml.Attr{}
			for _, a := range t.Attr {
				if extendedXMP(a.Name) {
					return nil, errPhotoExtendedXMP
				}
				if photoAttributes && unionKeywords && a.Name == plainSubject {
					subjects = append(subjects, a.Value)
					continue
				}
				if photoAttributes && sourceFlag(a.Name) {
					saveFlag(a.Value)
					continue
				}
				reject := photoAttributes && isRating(a.Name) && legacyReject(a.Value)
				if remove(a.Name, photoAttributes) || flagConfirmed && reject {
					continue
				}
				if value := normalized(a.Name, photoAttributes); value != "" {
					if a.Name.Local == "Orientation" {
						if err := checkOrientation(a.Value); err != nil {
							return nil, err
						}
					}
					a.Value = value
				}
				attrs = append(attrs, a)
			}
			t.Attr = attrs
			if value := normalized(t.Name, photoProperty); value != "" {
				// The replacement is a plain RDF literal, so the old value's
				// resource attributes and qualifiers no longer describe it.
				t.Attr = slices.DeleteFunc(t.Attr, func(attr xml.Attr) bool {
					return attr.Name.Space != xmlNamespacePrefix && attr.Name != (xml.Name{Local: xmlNamespacePrefix})
				})
				if t.Name.Local == "Orientation" {
					orientationDepth = depth
					orientationText.Reset()
				}
				for _, token := range []xml.Token{t, xml.CharData(value), t.End()} {
					if err := emit(token); err != nil {
						return nil, err
					}
				}
				skip = 1
				continue
			}
			token = t
		case xml.EndElement:
			if orientationDepth == depth {
				if err := checkOrientation(orientationText.String()); err != nil {
					return nil, err
				}
				orientationDepth = 0
			}
			elements = elements[:len(elements)-1]
			depth--
			if skip > 0 {
				skip--
				continue
			}
			if t.Name.Space == rdfNamespace && t.Name.Local == "RDF" {
				rdf++
				if rdf > 1 {
					return nil, errors.New("multiple XMP RDF roots")
				}
				if unionKeywords {
					seen := map[string]bool{}
					for _, value := range append(subjects, embedded...) {
						if !seen[value] {
							seen[value] = true
							input.Keywords = append(input.Keywords, value)
						}
					}
				}
				if err := writePhotoAuthoredXMP(encoder, input, subject); err != nil {
					return nil, err
				}
			}
		case xml.CharData:
			if orientationDepth > 0 && scalarText(orientationDepth) {
				orientationText.Write(t)
			}
			if skip > 0 {
				continue
			}
			if depth == 0 && strings.TrimSpace(string(t)) != "" {
				return nil, errors.New("text outside XMP root")
			}
			if strings.Trim(string(t), " \t\r\n") == "" && (depth == 0 ||
				depth == 1 && isXMPRoot(elements[0]) ||
				depth == rdfDepth && elements[rdfDepth-1] == (xml.Name{Space: rdfNamespace, Local: "RDF"}) ||
				depth == rdfDepth+1 && elements[rdfDepth-1] == (xml.Name{Space: rdfNamespace, Local: "RDF"}) && elements[rdfDepth] == (xml.Name{Space: rdfNamespace, Local: "Description"})) {
				continue
			}
		default:
			if skip > 0 {
				continue
			}
		}
		if err := emit(token); err != nil {
			return nil, err
		}
	}
	if roots != 1 || rdf != 1 || depth != 0 {
		return nil, errors.New("incomplete XMP packet")
	}
	if err := encoder.encoder.Flush(); err != nil {
		return nil, fmt.Errorf("flushing photo XMP: %w", err)
	}
	if out.Len() > maxPhotoSidecarBytes {
		return nil, bundle.ErrLimit
	}
	return out.Bytes(), nil
}

type photoXMPSubject struct {
	kind, value string
}

func readPhotoXMPSubject(attrs []xml.Attr) (photoXMPSubject, error) {
	subject := photoXMPSubject{kind: "about"}
	for _, attr := range attrs {
		if attr.Name.Space != rdfNamespace {
			continue
		}
		switch attr.Name.Local {
		case "about":
			if subject.kind == "about" {
				subject.value = attr.Value
			}
		case "nodeID":
			subject = photoXMPSubject{kind: "nodeID", value: attr.Value}
		case "ID":
			// A packet cannot repeat an rdf:ID, so authored values could not join it.
			return subject, errors.New("XMP rdf:ID subjects are unsupported; turn image metadata off")
		}
	}
	return subject, nil
}

func writePhotoAuthoredXMP(encoder *photoXMPEncoder, input store.PhotoExportInput, subject photoXMPSubject) error {
	if input.Authored.Confirmed == 0 && len(input.Keywords) == 0 {
		return nil
	}
	description := xml.StartElement{Name: xml.Name{Space: rdfNamespace, Local: "Description"}, Attr: []xml.Attr{{Name: xml.Name{Space: rdfNamespace, Local: subject.kind}, Value: subject.value}}}
	if err := encoder.EncodeToken(description); err != nil {
		return err
	}
	write := func(space, name, value string, collection string) error {
		e := xml.StartElement{Name: xml.Name{Space: space, Local: name}}
		if err := encoder.EncodeToken(e); err != nil {
			return err
		}
		if collection != "" {
			c := xml.StartElement{Name: xml.Name{Space: rdfNamespace, Local: collection}}
			if err := encoder.EncodeToken(c); err != nil {
				return err
			}
			values := []string{value}
			if name == "subject" {
				values = input.Keywords
			}
			for _, v := range values {
				li := xml.StartElement{Name: xml.Name{Space: rdfNamespace, Local: "li"}}
				if collection == "Alt" {
					li.Attr = []xml.Attr{{Name: xml.Name{Space: xmlNamespace, Local: "lang"}, Value: "x-default"}}
				}
				if err := encoder.EncodeElement(v, li); err != nil {
					return err
				}
			}
			if err := encoder.EncodeToken(c.End()); err != nil {
				return err
			}
		} else if err := encoder.EncodeToken(xml.CharData(value)); err != nil {
			return err
		}
		return encoder.EncodeToken(e.End())
	}
	v := input.Authored
	for _, p := range [][4]string{{xmpBasicNamespace, "Rating", strconv.Itoa(v.Rating), ""}, {xmpBasicNamespace, "Label", v.Label, ""}, {teststripXMPNamespace, "Pick", v.Flag, ""}, {teststripXMPNamespace, "Rotation", "0", ""}, {xmpDublinCoreNamespace, "description", v.Caption, "Alt"}, {xmpDublinCoreNamespace, sourceMetadataCreatorField, v.Creator, "Seq"}, {xmpDublinCoreNamespace, "rights", v.Copyright, "Alt"}, {xmpDublinCoreNamespace, "subject", "", "Bag"}} {
		if !photoXMPConfirmed(xml.Name{Space: p[0], Local: p[1]}, input) {
			continue
		}
		if err := write(p[0], p[1], p[2], p[3]); err != nil {
			return err
		}
	}
	return encoder.EncodeToken(description.End())
}

func photoXMPExportField(n xml.Name) string {
	if strings.TrimSuffix(n.Space, "/") == microsoftPhotoXMPNamespace && n.Local == "Rating" {
		return "Rating"
	}
	if n.Space == "http://ns.adobe.com/tiff/1.0/" {
		switch n.Local {
		case "ImageDescription":
			return "description"
		case "Artist":
			return sourceMetadataCreatorField
		case "Copyright":
			return "rights"
		}
	}
	return photoXMPField(n)
}

func photoXMPKeywords(n xml.Name) bool {
	if strings.TrimSuffix(n.Space, "/") == microsoftPhotoXMPNamespace {
		return n.Local == "LastKeywordXMP" || n.Local == "LastKeywordIPTC" || n.Local == "LastKeywordIPTC_TIFF_IRB"
	}
	return n.Space == xmpDublinCoreNamespace && n.Local == "subject" || n.Space == "http://ns.adobe.com/lightroom/1.0/" && n.Local == "hierarchicalSubject" || n.Space == xmpPDFNamespace && n.Local == "Keywords"
}

func photoXMPConfirmed(n xml.Name, input store.PhotoExportInput) bool {
	if photoXMPKeywords(n) {
		return len(input.Keywords) > 0
	}
	bit := map[string]store.PhotoAuthoredFields{
		"Rating":                   store.PhotoConfirmedRating,
		"Label":                    store.PhotoConfirmedLabel,
		"Pick":                     store.PhotoConfirmedFlag,
		"Rotation":                 store.PhotoConfirmedRotation,
		"description":              store.PhotoConfirmedCaption,
		sourceMetadataCreatorField: store.PhotoConfirmedCreator,
		"rights":                   store.PhotoConfirmedCopyright,
	}[photoXMPExportField(n)]
	return bit != 0 && input.Authored.Confirmed&bit != 0
}

const metadataOffRemedy = "; turn image metadata off to export this photo"

// oversizedPhotoEXIFTag refuses a retained value the export did not read.
// Values the export strips never reach this check.
func oversizedPhotoEXIFTag(entries map[uint16]exifEntry, remedy string) error {
	var tags []uint16
	for tag, entry := range entries {
		if entry.oversized {
			tags = append(tags, tag)
		}
	}
	if len(tags) == 0 {
		return nil
	}
	return fmt.Errorf("%w: EXIF tag 0x%04x exceeds 1 MiB%s", bundle.ErrLimit, slices.Min(tags), remedy)
}

// gifXMPTrailer is the XMP-in-GIF magic trailer: 0x01, then 0xFF down to 0x00,
// then the block terminator. It lets GIF readers skip the raw packet.
var gifXMPTrailer = func() []byte {
	trailer := []byte{1}
	for value := 255; value >= 0; value-- {
		trailer = append(trailer, byte(value))
	}
	return append(trailer, 0)
}()

// photoGIFMetadata returns the ICC profile and, when keepXMP is set, the XMP
// packet carried in GIF application extensions, bounded like other photo
// metadata. A discarded XMP packet is skipped without size limits. Damage outside
// those extensions ends the walk, because the decoder reads only the first
// frame and keeps such files exportable.
func photoGIFMetadata(data []byte, keepXMP bool) (icc, xmp []byte, err error) {
	malformed := errors.New("malformed GIF block structure")
	if len(data) < 13 {
		return nil, nil, malformed
	}
	offset := 13
	if data[10]&0x80 != 0 {
		offset += 3 << (data[10]&0x07 + 1)
	}
	subBlocks := func(collect *[]byte) error {
		for offset < len(data) {
			size := int(data[offset])
			offset++
			if size == 0 {
				return nil
			}
			if size > len(data)-offset {
				return malformed
			}
			if collect != nil {
				if len(*collect)+size > maxPhotoSidecarBytes {
					return errVisualMetadataLimit
				}
				*collect = append(*collect, data[offset:offset+size]...)
			}
			offset += size
		}
		return malformed
	}
	for offset < len(data) {
		switch data[offset] {
		case 0x3b:
			return icc, xmp, nil
		case 0x2c:
			if offset+10 > len(data) {
				return icc, xmp, nil
			}
			packed := data[offset+9]
			offset += 10
			if packed&0x80 != 0 {
				offset += 3 << (packed&0x07 + 1)
			}
			offset++
			if subBlocks(nil) != nil {
				return icc, xmp, nil //nolint:nilerr // Damage outside metadata ends scanning; the decoder validates the first frame.
			}
		case 0x21:
			if offset+2 > len(data) {
				return icc, xmp, nil
			}
			application := data[offset+1] == 0xff
			offset += 2
			identifier := ""
			if application && offset+12 <= len(data) && data[offset] == 11 {
				identifier = string(data[offset+1 : offset+12])
			}
			switch identifier {
			case "ICCRGBG1012":
				if icc != nil {
					return nil, nil, errors.New("duplicate GIF ICC profile")
				}
				offset += 12
				icc = []byte{}
				if err := subBlocks(&icc); err != nil {
					return nil, nil, err
				}
			case "XMP DataXMP":
				offset += 12
				end := bytes.Index(data[offset:], gifXMPTrailer)
				if !keepXMP {
					if end < 0 {
						return icc, nil, nil
					}
					offset += end + len(gifXMPTrailer)
					continue
				}
				if xmp != nil {
					return nil, nil, errors.New("duplicate GIF XMP packet")
				}
				if end < 0 {
					return nil, nil, malformed
				}
				if end > maxPhotoSidecarBytes {
					return nil, nil, errVisualMetadataLimit
				}
				xmp = data[offset : offset+end]
				offset += end + len(gifXMPTrailer)
			default:
				if subBlocks(nil) != nil {
					return icc, xmp, nil //nolint:nilerr // Unrelated extensions do not invalidate metadata already read.
				}
			}
		default:
			return icc, xmp, nil
		}
	}
	return icc, xmp, nil
}
