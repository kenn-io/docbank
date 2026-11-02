package strictio

import (
	"errors"
	"io"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// chunks returns each entry as one Read, with err after the last one.
type chunks struct {
	data []string
	err  error
}

func (c *chunks) Read(p []byte) (int, error) {
	if len(c.data) == 0 {
		return 0, io.EOF
	}
	n := copy(p, c.data[0])
	c.data = c.data[1:]
	if len(c.data) == 0 {
		return n, c.err
	}
	return n, nil
}

func TestReadFull(t *testing.T) {
	errIntegrity := errors.New("integrity")
	for _, tt := range []struct {
		name    string
		src     *chunks
		size    int
		wantN   int
		wantErr error
	}{
		{name: "complete", src: &chunks{data: []string{"ab", "cd"}}, size: 4, wantN: 4},
		{name: "complete with plain EOF", src: &chunks{data: []string{"abcd"}, err: io.EOF}, size: 4, wantN: 4},
		{name: "error with final bytes", src: &chunks{data: []string{"ab", "cd"}, err: errIntegrity}, size: 4, wantN: 4, wantErr: errIntegrity},
		{name: "error joined to EOF", src: &chunks{data: []string{"abcd"}, err: errors.Join(io.EOF, errIntegrity)}, size: 4, wantN: 4, wantErr: errIntegrity},
		{name: "short", src: &chunks{data: []string{"ab"}, err: io.EOF}, size: 4, wantN: 2, wantErr: io.ErrUnexpectedEOF},
		{name: "empty", src: &chunks{}, size: 4, wantErr: io.EOF},
	} {
		t.Run(tt.name, func(t *testing.T) {
			n, err := ReadFull(tt.src, make([]byte, tt.size))
			assert.Equal(t, tt.wantN, n)
			if tt.wantErr == nil {
				require.NoError(t, err)
				return
			}
			require.ErrorIs(t, err, tt.wantErr)
		})
	}
}

func TestFill(t *testing.T) {
	errIntegrity := errors.New("integrity")
	for _, tt := range []struct {
		name    string
		src     *chunks
		wantN   int
		wantErr error
	}{
		{name: "full", src: &chunks{data: []string{"ab", "cd"}}, wantN: 4},
		{name: "full with plain EOF", src: &chunks{data: []string{"abcd"}, err: io.EOF}, wantN: 4, wantErr: io.EOF},
		{name: "clean short end", src: &chunks{data: []string{"ab"}, err: io.EOF}, wantN: 2, wantErr: io.EOF},
		{name: "source reports truncation", src: &chunks{data: []string{"ab"}, err: io.ErrUnexpectedEOF}, wantN: 2, wantErr: io.ErrUnexpectedEOF},
		{name: "error with final bytes", src: &chunks{data: []string{"abcd"}, err: errIntegrity}, wantN: 4, wantErr: errIntegrity},
		{name: "empty", src: &chunks{}, wantErr: io.EOF},
	} {
		t.Run(tt.name, func(t *testing.T) {
			n, err := Fill(tt.src, make([]byte, 4))
			assert.Equal(t, tt.wantN, n)
			assert.Equal(t, tt.wantErr, err)
		})
	}
}

func TestRangeEnd(t *testing.T) {
	errIntegrity := errors.New("integrity")
	assert.Equal(t, io.EOF, RangeEnd(io.EOF, 0))
	assert.Equal(t, io.ErrUnexpectedEOF, RangeEnd(io.EOF, 3))
	joined := RangeEnd(errors.Join(io.EOF, errIntegrity), 3)
	require.ErrorIs(t, joined, io.ErrUnexpectedEOF)
	require.ErrorIs(t, joined, errIntegrity)
	require.ErrorIs(t, RangeEnd(errIntegrity, 3), errIntegrity)
	require.NoError(t, RangeEnd(nil, 3))
}
