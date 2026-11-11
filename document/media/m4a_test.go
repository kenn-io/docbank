package media_test

import (
	"bytes"
	"encoding/binary"
	"io"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document/media"
	"go.kenn.io/docbank/document/media/mediatest"
)

func TestInspectM4A(t *testing.T) {
	for _, codec := range []string{"mp4a", "alac"} {
		for _, brand := range []string{"M4A ", "isom", "mp42"} {
			for _, mime := range []string{"audio/mp4", "audio/x-m4a"} {
				t.Run(codec+brand+mime, func(t *testing.T) {
					data := mediatest.M4A(codec)
					copy(data[8:12], brand)
					policy := inspectionPolicy(data, "call.M4A", mime)
					policy.MaxDurationMS = 1000
					record, err := media.InspectCapability(t.Context(), bytes.NewReader(data), policy)
					require.NoError(t, err)
					require.True(t, record.Eligible, record.Reason)
					require.Equal(t, "audio", record.MediaFamily)
					require.Equal(t, "m4a", record.Format)
					require.Equal(t, "audio/mp4", record.MediaType)
					require.Equal(t, int64(1000), record.Measurements.DurationMS)
					require.NoError(t, media.ValidateCapabilityRecord(record))
					policy.MaxDurationMS = 999
					record, err = media.InspectCapability(t.Context(), bytes.NewReader(data), policy)
					require.NoError(t, err)
					require.False(t, record.Eligible)
					require.Equal(t, media.CapabilityReasonSemanticUnits, record.Reason)
				})
			}
		}
	}
}

func TestInspectM4ARejectsUnsafeContainers(t *testing.T) {
	for name, mutate := range map[string]func([]byte) []byte{
		"truncated":         func(b []byte) []byte { return b[:len(b)-1] },
		"bad box length":    func(b []byte) []byte { binary.BigEndian.PutUint32(b[:4], 7); return b },
		"unsupported codec": func(b []byte) []byte { return bytes.Replace(b, []byte("mp4a"), []byte("Opus"), 1) },
		"encrypted entry":   func(b []byte) []byte { return bytes.Replace(b, []byte("mp4a"), []byte("enca"), 1) },
		"video handler":     func(b []byte) []byte { return bytes.Replace(b, []byte("soun"), []byte("vide"), 1) },
		"unknown duration": func(b []byte) []byte {
			i := bytes.Index(b, []byte("mdhd")) + 4
			binary.BigEndian.PutUint32(b[i+16:i+20], 0xffffffff)
			return b
		},
		"fragmented":    func(b []byte) []byte { return append(b, mediatest.Box("moof", nil)...) },
		"external data": func(b []byte) []byte { i := bytes.Index(b, []byte("url ")) + 4; b[i+3] = 0; return b },
		"sample outside mdat": func(b []byte) []byte {
			i := bytes.Index(b, []byte("stco")) + 4
			binary.BigEndian.PutUint32(b[i+8:i+12], 0)
			return b
		},
		"duplicate movie": func(b []byte) []byte { i := bytes.Index(b, []byte("moov")) - 4; return append(b, b[i:]...) },
		"bad AAC config":  func(b []byte) []byte { i := bytes.Index(b, []byte{0x40, 0x15}); b[i] = 0x6b; return b },
	} {
		t.Run(name, func(t *testing.T) {
			data := mutate(mediatest.M4A("mp4a"))
			policy := inspectionPolicy(data, "call.m4a", "audio/mp4")
			policy.MaxDurationMS = 1000
			record, err := media.InspectCapability(t.Context(), bytes.NewReader(data), policy)
			require.NoError(t, err)
			require.False(t, record.Eligible)
			require.Equal(t, media.CapabilityReasonMalformed, record.Reason)
		})
	}
	data := mediatest.H264AACMP4()
	policy := inspectionPolicy(data, "video.mp4", "video/mp4")
	policy.MaxDurationMS = 300000
	policy.MaxPixels = 1 << 20
	policy.MaxFrames = 18000
	record, err := media.InspectCapability(t.Context(), bytes.NewReader(data), policy)
	require.NoError(t, err)
	require.True(t, record.Eligible, record.Reason)
	require.Equal(t, "video", record.MediaFamily)
	require.Equal(t, "mp4", record.Format)
	policy = inspectionPolicy(data, "disguised.m4a", "audio/mp4")
	policy.MaxDurationMS = 20000
	record, err = media.InspectCapability(t.Context(), bytes.NewReader(data), policy)
	require.NoError(t, err)
	require.False(t, record.Eligible)
}

func TestInspectM4AIdentityAndMetadataBound(t *testing.T) {
	data := mediatest.M4A("mp4a")
	for _, labels := range [][2]string{
		{"call.m4a", "audio/mp4"}, {"call.mp3", "audio/mp4"}, {"call.m4a", "audio/mpeg"}, {"call.m4a", "video/mp4"},
	} {
		policy := inspectionPolicy(data, labels[0], labels[1])
		policy.MaxDurationMS = 1000
		record, err := media.InspectCapability(t.Context(), bytes.NewReader(data), policy)
		require.NoError(t, err)
		canonical := labels == [2]string{"call.m4a", "audio/mp4"}
		require.Equal(t, canonical, record.Eligible, labels)
		if !canonical {
			require.Equal(t, media.CapabilityReasonMalformed, record.Reason, labels)
		}
	}
	policy := inspectionPolicy(data, "call.m4a", "audio/mp4")
	policy.MaxDurationMS = 1000
	policy.MaxEntryBytes = 32
	record, err := media.InspectCapability(t.Context(), bytes.NewReader(data), policy)
	require.NoError(t, err)
	require.False(t, record.Eligible)
	require.Equal(t, media.CapabilityReasonEntryBytes, record.Reason)
	policy.MaxEntryBytes = 1 << 20
	policy.ExpectedSHA256 = strings.Repeat("a", 64)
	_, err = media.InspectCapability(t.Context(), bytes.NewReader(data), policy)
	require.ErrorContains(t, err, "SHA-256")
}

func TestInspectM4ALongestDuration(t *testing.T) {
	for _, kind := range []string{"mvhd", "tkhd", "mdhd", "stts"} {
		t.Run(kind, func(t *testing.T) {
			data := mediatest.M4A("mp4a")
			i := bytes.Index(data, []byte(kind)) + 4
			switch kind {
			case "mvhd":
				binary.BigEndian.PutUint32(data[i+16:i+20], 2000)
			case "tkhd":
				binary.BigEndian.PutUint32(data[i+20:i+24], 2000)
			case "mdhd":
				binary.BigEndian.PutUint32(data[i+16:i+20], 96000)
			case "stts":
				binary.BigEndian.PutUint32(data[i+12:i+16], 96000)
			}
			policy := inspectionPolicy(data, "call.m4a", "audio/mp4")
			policy.MaxDurationMS = 2000
			record, err := media.InspectCapability(t.Context(), bytes.NewReader(data), policy)
			require.NoError(t, err)
			require.True(t, record.Eligible, record.Reason)
			require.Equal(t, int64(2000), record.Measurements.DurationMS)
			policy.MaxDurationMS = 1999
			record, err = media.InspectCapability(t.Context(), bytes.NewReader(data), policy)
			require.NoError(t, err)
			require.False(t, record.Eligible)
			require.Equal(t, media.CapabilityReasonSemanticUnits, record.Reason)
		})
	}
}

func TestInspectM4AExtendedAndToEndBoxes(t *testing.T) {
	for _, extended := range []bool{false, true} {
		data := mediatest.M4A("alac")
		i := bytes.Index(data, []byte("moov")) - 4
		if extended {
			payload := data[i+8:]
			header := make([]byte, 16)
			binary.BigEndian.PutUint32(header[:4], 1)
			copy(header[4:8], "moov")
			binary.BigEndian.PutUint64(header[8:], uint64(16+len(payload)))
			data = append(append(data[:i:i], header...), payload...)
		} else {
			binary.BigEndian.PutUint32(data[i:i+4], 0)
		}
		policy := inspectionPolicy(data, "call.m4a", "audio/mp4")
		policy.MaxDurationMS = 1000
		record, err := media.InspectCapability(t.Context(), bytes.NewReader(data), policy)
		require.NoError(t, err)
		require.True(t, record.Eligible, record.Reason)
	}
}

func TestInspectM4AEncoderFixtures(t *testing.T) {
	for name, data := range map[string][]byte{"AAC": mediatest.AACM4A(), "ALAC": mediatest.ALACM4A()} {
		t.Run(name, func(t *testing.T) {
			policy := inspectionPolicy(data, "silent.m4a", "audio/mp4")
			policy.MaxDurationMS = 1000
			record, err := media.InspectCapability(t.Context(), bytes.NewReader(data), policy)
			require.NoError(t, err)
			require.True(t, record.Eligible, record.Reason)
			require.Equal(t, "audio", record.MediaFamily)
			require.Equal(t, "m4a", record.Format)
			require.GreaterOrEqual(t, record.Measurements.DurationMS, int64(50))
			require.Less(t, record.Measurements.DurationMS, int64(100))
		})
	}
}

func TestInspectM4ARejectsTruncatedAACConfiguration(t *testing.T) {
	for name, config := range map[string][]byte{"core coder delay": {0x11, 0x8a}, "HE AAC extension": {0x29, 0x88}, "extension flag": {0x11, 0x89}} {
		t.Run(name, func(t *testing.T) {
			data := bytes.Replace(mediatest.M4A("mp4a"), []byte{0x11, 0x88}, config, 1)
			policy := inspectionPolicy(data, "call.m4a", "audio/mp4")
			policy.MaxDurationMS = 1000
			record, err := media.InspectCapability(t.Context(), bytes.NewReader(data), policy)
			require.NoError(t, err)
			require.False(t, record.Eligible)
			require.Equal(t, media.CapabilityReasonMalformed, record.Reason)
		})
	}
}

// Header durations are caller-controlled. Scaling the mvhd and mdhd timescales
// by 100 shrinks every declared duration below 1 ms, but the stored frames
// still decode to 4 × 1024 (or 4 × 960) AAC samples, or one 4096-sample ALAC
// frame, at 48 kHz.
func TestInspectM4AMeasuresDecodedFrames(t *testing.T) {
	for name, test := range map[string]struct {
		data []byte
		want int64
	}{
		"AAC 1024-sample frames": {mediatest.AACM4A(), 86},
		"AAC 960-sample frames": {
			bytes.Replace(mediatest.AACM4A(), []byte{5, 0x80, 0x80, 0x80, 5, 0x11, 0x88}, []byte{5, 0x80, 0x80, 0x80, 5, 0x11, 0x8c}, 1),
			80,
		},
		"ALAC": {mediatest.ALACM4A(), 86},
	} {
		t.Run(name, func(t *testing.T) {
			data := test.data
			for _, kind := range []string{"mvhd", "mdhd"} {
				i := bytes.Index(data, []byte(kind)) + 4
				timescale := binary.BigEndian.Uint32(data[i+12 : i+16])
				binary.BigEndian.PutUint32(data[i+12:i+16], timescale*100)
			}
			policy := inspectionPolicy(data, "call.m4a", "audio/mp4")
			policy.MaxDurationMS = test.want
			record, err := media.InspectCapability(t.Context(), bytes.NewReader(data), policy)
			require.NoError(t, err)
			require.True(t, record.Eligible, record.Reason)
			require.Equal(t, test.want, record.Measurements.DurationMS)
			policy.MaxDurationMS = test.want - 1
			record, err = media.InspectCapability(t.Context(), bytes.NewReader(data), policy)
			require.NoError(t, err)
			require.False(t, record.Eligible)
			require.Equal(t, media.CapabilityReasonSemanticUnits, record.Reason)
		})
	}
}

// The fixture has three top-level boxes, so a limit of four is exhausted
// inside the movie box rather than at the top level.
func TestInspectM4AReportsNestedEntryCount(t *testing.T) {
	data := mediatest.M4A("mp4a")
	policy := inspectionPolicy(data, "call.m4a", "audio/mp4")
	policy.MaxDurationMS = 1000
	policy.MaxEntries = 4
	record, err := media.InspectCapability(t.Context(), bytes.NewReader(data), policy)
	require.NoError(t, err)
	require.False(t, record.Eligible)
	require.Equal(t, media.CapabilityReasonEntryCount, record.Reason)
}

func TestInspectM4ABoundsChunkCount(t *testing.T) {
	for chunks, want := range map[int]media.CapabilityReason{
		1 << 20:       media.CapabilityReasonMalformed, // within the bound; zero offsets lie outside mdat
		(1 << 20) + 1: media.CapabilityReasonEntryCount,
	} {
		// stco is the last box of stbl, minf, mdia, trak, moov, and the file,
		// so growing it grows each ancestor by the same number of bytes.
		data := mediatest.M4A("mp4a")
		grow := uint32(4 * (chunks - 1))
		for _, kind := range []string{"moov", "trak", "mdia", "minf", "stbl", "stco"} {
			i := bytes.Index(data, []byte(kind)) - 4
			binary.BigEndian.PutUint32(data[i:i+4], binary.BigEndian.Uint32(data[i:i+4])+grow)
		}
		i := bytes.Index(data, []byte("stco")) + 4
		binary.BigEndian.PutUint32(data[i+4:i+8], uint32(chunks))
		data = append(data, make([]byte, grow)...)
		policy := inspectionPolicy(data, "call.m4a", "audio/mp4")
		policy.MaxSourceBytes, policy.MaxExpandedBytes, policy.MaxEntryBytes = 8<<20, 8<<20, 8<<20
		policy.MaxDurationMS = 1000
		record, err := media.InspectCapability(t.Context(), bytes.NewReader(data), policy)
		require.NoError(t, err)
		require.False(t, record.Eligible)
		require.Equal(t, want, record.Reason, "chunks=%d", chunks)
	}
}

func TestInspectMP4AudioTrack(t *testing.T) {
	for name, data := range map[string][]byte{
		"AAC video":     mediatest.H264AACMP4(),
		"no sound":      mediatest.H265MP4(),
		"metadata only": bytes.Replace(mediatest.M4A("mp4a"), []byte("soun"), []byte("meta"), 1),
	} {
		t.Run(name, func(t *testing.T) {
			policy := inspectionPolicy(data, "phone.mp4", "video/mp4")
			policy.VideoAudioTrack = true
			policy.MaxDurationMS = 300000
			record, err := media.InspectCapability(t.Context(), bytes.NewReader(data), policy)
			require.NoError(t, err)
			require.Equal(t, name == "AAC video", record.Eligible, record.Reason)
			if record.Eligible {
				require.Equal(t, "video", record.MediaFamily)
				require.Equal(t, "mp4", record.Format)
				require.Equal(t, "video/mp4", record.MediaType)
				require.GreaterOrEqual(t, record.Measurements.DurationMS, int64(5000))
				require.NoError(t, media.ValidateCapabilityRecord(record))
			}
		})
	}
	data := mediatest.H264AACMP4()
	policy := inspectionPolicy(data, "phone.mov", "video/quicktime")
	policy.VideoAudioTrack, policy.MaxDurationMS = true, 300000
	record, err := media.InspectCapability(t.Context(), bytes.NewReader(data), policy)
	require.NoError(t, err)
	require.False(t, record.Eligible)
	policy.Filename, policy.DeclaredMediaType, policy.MaxEntryBytes = "phone.mp4", "video/mp4", 32
	record, err = media.InspectCapability(t.Context(), bytes.NewReader(data), policy)
	require.NoError(t, err)
	require.Equal(t, media.CapabilityReasonEntryBytes, record.Reason)
}

func TestInspectMP4AudioWithNonVisualTracks(t *testing.T) {
	for _, handler := range []string{"meta", "tmcd"} {
		t.Run(handler, func(t *testing.T) {
			data := mediatest.M4A("mp4a")
			movie := bytes.Index(data, []byte("moov")) - 4
			track := bytes.Index(data, []byte("trak")) - 4
			extra := bytes.Replace(bytes.Clone(data[track:]), []byte("soun"), []byte(handler), 1)
			data = append(data, extra...)
			binary.BigEndian.PutUint32(data[movie:movie+4], uint32(len(data)-movie))
			policy := inspectionPolicy(data, "phone.mp4", "video/mp4")
			policy.VideoAudioTrack, policy.MaxDurationMS = true, 300000
			record, err := media.InspectCapability(t.Context(), bytes.NewReader(data), policy)
			require.NoError(t, err)
			require.True(t, record.Eligible, record.Reason)
		})
	}
}

// The provider transcribes one extracted sound stream, so video mode refuses
// a second sound track even when both tracks qualify on their own.
func TestInspectMP4RejectsSecondSoundTrack(t *testing.T) {
	data := mediatest.M4A("mp4a")
	mdat := bytes.Index(data, []byte("mdat")) - 4
	// Give the second track its own four sample bytes after the first's.
	data = slices.Insert(data, mdat+12, 5, 6, 7, 8)
	binary.BigEndian.PutUint32(data[mdat:mdat+4], 16)
	movie := bytes.Index(data, []byte("moov")) - 4
	track := bytes.Index(data, []byte("trak")) - 4
	second := bytes.Clone(data[track:])
	binary.BigEndian.PutUint32(second[bytes.Index(second, []byte("tkhd"))+16:], 2)
	stco := bytes.Index(second, []byte("stco")) + 12
	binary.BigEndian.PutUint32(second[stco:stco+4], uint32(mdat+12)) //nolint:gosec // tiny fixture
	data = append(data, second...)
	binary.BigEndian.PutUint32(data[movie:movie+4], uint32(len(data)-movie)) //nolint:gosec // tiny fixture

	audio := inspectionPolicy(data, "call.m4a", "audio/mp4")
	audio.MaxDurationMS = 1000
	record, err := media.InspectCapability(t.Context(), bytes.NewReader(data), audio)
	require.NoError(t, err)
	require.True(t, record.Eligible, "audio-only M4A keeps its existing behavior: %s", record.Reason)

	video := inspectionPolicy(data, "phone.mp4", "video/mp4")
	video.VideoAudioTrack, video.MaxDurationMS = true, 300000
	record, err = media.InspectCapability(t.Context(), bytes.NewReader(data), video)
	require.NoError(t, err)
	require.False(t, record.Eligible)
	require.Equal(t, media.CapabilityReasonMalformed, record.Reason)
}

func TestInspectMP4AudioWithVideoCompositionOffsets(t *testing.T) {
	data, err := io.ReadAll(mediatest.SizedMP4VideoComposition(64 << 10))
	require.NoError(t, err)
	policy := inspectionPolicy(data, "phone.mp4", "video/mp4")
	policy.VideoAudioTrack, policy.MaxDurationMS = true, 300000
	record, err := media.InspectCapability(t.Context(), bytes.NewReader(data), policy)
	require.NoError(t, err)
	require.True(t, record.Eligible, record.Reason)
	require.Equal(t, "video", record.MediaFamily)
	require.Positive(t, record.Measurements.DurationMS)
}

func TestInspectMP4ExistingPolicyIdentity(t *testing.T) {
	data := mediatest.H264AACMP4()
	policy := inspectionPolicy(data, "phone.mp4", "video/mp4")
	policy.MaxDurationMS, policy.MaxPixels, policy.MaxFrames = 300000, 1<<20, 18000
	record, err := media.InspectCapability(t.Context(), bytes.NewReader(data), policy)
	require.NoError(t, err)
	require.True(t, record.Eligible)
	require.Equal(t, "a3255099414355444e81b8fce938cb7ffd0c6b0fea0e9ed384be187cd26a5625", record.PolicyFingerprint)
}

func TestInspectQuickTimeAudioTrack(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, filename, mime string
		mutate               func([]byte)
		eligible             bool
	}{
		{"native AAC", "phone.mov", "video/quicktime", nil, true},
		{"wrong suffix", "phone.mp4", "video/quicktime", nil, false},
		{"wrong MIME", "phone.mov", "video/mp4", nil, false},
		{"wrong brand", "phone.mov", "video/quicktime", func(b []byte) { copy(b[8:12], "isom") }, false},
		{"silent", "phone.mov", "video/quicktime", func(b []byte) { copy(b[bytes.LastIndex(b, []byte("trak")):], "free") }, false},
		{"silent wrong suffix", "phone.mp4", "video/quicktime", func(b []byte) { copy(b[bytes.LastIndex(b, []byte("trak")):], "free") }, false},
		{"sound only", "phone.mov", "video/quicktime", func(b []byte) {
			moov := bytes.Index(b, []byte("moov"))
			copy(b[moov+bytes.Index(b[moov:], []byte("trak")):], "free")
		}, false},
		{"bad descriptor", "phone.mov", "video/quicktime", func(b []byte) { b[bytes.Index(b, []byte("esds"))+8] = 0 }, false},
		{"duplicate descriptor", "phone.mov", "video/quicktime", func(b []byte) { copy(b[bytes.Index(b, []byte("chan")):], "esds") }, false},
		{"ambiguous wave", "phone.mov", "video/quicktime", func(b []byte) { copy(b[bytes.Index(b, []byte("frma")):], "esds") }, false},
		{"external reference", "phone.mov", "video/quicktime", func(b []byte) { b[bytes.LastIndex(b, []byte("url "))+7] = 0 }, false},
		{"empty sound edit", "phone.mov", "video/quicktime", func(b []byte) {
			binary.BigEndian.PutUint32(b[bytes.LastIndex(b, []byte("elst"))+16:], 0xffffffff)
		}, false},
		{"sound edit within AAC priming", "phone.mov", "video/quicktime", func(b []byte) {
			binary.BigEndian.PutUint32(b[bytes.LastIndex(b, []byte("elst"))+16:], 2112)
		}, true},
		{"sound edit beyond AAC priming", "phone.mov", "video/quicktime", func(b []byte) {
			binary.BigEndian.PutUint32(b[bytes.LastIndex(b, []byte("elst"))+16:], 4096)
		}, false},
		{"discontinuous sound timing", "phone.mov", "video/quicktime", func(b []byte) {
			binary.BigEndian.PutUint32(b[bytes.LastIndex(b, []byte("stts"))+16:], 2048)
		}, false},
		{"sample outside mdat", "phone.mov", "video/quicktime", func(b []byte) {
			binary.BigEndian.PutUint32(b[bytes.LastIndex(b, []byte("stco"))+12:], 0)
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data := mediatest.H264AACMOV()
			if tc.mutate != nil {
				tc.mutate(data)
			}
			policy := inspectionPolicy(data, tc.filename, tc.mime)
			policy.MaxDurationMS, policy.QuickTimeAudioTrack = 24*60*60*1000, true
			record, err := media.InspectCapability(t.Context(), bytes.NewReader(data), policy)
			require.NoError(t, err)
			require.Equal(t, tc.eligible, record.Eligible, record)
			if tc.eligible {
				require.Equal(t, "mov", record.Format)
				require.Equal(t, "video/quicktime", record.MediaType)
				require.GreaterOrEqual(t, record.Measurements.DurationMS, int64(1000))
			}
		})
	}
}

func TestInspectMP4AudioTimeline(t *testing.T) {
	for _, testCase := range []struct {
		name     string
		codec    string
		edits    []mediatest.SoundEdit
		eligible bool
	}{
		{name: "no edit list", codec: "mp4a", eligible: true},
		{name: "AAC priming 1024", codec: "mp4a", edits: []mediatest.SoundEdit{{Duration: 1000, MediaTime: 1024}}, eligible: true},
		{name: "AAC priming 2112", codec: "mp4a", edits: []mediatest.SoundEdit{{Duration: 1000, MediaTime: 2112}}, eligible: true},
		{name: "ALAC from zero", codec: "alac", edits: []mediatest.SoundEdit{{Duration: 1000, MediaTime: 0}}, eligible: true},
		{name: "delayed media start", codec: "mp4a", edits: []mediatest.SoundEdit{{Duration: 1000, MediaTime: 94976}}},
		{name: "empty edit delays sound", codec: "mp4a", edits: []mediatest.SoundEdit{{Duration: 1979, MediaTime: -1}, {Duration: 1000, MediaTime: 1024}}},
		{name: "multiple edits", codec: "mp4a", edits: []mediatest.SoundEdit{{Duration: 500, MediaTime: 1024}, {Duration: 500, MediaTime: 24000}}},
		{name: "ALAC media offset", codec: "alac", edits: []mediatest.SoundEdit{{Duration: 1000, MediaTime: 1024}}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			data := mediatest.M4AWithSoundEdits(testCase.codec, testCase.edits...)
			if testCase.edits == nil {
				data = mediatest.M4A(testCase.codec)
			}
			policy := inspectionPolicy(data, "phone.mp4", "video/mp4")
			policy.VideoAudioTrack, policy.MaxDurationMS = true, 300000
			record, err := media.InspectCapability(t.Context(), bytes.NewReader(data), policy)
			require.NoError(t, err)
			require.Equal(t, testCase.eligible, record.Eligible, record.Reason)
			if !testCase.eligible {
				require.Equal(t, media.CapabilityReasonMalformed, record.Reason)
			}
		})
	}
}

func TestInspectQuickTimeRejectsLegacyPacketSizing(t *testing.T) {
	t.Parallel()
	data := mediatest.H264AACMOV()
	// Collapse the sound stts to one run with delta one. Keep the sample
	// count, local offsets and sample sizes from the eligible native movie.
	stts := bytes.LastIndex(data, []byte("stts"))
	count := binary.BigEndian.Uint32(data[stts+12:]) + binary.BigEndian.Uint32(data[stts+20:])
	binary.BigEndian.PutUint32(data[stts+8:], 1)
	binary.BigEndian.PutUint32(data[stts+12:], count)
	binary.BigEndian.PutUint32(data[stts+16:], 1)
	for _, box := range []string{"moov", "trak", "mdia", "minf", "stbl", "stts"} {
		size := bytes.LastIndex(data[:stts+4], []byte(box)) - 4
		binary.BigEndian.PutUint32(data[size:], binary.BigEndian.Uint32(data[size:])-8)
	}
	data = slices.Delete(data, stts+20, stts+28)
	policy := inspectionPolicy(data, "phone.mov", "video/quicktime")
	policy.MaxDurationMS, policy.QuickTimeAudioTrack = 24*60*60*1000, true
	record, err := media.InspectCapability(t.Context(), bytes.NewReader(data), policy)
	require.NoError(t, err)
	require.False(t, record.Eligible)
	require.Equal(t, media.CapabilityReasonMalformed, record.Reason)
}

func TestInspectQuickTimeRejectsDelayedSound(t *testing.T) {
	t.Parallel()
	data := mediatest.H264AACMOV()
	elst := bytes.LastIndex(data, []byte("elst"))
	for _, box := range []string{"moov", "trak", "edts", "elst"} {
		size := bytes.LastIndex(data[:elst+4], []byte(box)) - 4
		binary.BigEndian.PutUint32(data[size:], binary.BigEndian.Uint32(data[size:])+12)
	}
	binary.BigEndian.PutUint32(data[elst+8:], 2)
	// Insert a half-second empty edit before the native AAC priming edit.
	// v4 applies the shared MP4 sound-timeline rule to QuickTime.
	empty := []byte{0, 0, 0x56, 0x22, 0xff, 0xff, 0xff, 0xff, 0, 1, 0, 0}
	data = slices.Insert(data, elst+12, empty...)
	policy := inspectionPolicy(data, "phone.mov", "video/quicktime")
	policy.MaxDurationMS, policy.QuickTimeAudioTrack = 24*60*60*1000, true
	record, err := media.InspectCapability(t.Context(), bytes.NewReader(data), policy)
	require.NoError(t, err)
	require.False(t, record.Eligible)
	require.Equal(t, media.CapabilityReasonMalformed, record.Reason)
}
