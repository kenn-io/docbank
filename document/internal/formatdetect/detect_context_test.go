package formatdetect

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDetectFormatContextCancelsAfterReadingStarts(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	reader := cancelingReaderAt{reader: bytes.NewReader([]byte("plain text")), cancel: cancel}
	_, err := DetectFormatContext(ctx, reader, int64(reader.reader.Len()), "text/plain")
	require.ErrorIs(t, err, context.Canceled)
}

func TestDetectFormatContextCancelsDuringZIPDecompression(t *testing.T) {
	var buffer bytes.Buffer
	writer := zip.NewWriter(&buffer)
	file, err := writer.Create("mimetype")
	require.NoError(t, err)
	_, err = file.Write([]byte("application/epub+zip"))
	require.NoError(t, err)
	file, err = writer.Create("META-INF/container.xml")
	require.NoError(t, err)
	_, err = file.Write([]byte(`<container xmlns="urn:oasis:names:tc:opendocument:xmlns:container"><rootfiles><rootfile full-path="book.opf"/></rootfiles></container>`))
	require.NoError(t, err)
	require.NoError(t, writer.Close())
	data := buffer.Bytes()
	dataOffset := int64(30 + binary.LittleEndian.Uint16(data[26:28]) + binary.LittleEndian.Uint16(data[28:30]))
	ctx, cancel := context.WithCancel(t.Context())
	reads, readsAfterCancel := 0, 0
	reader := &cancelOnOffsetReaderAt{reader: bytes.NewReader(data), offset: dataOffset, cancel: cancel, reads: &reads, readsAfterCancel: &readsAfterCancel}
	_, err = DetectFormatContext(ctx, reader, int64(len(data)), "application/epub+zip")
	require.ErrorIs(t, err, context.Canceled)
	require.Positive(t, reads)
	require.Zero(t, readsAfterCancel)
}

type cancelingReaderAt struct {
	reader *bytes.Reader
	cancel context.CancelFunc
}

func (reader cancelingReaderAt) ReadAt(buffer []byte, offset int64) (int, error) {
	read, err := reader.reader.ReadAt(buffer, offset)
	reader.cancel()
	if err != nil {
		return read, fmt.Errorf("read test data: %w", err)
	}
	return read, nil
}

type cancelOnOffsetReaderAt struct {
	reader           *bytes.Reader
	offset           int64
	cancel           context.CancelFunc
	reads            *int
	readsAfterCancel *int
	canceled         bool
}

func (reader *cancelOnOffsetReaderAt) ReadAt(buffer []byte, offset int64) (int, error) {
	if reader.canceled {
		*reader.readsAfterCancel++
	}
	*reader.reads++
	read, err := reader.reader.ReadAt(buffer, offset)
	if offset == reader.offset {
		reader.cancel()
		reader.canceled = true
	}
	if err != nil {
		return read, fmt.Errorf("read test data: %w", err)
	}
	return read, nil
}
