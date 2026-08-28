package client

import (
	"bytes"
	"io"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type oversizedBody struct {
	remaining int64
	read      int64
	closed    bool
}

func (body *oversizedBody) Read(p []byte) (int, error) {
	if body.remaining == 0 {
		return 0, io.EOF
	}
	n := min(int64(len(p)), body.remaining)
	for i := range p[:n] {
		p[i] = 'x'
	}
	body.remaining -= n
	body.read += n
	return int(n), nil
}

func (body *oversizedBody) Close() error {
	body.closed = true
	return nil
}

func TestVerifiedStreamsStopAtDeclaredSizePlusOne(t *testing.T) {
	const declared = int64(3)
	const oversized = int64(64<<20 + 1)

	t.Run("content", func(t *testing.T) {
		body := &oversizedBody{remaining: oversized}
		stream := &ContentStream{ReadCloser: body, Size: declared}

		written, err := stream.CopyVerified(io.Discard)

		require.ErrorContains(t, err, "received 4 bytes, expected 3")
		require.ErrorIs(t, err, ErrIntegrity)
		assert.Equal(t, declared+1, written)
		assert.Equal(t, declared+1, body.read, "the oversized response must not be drained")
		assert.True(t, body.closed, "an overflowing response must be closed immediately")
	})

	t.Run("rendition", func(t *testing.T) {
		body := &oversizedBody{remaining: oversized}
		stream := &RenditionStream{ContentStream: &ContentStream{ReadCloser: body, Size: declared}}
		var published bytes.Buffer

		written, err := stream.CopyVerified(&published)

		require.ErrorContains(t, err, "received 4 bytes, expected 3")
		require.ErrorIs(t, err, ErrIntegrity)
		assert.Equal(t, declared+1, written)
		assert.Equal(t, declared+1, body.read, "the oversized rendition must not be drained")
		assert.True(t, body.closed, "an overflowing rendition must be closed immediately")
		assert.Empty(t, published.Bytes(), "unverified rendition bytes must stay in private staging")
	})
}

func TestRenditionRangeStopsAtRequestedSizePlusOne(t *testing.T) {
	body := &oversizedBody{remaining: 64<<20 + 1}
	stream := &RenditionRangeStream{ReadCloser: body, Start: 10, End: 13}
	var published bytes.Buffer

	written, err := stream.CopyVerified(&published)

	require.ErrorContains(t, err, "received 4 bytes, expected 3")
	require.ErrorIs(t, err, ErrIntegrity)
	assert.Equal(t, int64(4), written)
	assert.Equal(t, int64(4), body.read)
	assert.True(t, body.closed)
	assert.Empty(t, published.Bytes())
}
