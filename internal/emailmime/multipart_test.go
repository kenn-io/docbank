package emailmime

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"mime/multipart"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type chunkReader struct {
	data []byte
	size int
}

func TestMultipartReaderMatchesStandardDecoderForWellFormedBodies(t *testing.T) {
	raw := []byte("--b\r\nX-Test: one\r\n\r\na\x00b\r\n--b\r\n\r\nsecond\r\n--b--\r\n")
	standard := multipart.NewReader(bytes.NewReader(raw), "b")
	var want [][]byte
	for {
		part, err := standard.NextRawPart()
		if errors.Is(err, io.EOF) {
			break
		}
		require.NoError(t, err)
		value, err := io.ReadAll(part)
		require.NoError(t, err)
		want = append(want, value)
	}
	var hb int64
	hf := 0
	reader := newMultipartReader(bytes.NewReader(raw), "b", func(buffered *bufio.Reader) (parsedHeaderBlock, error) {
		return readHeaderBlock(context.Background(), buffered, 1024, 10, &hb, 2048, &hf, 20)
	})
	var got [][]byte
	for {
		part, err := reader.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		require.NoError(t, err)
		value, err := io.ReadAll(part)
		require.NoError(t, err)
		got = append(got, value)
	}
	assert.Equal(t, want, got)
}

func (r *chunkReader) Read(p []byte) (int, error) {
	if len(r.data) == 0 {
		return 0, io.EOF
	}
	n := min(len(p), r.size, len(r.data))
	copy(p, r.data[:n])
	r.data = r.data[n:]
	return n, nil
}

func TestMultipartReaderHandlesEverySmallDelimiterSplitAndPrefixData(t *testing.T) {
	body := []byte("pre\r\n--boundary\r\nA: b\r\n\r\nline\r\n--boundary-prefix\r\nend\r\n--boundary--")
	for chunk := 1; chunk <= len("\r\n--boundary--"); chunk++ {
		t.Run(string(rune('A'+chunk)), func(t *testing.T) {
			var headerBytes int64
			headerFields := 0
			mr := newMultipartReader(&chunkReader{data: bytes.Clone(body), size: chunk}, "boundary", func(reader *bufio.Reader) (parsedHeaderBlock, error) {
				return readHeaderBlock(context.Background(), reader, 1024, 10, &headerBytes, 2048, &headerFields, 20)
			})
			part, err := mr.NextPart()
			require.NoError(t, err)
			got, err := io.ReadAll(part)
			require.NoError(t, err)
			assert.Equal(t, "line\r\n--boundary-prefix\r\nend", string(got))
			_, err = mr.NextPart()
			assert.ErrorIs(t, err, io.EOF)
		})
	}
}

func TestMultipartReaderRequiresPartConsumption(t *testing.T) {
	raw := bytes.NewBufferString("--b\r\n\r\nbody\r\n--b--\r\n")
	var hb int64
	hf := 0
	mr := newMultipartReader(raw, "b", func(reader *bufio.Reader) (parsedHeaderBlock, error) {
		return readHeaderBlock(context.Background(), reader, 100, 10, &hb, 100, &hf, 10)
	})
	_, err := mr.NextPart()
	require.NoError(t, err)
	_, err = mr.NextPart()
	require.Error(t, err)
}

func TestMultipartReaderReportsMissingClosingBoundary(t *testing.T) {
	raw := bytes.NewBufferString("--b\n\n" + string(bytes.Repeat([]byte{'x'}, 32<<10)))
	var hb int64
	hf := 0
	mr := newMultipartReader(raw, "b", func(reader *bufio.Reader) (parsedHeaderBlock, error) {
		return readHeaderBlock(context.Background(), reader, 100, 10, &hb, 100, &hf, 10)
	})
	part, err := mr.NextPart()
	require.NoError(t, err)
	_, err = io.Copy(io.Discard, part)
	assert.ErrorIs(t, err, errBoundaryUnclosed)
}
