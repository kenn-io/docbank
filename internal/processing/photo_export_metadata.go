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

	"go.kenn.io/docbank/document/bundle"
	"go.kenn.io/docbank/internal/store"
)

const photoXMPJPEGPrefix = "http://ns.adobe.com/xap/1.0/\x00"
const photoXMPPNGKeyword = "XML:com.adobe.xmp"

type photoPackets struct {
	exif, xmp, icc   []byte
	rawPreview       *visualPreviewRAWLocation
	unsupportedColor bool
	orientation      int
	animated         bool
}

func photoExportMetadata(ctx context.Context, packets photoPackets, input store.PhotoExportInput, receipt bundle.PhotoRenderReceipt, pixels []byte) ([]byte, error) {
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
		packet, err = mergePhotoXMP(ctx, packet, input, receipt)
		if err != nil {
			return nil, err
		}
	}
	var out bytes.Buffer
	if receipt.Profile.Format == visualFormatJPEG {
		out.Write(pixels[:2])
		for _, payload := range [][]byte{append([]byte("Exif\x00\x00"), exif...), append([]byte(photoXMPJPEGPrefix), packet...)} {
			if !receipt.Profile.IncludeMetadata || bytes.HasPrefix(payload, []byte("Exif")) && len(exif) == 0 {
				continue
			}
			if len(payload) > 65533 {
				return nil, errors.New("export metadata exceeds JPEG APP1 limit")
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
		if receipt.Profile.IncludeMetadata {
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

// photoSourcePackets bounds metadata independently of the source's pixel bytes.
func photoSourcePackets(ctx context.Context, data []byte, metadata bool) (result photoPackets, err error) {
	result.orientation = 1
	inspectEXIF := func(payload []byte) error {
		if len(payload) > visualPreviewMaxEXIFBytes {
			return errVisualMetadataLimit
		}
		if orientation, colorSpace, found := visualPreviewEXIF(payload); found {
			result.orientation = orientation
			result.unsupportedColor = result.unsupportedColor || colorSpace != 0 && colorSpace != 1
		}
		return nil
	}
	var exif, packet []byte
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
		if len(result.icc) > 0 && (len(result.icc) < 132 || len(result.icc) > maxPhotoSidecarBytes || int(binary.BigEndian.Uint32(result.icc)) != len(result.icc) || string(result.icc[36:40]) != "acsp" || string(result.icc[16:20]) != "RGB ") {
			return result, errors.New("unsupported ICC profile")
		}
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
		err = walkPhotoContainer(ctx, bytes.NewReader(data), format, int64(len(data)), func(kind string, r io.Reader, n int64) error {
			if format == visualFormatJPEG && kind != "\xe1" && kind != "\xe2" {
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
					if e := inspectEXIF(payload); e != nil {
						return e
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
		root, ok := reader.readTypedEntries(reader.u32(4), true)
		if !ok {
			return result, errors.New("invalid RAW EXIF directory")
		}
		if value, ok := root[34675]; ok {
			result.icc = value.value
		}
		if metadata {
			if value, ok := root[700]; ok {
				if err := set(&packet, value.value); err != nil {
					return result, err
				}
			}
			exif = data
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
		packets, err := photoSourcePackets(ctx, data[location.offset:location.offset+location.length], metadata)
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
			// Pixel layout and RAW calibration describe the source, not the delivered pixels.
			if slices.Contains([]uint16{0x0102, 0x0103, 0x0106, 0x010a, 0x0111, 0x0115, 0x0116, 0x0117, 0x011c, 0x013d, 0x0144, 0x0145, 0x014a, 0x0153, 0x0201, 0x0202, 700, 0x83bb, 0x8773, 0x828d, 0x828e, 0x927c, 0xc612, 0xc613, 0xc616, 0xc617, 0xc618, 0xc619, 0xc61a, 0xc61b, 0xc61c, 0xc61d, 0xc61e, 0xc61f, 0xc620, 0xc634, 0xc68d, 0xc68e, 0xc740, 0xc741, 0xc74e}, tag) || removeGPS && tag == 0x8825 {
				delete(entries, tag)
				continue
			}
			if slices.Contains([]uint16{0x4746, 0x4749}, tag) && authored.Confirmed&store.PhotoConfirmedRating != 0 || tag == 0x9c9e && replaceKeywords || slices.Contains([]uint16{0x010e, 0x9c9b, 0x9c9c, 0x9c9f}, tag) && authored.Confirmed&store.PhotoConfirmedCaption != 0 || slices.Contains([]uint16{0x013b, 0x9c9d}, tag) && authored.Confirmed&store.PhotoConfirmedCreator != 0 || tag == 0x8298 && authored.Confirmed&store.PhotoConfirmedCopyright != 0 {
				delete(entries, tag)
				continue
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
			size := map[uint16]int{1: 1, 2: 1, 3: 2, 4: 4, 5: 8, 6: 1, 7: 1, 8: 2, 9: 4, 10: 8, 11: 4, 12: 8, 13: 4}[e.kind]
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

func (w *photoXMPEncoder) EncodeToken(token xml.Token) error {
	switch t := token.(type) {
	case xml.StartElement:
		namespaces := map[string]string{"xml": xmlNamespace}
		if len(w.frames) > 0 {
			namespaces = maps.Clone(w.frames[len(w.frames)-1].namespaces)
		}
		for _, attr := range t.Attr {
			if attr.Name.Space == "xmlns" {
				namespaces[attr.Name.Local] = attr.Value
			} else if attr.Name.Space == "" && attr.Name.Local == "xmlns" {
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
			if attr.Name.Space == "xmlns" {
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

func mergePhotoXMP(ctx context.Context, packet []byte, input store.PhotoExportInput, receipt bundle.PhotoRenderReceipt) ([]byte, error) {
	if len(packet) == 0 {
		packet = []byte(`<x:xmpmeta xmlns:x="adobe:ns:meta/"><rdf:RDF xmlns:rdf="http://www.w3.org/1999/02/22-rdf-syntax-ns#"></rdf:RDF></x:xmpmeta>`)
	}
	if len(packet) > maxPhotoSidecarBytes {
		return nil, bundle.ErrLimit
	}
	decoder := xml.NewDecoder(bytes.NewReader(bytes.TrimPrefix(packet, []byte{0xef, 0xbb, 0xbf})))
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
		rating, err := strconv.Atoi(strings.TrimSpace(value))
		if err != nil || rating != -1 {
			return false
		}
		if ratingConfirmed && !flagConfirmed {
			embeddedReject = true
			input.Authored.Flag = "reject"
			input.Authored.Confirmed |= store.PhotoConfirmedFlag
		}
		return true
	}
	isRating := func(n xml.Name) bool { return n.Space == xmpBasicNamespace && n.Local == "Rating" }
	sourceFlag := func(n xml.Name) bool {
		return ratingConfirmed && !flagConfirmed && n.Space == teststripXMPNamespace && n.Local == "Pick"
	}
	remove := func(n xml.Name) bool {
		if n.Space == "xmlns" || n.Space == "" && n.Local == "xmlns" {
			return false
		}
		return photoXMPConfirmed(n, input) || receipt.Profile.RemoveGPS && strings.HasPrefix(strings.ToUpper(n.Local), "GPS")
	}
	depth, skip, roots, rdf := 0, 0, 0, 0
	var ratingTokens []xml.Token
	var ratingValue strings.Builder
	ratingDepth := 0
	flagTokens := false
	emit := func(token xml.Token) error {
		if ratingTokens == nil {
			return encoder.EncodeToken(token)
		}
		ratingTokens = append(ratingTokens, xml.CopyToken(token))
		if text, ok := token.(xml.CharData); ok {
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
	normalized := func(name xml.Name) string {
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
		switch t := token.(type) {
		case xml.Directive:
			return nil, errors.New("XMP directives are unsupported")
		case xml.StartElement:
			depth++
			if depth > maxSourceMetadataXMLDepth {
				return nil, bundle.ErrLimit
			}
			if depth == 1 {
				roots++
				if roots > 1 || !isXMPRoot(t.Name) {
					return nil, errors.New("invalid XMP root")
				}
			}
			if skip > 0 {
				skip++
				continue
			}
			if remove(t.Name) && !isRating(t.Name) && !sourceFlag(t.Name) {
				skip = 1
				continue
			}
			if ((ratingConfirmed || flagConfirmed) && isRating(t.Name) || sourceFlag(t.Name)) && ratingTokens == nil {
				ratingTokens = []xml.Token{}
				ratingDepth = depth
				flagTokens = sourceFlag(t.Name)
			}
			attrs := []xml.Attr{}
			seen := map[xml.Name]bool{}
			for _, a := range t.Attr {
				if seen[a.Name] {
					return nil, errors.New("duplicate XMP attribute")
				}
				seen[a.Name] = true
				if sourceFlag(a.Name) {
					saveFlag(a.Value)
					continue
				}
				reject := isRating(a.Name) && legacyReject(a.Value)
				if remove(a.Name) || flagConfirmed && reject {
					continue
				}
				if value := normalized(a.Name); value != "" {
					a.Value = value
				}
				attrs = append(attrs, a)
			}
			t.Attr = attrs
			if value := normalized(t.Name); value != "" {
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
				if err := writePhotoAuthoredXMP(encoder, input); err != nil {
					return nil, err
				}
			}
		case xml.CharData:
			if skip > 0 {
				continue
			}
			if depth == 0 && strings.TrimSpace(string(t)) != "" {
				return nil, errors.New("text outside XMP root")
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

func writePhotoAuthoredXMP(encoder *photoXMPEncoder, input store.PhotoExportInput) error {
	description := xml.StartElement{Name: xml.Name{Space: rdfNamespace, Local: "Description"}, Attr: []xml.Attr{{Name: xml.Name{Space: rdfNamespace, Local: "about"}, Value: ""}}}
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

func photoXMPConfirmed(n xml.Name, input store.PhotoExportInput) bool {
	var bit store.PhotoAuthoredFields
	switch n.Space {
	case xmpBasicNamespace:
		if n.Local == "Rating" {
			bit = store.PhotoConfirmedRating
		}
		if n.Local == "Label" {
			bit = store.PhotoConfirmedLabel
		}
	case teststripXMPNamespace:
		if n.Local == "Pick" {
			bit = store.PhotoConfirmedFlag
		}
		if n.Local == "Rotation" {
			return true
		}
	case "http://ns.adobe.com/lightroom/1.0/":
		return n.Local == "hierarchicalSubject" && len(input.Keywords) > 0
	case xmpPDFNamespace:
		return n.Local == "Keywords" && len(input.Keywords) > 0
	case xmpDublinCoreNamespace:
		switch n.Local {
		case "description":
			bit = store.PhotoConfirmedCaption
		case sourceMetadataCreatorField:
			bit = store.PhotoConfirmedCreator
		case "rights":
			bit = store.PhotoConfirmedCopyright
		case "subject":
			return len(input.Keywords) > 0
		}
	}
	return bit != 0 && input.Authored.Confirmed&bit != 0
}
