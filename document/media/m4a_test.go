package media_test

import (
	"bytes"
	"encoding/binary"
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
					record, err := media.InspectCapability(bytes.NewReader(data), policy)
					require.NoError(t, err)
					require.True(t, record.Eligible, record.Reason)
					require.Equal(t, "audio", record.MediaFamily)
					require.Equal(t, "m4a", record.Format)
					require.Equal(t, "audio/mp4", record.MediaType)
					require.Equal(t, int64(1000), record.Measurements.DurationMS)
					require.NoError(t, media.ValidateCapabilityRecord(record))
					policy.MaxDurationMS = 999
					record, err = media.InspectCapability(bytes.NewReader(data), policy)
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
			record, err := media.InspectCapability(bytes.NewReader(data), policy)
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
	record, err := media.InspectCapability(bytes.NewReader(data), policy)
	require.NoError(t, err)
	require.True(t, record.Eligible, record.Reason)
	require.Equal(t, "video", record.MediaFamily)
	require.Equal(t, "mp4", record.Format)
	policy = inspectionPolicy(data, "disguised.m4a", "audio/mp4")
	policy.MaxDurationMS = 20000
	record, err = media.InspectCapability(bytes.NewReader(data), policy)
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
		record, err := media.InspectCapability(bytes.NewReader(data), policy)
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
	record, err := media.InspectCapability(bytes.NewReader(data), policy)
	require.NoError(t, err)
	require.False(t, record.Eligible)
	require.Equal(t, media.CapabilityReasonEntryBytes, record.Reason)
	policy.MaxEntryBytes = 1 << 20
	policy.ExpectedSHA256 = strings.Repeat("a", 64)
	_, err = media.InspectCapability(bytes.NewReader(data), policy)
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
			record, err := media.InspectCapability(bytes.NewReader(data), policy)
			require.NoError(t, err)
			require.True(t, record.Eligible, record.Reason)
			require.Equal(t, int64(2000), record.Measurements.DurationMS)
			policy.MaxDurationMS = 1999
			record, err = media.InspectCapability(bytes.NewReader(data), policy)
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
		record, err := media.InspectCapability(bytes.NewReader(data), policy)
		require.NoError(t, err)
		require.True(t, record.Eligible, record.Reason)
	}
}

func TestInspectM4AEncoderFixtures(t *testing.T) {
	for name, data := range map[string][]byte{"AAC": mediatest.AACM4A(), "ALAC": mediatest.ALACM4A()} {
		t.Run(name, func(t *testing.T) {
			policy := inspectionPolicy(data, "silent.m4a", "audio/mp4")
			policy.MaxDurationMS = 1000
			record, err := media.InspectCapability(bytes.NewReader(data), policy)
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
			record, err := media.InspectCapability(bytes.NewReader(data), policy)
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
			record, err := media.InspectCapability(bytes.NewReader(data), policy)
			require.NoError(t, err)
			require.True(t, record.Eligible, record.Reason)
			require.Equal(t, test.want, record.Measurements.DurationMS)
			policy.MaxDurationMS = test.want - 1
			record, err = media.InspectCapability(bytes.NewReader(data), policy)
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
	record, err := media.InspectCapability(bytes.NewReader(data), policy)
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
		record, err := media.InspectCapability(bytes.NewReader(data), policy)
		require.NoError(t, err)
		require.False(t, record.Eligible)
		require.Equal(t, want, record.Reason, "chunks=%d", chunks)
	}
}
