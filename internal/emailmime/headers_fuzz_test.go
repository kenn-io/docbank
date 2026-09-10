package emailmime

import (
	"bufio"
	"bytes"
	"context"
	"testing"
)

func FuzzReadHeaderBlockFinite(f *testing.F) {
	f.Add([]byte("A: b\r\n\r\n"))
	f.Add([]byte(" bad\n\n"))
	f.Fuzz(func(t *testing.T, data []byte) {
		var totalBytes int64
		totalFields := 0
		reader := bufio.NewReaderSize(bytes.NewReader(data), 17)
		block, err := readHeaderBlock(context.Background(), reader, 64, 8, &totalBytes, 128, &totalFields, 16)
		if err == nil && len(block.raw) > 64 {
			t.Fatalf("header reader retained %d bytes", len(block.raw))
		}
	})
}

func FuzzMultipartBoundaryScannerFinite(f *testing.F) {
	f.Add([]byte("hello\r\n--b\r\n"))
	f.Add([]byte("--b-prefix"))
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > 4096 {
			t.Skip()
		}
		n, _ := scanUntilBoundary(data, []byte("--b"), []byte("\r\n--b"), 0, nil)
		if n < 0 || n > len(data) {
			t.Fatalf("scanner returned %d for %d bytes", n, len(data))
		}
	})
}
