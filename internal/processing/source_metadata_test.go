package processing

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/store"
	"go.kenn.io/kit/packstore"
)

func TestExtractSourceMetadataFromSyntheticFormats(t *testing.T) {
	ooxml := syntheticOOXML(t)
	for _, testCase := range []struct {
		name      string
		payload   []byte
		keys      []string
		sensitive string
	}{
		{name: "PDF info and XMP", payload: syntheticMetadataPDF(9), keys: []string{"created", "creators", "description", "keywords", "modified", "page_count", "subject", "title"}},
		{name: "OOXML core, app, and custom", payload: ooxml, keys: []string{"created", "office.core.word_count", "office.custom.matter_number", "page_count", "title"}},
		{name: "RFC 5322 email", payload: []byte("From: Ada <ada@example.test>\r\nTo: Grace <grace@example.test>\r\nBcc: Private <private@example.test>\r\nSubject: Synthetic mail\r\nDate: Tue, 2 Jan 2024 03:04:05 -0700\r\n\r\nbody"), keys: []string{"email.bcc", "email.from", "email.sent", "email.subject", "email.to"}, sensitive: "email.bcc"},
		{name: "iCalendar", payload: []byte("BEGIN:VCALENDAR\r\nBEGIN:VEVENT\r\nSUMMARY:Synthetic meeting\r\nDTSTART:20240102T030405\r\nDTEND:20240102T040405\r\nEND:VEVENT\r\nEND:VCALENDAR"), keys: []string{"calendar.end", "calendar.start", "title"}},
		{name: "JPEG EXIF", payload: syntheticExifJPEG(), keys: []string{"creators", "description"}},
		{name: "ID3 media tags", payload: syntheticID3Tag(4,
			syntheticID3Frame{id: "TIT2", encoding: 3, text: []byte("Synthetic song")},
			syntheticID3Frame{id: "TPE1", encoding: 3, text: []byte("Ada")},
			syntheticID3Frame{id: "TALB", encoding: 3, text: []byte("Synthetic album")}),
			keys: []string{"creators", "media.id3.album", "title"}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			metadata := ExtractSourceMetadata(testCase.payload)
			keys := make([]string, 0, len(metadata.Fields))
			sensitive := map[string]bool{}
			for _, field := range metadata.Fields {
				keys = append(keys, field.Key)
				sensitive[field.Key] = field.Sensitive
			}
			assert.ElementsMatch(t, testCase.keys, keys)
			if testCase.sensitive != "" {
				assert.True(t, sensitive[testCase.sensitive])
			}
			canonical, _, err := document.MarshalSourceMetadataV1(metadata)
			require.NoError(t, err)
			assert.NotContains(t, string(canonical), "source_path")
		})
	}
}

func TestExtractSourceMetadataReadsOOXMLAppProperties(t *testing.T) {
	metadata := ExtractSourceMetadata(syntheticOOXML(t))
	pages, found := sourceMetadataInteger(metadata, "page_count")
	require.True(t, found)
	assert.Equal(t, int64(7), pages)
	words, found := sourceMetadataInteger(metadata, "office.core.word_count")
	require.True(t, found)
	assert.Equal(t, int64(321), words)
}

func TestExtractSourceMetadataUsesVerifiedPDFPageTree(t *testing.T) {
	metadata := ExtractSourceMetadata(syntheticMetadataPDF(2))
	pageCount, found := sourceMetadataInteger(metadata, "page_count")
	require.True(t, found)
	assert.Equal(t, int64(2), pageCount,
		"an unrelated earlier /Count must not override the catalog page tree")

	malformed := ExtractSourceMetadata([]byte("%PDF-1.7 /Count 99"))
	_, found = sourceMetadataInteger(malformed, "page_count")
	assert.False(t, found)
	assert.Contains(t, sourceMetadataWarningCodes(malformed), "unparseable_pdf_pages")
}

func TestExtractSourceMetadataUsesAuthoritativePDFInfo(t *testing.T) {
	metadata := ExtractSourceMetadata(syntheticMetadataPDF(1))
	title, found := sourceMetadataString(metadata, "title")
	require.True(t, found)
	assert.Equal(t, "Quarterly report", title)
}

func TestExtractSourceMetadataPreservesPDFPagesWhenInfoFieldIsMalformed(t *testing.T) {
	metadata := ExtractSourceMetadata(syntheticMetadataPDFWithInfo(2,
		"<< /Title 42 /Author (Ada) /Subject (Synthetic) >>"))
	pageCount, found := sourceMetadataInteger(metadata, "page_count")
	require.True(t, found)
	assert.Equal(t, int64(2), pageCount)
	creators, found := sourceMetadataStrings(metadata, "creators")
	require.True(t, found)
	assert.Equal(t, []string{"Ada"}, creators)
	assert.Contains(t, sourceMetadataWarningCodes(metadata), "unparseable_pdf_metadata")
	assert.NotContains(t, sourceMetadataWarningCodes(metadata), "unparseable_pdf_pages")
}

func TestExtractSourceMetadataPreservesPDFPagesWhenXMPIsMalformed(t *testing.T) {
	metadata := ExtractSourceMetadata(syntheticMetadataPDFWithInfoAndMetadata(2,
		"<< /Title (Quarterly report) >>",
		"<< /Type /Metadata /Subtype /XML /Filter /FlateDecode /Length 4 >>\nstream\nnope\nendstream"))
	pageCount, found := sourceMetadataInteger(metadata, "page_count")
	require.True(t, found)
	assert.Equal(t, int64(2), pageCount)
	title, found := sourceMetadataString(metadata, "title")
	require.True(t, found)
	assert.Equal(t, "Quarterly report", title)
	assert.Contains(t, sourceMetadataWarningCodes(metadata), "unparseable_pdf_metadata")
	assert.NotContains(t, sourceMetadataWarningCodes(metadata), "unparseable_pdf_pages")
}

func TestExtractID3TextEncodingsAndFrameBoundary(t *testing.T) {
	for _, testCase := range []struct {
		name     string
		version  byte
		encoding byte
		text     []byte
		want     string
	}{
		{name: "Latin-1", version: 3, encoding: 0, text: []byte{'C', 'a', 'f', 0xe9}, want: "Café"},
		{name: "UTF-16 with BOM", version: 3, encoding: 1, text: []byte{0xfe, 0xff, 0x00, 'C', 0x00, 'a', 0x00, 'f', 0x00, 0xe9}, want: "Café"},
		{name: "UTF-16BE", version: 4, encoding: 2, text: []byte{0x00, 'C', 0x00, 'a', 0x00, 'f', 0x00, 0xe9}, want: "Café"},
		{name: "UTF-8", version: 4, encoding: 3, text: []byte("Café"), want: "Café"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			metadata := ExtractSourceMetadata(syntheticID3TextTag(testCase.version, testCase.encoding, testCase.text))
			title, found := sourceMetadataString(metadata, "title")
			require.True(t, found)
			assert.Equal(t, testCase.want, title)
		})
	}

	metadata := ExtractSourceMetadata(append(syntheticID3TextTag(4, 3, nil), []byte("TIT2\x00Decoy audio bytes")...))
	_, found := sourceMetadataString(metadata, "title")
	assert.False(t, found)
}

func TestExtractXMLTextDoesNotCopyWrittenFrames(t *testing.T) {
	collector := metadataCollector{record: new(emptySourceMetadata()), seen: map[string]bool{}}
	require.NotPanics(t, func() {
		collector.extractXMLText([]byte(`<root>prefix<group><title>Synthetic</title></group></root>`), "office.core")
	})
	title, found := sourceMetadataString(*collector.record, "title")
	require.True(t, found)
	assert.Equal(t, "Synthetic", title)

	metadata := emptySourceMetadata()
	collector = metadataCollector{record: &metadata, seen: map[string]bool{}}
	collector.extractXMLText([]byte(`<root><title>`+strings.Repeat("x", document.MaxSourceMetadataValueBytes+1)+`</title></root>`), "office.core")
	_, found = sourceMetadataString(metadata, "title")
	assert.False(t, found)
	assert.Contains(t, sourceMetadataWarningCodes(metadata), "value_too_large")
}

func TestExtractXMLTextReadsXMPAttributesAndRDFCollections(t *testing.T) {
	const xmp = `<x:xmpmeta xmlns:x="adobe:ns:meta/" xmlns:rdf="http://www.w3.org/1999/02/22-rdf-syntax-ns#" xmlns:dc="http://purl.org/dc/elements/1.1/" xmlns:xmp="http://ns.adobe.com/xap/1.0/">
		<rdf:RDF><rdf:Description xmp:CreateDate="2024-01-02T03:04:05Z">
			<dc:title><rdf:Alt><rdf:li xml:lang="fr">Rapport</rdf:li><rdf:li xml:lang="x-default">Synthetic report</rdf:li></rdf:Alt></dc:title>
			<dc:creator><rdf:Seq><rdf:li>Ada</rdf:li><rdf:li>Grace</rdf:li></rdf:Seq></dc:creator>
		</rdf:Description></rdf:RDF>
	</x:xmpmeta>`

	t.Run("attribute", func(t *testing.T) {
		metadata := emptySourceMetadata()
		collector := metadataCollector{record: &metadata, seen: map[string]bool{}}
		collector.extractXMLText([]byte(xmp), "xmp")
		created, found := sourceMetadataTimestamp(metadata, "created")
		require.True(t, found)
		assert.Equal(t, "2024-01-02T03:04:05Z", created.Raw)
	})
	t.Run("collection", func(t *testing.T) {
		metadata := emptySourceMetadata()
		collector := metadataCollector{record: &metadata, seen: map[string]bool{}}
		collector.extractXMLText([]byte(xmp), "xmp")
		title, found := sourceMetadataString(metadata, "title")
		require.True(t, found)
		assert.Equal(t, "Synthetic report", title)
		creators, found := sourceMetadataStrings(metadata, "creators")
		require.True(t, found)
		assert.Equal(t, []string{"Ada", "Grace"}, creators)
	})
}

func TestExtractXMLTextSkipsXMPStructureAndUnknownNamespaces(t *testing.T) {
	t.Run("RDF structure", func(t *testing.T) {
		metadata := emptySourceMetadata()
		collector := metadataCollector{record: &metadata, seen: map[string]bool{}}
		collector.extractXMLText([]byte(`<rdf:RDF xmlns:rdf="http://www.w3.org/1999/02/22-rdf-syntax-ns#" xmlns:dc="http://purl.org/dc/elements/1.1/"><rdf:Description><dc:title>Synthetic title</dc:title></rdf:Description></rdf:RDF>`), "xmp")
		_, found := sourceMetadataString(metadata, "description")
		assert.False(t, found)
	})
	t.Run("unknown property namespace", func(t *testing.T) {
		metadata := emptySourceMetadata()
		collector := metadataCollector{record: &metadata, seen: map[string]bool{}}
		collector.extractXMLText([]byte(`<rdf:RDF xmlns:rdf="http://www.w3.org/1999/02/22-rdf-syntax-ns#" xmlns:unknown="https://example.test/xmp"><rdf:Description><unknown:title>Fabricated title</unknown:title></rdf:Description></rdf:RDF>`), "xmp")
		_, found := sourceMetadataString(metadata, "title")
		assert.False(t, found)
	})
}

func TestExtractImageIgnoresMetadataLikeEntropyBytes(t *testing.T) {
	data := append([]byte{0xff, 0xd8, 0xff, 0xda, 0x00, 0x02}, []byte("ImageDescription=Fabricated entropy\x00")...)
	data = append(data, 0xff, 0xd9)
	metadata := ExtractSourceMetadata(data)
	_, found := sourceMetadataString(metadata, "description")
	assert.False(t, found)
}

func TestMalformedXMLMetadataEmitsWarnings(t *testing.T) {
	t.Run("generic", func(t *testing.T) {
		metadata := emptySourceMetadata()
		collector := metadataCollector{record: &metadata, seen: map[string]bool{}}
		collector.extractXMLText([]byte(`<root><title>Partial title</title><description>`), "xmp")
		assert.Contains(t, sourceMetadataWarningCodes(metadata), "malformed_metadata")
	})
	t.Run("office custom", func(t *testing.T) {
		metadata := emptySourceMetadata()
		collector := metadataCollector{record: &metadata, seen: map[string]bool{}}
		collector.extractOfficeCustom([]byte(`<Properties><property name="Matter Number"><lpwstr>MAT-001`))
		assert.Contains(t, sourceMetadataWarningCodes(metadata), "malformed_metadata")
	})
	t.Run("office app", func(t *testing.T) {
		metadata := emptySourceMetadata()
		collector := metadataCollector{record: &metadata, seen: map[string]bool{}}
		collector.extractOfficeApp([]byte(`<Properties><Pages>7</Pages>`))
		assert.Contains(t, sourceMetadataWarningCodes(metadata), "malformed_metadata")
	})
}

func TestExtractCalendarRetainsUnsupportedNamedTimezone(t *testing.T) {
	metadata := ExtractSourceMetadata([]byte("BEGIN:VCALENDAR\r\nBEGIN:VEVENT\r\nDTSTART;TZID=America/New_York:20240102T030405\r\nEND:VEVENT\r\nEND:VCALENDAR"))
	raw, found := sourceMetadataString(metadata, "calendar.start.raw")
	require.True(t, found)
	assert.Equal(t, "20240102T030405", raw)
	_, found = sourceMetadataTimestamp(metadata, "calendar.start")
	assert.False(t, found)
	assert.Contains(t, sourceMetadataWarningCodes(metadata), "unsupported_timezone")
}

func TestMetadataCollectorBoundsLabelsAndAggregateValues(t *testing.T) {
	metadata := emptySourceMetadata()
	collector := metadataCollector{record: &metadata, seen: map[string]bool{}}
	collector.extractOfficeCustom([]byte(`<Properties><property name="` +
		strings.Repeat("a", 257) + `"><lpwstr>value</lpwstr></property></Properties>`))
	for index := range 70 {
		collector.string(fmt.Sprintf("office.custom.field_%d", index), "office.custom",
			fmt.Sprintf("Field%d", index), strings.Repeat(`"`, document.MaxSourceMetadataValueBytes), false)
	}
	_, _, err := document.MarshalSourceMetadataV1(metadata)
	require.NoError(t, err)
	assert.Contains(t, sourceMetadataWarningCodes(metadata), "invalid_label")
	assert.Contains(t, sourceMetadataWarningCodes(metadata), "aggregate_value_limit")
}

func TestCanonicalSourceMetadataResultPublishesWarningForInvalidExtraction(t *testing.T) {
	metadata := emptySourceMetadata()
	metadata.Fields = append(metadata.Fields, document.SourceMetadataFieldV1{
		Key: "forbidden", Namespace: "xmp", SourceField: "Synthetic",
		Value: document.SourceMetadataValueV1{Kind: document.SourceMetadataString, String: "value"},
	})

	bounded := canonicalSourceMetadataResult(metadata)
	canonical, _, err := document.MarshalSourceMetadataV1(bounded)
	require.NoError(t, err)
	assert.Empty(t, bounded.Fields)
	assert.Equal(t, []string{"extraction_limit"}, sourceMetadataWarningCodes(bounded))
	assert.NotEmpty(t, canonical)
}

func TestExtractSourceMetadataParsesMultipartAttachmentHeaders(t *testing.T) {
	payload := strings.Join([]string{
		"From: Ada <ada@example.test>",
		"Subject: Attachments",
		`Content-Type: multipart/mixed; boundary="synthetic-boundary"`,
		"",
		"--synthetic-boundary",
		"Content-Type: text/plain",
		"",
		"literal Content-Disposition: attachment is not a MIME part header",
		"--synthetic-boundary",
		"content-disposition: ATTACHMENT; filename=one.txt",
		"",
		"one",
		"--synthetic-boundary",
		"Content-Disposition:",
		" attachment; filename=two.txt",
		"",
		"two",
		"--synthetic-boundary--",
		"",
	}, "\r\n")
	metadata := ExtractSourceMetadata([]byte(payload))
	count, found := sourceMetadataInteger(metadata, "attachment_count")
	require.True(t, found)
	assert.Equal(t, int64(2), count)
}

func sourceMetadataInteger(metadata document.SourceMetadataV1, key string) (int64, bool) {
	for _, field := range metadata.Fields {
		if field.Key == key && field.Value.Integer != nil {
			return *field.Value.Integer, true
		}
	}
	return 0, false
}

func sourceMetadataString(metadata document.SourceMetadataV1, key string) (string, bool) {
	for _, field := range metadata.Fields {
		if field.Key == key && field.Value.Kind == document.SourceMetadataString {
			return field.Value.String, true
		}
	}
	return "", false
}

func sourceMetadataStrings(metadata document.SourceMetadataV1, key string) ([]string, bool) {
	for _, field := range metadata.Fields {
		if field.Key == key && field.Value.Kind == document.SourceMetadataStringList {
			return field.Value.Strings, true
		}
	}
	return nil, false
}

func sourceMetadataTimestamp(metadata document.SourceMetadataV1, key string) (document.SourceMetadataTimestampV1, bool) {
	for _, field := range metadata.Fields {
		if field.Key == key && field.Value.Timestamp != nil {
			return *field.Value.Timestamp, true
		}
	}
	return document.SourceMetadataTimestampV1{}, false
}

func sourceMetadataWarningCodes(metadata document.SourceMetadataV1) []string {
	codes := make([]string, 0, len(metadata.Warnings))
	for _, warning := range metadata.Warnings {
		codes = append(codes, warning.Code)
	}
	return codes
}

func syntheticMetadataPDF(pageCount int) []byte {
	return syntheticMetadataPDFWithInfo(pageCount,
		"<< /Title (Quarterly report) /Author (Ada; Grace) /Subject (Synthetic) /Keywords (one,two) /CreationDate (D:20240102030405) >>")
}

func syntheticMetadataPDFWithInfo(pageCount int, info string) []byte {
	xmp := `<x:xmpmeta xmlns:x="adobe:ns:meta/" xmlns:rdf="http://www.w3.org/1999/02/22-rdf-syntax-ns#" xmlns:dc="http://purl.org/dc/elements/1.1/" xmlns:xmp="http://ns.adobe.com/xap/1.0/"><rdf:RDF><rdf:Description xmp:ModifyDate="2024-01-03T04:05:06Z"><dc:description><rdf:Alt><rdf:li xml:lang="x-default">Synthetic PDF description</rdf:li></rdf:Alt></dc:description></rdf:Description></rdf:RDF></x:xmpmeta>`
	return syntheticMetadataPDFWithInfoAndMetadata(pageCount, info,
		fmt.Sprintf("<< /Type /Metadata /Subtype /XML /Length %d >>\nstream\n%s\nendstream", len(xmp), xmp))
}

func syntheticMetadataPDFWithInfoAndMetadata(pageCount int, info, metadata string) []byte {
	kids := make([]string, 0, pageCount)
	objects := []string{
		"",
		"",
	}
	for index := range pageCount {
		kids = append(kids, fmt.Sprintf("%d 0 R", index+3))
		objects = append(objects, fmt.Sprintf(
			"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Label (page-%d) >>", index+1))
	}
	objects[1] = fmt.Sprintf("<< /Type /Pages /Kids [%s] /Count %d >>", strings.Join(kids, " "), pageCount)
	objects = append(objects, "<< /Length 14 >>\nstream\n/Title (Decoy)\nendstream")
	infoObject := len(objects) + 1
	objects = append(objects, info)
	metadataObject := len(objects) + 1
	objects = append(objects, metadata)
	objects[0] = fmt.Sprintf("<< /Type /Catalog /Pages 2 0 R /Metadata %d 0 R /Count 99 >>", metadataObject)
	var output bytes.Buffer
	_, _ = output.WriteString("%PDF-1.4\n")
	offsets := make([]int, len(objects))
	for index, object := range objects {
		offsets[index] = output.Len()
		_, _ = fmt.Fprintf(&output, "%d 0 obj\n%s\nendobj\n", index+1, object)
	}
	xref := output.Len()
	_, _ = fmt.Fprintf(&output, "xref\n0 %d\n0000000000 65535 f \n", len(objects)+1)
	for _, offset := range offsets {
		_, _ = fmt.Fprintf(&output, "%010d 00000 n \n", offset)
	}
	_, _ = fmt.Fprintf(&output,
		"trailer\n<< /Size %d /Root 1 0 R /Info %d 0 R >>\nstartxref\n%d\n%%%%EOF\n",
		len(objects)+1, infoObject, xref)
	return output.Bytes()
}

func syntheticID3TextTag(version, encoding byte, text []byte) []byte {
	return syntheticID3Tag(version, syntheticID3Frame{id: "TIT2", encoding: encoding, text: text})
}

func syntheticExifJPEG() []byte {
	description := []byte("Synthetic image\x00")
	artist := []byte("Ada\x00")
	const directoryEnd = 38
	tiff := make([]byte, directoryEnd+len(description)+len(artist))
	copy(tiff, "II")
	binary.LittleEndian.PutUint16(tiff[2:4], 42)
	binary.LittleEndian.PutUint32(tiff[4:8], 8)
	binary.LittleEndian.PutUint16(tiff[8:10], 2)
	for _, entry := range []struct {
		offset int
		tag    uint16
		value  []byte
		start  int
	}{
		{offset: 10, tag: 0x010e, value: description, start: directoryEnd},
		{offset: 22, tag: 0x013b, value: artist, start: directoryEnd + len(description)},
	} {
		binary.LittleEndian.PutUint16(tiff[entry.offset:], entry.tag)
		binary.LittleEndian.PutUint16(tiff[entry.offset+2:], 2)
		binary.LittleEndian.PutUint32(tiff[entry.offset+4:], uint32(len(entry.value)))
		binary.LittleEndian.PutUint32(tiff[entry.offset+8:], uint32(entry.start))
		copy(tiff[entry.start:], entry.value)
	}
	segment := append([]byte("Exif\x00\x00"), tiff...)
	jpeg := []byte{0xff, 0xd8, 0xff, 0xe1, 0, 0}
	binary.BigEndian.PutUint16(jpeg[4:6], uint16(len(segment)+2))
	jpeg = append(jpeg, segment...)
	return append(jpeg, 0xff, 0xd9)
}

type syntheticID3Frame struct {
	id       string
	encoding byte
	text     []byte
}

func syntheticID3Tag(version byte, frames ...syntheticID3Frame) []byte {
	var body bytes.Buffer
	for _, source := range frames {
		payload := append([]byte{source.encoding}, source.text...)
		frame := make([]byte, 10+len(payload))
		copy(frame, source.id)
		if version == 4 {
			putSynchsafe(frame[4:8], len(payload))
		} else {
			binary.BigEndian.PutUint32(frame[4:8], uint32(len(payload)))
		}
		copy(frame[10:], payload)
		_, _ = body.Write(frame)
	}
	tag := make([]byte, 10, 10+body.Len())
	copy(tag, "ID3")
	tag[3] = version
	putSynchsafe(tag[6:10], body.Len())
	return append(tag, body.Bytes()...)
}

func putSynchsafe(target []byte, value int) {
	target[0] = byte(value >> 21 & 0x7f)
	target[1] = byte(value >> 14 & 0x7f)
	target[2] = byte(value >> 7 & 0x7f)
	target[3] = byte(value & 0x7f)
}

func syntheticOOXML(t *testing.T) []byte {
	t.Helper()
	var output bytes.Buffer
	writer := zip.NewWriter(&output)
	core, err := writer.Create("docProps/core.xml")
	require.NoError(t, err)
	_, err = io.WriteString(core, `<cp:coreProperties xmlns:cp="urn:cp" xmlns:dc="urn:dc" xmlns:dcterms="urn:dcterms"><dc:title>Synthetic office</dc:title><dcterms:created>2024-01-02T03:04:05Z</dcterms:created></cp:coreProperties>`)
	require.NoError(t, err)
	custom, err := writer.Create("docProps/custom.xml")
	require.NoError(t, err)
	_, err = io.WriteString(custom, `<Properties><property name="Matter Number"><lpwstr>MAT-001</lpwstr></property></Properties>`)
	require.NoError(t, err)
	app, err := writer.Create("docProps/app.xml")
	require.NoError(t, err)
	_, err = io.WriteString(app, `<Properties><Pages>7</Pages><Words>321</Words></Properties>`)
	require.NoError(t, err)
	require.NoError(t, writer.Close())
	return output.Bytes()
}

func TestParseSourceTimestampPreservesExplicitZeroOffset(t *testing.T) {
	stamp, ok := parseSourceTimestamp("2024-01-02T03:04:05+00:00")
	require.True(t, ok)
	assert.Equal(t, document.SourceMetadataTimezoneOffset, stamp.Timezone)
	assert.Equal(t, "+00:00", stamp.Offset)
	assert.Equal(t, "2024-01-02T03:04:05+00:00", stamp.Normalized)

	record := document.SourceMetadataV1{ContractVersion: document.SourceMetadataContractV1,
		Fields: []document.SourceMetadataFieldV1{{Key: "created", Namespace: "xmp", SourceField: "CreateDate",
			Value: document.SourceMetadataValueV1{Kind: document.SourceMetadataTimestamp, Timestamp: &stamp}}}}
	_, _, err := document.MarshalSourceMetadataV1(record)
	require.NoError(t, err)
}

func TestInvalidCompactDateDoesNotDiscardOtherMetadata(t *testing.T) {
	metadata := emptySourceMetadata()
	collector := metadataCollector{record: &metadata, seen: map[string]bool{}}
	collector.string("title", "pdf.info", "Title", "Synthetic report", false)
	collector.timestamp("created", "pdf.info", "CreationDate", "D:20241399")
	metadata = canonicalSourceMetadataResult(metadata)

	title, found := sourceMetadataString(metadata, "title")
	require.True(t, found)
	assert.Equal(t, "Synthetic report", title)
	_, found = sourceMetadataTimestamp(metadata, "created")
	assert.False(t, found)
	assert.Contains(t, sourceMetadataWarningCodes(metadata), "unparseable_timestamp")
	assert.NotContains(t, sourceMetadataWarningCodes(metadata), "extraction_limit")
}

func TestBackfillSourceMetadataPublishesOnlyAfterVerifiedEOF(t *testing.T) {
	payload := []byte("%PDF-1.7 /Title (Verified report)")
	target := store.SourceMetadataTarget{SourceSHA256: processingHash("a1"), Size: int64(len(payload))}
	catalog := &sourceMetadataCatalogStub{targets: []store.SourceMetadataTarget{target}}
	reader := &sourceMetadataReaderStub{payload: payload, closeErr: errors.New("verification failed")}
	completed, err := BackfillSourceMetadata(t.Context(), catalog, reader, 10)
	require.ErrorContains(t, err, "verification failed")
	assert.Zero(t, completed)
	assert.Zero(t, catalog.published)
	reader.closeErr = nil
	completed, err = BackfillSourceMetadata(t.Context(), catalog, reader, 10)
	require.NoError(t, err)
	assert.Equal(t, 1, completed)
	assert.Equal(t, 1, catalog.published)
}

func TestBackfillSourceMetadataContinuesPastUnreadableTarget(t *testing.T) {
	payload := []byte("%PDF-1.7 /Title (Verified report)")
	targets := []store.SourceMetadataTarget{{SourceSHA256: processingHash("a1"), Size: int64(len(payload))}, {SourceSHA256: processingHash("b2"), Size: int64(len(payload))}}
	catalog := &sourceMetadataCatalogStub{targets: targets}
	reader := &sourceMetadataReaderStub{payload: payload, closeErrors: []error{errors.New("first corrupt"), nil}}
	completed, err := BackfillSourceMetadataTargets(t.Context(), catalog, reader, targets)
	require.ErrorContains(t, err, "first corrupt")
	assert.Equal(t, 1, completed)
	assert.Equal(t, 1, catalog.published)
}

type sourceMetadataCatalogStub struct {
	targets   []store.SourceMetadataTarget
	published int
}

func (s *sourceMetadataCatalogStub) MissingSourceMetadataTargets(context.Context, string, int) ([]store.SourceMetadataTarget, error) {
	return s.targets, nil
}
func (s *sourceMetadataCatalogStub) PublishSourceMetadata(context.Context, string, string, []byte) (store.SourceMetadataGeneration, error) {
	s.published++
	return store.SourceMetadataGeneration{}, nil
}

type sourceMetadataReaderStub struct {
	payload     []byte
	closeErr    error
	closeErrors []error
	calls       int
}

func (s *sourceMetadataReaderStub) OpenStreamContext(context.Context, string) (packstore.VerifiedReadCloser, int64, error) {
	closeErr := s.closeErr
	if s.calls < len(s.closeErrors) {
		closeErr = s.closeErrors[s.calls]
	}
	s.calls++
	return &verifiedSourceMetadataReader{Reader: bytes.NewReader(s.payload), closeErr: closeErr}, int64(len(s.payload)), nil
}

type verifiedSourceMetadataReader struct {
	*bytes.Reader

	closeErr error
}

func (r *verifiedSourceMetadataReader) Close() error   { return r.closeErr }
func (r *verifiedSourceMetadataReader) Verified() bool { return r.closeErr == nil }
func (r *verifiedSourceMetadataReader) Verify() error  { return r.closeErr }

var _ packstore.VerifiedReadCloser = (*verifiedSourceMetadataReader)(nil)
