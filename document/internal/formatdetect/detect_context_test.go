package formatdetect

import (
	"archive/zip"
	"bytes"
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type cancelAfterErrContext struct {
	calls    int
	cancelAt int
}

func (ctx *cancelAfterErrContext) Deadline() (time.Time, bool) { return time.Time{}, false }
func (ctx *cancelAfterErrContext) Done() <-chan struct{}       { return nil }
func (ctx *cancelAfterErrContext) Value(any) any               { return nil }

func (ctx *cancelAfterErrContext) Err() error {
	ctx.calls++
	if ctx.calls >= ctx.cancelAt {
		return context.Canceled
	}
	return nil
}

func TestVerifyZIPEntryContextCancelsDuringDecompression(t *testing.T) {
	var buffer bytes.Buffer
	writer := zip.NewWriter(&buffer)
	entry, err := writer.Create("payload")
	require.NoError(t, err)
	payload := make([]byte, 128<<10)
	for index := range payload {
		payload[index] = byte((index*31 + index/251) % 251)
	}
	_, err = entry.Write(payload)
	require.NoError(t, err)
	require.NoError(t, writer.Close())
	archive, err := zip.NewReader(bytes.NewReader(buffer.Bytes()), int64(buffer.Len()))
	require.NoError(t, err)

	ctx := &cancelAfterErrContext{cancelAt: 6}
	err = verifyZIPEntryContext(ctx, archive.File[0])
	require.ErrorIs(t, err, context.Canceled)
	require.GreaterOrEqual(t, ctx.calls, ctx.cancelAt)
}

func TestDetectFormatContextCancelsAfterReadingStarts(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	reader := cancelingReaderAt{reader: bytes.NewReader([]byte("plain text")), cancel: cancel}
	_, err := DetectFormatContext(ctx, reader, int64(reader.reader.Len()), "text/plain")
	require.ErrorIs(t, err, context.Canceled)
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
