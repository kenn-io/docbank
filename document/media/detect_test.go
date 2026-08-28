package media_test

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"image/color"
	"io"
	"math"
	"slices"
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
		{name: "apng frame control before animation control", data: apngFCTLBeforeACTL(mediatest.PNG(4, 3, nil)), declared: "image/png", wantFormat: media.FormatPNG, wantKind: media.KindImage, wantType: "image/png", width: 4, height: 3, frames: 2, animated: true},
		{name: "apng split by empty idat", data: apngWithEmptyIDATChunk(mediatest.PNG(4, 3, nil)), declared: "image/png", wantFormat: media.FormatPNG, wantKind: media.KindImage, wantType: "image/png", width: 4, height: 3, frames: 2, animated: true},
		{name: "apng split by empty fdat", data: apngWithEmptyFDATChunk(mediatest.PNG(4, 3, nil)), declared: "image/png", wantFormat: media.FormatPNG, wantKind: media.KindImage, wantType: "image/png", width: 4, height: 3, frames: 2, animated: true},
		{name: "mp4 v0", data: mediatest.MP4(640, 360, 1250), declared: "video/quicktime", wantFormat: media.FormatMP4, wantKind: media.KindVideo, wantType: "video/mp4", width: 640, height: 368, duration: 1250, known: true},
		{name: "mp4 v1", data: mp4Version1(320, 240, 90_000, 30), declared: "video/mp4", wantFormat: media.FormatMP4, wantKind: media.KindVideo, wantType: "video/mp4", width: 320, height: 240, duration: 30000, known: true},
		{name: "mp4 unknown duration", data: mediatest.MP4(640, 360, 0), declared: "video/mp4", wantFormat: media.FormatMP4, wantKind: media.KindVideo, wantType: "video/mp4", width: 640, height: 368},
		{name: "mp4 audio track first", data: mp4AudioTrackFirst(640, 360, 700), declared: "video/mp4", wantFormat: media.FormatMP4, wantKind: media.KindVideo, wantType: "video/mp4", width: 640, height: 368, duration: 700, known: true},
		{name: "mp4 largest picture track wins", data: mp4TwoPictureTracks(16, 16, 4096, 2160), declared: "video/mp4", wantFormat: media.FormatMP4, wantKind: media.KindVideo, wantType: "video/mp4", width: 4096, height: 2160},
		{name: "mp4 coded dimensions exceed presentation", data: mp4WithCodedDimensions(320, 180, 4096, 2160), declared: "video/mp4", wantFormat: media.FormatMP4, wantKind: media.KindVideo, wantType: "video/mp4", width: 4096, height: 2160, duration: 500, known: true},
		{name: "mp4 moov to end of file", data: mp4WithSizeZeroMoov(320, 240, 500), declared: "video/mp4", wantFormat: media.FormatMP4, wantKind: media.KindVideo, wantType: "video/mp4", width: 320, height: 240, duration: 500, known: true},
		{name: "mp4 largesize moov", data: mp4WithLargesizeMoov(320, 240, 500), declared: "video/mp4", wantFormat: media.FormatMP4, wantKind: media.KindVideo, wantType: "video/mp4", width: 320, height: 240, duration: 500, known: true},
		{name: "mp4 longest track duration wins", data: mp4WithTrackDuration(320, 240, 500, 9000, false), declared: "video/mp4", wantFormat: media.FormatMP4, wantKind: media.KindVideo, wantType: "video/mp4", width: 320, height: 240, duration: 9000, known: true},
		{name: "mp4 unknown track duration", data: mp4WithTrackDuration(320, 240, 500, 0, true), declared: "video/mp4", wantFormat: media.FormatMP4, wantKind: media.KindVideo, wantType: "video/mp4", width: 320, height: 240},
		{name: "mp4 v1 track duration wins", data: mp4WithV1Track(320, 240, 500, 7000), declared: "video/mp4", wantFormat: media.FormatMP4, wantKind: media.KindVideo, wantType: "video/mp4", width: 320, height: 240, duration: 7000, known: true},
		{name: "mp4 fragmented is unknown duration", data: mp4Fragmented(320, 240, 500), declared: "video/mp4", wantFormat: media.FormatMP4, wantKind: media.KindVideo, wantType: "video/mp4", width: 320, height: 240},
		{name: "mp4 moof fragment is unknown duration", data: mp4WithMoof(320, 240, 500), declared: "video/mp4", wantFormat: media.FormatMP4, wantKind: media.KindVideo, wantType: "video/mp4", width: 320, height: 240},
		{name: "mp4 v0 sentinel movie duration is unknown", data: mp4WithRawDuration(320, 240, 1000, math.MaxUint32), declared: "video/mp4", wantFormat: media.FormatMP4, wantKind: media.KindVideo, wantType: "video/mp4", width: 320, height: 240},
		{name: "mp4 v1 sentinel movie duration is unknown", data: mp4V1SentinelDuration(320, 240), declared: "video/mp4", wantFormat: media.FormatMP4, wantKind: media.KindVideo, wantType: "video/mp4", width: 320, height: 240},
		{name: "mp4 duration rounds up", data: mp4WithRawDuration(320, 240, 3, 1), declared: "video/mp4", wantFormat: media.FormatMP4, wantKind: media.KindVideo, wantType: "video/mp4", width: 320, height: 240, duration: 334, known: true},
		{name: "mp4 edit list duration wins", data: mp4WithEditListDuration(320, 240, 500, 1500), declared: "video/mp4", wantFormat: media.FormatMP4, wantKind: media.KindVideo, wantType: "video/mp4", width: 320, height: 240, duration: 1500, known: true},
		{name: "mp4 version one edit list duration wins", data: mp4WithEditListPayload(320, 240, 500, mp4EditList(1, 1, 500, 1000)), declared: "video/mp4", wantFormat: media.FormatMP4, wantKind: media.KindVideo, wantType: "video/mp4", width: 320, height: 240, duration: 1500, known: true},
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

// TestDetectReportsVerifiedMP4CodecAndQuickTimeContainer catches a detector
// that treats every ISO base-media file as generic MP4 or omits the verified
// H.264 sample-entry identity needed for Gemini video eligibility.
func TestDetectReportsVerifiedMP4CodecAndQuickTimeContainer(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name, codec, container string
		data                   []byte
	}{
		{name: "QuickTime H.264", codec: "h264", container: "quicktime", data: mediatest.H264MOV()},
		{name: "MP4 H.265", codec: "h265", container: "mp4", data: mediatest.H265MP4()},
		{name: "MP4 VP9", codec: "vp9", container: "mp4", data: mediatest.VP9MP4()},
		{name: "MP4 AV1", codec: "av1", container: "mp4", data: mediatest.AV1MP4()},
	} {
		t.Run(tt.name, func(t *testing.T) {
			metadata, err := media.DetectBytes(tt.data, "video/mp4")
			require.NoError(t, err)
			assert.Equal(t, tt.container, metadata.Container)
			assert.Equal(t, tt.codec, metadata.Codec)
			assert.Equal(t, int64(1_000), metadata.DurationMS)
			assert.True(t, metadata.DurationKnown)
		})
	}

	// Two authoritative visual sample entries must not collapse into an
	// arbitrary codec choice.
	conflicting := mp4TwoPictureTracks(16, 16, 16, 16)
	secondEntry := bytes.LastIndex(conflicting, []byte("avc1"))
	secondConfig := bytes.LastIndex(conflicting, []byte("avcC"))
	require.NotEqual(t, -1, secondEntry)
	require.NotEqual(t, -1, secondConfig)
	copy(conflicting[secondEntry:secondEntry+4], "av01")
	copy(conflicting[secondConfig:secondConfig+4], "av1C")
	conflicting[secondConfig+4] = 0x81
	_, err := media.DetectBytes(conflicting, "video/mp4")
	require.ErrorIs(t, err, media.ErrMalformedMedia)
}

func TestDetectRejectsRenamedAVCConfigurationAsAnotherCodec(t *testing.T) {
	t.Parallel()
	for _, testCase := range []struct {
		name, sampleEntry, configBox string
	}{
		{name: "VP9", sampleEntry: "vp09", configBox: "vpcC"},
		{name: "AV1", sampleEntry: "av01", configBox: "av1C"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			data := decodableAVCMP4(t)
			entry := bytes.Index(data, []byte("avc1"))
			config := bytes.Index(data, []byte("avcC"))
			require.NotEqual(t, -1, entry)
			require.NotEqual(t, -1, config)
			copy(data[entry:entry+4], testCase.sampleEntry)
			copy(data[config:config+4], testCase.configBox)
			if testCase.configBox == "av1C" {
				copy(data[config+4:config+8], []byte{0x81, 0, 0, 0})
			}

			_, err := media.DetectBytes(data, "video/mp4")
			require.ErrorIs(t, err, media.ErrMalformedMedia)
		})
	}
}

func quickTimeH264MP4() []byte {
	return mediatest.H264MOV()
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
		{name: "apng default frame smaller than canvas", data: apngIDATGeometryMismatch(mediatest.PNG(4, 3, nil)), want: media.ErrMalformedMedia},
		{name: "apng bad chunk crc", data: apngBadCRC(mediatest.PNG(4, 3, nil)), want: media.ErrMalformedMedia},
		{name: "apng out of order sequence", data: apngOutOfOrderSequence(mediatest.PNG(4, 3, nil)), want: media.ErrMalformedMedia},
		{name: "apng frame without data", data: apngFrameWithoutData(mediatest.PNG(4, 3, nil)), want: media.ErrMalformedMedia},
		{name: "mp4 audio only", data: mp4AudioTrackFirst(0, 0, 700), want: media.ErrMalformedMedia},
		{name: "mp4 mvhd outside moov", data: mp4MisplacedMVHD(), want: media.ErrMalformedMedia},
		{name: "mp4 unsupported tkhd version", data: mp4WithTKHDVersion(7), want: media.ErrMalformedMedia},
		{name: "mp4 version zero tkhd with trailing payload", data: mp4WithTrailingTKHD(0), want: media.ErrMalformedMedia},
		{name: "mp4 version one tkhd with trailing payload", data: mp4WithTrailingTKHD(1), want: media.ErrMalformedMedia},
		{name: "mp4 unsupported mvhd version", data: mp4WithMVHDVersion(2), want: media.ErrMalformedMedia},
		{name: "mp4 missing coded dimensions", data: mp4WithoutCodedDimensions(320, 240, 500), want: media.ErrMalformedMedia},
		{name: "mp4 partial coded dimensions", data: mp4WithCodedDimensions(320, 240, 320, 0), want: media.ErrMalformedMedia},
		{name: "mp4 missing codec configuration", data: mp4WithRenamedBox("avcC"), want: media.ErrMalformedMedia},
		{name: "mp4 missing media header", data: mp4WithRenamedBox("mdhd"), want: media.ErrMalformedMedia},
		{name: "mp4 missing sample timing", data: mp4WithRenamedBox("stts"), want: media.ErrMalformedMedia},
		{name: "mp4 missing sample count", data: mp4WithRenamedBox("stsz"), want: media.ErrMalformedMedia},
		{name: "mp4 sample count mismatch", data: mp4WithSampleCountMismatch(), want: media.ErrMalformedMedia},
		{name: "mp4 zero sample delta", data: mp4WithZeroSampleDelta(), want: media.ErrMalformedMedia},
		{name: "mp4 composition sample count mismatch", data: mp4WithCTTS(0, 2, 1000), want: media.ErrMalformedMedia},
		{name: "mp4 unsupported composition timing version", data: mp4WithCTTS(2, 1, 1000), want: media.ErrMalformedMedia},
		{name: "mp4 misplaced composition timing", data: mp4MisplacedCTTS(), want: media.ErrMalformedMedia},
		{name: "mp4 unsupported edit list version", data: mp4WithEditListVersion(2), want: media.ErrMalformedMedia},
		{name: "mp4 unsupported edit list rate", data: mp4WithEditListRate(0), want: media.ErrMalformedMedia},
		{name: "mp4 invalid negative edit media time", data: mp4WithInvalidEditMediaTime(), want: media.ErrMalformedMedia},
		{name: "mp4 misplaced edit list", data: mp4MisplacedELST(), want: media.ErrMalformedMedia},
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

func TestDetectBoundsDecodableMP4FromCodecAndSampleTiming(t *testing.T) {
	t.Run("fractional presentation dimensions round up", func(t *testing.T) {
		data := mediatest.MP4(16, 16, 500)
		tkhd := mp4TestBoxPayload(t, data, "tkhd")
		binary.BigEndian.PutUint32(tkhd[76:80], 16<<16|1)
		binary.BigEndian.PutUint32(tkhd[80:84], 16<<16|1)

		metadata, err := media.DetectBytes(data, "")
		require.NoError(t, err)
		assert.Equal(t, int64(17), metadata.Width)
		assert.Equal(t, int64(17), metadata.Height)
		_, reason := media.InspectBytes(data, "", media.Policy{MaxBytes: media.MaxBytes, MaxPixels: 256, AllowVideo: true})
		assert.Equal(t, media.ReasonTooManyPixels, reason)
	})

	t.Run("missing movie timescale leaves duration unknown", func(t *testing.T) {
		data := mp4WithoutMovieHeader(9000)

		metadata, err := media.DetectBytes(data, "")
		require.NoError(t, err)
		assert.False(t, metadata.DurationKnown)
		_, reason := media.InspectBytes(data, "", media.Policy{
			MaxBytes: media.MaxBytes, MaxPixels: media.DefaultMaxPixels, MaxDurationMS: 1000, AllowVideo: true,
		})
		assert.Equal(t, media.ReasonTooLong, reason)
	})

	t.Run("edit list presentation duration enforces the policy cap", func(t *testing.T) {
		data := mp4WithEditListDuration(16, 16, 500, 1500)

		metadata, err := media.DetectBytes(data, "")
		require.NoError(t, err)
		assert.Equal(t, int64(1500), metadata.DurationMS)
		_, reason := media.InspectBytes(data, "", media.Policy{
			MaxBytes: media.MaxBytes, MaxPixels: media.DefaultMaxPixels, MaxDurationMS: 1000, AllowVideo: true,
		})
		assert.Equal(t, media.ReasonTooLong, reason)
	})

	t.Run("cropped AVC bounds the uncropped coded frame", func(t *testing.T) {
		metadata, err := media.DetectBytes(mediatest.MP4(18, 18, 500), "")
		require.NoError(t, err)
		assert.Equal(t, int64(32), metadata.Width)
		assert.Equal(t, int64(32), metadata.Height)
	})

	t.Run("cropped HEVC bounds the uncropped coded frame", func(t *testing.T) {
		metadata, err := media.DetectBytes(mp4WithHEVCDimensions(18, 18, 32, 32), "")
		require.NoError(t, err)
		assert.Equal(t, int64(32), metadata.Width)
		assert.Equal(t, int64(32), metadata.Height)
	})

	t.Run("codec dimensions override smaller summaries", func(t *testing.T) {
		data := decodableAVCMP4(t)
		tkhd := mp4TestBoxPayload(t, data, "tkhd")
		binary.BigEndian.PutUint32(tkhd[76:80], 1<<16)
		binary.BigEndian.PutUint32(tkhd[80:84], 1<<16)
		stsd := bytes.Index(data, []byte("stsd"))
		require.NotEqual(t, -1, stsd)
		avc1Relative := bytes.Index(data[stsd+4:], []byte("avc1"))
		require.NotEqual(t, -1, avc1Relative)
		avc1 := stsd + 4 + avc1Relative
		binary.BigEndian.PutUint16(data[avc1+28:avc1+30], 1)
		binary.BigEndian.PutUint16(data[avc1+30:avc1+32], 1)

		metadata, err := media.DetectBytes(data, "")
		require.NoError(t, err)
		assert.Equal(t, int64(16), metadata.Width)
		assert.Equal(t, int64(16), metadata.Height)
		_, reason := media.InspectBytes(data, "", media.Policy{MaxBytes: media.MaxBytes, MaxPixels: 4, AllowVideo: true})
		assert.Equal(t, media.ReasonTooManyPixels, reason)
	})

	t.Run("sample timing overrides shorter summaries", func(t *testing.T) {
		data := decodableAVCMP4(t)
		binary.BigEndian.PutUint32(mp4TestBoxPayload(t, data, "mvhd")[16:20], 1)
		binary.BigEndian.PutUint32(mp4TestBoxPayload(t, data, "tkhd")[20:24], 1)
		binary.BigEndian.PutUint32(mp4TestBoxPayload(t, data, "mdhd")[16:20], 1)

		metadata, err := media.DetectBytes(data, "")
		require.NoError(t, err)
		assert.Equal(t, int64(1000), metadata.DurationMS)
		assert.True(t, metadata.DurationKnown)
		_, reason := media.InspectBytes(data, "", media.Policy{
			MaxBytes: media.MaxBytes, MaxPixels: media.DefaultMaxPixels, MaxDurationMS: 500, AllowVideo: true,
		})
		assert.Equal(t, media.ReasonTooLong, reason)
	})

	for _, tt := range []struct {
		name    string
		version byte
		offset  uint32
	}{
		{name: "unsigned composition offsets", version: 0, offset: 1000},
		{name: "signed composition offsets", version: 1, offset: math.MaxUint32 - 249},
	} {
		t.Run(tt.name+" leave duration unknown", func(t *testing.T) {
			data := mp4WithCTTS(tt.version, 1, tt.offset)

			metadata, err := media.DetectBytes(data, "")
			require.NoError(t, err)
			assert.False(t, metadata.DurationKnown)
			_, reason := media.InspectBytes(data, "", media.Policy{
				MaxBytes: media.MaxBytes, MaxPixels: media.DefaultMaxPixels, MaxDurationMS: 500, AllowVideo: true,
			})
			assert.Equal(t, media.ReasonTooLong, reason)
		})
	}

	t.Run("hevc codec dimensions override smaller summaries", func(t *testing.T) {
		data := decodableHEVCMP4(t)
		tkhd := mp4TestBoxPayload(t, data, "tkhd")
		binary.BigEndian.PutUint32(tkhd[76:80], 1<<16)
		binary.BigEndian.PutUint32(tkhd[80:84], 1<<16)
		stsd := bytes.Index(data, []byte("stsd"))
		require.NotEqual(t, -1, stsd)
		hvc1Relative := bytes.Index(data[stsd+4:], []byte("hvc1"))
		require.NotEqual(t, -1, hvc1Relative)
		hvc1 := stsd + 4 + hvc1Relative
		binary.BigEndian.PutUint16(data[hvc1+28:hvc1+30], 1)
		binary.BigEndian.PutUint16(data[hvc1+30:hvc1+32], 1)

		metadata, err := media.DetectBytes(data, "")
		require.NoError(t, err)
		assert.Equal(t, int64(16), metadata.Width)
		assert.Equal(t, int64(16), metadata.Height)
	})
}

// This 16x16, two-frame H.264 MP4 is a deterministic synthetic red video.
// Its SEI encoder metadata was removed; changing only container summaries
// leaves the codec configuration and media samples decodable.
const decodableAVCMP4Base64 = "AAAAIGZ0eXBpc29tAAACAGlzb21pc28yYXZjMW1wNDEAAAMGbW9vdgAAAGxtdmhkAAAAAAAAAAAAAAAAAAAD6AAAA+gAAQAAAQAAAAAAAAAAAAAAAAEAAAAAAAAAAAAAAAAAAAABAAAAAAAAAAAAAAAAAABAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAgAAAlV0cmFrAAAAXHRraGQAAAADAAAAAAAAAAAAAAABAAAAAAAAA+gAAAAAAAAAAAAAAAAAAAAAAAEAAAAAAAAAAAAAAAAAAAABAAAAAAAAAAAAAAAAAABAAAAAABAAAAAQAAAAAAAkZWR0cwAAABxlbHN0AAAAAAAAAAEAAAPoAAAAAAABAAAAAAHNbWRpYQAAACBtZGhkAAAAAAAAAAAAAAAAAABAAAAAQABVxAAAAAAALWhkbHIAAAAAAAAAAHZpZGUAAAAAAAAAAAAAAABWaWRlb0hhbmRsZXIAAAABeG1pbmYAAAAUdm1oZAAAAAEAAAAAAAAAAAAAACRkaW5mAAAAHGRyZWYAAAAAAAAAAQAAAAx1cmwgAAAAAQAAAThzdGJsAAAAuHN0c2QAAAAAAAAAAQAAAKhhdmMxAAAAAAAAAAEAAAAAAAAAAAAAAAAAAAAAABAAEABIAAAASAAAAAAAAAABDExhdmMgbGlieDI2NAAAAAAAAAAAAAAAAAAAAAAAAAAAGP//AAAALmF2Y0MBQsAK/+EAFmdCwArZHsBEAAADAAQAAAMAEDxImSABAAVoy4PLIAAAABBwYXNwAAAAAQAAAAEAAAAUYnRydAAAAAAAAAEQAAAAAAAAABhzdHRzAAAAAAAAAAEAAAACAAAgAAAAABRzdHNzAAAAAAAAAAEAAAABAAAAHHN0c2MAAAAAAAAAAQAAAAEAAAACAAAAAQAAABxzdHN6AAAAAAAAAAAAAAACAAAAGAAAAAoAAAAUc3RjbwAAAAAAAAABAAADNgAAAD11ZHRhAAAANW1ldGEAAAAAAAAAIWhkbHIAAAAAAAAAAG1kaXJhcHBsAAAAAAAAAAAAAAAACGlsc3QAAAAIZnJlZQAAACptZGF0AAAAFGWIhAU8RigAC0rHAAE0mOAANAWAAAAABkGaOAl6gA=="

const decodableHEVCMP4Base64 = "AAAAHGZ0eXBpc29tAAACAGlzb21pc28ybXA0MQAAAzltb292AAAAbG12aGQAAAAAAAAAAAAAAAAAAAPoAAAD6AABAAABAAAAAAAAAAAAAAAAAQAAAAAAAAAAAAAAAAAAAAEAAAAAAAAAAAAAAAAAAEAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAACAAACiHRyYWsAAABcdGtoZAAAAAMAAAAAAAAAAAAAAAEAAAAAAAAD6AAAAAAAAAAAAAAAAAAAAAAAAQAAAAAAAAAAAAAAAAAAAAEAAAAAAAAAAAAAAAAAAEAAAAAAEAAAABAAAAAAACRlZHRzAAAAHGVsc3QAAAAAAAAAAQAAA+gAAAAAAAEAAAAAAgBtZGlhAAAAIG1kaGQAAAAAAAAAAAAAAAAAAEAAAABAAFXEAAAAAAAtaGRscgAAAAAAAAAAdmlkZQAAAAAAAAAAAAAAAFZpZGVvSGFuZGxlcgAAAAGrbWluZgAAABR2bWhkAAAAAQAAAAAAAAAAAAAAJGRpbmYAAAAcZHJlZgAAAAAAAAABAAAADHVybCAAAAABAAABa3N0YmwAAAEHc3RzZAAAAAAAAAABAAAA92h2YzEAAAAAAAAAAQAAAAAAAAAAAAAAAAAAAAAAEAAQAEgAAABIAAAAAAAAAAEMTGF2YyBsaWJ4MjY1AAAAAAAAAAAAAAAAAAAAAAAAAAAY//8AAABzaHZjQwEBYAAAAJAAAAAAAB7wAPz9+PgAAA8DoAABABhAAQwB//8BYAAAAwCQAAADAAADAB6SgJChAAEAJ0IBAQFgAAADAJAAAAMAAAMAHqCIRZZKq8rwFoCAAAADAIAAAAMAhKIAAQAGRAHBc9CJAAAACmZpZWwBAAAAABBwYXNwAAAAAQAAAAEAAAAUYnRydAAAAAAAAACwAAAAAAAAABhzdHRzAAAAAAAAAAEAAAABAABAAAAAABxzdHNjAAAAAAAAAAEAAAABAAAAAQAAAAEAAAAUc3RzegAAAAAAAAAWAAAAAQAAABRzdGNvAAAAAAAAAAEAAANlAAAAPXVkdGEAAAA1bWV0YQAAAAAAAAAhaGRscgAAAAAAAAAAbWRpcmFwcGwAAAAAAAAAAAAAAAAIaWxzdAAAAAhmcmVlAAAAHm1kYXQAAAASKAGvE4D1JKP/sMNYO5pB9f+A"

func decodableAVCMP4(t *testing.T) []byte {
	t.Helper()
	data, err := base64.StdEncoding.DecodeString(decodableAVCMP4Base64)
	require.NoError(t, err)
	return data
}

func decodableHEVCMP4(t *testing.T) []byte {
	t.Helper()
	data, err := base64.StdEncoding.DecodeString(decodableHEVCMP4Base64)
	require.NoError(t, err)
	return data
}

func mp4TestBoxPayload(t *testing.T, data []byte, kind string) []byte {
	t.Helper()
	index := bytes.Index(data, []byte(kind))
	require.GreaterOrEqual(t, index, 4)
	size := int(binary.BigEndian.Uint32(data[index-4 : index]))
	require.GreaterOrEqual(t, size, 8)
	require.LessOrEqual(t, index-4+size, len(data))
	return data[index+4 : index-4+size]
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
	actlData := make([]byte, 8)
	binary.BigEndian.PutUint32(actlData[0:4], uint32(declared))
	out := append([]byte(nil), png[:ihdrEnd]...)
	out = append(out, pngTestChunk("acTL", actlData)...)
	idat := pngTestChunkPayloads(png, "IDAT")
	sequence := uint32(0)
	for frame := range actual {
		fctlData := make([]byte, 26)
		binary.BigEndian.PutUint32(fctlData[0:4], sequence)
		sequence++
		copy(fctlData[4:12], png[16:24])
		out = append(out, pngTestChunk("fcTL", fctlData)...)
		if frame == 0 {
			for _, payload := range idat {
				out = append(out, pngTestChunk("IDAT", payload)...)
			}
			continue
		}
		for _, payload := range idat {
			fdatData := binary.BigEndian.AppendUint32(nil, sequence)
			sequence++
			fdatData = append(fdatData, payload...)
			out = append(out, pngTestChunk("fdAT", fdatData)...)
		}
	}
	return append(out, pngTestChunk("IEND", nil)...)
}

func apngOutsideCanvas(png []byte) []byte {
	out := apng(png, 1)
	index := bytes.Index(out, []byte("fcTL"))
	binary.BigEndian.PutUint32(out[index+8:index+12], binary.BigEndian.Uint32(png[16:20])+1)
	pngRewriteChunkCRC(out, index)
	return out
}

func apngIDATGeometryMismatch(png []byte) []byte {
	out := apng(png, 2)
	index := bytes.Index(out, []byte("fcTL"))
	binary.BigEndian.PutUint32(out[index+8:index+12], binary.BigEndian.Uint32(png[16:20])-1)
	pngRewriteChunkCRC(out, index)
	return out
}

func apngFCTLBeforeACTL(png []byte) []byte {
	out := apng(png, 2)
	actl := pngTestChunkRange(out, "acTL")
	fctl := pngTestChunkRange(out, "fcTL")
	return slices.Concat(out[:actl[0]], out[fctl[0]:fctl[1]], out[actl[0]:actl[1]], out[fctl[1]:])
}

func apngWithEmptyIDATChunk(png []byte) []byte {
	out := apng(png, 2)
	idat := pngTestChunkRange(out, "IDAT")
	return slices.Concat(out[:idat[0]], pngTestChunk("IDAT", nil), out[idat[0]:])
}

func apngWithEmptyFDATChunk(png []byte) []byte {
	out := apng(png, 2)
	fdat := pngTestChunkRange(out, "fdAT")
	kindOffset := fdat[0] + 4
	sequence := binary.BigEndian.Uint32(out[kindOffset+4 : kindOffset+8])
	binary.BigEndian.PutUint32(out[kindOffset+4:kindOffset+8], sequence+1)
	pngRewriteChunkCRC(out, kindOffset)
	emptyData := binary.BigEndian.AppendUint32(nil, sequence)
	return slices.Concat(out[:fdat[0]], pngTestChunk("fdAT", emptyData), out[fdat[0]:])
}

func apngBadCRC(png []byte) []byte {
	out := apng(png, 2)
	index := bytes.Index(out, []byte("fcTL"))
	size := int(binary.BigEndian.Uint32(out[index-4 : index]))
	out[index+4+size] ^= 1
	return out
}

func apngOutOfOrderSequence(png []byte) []byte {
	out := apng(png, 2)
	index := bytes.Index(out, []byte("fdAT"))
	binary.BigEndian.PutUint32(out[index+4:index+8], 0)
	pngRewriteChunkCRC(out, index)
	return out
}

func apngFrameWithoutData(png []byte) []byte {
	out := apng(png, 2)
	index := bytes.Index(out, []byte("IDAT"))
	start := index - 4
	size := int(binary.BigEndian.Uint32(out[start:index]))
	return slices.Concat(out[:start], out[start+12+size:])
}

func pngTestChunk(kind string, payload []byte) []byte {
	chunk := make([]byte, 12+len(payload))
	binary.BigEndian.PutUint32(chunk[:4], uint32(len(payload)))
	copy(chunk[4:8], kind)
	copy(chunk[8:], payload)
	binary.BigEndian.PutUint32(chunk[8+len(payload):], crc32.ChecksumIEEE(chunk[4:8+len(payload)]))
	return chunk
}

func pngTestChunkPayloads(png []byte, want string) [][]byte {
	var payloads [][]byte
	for offset := 8; offset+12 <= len(png); {
		size := int(binary.BigEndian.Uint32(png[offset : offset+4]))
		if string(png[offset+4:offset+8]) == want {
			payloads = append(payloads, append([]byte(nil), png[offset+8:offset+8+size]...))
		}
		offset += 12 + size
	}
	return payloads
}

func pngTestChunkRange(png []byte, want string) [2]int {
	kindOffset := bytes.Index(png, []byte(want))
	if kindOffset < 4 {
		panic("PNG test chunk not found: " + want)
	}
	start := kindOffset - 4
	size := int(binary.BigEndian.Uint32(png[start:kindOffset]))
	return [2]int{start, start + 12 + size}
}

func pngRewriteChunkCRC(png []byte, kindOffset int) {
	size := int(binary.BigEndian.Uint32(png[kindOffset-4 : kindOffset]))
	binary.BigEndian.PutUint32(png[kindOffset+4+size:], crc32.ChecksumIEEE(png[kindOffset:kindOffset+4+size]))
}

// mp4AudioTrackFirst places a zero-dimension track before the picture track.
func mp4AudioTrackFirst(width, height int, durationMS int64) []byte {
	mvhd := make([]byte, 20)
	binary.BigEndian.PutUint32(mvhd[12:16], 1000)
	binary.BigEndian.PutUint32(mvhd[16:20], uint32(durationMS))
	audio := mediatest.Box("trak", append(mediatest.Box("tkhd", make([]byte, 84)), mp4TimedHandler("soun", durationMS)...))
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
	trak = append(trak, mp4SampleTableDuration(codedWidth, codedHeight, 500)...)
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
	return mp4SampleTableDuration(width, height, 0)
}

func mp4SampleTableDuration(width, height int, durationMS int64) []byte {
	return mp4SampleTableDurationWithExtra(width, height, durationMS, nil)
}

func mp4SampleTableDurationWithExtra(width, height int, durationMS int64, extra []byte) []byte {
	sample := make([]byte, 78)
	binary.BigEndian.PutUint16(sample[24:26], uint16(width))
	binary.BigEndian.PutUint16(sample[26:28], uint16(height))
	if width > 0 && height > 0 {
		sample = slices.Concat(sample, mediatest.Box("avcC", mediatest.AVCConfig(width, height)))
	}
	stsd := make([]byte, 0, 8+8+len(sample))
	stsd = append(stsd, make([]byte, 8)...)
	binary.BigEndian.PutUint32(stsd[4:8], 1)
	stsd = append(stsd, mediatest.Box("avc1", sample)...)
	stts := mp4STTS(durationMS)
	stsz := mp4STSZ(durationMS)
	minf := mediatest.Box("minf", mediatest.Box("stbl", slices.Concat(
		mediatest.Box("stsd", stsd), mediatest.Box("stts", stts), extra, mediatest.Box("stsz", stsz),
	)))
	mdhd := make([]byte, 24)
	binary.BigEndian.PutUint32(mdhd[12:16], 1000)
	binary.BigEndian.PutUint32(mdhd[16:20], uint32(durationMS))
	return mediatest.Box("mdia", append(append(mediatest.Box("mdhd", mdhd), mp4Handler("vide")...), minf...))
}

func mp4TimedHandler(kind string, durationMS int64) []byte {
	mdhd := make([]byte, 24)
	binary.BigEndian.PutUint32(mdhd[12:16], 1000)
	binary.BigEndian.PutUint32(mdhd[16:20], uint32(durationMS))
	stbl := mediatest.Box("stbl", append(mediatest.Box("stts", mp4STTS(durationMS)), mediatest.Box("stsz", mp4STSZ(durationMS))...))
	return mediatest.Box("mdia", append(append(mediatest.Box("mdhd", mdhd), mp4Handler(kind)...), mediatest.Box("minf", stbl)...))
}

func mp4STTS(durationMS int64) []byte {
	stts := make([]byte, 8)
	if durationMS > 0 {
		binary.BigEndian.PutUint32(stts[4:8], 1)
		entry := make([]byte, 8)
		binary.BigEndian.PutUint32(entry[:4], 1)
		binary.BigEndian.PutUint32(entry[4:8], uint32(durationMS))
		stts = slices.Concat(stts, entry)
	}
	return stts
}

func mp4STSZ(durationMS int64) []byte {
	stsz := make([]byte, 12)
	binary.BigEndian.PutUint32(stsz[4:8], 1)
	if durationMS > 0 {
		binary.BigEndian.PutUint32(stsz[8:12], 1)
	}
	return stsz
}

func mp4WithSampleCountMismatch() []byte {
	data := mediatest.MP4(16, 16, 500)
	box := bytes.Index(data, []byte("stsz"))
	binary.BigEndian.PutUint32(data[box+12:box+16], 2)
	return data
}

func mp4WithZeroSampleDelta() []byte {
	data := mediatest.MP4(16, 16, 500)
	box := bytes.Index(data, []byte("stts"))
	binary.BigEndian.PutUint32(data[box+16:box+20], 0)
	return data
}

func mp4WithCTTS(version byte, sampleCount, offset uint32) []byte {
	ctts := make([]byte, 16)
	ctts[0] = version
	binary.BigEndian.PutUint32(ctts[4:8], 1)
	binary.BigEndian.PutUint32(ctts[8:12], sampleCount)
	binary.BigEndian.PutUint32(ctts[12:16], offset)
	ftyp := mediatest.Box("ftyp", append([]byte("isom"), make([]byte, 12)...))
	mvhd := make([]byte, 20)
	binary.BigEndian.PutUint32(mvhd[12:16], 1000)
	binary.BigEndian.PutUint32(mvhd[16:20], 500)
	tkhd := make([]byte, 84)
	binary.BigEndian.PutUint32(tkhd[20:24], 500)
	binary.BigEndian.PutUint32(tkhd[76:80], 16<<16)
	binary.BigEndian.PutUint32(tkhd[80:84], 16<<16)
	trak := mediatest.Box("trak", append(
		mediatest.Box("tkhd", tkhd),
		mp4SampleTableDurationWithExtra(16, 16, 500, mediatest.Box("ctts", ctts))...,
	))
	return append(ftyp, mediatest.Box("moov", append(mediatest.Box("mvhd", mvhd), trak...))...)
}

func mp4MisplacedCTTS() []byte {
	ctts := make([]byte, 16)
	binary.BigEndian.PutUint32(ctts[4:8], 1)
	binary.BigEndian.PutUint32(ctts[8:12], 1)
	return append(mediatest.MP4(16, 16, 500), mediatest.Box("ctts", ctts)...)
}

func mp4WithEditListDuration(width, height int, movieMS, editMS int64) []byte {
	return mp4WithEditListPayload(width, height, movieMS, mp4EditList(0, 1, uint64(editMS)))
}

func mp4WithEditListVersion(version byte) []byte {
	return mp4WithEditListPayload(16, 16, 500, mp4EditList(version, 1, 500))
}

func mp4WithEditListRate(rate int16) []byte {
	return mp4WithEditListPayload(16, 16, 500, mp4EditList(0, rate, 500))
}

func mp4WithInvalidEditMediaTime() []byte {
	elst := mp4EditList(0, 1, 500)
	binary.BigEndian.PutUint32(elst[12:16], math.MaxUint32-1)
	return mp4WithEditListPayload(16, 16, 500, elst)
}

func mp4WithEditListPayload(width, height int, movieMS int64, elst []byte) []byte {
	ftyp, mvhd, trak := mp4Parts(width, height, movieMS)
	trackPayload := slices.Concat(trak[8:], mediatest.Box("edts", mediatest.Box("elst", elst)))
	return append(ftyp, mediatest.Box("moov", append(mvhd, mediatest.Box("trak", trackPayload)...))...)
}

func mp4EditList(version byte, rate int16, durations ...uint64) []byte {
	entrySize := 12
	if version == 1 {
		entrySize = 20
	}
	payload := make([]byte, 8+len(durations)*entrySize)
	payload[0] = version
	binary.BigEndian.PutUint32(payload[4:8], uint32(len(durations)))
	for index, duration := range durations {
		offset := 8 + index*entrySize
		if version == 1 {
			binary.BigEndian.PutUint64(payload[offset:offset+8], duration)
			binary.BigEndian.PutUint64(payload[offset+8:offset+16], 0)
			binary.BigEndian.PutUint16(payload[offset+16:offset+18], uint16(rate))
		} else {
			binary.BigEndian.PutUint32(payload[offset:offset+4], uint32(duration))
			binary.BigEndian.PutUint32(payload[offset+4:offset+8], 0)
			binary.BigEndian.PutUint16(payload[offset+8:offset+10], uint16(rate))
		}
	}
	return payload
}

func mp4MisplacedELST() []byte {
	return append(mediatest.MP4(16, 16, 500), mediatest.Box("elst", mp4EditList(0, 1, 500))...)
}

func mp4WithHEVCDimensions(visibleWidth, visibleHeight, codedWidth, codedHeight int) []byte {
	ftyp := mediatest.Box("ftyp", append([]byte("isom"), make([]byte, 12)...))
	mvhd := make([]byte, 20)
	binary.BigEndian.PutUint32(mvhd[12:16], 1000)
	binary.BigEndian.PutUint32(mvhd[16:20], 500)
	tkhd := make([]byte, 84)
	binary.BigEndian.PutUint32(tkhd[76:80], uint32(visibleWidth<<16))
	binary.BigEndian.PutUint32(tkhd[80:84], uint32(visibleHeight<<16))
	sample := make([]byte, 78)
	binary.BigEndian.PutUint16(sample[24:26], uint16(visibleWidth))
	binary.BigEndian.PutUint16(sample[26:28], uint16(visibleHeight))
	sample = slices.Concat(sample, mediatest.Box("hvcC", mediatest.HEVCConfig(codedWidth, codedHeight, visibleWidth, visibleHeight)))
	stsd := make([]byte, 8)
	binary.BigEndian.PutUint32(stsd[4:8], 1)
	stsd = slices.Concat(stsd, mediatest.Box("hvc1", sample))
	mdhd := make([]byte, 24)
	binary.BigEndian.PutUint32(mdhd[12:16], 1000)
	binary.BigEndian.PutUint32(mdhd[16:20], 500)
	stbl := mediatest.Box("stbl", slices.Concat(
		mediatest.Box("stsd", stsd), mediatest.Box("stts", mp4STTS(500)), mediatest.Box("stsz", mp4STSZ(500)),
	))
	mdia := mediatest.Box("mdia", slices.Concat(
		mediatest.Box("mdhd", mdhd), mp4Handler("vide"), mediatest.Box("minf", stbl),
	))
	trak := mediatest.Box("trak", append(mediatest.Box("tkhd", tkhd), mdia...))
	return append(ftyp, mediatest.Box("moov", append(mediatest.Box("mvhd", mvhd), trak...))...)
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
	trak = mediatest.Box("trak", append(mediatest.Box("tkhd", tkhd), mp4SampleTableDuration(width, height, durationMS)...))
	return ftyp, mvhd, trak
}

func mp4WithoutMovieHeader(trackMS int64) []byte {
	ftyp, _, trak := mp4Parts(16, 16, 500)
	tkhd := trak[16:] // strip trak and tkhd headers
	binary.BigEndian.PutUint32(tkhd[20:24], uint32(trackMS))
	return append(ftyp, mediatest.Box("moov", trak)...)
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

func mp4WithMoof(width, height int, movieMS int64) []byte {
	ftyp, mvhd, trak := mp4Parts(width, height, movieMS)
	moov := mediatest.Box("moov", append(mvhd, trak...))
	moof := mediatest.Box("moof", mediatest.Box("mfhd", make([]byte, 8)))
	return append(ftyp, append(moov, moof...)...)
}

func mp4V1SentinelDuration(width, height int) []byte {
	data := mp4Version1(width, height, 1000, 1)
	payload := data[bytes.Index(data, []byte("mvhd"))+4:]
	binary.BigEndian.PutUint64(payload[24:32], math.MaxUint64)
	return data
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

func mp4WithTrailingTKHD(version byte) []byte {
	ftyp, mvhd, _ := mp4Parts(16, 16, 500)
	payloadSize, widthOffset := 92, 76
	if version == 1 {
		payloadSize, widthOffset = 104, 88
	}
	tkhd := make([]byte, payloadSize)
	tkhd[0] = version
	binary.BigEndian.PutUint32(tkhd[widthOffset:widthOffset+4], uint32(4096<<16))
	binary.BigEndian.PutUint32(tkhd[widthOffset+4:widthOffset+8], uint32(2160<<16))
	binary.BigEndian.PutUint32(tkhd[payloadSize-8:payloadSize-4], uint32(16<<16))
	binary.BigEndian.PutUint32(tkhd[payloadSize-4:], uint32(16<<16))
	trak := mediatest.Box("trak", append(mediatest.Box("tkhd", tkhd), mp4SampleTable(16, 16)...))
	return append(ftyp, mediatest.Box("moov", append(mvhd, trak...))...)
}

func mp4WithMVHDVersion(version byte) []byte {
	ftyp, mvhd, trak := mp4Parts(320, 240, 500)
	mvhd[8] = version
	return append(ftyp, mediatest.Box("moov", append(mvhd, trak...))...)
}

func mp4WithRenamedBox(kind string) []byte {
	data := mediatest.MP4(320, 240, 500)
	index := bytes.Index(data, []byte(kind))
	if index >= 0 {
		copy(data[index:index+4], "free")
	}
	return data
}
