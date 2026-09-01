package processing

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"image/color"
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/media/mediatest"
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
		{name: "JPEG EXIF", payload: syntheticExifJPEG(), keys: []string{"creators", "description", "media.container.animated", "media.container.format", "media.container.frame_count", "media.container.height_px", "media.container.kind", "media.container.width_px"}},
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

func TestExtractSourceMetadataReadsVisualContainerFacts(t *testing.T) {
	for _, testCase := range []struct {
		name       string
		payload    []byte
		kind       string
		width      int64
		height     int64
		frames     int64
		durationMS int64
		animated   bool
	}{
		{name: "PNG", payload: mediatest.PNG(12, 8, color.Black), kind: "image", width: 12, height: 8, frames: 1},
		{name: "animated GIF", payload: mediatest.GIF(16, 10, 2), kind: "image", width: 16, height: 10, frames: 2, animated: true},
		{name: "WebP", payload: mediatest.WebP(20, 14), kind: "image", width: 20, height: 14, frames: 1},
		{name: "MP4", payload: mediatest.MP4(640, 368, 3500), kind: "video", width: 640, height: 368, durationMS: 3500},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			metadata := ExtractSourceMetadata(testCase.payload)
			kind, found := sourceMetadataString(metadata, "media.container.kind")
			require.True(t, found)
			assert.Equal(t, testCase.kind, kind)
			width, found := sourceMetadataInteger(metadata, "media.container.width_px")
			require.True(t, found)
			assert.Equal(t, testCase.width, width)
			height, found := sourceMetadataInteger(metadata, "media.container.height_px")
			require.True(t, found)
			assert.Equal(t, testCase.height, height)
			if testCase.frames > 0 {
				frames, found := sourceMetadataInteger(metadata, "media.container.frame_count")
				require.True(t, found)
				assert.Equal(t, testCase.frames, frames)
				animated, found := sourceMetadataBoolean(metadata, "media.container.animated")
				require.True(t, found)
				assert.Equal(t, testCase.animated, animated)
			}
			if testCase.durationMS > 0 {
				duration, found := sourceMetadataInteger(metadata, "media.container.duration_ms")
				require.True(t, found)
				assert.Equal(t, testCase.durationMS, duration)
			}
		})
	}
}

func TestExtractSourceMetadataReadsTIFFPhotoFacts(t *testing.T) {
	metadata := ExtractSourceMetadata(syntheticRichExifTIFF())
	for key, want := range map[string]string{
		"media.container.format":  "tiff",
		"media.container.kind":    "image",
		"image.exif.camera_make":  "Fiction Camera Co.",
		"image.exif.camera_model": "Model One",
		"image.exif.lens_make":    "Fiction Lens Co.",
		"image.exif.lens_model":   "Prime 50mm",
	} {
		value, found := sourceMetadataString(metadata, key)
		require.True(t, found, key)
		assert.Equal(t, want, value, key)
	}
	for key, want := range map[string]int64{
		"media.container.width_px":  6000,
		"media.container.height_px": 4000,
		"image.exif.orientation":    6,
		"image.exif.iso":            800,
		"image.exif.pixel_width":    6000,
		"image.exif.pixel_height":   4000,
	} {
		value, found := sourceMetadataInteger(metadata, key)
		require.True(t, found, key)
		assert.Equal(t, want, value, key)
	}
	for key, want := range map[string]float64{
		"image.exif.exposure_time_seconds": 0.004,
		"image.exif.f_number":              2.8,
		"image.exif.exposure_bias_ev":      -1.0 / 3.0,
		"image.exif.focal_length_mm":       50,
	} {
		value, found := sourceMetadataNumber(metadata, key)
		require.True(t, found, key)
		assert.InDelta(t, want, value, 0.0000001, key)
	}
	created, found := sourceMetadataTimestamp(metadata, "created")
	require.True(t, found)
	assert.Equal(t, "2024:01:02 03:04:05", created.Raw)
}

func TestExtractSourceMetadataReadsRawTIFFVariants(t *testing.T) {
	for _, testCase := range []struct {
		name, format string
		magic        uint16
		standardISO  uint16
		rw2ISO       uint16
		wantISO      int64
	}{
		{name: "ORF original signature", format: "orf", magic: 0x4f52, standardISO: 640, wantISO: 640},
		{name: "ORF later signature", format: "orf", magic: 0x5352, standardISO: 640, wantISO: 640},
		{name: "RW2 fallback ISO", format: "rw2", magic: 0x0055, rw2ISO: 1250, wantISO: 1250},
		{name: "RW2 standard ISO wins", format: "rw2", magic: 0x0055, standardISO: 800, rw2ISO: 1250, wantISO: 800},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			root := []syntheticTIFFEntry{
				tiffLong(0x0100, 6000),
				tiffLong(0x0101, 4000),
				tiffASCII(0x010f, "Synthetic Camera Co."),
				tiffASCII(0x0110, "Model Raw"),
			}
			var exif []syntheticTIFFEntry
			if testCase.standardISO > 0 {
				exif = append(exif, tiffShort(0x8827, testCase.standardISO))
			}
			if testCase.rw2ISO > 0 {
				root = append(root, tiffShort(0x0017, testCase.rw2ISO))
			}
			metadata := ExtractSourceMetadata(syntheticTIFF(testCase.magic, root, exif))
			format, found := sourceMetadataString(metadata, "media.container.format")
			require.True(t, found)
			assert.Equal(t, testCase.format, format)
			model, found := sourceMetadataString(metadata, "image.exif.camera_model")
			require.True(t, found)
			assert.Equal(t, "Model Raw", model)
			iso, found := sourceMetadataInteger(metadata, "image.exif.iso")
			require.True(t, found)
			assert.Equal(t, testCase.wantISO, iso)
		})
	}
}

func TestExtractSourceMetadataReadsRAFPhotoFacts(t *testing.T) {
	metadata := ExtractSourceMetadata(syntheticRAF())
	for key, want := range map[string]string{
		"media.container.format":  "raf",
		"media.container.kind":    "image",
		"image.exif.camera_make":  "Fiction Camera Co.",
		"image.exif.camera_model": "Model One",
	} {
		value, found := sourceMetadataString(metadata, key)
		require.True(t, found, key)
		assert.Equal(t, want, value, key)
	}
	for key, want := range map[string]int64{
		"media.container.width_px":  6240,
		"media.container.height_px": 4160,
		"image.exif.iso":            800,
	} {
		value, found := sourceMetadataInteger(metadata, key)
		require.True(t, found, key)
		assert.Equal(t, want, value, key)
	}
	assert.Empty(t, metadata.Warnings)
}

func TestExtractSourceMetadataWarnsForMalformedRAFOffsets(t *testing.T) {
	payload := syntheticRAF()
	binary.BigEndian.PutUint32(payload[sourceMetadataRAFDirectoryOffset:], uint32(len(payload)+1))

	metadata := ExtractSourceMetadata(payload)
	assert.Contains(t, sourceMetadataWarningCodes(metadata), "unparseable_metadata")
	format, found := sourceMetadataString(metadata, "media.container.format")
	require.True(t, found)
	assert.Equal(t, "raf", format)
}

func TestExtractSourceMetadataKeepsRAFDirectoryInsideDeclaredBounds(t *testing.T) {
	payload := syntheticRAF()
	binary.BigEndian.PutUint32(payload[sourceMetadataRAFDirectoryLength:], 4)

	metadata := ExtractSourceMetadata(payload)
	assert.Contains(t, sourceMetadataWarningCodes(metadata), "unparseable_metadata")
	_, found := sourceMetadataInteger(metadata, "media.container.width_px")
	assert.False(t, found)
}

func TestExtractSourceMetadataRejectsOverlappingRAFMetadataRegions(t *testing.T) {
	payload := syntheticRAF()
	jpegOffset := int(binary.BigEndian.Uint32(payload[sourceMetadataRAFJPEGOffset:]))
	directoryOffset := jpegOffset + 2
	directory := append([]byte(nil), payload[len(payload)-12:]...)
	copy(payload[directoryOffset:], directory)
	binary.BigEndian.PutUint32(payload[sourceMetadataRAFDirectoryOffset:], uint32(directoryOffset))
	binary.BigEndian.PutUint32(payload[sourceMetadataRAFDirectoryLength:], uint32(len(directory)))

	metadata := ExtractSourceMetadata(payload)
	assert.Contains(t, sourceMetadataWarningCodes(metadata), "unparseable_metadata")
	_, found := sourceMetadataInteger(metadata, "media.container.width_px")
	assert.False(t, found)
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

func sourceMetadataNumber(metadata document.SourceMetadataV1, key string) (float64, bool) {
	for _, field := range metadata.Fields {
		if field.Key == key && field.Value.Number != nil {
			return *field.Value.Number, true
		}
	}
	return 0, false
}

func sourceMetadataBoolean(metadata document.SourceMetadataV1, key string) (bool, bool) {
	for _, field := range metadata.Fields {
		if field.Key == key && field.Value.Boolean != nil {
			return *field.Value.Boolean, true
		}
	}
	return false, false
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
	app1 := []byte{0xff, 0xe1, 0, 0}
	binary.BigEndian.PutUint16(app1[2:4], uint16(len(segment)+2))
	jpeg := mediatest.JPEG(40, 30, color.Black)
	result := append([]byte{}, jpeg[:2]...)
	result = append(result, app1...)
	result = append(result, segment...)
	return append(result, jpeg[2:]...)
}

func syntheticRAF() []byte {
	tiff := syntheticRichExifTIFF()
	segment := append([]byte("Exif\x00\x00"), tiff...)
	app1 := []byte{0xff, 0xe1, 0, 0}
	binary.BigEndian.PutUint16(app1[2:4], uint16(len(segment)+2))
	baseJPEG := mediatest.JPEG(40, 30, color.Black)
	jpeg := append([]byte{}, baseJPEG[:2]...)
	jpeg = append(jpeg, app1...)
	jpeg = append(jpeg, segment...)
	jpeg = append(jpeg, baseJPEG[2:]...)

	cfa := make([]byte, 12)
	binary.BigEndian.PutUint32(cfa[:4], 1)
	binary.BigEndian.PutUint16(cfa[4:6], sourceMetadataRAFImageSizeTag)
	binary.BigEndian.PutUint16(cfa[6:8], 4)
	binary.BigEndian.PutUint16(cfa[8:10], 4160)
	binary.BigEndian.PutUint16(cfa[10:12], 6240)

	jpegOffset := sourceMetadataRAFHeaderBytes
	cfaOffset := jpegOffset + len(jpeg)
	payload := make([]byte, cfaOffset+len(cfa))
	copy(payload, sourceMetadataRAFSignature)
	binary.BigEndian.PutUint32(payload[sourceMetadataRAFJPEGOffset:], uint32(jpegOffset))
	binary.BigEndian.PutUint32(payload[sourceMetadataRAFJPEGOffset+4:], uint32(len(jpeg)))
	binary.BigEndian.PutUint32(payload[sourceMetadataRAFDirectoryOffset:], uint32(cfaOffset))
	binary.BigEndian.PutUint32(payload[sourceMetadataRAFDirectoryLength:], uint32(len(cfa)))
	copy(payload[jpegOffset:], jpeg)
	copy(payload[cfaOffset:], cfa)
	return payload
}

type syntheticTIFFEntry struct {
	tag   uint16
	kind  uint16
	value []byte
}

func syntheticRichExifTIFF() []byte {
	return syntheticTIFF(42,
		[]syntheticTIFFEntry{
			tiffLong(0x0100, 6000),
			tiffLong(0x0101, 4000),
			tiffASCII(0x010f, "Fiction Camera Co."),
			tiffASCII(0x0110, "Model One"),
			tiffShort(0x0112, 6),
		},
		[]syntheticTIFFEntry{
			tiffRational(0x829a, 1, 250, false),
			tiffRational(0x829d, 28, 10, false),
			tiffShort(0x8827, 800),
			tiffASCII(0x9003, "2024:01:02 03:04:05"),
			tiffRational(0x9204, -1, 3, true),
			tiffRational(0x920a, 50, 1, false),
			tiffLong(0xa002, 6000),
			tiffLong(0xa003, 4000),
			tiffASCII(0xa433, "Fiction Lens Co."),
			tiffASCII(0xa434, "Prime 50mm"),
		},
	)
}

func syntheticTIFF(magic uint16, root, exif []syntheticTIFFEntry) []byte {
	const headerSize = 8
	rootEntries := append([]syntheticTIFFEntry{}, root...)
	rootIFDSize := 2 + (len(rootEntries)+1)*12 + 4
	exifOffset := headerSize + rootIFDSize
	pointer := make([]byte, 4)
	binary.LittleEndian.PutUint32(pointer, uint32(exifOffset))
	rootEntries = append(rootEntries, syntheticTIFFEntry{tag: 0x8769, kind: 4, value: pointer})
	exifIFDSize := 2 + len(exif)*12 + 4
	externalSize := 0
	for _, entry := range append(append([]syntheticTIFFEntry{}, rootEntries...), exif...) {
		if len(entry.value) > 4 {
			externalSize += len(entry.value)
		}
	}
	tiff := make([]byte, exifOffset+exifIFDSize+externalSize)
	copy(tiff, "II")
	binary.LittleEndian.PutUint16(tiff[2:4], magic)
	binary.LittleEndian.PutUint32(tiff[4:8], headerSize)
	externalOffset := exifOffset + exifIFDSize
	writeSyntheticTIFFIFD(tiff, headerSize, rootEntries, &externalOffset)
	writeSyntheticTIFFIFD(tiff, exifOffset, exif, &externalOffset)
	return tiff
}

func writeSyntheticTIFFIFD(data []byte, offset int, entries []syntheticTIFFEntry, externalOffset *int) {
	binary.LittleEndian.PutUint16(data[offset:], uint16(len(entries)))
	for index, entry := range entries {
		base := offset + 2 + index*12
		binary.LittleEndian.PutUint16(data[base:], entry.tag)
		binary.LittleEndian.PutUint16(data[base+2:], entry.kind)
		width := map[uint16]int{2: 1, 3: 2, 4: 4, 5: 8, 10: 8}[entry.kind]
		binary.LittleEndian.PutUint32(data[base+4:], uint32(len(entry.value)/width))
		if len(entry.value) <= 4 {
			copy(data[base+8:base+12], entry.value)
			continue
		}
		binary.LittleEndian.PutUint32(data[base+8:], uint32(*externalOffset))
		copy(data[*externalOffset:], entry.value)
		*externalOffset += len(entry.value)
	}
}

func tiffASCII(tag uint16, value string) syntheticTIFFEntry {
	return syntheticTIFFEntry{tag: tag, kind: 2, value: append([]byte(value), 0)}
}

func tiffShort(tag, value uint16) syntheticTIFFEntry {
	encoded := make([]byte, 2)
	binary.LittleEndian.PutUint16(encoded, value)
	return syntheticTIFFEntry{tag: tag, kind: 3, value: encoded}
}

func tiffLong(tag uint16, value uint32) syntheticTIFFEntry {
	encoded := make([]byte, 4)
	binary.LittleEndian.PutUint32(encoded, value)
	return syntheticTIFFEntry{tag: tag, kind: 4, value: encoded}
}

func tiffRational(tag uint16, numerator, denominator int32, signed bool) syntheticTIFFEntry {
	encoded := make([]byte, 8)
	binary.LittleEndian.PutUint32(encoded, uint32(numerator))
	binary.LittleEndian.PutUint32(encoded[4:], uint32(denominator))
	kind := uint16(5)
	if signed {
		kind = 10
	}
	return syntheticTIFFEntry{tag: tag, kind: kind, value: encoded}
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

func TestLargeMP4SourceMetadataSkipsPayloadWithoutBufferingIt(t *testing.T) {
	reader := syntheticSparseLargeMP4()
	hasher := sha256.New()
	_, err := io.Copy(hasher, reader.clone())
	require.NoError(t, err)
	expected := hex.EncodeToString(hasher.Sum(nil))
	blobs := &largeSourceMetadataReaderStub{reader: reader}

	metadata, err := sourceMetadataForTarget(t.Context(), blobs, store.SourceMetadataTarget{
		SourceSHA256: expected, Size: reader.size,
	})
	require.NoError(t, err)
	assert.Equal(t, 0, blobs.streamCalls)
	assert.Equal(t, 1, blobs.seekCalls)
	width, found := sourceMetadataInteger(metadata, "media.container.width_px")
	require.True(t, found)
	assert.Equal(t, int64(640), width)
	duration, found := sourceMetadataInteger(metadata, "media.container.duration_ms")
	require.True(t, found)
	assert.Equal(t, int64(3500), duration)
	assert.NotContains(t, sourceMetadataWarningCodes(metadata), "input_too_large")
}

func TestLargeMP4SourceMetadataMalformedBoxWarnsInsteadOfRetrying(t *testing.T) {
	reader := syntheticSparseLargeMP4()
	binary.BigEndian.PutUint64(reader.segments[1].data[8:16], 8)
	hasher := sha256.New()
	_, err := io.Copy(hasher, reader.clone())
	require.NoError(t, err)
	blobs := &largeSourceMetadataReaderStub{reader: reader}

	metadata, err := sourceMetadataForTarget(t.Context(), blobs, store.SourceMetadataTarget{
		SourceSHA256: hex.EncodeToString(hasher.Sum(nil)), Size: reader.size,
	})
	require.NoError(t, err)
	assert.Contains(t, sourceMetadataWarningCodes(metadata), "unparseable_metadata")
	assert.Empty(t, metadata.Fields)
}

func TestLargeRAFSourceMetadataUsesBoundedContainerReads(t *testing.T) {
	reader := syntheticSparseLargeRAF()
	hasher := sha256.New()
	_, err := io.Copy(hasher, reader.clone())
	require.NoError(t, err)
	blobs := &largeSourceMetadataReaderStub{reader: reader}

	metadata, err := sourceMetadataForTarget(t.Context(), blobs, store.SourceMetadataTarget{
		SourceSHA256: hex.EncodeToString(hasher.Sum(nil)), Size: reader.size,
	})
	require.NoError(t, err)
	assert.Zero(t, blobs.streamCalls)
	assert.Equal(t, 1, blobs.seekCalls)
	format, found := sourceMetadataString(metadata, "media.container.format")
	require.True(t, found)
	assert.Equal(t, "raf", format)
	width, found := sourceMetadataInteger(metadata, "media.container.width_px")
	require.True(t, found)
	assert.Equal(t, int64(6240), width)
	model, found := sourceMetadataString(metadata, "image.exif.camera_model")
	require.True(t, found)
	assert.Equal(t, "Model One", model)
	assert.NotContains(t, sourceMetadataWarningCodes(metadata), "input_too_large")
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

func (s *sourceMetadataReaderStub) OpenSeekableContext(context.Context, string) (io.ReadSeekCloser, int64, error) {
	return &sourceMetadataSeekableReader{Reader: bytes.NewReader(s.payload)}, int64(len(s.payload)), nil
}

type sourceMetadataSeekableReader struct {
	*bytes.Reader
}

func (r *sourceMetadataSeekableReader) Close() error { return nil }

type largeSourceMetadataReaderStub struct {
	reader      *sparseSourceMetadataReader
	streamCalls int
	seekCalls   int
}

func (s *largeSourceMetadataReaderStub) OpenStreamContext(context.Context, string) (packstore.VerifiedReadCloser, int64, error) {
	s.streamCalls++
	return nil, 0, errors.New("large metadata must not open a buffered stream")
}

func (s *largeSourceMetadataReaderStub) OpenSeekableContext(context.Context, string) (io.ReadSeekCloser, int64, error) {
	s.seekCalls++
	return s.reader.clone(), s.reader.size, nil
}

type sparseSourceMetadataSegment struct {
	offset int64
	data   []byte
}

type sparseSourceMetadataReader struct {
	size     int64
	position int64
	segments []sparseSourceMetadataSegment
}

func syntheticSparseLargeMP4() *sparseSourceMetadataReader {
	metadata := mediatest.MP4(640, 368, 3500)
	ftypSize := int(binary.BigEndian.Uint32(metadata[:4]))
	ftyp := append([]byte(nil), metadata[:ftypSize]...)
	moov := append([]byte(nil), metadata[ftypSize:]...)
	mdatSize := int64(maxSourceMetadataOriginalBytes + 1024)
	mdatHeader := make([]byte, 16)
	binary.BigEndian.PutUint32(mdatHeader[:4], 1)
	copy(mdatHeader[4:8], "mdat")
	binary.BigEndian.PutUint64(mdatHeader[8:16], uint64(mdatSize))
	moovOffset := int64(len(ftyp)) + mdatSize
	return &sparseSourceMetadataReader{
		size: moovOffset + int64(len(moov)),
		segments: []sparseSourceMetadataSegment{
			{offset: 0, data: ftyp},
			{offset: int64(len(ftyp)), data: mdatHeader},
			{offset: moovOffset, data: moov},
		},
	}
}

func syntheticSparseLargeRAF() *sparseSourceMetadataReader {
	payload := syntheticRAF()
	return &sparseSourceMetadataReader{
		size: maxSourceMetadataOriginalBytes + 1024,
		segments: []sparseSourceMetadataSegment{
			{offset: 0, data: payload},
		},
	}
}

func (r *sparseSourceMetadataReader) clone() *sparseSourceMetadataReader {
	return &sparseSourceMetadataReader{size: r.size, segments: r.segments}
}

func (r *sparseSourceMetadataReader) Read(target []byte) (int, error) {
	n, err := r.ReadAt(target, r.position)
	r.position += int64(n)
	return n, err
}

func (r *sparseSourceMetadataReader) ReadAt(target []byte, offset int64) (int, error) {
	if offset < 0 || offset >= r.size {
		return 0, io.EOF
	}
	count := min(int64(len(target)), r.size-offset)
	clear(target[:count])
	end := offset + count
	for _, segment := range r.segments {
		segmentEnd := segment.offset + int64(len(segment.data))
		start := max(offset, segment.offset)
		stop := min(end, segmentEnd)
		if start < stop {
			copy(target[start-offset:stop-offset], segment.data[start-segment.offset:stop-segment.offset])
		}
	}
	if count < int64(len(target)) {
		return int(count), io.EOF
	}
	return int(count), nil
}

func (r *sparseSourceMetadataReader) Seek(offset int64, whence int) (int64, error) {
	position := offset
	switch whence {
	case io.SeekCurrent:
		position = r.position + offset
	case io.SeekEnd:
		position = r.size + offset
	case io.SeekStart:
	default:
		return 0, errors.New("invalid seek origin")
	}
	if position < 0 {
		return 0, errors.New("negative seek position")
	}
	r.position = position
	return position, nil
}

func (r *sparseSourceMetadataReader) Close() error { return nil }

type verifiedSourceMetadataReader struct {
	*bytes.Reader

	closeErr error
}

func (r *verifiedSourceMetadataReader) Close() error   { return r.closeErr }
func (r *verifiedSourceMetadataReader) Verified() bool { return r.closeErr == nil }
func (r *verifiedSourceMetadataReader) Verify() error  { return r.closeErr }

var _ packstore.VerifiedReadCloser = (*verifiedSourceMetadataReader)(nil)
