package media_test

import (
	"image/color"
	"testing"

	"github.com/stretchr/testify/assert"
	"go.kenn.io/docbank/document/media"
	"go.kenn.io/docbank/document/media/mediatest"
)

// FuzzDetectBytes checks that container sniffing never panics on arbitrary
// bytes and that every accepted result carries positive dimensions and the
// exact input size.
func FuzzDetectBytes(f *testing.F) {
	for _, seed := range [][]byte{
		mediatest.JPEG(8, 6, color.White),
		mediatest.PNG(12, 8, color.Black),
		mediatest.GIF(16, 10, 2),
		mediatest.GIFShifted(16, 10, 2, 3),
		mediatest.WebP(20, 14),
		mediatest.MP4(64, 48, 1000),
		mediatest.MP4(0, 0, 0),
		[]byte("%PDF-1.4\n"),
		[]byte("RIFF\x00\x00\x00\x00WEBP"),
		{},
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		metadata, err := media.DetectBytes(data, "application/octet-stream")
		if err != nil {
			assert.Equal(t, media.Metadata{}, metadata)
			return
		}
		assert.Positive(t, metadata.Width)
		assert.Positive(t, metadata.Height)
		assert.Equal(t, int64(len(data)), metadata.Size)
		assert.Equal(t, "application/octet-stream", metadata.DeclaredMediaType)
	})
}
