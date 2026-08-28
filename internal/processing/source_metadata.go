package processing

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/mail"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"go.kenn.io/docbank/document"
	documentmedia "go.kenn.io/docbank/document/media"
	"go.kenn.io/docbank/internal/store"
)

const maxSourceMetadataOriginalBytes = 64 << 20

var (
	// SourceMetadataExtractorFingerprint is the stable identity of the local
	// parser bundle. Any semantic parser change must change the descriptor.
	SourceMetadataExtractorFingerprint = fingerprintSourceMetadataExtractor(
		"docbank-source-metadata:pdf-info+xmp,ooxml-core+custom,rfc5322,ical,jpeg-exif,media-id3:v3")
)

func fingerprintSourceMetadataExtractor(descriptor string) string {
	digest := sha256.Sum256([]byte(descriptor))
	return hex.EncodeToString(digest[:])
}

type sourceMetadataCatalog interface {
	MissingSourceMetadataTargets(ctx context.Context, fingerprint string, limit int) ([]store.SourceMetadataTarget, error)
	PublishSourceMetadata(ctx context.Context, sourceSHA256, fingerprint string, canonical []byte) (store.SourceMetadataGeneration, error)
}

// BackfillSourceMetadata extracts a deterministic resumable batch from exact,
// locally verified original bytes. No provider or network boundary is used.
func BackfillSourceMetadata(ctx context.Context, catalog sourceMetadataCatalog, blobs verifiedBlobReader, limit int) (int, error) {
	if catalog == nil || blobs == nil {
		return 0, errors.New("source metadata backfill requires catalog and blob stores")
	}
	targets, err := catalog.MissingSourceMetadataTargets(ctx, SourceMetadataExtractorFingerprint, limit)
	if err != nil {
		return 0, err
	}
	return BackfillSourceMetadataTargets(ctx, catalog, blobs, targets)
}

// BackfillSourceMetadataTargets processes a selected batch while allowing
// later originals to progress past a corrupt or temporarily unavailable one.
func BackfillSourceMetadataTargets(ctx context.Context, catalog sourceMetadataCatalog, blobs verifiedBlobReader, targets []store.SourceMetadataTarget) (int, error) {
	completed := 0
	var targetErrors error
	for _, target := range targets {
		if err := ctx.Err(); err != nil {
			return completed, errors.Join(targetErrors, err)
		}
		stream, size, err := blobs.OpenStreamContext(ctx, target.SourceSHA256)
		if err != nil {
			targetErrors = errors.Join(targetErrors, fmt.Errorf("opening source metadata target %s: %w", target.SourceSHA256, err))
			continue
		}
		if size != target.Size {
			closeErr := stream.Close()
			targetErrors = errors.Join(targetErrors, fmt.Errorf("source metadata target %s size changed", target.SourceSHA256), closeErr)
			continue
		}
		var data []byte
		var read int64
		if size <= maxSourceMetadataOriginalBytes {
			data, err = io.ReadAll(io.LimitReader(stream, size+1))
			read = int64(len(data))
		} else {
			read, err = io.Copy(io.Discard, stream)
		}
		err = errors.Join(err, stream.Close())
		if err != nil {
			targetErrors = errors.Join(targetErrors, fmt.Errorf("verifying source metadata target %s: %w", target.SourceSHA256, err))
			continue
		}
		if read != size {
			targetErrors = errors.Join(targetErrors, fmt.Errorf("source metadata target %s length changed: catalog=%d read=%d", target.SourceSHA256, size, read))
			continue
		}
		var metadata document.SourceMetadataV1
		if size > maxSourceMetadataOriginalBytes {
			metadata = emptySourceMetadata()
			metadata.Warnings = append(metadata.Warnings, sourceWarning("input_too_large", "container", "bytes", "verified original exceeds the local extraction limit"))
		} else {
			metadata = ExtractSourceMetadata(data)
		}
		canonical, _, err := document.MarshalSourceMetadataV1(metadata)
		if err != nil {
			targetErrors = errors.Join(targetErrors, fmt.Errorf("canonicalizing source metadata target %s: %w", target.SourceSHA256, err))
			continue
		}
		if _, err := catalog.PublishSourceMetadata(ctx, target.SourceSHA256, SourceMetadataExtractorFingerprint, canonical); err != nil {
			targetErrors = errors.Join(targetErrors, fmt.Errorf("publishing source metadata target %s: %w", target.SourceSHA256, err))
			continue
		}
		completed++
	}
	return completed, targetErrors
}

func emptySourceMetadata() document.SourceMetadataV1 {
	return document.SourceMetadataV1{ContractVersion: document.SourceMetadataContractV1,
		Fields: []document.SourceMetadataFieldV1{}, Warnings: []document.SourceMetadataWarningV1{}}
}

// ExtractSourceMetadata performs bounded, format-signature-based local
// extraction. It returns warnings for unsupported or malformed input and does
// not guess from a filename, path, MIME declaration, or caller metadata.
func ExtractSourceMetadata(data []byte) document.SourceMetadataV1 {
	result := emptySourceMetadata()
	collector := metadataCollector{record: &result, seen: map[string]bool{}}
	switch {
	case bytes.HasPrefix(data, []byte("%PDF-")):
		collector.extractPDF(data)
	case bytes.HasPrefix(data, []byte("PK\x03\x04")):
		collector.extractOOXML(data)
	case bytes.HasPrefix(data, []byte("BEGIN:VCALENDAR")):
		collector.extractCalendar(data)
	case bytes.HasPrefix(data, []byte("ID3")):
		collector.extractID3(data)
	case bytes.HasPrefix(data, []byte{0xff, 0xd8}):
		collector.extractImage(data)
	default:
		if message, err := mail.ReadMessage(bytes.NewReader(data)); err == nil {
			collector.extractEmail(message)
		} else {
			result.Warnings = append(result.Warnings, sourceWarning("unsupported_format", "container", "signature", "no supported embedded metadata container was recognized"))
		}
	}
	return result
}

type metadataCollector struct {
	record *document.SourceMetadataV1
	seen   map[string]bool
}

func (c *metadataCollector) string(key, namespace, source, value string, sensitive bool) {
	value = strings.TrimSpace(value)
	if value == "" || c.seen[key] {
		return
	}
	if len(c.record.Fields) >= document.MaxSourceMetadataFields {
		c.warn("field_limit", namespace, source, "additional embedded fields were omitted")
		return
	}
	if !utf8.ValidString(value) {
		c.warn("invalid_utf8", namespace, source, "embedded value was omitted")
		return
	}
	if len(value) > document.MaxSourceMetadataValueBytes {
		c.warn("value_too_large", namespace, source, "embedded value was omitted")
		return
	}
	c.seen[key] = true
	c.record.Fields = append(c.record.Fields, document.SourceMetadataFieldV1{Key: key, Namespace: namespace,
		SourceField: source, Sensitive: sensitive, Value: document.SourceMetadataValueV1{Kind: document.SourceMetadataString, String: value}})
}
func (c *metadataCollector) strings(key, namespace, source string, values []string) {
	filtered := make([]string, 0, len(values))
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			if !utf8.ValidString(value) {
				c.warn("invalid_utf8", namespace, source, "embedded list value was omitted")
				continue
			}
			filtered = append(filtered, value)
		}
	}
	if len(filtered) == 0 || c.seen[key] {
		return
	}
	if len(c.record.Fields) >= document.MaxSourceMetadataFields {
		c.warn("field_limit", namespace, source, "additional embedded fields were omitted")
		return
	}
	if len(filtered) > document.MaxSourceMetadataListValues {
		c.warn("value_too_large", namespace, source, "embedded list was omitted")
		return
	}
	c.seen[key] = true
	c.record.Fields = append(c.record.Fields, document.SourceMetadataFieldV1{Key: key, Namespace: namespace,
		SourceField: source, Value: document.SourceMetadataValueV1{Kind: document.SourceMetadataStringList, Strings: filtered}})
}
func (c *metadataCollector) integer(key, namespace, source string, value int64) {
	if c.seen[key] {
		return
	}
	if len(c.record.Fields) >= document.MaxSourceMetadataFields {
		c.warn("field_limit", namespace, source, "additional embedded fields were omitted")
		return
	}
	c.seen[key] = true
	c.record.Fields = append(c.record.Fields, document.SourceMetadataFieldV1{Key: key, Namespace: namespace, SourceField: source,
		Value: document.SourceMetadataValueV1{Kind: document.SourceMetadataInteger, Integer: &value}})
}
func (c *metadataCollector) timestamp(key, namespace, source, raw string) {
	if c.seen[key] || strings.TrimSpace(raw) == "" {
		return
	}
	if len(c.record.Fields) >= document.MaxSourceMetadataFields {
		c.warn("field_limit", namespace, source, "additional embedded fields were omitted")
		return
	}
	stamp, ok := parseSourceTimestamp(strings.TrimSpace(raw))
	if !ok {
		c.warn("unparseable_timestamp", namespace, source, "embedded timestamp was not coerced")
		return
	}
	c.seen[key] = true
	c.record.Fields = append(c.record.Fields, document.SourceMetadataFieldV1{Key: key, Namespace: namespace, SourceField: source,
		Value: document.SourceMetadataValueV1{Kind: document.SourceMetadataTimestamp, Timestamp: &stamp}})
}
func (c *metadataCollector) warn(code, namespace, source, detail string) {
	if len(c.record.Warnings) >= document.MaxSourceMetadataWarnings {
		return
	}
	c.record.Warnings = append(c.record.Warnings, sourceWarning(code, namespace, source, detail))
}
func sourceWarning(code, namespace, source, detail string) document.SourceMetadataWarningV1 {
	return document.SourceMetadataWarningV1{Code: code, Namespace: namespace, SourceField: source, Detail: detail}
}

func (c *metadataCollector) extractPDF(data []byte) {
	info, infoErr := documentmedia.PDFInfoFields(data)
	if infoErr != nil {
		c.warn("unparseable_pdf_info", "pdf.info", "Info", "PDF Info dictionary could not be verified")
	}
	for _, name := range []string{"Title", "Author", "Subject", "Keywords", "CreationDate", "ModDate"} {
		value, ok := info[name]
		if !ok {
			continue
		}
		switch name {
		case "Title":
			c.string("title", "pdf.info", name, value, false)
		case "Author":
			c.strings("creators", "pdf.info", name, splitValues(value))
		case "Subject":
			c.string("subject", "pdf.info", name, value, false)
		case "Keywords":
			c.strings("keywords", "pdf.info", name, splitValues(value))
		case "CreationDate":
			c.timestamp("created", "pdf.info", name, value)
		case "ModDate":
			c.timestamp("modified", "pdf.info", name, value)
		}
	}
	if count, err := documentmedia.CountPDFPages(data); err == nil {
		c.integer("page_count", "pdf.info", "Pages", count)
	} else {
		c.warn("unparseable_pdf_pages", "pdf.info", "Pages", "PDF page tree could not be verified")
	}
	if start := bytes.Index(data, []byte("<x:xmpmeta")); start >= 0 {
		if relativeEnd := bytes.Index(data[start:], []byte("</x:xmpmeta>")); relativeEnd >= 0 {
			end := start + relativeEnd + len("</x:xmpmeta>")
			c.extractXMLText(data[start:end], "xmp")
		}
	}
}

func (c *metadataCollector) extractOOXML(data []byte) {
	reader, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		c.warn("malformed_container", "office.core", "zip", "OOXML container could not be read")
		return
	}
	if len(reader.File) > 4096 {
		c.warn("container_limit", "office.core", "zip", "OOXML container has too many entries")
		return
	}
	var extracted int64
	for _, file := range reader.File {
		if file.UncompressedSize64 > 4<<20 {
			continue
		}
		if file.Name != "docProps/core.xml" && file.Name != "docProps/custom.xml" && file.Name != "docProps/app.xml" {
			continue
		}
		extracted += int64(file.UncompressedSize64)
		if extracted > 16<<20 {
			c.warn("container_limit", "office.core", "zip", "OOXML property data exceeds extraction limit")
			return
		}
		stream, err := file.Open()
		if err != nil {
			continue
		}
		payload, readErr := io.ReadAll(io.LimitReader(stream, 4<<20+1))
		_ = stream.Close()
		if readErr != nil || len(payload) > 4<<20 {
			c.warn("malformed_container", "office.core", file.Name, "property document could not be read")
			continue
		}
		switch file.Name {
		case "docProps/app.xml":
			c.extractOfficeApp(payload)
		case "docProps/custom.xml":
			c.extractOfficeCustom(payload)
		default:
			c.extractXMLText(payload, strings.TrimSuffix(strings.ReplaceAll(file.Name, "docProps/", "office."), ".xml"))
		}
	}
}

func (c *metadataCollector) extractXMLText(data []byte, namespace string) {
	decoder := xml.NewDecoder(bytes.NewReader(data))
	type element struct {
		name string
		text strings.Builder
	}
	var stack []element
	for {
		token, err := decoder.Token()
		if err != nil {
			break
		}
		switch value := token.(type) {
		case xml.StartElement:
			stack = append(stack, element{name: value.Name.Local})
		case xml.CharData:
			for index := range stack {
				stack[index].text.Write([]byte(value))
			}
		case xml.EndElement:
			if len(stack) == 0 {
				continue
			}
			current := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			c.extractXMLValue(namespace, current.name, current.text.String())
		}
	}
}

func (c *metadataCollector) extractXMLValue(namespace, name, text string) {
	switch strings.ToLower(name) {
	case "title":
		c.string("title", namespace, name, text, false)
	case "creator", "author":
		c.strings("creators", namespace, name, splitValues(text))
	case "subject":
		c.string("subject", namespace, name, text, false)
	case "description":
		c.string("description", namespace, name, text, false)
	case "keywords":
		c.strings("keywords", namespace, name, splitValues(text))
	case "language":
		c.string("language", namespace, name, text, false)
	case "created", "createdate":
		c.timestamp("created", namespace, name, text)
	case "modified", "modifydate":
		c.timestamp("modified", namespace, name, text)
	default:
		if strings.HasPrefix(namespace, "office.custom") {
			key := "office.custom." + canonicalFieldName(name)
			if document.SourceMetadataCanonicalKeyAllowed(key) {
				c.string(key, namespace, name, text, false)
			}
		}
	}
}

func (c *metadataCollector) extractOfficeApp(data []byte) {
	decoder := xml.NewDecoder(bytes.NewReader(data))
	for {
		token, err := decoder.Token()
		if err != nil {
			return
		}
		start, ok := token.(xml.StartElement)
		if !ok {
			continue
		}
		var text string
		if err := decoder.DecodeElement(&text, &start); err != nil {
			continue
		}
		value, err := strconv.ParseInt(strings.TrimSpace(text), 10, 64)
		if err != nil {
			continue
		}
		switch strings.ToLower(start.Name.Local) {
		case "pages", "slides":
			c.integer("page_count", "office.core", start.Name.Local, value)
		case "words":
			c.integer("office.core.word_count", "office.core", start.Name.Local, value)
		}
	}
}

func (c *metadataCollector) extractOfficeCustom(data []byte) {
	decoder := xml.NewDecoder(bytes.NewReader(data))
	for {
		token, err := decoder.Token()
		if err != nil {
			return
		}
		start, ok := token.(xml.StartElement)
		if !ok || !strings.EqualFold(start.Name.Local, "property") {
			continue
		}
		name := ""
		for _, attribute := range start.Attr {
			if strings.EqualFold(attribute.Name.Local, "name") {
				name = attribute.Value
			}
		}
		var text strings.Builder
		depth := 1
		for depth > 0 {
			inner, innerErr := decoder.Token()
			if innerErr != nil {
				break
			}
			switch value := inner.(type) {
			case xml.StartElement:
				depth++
			case xml.EndElement:
				depth--
			case xml.CharData:
				text.Write([]byte(value))
			}
		}
		if name == "" {
			continue
		}
		key := "office.custom." + canonicalFieldName(name)
		if document.SourceMetadataCanonicalKeyAllowed(key) {
			c.string(key, "office.custom", name, text.String(), false)
		}
	}
}

func (c *metadataCollector) extractEmail(message *mail.Message) {
	for _, item := range []struct {
		header, key string
		sensitive   bool
	}{{"From", "email.from", false}, {"To", "email.to", false}, {"Cc", "email.cc", false}, {"Bcc", "email.bcc", true}, {"Subject", "email.subject", false}} {
		if value := message.Header.Get(item.header); value != "" {
			c.string(item.key, "email", item.header, value, item.sensitive)
		}
	}
	if date := message.Header.Get("Date"); date != "" {
		c.timestamp("email.sent", "email", "Date", date)
	}
	if values := message.Header["Received"]; len(values) > 0 {
		c.strings("email.received", "email", "Received", values)
	}
	parts := 0
	count, err := countEmailAttachments(message.Header, message.Body, 0, &parts)
	if err != nil {
		c.warn("unparseable_attachments", "email", "Content-Disposition", "MIME attachment structure could not be verified")
	} else if count > 0 {
		c.integer("attachment_count", "email", "Content-Disposition", count)
	}
}

type emailMIMEHeader interface {
	Get(key string) string
}

func countEmailAttachments(
	header emailMIMEHeader, body io.Reader, depth int, parts *int,
) (int64, error) {
	const (
		maxMIMEDepth = 8
		maxMIMEParts = 4096
	)
	if depth > maxMIMEDepth {
		return 0, errors.New("MIME nesting exceeds the supported bound")
	}
	contentType := header.Get("Content-Type")
	mediaType, parameters, err := mime.ParseMediaType(contentType)
	if err != nil {
		if strings.TrimSpace(contentType) == "" {
			return 0, nil
		}
		return 0, fmt.Errorf("parse MIME content type: %w", err)
	}
	if !strings.HasPrefix(strings.ToLower(mediaType), "multipart/") {
		return 0, nil
	}
	boundary := parameters["boundary"]
	if boundary == "" {
		return 0, errors.New("multipart MIME content type has no boundary")
	}
	reader := multipart.NewReader(body, boundary)
	var count int64
	for {
		part, nextErr := reader.NextPart()
		if errors.Is(nextErr, io.EOF) {
			return count, nil
		}
		if nextErr != nil {
			return 0, fmt.Errorf("read MIME part: %w", nextErr)
		}
		*parts++
		if *parts > maxMIMEParts {
			_ = part.Close()
			return 0, errors.New("MIME part count exceeds the supported bound")
		}
		dispositionValue := part.Header.Get("Content-Disposition")
		if dispositionValue != "" {
			disposition, _, dispositionErr := mime.ParseMediaType(dispositionValue)
			if dispositionErr != nil {
				_ = part.Close()
				return 0, fmt.Errorf("parse MIME content disposition: %w", dispositionErr)
			}
			if strings.EqualFold(disposition, "attachment") {
				count++
			}
		}
		nested, nestedErr := countEmailAttachments(part.Header, part, depth+1, parts)
		closeErr := part.Close()
		if err := errors.Join(nestedErr, closeErr); err != nil {
			return 0, err
		}
		count += nested
	}
}

func (c *metadataCollector) extractCalendar(data []byte) {
	lines := unfoldCalendar(string(data))
	for _, line := range lines {
		name, value, found := strings.Cut(line, ":")
		if !found {
			continue
		}
		base := strings.ToUpper(strings.Split(name, ";")[0])
		switch base {
		case "SUMMARY":
			c.string("title", "calendar", name, value, false)
		case "DESCRIPTION":
			c.string("description", "calendar", name, value, false)
		case "DTSTART":
			c.timestamp("calendar.start", "calendar", name, value)
		case "DTEND":
			c.timestamp("calendar.end", "calendar", name, value)
		case "ORGANIZER":
			c.strings("creators", "calendar", name, []string{value})
		}
	}
}

func (c *metadataCollector) extractID3(data []byte) {
	if len(data) >= 10 {
		tagEnd := min(10+synchsafeInt(data[6:10]), len(data))
		for offset := 10; offset+10 <= tagEnd; {
			id := string(data[offset : offset+4])
			if strings.Trim(id, "\x00") == "" {
				break
			}
			size := int(binary.BigEndian.Uint32(data[offset+4 : offset+8]))
			if data[3] >= 4 {
				size = synchsafeInt(data[offset+4 : offset+8])
			}
			offset += 10
			if size < 1 || offset+size > tagEnd {
				break
			}
			payload := data[offset : offset+size]
			offset += size
			value := ""
			if payload[0] == 0 || payload[0] == 3 {
				value = strings.TrimRight(string(payload[1:]), "\x00")
			}
			if value == "" {
				continue
			}
			c.addID3Frame(id, value)
		}
	}
}
func synchsafeInt(value []byte) int {
	if len(value) < 4 {
		return 0
	}
	return int(value[0]&0x7f)<<21 | int(value[1]&0x7f)<<14 | int(value[2]&0x7f)<<7 | int(value[3]&0x7f)
}
func (c *metadataCollector) addID3Frame(frame, value string) {
	switch frame {
	case "TIT2":
		c.string("title", "media.id3", "TIT2", value, false)
	case "TPE1":
		c.strings("creators", "media.id3", "TPE1", splitValues(value))
	case "TALB":
		c.string("media.id3.album", "media.id3", "TALB", value, false)
	case "TDRC":
		c.timestamp("created", "media.id3", "TDRC", value)
	}
}

func (c *metadataCollector) extractImage(data []byte) {
	for offset := 2; offset+4 <= len(data); {
		if data[offset] != 0xff {
			offset++
			continue
		}
		marker := data[offset+1]
		offset += 2
		if marker == 0xd9 || marker == 0xda {
			break
		}
		if offset+2 > len(data) {
			break
		}
		length := int(binary.BigEndian.Uint16(data[offset : offset+2]))
		if length < 2 || offset+length > len(data) {
			break
		}
		segment := data[offset+2 : offset+length]
		if marker == 0xe1 && bytes.HasPrefix(segment, []byte("Exif\x00\x00")) {
			c.extractExifTIFF(segment[6:])
		}
		offset += length
	}
	if len(c.record.Fields) == 0 {
		c.warn("unparseable_metadata", "image.exif", "APP1", "image contained no supported metadata values")
	}
}

type exifReader struct {
	data  []byte
	order binary.ByteOrder
}

func newExifReader(data []byte) (exifReader, bool) {
	if len(data) < 8 {
		return exifReader{}, false
	}
	var order binary.ByteOrder
	switch string(data[:2]) {
	case "II":
		order = binary.LittleEndian
	case "MM":
		order = binary.BigEndian
	default:
		return exifReader{}, false
	}
	if order.Uint16(data[2:4]) != 42 {
		return exifReader{}, false
	}
	return exifReader{data: data, order: order}, true
}
func (r exifReader) u16(offset int) (uint16, bool) {
	if offset < 0 || offset+2 > len(r.data) {
		return 0, false
	}
	return r.order.Uint16(r.data[offset:]), true
}
func (r exifReader) u32(offset int) uint32 {
	if offset < 0 || offset+4 > len(r.data) {
		return 0
	}
	return r.order.Uint32(r.data[offset:])
}
func (r exifReader) entries(offset uint32) map[uint16][]byte {
	result := map[uint16][]byte{}
	count, ok := r.u16(int(offset))
	if !ok || count > 1024 {
		return result
	}
	for index := range count {
		base := int(offset) + 2 + int(index)*12
		tag, ok := r.u16(base)
		if !ok {
			break
		}
		kind, _ := r.u16(base + 2)
		items := r.u32(base + 4)
		width := map[uint16]uint32{1: 1, 2: 1, 3: 2, 4: 4, 5: 8, 7: 1, 9: 4, 10: 8}[kind]
		size := items * width
		if width == 0 || size > 1<<20 {
			continue
		}
		start := base + 8
		if size > 4 {
			pointer := r.u32(base + 8)
			start = int(pointer)
		}
		if start < 0 || start+int(size) > len(r.data) {
			continue
		}
		result[tag] = r.data[start : start+int(size)]
	}
	return result
}
func exifASCII(value []byte) string {
	return strings.TrimSpace(strings.TrimRight(string(value), "\x00"))
}
func (c *metadataCollector) extractExifTIFF(data []byte) {
	reader, ok := newExifReader(data)
	if !ok {
		c.warn("unparseable_metadata", "image.exif", "TIFF", "EXIF TIFF header is malformed")
		return
	}
	rootOffset := reader.u32(4)
	root := reader.entries(rootOffset)
	c.string("description", "image.exif", "ImageDescription", exifASCII(root[0x010e]), false)
	if artist := exifASCII(root[0x013b]); artist != "" {
		c.strings("creators", "image.exif", "Artist", splitValues(artist))
	}
	if stamp := exifASCII(root[0x0132]); stamp != "" {
		c.timestamp("modified", "image.exif", "DateTime", stamp)
	}
	if raw := root[0x8769]; len(raw) >= 4 {
		offset := reader.order.Uint32(raw)
		exif := reader.entries(offset)
		if stamp := exifASCII(exif[0x9003]); stamp != "" {
			c.timestamp("created", "image.exif", "DateTimeOriginal", stamp)
		}
	}
	if raw := root[0x8825]; len(raw) >= 4 {
		offset := reader.order.Uint32(raw)
		gps := reader.entries(offset)
		c.exifGPS(reader, gps)
	}
}
func (c *metadataCollector) exifGPS(reader exifReader, gps map[uint16][]byte) {
	for _, item := range []struct {
		refTag, valueTag uint16
		key, source      string
	}{{1, 2, "image.exif.gps_latitude", "GPSLatitude"}, {3, 4, "image.exif.gps_longitude", "GPSLongitude"}} {
		raw := gps[item.valueTag]
		if len(raw) < 24 {
			continue
		}
		parts := make([]float64, 3)
		valid := true
		for index := range 3 {
			numerator := reader.order.Uint32(raw[index*8:])
			denominator := reader.order.Uint32(raw[index*8+4:])
			if denominator == 0 {
				valid = false
				break
			}
			parts[index] = float64(numerator) / float64(denominator)
		}
		if !valid {
			continue
		}
		value := parts[0] + parts[1]/60 + parts[2]/3600
		ref := strings.ToUpper(exifASCII(gps[item.refTag]))
		if ref == "S" || ref == "W" {
			value = -value
		}
		c.string(item.key, "image.exif", item.source, strconv.FormatFloat(value, 'f', 7, 64), true)
	}
}

func splitValues(value string) []string {
	return strings.FieldsFunc(value, func(r rune) bool { return r == ';' || r == ',' })
}
func canonicalFieldName(value string) string {
	value = strings.ToLower(value)
	return strings.Trim(strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			return r
		}
		return '_'
	}, value), "_")
}
func unfoldCalendar(value string) []string {
	value = strings.ReplaceAll(value, "\r\n", "\n")
	value = strings.ReplaceAll(value, "\n ", "")
	value = strings.ReplaceAll(value, "\n\t", "")
	return strings.Split(value, "\n")
}

func parseSourceTimestamp(raw string) (document.SourceMetadataTimestampV1, bool) {
	value := raw
	if after, ok := strings.CutPrefix(value, "D:"); ok {
		value = after
		value = strings.ReplaceAll(value, "'", "")
		if len(value) >= 14 {
			value = value[:4] + "-" + value[4:6] + "-" + value[6:8] + "T" + value[8:10] + ":" + value[10:12] + ":" + value[12:14] + value[14:]
		}
		if len(value) >= 5 && (value[len(value)-5] == '+' || value[len(value)-5] == '-') {
			value = value[:len(value)-2] + ":" + value[len(value)-2:]
		}
	}
	if len(value) == 8 && strings.IndexFunc(value, func(r rune) bool { return r < '0' || r > '9' }) < 0 {
		value = value[:4] + "-" + value[4:6] + "-" + value[6:]
		return document.SourceMetadataTimestampV1{Raw: raw, Normalized: value, Precision: document.SourceMetadataPrecisionDate, Timezone: document.SourceMetadataTimezoneOmitted}, true
	}
	if len(value) == 15 && value[8] == 'T' {
		value = value[:4] + "-" + value[4:6] + "-" + value[6:8] + "T" + value[9:11] + ":" + value[11:13] + ":" + value[13:15]
	}
	if len(value) == 16 && strings.HasSuffix(value, "Z") && value[8] == 'T' {
		value = value[:4] + "-" + value[4:6] + "-" + value[6:8] + "T" + value[9:11] + ":" + value[11:13] + ":" + value[13:]
	}
	if parsed, err := time.Parse(time.RFC3339Nano, value); err == nil {
		zone, offset := parsed.Zone()
		kind := document.SourceMetadataTimezoneOffset
		off := value[len(value)-6:]
		if zone == "UTC" || offset == 0 && strings.HasSuffix(value, "Z") {
			kind = document.SourceMetadataTimezoneUTC
			off = ""
		}
		precision := document.SourceMetadataPrecisionSecond
		if strings.Contains(strings.Split(value, "T")[1], ".") {
			precision = document.SourceMetadataPrecisionFraction
		}
		return document.SourceMetadataTimestampV1{Raw: raw, Normalized: value, Offset: off, Precision: precision, Timezone: kind}, true
	}
	if parsed, err := time.Parse("2006-01-02T15:04:05.999999999", value); err == nil {
		precision := document.SourceMetadataPrecisionSecond
		if strings.Contains(value, ".") {
			precision = document.SourceMetadataPrecisionFraction
		}
		return document.SourceMetadataTimestampV1{Raw: raw, Normalized: parsed.Format(localTimestampLayout(value)), Precision: precision, Timezone: document.SourceMetadataTimezoneOmitted}, true
	}
	if parsed, err := mail.ParseDate(value); err == nil {
		_, offset := parsed.Zone()
		normalized := parsed.Format(time.RFC3339)
		off := normalized[len(normalized)-6:]
		kind := document.SourceMetadataTimezoneOffset
		if offset == 0 {
			kind = document.SourceMetadataTimezoneUTC
			off = ""
			normalized = parsed.UTC().Format(time.RFC3339)
		}
		return document.SourceMetadataTimestampV1{Raw: raw, Normalized: normalized, Offset: off, Precision: document.SourceMetadataPrecisionSecond, Timezone: kind}, true
	}
	if parsed, err := time.Parse("2006-01-02", value); err == nil {
		return document.SourceMetadataTimestampV1{Raw: raw, Normalized: parsed.Format("2006-01-02"), Precision: document.SourceMetadataPrecisionDate, Timezone: document.SourceMetadataTimezoneOmitted}, true
	}
	if parsed, err := time.Parse("2006:01:02 15:04:05", value); err == nil {
		return document.SourceMetadataTimestampV1{Raw: raw, Normalized: parsed.Format("2006-01-02T15:04:05"), Precision: document.SourceMetadataPrecisionSecond, Timezone: document.SourceMetadataTimezoneOmitted}, true
	}
	return document.SourceMetadataTimestampV1{}, false
}

func localTimestampLayout(value string) string {
	if strings.Contains(value, ".") {
		return "2006-01-02T15:04:05.999999999"
	}
	return "2006-01-02T15:04:05"
}
