package formatdetect

import (
	"archive/zip"
	"bytes"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
)

func TestCountPDFPagesReaderCountsExactPageTree(t *testing.T) {
	data := testPDFPages(2)
	pages, err := CountPDFPagesReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	if pages != 2 {
		t.Fatalf("CountPDFPagesReader() = %d, want 2", pages)
	}
}

func TestCountPDFPagesReaderRejectsMalformedZeroAndOverBoundInput(t *testing.T) {
	tests := []struct {
		name string
		data []byte
		size int64
		want string
	}{
		{name: "zero", size: 0, want: "nonempty"},
		{name: "malformed", data: []byte("%PDF-1.7\n"), size: 9, want: "PDF"},
		{name: "short reader", data: []byte("short"), size: 6, want: "changed"},
		{name: "over bound", size: MaxDocumentBytes + 1, want: "byte limit"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := CountPDFPagesReader(bytes.NewReader(test.data), test.size)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("CountPDFPagesReader() error = %v, want substring %q", err, test.want)
			}
		})
	}
}

func TestDetectFormatRejectsUnsafeZIPNamesAndExpansion(t *testing.T) {
	unsafe := zipBytes(t, func(writer *zip.Writer) error {
		_, err := writer.Create("../escape")
		if err != nil {
			return fmt.Errorf("create unsafe ZIP entry: %w", err)
		}
		return nil
	})
	_, err := DetectFormat(bytes.NewReader(unsafe), int64(len(unsafe)), "application/vnd.openxmlformats-officedocument.wordprocessingml.document")
	if err == nil || !strings.Contains(err.Error(), "traversing") {
		t.Fatalf("unsafe ZIP error = %v", err)
	}

	expanded := zipBytes(t, func(writer *zip.Writer) error {
		header := &zip.FileHeader{Name: "word/document.xml", Method: zip.Store, UncompressedSize64: maxZIPSingleExpandedByte + 1}
		_, err := writer.CreateRaw(header)
		if err != nil {
			return fmt.Errorf("create expanded ZIP entry: %w", err)
		}
		return nil
	})
	_, err = DetectFormat(bytes.NewReader(expanded), int64(len(expanded)), "application/vnd.openxmlformats-officedocument.wordprocessingml.document")
	if err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("expanded ZIP error = %v", err)
	}
}

func TestCountPDFPagesReaderDoesNotReadBeyondRequestedBytes(t *testing.T) {
	reader := boundedReaderAt{data: []byte("%PDF-1.7\n"), max: 4}
	_, err := CountPDFPagesReader(reader, int64(len(reader.data)))
	if !errors.Is(err, io.ErrUnexpectedEOF) && (err == nil || !strings.Contains(err.Error(), "changed")) {
		t.Fatalf("CountPDFPagesReader() error = %v, want bounded read failure", err)
	}
}

func testPDFPages(pages int) []byte {
	objects := []string{"<< /Type /Catalog /Pages 2 0 R >>"}
	kids := make([]string, pages)
	for index := range pages {
		kids[index] = fmt.Sprintf("%d 0 R", index+3)
	}
	objects = append(objects, fmt.Sprintf("<< /Type /Pages /Kids [%s] /Count %d >>", strings.Join(kids, " "), pages))
	for range pages {
		objects = append(objects, "<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] >>")
	}
	var output bytes.Buffer
	output.WriteString("%PDF-1.4\n")
	offsets := make([]int, len(objects))
	for index, object := range objects {
		offsets[index] = output.Len()
		fmt.Fprintf(&output, "%d 0 obj\n%s\nendobj\n", index+1, object)
	}
	xref := output.Len()
	fmt.Fprintf(&output, "xref\n0 %d\n0000000000 65535 f \n", len(objects)+1)
	for _, offset := range offsets {
		fmt.Fprintf(&output, "%010d 00000 n \n", offset)
	}
	fmt.Fprintf(&output, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(objects)+1, xref)
	return output.Bytes()
}

func zipBytes(t *testing.T, write func(*zip.Writer) error) []byte {
	t.Helper()
	var data bytes.Buffer
	writer := zip.NewWriter(&data)
	if err := write(writer); err != nil {
		t.Fatalf("write test ZIP: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close test ZIP: %v", err)
	}
	return data.Bytes()
}

type boundedReaderAt struct {
	data []byte
	max  int
}

func (r boundedReaderAt) ReadAt(p []byte, offset int64) (int, error) {
	if len(p) > r.max {
		return 0, io.ErrUnexpectedEOF
	}
	read, err := bytes.NewReader(r.data).ReadAt(p, offset)
	if err != nil {
		return read, fmt.Errorf("read bounded test data: %w", err)
	}
	return read, nil
}
