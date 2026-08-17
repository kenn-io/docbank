package mistral

import (
	"archive/zip"
	"bytes"
	"encoding/binary"
	"io"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
		{name: "PDF", content: []byte("%PDF-1.7\nsynthetic"), mediaType: "application/pdf", wantID: "pdf"},
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
		{name: "Go", content: []byte("package synthetic\n"), mediaType: "text/x-go", wantID: "go"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			format, err := DetectFormat(bytes.NewReader(test.content), int64(len(test.content)), test.mediaType)
			require.NoError(t, err)
			assert.Equal(t, test.wantID, format.ID)
		})
	}
}

func TestDetectFormatRejectsMismatchUnsafeZIPAndAmbiguousCompound(t *testing.T) {
	require := require.New(t)
	pdf := []byte("%PDF-1.7\nsynthetic")
	_, err := DetectFormat(bytes.NewReader(pdf), int64(len(pdf)), "text/plain")
	require.ErrorContains(err, "not declared")

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
}

func (r *observedReadCloser) Close() error {
	r.closed = true
	return r.closeErr
}

func stringsOfZero(length int) string {
	return string(bytes.Repeat([]byte{'0'}, length))
}
