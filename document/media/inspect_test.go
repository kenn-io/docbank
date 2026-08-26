package media_test

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json/v2"
	"fmt"
	"math"
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
		{name: "external DTD", body: `<!DOCTYPE worksheet SYSTEM "https://example.invalid/sheet.dtd"><worksheet/>`, reason: media.CapabilityReasonExternalReference},
		{name: "public DTD", body: `<!DOCTYPE worksheet PUBLIC "-//EXAMPLE//DTD Sheet//EN" "sheet.dtd"><worksheet/>`, reason: media.CapabilityReasonExternalReference},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			data := zipBytes(t, []zipEntry{{name: "xl/worksheets/sheet1.xml", body: tt.body}})
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
	data := zipBytes(t, validEPUBEntries(
		zipEntry{name: "OPS/content.opf", body: `<package><manifest><item id="chapter" href="chapter.xhtml"/></manifest><spine><itemref idref="chapter"/></spine></package>`},
		zipEntry{name: "OPS/chapter.xhtml", body: `<html xmlns="http://www.w3.org/1999/xhtml"><body><img src="https://example.invalid/tracker.png"/></body></html>`},
	))
	record, err := media.InspectCapability(bytes.NewReader(data),
		inspectionPolicy(data, "book.epub", "application/epub+zip"))
	require.NoError(t, err)
	assert.False(t, record.Eligible)
	assert.Equal(t, media.CapabilityReasonExternalReference, record.Reason)
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

func TestInspectCountsXRefStreamPDFPageTree(t *testing.T) {
	t.Parallel()
	for _, testCase := range []struct {
		name  string
		index string
	}{
		{name: "default Index"},
		{name: "explicit Index sections", index: "/Index [0 3 3 3]"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			pdf := syntheticXRefStreamPDFWith(xrefStreamFixture{index: testCase.index})

			pages, err := media.CountPDFPages(pdf)
			require.NoError(t, err)
			assert.Equal(t, int64(1), pages)
			info, err := media.PDFInfoFields(pdf)
			require.NoError(t, err)
			assert.Equal(t, map[string]string{"Title": "Synthetic xref stream"}, info)

			policy := inspectionPolicy(pdf, "report.pdf", "application/pdf")
			policy.MaxPages = 1
			record, err := media.InspectCapability(bytes.NewReader(pdf), policy)
			require.NoError(t, err)
			require.True(t, record.Eligible, record.Reason)
			assert.Equal(t, int64(1), record.Measurements.Pages)
		})
	}
}

func TestInspectRejectsUnprovenXRefStreamAuthority(t *testing.T) {
	t.Parallel()
	for _, testCase := range []struct {
		name    string
		fixture xrefStreamFixture
	}{
		{
			name: "short stream does not cover Size",
			fixture: xrefStreamFixture{mutate: func(entries []byte, _ []int) []byte {
				return entries[:7]
			}},
		},
		{
			name: "root entry has forged offset",
			fixture: xrefStreamFixture{mutate: func(entries []byte, offsets []int) []byte {
				putSyntheticXRefEntry(entries, 1, 1, uint32(offsets[3]), 0)
				return entries
			}},
		},
		{
			name: "info entry has forged offset",
			fixture: xrefStreamFixture{mutate: func(entries []byte, offsets []int) []byte {
				putSyntheticXRefEntry(entries, 4, 1, uint32(offsets[0]), 0)
				return entries
			}},
		},
		{
			name: "root entry uses a compressed object",
			fixture: xrefStreamFixture{mutate: func(entries []byte, _ []int) []byte {
				putSyntheticXRefEntry(entries, 1, 2, 5, 0)
				return entries
			}},
		},
		{
			name:    "filtered stream is unsupported",
			fixture: xrefStreamFixture{dictionaryExtra: "/Filter /FlateDecode"},
		},
		{
			name:    "duplicate Index sections",
			fixture: xrefStreamFixture{index: "/Index [0 4 3 3]", mutate: appendSyntheticXRefEntry(3)},
		},
		{
			name:    "out of range Index section",
			fixture: xrefStreamFixture{index: "/Index [0 6 6 1]", mutate: appendSyntheticXRefEntry(0)},
		},
		{
			name: "in-use entry points outside the document",
			fixture: xrefStreamFixture{mutate: func(entries []byte, _ []int) []byte {
				putSyntheticXRefEntry(entries, 2, 1, math.MaxUint32, 0)
				return entries
			}},
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			pdf := syntheticXRefStreamPDFWith(testCase.fixture)
			_, err := media.CountPDFPages(pdf)
			require.Error(t, err)
			_, err = media.PDFInfoFields(pdf)
			require.Error(t, err)

			policy := inspectionPolicy(pdf, "report.pdf", "application/pdf")
			policy.MaxPages = 1
			record, err := media.InspectCapability(bytes.NewReader(pdf), policy)
			require.NoError(t, err)
			assert.False(t, record.Eligible)
			assert.Equal(t, media.CapabilityReasonMalformed, record.Reason)
		})
	}
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
	var output bytes.Buffer
	_, _ = fmt.Fprintf(&output, "%%PDF-1.4\n%%%x\n", label)
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
	_, _ = fmt.Fprintf(&output, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(objects)+1, xref)
	return output.Bytes()
}

type xrefStreamFixture struct {
	index, dictionaryExtra string
	mutate                 func([]byte, []int) []byte
}

func syntheticXRefStreamPDFWith(fixture xrefStreamFixture) []byte {
	objects := []string{
		"<< /Type /Catalog /Pages 2 0 R >>",
		"<< /Type /Pages /Kids [3 0 R] /Count 1 >>",
		"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] >>",
		"<< /Title (Synthetic xref stream) >>",
	}
	var output bytes.Buffer
	_, _ = output.WriteString("%PDF-1.7\n")
	offsets := make([]int, len(objects))
	for index, object := range objects {
		offsets[index] = output.Len()
		_, _ = fmt.Fprintf(&output, "%d 0 obj\n%s\nendobj\n", index+1, object)
	}
	xref := output.Len()
	entries := make([]byte, 6*7)
	putSyntheticXRefEntry(entries, 0, 0, 0, 65_535)
	for index, offset := range offsets {
		putSyntheticXRefEntry(entries, index+1, 1, uint32(offset), 0) // #nosec G115 -- bounded fixture.
	}
	putSyntheticXRefEntry(entries, 5, 1, uint32(xref), 0) // #nosec G115 -- bounded fixture.
	if fixture.mutate != nil {
		entries = fixture.mutate(entries, offsets)
	}
	_, _ = fmt.Fprintf(&output,
		"5 0 obj\n<< /Type /XRef /Size 6 /Root 1 0 R /Info 4 0 R /W [1 4 2] /Length %d %s %s >>\nstream\n",
		len(entries), fixture.index, fixture.dictionaryExtra)
	_, _ = output.Write(entries)
	_, _ = fmt.Fprintf(&output, "\nendstream\nendobj\nstartxref\n%d\n%%%%EOF\n", xref)
	return output.Bytes()
}

func appendSyntheticXRefEntry(index int) func([]byte, []int) []byte {
	return func(entries []byte, _ []int) []byte {
		return append(entries, entries[index*7:(index+1)*7]...)
	}
}

func putSyntheticXRefEntry(entries []byte, index int, kind byte, offset uint32, generation uint16) {
	entry := entries[index*7 : (index+1)*7]
	entry[0] = kind
	binary.BigEndian.PutUint32(entry[1:5], offset)
	binary.BigEndian.PutUint16(entry[5:7], generation)
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
		"trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(objects)+1, xref)
	return output.Bytes()
}
