package processing

import (
	"bytes"
	"compress/zlib"
	"context"
	"encoding/binary"
	"encoding/xml"
	"errors"
	"hash/crc32"
	"io"
	"slices"
	"strconv"
	"strings"

	"go.kenn.io/docbank/document/bundle"
	"go.kenn.io/docbank/internal/store"
)

const photoXMPJPEGPrefix = "http://ns.adobe.com/xap/1.0/\x00"
const photoXMPPNGKeyword = "XML:com.adobe.xmp"

func photoExportMetadata(ctx context.Context, source []byte, input store.PhotoExportInput, receipt bundle.PhotoRenderReceipt, pixels []byte) ([]byte, error) {
	exif, packet, err := photoSourcePackets(source)
	if err != nil {
		return nil, err
	}
	if len(exif) > 0 {
		exif, err = rewritePhotoEXIF(exif, receipt.Width, receipt.Height, receipt.Profile.RemoveGPS)
		if err != nil {
			return nil, err
		}
	}
	packet, err = mergePhotoXMP(ctx, packet, input, receipt)
	if err != nil {
		return nil, err
	}
	var out bytes.Buffer
	if receipt.Profile.Format == "jpeg" {
		out.Write(pixels[:2])
		for _, payload := range [][]byte{append([]byte("Exif\x00\x00"), exif...), append([]byte(photoXMPJPEGPrefix), packet...)} {
			if bytes.HasPrefix(payload, []byte("Exif")) && len(exif) == 0 {
				continue
			}
			if len(payload) > 65533 {
				return nil, errors.New("export metadata exceeds JPEG APP1 limit")
			}
			out.Write([]byte{0xff, 0xe1, byte((len(payload) + 2) >> 8), byte(len(payload) + 2)})
			out.Write(payload)
		}
		out.Write(pixels[2:])
	} else {
		out.Write(pixels[:33])
		if len(exif) > 0 {
			writePhotoPNGChunk(&out, "eXIf", exif)
		}
		writePhotoPNGChunk(&out, "iTXt", append([]byte(photoXMPPNGKeyword+"\x00\x00\x00\x00\x00"), packet...))
		out.Write(pixels[33:])
	}
	return out.Bytes(), nil
}

func writePhotoPNGChunk(out *bytes.Buffer, kind string, payload []byte) {
	var n [4]byte
	binary.BigEndian.PutUint32(n[:], uint32(len(payload)))
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
func photoSourcePackets(data []byte) (exif, packet []byte, err error) {
	set := func(destination *[]byte, value []byte) error {
		if len(*destination) != 0 || len(value) == 0 || len(value) > maxPhotoSidecarBytes {
			return errors.New("duplicate or oversized photo metadata")
		}
		*destination = value
		return nil
	}
	switch {
	case bytes.HasPrefix(data, []byte{0xff, 0xd8}):
		position := 2
		for range visualPreviewMaxJPEGSegments {
			if position >= len(data) || data[position] != 0xff {
				return nil, nil, errors.New("malformed JPEG metadata")
			}
			for position < len(data) && data[position] == 0xff {
				position++
			}
			if position >= len(data) {
				return nil, nil, io.ErrUnexpectedEOF
			}
			marker := data[position]
			position++
			if marker == 0xda || marker == 0xd9 {
				return exif, packet, nil
			}
			if marker == 0x01 || marker >= 0xd0 && marker <= 0xd7 {
				continue
			}
			if position > len(data)-2 {
				return nil, nil, io.ErrUnexpectedEOF
			}
			n := int(binary.BigEndian.Uint16(data[position:]))
			if n < 2 || n > len(data)-position {
				return nil, nil, io.ErrUnexpectedEOF
			}
			payload := data[position+2 : position+n]
			position += n
			if marker != 0xe1 {
				continue
			}
			switch {
			case bytes.HasPrefix(payload, []byte("Exif\x00\x00")):
				err = set(&exif, payload[6:])
			case bytes.HasPrefix(payload, []byte(photoXMPJPEGPrefix)):
				err = set(&packet, payload[len(photoXMPJPEGPrefix):])
			case bytes.HasPrefix(payload, []byte("http://ns.adobe.com/xmp/extension/")):
				return nil, nil, errors.New("extended XMP metadata is unsupported")
			}
			if err != nil {
				return nil, nil, err
			}
		}
		return nil, nil, errors.New("too many JPEG segments")
	case bytes.HasPrefix(data, []byte("\x89PNG\r\n\x1a\n")):
		position := 8
		for range visualPreviewMaxPNGChunks {
			if position > len(data)-12 {
				return nil, nil, io.ErrUnexpectedEOF
			}
			n := int64(binary.BigEndian.Uint32(data[position:]))
			if n > int64(len(data)-position-12) {
				return nil, nil, io.ErrUnexpectedEOF
			}
			kind := string(data[position+4 : position+8])
			payload := data[position+8 : position+8+int(n)]
			position += 12 + int(n)
			if kind == "eXIf" {
				err = set(&exif, payload)
			}
			if kind == "iTXt" && bytes.HasPrefix(payload, []byte(photoXMPPNGKeyword+"\x00")) {
				text := payload[len(photoXMPPNGKeyword)+1:]
				if len(text) < 4 || text[0] > 1 || text[1] != 0 {
					return nil, nil, errors.New("malformed PNG XMP")
				}
				compressed := text[0] == 1
				text = text[2:]
				for range 2 {
					at := bytes.IndexByte(text, 0)
					if at < 0 {
						return nil, nil, errors.New("malformed PNG XMP")
					}
					text = text[at+1:]
				}
				if compressed {
					reader, e := zlib.NewReader(bytes.NewReader(text))
					if e != nil {
						return nil, nil, e
					}
					text, e = io.ReadAll(io.LimitReader(reader, maxPhotoSidecarBytes+1))
					e = errors.Join(e, reader.Close())
					if e != nil {
						return nil, nil, e
					}
				}
				err = set(&packet, text)
			}
			if err != nil {
				return nil, nil, err
			}
			if kind == "IEND" {
				return exif, packet, nil
			}
		}
		return nil, nil, errors.New("too many PNG chunks")
	case exifTIFFSignature(data):
		reader, ok := newExifReader(data)
		if !ok {
			return nil, nil, errors.New("invalid RAW EXIF")
		}
		root, ok := reader.typedEntries(reader.u32(4))
		if !ok {
			return nil, nil, errors.New("invalid RAW EXIF directory")
		}
		if value, ok := root[700]; ok {
			if err := set(&packet, value.value); err != nil {
				return nil, nil, err
			}
		}
		return data, packet, nil
	case isSourceMetadataRAF(data):
		location, found, malformed, err := inspectVisualPreviewRAF(bytes.NewReader(data), int64(len(data)))
		if err != nil {
			return nil, nil, err
		}
		if malformed || !found {
			return nil, nil, errors.New("invalid RAF metadata")
		}
		return photoSourcePackets(data[location.offset : location.offset+location.length])
	case bytes.HasPrefix(data, []byte("RIFF")) && len(data) >= 12 && string(data[8:12]) == "WEBP":
		for position, count := 12, 0; position < len(data); count++ {
			if count >= visualPreviewMaxWebPChunks || position > len(data)-8 {
				return nil, nil, errors.New("malformed WebP metadata")
			}
			n := int64(binary.LittleEndian.Uint32(data[position+4:]))
			if n > int64(len(data)-position-8) {
				return nil, nil, io.ErrUnexpectedEOF
			}
			payload := data[position+8 : position+8+int(n)]
			switch string(data[position : position+4]) {
			case "EXIF":
				err = set(&exif, bytes.TrimPrefix(payload, []byte("Exif\x00\x00")))
			case "XMP ":
				err = set(&packet, payload)
			}
			if err != nil {
				return nil, nil, err
			}
			position += 8 + int(n) + int(n%2)
		}
	}
	return exif, packet, nil
}

// Rebuilding only reachable directories removes GPS payloads and stale thumbnails.
func rewritePhotoEXIF(data []byte, width, height int, removeGPS bool) ([]byte, error) {
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
		entries, ok := r.typedEntries(offset)
		if !ok {
			return 0, errors.New("malformed EXIF directory")
		}
		for tag, entry := range entries {
			// These fields address original pixels, alternate images, or duplicate XMP.
			if slices.Contains([]uint16{0x0111, 0x0117, 0x0144, 0x0145, 0x014a, 0x0201, 0x0202, 700}, tag) || removeGPS && tag == 0x8825 {
				delete(entries, tag)
				continue
			}
			if len(entry.value) == 0 {
				return 0, errors.New("unsupported EXIF entry")
			}
		}
		long := func(n int) exifEntry {
			b := make([]byte, 4)
			r.order.PutUint32(b, uint32(n))
			return exifEntry{kind: 4, value: b}
		}
		short := func(n int) exifEntry {
			b := make([]byte, 2)
			r.order.PutUint16(b, uint16(n))
			return exifEntry{kind: 3, value: b}
		}
		if depth == 0 {
			entries[0x0100], entries[0x0101], entries[0x0112] = long(width), long(height), short(1)
		}
		if _, exists := entries[0x0112]; exists {
			entries[0x0112] = short(1)
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
		start := uint32(len(out))
		out = append(out, make([]byte, 2+12*len(tags)+4)...)
		r.order.PutUint16(out[start:], uint16(len(tags)))
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
			r.order.PutUint32(out[base+4:], uint32(len(e.value)/size))
			if len(e.value) <= 4 {
				copy(out[base+8:base+12], e.value)
			} else {
				r.order.PutUint32(out[base+8:], uint32(len(out)))
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

func mergePhotoXMP(ctx context.Context, packet []byte, input store.PhotoExportInput, receipt bundle.PhotoRenderReceipt) ([]byte, error) {
	if len(packet) == 0 {
		packet = []byte(`<x:xmpmeta xmlns:x="adobe:ns:meta/"><rdf:RDF xmlns:rdf="http://www.w3.org/1999/02/22-rdf-syntax-ns#"></rdf:RDF></x:xmpmeta>`)
	}
	if len(packet) > maxPhotoSidecarBytes {
		return nil, bundle.ErrLimit
	}
	decoder := xml.NewDecoder(bytes.NewReader(packet))
	var out bytes.Buffer
	encoder := xml.NewEncoder(&out)
	authored := func(n xml.Name) bool {
		return n.Space == xmpBasicNamespace && (n.Local == "Rating" || n.Local == "Label") || n.Space == teststripXMPNamespace && (n.Local == "Pick" || n.Local == "Rotation") || n.Space == xmpDublinCoreNamespace && (n.Local == "description" || n.Local == "creator" || n.Local == "rights" || n.Local == "subject")
	}
	remove := func(n xml.Name) bool {
		return authored(n) || receipt.Profile.RemoveGPS && strings.HasPrefix(strings.ToUpper(n.Local), "GPS")
	}
	depth, skip, roots, rdf := 0, 0, 0, 0
	normalized := func(name string) string {
		switch name {
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
			return nil, err
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
			if remove(t.Name) {
				skip = 1
				continue
			}
			attrs := []xml.Attr{}
			seen := map[xml.Name]bool{}
			for _, a := range t.Attr {
				if seen[a.Name] {
					return nil, errors.New("duplicate XMP attribute")
				}
				seen[a.Name] = true
				if remove(a.Name) || a.Name.Local == "xmlns" {
					continue
				}
				if a.Name.Space == "xmlns" {
					a.Name = xml.Name{Local: "xmlns:" + a.Name.Local}
				}
				if value := normalized(a.Name.Local); value != "" {
					a.Value = value
				}
				attrs = append(attrs, a)
			}
			t.Attr = attrs
			if value := normalized(t.Name.Local); value != "" {
				if err := encoder.EncodeElement(value, t); err != nil {
					return nil, err
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
		if err := encoder.EncodeToken(token); err != nil {
			return nil, err
		}
	}
	if roots != 1 || rdf != 1 || depth != 0 {
		return nil, errors.New("incomplete XMP packet")
	}
	if err := encoder.Flush(); err != nil {
		return nil, err
	}
	if out.Len() > maxPhotoSidecarBytes {
		return nil, bundle.ErrLimit
	}
	return out.Bytes(), nil
}

func writePhotoAuthoredXMP(encoder *xml.Encoder, input store.PhotoExportInput) error {
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
	for _, p := range [][4]string{{xmpBasicNamespace, "Rating", strconv.Itoa(v.Rating), ""}, {xmpBasicNamespace, "Label", v.Label, ""}, {teststripXMPNamespace, "Pick", v.Flag, ""}, {teststripXMPNamespace, "Rotation", "0", ""}, {xmpDublinCoreNamespace, "description", v.Caption, "Alt"}, {xmpDublinCoreNamespace, "creator", v.Creator, "Seq"}, {xmpDublinCoreNamespace, "rights", v.Copyright, "Alt"}, {xmpDublinCoreNamespace, "subject", "", "Bag"}} {
		if err := write(p[0], p[1], p[2], p[3]); err != nil {
			return err
		}
	}
	return encoder.EncodeToken(description.End())
}
