package mistral

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document/internal/formatdetect"
)

func TestDetectFormatAcceptsBoundedPDFTrailingData(t *testing.T) {
	linearized, err := os.ReadFile("testdata/linearized.pdf")
	require.NoError(t, err)
	linearizedStream, err := os.ReadFile("testdata/linearized-xref-stream.pdf")
	require.NoError(t, err)
	scanner := []byte("\r\nscanner padding\x00\xff\x01\r\nEND SCAN\n")
	incremental := append(testPDFIncrementalUpdateWithoutFinalMarker(t), []byte("%%EOF\n")...)
	for _, test := range []struct {
		name        string
		pdf, suffix []byte
	}{
		{"whitespace and NULs", testPDF("padding"), []byte(" \t\r\n\f\x00\x00")},
		{"junk after final marker", testPDF("trailing-junk"), []byte("producer residue\n")},
		{"junk without a separator", bytes.TrimSuffix(testPDF("adjacent-junk"), []byte("\n")), []byte("producer residue\n")},
		{"scanner output", testPDF("scanner"), scanner},
		{"xref stream scanner output", testPDFXRefStreamWithPageBox(), scanner},
		{"large xref stream", pdfTailXRefStream(800, false), scanner},
		{"indirect stream length", pdfTailXRefStream(6, true), scanner},
		{"large indirect stream length", pdfTailXRefStream(800, true), scanner},
		{"zero-padded indirect length after stream", pdfTailXRefStreamLength(6, true, true), scanner},
		{"incremental update", incremental, scanner},
		{"linearized scanner output", linearized, scanner},
		{"linearized xref stream", linearizedStream, scanner},
		{"linearized stream with table main xref", pdfTailLinearizedStreamWithTableMain(), scanner},
	} {
		t.Run(test.name, func(t *testing.T) {
			content := append(bytes.Clone(test.pdf), test.suffix...)
			format, err := DetectFormat(bytes.NewReader(content), int64(len(content)), "application/pdf")
			require.NoError(t, err)
			require.Equal(t, "pdf", format.ID)
			pages, err := formatdetect.CountPDFPages(content)
			require.NoError(t, err)
			require.Equal(t, int64(1), pages)
		})
	}
	// Bounded recognition preserves missing-marker recovery. Repair of the
	// compressed page tree is a separate parser contract.
	t.Run("linearized stream missing final marker", func(t *testing.T) {
		content := bytes.TrimSuffix(linearizedStream, []byte("%%EOF\n"))
		format, err := DetectFormat(bytes.NewReader(content), int64(len(content)), "application/pdf")
		require.NoError(t, err)
		require.Equal(t, "pdf", format.ID)
	})
}

func TestDetectFormatUsesLastStandaloneStartXRef(t *testing.T) {
	t.Run("accepts metadata with embedded keyword", func(t *testing.T) {
		content := append(testPDF("scanner-metadata"), []byte("scanner_last_startxref=123\n")...)
		format, err := DetectFormat(bytes.NewReader(content), int64(len(content)), "application/pdf")
		require.NoError(t, err)
		require.Equal(t, "pdf", format.ID)
		pages, err := formatdetect.CountPDFPages(content)
		require.NoError(t, err)
		require.Equal(t, int64(1), pages)
	})

	t.Run("rejects later standalone keyword with invalid offset", func(t *testing.T) {
		content := append(testPDF("scanner-invalid-startxref"), []byte("scanner_last_startxref=123\nstartxref\ninvalid\n")...)
		_, err := DetectFormat(bytes.NewReader(content), int64(len(content)), "application/pdf")
		require.ErrorContains(t, err, "PDF startxref offset is invalid")
	})
}

func TestDetectFormatAcceptsPlusPrefixedIndirectPDFStreamLength(t *testing.T) {
	content := pdfTailXRefStreamLength(6, true, true)
	lengthObject := []byte("5 0 obj\n42\nendobj\n")
	require.Equal(t, 1, bytes.Count(content, lengthObject))
	content = bytes.Replace(content, lengthObject, []byte("5 0 obj\n+42\nendobj\n"), 1)
	content = append(content, []byte("\r\nscanner padding\x00\xff\x01\r\nEND SCAN\n")...)

	format, err := DetectFormat(bytes.NewReader(content), int64(len(content)), "application/pdf")
	require.NoError(t, err)
	require.Equal(t, "pdf", format.ID)
	pages, err := formatdetect.CountPDFPages(content)
	require.NoError(t, err)
	require.Equal(t, int64(1), pages)
}

func TestDetectFormatAcceptsEOFMentionInCommentBeforeStartXRef(t *testing.T) {
	for _, lineEnding := range []string{"\n", "\r", "\r\n"} {
		t.Run(fmt.Sprintf("line-ending-%q", lineEnding), func(t *testing.T) {
			content := pdfTailXRefStreamLength(6, true, true)
			startXRef := bytes.LastIndex(content, []byte("startxref\n"))
			require.NotEqual(t, -1, startXRef)
			comment := []byte("% producer note: %%EOF follows" + lineEnding)
			content = bytes.Join([][]byte{content[:startXRef], comment, content[startXRef:]}, nil)

			format, err := DetectFormat(bytes.NewReader(content), int64(len(content)), "application/pdf")
			require.NoError(t, err)
			require.Equal(t, "pdf", format.ID)
			pages, err := formatdetect.CountPDFPages(content)
			require.NoError(t, err)
			require.Equal(t, int64(1), pages)
		})
	}
}

func TestDetectFormatRejectsStandaloneEOFPriorToStartXRef(t *testing.T) {
	for _, lineEnding := range []string{"\n", "\r", "\r\n"} {
		t.Run(fmt.Sprintf("line-ending-%q", lineEnding), func(t *testing.T) {
			content := pdfTailXRefStreamLength(6, true, true)
			startXRef := bytes.LastIndex(content, []byte("startxref\n"))
			require.NotEqual(t, -1, startXRef)
			marker := []byte(" \t%%EOF \t" + lineEnding)
			content = bytes.Join([][]byte{content[:startXRef], marker, content[startXRef:]}, nil)

			_, err := DetectFormat(bytes.NewReader(content), int64(len(content)), "application/pdf")
			require.ErrorContains(t, err, "PDF cross-reference data is invalid")
		})
	}
}

func TestDetectFormatRejectsForgedIndirectStreamClosureOutsideTail(t *testing.T) {
	content := pdfTailXRefStream(6, true)
	startXRef := bytes.LastIndex(content, []byte("startxref\n"))
	require.NotEqual(t, -1, startXRef)
	offset := bytes.Fields(content[startXRef+len("startxref\n"):])[0]
	content = append(content, bytes.Repeat([]byte{'x'}, 64<<10)...)
	content = append(content, []byte("PK\x03\x04synthetic\nendstream\nendobj\nstartxref\n"+string(offset)+"\n%%EOF\n")...)

	_, err := DetectFormat(bytes.NewReader(content), int64(len(content)), "application/pdf")
	require.ErrorContains(t, err, "PDF cross-reference data is invalid")
}

// An indirect length cannot be followed outside the tail, so an xref stream
// starting before the tail needs a direct length.
func TestDetectFormatRejectsIndirectPDFStreamLengthStartingBeforeTail(t *testing.T) {
	content := pdfTailXRefStream(12000, true)
	_, err := DetectFormat(bytes.NewReader(content), int64(len(content)), "application/pdf")
	require.ErrorContains(t, err, "PDF cross-reference data is invalid")
}

func TestDetectFormatAcceptsPDFXRefStreamClosureVariants(t *testing.T) {
	streamVariant := func(old, replacement string) []byte {
		content := testPDFXRefStreamWithPageBox()
		require.Equal(t, 1, bytes.Count(content, []byte(old)))
		return bytes.Replace(content, []byte(old), []byte(replacement), 1)
	}
	for _, test := range []struct {
		name    string
		content []byte
	}{
		{"filtered stream with indirect length", pdfTailFilteredXRefStreamIndirectLength(t)},
		{"length object in an object stream", pdfTailXRefStreamCompressedLengthObject()},
		{"high-numbered length object after stream", pdfTailXRefStreamLengthObject(800, true, true, 799)},
		{"length longer than data", streamVariant("/Length 35 ", "/Length 37 ")},
		{"carriage return after stream keyword", streamVariant(">>\nstream\n", ">>\nstream\r")},
		{"space before stream line feed", streamVariant(">>\nstream\n", ">>\nstream \n")},
		{"object before startxref", streamVariant("endobj\nstartxref", "endobj\n9 0 obj\n<< /Producer (x) >>\nendobj\nstartxref")},
	} {
		t.Run(test.name, func(t *testing.T) {
			format, err := DetectFormat(bytes.NewReader(test.content), int64(len(test.content)), "application/pdf")
			require.NoError(t, err)
			require.Equal(t, "pdf", format.ID)
		})
	}
}

func TestDetectFormatRejectsUnparsedDataBeforePDFXRefStreamStartXRef(t *testing.T) {
	for _, inserted := range []string{
		"PK\x03\x04synthetic\n",
		"9 0 obj\n<< /Length 1 >>\nstream\nx\nendstream\nendobj\n",
		"9 0 obj\nstartxref\nendobj\n",
		"9 0 obj\n<< /Producer (x) >>\n",
		"trailer\n<< /Size 5 /Root 1 0 R >>\n",
	} {
		t.Run(fmt.Sprintf("%q", inserted), func(t *testing.T) {
			content := testPDFXRefStreamWithPageBox()
			startXRef := bytes.LastIndex(content, []byte("startxref\n"))
			require.NotEqual(t, -1, startXRef)
			content = bytes.Join([][]byte{content[:startXRef], []byte(inserted), content[startXRef:]}, nil)

			_, err := DetectFormat(bytes.NewReader(content), int64(len(content)), "application/pdf")
			require.ErrorContains(t, err, "PDF cross-reference data is invalid")
		})
	}
}

func TestDetectFormatPDFTailWindow(t *testing.T) {
	const window = 64 << 10
	original := testPDF("bounded-tail")
	// Retain the existing requirement to validate the complete trailer from
	// the bounded tail, not merely find an EOF or startxref inside it.
	trailer := bytes.LastIndex(original, []byte("trailer\n"))
	padding := window - (len(original) - trailer)
	require.Positive(t, padding)
	for _, extra := range []int{0, 1} {
		t.Run(fmt.Sprintf("outside-window-%d", extra), func(t *testing.T) {
			content := append(bytes.Clone(original), bytes.Repeat([]byte{'x'}, padding+extra)...)
			_, err := DetectFormat(bytes.NewReader(content), int64(len(content)), "application/pdf")
			if extra == 0 {
				require.NoError(t, err)
			} else {
				require.ErrorContains(t, err, "cross-reference data is invalid")
			}
		})
	}
	content := append(bytes.Clone(original), bytes.Repeat([]byte{'x'}, window)...)
	_, err := DetectFormat(bytes.NewReader(content), int64(len(content)), "application/pdf")
	require.ErrorContains(t, err, "startxref is missing")
}

func TestDetectFormatLinearizedXRefOutsideTail(t *testing.T) {
	for _, padding := range []int{0, 8 << 20} {
		t.Run(strconv.Itoa(padding), func(t *testing.T) {
			content := pdfTailLinearizedStream(padding)
			format, err := DetectFormat(bytes.NewReader(content), int64(len(content)), "application/pdf")
			require.NoError(t, err)
			require.Equal(t, "pdf", format.ID)
			pages, err := formatdetect.CountPDFPages(content)
			require.NoError(t, err)
			require.Equal(t, int64(1), pages)
		})
	}
}

func TestDetectFormatAcceptsPlusPrefixedLinearizedPrev(t *testing.T) {
	for _, padding := range []int{0, 8 << 20} {
		t.Run(strconv.Itoa(padding), func(t *testing.T) {
			content := pdfTailLinearizedStream(padding)
			previous := bytes.Index(content, []byte("/Prev "))
			require.NotEqual(t, -1, previous)
			previous += len("/Prev ")
			require.Equal(t, byte('0'), content[previous])
			content[previous] = '+' // Keep the numeric value and every byte offset unchanged.

			format, err := DetectFormat(bytes.NewReader(content), int64(len(content)), "application/pdf")
			require.NoError(t, err)
			require.Equal(t, "pdf", format.ID)
			pages, err := formatdetect.CountPDFPages(content)
			require.NoError(t, err)
			require.Equal(t, int64(1), pages)
		})
	}
}

func TestDetectFormatRejectsUnsafePDFTrailingData(t *testing.T) {
	linearizedStream, err := os.ReadFile("testdata/linearized-xref-stream.pdf")
	require.NoError(t, err)
	for _, pdf := range []struct {
		name    string
		content []byte
	}{
		{"table", testPDF("negative-tail")},
		{"stream", testPDFXRefStreamWithPageBox()},
		{"indirect stream", pdfTailXRefStream(6, true)},
		{"linearized stream", linearizedStream},
		{"linearized stream with table main xref", pdfTailLinearizedStreamWithTableMain()},
	} {
		t.Run(pdf.name, func(t *testing.T) {
			xref := bytes.LastIndex(pdf.content, []byte("startxref\n"))
			offset := string(bytes.Fields(pdf.content[xref+len("startxref\n"):])[0])
			const notFinal = "PDF trailer is not final"
			const invalidXRef = "PDF cross-reference data is invalid"
			for _, test := range []struct{ suffix, want string }{
				{"PK\x03\x04synthetic", notFinal},
				{"padding\x00PK\x05\x06synthetic", notFinal},
				{"PK\x07\x08synthetic", notFinal},
				{"<html><body>synthetic</body></html>", notFinal},
				{"<!DOCTYPE html>synthetic", notFinal},
				{"<HTML>synthetic</HTML>", notFinal},
				{"4 0 obj\n<< >>\nendobj\n", notFinal},
				{"%%EOF\n", notFinal},
				{"%PDF-1.4\n", notFinal},
				{"startxref\ninvalid\n%%EOF\n", "PDF startxref offset is invalid"},
				{"startxref\n0\n%%EOF\n", "PDF startxref offset is outside the document"},
				{"PK\x03\x04synthetic\nstartxref\n" + offset + "\n%%EOF\n", invalidXRef},
				{"<html>synthetic</html>\nstartxref\n" + offset + "\n", invalidXRef},
				{"PK\x03\x04synthetic\nendstream\nendobj\nstartxref\n" + offset + "\n%%EOF\n", invalidXRef},
				{"scanner residue\nstartxref\n" + offset + "\n%%EOF\n", invalidXRef},
			} {
				t.Run(fmt.Sprintf("%q", test.suffix), func(t *testing.T) {
					content := append(bytes.Clone(pdf.content), test.suffix...)
					_, err := DetectFormat(bytes.NewReader(content), int64(len(content)), "application/pdf")
					require.ErrorContains(t, err, test.want)
				})
			}
		})
	}
	for _, test := range []struct {
		content []byte
		want    string
	}{
		{[]byte("plain text\n%%EOF\nscanner padding"), "no supported signature"},
		{[]byte("%PDF-1.7\n%%EOF\nscanner padding"), "PDF startxref is missing"},
		{bytes.Replace(testPDF("bad-version"), []byte("%PDF-1.4"), []byte("%PDF-3.0"), 1), "PDF header is invalid"},
		{bytes.Replace(testPDF("bad-xref"), []byte("xref\n"), []byte("xref garbage\n"), 1), "PDF cross-reference data is invalid"},
	} {
		content := append(bytes.Clone(test.content), []byte("scanner padding\n")...)
		_, err := DetectFormat(bytes.NewReader(content), int64(len(content)), "application/pdf")
		require.ErrorContains(t, err, test.want)
	}
	content := append(testPDF("mime-mismatch"), []byte("scanner padding\n")...)
	_, err = DetectFormat(bytes.NewReader(content), int64(len(content)), "text/plain")
	require.ErrorContains(t, err, "not declared")
}

// Build a complete synthetic page tree with unused free xref entries to
// exercise stream bodies larger than the detector's 4 KiB xref read.
func pdfTailXRefStream(count int, indirect bool) []byte {
	return pdfTailXRefStreamLength(count, indirect, false)
}

func pdfTailXRefStreamLength(count int, indirect, after bool) []byte {
	return pdfTailXRefStreamLengthObject(count, indirect, after, 5)
}

// Producers that write an indirect length after its stream usually give the
// length object the highest object number.
func pdfTailXRefStreamLengthObject(count int, indirect, after bool, lengthNumber int) []byte {
	var output bytes.Buffer
	output.WriteString("%PDF-1.5\n")
	offsets := make([]int, max(6, lengthNumber+1))
	for index, object := range []string{
		"<< /Type /Catalog /Pages 2 0 R >>",
		"<< /Type /Pages /Kids [3 0 R] /Count 1 >>",
		"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 300 300] >>",
	} {
		offsets[index+1] = output.Len()
		_, _ = fmt.Fprintf(&output, "%d 0 obj\n%s\nendobj\n", index+1, object)
	}
	length := strconv.Itoa(count * 7)
	if indirect && !after {
		offsets[lengthNumber] = output.Len()
		_, _ = fmt.Fprintf(&output, "%d 0 obj\n%s\nendobj\n", lengthNumber, length)
	}
	if indirect {
		length = fmt.Sprintf("%d 0 R", lengthNumber)
		if after {
			length = fmt.Sprintf("%05d 00000 R", lengthNumber)
		}
	}
	offsets[4] = output.Len()
	header := fmt.Sprintf("4 0 obj\n<< /Type /XRef /Size %d /Root 1 0 R /W [1 4 2] /Length %s >>\nstream\n", count, length)
	if indirect && after {
		offsets[lengthNumber] = output.Len() + len(header) + count*7 + len("\nendstream\nendobj\n")
	}
	entries := make([]byte, count*7)
	for index := range count {
		putTestXRefEntry(entries, index, 0, 0, 65535)
	}
	for index := 1; index < len(offsets); index++ {
		if offsets[index] != 0 {
			putTestXRefEntry(entries, index, 1, uint32(offsets[index]), 0) // #nosec G115 -- bounded synthetic fixture.
		}
	}
	output.WriteString(header)
	output.Write(entries)
	output.WriteString("\nendstream\nendobj\n")
	if indirect && after {
		_, _ = fmt.Fprintf(&output, "%d 0 obj\n%d\nendobj\n", lengthNumber, len(entries))
	}
	_, _ = fmt.Fprintf(&output, "startxref\n%d\n%%%%EOF\n", offsets[4])
	return output.Bytes()
}

func pdfTailFilteredXRefStreamIndirectLength(t *testing.T) []byte {
	t.Helper()
	var output bytes.Buffer
	output.WriteString("%PDF-1.5\n")
	offsets := make([]int, 6)
	for index, object := range []string{
		"<< /Type /Catalog /Pages 2 0 R >>",
		"<< /Type /Pages /Kids [3 0 R] /Count 1 >>",
		"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 300 300] >>",
	} {
		offsets[index+1] = output.Len()
		_, _ = fmt.Fprintf(&output, "%d 0 obj\n%s\nendobj\n", index+1, object)
	}
	offsets[5] = output.Len()
	lengthObject := []byte("5 0 obj\n00000000\nendobj\n")
	output.Write(lengthObject)
	offsets[4] = output.Len()
	entries := make([]byte, 6*7)
	for index := range 6 {
		putTestXRefEntry(entries, index, 0, 0, 65_535)
	}
	for index := 1; index <= 3; index++ {
		putTestXRefEntry(entries, index, 1, uint32(offsets[index]), 0) // #nosec G115 -- bounded synthetic fixture.
	}
	putTestXRefEntry(entries, 4, 1, uint32(offsets[4]), 0) // #nosec G115 -- bounded synthetic fixture.
	putTestXRefEntry(entries, 5, 1, uint32(offsets[5]), 0) // #nosec G115 -- bounded synthetic fixture.
	var compressed bytes.Buffer
	writer := zlib.NewWriter(&compressed)
	_, err := writer.Write(entries)
	require.NoError(t, err)
	require.NoError(t, writer.Close())
	output.WriteString("4 0 obj\n<< /Type /XRef /Size 6 /Root 1 0 R /W [1 4 2] /Length 5 0 R /Filter /FlateDecode >>\nstream\n")
	output.Write(compressed.Bytes())
	output.WriteString("\nendstream\nendobj\n")
	_, _ = fmt.Fprintf(&output, "startxref\n%d\n%%%%EOF\n", offsets[4])
	copy(output.Bytes()[offsets[5]+len("5 0 obj\n"):], fmt.Sprintf("%08d", compressed.Len()))
	return output.Bytes()
}

func pdfTailXRefStreamCompressedLengthObject() []byte {
	var output bytes.Buffer
	output.WriteString("%PDF-1.5\n")
	offsets := make([]int, 7)
	for index, object := range []string{
		"<< /Type /Catalog /Pages 2 0 R >>",
		"<< /Type /Pages /Kids [3 0 R] /Count 1 >>",
		"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 300 300] >>",
	} {
		offsets[index+1] = output.Len()
		_, _ = fmt.Fprintf(&output, "%d 0 obj\n%s\nendobj\n", index+1, object)
	}
	objectStreamData := []byte("5 0 49")
	offsets[6] = output.Len()
	_, _ = fmt.Fprintf(&output, "6 0 obj\n<< /Type /ObjStm /N 1 /First 4 /Length %d >>\nstream\n", len(objectStreamData))
	output.Write(objectStreamData)
	output.WriteString("\nendstream\nendobj\n")
	offsets[4] = output.Len()
	entries := make([]byte, 7*7)
	for index := range 7 {
		putTestXRefEntry(entries, index, 0, 0, 65_535)
	}
	for index := 1; index <= 4; index++ {
		putTestXRefEntry(entries, index, 1, uint32(offsets[index]), 0) // #nosec G115 -- bounded synthetic fixture.
	}
	putTestXRefEntry(entries, 5, 2, 6, 0)
	putTestXRefEntry(entries, 6, 1, uint32(offsets[6]), 0) // #nosec G115 -- bounded synthetic fixture.
	output.WriteString("4 0 obj\n<< /Type /XRef /Size 7 /Root 1 0 R /W [1 4 2] /Length 5 0 R >>\nstream\n")
	output.Write(entries)
	_, _ = fmt.Fprintf(&output, "\nendstream\nendobj\nstartxref\n%d\n%%%%EOF\n", offsets[4])
	return output.Bytes()
}

// A main xref stream larger than the tail drives the prefix and ReaderAt
// paths. Padding before that stream puts its header beyond the sniff prefix.
func pdfTailLinearizedStream(padding int) []byte {
	const count = 12000
	const header = "%PDF-1.5\n"
	original := pdfTailXRefStream(count, false)
	oldStart := bytes.LastIndex(original, []byte("startxref\n"))
	oldOffset, _ := strconv.Atoi(string(bytes.Fields(original[oldStart+len("startxref\n"):])[0]))
	linearization := "6 0 obj\n<< /Linearized 1 /H [0 0] >>\nendobj\n"
	firstOffset := len(header) + len(linearization)
	first := fmt.Sprintf("7 0 obj\n<< /Type /XRef /Size %d /Root 1 0 R /W [1 4 2] /Index [0 1] /Length 7 /Prev 0000000000 >>\nstream\n", count)
	firstEntry := make([]byte, 7)
	putTestXRefEntry(firstEntry, 0, 0, 0, 65535)
	first += string(firstEntry) + "\nendstream\nendobj\n"
	shift := len(linearization) + len(first) + padding
	mainOffset := oldOffset + shift
	first = strings.Replace(first, "0000000000", fmt.Sprintf("%010d", mainOffset), 1)
	var output bytes.Buffer
	output.WriteString(header + linearization + first)
	output.Write(bytes.Repeat([]byte{' '}, padding))
	output.Write(original[len(header):oldStart])
	_, _ = fmt.Fprintf(&output, "startxref\n%d\n%%%%EOF\nscanner residue\n", firstOffset)
	content := output.Bytes()
	dataStart := mainOffset + bytes.Index(content[mainOffset:], []byte("stream\n")) + len("stream\n")
	entries := content[dataStart : dataStart+count*7]
	for number := 1; number <= 4; number++ {
		oldEntry := entries[number*7 : number*7+7]
		offset := binary.BigEndian.Uint32(oldEntry[1:5])
		putTestXRefEntry(entries, number, 1, offset+uint32(shift), 0) // #nosec G115 -- bounded synthetic fixture.
	}
	putTestXRefEntry(entries, 6, 1, uint32(len(header)), 0)
	putTestXRefEntry(entries, 7, 1, uint32(firstOffset), 0) // #nosec G115 -- bounded synthetic fixture.
	return content
}

// A first-page xref stream may link forward to a main xref table whose trailer
// inherits the first-page /Root.
func pdfTailLinearizedStreamWithTableMain() []byte {
	const header = "%PDF-1.5\n"
	const linearization = "6 0 obj\n<< /Linearized 1 /H [0 0] >>\nendobj\n"
	firstOffset := len(header) + len(linearization)
	firstEntry := make([]byte, 7)
	putTestXRefEntry(firstEntry, 0, 0, 0, 65535)
	firstWithPrev := func(previous int) string {
		return fmt.Sprintf("7 0 obj\n<< /Type /XRef /Size 8 /Root 1 0 R /W [1 4 2] /Index [0 1] "+
			"/Length 7 /Prev %010d >>\nstream\n%s\nendstream\nendobj\n", previous, firstEntry)
	}
	var output bytes.Buffer
	output.WriteString(header + linearization + firstWithPrev(0))
	offsets := map[int]int{6: len(header), 7: firstOffset}
	for index, object := range []string{
		"<< /Type /Catalog /Pages 2 0 R >>",
		"<< /Type /Pages /Kids [3 0 R] /Count 1 >>",
		"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 300 300] >>",
	} {
		offsets[index+1] = output.Len()
		_, _ = fmt.Fprintf(&output, "%d 0 obj\n%s\nendobj\n", index+1, object)
	}
	mainOffset := output.Len()
	output.WriteString("xref\n0 8\n0000000000 65535 f \n")
	for number := 1; number < 8; number++ {
		if offset, ok := offsets[number]; ok {
			_, _ = fmt.Fprintf(&output, "%010d 00000 n \n", offset)
		} else {
			output.WriteString("0000000000 65535 f \n")
		}
	}
	_, _ = fmt.Fprintf(&output, "trailer\n<< /Size 8 >>\nstartxref\n%d\n%%%%EOF\n", firstOffset)
	content := output.Bytes()
	copy(content[firstOffset:], firstWithPrev(mainOffset))
	return content
}
