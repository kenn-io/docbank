package media_test

import (
	"bytes"
	"encoding/binary"
	"errors"
	"image/color"
	"io"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/document/media"
	"go.kenn.io/docbank/document/media/mediatest"
)

func TestDetectBytesRecognizesSupportedContainers(t *testing.T) {
	tests := []struct {
		name       string
		data       []byte
		declared   string
		wantFormat media.Format
		wantKind   media.Kind
		wantType   string
		width      int64
		height     int64
		frames     int
		duration   int64
		animated   bool
	}{
		{name: "jpeg", data: mediatest.JPEG(3, 2, nil), declared: "text/plain", wantFormat: media.FormatJPEG, wantKind: media.KindImage, wantType: "image/jpeg", width: 3, height: 2},
		{name: "png", data: mediatest.PNG(4, 3, nil), declared: "image/png", wantFormat: media.FormatPNG, wantKind: media.KindImage, wantType: "image/png", width: 4, height: 3},
		{name: "webp vp8x", data: mediatest.WebP(5, 4), declared: "image/webp", wantFormat: media.FormatWebP, wantKind: media.KindImage, wantType: "image/webp", width: 5, height: 4},
		{name: "webp vp8l", data: webPLossless(7, 6), declared: "", wantFormat: media.FormatWebP, wantKind: media.KindImage, wantType: "image/webp", width: 7, height: 6},
		{name: "webp vp8", data: webPLossy(9, 8), declared: "", wantFormat: media.FormatWebP, wantKind: media.KindImage, wantType: "image/webp", width: 9, height: 8},
		{name: "still gif", data: mediatest.GIF(2, 2, 1), declared: "image/gif", wantFormat: media.FormatGIF, wantKind: media.KindImage, wantType: "image/gif", width: 2, height: 2, frames: 1},
		{name: "animated gif", data: mediatest.GIF(2, 2, 3), declared: "image/gif", wantFormat: media.FormatGIF, wantKind: media.KindImage, wantType: "image/gif", width: 2, height: 2, frames: 3, animated: true},
		{name: "mp4 v0", data: mediatest.MP4(640, 360, 1250), declared: "video/quicktime", wantFormat: media.FormatMP4, wantKind: media.KindVideo, wantType: "video/mp4", width: 640, height: 360, duration: 1250},
		{name: "mp4 v1", data: mp4Version1(320, 240, 90_000, 30), declared: "video/mp4", wantFormat: media.FormatMP4, wantKind: media.KindVideo, wantType: "video/mp4", width: 320, height: 240, duration: 30000},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := media.DetectBytes(tt.data, tt.declared)
			require.NoError(t, err)
			assert.Equal(t, tt.wantFormat, got.Format)
			assert.Equal(t, tt.wantKind, got.Kind)
			assert.Equal(t, tt.wantType, got.MediaType)
			assert.Equal(t, tt.declared, got.DeclaredMediaType)
			assert.Equal(t, int64(len(tt.data)), got.Size)
			assert.Equal(t, tt.width, got.Width)
			assert.Equal(t, tt.height, got.Height)
			assert.Equal(t, tt.frames, got.FrameCount)
			assert.Equal(t, tt.duration, got.DurationMS)
			assert.Equal(t, tt.animated, got.Animated)
			assert.Equal(t, tt.width*tt.height, got.Pixels())
		})
	}
}

func TestDetectBytesRejectsUnsupportedAndMalformedInput(t *testing.T) {
	tests := []struct {
		name string
		data []byte
		want error
	}{
		{name: "empty", data: nil, want: media.ErrUnsupportedMedia},
		{name: "pdf", data: []byte("%PDF-1.7"), want: media.ErrUnsupportedMedia},
		{name: "ogg", data: []byte("OggS\x00synthetic"), want: media.ErrUnsupportedMedia},
		{name: "id3", data: []byte("ID3\x04\x00"), want: media.ErrUnsupportedMedia},
		{name: "text", data: []byte("hello world"), want: media.ErrUnsupportedMedia},
		{name: "truncated png", data: []byte("\x89PNG\r\n\x1a\nshort"), want: media.ErrMalformedMedia},
		{name: "truncated jpeg", data: []byte("\xff\xd8\xff"), want: media.ErrMalformedMedia},
		{name: "truncated webp", data: []byte("RIFF\x00\x00\x00\x00WEBPVP8X"), want: media.ErrMalformedMedia},
		{name: "unknown webp chunk", data: append([]byte("RIFF\x16\x00\x00\x00WEBPALPH"), make([]byte, 14)...), want: media.ErrMalformedMedia},
		{name: "gif without trailer", data: []byte("GIF89a\x02\x00\x02\x00\x00\x00\x00"), want: media.ErrMalformedMedia},
		{name: "gif zero width", data: []byte("GIF89a\x00\x00\x02\x00\x00\x00\x00;"), want: media.ErrMalformedMedia},
		{name: "mp4 without moov", data: mediatest.Box("ftyp", append([]byte("isom"), make([]byte, 12)...)), want: media.ErrMalformedMedia},
		{name: "mp4 bad box size", data: append(mediatest.Box("ftyp", append([]byte("isom"), make([]byte, 12)...)), 0, 0, 0, 3, 'm', 'o', 'o', 'v'), want: media.ErrMalformedMedia},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := media.DetectBytes(tt.data, "")
			require.ErrorIs(t, err, tt.want)
		})
	}
}

func TestDetectReadsThroughReaderAtAndBoundsSize(t *testing.T) {
	data := mediatest.PNG(2, 2, color.White)
	got, err := media.Detect(bytes.NewReader(data), int64(len(data)), "image/png")
	require.NoError(t, err)
	assert.Equal(t, media.FormatPNG, got.Format)

	_, err = media.Detect(bytes.NewReader(data), media.MaxBytes+1, "image/png")
	require.ErrorIs(t, err, media.ErrTooLarge)

	_, err = media.Detect(nil, 0, "")
	require.Error(t, err)

	_, err = media.Detect(failingReaderAt{}, 4, "")
	require.Error(t, err)
	require.NotErrorIs(t, err, media.ErrUnsupportedMedia)
}

func TestDetectRejectsDeeplyNestedMP4Boxes(t *testing.T) {
	payload := mediatest.Box("tkhd", make([]byte, 84))
	for range 12 {
		payload = mediatest.Box("trak", payload)
	}
	data := append(mediatest.Box("ftyp", append([]byte("isom"), make([]byte, 12)...)), mediatest.Box("moov", payload)...)
	_, err := media.DetectBytes(data, "")
	require.ErrorIs(t, err, media.ErrMalformedMedia)
}

type failingReaderAt struct{}

func (failingReaderAt) ReadAt([]byte, int64) (int, error) { return 0, errors.New("disk failure") }

var _ io.ReaderAt = failingReaderAt{}

func webPLossless(width, height int) []byte {
	data := make([]byte, 25)
	copy(data[0:4], "RIFF")
	copy(data[8:12], "WEBP")
	copy(data[12:16], "VP8L")
	data[20] = 0x2f
	packed := uint32(width-1) | uint32(height-1)<<14
	binary.LittleEndian.PutUint32(data[21:25], packed)
	return data
}

func webPLossy(width, height int) []byte {
	data := make([]byte, 30)
	copy(data[0:4], "RIFF")
	copy(data[8:12], "WEBP")
	copy(data[12:16], "VP8 ")
	copy(data[23:26], []byte{0x9d, 0x01, 0x2a})
	binary.LittleEndian.PutUint16(data[26:28], uint16(width))
	binary.LittleEndian.PutUint16(data[28:30], uint16(height))
	return data
}

func mp4Version1(width, height int, timescale uint32, seconds uint64) []byte {
	mvhd := make([]byte, 32)
	mvhd[0] = 1
	binary.BigEndian.PutUint32(mvhd[20:24], timescale)
	binary.BigEndian.PutUint64(mvhd[24:32], uint64(timescale)*seconds)
	tkhd := make([]byte, 84)
	binary.BigEndian.PutUint32(tkhd[76:80], uint32(width<<16))
	binary.BigEndian.PutUint32(tkhd[80:84], uint32(height<<16))
	moov := mediatest.Box("moov", append(mediatest.Box("mvhd", mvhd), mediatest.Box("trak", mediatest.Box("tkhd", tkhd))...))
	return append(mediatest.Box("ftyp", append([]byte("isom"), make([]byte, 12)...)), moov...)
}
