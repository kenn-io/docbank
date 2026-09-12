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
