package mistral

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json/v2"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document/internal/formatdetect"
	"go.kenn.io/docbank/internal/formatqualification"
)

func TestDetectFormatRecognizesBoundedDocumentFamilies(t *testing.T) {
	docx := documentZIP(t, map[string]string{
		ooxmlContentTypesName: docxContentTypes("application/vnd.openxmlformats-officedocument.wordprocessingml.document.main+xml"), "word/document.xml": "<document/>",
	})
	epub := documentZIP(t, map[string]string{
		"mimetype": "application/epub+zip", "META-INF/container.xml": "<container/>",
	})
	compound := compoundDocument(t, "WordDocument")

	tests := []struct {
		name      string
		content   []byte
		mediaType string
		wantID    string
	}{
		{name: "PDF", content: testPDF("synthetic"), mediaType: "application/pdf", wantID: "pdf"},
		{name: "PDF xref stream", content: testPDFXRefStream(), mediaType: "application/pdf", wantID: "pdf"},
		{name: "DOCX", content: docx, mediaType: "application/vnd.openxmlformats-officedocument.wordprocessingml.document", wantID: "docx"},
		{name: "EPUB", content: epub, mediaType: "application/epub+zip", wantID: "epub"},
		{name: "legacy DOC", content: compound, mediaType: "application/msword", wantID: "doc"},
		{name: "CSV", content: []byte("name,value\nalpha,42\n"), mediaType: "text/csv", wantID: "csv"},
		{name: "JSON", content: []byte(`{"alpha":42}`), mediaType: "application/json", wantID: "json"},
		{name: "JSONL", content: []byte("{\"alpha\":1}\n{\"alpha\":2}\n"), mediaType: "application/x-ndjson", wantID: "jsonl"},
		{name: "XML", content: []byte(`<root><value>42</value></root>`), mediaType: "application/xml", wantID: "xml"},
		{name: "YAML", content: []byte("---\nalpha: 42\n"), mediaType: "application/yaml", wantID: "yaml"},
		{name: "LaTeX", content: []byte(`\documentclass{article}\begin{document}x\end{document}`), mediaType: "application/x-tex", wantID: "latex"},
		{name: "EML", content: []byte("From: sender@example.test\r\nDate: Thu, 13 Aug 2026 00:00:00 +0000\r\nSubject: Synthetic\r\n\r\nBody"), mediaType: "message/rfc822", wantID: "eml"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			format, err := DetectFormat(bytes.NewReader(test.content), int64(len(test.content)), test.mediaType)
			require.NoError(t, err)
			assert.Equal(t, test.wantID, format.ID)
			_, qualified := formatqualification.Lookup(formatqualification.Query{
				CatalogID: test.wantID, Capability: formatqualification.CapabilityDetect,
				Evidence:         "TestDetectFormatRecognizesBoundedDocumentFamilies",
				ImplementationID: formatdetect.DetectionImplementationID,
				InputKind:        formatqualification.InputOriginalFile,
			})
			assert.True(t, qualified, "executed document detector format %s is absent from the qualification manifest", test.wantID)
		})
	}
}

func TestDetectFormatDoesNotQualifyMIMEOnlyGo(t *testing.T) {
	content := []byte("arbitrary UTF-8 prose\n")
	format, err := DetectFormat(bytes.NewReader(content), int64(len(content)), "text/x-go")
	require.NoError(t, err)
	assert.Equal(t, "go", format.ID)
	_, qualified := formatqualification.Lookup(formatqualification.Query{
		CatalogID: "go", Capability: formatqualification.CapabilityDetect,
		Evidence:         "TestDetectFormatRecognizesBoundedDocumentFamilies",
		ImplementationID: formatdetect.DetectionImplementationID,
		InputKind:        formatqualification.InputOriginalFile,
	})
	assert.False(t, qualified, "MIME candidate acceptance is not byte recognition")
}

func TestDetectFormatRejectsMismatchUnsafeZIPAndAmbiguousCompound(t *testing.T) {
	require := require.New(t)
	pdf := testPDF("synthetic")
	_, err := DetectFormat(bytes.NewReader(pdf), int64(len(pdf)), "text/plain")
	require.ErrorContains(err, "not declared")

	malformedPDF := []byte("%PDF-1.7\nsynthetic")
	_, err = DetectFormat(bytes.NewReader(malformedPDF), int64(len(malformedPDF)), "application/pdf")
	require.ErrorContains(err, "PDF startxref is missing")

	malformedXRef := bytes.Replace(testPDF("malformed-xref"), []byte("xref\n"), []byte("xref garbage\n"), 1)
	_, err = DetectFormat(bytes.NewReader(malformedXRef), int64(len(malformedXRef)), "application/pdf")
	require.ErrorContains(err, "cross-reference data")

	malformedRecord := bytes.Replace(testPDF("malformed-record"), []byte("0000000000 65535 f"), []byte("000000000X 65535 f"), 1)
	_, err = DetectFormat(bytes.NewReader(malformedRecord), int64(len(malformedRecord)), "application/pdf")
	require.ErrorContains(err, "cross-reference data")

	prefixKeys := bytes.Replace(testPDF("prefix-keys"), []byte("/Size 4 /Root "), []byte("/SizeFoo 4 /Rootkit "), 1)
	_, err = DetectFormat(bytes.NewReader(prefixKeys), int64(len(prefixKeys)), "application/pdf")
	require.ErrorContains(err, "cross-reference data")

	prefixStreamType := bytes.Replace(testPDFXRefStream(), []byte("/Type /XRef "), []byte("/Type /XRefish "), 1)
	_, err = DetectFormat(bytes.NewReader(prefixStreamType), int64(len(prefixStreamType)), "application/pdf")
	require.ErrorContains(err, "cross-reference data")

	polyglotPDF := append(testPDF("polyglot"), []byte("PK\x03\x04synthetic")...)
	_, err = DetectFormat(bytes.NewReader(polyglotPDF), int64(len(polyglotPDF)), "application/pdf")
	require.ErrorContains(err, "not final")

	unsafe := documentZIP(t, map[string]string{
		ooxmlContentTypesName: docxContentTypes("application/vnd.openxmlformats-officedocument.wordprocessingml.document.main+xml"), "word/document.xml": "<document/>", "../escape": "x",
	})
	_, err = DetectFormat(bytes.NewReader(unsafe), int64(len(unsafe)), "application/vnd.openxmlformats-officedocument.wordprocessingml.document")
	require.ErrorContains(err, "traversing")

	compound := compoundDocument(t, "WordDocument", "Workbook")
	_, err = DetectFormat(bytes.NewReader(compound), int64(len(compound)), "application/msword")
	require.ErrorContains(err, "ambiguous")

	embedded := compoundDocumentWithEmbeddedWorkbook(t)
	format, err := DetectFormat(bytes.NewReader(embedded), int64(len(embedded)), "application/msword")
	require.NoError(err)
	assert.Equal(t, "doc", format.ID)

	rootStorage := compoundDocument(t, "WordDocument", "Workbook")
	rootStorage[1024+256+66] = 1
	format, err = DetectFormat(bytes.NewReader(rootStorage), int64(len(rootStorage)), "application/msword")
	require.NoError(err)
	assert.Equal(t, "doc", format.ID)

	invalidJSON := []byte(`{"unterminated":`)
	_, err = DetectFormat(bytes.NewReader(invalidJSON), int64(len(invalidJSON)), "application/json")
	require.ErrorContains(err, "invalid")

	invalidCSV := []byte("name,value\n\"unterminated,42\n")
	_, err = DetectFormat(bytes.NewReader(invalidCSV), int64(len(invalidCSV)), "text/csv")
	require.ErrorContains(err, "invalid")

	macroEnabled := documentZIP(t, map[string]string{
		ooxmlContentTypesName: docxContentTypes("application/vnd.ms-word.document.macroEnabled.main+xml"),
		"word/document.xml":   "<document/>",
	})
	_, err = DetectFormat(bytes.NewReader(macroEnabled), int64(len(macroEnabled)), "application/vnd.openxmlformats-officedocument.wordprocessingml.document")
	require.ErrorContains(err, "not a supported document format")

	wrongNamespace := documentZIP(t, map[string]string{
		ooxmlContentTypesName: `<Types xmlns="urn:unrelated"><Override PartName="/word/document.xml" ContentType="application/vnd.openxmlformats-officedocument.wordprocessingml.document.main+xml"/></Types>`,
		"word/document.xml":   "<document/>",
	})
	_, err = DetectFormat(bytes.NewReader(wrongNamespace), int64(len(wrongNamespace)), "application/vnd.openxmlformats-officedocument.wordprocessingml.document")
	require.ErrorContains(err, "not a supported document format")

	trailingMalformed := documentZIP(t, map[string]string{
		ooxmlContentTypesName: docxContentTypes("application/vnd.openxmlformats-officedocument.wordprocessingml.document.main+xml") + "<broken>",
		"word/document.xml":   "<document/>",
	})
	_, err = DetectFormat(bytes.NewReader(trailingMalformed), int64(len(trailingMalformed)), "application/vnd.openxmlformats-officedocument.wordprocessingml.document")
	require.ErrorContains(err, "not a supported document format")

	for _, invalidXML := range []string{"", "<first/><second/>", "outside<root/>"} {
		_, err = DetectFormat(bytes.NewReader([]byte(invalidXML)), int64(len(invalidXML)), "application/xml")
		require.Error(err)
	}
}

func TestDetectFormatAcceptsRecoverablePDFs(t *testing.T) {
	tests := []struct {
		name    string
		content func(*testing.T) []byte
	}{
		{name: "missing final marker", content: func(t *testing.T) []byte {
			t.Helper()
			original := testPDF("recoverable")
			content := bytes.TrimSuffix(original, []byte("%%EOF\n"))
			require.Equal(t, 6, len(original)-len(content))
			return content
		}},
		{name: "early marker followed by incremental update", content: testPDFIncrementalUpdateWithoutFinalMarker},
		{name: "xref stream without final marker", content: func(t *testing.T) []byte {
			t.Helper()
			original := testPDFXRefStreamWithPageBox()
			content := bytes.TrimSuffix(original, []byte("%%EOF\n"))
			require.Equal(t, 6, len(original)-len(content))
			return content
		}},
		{name: "multiple xref subsections", content: func(t *testing.T) []byte {
			t.Helper()
			original := testPDF("multiple-xref-subsections")
			xrefStart := bytes.Index(original, []byte("xref\n0 4\n"))
			require.GreaterOrEqual(t, xrefStart, 0)
			recordsStart := xrefStart + len("xref\n0 4\n")
			firstRecordEnd := bytes.IndexByte(original[recordsStart:], '\n')
			require.GreaterOrEqual(t, firstRecordEnd, 0)
			split := recordsStart + firstRecordEnd + 1
			content := bytes.Clone(original[:xrefStart])
			content = append(content, []byte("xref\n0 1\n")...)
			content = append(content, original[recordsStart:split]...)
			content = append(content, []byte("1 3\n")...)
			return append(content, original[split:]...)
		}},
		{name: "trailer dictionary on same line", content: func(t *testing.T) []byte {
			t.Helper()
			original := testPDF("same-line-trailer")
			content := bytes.Replace(original, []byte("trailer\n<<"), []byte("trailer <<"), 1)
			require.NotEqual(t, original, content)
			return content
		}},
		{name: "trailer adjacent to dictionary", content: func(t *testing.T) []byte {
			t.Helper()
			original := testPDF("adjacent-trailer-dictionary")
			content := bytes.Replace(original, []byte("trailer\n<<"), []byte("trailer<<"), 1)
			require.NotEqual(t, original, content)
			return content
		}},
		{name: "large file without final marker", content: func(t *testing.T) []byte {
			t.Helper()
			original := testPDF(strings.Repeat("x", 40_000))
			content := bytes.TrimSuffix(original, []byte("%%EOF\n"))
			require.Greater(t, len(content), 65536)
			t.Logf("fixture length=%d; tail boundary=65536", len(content))
			return content
		}},
		{name: "final marker with CRLF", content: func(t *testing.T) []byte {
			t.Helper()
			return bytes.Replace(testPDF("crlf"), []byte("%%EOF\n"), []byte("%%EOF\r\n"), 1)
		}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			content := test.content(t)
			format, err := DetectFormat(bytes.NewReader(content), int64(len(content)), "application/pdf")
			require.NoError(t, err)
			assert.Equal(t, "pdf", format.ID)
			pages, err := formatdetect.CountPDFPages(content)
			require.NoError(t, err)
			assert.Equal(t, int64(1), pages)
		})
	}
}

func TestDetectFormatRejectsUnrecoverablePDFTrailers(t *testing.T) {
	original := testPDF("negative")
	recoverable := bytes.TrimSuffix(original, []byte("%%EOF\n"))

	t.Run("trailing bytes", func(t *testing.T) {
		for _, test := range []struct {
			name   string
			suffix string
		}{
			{name: "ZIP local header", suffix: "PK\x03\x04synthetic"},
			{name: "HTML document", suffix: "<html><body>synthetic</body></html>\n"},
			{name: "repeated marker", suffix: "%%EOF\n%%EOF\n"},
			{name: "partial marker", suffix: "%%EO"},
			{name: "object after trailer", suffix: "4 0 obj\n<< >>\nendobj\n"},
		} {
			t.Run(test.name, func(t *testing.T) {
				content := append(bytes.Clone(recoverable), test.suffix...)
				_, err := DetectFormat(bytes.NewReader(content), int64(len(content)), "application/pdf")
				require.ErrorContains(t, err, "not final")
			})
		}
	})

	t.Run("forged startxref to prior xref", func(t *testing.T) {
		for _, test := range []struct {
			name    string
			content []byte
		}{
			{name: "table", content: original},
			{name: "stream", content: testPDFXRefStreamWithPageBox()},
		} {
			t.Run(test.name, func(t *testing.T) {
				startXRef := bytes.LastIndex(test.content, []byte("startxref\n"))
				require.Positive(t, startXRef)
				var xrefOffset int
				_, err := fmt.Sscanf(string(test.content[startXRef+len("startxref\n"):]), "%d", &xrefOffset)
				require.NoError(t, err)

				content := fmt.Appendf(bytes.Clone(test.content[:startXRef]),
					"%%PK\x03\x04synthetic startxref %d\n", xrefOffset)
				_, err = DetectFormat(bytes.NewReader(content), int64(len(content)), "application/pdf")
				require.ErrorContains(t, err, "cross-reference data")
			})
		}
	})

	t.Run("xref section exceeds bound", func(t *testing.T) {
		const xrefEntries = 53_000
		original := testPDF("xref-section-bound")
		xrefStart := bytes.Index(original, []byte("\nxref\n")) + 1
		require.Positive(t, xrefStart)
		trailer := bytes.Index(original[xrefStart:], []byte("trailer\n"))
		require.Positive(t, trailer)
		xref := bytes.Replace(bytes.Clone(original[xrefStart:xrefStart+trailer]),
			[]byte("0 4\n"), []byte(fmt.Sprintf("0 %d\n", xrefEntries)), 1)
		content := bytes.Clone(original[:xrefStart])
		content = append(content, xref...)
		content = append(content, bytes.Repeat([]byte("0000000000 65535 f \n"), xrefEntries-4)...)
		content = fmt.Appendf(content, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n",
			xrefEntries, xrefStart)
		_, err := DetectFormat(bytes.NewReader(content), int64(len(content)), "application/pdf")
		require.ErrorContains(t, err, "cross-reference data exceeds the bound")
	})

	t.Run("xref stream indirect length", func(t *testing.T) {
		content := bytes.Replace(testPDFXRefStreamWithPageBox(), []byte("/Length 35"), []byte("/Length 5 0 R"), 1)
		_, err := DetectFormat(bytes.NewReader(content), int64(len(content)), "application/pdf")
		require.ErrorContains(t, err, "cross-reference data")
	})

	t.Run("offset at startxref keyword", func(t *testing.T) {
		content := testPDF("offset-bound")
		keyword := bytes.LastIndex(content, []byte("startxref"))
		bounded := fmt.Appendf(bytes.Clone(content[:keyword]), "startxref\n%d\n", keyword)
		_, err := DetectFormat(bytes.NewReader(bounded), int64(len(bounded)), "application/pdf")
		t.Logf("startxref offset=%d equals keyword position=%d; error=%v", keyword, keyword, err)
		require.ErrorContains(t, err, "outside the document")
	})

	t.Run("malformed xref", func(t *testing.T) {
		content := bytes.Replace(recoverable, []byte("xref\n"), []byte("xref garbage\n"), 1)
		_, err := DetectFormat(bytes.NewReader(content), int64(len(content)), "application/pdf")
		require.ErrorContains(t, err, "cross-reference data")
	})

	t.Run("invalid header version", func(t *testing.T) {
		content := bytes.Replace(recoverable, []byte("%PDF-1.4"), []byte("%PDF-3.0"), 1)
		_, err := DetectFormat(bytes.NewReader(content), int64(len(content)), "application/pdf")
		require.ErrorContains(t, err, "PDF header is invalid")
	})

	t.Run("declared text plain", func(t *testing.T) {
		_, err := DetectFormat(bytes.NewReader(recoverable), int64(len(recoverable)), "text/plain")
		require.ErrorContains(t, err, "not declared")
	})
}

func TestPrepareStagesRecoverablePDF(t *testing.T) {
	original := testPDF("recoverable-prepare")
	content := bytes.TrimSuffix(original, []byte("%%EOF\n"))
	digest := sha256.Sum256(content)
	directory := filepath.Join(t.TempDir(), "spool")
	makePrivateDirectory(t, directory)

	prepared, err := Prepare(t.Context(), io.NopCloser(bytes.NewReader(content)), testPolicy(t, 1024, 10), PrepareOptions{
		Directory: directory, DeclaredMediaType: mediaTypePDF, ExpectedSize: int64(len(content)),
		ExpectedSHA256: hex.EncodeToString(digest[:]), MaxSpoolBytes: 2048, MinFreeBytes: 1,
	})
	require.NoError(t, err)
	assert.Equal(t, "pdf", prepared.Format().ID)
	require.NoError(t, prepared.Release())
}

func TestRenditionClientVerifiesRecoverablePDFs(t *testing.T) {
	policy := testPolicy(t, 1<<20, 10)
	manifest := syntheticManifest(t, policy, true)
	descriptor := renditionDescriptor(t, policy, manifest, "pdf")
	tests := []struct {
		name    string
		content func(*testing.T) []byte
	}{
		{name: "missing final marker", content: func(t *testing.T) []byte {
			t.Helper()
			return bytes.TrimSuffix(testPDF("recoverable-rendition"), []byte("%%EOF\n"))
		}},
		{name: "early marker followed by incremental update", content: testPDFIncrementalUpdateWithoutFinalMarker},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			source := test.content(t)
			var requests int
			var uploaded []byte
			client, err := NewRenditionProvider(Profile{
				Policy: policy, CapabilityManifest: manifest, Descriptor: descriptor,
				SecretBinding: "mistral-ocr", Timeout: DefaultTimeout,
				MaxRetries: DefaultMaxRetries, MaxRetryDelay: DefaultMaxRetryDelay,
			}, renditionSecrets{"mistral-ocr": "synthetic-key"}, &http.Client{
				Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
					requests++
					body, err := io.ReadAll(request.Body)
					require.NoError(t, err)
					var wire struct {
						Document struct {
							URL string `json:"document_url"`
						} `json:"document"`
					}
					require.NoError(t, json.Unmarshal(body, &wire))
					encoded := strings.TrimPrefix(wire.Document.URL, "data:application/pdf;base64,")
					uploaded, err = base64.StdEncoding.DecodeString(encoded)
					require.NoError(t, err)
					return &http.Response{
						StatusCode: http.StatusOK,
						Header:     http.Header{"Content-Type": []string{"application/json"}},
						Body:       io.NopCloser(strings.NewReader(mistralRenditionResponse("synthetic"))),
						Request:    request,
					}, nil
				}),
			})
			require.NoError(t, err)

			fixture := renditionFixture(t, descriptor, source)
			result, err := client.Render(t.Context(), fixture.upload(), fixture.authorization)
			require.NoError(t, err)
			assert.Equal(t, 1, requests)
			assert.Equal(t, source, uploaded)
			assert.Equal(t, int64(1), result.Receipt.Usage.Units)
		})
	}
}

func TestDetectFormatBoundsCompoundAllocationBeforeReadingSectors(t *testing.T) {
	_, err := DetectFormat(bytes.NewReader([]byte("x")), MaxDocumentBytes+1, "text/plain")
	require.ErrorContains(t, err, "format-detection byte limit")

	header := make([]byte, 512)
	copy(header, compoundFileMagic)
	binary.LittleEndian.PutUint16(header[26:28], 3)
	binary.LittleEndian.PutUint16(header[28:30], 0xfffe)
	binary.LittleEndian.PutUint16(header[30:32], 9)
	binary.LittleEndian.PutUint32(header[44:48], ^uint32(0))
	_, err = compoundDirectoryNames(bytes.NewReader(header), MaxDocumentBytes)
	require.ErrorContains(t, err, "allocation table exceeds limits")
}

func TestValidateZIPEndRecordRejectsUnboundedDirectory(t *testing.T) {
	archive := documentZIP(t, map[string]string{
		ooxmlContentTypesName: docxContentTypes("application/vnd.openxmlformats-officedocument.wordprocessingml.document.main+xml"), "word/document.xml": "<document/>",
	})
	offset := bytes.LastIndex(archive, []byte{'P', 'K', 0x05, 0x06})
	require.GreaterOrEqual(t, offset, 0)
	binary.LittleEndian.PutUint16(archive[offset+8:offset+10], maxZIPEntries+1)
	binary.LittleEndian.PutUint16(archive[offset+10:offset+12], maxZIPEntries+1)
	_, err := DetectFormat(bytes.NewReader(archive), int64(len(archive)), "application/vnd.openxmlformats-officedocument.wordprocessingml.document")
	require.ErrorContains(t, err, "central directory")
}

func documentZIP(t *testing.T, entries map[string]string) []byte {
	t.Helper()
	var output bytes.Buffer
	writer := zip.NewWriter(&output)
	for name, value := range entries {
		entry, err := writer.Create(name)
		require.NoError(t, err)
		_, err = io.WriteString(entry, value)
		require.NoError(t, err)
	}
	require.NoError(t, writer.Close())
	return output.Bytes()
}

func docxContentTypes(mainContentType string) string {
	return `<Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types">` +
		`<Override PartName="/word/document.xml" ContentType="` + mainContentType + `"/>` +
		`</Types>`
}

func compoundDocument(t *testing.T, streamNames ...string) []byte {
	t.Helper()
	const (
		freeSector = uint32(0xffffffff)
		endOfChain = uint32(0xfffffffe)
		fatSector  = uint32(0xfffffffd)
	)
	content := make([]byte, 3*512)
	header := content[:512]
	copy(header, compoundFileMagic)
	binary.LittleEndian.PutUint16(header[26:28], 3)
	binary.LittleEndian.PutUint16(header[28:30], 0xfffe)
	binary.LittleEndian.PutUint16(header[30:32], 9)
	binary.LittleEndian.PutUint16(header[32:34], 6)
	binary.LittleEndian.PutUint32(header[44:48], 1)
	binary.LittleEndian.PutUint32(header[48:52], 1)
	binary.LittleEndian.PutUint32(header[56:60], 4096)
	binary.LittleEndian.PutUint32(header[60:64], endOfChain)
	binary.LittleEndian.PutUint32(header[68:72], endOfChain)
	for offset := 76; offset < 512; offset += 4 {
		binary.LittleEndian.PutUint32(header[offset:offset+4], freeSector)
	}
	binary.LittleEndian.PutUint32(header[76:80], 0)

	fat := content[512:1024]
	for offset := 0; offset < len(fat); offset += 4 {
		binary.LittleEndian.PutUint32(fat[offset:offset+4], freeSector)
	}
	binary.LittleEndian.PutUint32(fat[0:4], fatSector)
	binary.LittleEndian.PutUint32(fat[4:8], endOfChain)

	directory := content[1024:]
	writeCompoundDirectoryEntry(t, directory[:128], "Root Entry", 5)
	if len(streamNames) > 0 {
		binary.LittleEndian.PutUint32(directory[76:80], 1)
	}
	for i, name := range streamNames {
		offset := (i + 1) * 128
		require.LessOrEqual(t, offset+128, len(directory))
		writeCompoundDirectoryEntry(t, directory[offset:offset+128], name, 2)
		if i+1 < len(streamNames) {
			binary.LittleEndian.PutUint32(directory[offset+72:offset+76], uint32(i+2))
		}
	}
	return content
}

func compoundDocumentWithEmbeddedWorkbook(t *testing.T) []byte {
	t.Helper()
	content := compoundDocument(t, "WordDocument", "ObjectPool", "Workbook")
	directory := content[1024:]
	// Root tree contains WordDocument and ObjectPool only.
	binary.LittleEndian.PutUint32(directory[128+72:128+76], 2)
	binary.LittleEndian.PutUint32(directory[256+72:256+76], compoundNoStream)
	// Workbook is a child of ObjectPool, not a root-level stream.
	directory[256+66] = 1
	binary.LittleEndian.PutUint32(directory[256+76:256+80], 3)
	return content
}

func writeCompoundDirectoryEntry(t *testing.T, entry []byte, name string, entryType byte) {
	t.Helper()
	encoded := make([]byte, 0, (len(name)+1)*2)
	for _, character := range name {
		require.Less(t, character, rune(0x10000))
		encoded = binary.LittleEndian.AppendUint16(encoded, uint16(character))
	}
	encoded = binary.LittleEndian.AppendUint16(encoded, 0)
	require.LessOrEqual(t, len(encoded), 64)
	copy(entry, encoded)
	binary.LittleEndian.PutUint16(entry[64:66], uint16(len(encoded)))
	entry[66] = entryType
	binary.LittleEndian.PutUint32(entry[68:72], compoundNoStream)
	binary.LittleEndian.PutUint32(entry[72:76], compoundNoStream)
	binary.LittleEndian.PutUint32(entry[76:80], compoundNoStream)
}

type observedReadCloser struct {
	io.Reader

	closed   bool
	closeErr error
	onClose  func()
}

func (r *observedReadCloser) Close() error {
	r.closed = true
	if r.onClose != nil {
		r.onClose()
	}
	return r.closeErr
}

func zeroSHA256() string {
	return string(bytes.Repeat([]byte{'0'}, sha256.Size*2))
}

func testPDFIncrementalUpdateWithoutFinalMarker(t *testing.T) []byte {
	t.Helper()
	original := testPDF("incremental")
	previousXRef := bytes.Index(original, []byte("\nxref\n")) + 1
	require.Positive(t, previousXRef)
	var output bytes.Buffer
	output.Write(original)
	page := output.Len()
	output.WriteString("3 0 obj\n<< /Type /Page /Parent 2 0 R /MediaBox [0 0 300 300] >>\nendobj\n")
	xref := output.Len()
	_, _ = fmt.Fprintf(&output, "xref\n3 1\n%010d 00000 n \ntrailer\n<< /Size 4 /Root 1 0 R /Prev %d >>\nstartxref\n%d\n",
		page, previousXRef, xref)
	content := output.Bytes()
	require.Equal(t, 1, bytes.Count(content, []byte("%%EOF")))
	require.Less(t, bytes.Index(content, []byte("%%EOF")), xref)
	return content
}

func testPDFXRefStreamWithPageBox() []byte {
	var output bytes.Buffer
	output.WriteString("%PDF-1.5\n")
	offsets := make([]int, 3)
	for index, object := range []string{
		"<< /Type /Catalog /Pages 2 0 R >>",
		"<< /Type /Pages /Kids [3 0 R] /Count 1 >>",
		"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 300 300] >>",
	} {
		offsets[index] = output.Len()
		_, _ = fmt.Fprintf(&output, "%d 0 obj\n%s\nendobj\n", index+1, object)
	}
	xref := output.Len()
	entries := make([]byte, 5*7)
	putTestXRefEntry(entries, 0, 0, 0, 65_535)
	for index, offset := range offsets {
		putTestXRefEntry(entries, index+1, 1, uint32(offset), 0) // #nosec G115 -- bounded fixture.
	}
	putTestXRefEntry(entries, 4, 1, uint32(xref), 0) // #nosec G115 -- bounded fixture.
	_, _ = fmt.Fprintf(&output,
		"4 0 obj\n<< /Type /XRef /Size 5 /Root 1 0 R /W [1 4 2] /Length %d >>\nstream\n", len(entries))
	output.Write(entries)
	_, _ = fmt.Fprintf(&output, "\nendstream\nendobj\nstartxref\n%d\n%%%%EOF\n", xref)
	return output.Bytes()
}
