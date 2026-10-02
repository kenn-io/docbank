package main

import (
	"crypto/sha256"
	"errors"
	"io"
	"mime"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestPutSourceMIMEUsesBytesBeforeExtension(t *testing.T) {
	const extension = ".docbank561"
	require.NoError(t, mime.AddExtensionType(extension, "application/jpg"))
	require.Equal(t, "application/jpg", mime.TypeByExtension(extension))

	got, err := putSourceMIME("photo"+extension, []byte{0xff, 0xd8, 0xff}, "")
	require.NoError(t, err)
	require.Equal(t, "image/jpeg", got)
}

func TestPutSourceMIMEKeepsExplicitOverride(t *testing.T) {
	got, err := putSourceMIME("photo.jpg", []byte{0xff, 0xd8, 0xff}, "application/x-user; version=1")
	require.NoError(t, err)
	require.Equal(t, "application/x-user; version=1", got)
}

func TestPutSourceMIMEUsesBytesFromHashPass(t *testing.T) {
	oldBytes := make([]byte, 600)
	copy(oldBytes, "\x89PNG\r\n\x1a\n")
	newBytes := make([]byte, len(oldBytes))
	copy(newBytes, []byte{0xff, 0xd8, 0xff, 0xe0})
	source := &switchingReadSeeker{current: oldBytes, next: newBytes, switchOnRewind: true}

	hash := sha256.New()
	read, prefix, err := hashPutSource(source, hash)
	require.NoError(t, err)
	require.Equal(t, int64(len(oldBytes)), read)
	wantHash := sha256.Sum256(oldBytes)
	require.Equal(t, wantHash[:], hash.Sum(nil))
	mimeType, err := putSourceMIME("photo.jpg", prefix, "")
	require.NoError(t, err)
	require.Equal(t, "image/png", mimeType)
	require.True(t, source.switchOnRewind)
}

type switchingReadSeeker struct {
	current        []byte
	next           []byte
	pos            int
	switchOnRewind bool
}

func (r *switchingReadSeeker) Read(p []byte) (int, error) {
	if r.pos == len(r.current) {
		return 0, io.EOF
	}
	n := copy(p, r.current[r.pos:])
	r.pos += n
	return n, nil
}

func (r *switchingReadSeeker) Seek(offset int64, whence int) (int64, error) {
	if whence != io.SeekStart || offset < 0 || offset > int64(len(r.current)) {
		return 0, errors.New("unsupported seek")
	}
	if r.switchOnRewind && offset == 0 {
		r.current = r.next
		r.switchOnRewind = false
	}
	r.pos = int(offset)
	return offset, nil
}
