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
	"unicode/utf16"
	"unicode/utf8"

	"go.kenn.io/docbank/document"
	documentmedia "go.kenn.io/docbank/document/media"
	"go.kenn.io/docbank/internal/store"
)

const (
	maxSourceMetadataOriginalBytes       = 64 << 20
	maxSourceMetadataAggregateValueBytes = 1 << 20
	maxSourceMetadataXMLDepth            = 64
	rdfNamespace                         = "http://www.w3.org/1999/02/22-rdf-syntax-ns#"
	xmlNamespace                         = "http://www.w3.org/XML/1998/namespace"
	xmpBasicNamespace                    = "http://ns.adobe.com/xap/1.0/"
	xmpDublinCoreNamespace               = "http://purl.org/dc/elements/1.1/"
	xmpPDFNamespace                      = "http://ns.adobe.com/pdf/1.3/"
)

var (
	// SourceMetadataExtractorFingerprint is the stable identity of the local
	// parser bundle. Any semantic parser change must change the descriptor.
	SourceMetadataExtractorFingerprint = fingerprintSourceMetadataExtractor(
		"docbank-source-metadata:pdfcpu-info+xmp+pages,ooxml-core+custom,rfc5322,ical,jpeg-exif,media-id3:v7")
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
	return canonicalSourceMetadataResult(result)
}

func canonicalSourceMetadataResult(metadata document.SourceMetadataV1) document.SourceMetadataV1 {
	if _, _, err := document.MarshalSourceMetadataV1(metadata); err == nil {
		return metadata
	}
	fallback := emptySourceMetadata()
	fallback.Warnings = append(fallback.Warnings, sourceWarning(
		"extraction_limit", "container", "metadata",
		"embedded metadata exceeded the canonical extraction bounds"))
	return fallback
}

type metadataCollector struct {
	record     *document.SourceMetadataV1
	seen       map[string]bool
	valueBytes int
}

func validSourceMetadataLabel(value string) bool {
	value = strings.TrimSpace(value)
	return value != "" && len(value) <= document.MaxSourceMetadataLabelBytes && utf8.ValidString(value)
}

func boundedSourceMetadataLabel(value, fallback string) string {
	value = strings.TrimSpace(value)
	if validSourceMetadataLabel(value) {
		return value
	}
	return fallback
}

func (c *metadataCollector) fieldLabelsAllowed(key, namespace, source string) bool {
	if document.SourceMetadataCanonicalKeyAllowed(key) && validSourceMetadataLabel(namespace) &&
		validSourceMetadataLabel(source) {
		return true
	}
	c.warn("invalid_label", namespace, source, "embedded metadata with an invalid label was omitted")
	return false
}

func (c *metadataCollector) reserveValueBytes(size int, namespace, source string) bool {
	if size < 0 || size > maxSourceMetadataAggregateValueBytes-c.valueBytes {
		c.warn("aggregate_value_limit", namespace, source, "additional embedded values were omitted")
		return false
	}
	c.valueBytes += size
	return true
}

func (c *metadataCollector) string(key, namespace, source, value string, sensitive bool) {
	value = strings.TrimSpace(value)
	if value == "" || c.seen[key] {
		return
	}
	if !c.fieldLabelsAllowed(key, namespace, source) {
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
	if !c.reserveValueBytes(len(value), namespace, source) {
		return
	}
	c.seen[key] = true
	c.record.Fields = append(c.record.Fields, document.SourceMetadataFieldV1{Key: key, Namespace: namespace,
		SourceField: source, Sensitive: sensitive, Value: document.SourceMetadataValueV1{Kind: document.SourceMetadataString, String: value}})
}
func (c *metadataCollector) strings(key, namespace, source string, values []string, sensitive bool) {
	filtered := make([]string, 0, len(values))
	valueBytes := 0
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			if !utf8.ValidString(value) {
				c.warn("invalid_utf8", namespace, source, "embedded list value was omitted")
				continue
			}
			if len(value) > document.MaxSourceMetadataValueBytes {
				c.warn("value_too_large", namespace, source, "embedded list value was omitted")
				continue
			}
			filtered = append(filtered, value)
			valueBytes += len(value)
		}
	}
	if len(filtered) == 0 || c.seen[key] {
		return
	}
	if !c.fieldLabelsAllowed(key, namespace, source) {
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
	if !c.reserveValueBytes(valueBytes, namespace, source) {
		return
	}
	c.seen[key] = true
	c.record.Fields = append(c.record.Fields, document.SourceMetadataFieldV1{Key: key, Namespace: namespace,
		SourceField: source, Sensitive: sensitive, Value: document.SourceMetadataValueV1{Kind: document.SourceMetadataStringList, Strings: filtered}})
}
func (c *metadataCollector) integer(key, namespace, source string, value int64) {
	if c.seen[key] {
		return
	}
	if !c.fieldLabelsAllowed(key, namespace, source) {
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
	raw = strings.TrimSpace(raw)
	if c.seen[key] || raw == "" {
		return
	}
	if !c.fieldLabelsAllowed(key, namespace, source) {
		return
	}
	if len(c.record.Fields) >= document.MaxSourceMetadataFields {
		c.warn("field_limit", namespace, source, "additional embedded fields were omitted")
		return
	}
	if len(raw) > document.MaxSourceMetadataValueBytes {
		c.warn("value_too_large", namespace, source, "embedded timestamp was omitted")
		return
	}
	stamp, ok := parseSourceTimestamp(raw)
	if !ok {
		c.warn("unparseable_timestamp", namespace, source, "embedded timestamp was not coerced")
		return
	}
	if !c.reserveValueBytes(len(stamp.Raw)+len(stamp.Normalized)+len(stamp.Offset), namespace, source) {
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
	code = boundedSourceMetadataLabel(code, "extraction_warning")
	namespace = boundedSourceMetadataLabel(namespace, "container")
	source = boundedSourceMetadataLabel(source, "metadata")
	if !utf8.ValidString(detail) || len(detail) > document.MaxSourceMetadataValueBytes {
		detail = "embedded metadata warning detail was omitted"
	}
	c.record.Warnings = append(c.record.Warnings, sourceWarning(code, namespace, source, detail))
}
func sourceWarning(code, namespace, source, detail string) document.SourceMetadataWarningV1 {
	return document.SourceMetadataWarningV1{Code: code, Namespace: namespace, SourceField: source, Detail: detail}
}

func (c *metadataCollector) extractPDF(data []byte) {
	metadata, err := documentmedia.ReadPDFMetadata(data)
	if err != nil {
		c.warn("unparseable_pdf_pages", "pdf.info", "Pages", "PDF page tree could not be verified")
		return
	}
	c.string("title", "pdf.info", "Title", metadata.Info.Title, false)
	c.strings("creators", "pdf.info", "Author", splitValues(metadata.Info.Author), false)
	c.string("subject", "pdf.info", "Subject", metadata.Info.Subject, false)
	c.strings("keywords", "pdf.info", "Keywords", splitValues(metadata.Info.Keywords), false)
	c.timestamp("created", "pdf.info", "CreationDate", metadata.Info.CreationDate)
	c.timestamp("modified", "pdf.info", "ModDate", metadata.Info.ModDate)
	c.integer("page_count", "pdf.info", "Pages", metadata.Pages)
	for _, issue := range metadata.Issues {
		namespace := "pdf.info"
		if issue.SourceField == "XMP" {
			namespace = "xmp"
		}
		c.warn("unparseable_pdf_metadata", namespace, issue.SourceField, "optional PDF metadata was omitted")
	}
	if len(metadata.XMP) != 0 {
		c.extractXMLText(metadata.XMP, "xmp")
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

type boundedSourceMetadataText struct {
	value    []byte
	overflow bool
}

func (text *boundedSourceMetadataText) write(value []byte) {
	remaining := document.MaxSourceMetadataValueBytes - len(text.value)
	if remaining <= 0 {
		text.overflow = text.overflow || len(value) != 0
		return
	}
	if len(value) > remaining {
		text.value = append(text.value, value[:remaining]...)
		text.overflow = true
		return
	}
	text.value = append(text.value, value...)
}

func (text *boundedSourceMetadataText) string() string {
	return string(text.value)
}

type sourceMetadataXMLMember struct {
	value    string
	language string
}

func (c *metadataCollector) extractXMLText(data []byte, namespace string) {
	decoder := xml.NewDecoder(bytes.NewReader(data))
	type element struct {
		name    xml.Name
		text    boundedSourceMetadataText
		members []sourceMetadataXMLMember
		lang    string
	}
	var stack []element
	for {
		token, err := decoder.Token()
		if errors.Is(err, io.EOF) {
			return
		}
		if err != nil {
			c.warnMalformedXML(namespace, "XML")
			return
		}
		switch value := token.(type) {
		case xml.StartElement:
			if len(stack) >= maxSourceMetadataXMLDepth {
				c.warn("xml_depth_limit", namespace, value.Name.Local, "embedded XML nesting exceeds the extraction limit")
				return
			}
			current := element{name: value.Name}
			for _, attribute := range value.Attr {
				if attribute.Name.Space == xmlNamespace && attribute.Name.Local == "lang" {
					current.lang = attribute.Value
				}
				if namespace == "xmp" && xmpAttributeAllowed(attribute.Name) {
					c.extractXMLValue(namespace, attribute.Name.Local, attribute.Value)
				}
			}
			stack = append(stack, current)
		case xml.CharData:
			for index := range stack {
				stack[index].text.write(value)
			}
		case xml.EndElement:
			if len(stack) == 0 {
				continue
			}
			current := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			if current.text.overflow {
				c.warn("value_too_large", namespace, current.name.Local, "embedded XML value was omitted")
				continue
			}
			if namespace == "xmp" && current.name.Space == rdfNamespace && current.name.Local == "li" {
				for index := len(stack) - 1; index >= 0; index-- {
					if stack[index].name.Space == rdfNamespace || !sourceMetadataXMLValueAllowed(stack[index].name.Local) {
						continue
					}
					if len(stack[index].members) >= document.MaxSourceMetadataListValues {
						c.warn("value_too_large", namespace, stack[index].name.Local, "embedded XML collection was omitted")
						break
					}
					stack[index].members = append(stack[index].members, sourceMetadataXMLMember{
						value: current.text.string(), language: current.lang,
					})
					break
				}
				continue
			}
			if len(current.members) != 0 {
				c.extractXMLCollection(namespace, current.name.Local, current.members)
				continue
			}
			c.extractXMLValue(namespace, current.name.Local, current.text.string())
		}
	}
}

func (c *metadataCollector) warnMalformedXML(namespace, source string) {
	c.warn("malformed_metadata", namespace, source, "embedded XML metadata is malformed")
}

func xmpAttributeAllowed(name xml.Name) bool {
	switch name.Space {
	case xmpBasicNamespace:
		return strings.EqualFold(name.Local, "CreateDate") || strings.EqualFold(name.Local, "ModifyDate")
	case xmpDublinCoreNamespace:
		return sourceMetadataXMLValueAllowed(name.Local)
	case xmpPDFNamespace:
		return strings.EqualFold(name.Local, "Keywords")
	default:
		return false
	}
}

func sourceMetadataXMLValueAllowed(name string) bool {
	switch strings.ToLower(name) {
	case "title", "creator", "author", "subject", "description", "keywords", "language",
		"created", "createdate", "modified", "modifydate":
		return true
	default:
		return false
	}
}

func (c *metadataCollector) extractXMLCollection(namespace, name string, members []sourceMetadataXMLMember) {
	values := make([]string, 0, len(members))
	defaultValue := ""
	for _, member := range members {
		if value := strings.TrimSpace(member.value); value != "" {
			values = append(values, value)
			if strings.EqualFold(member.language, "x-default") {
				defaultValue = value
			}
		}
	}
	if len(values) == 0 {
		return
	}
	switch strings.ToLower(name) {
	case "creator", "author":
		c.strings("creators", namespace, name, values, false)
	case "keywords":
		c.strings("keywords", namespace, name, values, false)
	default:
		if defaultValue == "" {
			defaultValue = values[0]
		}
		c.extractXMLValue(namespace, name, defaultValue)
	}
}

func (c *metadataCollector) extractXMLValue(namespace, name, text string) {
	switch strings.ToLower(name) {
	case "title":
		c.string("title", namespace, name, text, false)
	case "creator", "author":
		c.strings("creators", namespace, name, splitValues(text), false)
	case "subject":
		c.string("subject", namespace, name, text, false)
	case "description":
		c.string("description", namespace, name, text, false)
	case "keywords":
		c.strings("keywords", namespace, name, splitValues(text), false)
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
	var properties struct {
		Pages  string `xml:"Pages"`
		Slides string `xml:"Slides"`
		Words  string `xml:"Words"`
	}
	if err := xml.Unmarshal(data, &properties); err != nil {
		c.warnMalformedXML("office.core", "app.xml")
		return
	}
	for _, field := range []struct {
		name string
		text string
	}{{"Pages", properties.Pages}, {"Slides", properties.Slides}, {"Words", properties.Words}} {
		value, err := strconv.ParseInt(strings.TrimSpace(field.text), 10, 64)
		if err != nil {
			continue
		}
		switch strings.ToLower(field.name) {
		case "pages", "slides":
			c.integer("page_count", "office.core", field.name, value)
		case "words":
			c.integer("office.core.word_count", "office.core", field.name, value)
		}
	}
}

func (c *metadataCollector) extractOfficeCustom(data []byte) {
	decoder := xml.NewDecoder(bytes.NewReader(data))
	for {
		token, err := decoder.Token()
		if errors.Is(err, io.EOF) {
			return
		}
		if err != nil {
			c.warnMalformedXML("office.custom", "custom.xml")
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
		var text boundedSourceMetadataText
		depth := 1
		for depth > 0 {
			inner, innerErr := decoder.Token()
			if innerErr != nil {
				c.warnMalformedXML("office.custom", name)
				return
			}
			switch value := inner.(type) {
			case xml.StartElement:
				depth++
			case xml.EndElement:
				depth--
			case xml.CharData:
				text.write(value)
			}
		}
		if name == "" {
			continue
		}
		if text.overflow {
			c.warn("value_too_large", "office.custom", name, "embedded custom property was omitted")
			continue
		}
		key := "office.custom." + canonicalFieldName(name)
		if document.SourceMetadataCanonicalKeyAllowed(key) {
			c.string(key, "office.custom", name, text.string(), false)
		} else if !validSourceMetadataLabel(name) || len(key) > document.MaxSourceMetadataLabelBytes {
			c.warn("invalid_label", "office.custom", name, "embedded custom property with an invalid label was omitted")
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
		c.strings("email.received", "email", "Received", values, false)
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
		parts := strings.Split(name, ";")
		base := strings.ToUpper(parts[0])
		hasNamedTimezone := false
		for _, parameter := range parts[1:] {
			parameterName, timezone, found := strings.Cut(parameter, "=")
			if found && strings.EqualFold(parameterName, "TZID") && timezone != "" {
				hasNamedTimezone = true
				break
			}
		}
		switch base {
		case "SUMMARY":
			c.string("title", "calendar", name, value, false)
		case "DESCRIPTION":
			c.string("description", "calendar", name, value, false)
		case "DTSTART":
			if hasNamedTimezone {
				c.string("calendar.start.raw", "calendar", name, value, false)
				c.warn("unsupported_timezone", "calendar", name, "named calendar timezone prevented timestamp normalization")
			} else {
				c.timestamp("calendar.start", "calendar", name, value)
			}
		case "DTEND":
			if hasNamedTimezone {
				c.string("calendar.end.raw", "calendar", name, value, false)
				c.warn("unsupported_timezone", "calendar", name, "named calendar timezone prevented timestamp normalization")
			} else {
				c.timestamp("calendar.end", "calendar", name, value)
			}
		case "ORGANIZER":
			c.strings("creators", "calendar", name, []string{value}, false)
		}
	}
}

func (c *metadataCollector) extractID3(data []byte) {
	if len(data) < 10 || data[3] < 3 || data[3] > 4 || data[5]&0xc0 != 0 {
		return
	}
	tagSize := synchsafeInt(data[6:10])
	if tagSize > len(data)-10 {
		return
	}
	tagEnd := 10 + tagSize
	for offset := 10; offset+10 <= tagEnd; {
		id := string(data[offset : offset+4])
		if strings.Trim(id, "\x00") == "" {
			return
		}
		if strings.IndexFunc(id, func(value rune) bool {
			return (value < 'A' || value > 'Z') && (value < '0' || value > '9')
		}) >= 0 {
			return
		}
		size := int(binary.BigEndian.Uint32(data[offset+4 : offset+8]))
		if data[3] == 4 {
			size = synchsafeInt(data[offset+4 : offset+8])
		}
		unsupportedFlags := data[offset+9]
		offset += 10
		if size < 1 || size > tagEnd-offset {
			return
		}
		payload := data[offset : offset+size]
		offset += size
		if unsupportedFlags != 0 {
			continue
		}
		value, ok := decodeID3Text(data[3], payload)
		if ok {
			c.addID3Frame(id, value)
		}
	}
}

func decodeID3Text(version byte, payload []byte) (string, bool) {
	if len(payload) < 2 {
		return "", false
	}
	var value string
	switch payload[0] {
	case 0:
		runes := make([]rune, len(payload)-1)
		for index, character := range payload[1:] {
			runes[index] = rune(character)
		}
		value = string(runes)
	case 1:
		var ok bool
		value, ok = decodeID3UTF16(payload[1:], true)
		if !ok {
			return "", false
		}
	case 2:
		if version != 4 {
			return "", false
		}
		var ok bool
		value, ok = decodeID3UTF16(payload[1:], false)
		if !ok {
			return "", false
		}
	case 3:
		if version != 4 || !utf8.Valid(payload[1:]) {
			return "", false
		}
		value = string(payload[1:])
	default:
		return "", false
	}
	value = strings.Trim(value, "\x00")
	value = strings.ReplaceAll(value, "\x00", "; ")
	return value, value != ""
}

func decodeID3UTF16(data []byte, withBOM bool) (string, bool) {
	var order binary.ByteOrder = binary.BigEndian
	if withBOM {
		if len(data) < 2 {
			return "", false
		}
		switch {
		case bytes.Equal(data[:2], []byte{0xfe, 0xff}):
		case bytes.Equal(data[:2], []byte{0xff, 0xfe}):
			order = binary.LittleEndian
		default:
			return "", false
		}
		data = data[2:]
	}
	if len(data)%2 != 0 {
		return "", false
	}
	units := make([]uint16, len(data)/2)
	for index := range units {
		units[index] = order.Uint16(data[index*2:])
	}
	for index, unit := range units {
		if unit >= 0xd800 && unit <= 0xdbff {
			if index+1 >= len(units) || units[index+1] < 0xdc00 || units[index+1] > 0xdfff {
				return "", false
			}
		} else if unit >= 0xdc00 && unit <= 0xdfff && (index == 0 || units[index-1] < 0xd800 || units[index-1] > 0xdbff) {
			return "", false
		}
	}
	return string(utf16.Decode(units)), true
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
		c.strings("creators", "media.id3", "TPE1", splitValues(value), false)
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
	text := string(data)
	for _, item := range []struct {
		marker, key string
		sensitive   bool
	}{{"ImageDescription=", "description", false}, {"Artist=", "creators", false}, {"GPSLatitude=", "image.exif.gps_latitude", true}, {"GPSLongitude=", "image.exif.gps_longitude", true}} {
		if index := strings.Index(text, item.marker); index >= 0 {
			value := text[index+len(item.marker):]
			if end := strings.IndexAny(value, "\x00\r\n;"); end >= 0 {
				value = value[:end]
			}
			if item.key == "creators" {
				c.strings(item.key, "image.exif", strings.TrimSuffix(item.marker, "="), splitValues(value), item.sensitive)
			} else {
				c.string(item.key, "image.exif", strings.TrimSuffix(item.marker, "="), value, item.sensitive)
			}
		}
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
		c.strings("creators", "image.exif", "Artist", splitValues(artist), false)
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
		parsed, err := time.Parse("20060102", value)
		if err != nil {
			return document.SourceMetadataTimestampV1{}, false
		}
		return document.SourceMetadataTimestampV1{Raw: raw, Normalized: parsed.Format("2006-01-02"), Precision: document.SourceMetadataPrecisionDate, Timezone: document.SourceMetadataTimezoneOmitted}, true
	}
	if len(value) == 15 && value[8] == 'T' {
		value = value[:4] + "-" + value[4:6] + "-" + value[6:8] + "T" + value[9:11] + ":" + value[11:13] + ":" + value[13:15]
	}
	if len(value) == 16 && strings.HasSuffix(value, "Z") && value[8] == 'T' {
		value = value[:4] + "-" + value[4:6] + "-" + value[6:8] + "T" + value[9:11] + ":" + value[11:13] + ":" + value[13:]
	}
	if _, err := time.Parse(time.RFC3339Nano, value); err == nil {
		kind := document.SourceMetadataTimezoneOffset
		off := value[len(value)-6:]
		if strings.HasSuffix(value, "Z") {
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
