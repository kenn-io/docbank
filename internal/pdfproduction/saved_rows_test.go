package pdfproduction

import (
	"bytes"
	"compress/zlib"
	"context"
	"errors"
	"image"
	"testing"

	"github.com/stretchr/testify/require"
)

func savedStream(t *testing.T, pixels []byte, level int) []byte {
	t.Helper()
	var b bytes.Buffer
	w, err := zlib.NewWriterLevel(&b, level)
	require.NoError(t, err)
	_, err = w.Write(pixels)
	require.NoError(t, err)
	require.NoError(t, w.Close())
	return b.Bytes()
}

func TestSavedRGBRows(t *testing.T) {
	pixels := []byte{0, 0, 0, 10, 20, 30}
	data := savedStream(t, pixels, zlib.DefaultCompression)
	rows, err := openSavedRGBRows(t.Context(), bytes.NewReader(data), 2, 1,
		[]image.Rectangle{image.Rect(0, 0, 1, 1)})
	require.NoError(t, err)
	row, err := rows.Next()
	require.NoError(t, err)
	require.Equal(t, []byte{0, 0, 0, 255, 10, 20, 30, 255}, row)
	require.NoError(t, rows.Finish())
	require.NoError(t, rows.Close())
}

func TestSavedRGBStreamRefusals(t *testing.T) {
	valid := savedStream(t, []byte{0, 0, 0}, zlib.DefaultCompression)
	dict := append([]byte{0x78, 0x20, 0, 0, 0, 1}, valid[2:]...)
	for name, data := range map[string][]byte{
		"appended payload":  append(bytes.Clone(valid), []byte("hidden")...),
		"second stream":     append(bytes.Clone(valid), valid...),
		"empty dictionary":  dict,
		"extra pixels":      savedStream(t, []byte{0, 0, 0, 0}, zlib.DefaultCompression),
		"short pixels":      savedStream(t, []byte{0, 0}, zlib.DefaultCompression),
		"truncated trailer": valid[:len(valid)-1],
		"checksum":          append(bytes.Clone(valid[:len(valid)-1]), valid[len(valid)-1]^1),
		"mask pixel":        savedStream(t, []byte{0, 0, 1}, zlib.DefaultCompression),
	} {
		t.Run(name, func(t *testing.T) {
			rows, err := openSavedRGBRows(t.Context(), bytes.NewReader(data), 1, 1, []image.Rectangle{image.Rect(0, 0, 1, 1)})
			if err == nil {
				_, err = rows.Next()
				if err == nil {
					err = rows.Finish()
				}
				err = errors.Join(err, savedStreamError(rows.Close()))
			}
			require.ErrorIs(t, err, ErrPDFIntegrity)
		})
	}
}

func TestSavedRGBReadErrors(t *testing.T) {
	failure := errors.New("synthetic storage read failure")
	_, err := openSavedRGBRows(t.Context(), savedFailedReader{failure}, 1, 1, nil)
	require.ErrorIs(t, err, failure)
	require.NotErrorIs(t, err, ErrPDFIntegrity)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = openSavedRGBRows(ctx, bytes.NewReader(savedStream(t, []byte{0, 0, 0}, zlib.DefaultCompression)), 1, 1, nil)
	require.ErrorIs(t, err, context.Canceled)
}

type savedFailedReader struct{ err error }

func (r savedFailedReader) Read([]byte) (int, error) { return 0, r.err }
