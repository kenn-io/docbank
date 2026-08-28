package media_test

import (
	"archive/zip"
	"bytes"
	"compress/zlib"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json/v2"
	"fmt"
	"math"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/media"
	"go.kenn.io/docbank/document/media/mediatest"
)

func TestInspectBindsFiniteTextToPolicyAndSource(t *testing.T) {
	t.Parallel()
	data := []byte("alpha\nbeta\n")
	policy := inspectionPolicy(data, "notes.txt", "text/plain")
	record, err := media.InspectCapability(bytes.NewReader(data), policy)
	require.NoError(t, err)
	require.True(t, record.Eligible)
	assert.Equal(t, "text", record.MediaFamily)
	assert.Equal(t, "text/plain", record.MediaType)
	assert.Equal(t, int64(2), record.Measurements.TextLines)
	assert.Equal(t, int64(11), record.Measurements.Characters)
	assert.Equal(t, sha256Hex(data), record.SourceSHA256)
	assert.NotEmpty(t, record.PolicyFingerprint)
	assert.NotEmpty(t, record.Checksum)
	require.NoError(t, media.ValidateCapabilityRecord(record))

	mutated := record
	mutated.Measurements.TextLines++
	require.ErrorContains(t, media.ValidateCapabilityRecord(mutated), "checksum")

	encoded, err := json.Marshal(record, json.Deterministic(true))
	require.NoError(t, err)
	assert.Contains(t, string(encoded), `"max_text_lines":1000`)
	var decoded media.CapabilityRecord
	require.NoError(t, json.Unmarshal(encoded, &decoded))
	assert.Equal(t, policy, decoded.Policy)
	require.NoError(t, media.ValidateCapabilityRecord(decoded))
	_, local := decoded.InspectionPolicy()
	assert.False(t, local, "a portable record must not recreate local upload authority")
}

func TestInspectRejectsDeclaredSourceMismatchAndExcessiveUnits(t *testing.T) {
	t.Parallel()
	data := []byte("alpha\nbeta\n")
	policy := inspectionPolicy(data, "notes.txt", "text/plain")
	policy.ExpectedSHA256 = strings.Repeat("0", 64)
	_, err := media.InspectCapability(bytes.NewReader(data), policy)
	require.ErrorContains(t, err, "SHA-256")

	policy = inspectionPolicy(data, "notes.txt", "text/plain")
	policy.MaxTextLines = 1
	record, err := media.InspectCapability(bytes.NewReader(data), policy)
	require.NoError(t, err)
	assert.False(t, record.Eligible)
	assert.Equal(t, media.CapabilityReasonSemanticUnits, record.Reason)

	jsonl := []byte("{\"a\":1}\n{\"b\":2}\n")
	policy = inspectionPolicy(jsonl, "records.jsonl", "application/x-ndjson")
	policy.MaxRecords = 1
	record, err = media.InspectCapability(bytes.NewReader(jsonl), policy)
	require.NoError(t, err)
	assert.False(t, record.Eligible)
	assert.Equal(t, int64(2), record.Measurements.Records)
	assert.Equal(t, media.CapabilityReasonSemanticUnits, record.Reason)
}

func TestInspectBoundsZIPContainers(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		data   []byte
		policy func([]byte) media.InspectionPolicy
		reason media.CapabilityReason
	}{
		{
			name: "aggregate expansion",
			data: zipBytes(t, validPPTXEntries(zipEntry{name: "slides/one.xml", body: strings.Repeat("x", 32)})),
			policy: func(data []byte) media.InspectionPolicy {
				p := inspectionPolicy(data, "deck.pptx", "application/vnd.openxmlformats-officedocument.presentationml.presentation")
				p.MaxExpandedBytes = 16
				return p
			},
			reason: media.CapabilityReasonExpandedBytes,
		},
		{
			name: "nested archive",
			data: zipBytes(t, validPPTXEntries(zipEntry{name: "nested.zip", body: "PK\x03\x04nested"})),
			policy: func(data []byte) media.InspectionPolicy {
				return inspectionPolicy(data, "deck.pptx", "application/vnd.openxmlformats-officedocument.presentationml.presentation")
			},
			reason: media.CapabilityReasonNestedContainer,
		},
		{
			name: "external relationship",
			data: zipBytes(t, validPPTXEntries(zipEntry{name: "_rels/.rels", body: `<Relationships><Relationship TargetMode="External" Target="https://example.invalid"/></Relationships>`})),
			policy: func(data []byte) media.InspectionPolicy {
				return inspectionPolicy(data, "deck.pptx", "application/vnd.openxmlformats-officedocument.presentationml.presentation")
			},
			reason: media.CapabilityReasonExternalReference,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			record, err := media.InspectCapability(bytes.NewReader(tt.data), tt.policy(tt.data))
			require.NoError(t, err)
			assert.False(t, record.Eligible)
			assert.Equal(t, tt.reason, record.Reason)
			if tt.reason == media.CapabilityReasonNestedContainer {
				assert.Equal(t, int64(2), record.Measurements.NestingDepth)
			}
		})
	}
}

func TestInspectRejectsEncryptedZIPEntry(t *testing.T) {
	t.Parallel()
	data := zipBytes(t, validPPTXEntries(zipEntry{name: "slides/one.xml", body: "safe"}))
	// Both the local and central directory general-purpose flags carry bit 0.
	data[6] |= 1
	central := bytes.Index(data, []byte("PK\x01\x02"))
	require.NotEqual(t, -1, central)
	data[central+8] |= 1

	record, err := media.InspectCapability(bytes.NewReader(data), inspectionPolicy(data, "deck.pptx", "application/vnd.openxmlformats-officedocument.presentationml.presentation"))
	require.NoError(t, err)
	assert.False(t, record.Eligible)
	assert.Equal(t, media.CapabilityReasonEncryptedContainer, record.Reason)
}

func TestInspectRejectsMalformedAndExternallyReferentialContainerXML(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		body   string
		reason media.CapabilityReason
	}{
		{name: "malformed XML", body: `<worksheet><c></worksheet>`, reason: media.CapabilityReasonMalformed},
		{name: "multiple roots", body: `<worksheet/><worksheet/>`, reason: media.CapabilityReasonMalformed},
		{name: "external DTD", body: `<!DOCTYPE worksheet SYSTEM "https://example.invalid/sheet.dtd"><worksheet/>`, reason: media.CapabilityReasonExternalReference},
		{name: "public DTD", body: `<!DOCTYPE worksheet PUBLIC "-//EXAMPLE//DTD Sheet//EN" "sheet.dtd"><worksheet/>`, reason: media.CapabilityReasonExternalReference},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			data := zipBytes(t, validXLSXEntries(zipEntry{name: "xl/worksheets/sheet1.xml", body: tt.body}))
			record, err := media.InspectCapability(bytes.NewReader(data), inspectionPolicy(data, "book.xlsx",
				"application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"))
			require.NoError(t, err)
			assert.False(t, record.Eligible)
			assert.Equal(t, tt.reason, record.Reason)
		})
	}
}

func TestInspectRejectsExternalReferenceInEPUBContent(t *testing.T) {
	t.Parallel()
	content := []string{
		`<html xmlns="http://www.w3.org/1999/xhtml"><body><img src="https://example.invalid/tracker.png"/></body></html>`,
		`<html xmlns="http://www.w3.org/1999/xhtml" xml:base="https://example.invalid/"><body><img src="tracker.png"/></body></html>`,
		`<?xml-stylesheet href="https://example.invalid/book.css"?><html xmlns="http://www.w3.org/1999/xhtml"/>`,
		`<html xmlns="http://www.w3.org/1999/xhtml"><body><img srcset="cover.png 1x, https://example.invalid/cover.png 2x"/></body></html>`,
		`<html xmlns="http://www.w3.org/1999/xhtml"><body style="background: url(https://example.invalid/paper.png)"/></html>`,
	}
	for _, chapter := range content {
		data := zipBytes(t, validEPUBEntries(
			zipEntry{name: "OPS/content.opf", body: `<package><manifest><item id="chapter" href="chapter.xhtml"/></manifest><spine><itemref idref="chapter"/></spine></package>`},
			zipEntry{name: "OPS/chapter.xhtml", body: chapter},
		))
		record, err := media.InspectCapability(bytes.NewReader(data),
			inspectionPolicy(data, "book.epub", "application/epub+zip"))
		require.NoError(t, err)
		assert.False(t, record.Eligible)
		assert.Equal(t, media.CapabilityReasonExternalReference, record.Reason)
	}
}

func TestInspectCountsODSRepeatedCells(t *testing.T) {
	t.Parallel()
	data := zipBytes(t, []zipEntry{
		{name: "mimetype", body: "application/vnd.oasis.opendocument.spreadsheet"},
		{name: "META-INF/manifest.xml", body: `<manifest:manifest xmlns:manifest="urn:oasis:names:tc:opendocument:xmlns:manifest:1.0"/>`},
		{name: "content.xml", body: `<office:document-content xmlns:office="urn:oasis:names:tc:opendocument:xmlns:office:1.0" xmlns:table="urn:oasis:names:tc:opendocument:xmlns:table:1.0"><office:body><office:spreadsheet><table:table><table:table-column table:number-columns-repeated="3"/><table:table-row table:number-rows-repeated="500"><table:table-cell table:number-columns-repeated="200"/></table:table-row></table:table></office:spreadsheet></office:body></office:document-content>`},
	})
	policy := inspectionPolicy(data, "book.ods", "application/vnd.oasis.opendocument.spreadsheet")
	policy.MaxCells = 100_000
	record, err := media.InspectCapability(bytes.NewReader(data), policy)
	require.NoError(t, err)
	require.True(t, record.Eligible, record.Reason)
	assert.Equal(t, int64(100_000), record.Measurements.Cells)

	policy.MaxCells--
	record, err = media.InspectCapability(bytes.NewReader(data), policy)
	require.NoError(t, err)
	assert.False(t, record.Eligible)
	assert.Equal(t, media.CapabilityReasonSemanticUnits, record.Reason)
}

func TestInspectCountsFinitePresentationSpreadsheetAndEPUBUnits(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		filename   string
		mediaType  string
		entries    []zipEntry
		family     string
		assertions func(*testing.T, media.CapabilityRecord)
	}{
		{
			name: "slides", filename: "deck.pptx", mediaType: "application/vnd.openxmlformats-officedocument.presentationml.presentation",
			entries: validPPTXEntries(
				zipEntry{name: "ppt/slides/slide1.xml", body: "<slide/>"},
				zipEntry{name: "ppt/slides/slide2.xml", body: "<slide/>"},
			), family: "presentation",
			assertions: func(t *testing.T, r media.CapabilityRecord) {
				t.Helper()
				assert.Equal(t, int64(2), r.Measurements.Slides)
			},
		},
		{
			name: "sheets and cells", filename: "book.xlsx", mediaType: "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
			entries: validXLSXEntries(zipEntry{name: "xl/worksheets/sheet1.xml", body: `<worksheet><c/><c/></worksheet>`}), family: "spreadsheet",
			assertions: func(t *testing.T, r media.CapabilityRecord) {
				t.Helper()
				assert.Equal(t, int64(1), r.Measurements.Sheets)
				assert.Equal(t, int64(2), r.Measurements.Cells)
			},
		},
		{
			name: "spine and resources", filename: "book.epub", mediaType: "application/epub+zip",
			entries: validEPUBEntries(zipEntry{name: "OPS/content.opf", body: `<package><manifest><item id="a" href="a.xhtml"/><item id="b" href="b.png"/></manifest><spine><itemref idref="a"/></spine></package>`}), family: "ebook",
			assertions: func(t *testing.T, r media.CapabilityRecord) {
				t.Helper()
				assert.Equal(t, int64(1), r.Measurements.SpineItems)
				assert.Equal(t, int64(2), r.Measurements.Resources)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			data := zipBytes(t, tt.entries)
			record, err := media.InspectCapability(bytes.NewReader(data), inspectionPolicy(data, tt.filename, tt.mediaType))
			require.NoError(t, err)
			require.True(t, record.Eligible, record.Reason)
			assert.Equal(t, tt.family, record.MediaFamily)
			tt.assertions(t, record)
		})
	}
}

func TestInspectRejectsDeceptiveDeclaredFormatAndFilename(t *testing.T) {
	t.Parallel()
	pdf := syntheticPDF("deceptive")
	tests := []struct {
		name, filename, mediaType string
		data                      []byte
	}{
		{name: "PDF with text filename", filename: "report.txt", mediaType: "application/pdf", data: pdf},
		{name: "text declared as PDF", filename: "report.pdf", mediaType: "application/pdf", data: []byte("not a PDF")},
		{name: "OOXML declared generic ZIP", filename: "deck.pptx", mediaType: "application/zip", data: zipBytes(t, validPPTXEntries())},
		{name: "PPTX without main markers", filename: "deck.pptx", mediaType: "application/vnd.openxmlformats-officedocument.presentationml.presentation", data: zipBytes(t, []zipEntry{{name: "ppt/slides/slide1.xml", body: "<slide/>"}})},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			record, err := media.InspectCapability(bytes.NewReader(tt.data), inspectionPolicy(tt.data, tt.filename, tt.mediaType))
			require.NoError(t, err)
			assert.False(t, record.Eligible)
			assert.Equal(t, media.CapabilityReasonMalformed, record.Reason)
		})
	}
}

func TestInspectBindsVisualAndStandaloneXMLIdentity(t *testing.T) {
	t.Parallel()
	png := mediatest.PNG(4, 3, nil)
	for _, filename := range []string{"image.txt", "image.jpeg"} {
		policy := inspectionPolicy(png, filename, "image/png")
		policy.MaxPixels, policy.MaxFrames = 100, 2
		record, err := media.InspectCapability(bytes.NewReader(png), policy)
		require.NoError(t, err)
		assert.False(t, record.Eligible)
		assert.Equal(t, media.CapabilityReasonMalformed, record.Reason)
	}
	policy := inspectionPolicy(png, "image.png", "image/png")
	policy.MaxPixels, policy.MaxFrames = 100, 2
	record, err := media.InspectCapability(bytes.NewReader(png), policy)
	require.NoError(t, err)
	assert.True(t, record.Eligible)

	xml := []byte(`<!DOCTYPE root SYSTEM "file:///tmp/private.dtd"><root/>`)
	record, err = media.InspectCapability(bytes.NewReader(xml), inspectionPolicy(xml, "record.xml", "application/xml"))
	require.NoError(t, err)
	assert.False(t, record.Eligible)
	assert.Equal(t, media.CapabilityReasonExternalReference, record.Reason)
}

func TestInspectRejectsUnregisteredFamily(t *testing.T) {
	t.Parallel()
	data := []byte("legacy binary document")
	record, err := media.InspectCapability(bytes.NewReader(data), inspectionPolicy(data, "report.doc", "application/msword"))
	require.NoError(t, err)
	assert.False(t, record.Eligible)
	assert.Equal(t, media.CapabilityReasonUnboundedFamily, record.Reason)
}

func TestInspectBoundsPDFPagesAndWAVDuration(t *testing.T) {
	t.Parallel()
	pdf := syntheticPDFPages(2, "bounds")
	pdfPolicy := inspectionPolicy(pdf, "report.pdf", "application/pdf")
	pdfPolicy.MaxPages = 1
	record, err := media.InspectCapability(bytes.NewReader(pdf), pdfPolicy)
	require.NoError(t, err)
	assert.Equal(t, int64(2), record.Measurements.Pages)
	assert.Equal(t, media.CapabilityReasonSemanticUnits, record.Reason)

	wav := wavBytes(8_000, 8_000)
	wavPolicy := inspectionPolicy(wav, "sample.wav", "audio/wav")
	wavPolicy.MaxDurationMS = 500
	record, err = media.InspectCapability(bytes.NewReader(wav), wavPolicy)
	require.NoError(t, err)
	assert.Equal(t, int64(1_000), record.Measurements.DurationMS)
	assert.Equal(t, media.CapabilityReasonVisualBounds, record.Reason)
}

func TestInspectResolvesAndBoundsAuthoritativePDFObjects(t *testing.T) {
	t.Parallel()
	t.Run("xref ignores unreferenced duplicate", func(t *testing.T) {
		pdf := syntheticPDFObjects("xref-authority", []string{
			"<< /Type /Catalog /Pages 2 0 R >>",
			"<< /Type /Pages /Kids [3 0 R 4 0 R] /Count 2 >>",
			"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] >>",
			"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] >>",
		}, "2 0 obj\n<< /Type /Pages /Kids [3 0 R] /Count 1 >>\nendobj\n")
		policy := inspectionPolicy(pdf, "report.pdf", "application/pdf")
		policy.MaxPages = 1
		record, err := media.InspectCapability(bytes.NewReader(pdf), policy)
		require.NoError(t, err)
		assert.Equal(t, int64(2), record.Measurements.Pages)
		assert.Equal(t, media.CapabilityReasonSemanticUnits, record.Reason)
	})

	buildStreamPDF := func(t *testing.T, decoded []byte) []byte {
		t.Helper()
		var compressed bytes.Buffer
		writer := zlib.NewWriter(&compressed)
		_, err := writer.Write(decoded)
		require.NoError(t, err)
		require.NoError(t, writer.Close())
		return syntheticPDFObjects("indirect-stream", []string{
			"<< /Type /Catalog /Pages 2 0 R >>",
			"<< /Type /Pages /Kids [3 0 R] /Count 1 >>",
			"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Contents 4 0 R >>",
			fmt.Sprintf("<< /Length 5 0 R /Filter /FlateDecode >>\nstream\n%s\nendstream", compressed.Bytes()),
			strconv.Itoa(compressed.Len()),
		})
	}

	t.Run("indirect stream length", func(t *testing.T) {
		decoded := []byte("BT /F1 12 Tf 72 720 Td (bounded) Tj ET")
		pdf := buildStreamPDF(t, decoded)
		record, err := media.InspectCapability(bytes.NewReader(pdf),
			inspectionPolicy(pdf, "report.pdf", "application/pdf"))
		require.NoError(t, err)
		require.True(t, record.Eligible, record.Reason)
		assert.Equal(t, int64(len(decoded)), record.Measurements.ExpandedBytes)
	})

	t.Run("aggregate expansion", func(t *testing.T) {
		pdf := buildStreamPDF(t, bytes.Repeat([]byte("expanded "), 100_000))
		policy := inspectionPolicy(pdf, "report.pdf", "application/pdf")
		policy.MaxExpandedBytes = 128
		record, err := media.InspectCapability(bytes.NewReader(pdf), policy)
		require.NoError(t, err)
		assert.False(t, record.Eligible)
		assert.Equal(t, media.CapabilityReasonExpandedBytes, record.Reason)
	})

	t.Run("xref and object streams", func(t *testing.T) {
		bodies := []string{
			"<< /Type /Catalog /Pages 2 0 R >>",
			"<< /Type /Pages /Kids [3 0 R] /Count 1 >>",
			"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] >>",
		}
		second := len(bodies[0]) + 1
		third := second + len(bodies[1]) + 1
		prolog := fmt.Sprintf("1 0 2 %d 3 %d ", second, third)
		decoded := []byte(prolog + strings.Join(bodies, " "))
		var compressed bytes.Buffer
		writer := zlib.NewWriter(&compressed)
		_, err := writer.Write(decoded)
		require.NoError(t, err)
		require.NoError(t, writer.Close())

		var pdf bytes.Buffer
		_, _ = pdf.WriteString("%PDF-1.5\n")
		objectStreamOffset := pdf.Len()
		_, _ = fmt.Fprintf(&pdf, "4 0 obj\n<< /Type /ObjStm /N 3 /First %d /Length %d /Filter /FlateDecode >>\nstream\n", len(prolog), compressed.Len())
		_, _ = pdf.Write(compressed.Bytes())
		_, _ = pdf.WriteString("\nendstream\nendobj\n")
		xrefOffset := pdf.Len()
		entries := make([]byte, 0, 6*7)
		appendEntry := func(kind byte, field1 uint32, field2 uint16) {
			entries = append(entries, kind, byte(field1>>24), byte(field1>>16), byte(field1>>8), byte(field1), byte(field2>>8), byte(field2))
		}
		appendEntry(0, 0, math.MaxUint16)
		appendEntry(2, 4, 0)
		appendEntry(2, 4, 1)
		appendEntry(2, 4, 2)
		appendEntry(1, uint32(objectStreamOffset), 0) // #nosec G115 -- synthetic fixture is bounded
		appendEntry(1, uint32(xrefOffset), 0)         // #nosec G115 -- synthetic fixture is bounded
		_, _ = fmt.Fprintf(&pdf, "5 0 obj\n<< /Type /XRef /Size 6 /Root 1 0 R /W [1 4 2] /Length %d >>\nstream\n", len(entries))
		_, _ = pdf.Write(entries)
		_, _ = fmt.Fprintf(&pdf, "\nendstream\nendobj\nstartxref\n%d\n%%%%EOF\n", xrefOffset)

		data := pdf.Bytes()
		record, err := media.InspectCapability(bytes.NewReader(data),
			inspectionPolicy(data, "report.pdf", "application/pdf"))
		require.NoError(t, err)
		require.True(t, record.Eligible, record.Reason)
		assert.Equal(t, int64(1), record.Measurements.Pages)
		assert.Positive(t, record.Measurements.ExpandedBytes)
	})
}

func TestInspectRejectsForgedWAVRateAndTrailingBytes(t *testing.T) {
	t.Parallel()
	tests := map[string]func([]byte) []byte{
		"forged byte rate": func(data []byte) []byte {
			binary.LittleEndian.PutUint32(data[28:32], 1<<30)
			return data
		},
		"trailing bytes outside RIFF": func(data []byte) []byte {
			return append(data, []byte("trailing")...)
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			wav := mutate(wavBytes(8_000, 8_000))
			policy := inspectionPolicy(wav, "sample.wav", "audio/wav")
			policy.MaxDurationMS = 2_000
			record, err := media.InspectCapability(bytes.NewReader(wav), policy)
			require.NoError(t, err)
			assert.False(t, record.Eligible)
			assert.Equal(t, media.CapabilityReasonMalformed, record.Reason)
		})
	}
}

func TestInspectRejectsPathLikePortableFilename(t *testing.T) {
	t.Parallel()
	data := []byte("alpha\n")
	for _, filename := range []string{".", "..", `folder\notes.txt`, "folder/notes.txt"} {
		policy := inspectionPolicy(data, filename, "text/plain")
		_, err := media.InspectCapability(bytes.NewReader(data), policy)
		require.ErrorContains(t, err, "filename", filename)
	}
}

func TestInspectCountsOnlyPDFPageTreeObjects(t *testing.T) {
	t.Parallel()
	pdf := syntheticPDF("/Type /Page in a comment")
	policy := inspectionPolicy(pdf, "report.pdf", "application/pdf")
	policy.MaxPages = 1
	record, err := media.InspectCapability(bytes.NewReader(pdf), policy)
	require.NoError(t, err)
	require.True(t, record.Eligible, record.Reason)
	assert.Equal(t, int64(1), record.Measurements.Pages)
}

func TestInspectRejectsExcessivelyDeepPDFPageTree(t *testing.T) {
	t.Parallel()
	pdf := syntheticDeepPDF(300)
	policy := inspectionPolicy(pdf, "report.pdf", "application/pdf")
	policy.MaxPages = 1
	record, err := media.InspectCapability(bytes.NewReader(pdf), policy)
	require.NoError(t, err)
	assert.False(t, record.Eligible)
	assert.Equal(t, media.CapabilityReasonMalformed, record.Reason)
}

func TestInspectBoundsVideoFramesFromValidatedSampleTables(t *testing.T) {
	t.Parallel()
	video := mediatest.MP4(64, 48, 1_000)
	decoy := make([]byte, 20)
	binary.BigEndian.PutUint32(decoy[:4], 20)
	copy(decoy[4:8], "stsz")
	binary.BigEndian.PutUint32(decoy[16:20], 100)
	video = append(video, mediatest.Box("free", decoy)...)
	policy := inspectionPolicy(video, "clip.mp4", "video/mp4")
	policy.MaxPixels = 64 * 48
	policy.MaxFrames = 1
	policy.MaxDurationMS = 1_000
	record, err := media.InspectCapability(bytes.NewReader(video), policy)
	require.NoError(t, err)
	require.True(t, record.Eligible, record.Reason)
	assert.Equal(t, int64(1), record.Measurements.Frames)

	policy.MaxFrames = 0
	record, err = media.InspectCapability(bytes.NewReader(video), policy)
	require.NoError(t, err)
	assert.False(t, record.Eligible)
	assert.Equal(t, media.CapabilityReasonVisualBounds, record.Reason)
}

func inspectionPolicy(data []byte, filename, mediaType string) media.InspectionPolicy {
	return media.InspectionPolicy{
		Filename: filename, DeclaredMediaType: mediaType,
		ExpectedBytes: int64(len(data)), ExpectedSHA256: sha256Hex(data),
		DescriptorFingerprint: strings.Repeat("a", 64), ProfileFingerprint: strings.Repeat("b", 64),
		DisclosureFingerprint: strings.Repeat("c", 64), InputKind: document.RenditionInputOriginalFile,
		MaxSourceBytes: 1 << 20, MaxExpandedBytes: 1 << 20, MaxEntryBytes: 1 << 20,
		MaxEntries: 100, MaxNestingDepth: 1, MaxTextLines: 1_000, MaxCharacters: 1 << 20,
		MaxPages: 100, MaxSlides: 100, MaxSheets: 100, MaxCells: 10_000, MaxSpineItems: 1_000, MaxResources: 10_000,
	}
}

type zipEntry struct{ name, body string }

func validPPTXEntries(extra ...zipEntry) []zipEntry {
	return append([]zipEntry{
		{name: "[Content_Types].xml", body: `<Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types"><Override PartName="/ppt/presentation.xml" ContentType="application/vnd.openxmlformats-officedocument.presentationml.presentation.main+xml"/></Types>`},
		{name: "ppt/presentation.xml", body: `<p:presentation xmlns:p="http://schemas.openxmlformats.org/presentationml/2006/main"/>`},
	}, extra...)
}

func validXLSXEntries(extra ...zipEntry) []zipEntry {
	return append([]zipEntry{
		{name: "[Content_Types].xml", body: `<Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types"><Override PartName="/xl/workbook.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.sheet.main+xml"/></Types>`},
		{name: "xl/workbook.xml", body: `<workbook xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main"/>`},
	}, extra...)
}

func validEPUBEntries(extra ...zipEntry) []zipEntry {
	return append([]zipEntry{
		{name: "mimetype", body: "application/epub+zip"},
		{name: "META-INF/container.xml", body: `<container xmlns="urn:oasis:names:tc:opendocument:xmlns:container"><rootfiles><rootfile full-path="OPS/content.opf"/></rootfiles></container>`},
	}, extra...)
}

func zipBytes(t *testing.T, entries []zipEntry) []byte {
	t.Helper()
	var out bytes.Buffer
	zw := zip.NewWriter(&out)
	for _, entry := range entries {
		w, err := zw.Create(entry.name)
		require.NoError(t, err)
		_, err = w.Write([]byte(entry.body))
		require.NoError(t, err)
	}
	require.NoError(t, zw.Close())
	return out.Bytes()
}

func sha256Hex(data []byte) string {
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}

func wavBytes(byteRate, dataBytes uint32) []byte {
	data := make([]byte, 44+dataBytes)
	copy(data[0:4], "RIFF")
	binary.LittleEndian.PutUint32(data[4:8], uint32(len(data)-8)) // #nosec G115 -- synthetic fixture is bounded
	copy(data[8:12], "WAVE")
	copy(data[12:16], "fmt ")
	binary.LittleEndian.PutUint32(data[16:20], 16)
	binary.LittleEndian.PutUint16(data[20:22], 1)
	binary.LittleEndian.PutUint16(data[22:24], 1)
	binary.LittleEndian.PutUint32(data[24:28], byteRate)
	binary.LittleEndian.PutUint32(data[28:32], byteRate)
	binary.LittleEndian.PutUint16(data[32:34], 1)
	binary.LittleEndian.PutUint16(data[34:36], 8)
	copy(data[36:40], "data")
	binary.LittleEndian.PutUint32(data[40:44], dataBytes)
	return data
}

func syntheticPDF(label string) []byte {
	return syntheticPDFPages(1, label)
}

func syntheticPDFPages(pageCount int, label string) []byte {
	kids := make([]string, 0, pageCount)
	objects := []string{
		"<< /Type /Catalog /Pages 2 0 R /Note (/Type /Page in a string) >>",
		"",
	}
	for index := range pageCount {
		kids = append(kids, fmt.Sprintf("%d 0 R", index+3))
		objects = append(objects, "<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] >>")
	}
	objects[1] = fmt.Sprintf("<< /Type /Pages /Kids [%s] /Count %d >>", strings.Join(kids, " "), pageCount)
	objects = append(objects,
		"<< /Type /Page /Parent 2 0 R /Note (orphan page object) >>",
		"<< /Length 11 >>\nstream\n/Type /Page\nendstream",
	)
	return syntheticPDFObjects(label, objects)
}

func syntheticDeepPDF(depth int) []byte {
	objects := make([]string, 0, depth+2)
	objects = append(objects, "<< /Type /Catalog /Pages 2 0 R >>")
	for index := range depth {
		number := index + 2
		child := number + 1
		parent := ""
		if index != 0 {
			parent = fmt.Sprintf(" /Parent %d 0 R", number-1)
		}
		objects = append(objects, fmt.Sprintf(
			"<< /Type /Pages /Kids [%d 0 R] /Count 1%s >>", child, parent))
	}
	objects = append(objects, fmt.Sprintf(
		"<< /Type /Page /Parent %d 0 R /MediaBox [0 0 612 792] >>", depth+1))
	return syntheticPDFObjects("deep", objects)
}

func syntheticPDFObjects(label string, objects []string, extraDefinitions ...string) []byte {
	var output bytes.Buffer
	_, _ = fmt.Fprintf(&output, "%%PDF-1.4\n%%%x\n", label)
	offsets := make([]int, len(objects))
	for index, object := range objects {
		offsets[index] = output.Len()
		_, _ = fmt.Fprintf(&output, "%d 0 obj\n%s\nendobj\n", index+1, object)
	}
	for _, definition := range extraDefinitions {
		_, _ = output.WriteString(definition)
	}
	xref := output.Len()
	_, _ = fmt.Fprintf(&output, "xref\n0 %d\n0000000000 65535 f \n", len(objects)+1)
	for _, offset := range offsets {
		_, _ = fmt.Fprintf(&output, "%010d 00000 n \n", offset)
	}
	_, _ = fmt.Fprintf(&output,
		"trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(objects)+1, xref)
	return output.Bytes()
}
