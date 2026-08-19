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
		known      bool
		animated   bool
	}{
		{name: "jpeg", data: mediatest.JPEG(3, 2, nil), declared: "text/plain", wantFormat: media.FormatJPEG, wantKind: media.KindImage, wantType: "image/jpeg", width: 3, height: 2, frames: 1},
		{name: "png", data: mediatest.PNG(4, 3, nil), declared: "image/png", wantFormat: media.FormatPNG, wantKind: media.KindImage, wantType: "image/png", width: 4, height: 3, frames: 1},
		{name: "webp vp8x", data: mediatest.WebP(5, 4), declared: "image/webp", wantFormat: media.FormatWebP, wantKind: media.KindImage, wantType: "image/webp", width: 5, height: 4, frames: 1},
		{name: "webp vp8l", data: webPLossless(7, 6), declared: "", wantFormat: media.FormatWebP, wantKind: media.KindImage, wantType: "image/webp", width: 7, height: 6, frames: 1},
		{name: "webp vp8", data: webPLossy(9, 8), declared: "", wantFormat: media.FormatWebP, wantKind: media.KindImage, wantType: "image/webp", width: 9, height: 8, frames: 1},
		{name: "still gif", data: mediatest.GIF(2, 2, 1), declared: "image/gif", wantFormat: media.FormatGIF, wantKind: media.KindImage, wantType: "image/gif", width: 2, height: 2, frames: 1},
		{name: "animated gif", data: mediatest.GIF(2, 2, 3), declared: "image/gif", wantFormat: media.FormatGIF, wantKind: media.KindImage, wantType: "image/gif", width: 2, height: 2, frames: 3, animated: true},
		{name: "webp animated", data: webPAnimated(6, 5, 3), declared: "image/webp", wantFormat: media.FormatWebP, wantKind: media.KindImage, wantType: "image/webp", width: 6, height: 5, frames: 3, animated: true},
		{name: "apng", data: apng(mediatest.PNG(4, 3, nil), 2), declared: "image/png", wantFormat: media.FormatPNG, wantKind: media.KindImage, wantType: "image/png", width: 4, height: 3, frames: 2, animated: true},
		{name: "mp4 v0", data: mediatest.MP4(640, 360, 1250), declared: "video/quicktime", wantFormat: media.FormatMP4, wantKind: media.KindVideo, wantType: "video/mp4", width: 640, height: 360, duration: 1250, known: true},
		{name: "mp4 v1", data: mp4Version1(320, 240, 90_000, 30), declared: "video/mp4", wantFormat: media.FormatMP4, wantKind: media.KindVideo, wantType: "video/mp4", width: 320, height: 240, duration: 30000, known: true},
		{name: "mp4 unknown duration", data: mediatest.MP4(640, 360, 0), declared: "video/mp4", wantFormat: media.FormatMP4, wantKind: media.KindVideo, wantType: "video/mp4", width: 640, height: 360},
		{name: "mp4 audio track first", data: mp4AudioTrackFirst(640, 360, 700), declared: "video/mp4", wantFormat: media.FormatMP4, wantKind: media.KindVideo, wantType: "video/mp4", width: 640, height: 360, duration: 700, known: true},
		{name: "mp4 largest picture track wins", data: mp4TwoPictureTracks(16, 16, 4096, 2160), declared: "video/mp4", wantFormat: media.FormatMP4, wantKind: media.KindVideo, wantType: "video/mp4", width: 4096, height: 2160},
		{name: "mp4 coded dimensions exceed presentation", data: mp4WithCodedDimensions(320, 180, 4096, 2160), declared: "video/mp4", wantFormat: media.FormatMP4, wantKind: media.KindVideo, wantType: "video/mp4", width: 4096, height: 2160, duration: 500, known: true},
		{name: "mp4 moov to end of file", data: mp4WithSizeZeroMoov(320, 240, 500), declared: "video/mp4", wantFormat: media.FormatMP4, wantKind: media.KindVideo, wantType: "video/mp4", width: 320, height: 240, duration: 500, known: true},
		{name: "mp4 largesize moov", data: mp4WithLargesizeMoov(320, 240, 500), declared: "video/mp4", wantFormat: media.FormatMP4, wantKind: media.KindVideo, wantType: "video/mp4", width: 320, height: 240, duration: 500, known: true},
		{name: "mp4 longest track duration wins", data: mp4WithTrackDuration(320, 240, 500, 9000, false), declared: "video/mp4", wantFormat: media.FormatMP4, wantKind: media.KindVideo, wantType: "video/mp4", width: 320, height: 240, duration: 9000, known: true},
		{name: "mp4 unknown track duration", data: mp4WithTrackDuration(320, 240, 500, 0, true), declared: "video/mp4", wantFormat: media.FormatMP4, wantKind: media.KindVideo, wantType: "video/mp4", width: 320, height: 240},
		{name: "mp4 v1 track duration wins", data: mp4WithV1Track(320, 240, 500, 7000), declared: "video/mp4", wantFormat: media.FormatMP4, wantKind: media.KindVideo, wantType: "video/mp4", width: 320, height: 240, duration: 7000, known: true},
		{name: "mp4 fragmented is unknown duration", data: mp4Fragmented(320, 240, 500), declared: "video/mp4", wantFormat: media.FormatMP4, wantKind: media.KindVideo, wantType: "video/mp4", width: 320, height: 240},
		{name: "mp4 duration rounds up", data: mp4WithRawDuration(320, 240, 3, 1), declared: "video/mp4", wantFormat: media.FormatMP4, wantKind: media.KindVideo, wantType: "video/mp4", width: 320, height: 240, duration: 334, known: true},
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
			assert.Equal(t, tt.known, got.DurationKnown)
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
		{name: "animated webp without frames", data: webPAnimated(6, 5, 0), want: media.ErrMalformedMedia},
		{name: "webp animation chunk without flag", data: webPAnimationWithoutFlag(6, 5), want: media.ErrMalformedMedia},
		{name: "webp frame outside canvas", data: webPAnimatedOutsideCanvas(6, 5), want: media.ErrMalformedMedia},
		{name: "webp coded image outside canvas", data: webPCodedOutsideCanvas(6, 5), want: media.ErrMalformedMedia},
		{name: "apng zero frames", data: apng(mediatest.PNG(4, 3, nil), 0), want: media.ErrMalformedMedia},
		{name: "apng declared frame count mismatch", data: apngWithFrameCounts(mediatest.PNG(4, 3, nil), 1, 2), want: media.ErrMalformedMedia},
		{name: "apng frame outside canvas", data: apngOutsideCanvas(mediatest.PNG(4, 3, nil)), want: media.ErrMalformedMedia},
		{name: "mp4 audio only", data: mp4AudioTrackFirst(0, 0, 700), want: media.ErrMalformedMedia},
		{name: "mp4 mvhd outside moov", data: mp4MisplacedMVHD(), want: media.ErrMalformedMedia},
		{name: "mp4 unsupported tkhd version", data: mp4WithTKHDVersion(7), want: media.ErrMalformedMedia},
		{name: "mp4 unsupported mvhd version", data: mp4WithMVHDVersion(2), want: media.ErrMalformedMedia},
		{name: "mp4 missing coded dimensions", data: mp4WithoutCodedDimensions(320, 240, 500), want: media.ErrMalformedMedia},
		{name: "mp4 partial coded dimensions", data: mp4WithCodedDimensions(320, 240, 320, 0), want: media.ErrMalformedMedia},
		{name: "mp4 unknown visual sample entry", data: mp4WithUnknownVisualSample(320, 240), want: media.ErrMalformedMedia},
		{name: "mp4 zero-presentation unknown video track", data: mp4WithUnknownVideoTrack(), want: media.ErrMalformedMedia},
		{name: "mp4 duplicate mvhd", data: mp4DuplicateMVHD(), want: media.ErrMalformedMedia},
		{name: "mp4 duplicate moov", data: append(mediatest.MP4(320, 240, 1), mediatest.Box("moov", nil)...), want: media.ErrMalformedMedia},
		{name: "mp4 largesize truncated", data: append(mediatest.Box("ftyp", append([]byte("isom"), make([]byte, 12)...)), 0, 0, 0, 1, 'm', 'o', 'o', 'v', 0, 0, 0, 0), want: media.ErrMalformedMedia},
		{name: "gif frame outside screen", data: gifWithFrameSize(mediatest.GIF(2, 2, 1), 9, 2), want: media.ErrMalformedMedia},
		{name: "gif zero frame", data: gifWithFrameSize(mediatest.GIF(2, 2, 1), 0, 2), want: media.ErrMalformedMedia},
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
	return webPContainer(webPChunk("VP8L", webPLosslessPayload(width, height)))
}

func webPLossy(width, height int) []byte {
	payload := make([]byte, 10)
	copy(payload[3:6], []byte{0x9d, 0x01, 0x2a})
	binary.LittleEndian.PutUint16(payload[6:8], uint16(width))
	binary.LittleEndian.PutUint16(payload[8:10], uint16(height))
	return webPContainer(webPChunk("VP8 ", payload))
}

func mp4Version1(width, height int, timescale uint32, seconds uint64) []byte {
	mvhd := make([]byte, 32)
	mvhd[0] = 1
	binary.BigEndian.PutUint32(mvhd[20:24], timescale)
	binary.BigEndian.PutUint64(mvhd[24:32], uint64(timescale)*seconds)
	tkhd := make([]byte, 84)
	binary.BigEndian.PutUint32(tkhd[76:80], uint32(width<<16))
	binary.BigEndian.PutUint32(tkhd[80:84], uint32(height<<16))
	trak := mediatest.Box("trak", append(mediatest.Box("tkhd", tkhd), mp4SampleTable(width, height)...))
	moov := mediatest.Box("moov", append(mediatest.Box("mvhd", mvhd), trak...))
	return append(mediatest.Box("ftyp", append([]byte("isom"), make([]byte, 12)...)), moov...)
}

// webPAnimated returns a VP8X WebP with the animation flag and the given
// number of ANMF chunks; zero frames yields a flagged container with none.
func webPAnimated(width, height, frames int) []byte {
	chunks := [][]byte{webPVP8X(width, height, 0x02)}
	for range frames {
		chunks = append(chunks, webPANMF(width, height, width, height))
	}
	return webPContainer(chunks...)
}

func webPAnimationWithoutFlag(width, height int) []byte {
	return webPContainer(webPVP8X(width, height, 0), webPANMF(width, height, width, height))
}

func webPAnimatedOutsideCanvas(width, height int) []byte {
	return webPContainer(webPVP8X(width, height, 0x02), webPANMF(width+1, height, width+1, height))
}

func webPCodedOutsideCanvas(width, height int) []byte {
	return webPContainer(webPVP8X(width, height, 0), webPChunk("VP8L", webPLosslessPayload(width+1, height)))
}

func webPContainer(chunks ...[]byte) []byte {
	size := 12
	for _, chunk := range chunks {
		size += len(chunk)
	}
	data := make([]byte, 0, size)
	data = append(data, make([]byte, 12)...)
	copy(data[0:4], "RIFF")
	copy(data[8:12], "WEBP")
	for _, chunk := range chunks {
		data = append(data, chunk...)
	}
	binary.LittleEndian.PutUint32(data[4:8], uint32(len(data)-8))
	return data
}

func webPChunk(kind string, payload []byte) []byte {
	chunk := make([]byte, 8+len(payload)+len(payload)%2)
	copy(chunk[:4], kind)
	binary.LittleEndian.PutUint32(chunk[4:8], uint32(len(payload)))
	copy(chunk[8:], payload)
	return chunk
}

func webPVP8X(width, height int, flags byte) []byte {
	payload := make([]byte, 10)
	payload[0] = flags
	w, h := width-1, height-1
	payload[4], payload[5], payload[6] = byte(w), byte(w>>8), byte(w>>16)
	payload[7], payload[8], payload[9] = byte(h), byte(h>>8), byte(h>>16)
	return webPChunk("VP8X", payload)
}

func webPANMF(frameWidth, frameHeight, codedWidth, codedHeight int) []byte {
	image := webPChunk("VP8L", webPLosslessPayload(codedWidth, codedHeight))
	payload := make([]byte, 16+len(image))
	w, h := frameWidth-1, frameHeight-1
	payload[6], payload[7], payload[8] = byte(w), byte(w>>8), byte(w>>16)
	payload[9], payload[10], payload[11] = byte(h), byte(h>>8), byte(h>>16)
	copy(payload[16:], image)
	return webPChunk("ANMF", payload)
}

func webPLosslessPayload(width, height int) []byte {
	payload := make([]byte, 5)
	payload[0] = 0x2f
	binary.LittleEndian.PutUint32(payload[1:5], uint32(width-1)|uint32(height-1)<<14)
	return payload
}

// apng inserts matching animation-control and frame-control chunks after IHDR.
func apng(png []byte, frames int) []byte {
	return apngWithFrameCounts(png, frames, frames)
}

func apngWithFrameCounts(png []byte, declared, actual int) []byte {
	ihdrEnd := 8 + 12 + 13
	actl := make([]byte, 12+8)
	binary.BigEndian.PutUint32(actl[0:4], 8)
	copy(actl[4:8], "acTL")
	binary.BigEndian.PutUint32(actl[8:12], uint32(declared))
	out := append([]byte(nil), png[:ihdrEnd]...)
	out = append(out, actl...)
	for range actual {
		fctl := make([]byte, 12+26)
		binary.BigEndian.PutUint32(fctl[0:4], 26)
		copy(fctl[4:8], "fcTL")
		copy(fctl[12:20], png[16:24])
		out = append(out, fctl...)
	}
	return append(out, png[ihdrEnd:]...)
}

func apngOutsideCanvas(png []byte) []byte {
	out := apng(png, 1)
	index := bytes.Index(out, []byte("fcTL"))
	binary.BigEndian.PutUint32(out[index+8:index+12], binary.BigEndian.Uint32(png[16:20])+1)
	return out
}

// mp4AudioTrackFirst places a zero-dimension track before the picture track.
func mp4AudioTrackFirst(width, height int, durationMS int64) []byte {
	mvhd := make([]byte, 20)
	binary.BigEndian.PutUint32(mvhd[12:16], 1000)
	binary.BigEndian.PutUint32(mvhd[16:20], uint32(durationMS))
	audio := mediatest.Box("trak", append(mediatest.Box("tkhd", make([]byte, 84)), mediatest.Box("mdia", mp4Handler("soun"))...))
	tkhd := make([]byte, 84)
	binary.BigEndian.PutUint32(tkhd[76:80], uint32(width<<16))
	binary.BigEndian.PutUint32(tkhd[80:84], uint32(height<<16))
	video := mediatest.Box("trak", append(mediatest.Box("tkhd", tkhd), mp4SampleTable(width, height)...))
	moov := mediatest.Box("moov", append(append(mediatest.Box("mvhd", mvhd), audio...), video...))
	return append(mediatest.Box("ftyp", append([]byte("isom"), make([]byte, 12)...)), moov...)
}

// mp4TwoPictureTracks places a small picture track before a large one.
func mp4TwoPictureTracks(w1, h1, w2, h2 int) []byte {
	track := func(width, height int) []byte {
		tkhd := make([]byte, 84)
		binary.BigEndian.PutUint32(tkhd[76:80], uint32(width<<16))
		binary.BigEndian.PutUint32(tkhd[80:84], uint32(height<<16))
		return mediatest.Box("trak", append(mediatest.Box("tkhd", tkhd), mp4SampleTable(width, height)...))
	}
	moov := mediatest.Box("moov", append(track(w1, h1), track(w2, h2)...))
	return append(mediatest.Box("ftyp", append([]byte("isom"), make([]byte, 12)...)), moov...)
}

func mp4WithCodedDimensions(presentationWidth, presentationHeight, codedWidth, codedHeight int) []byte {
	ftyp := mediatest.Box("ftyp", append([]byte("isom"), make([]byte, 12)...))
	mvhdPayload := make([]byte, 20)
	binary.BigEndian.PutUint32(mvhdPayload[12:16], 1000)
	binary.BigEndian.PutUint32(mvhdPayload[16:20], 500)
	mvhd := mediatest.Box("mvhd", mvhdPayload)
	tkhd := make([]byte, 84)
	binary.BigEndian.PutUint32(tkhd[76:80], uint32(presentationWidth<<16))
	binary.BigEndian.PutUint32(tkhd[80:84], uint32(presentationHeight<<16))
	trak := mediatest.Box("trak", mediatest.Box("tkhd", tkhd))
	sample := make([]byte, 28)
	binary.BigEndian.PutUint16(sample[24:26], uint16(codedWidth))
	binary.BigEndian.PutUint16(sample[26:28], uint16(codedHeight))
	stsd := make([]byte, 0, 8+8+len(sample))
	stsd = append(stsd, make([]byte, 8)...)
	binary.BigEndian.PutUint32(stsd[4:8], 1)
	stsd = append(stsd, mediatest.Box("avc1", sample)...)
	stbl := mediatest.Box("stbl", mediatest.Box("stsd", stsd))
	minf := mediatest.Box("minf", stbl)
	mdia := mediatest.Box("mdia", append(mp4Handler("vide"), minf...))
	trak = append(trak, mdia...)
	binary.BigEndian.PutUint32(trak[:4], uint32(len(trak)))
	return append(ftyp, mediatest.Box("moov", append(mvhd, trak...))...)
}

func mp4WithUnknownVisualSample(width, height int) []byte {
	data := mp4WithCodedDimensions(width, height, width, height)
	index := bytes.Index(data, []byte("stsd"))
	entryCount := index + 8
	binary.BigEndian.PutUint32(data[entryCount:entryCount+4], 2)
	unknown := mediatest.Box("zzzz", make([]byte, 28))
	data = append(data, unknown...)
	moov := bytes.Index(data, []byte("moov")) - 4
	trak := bytes.Index(data, []byte("trak")) - 4
	mdia := bytes.Index(data, []byte("mdia")) - 4
	minf := bytes.Index(data, []byte("minf")) - 4
	stbl := bytes.Index(data, []byte("stbl")) - 4
	stsd := index - 4
	for _, offset := range []int{moov, trak, mdia, minf, stbl, stsd} {
		binary.BigEndian.PutUint32(data[offset:offset+4], binary.BigEndian.Uint32(data[offset:offset+4])+uint32(len(unknown)))
	}
	return data
}

func mp4WithUnknownVideoTrack() []byte {
	ftyp, mvhd, picture := mp4Parts(16, 16, 500)
	tkhd := make([]byte, 84)
	stsd := make([]byte, 0, 8+8+28)
	stsd = append(stsd, make([]byte, 8)...)
	binary.BigEndian.PutUint32(stsd[4:8], 1)
	stsd = append(stsd, mediatest.Box("zzzz", make([]byte, 28))...)
	mdia := mediatest.Box("mdia", append(mp4Handler("vide"), mediatest.Box("minf", mediatest.Box("stbl", mediatest.Box("stsd", stsd)))...))
	unknown := mediatest.Box("trak", append(mediatest.Box("tkhd", tkhd), mdia...))
	moov := mediatest.Box("moov", append(append(mvhd, unknown...), picture...))
	return append(ftyp, moov...)
}

func mp4Handler(kind string) []byte {
	payload := make([]byte, 12)
	copy(payload[8:12], kind)
	return mediatest.Box("hdlr", payload)
}

func mp4WithoutCodedDimensions(width, height int, durationMS int64) []byte {
	ftyp := mediatest.Box("ftyp", append([]byte("isom"), make([]byte, 12)...))
	mvhd := make([]byte, 20)
	binary.BigEndian.PutUint32(mvhd[12:16], 1000)
	binary.BigEndian.PutUint32(mvhd[16:20], uint32(durationMS))
	tkhd := make([]byte, 84)
	binary.BigEndian.PutUint32(tkhd[76:80], uint32(width<<16))
	binary.BigEndian.PutUint32(tkhd[80:84], uint32(height<<16))
	trak := mediatest.Box("trak", append(mediatest.Box("tkhd", tkhd), mediatest.Box("mdia", mp4Handler("vide"))...))
	return append(ftyp, mediatest.Box("moov", append(mediatest.Box("mvhd", mvhd), trak...))...)
}

func mp4SampleTable(width, height int) []byte {
	sample := make([]byte, 28)
	binary.BigEndian.PutUint16(sample[24:26], uint16(width))
	binary.BigEndian.PutUint16(sample[26:28], uint16(height))
	stsd := make([]byte, 0, 8+8+len(sample))
	stsd = append(stsd, make([]byte, 8)...)
	binary.BigEndian.PutUint32(stsd[4:8], 1)
	stsd = append(stsd, mediatest.Box("avc1", sample)...)
	minf := mediatest.Box("minf", mediatest.Box("stbl", mediatest.Box("stsd", stsd)))
	return mediatest.Box("mdia", append(mp4Handler("vide"), minf...))
}

// gifWithFrameSize rewrites the first image descriptor's frame width and
// height without touching the logical screen size.
func gifWithFrameSize(gif []byte, width, height int) []byte {
	out := append([]byte(nil), gif...)
	index := bytes.IndexByte(out[13:], 0x2c) + 13
	binary.LittleEndian.PutUint16(out[index+5:index+7], uint16(width))
	binary.LittleEndian.PutUint16(out[index+7:index+9], uint16(height))
	return out
}

func mp4Parts(width, height int, durationMS int64) (ftyp, mvhd, trak []byte) {
	ftyp = mediatest.Box("ftyp", append([]byte("isom"), make([]byte, 12)...))
	mvhdPayload := make([]byte, 20)
	binary.BigEndian.PutUint32(mvhdPayload[12:16], 1000)
	binary.BigEndian.PutUint32(mvhdPayload[16:20], uint32(durationMS))
	mvhd = mediatest.Box("mvhd", mvhdPayload)
	tkhd := make([]byte, 84)
	binary.BigEndian.PutUint32(tkhd[76:80], uint32(width<<16))
	binary.BigEndian.PutUint32(tkhd[80:84], uint32(height<<16))
	trak = mediatest.Box("trak", append(mediatest.Box("tkhd", tkhd), mp4SampleTable(width, height)...))
	return ftyp, mvhd, trak
}

func mp4WithSizeZeroMoov(width, height int, durationMS int64) []byte {
	ftyp, mvhd, trak := mp4Parts(width, height, durationMS)
	moov := mediatest.Box("moov", append(mvhd, trak...))
	binary.BigEndian.PutUint32(moov[:4], 0)
	return append(ftyp, moov...)
}

func mp4WithLargesizeMoov(width, height int, durationMS int64) []byte {
	ftyp, mvhd, trak := mp4Parts(width, height, durationMS)
	payload := append(append([]byte(nil), mvhd...), trak...)
	moov := make([]byte, 16+len(payload))
	binary.BigEndian.PutUint32(moov[:4], 1)
	copy(moov[4:8], "moov")
	binary.BigEndian.PutUint64(moov[8:16], uint64(len(moov)))
	copy(moov[16:], payload)
	return append(ftyp, moov...)
}

func mp4WithTrackDuration(width, height int, movieMS, trackMS int64, unknown bool) []byte {
	ftyp, mvhd, trak := mp4Parts(width, height, movieMS)
	tkhd := trak[16:] // strip trak and tkhd headers
	if unknown {
		binary.BigEndian.PutUint32(tkhd[20:24], 0xFFFFFFFF)
	} else {
		binary.BigEndian.PutUint32(tkhd[20:24], uint32(trackMS))
	}
	return append(ftyp, mediatest.Box("moov", append(mvhd, trak...))...)
}

func mp4MisplacedMVHD() []byte {
	ftyp, mvhd, trak := mp4Parts(320, 240, 500)
	return append(append(ftyp, mvhd...), mediatest.Box("moov", trak)...)
}

func mp4DuplicateMVHD() []byte {
	ftyp, mvhd, trak := mp4Parts(320, 240, 500)
	return append(ftyp, mediatest.Box("moov", append(append(mvhd, mvhd...), trak...))...)
}

func mp4WithV1Track(width, height int, movieMS, trackMS int64) []byte {
	ftyp, mvhd, _ := mp4Parts(width, height, movieMS)
	tkhd := make([]byte, 96)
	tkhd[0] = 1
	binary.BigEndian.PutUint64(tkhd[28:36], uint64(trackMS))
	binary.BigEndian.PutUint32(tkhd[88:92], uint32(width<<16))
	binary.BigEndian.PutUint32(tkhd[92:96], uint32(height<<16))
	trak := mediatest.Box("trak", append(mediatest.Box("tkhd", tkhd), mp4SampleTable(width, height)...))
	return append(ftyp, mediatest.Box("moov", append(mvhd, trak...))...)
}

func mp4Fragmented(width, height int, movieMS int64) []byte {
	ftyp, mvhd, trak := mp4Parts(width, height, movieMS)
	mvex := mediatest.Box("mvex", mediatest.Box("trex", make([]byte, 24)))
	return append(ftyp, mediatest.Box("moov", append(append(mvhd, trak...), mvex...))...)
}

func mp4WithRawDuration(width, height int, timescale, duration uint32) []byte {
	ftyp, _, trak := mp4Parts(width, height, 0)
	mvhd := make([]byte, 20)
	binary.BigEndian.PutUint32(mvhd[12:16], timescale)
	binary.BigEndian.PutUint32(mvhd[16:20], duration)
	return append(ftyp, mediatest.Box("moov", append(mediatest.Box("mvhd", mvhd), trak...))...)
}

func mp4WithTKHDVersion(version byte) []byte {
	ftyp, mvhd, trak := mp4Parts(320, 240, 500)
	trak[16] = version
	return append(ftyp, mediatest.Box("moov", append(mvhd, trak...))...)
}

func mp4WithMVHDVersion(version byte) []byte {
	ftyp, mvhd, trak := mp4Parts(320, 240, 500)
	mvhd[8] = version
	return append(ftyp, mediatest.Box("moov", append(mvhd, trak...))...)
}
