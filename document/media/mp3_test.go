package media_test

import (
	"bytes"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/document/media"
	"go.kenn.io/docbank/document/media/mediatest"
)

func TestInspectCapabilityProvesMP3Duration(t *testing.T) {
	t.Parallel()
	data := syntheticMP3Frames(10)
	policy := inspectionPolicy(data, "synthetic.mp3", "audio/mpeg")
	policy.MaxDurationMS = 262

	record, err := media.InspectCapability(bytes.NewReader(data), policy)
	require.NoError(t, err)
	require.True(t, record.Eligible, record.Reason)
	assert.Equal(t, "audio", record.MediaFamily)
	assert.Equal(t, "audio/mpeg", record.MediaType)
	assert.Equal(t, "mp3", record.Format)
	assert.Equal(t, int64(262), record.Measurements.DurationMS)

	policy.MaxDurationMS = 261
	record, err = media.InspectCapability(bytes.NewReader(data), policy)
	require.NoError(t, err)
	assert.False(t, record.Eligible)
	assert.Equal(t, media.CapabilityReasonVisualBounds, record.Reason)
	assert.Equal(t, int64(262), record.Measurements.DurationMS)
}

func TestInspectCapabilityAcceptsMPEGVersions(t *testing.T) {
	t.Parallel()
	for _, testCase := range []struct {
		name       string
		data       []byte
		durationMS int64
	}{
		{name: "MPEG-1", data: syntheticMP3Frames(1), durationMS: 27},
		{name: "MPEG-2", data: syntheticMPEG2Frame(), durationMS: 27},
		{name: "MPEG-2.5", data: syntheticMPEG25Frame(), durationMS: 53},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			policy := inspectionPolicy(testCase.data, "sample.mp3", "audio/mpeg")
			policy.MaxDurationMS = testCase.durationMS
			record, err := media.InspectCapability(bytes.NewReader(testCase.data), policy)
			require.NoError(t, err)
			require.True(t, record.Eligible, record.Reason)
			assert.Equal(t, testCase.durationMS, record.Measurements.DurationMS)
		})
	}
}

func TestInspectCapabilityAcceptsBoundedMP3Tags(t *testing.T) {
	t.Parallel()
	audio := mediatest.MP3()
	barePolicy := inspectionPolicy(audio, "sample.mp3", "audio/mpeg")
	barePolicy.MaxDurationMS = 1_000
	bare, err := media.InspectCapability(bytes.NewReader(audio), barePolicy)
	require.NoError(t, err)
	require.True(t, bare.Eligible, bare.Reason)

	id3v1 := make([]byte, 128)
	copy(id3v1, "TAG")
	tagged := slices.Concat(id3v24Tag([]byte("TEST"), true), audio, id3v1)
	policy := inspectionPolicy(tagged, "sample.mp3", "audio/mpeg")
	policy.MaxDurationMS = 1_000
	record, err := media.InspectCapability(bytes.NewReader(tagged), policy)
	require.NoError(t, err)
	require.True(t, record.Eligible, record.Reason)
	assert.Equal(t, bare.Measurements.DurationMS, record.Measurements.DurationMS)
	for _, version := range []byte{2, 3} {
		withHeader := append(id3v2Tag(version, nil), audio...)
		policy = inspectionPolicy(withHeader, "sample.mp3", "audio/mpeg")
		policy.MaxDurationMS = 1_000
		record, err = media.InspectCapability(bytes.NewReader(withHeader), policy)
		require.NoError(t, err)
		require.True(t, record.Eligible, "ID3v2.%d: %s", version, record.Reason)
	}

	maximumTag := slices.Concat(id3v24Tag(make([]byte, 1<<20), false), audio)
	policy = inspectionPolicy(maximumTag, "sample.mp3", "audio/mpeg")
	policy.MaxSourceBytes = int64(len(maximumTag))
	policy.MaxDurationMS = 1_000
	record, err = media.InspectCapability(bytes.NewReader(maximumTag), policy)
	require.NoError(t, err)
	require.True(t, record.Eligible, record.Reason)
}

func TestInspectCapabilityRejectsMalformedMP3Tags(t *testing.T) {
	t.Parallel()
	audio := mediatest.MP3()
	footer := id3v24Tag([]byte("TEST"), true)
	footer[len(footer)-1] ^= 1
	oversized := []byte{'I', 'D', '3', 4, 0, 0, 0, 0x40, 0, 1}

	for _, testCase := range []struct {
		name string
		tag  []byte
	}{
		{name: "truncated header", tag: []byte("ID3\x04\x00")},
		{name: "unknown version", tag: []byte{'I', 'D', '3', 5, 0, 0, 0, 0, 0, 0}},
		{name: "unknown flags", tag: []byte{'I', 'D', '3', 4, 0, 1, 0, 0, 0, 0}},
		{name: "invalid revision", tag: []byte{'I', 'D', '3', 4, 0xff, 0, 0, 0, 0, 0}},
		{name: "non synchsafe size", tag: []byte{'I', 'D', '3', 4, 0, 0, 0, 0, 0x80, 0}},
		{name: "declared body exceeds input", tag: []byte{'I', 'D', '3', 4, 0, 0, 0, 0, 4, 0}},
		{name: "declared body exceeds bound", tag: oversized},
		{name: "invalid footer", tag: footer},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			candidate := slices.Concat(testCase.tag, audio)
			policy := inspectionPolicy(candidate, "sample.mp3", "audio/mpeg")
			policy.MaxDurationMS = 1_000
			record, err := media.InspectCapability(bytes.NewReader(candidate), policy)
			require.NoError(t, err)
			assert.False(t, record.Eligible)
			assert.Equal(t, media.CapabilityReasonMalformed, record.Reason)
		})
	}
}

func TestInspectCapabilityRejectsMalformedMP3Frames(t *testing.T) {
	t.Parallel()
	for _, testCase := range []struct {
		name   string
		mutate func([]byte) []byte
	}{
		{name: "truncated frame", mutate: func(data []byte) []byte { return data[:len(data)-1] }},
		{name: "inconsistent MPEG version", mutate: func(data []byte) []byte { data[417+1] = 0xf3; return data }},
		{name: "inconsistent sample rate", mutate: func(data []byte) []byte { data[417+2] ^= 0x04; return data }},
		{name: "reserved MPEG version", mutate: func(data []byte) []byte { data[1] = data[1]&^0x18 | 0x08; return data }},
		{name: "reserved layer", mutate: func(data []byte) []byte { data[1] &^= 0x06; return data }},
		{name: "free format bitrate", mutate: func(data []byte) []byte { data[2] &^= 0xf0; return data }},
		{name: "reserved bitrate", mutate: func(data []byte) []byte { data[2] |= 0xf0; return data }},
		{name: "reserved sample rate", mutate: func(data []byte) []byte { data[2] |= 0x0c; return data }},
		{name: "reserved emphasis", mutate: func(data []byte) []byte { data[3] = data[3]&^0x03 | 0x02; return data }},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			data := testCase.mutate(syntheticMP3Frames(2))
			policy := inspectionPolicy(data, "sample.mp3", "audio/mpeg")
			policy.MaxDurationMS = 1_000
			record, err := media.InspectCapability(bytes.NewReader(data), policy)
			require.NoError(t, err)
			assert.False(t, record.Eligible)
			assert.Equal(t, media.CapabilityReasonMalformed, record.Reason)
		})
	}
}

func TestInspectCapabilityRejectsMP3IdentityMismatch(t *testing.T) {
	t.Parallel()
	for _, testCase := range []struct {
		name, filename, mediaType string
	}{
		{name: "wrong extension", filename: "sample.wav", mediaType: "audio/mpeg"},
		{name: "wrong media type", filename: "sample.mp3", mediaType: "audio/wav"},
		{name: "generic audio media type", filename: "sample.mp3", mediaType: "audio/octet-stream"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			data := syntheticMP3Frames(1)
			policy := inspectionPolicy(data, testCase.filename, testCase.mediaType)
			policy.MaxDurationMS = 1_000
			record, err := media.InspectCapability(bytes.NewReader(data), policy)
			require.NoError(t, err)
			assert.False(t, record.Eligible)
			assert.Equal(t, media.CapabilityReasonMalformed, record.Reason)
		})
	}
}

func syntheticMP3Frames(count int) []byte {
	const frameBytes = 417 // MPEG-1 Layer III, 128 kbps, 44.1 kHz, no padding.
	frames := make([]byte, count*frameBytes)
	for offset := 0; offset < len(frames); offset += frameBytes {
		frames[offset], frames[offset+1], frames[offset+2], frames[offset+3] = 0xff, 0xfb, 0x90, 0
	}
	return frames
}

func syntheticMPEG2Frame() []byte {
	const frameBytes = 261 // MPEG-2 Layer III, 80 kbps, 22.05 kHz, no padding.
	frame := make([]byte, frameBytes)
	frame[0], frame[1], frame[2], frame[3] = 0xff, 0xf3, 0x90, 0
	return frame
}

func syntheticMPEG25Frame() []byte {
	const frameBytes = 522 // MPEG-2.5 Layer III, 80 kbps, 11.025 kHz, no padding.
	frame := make([]byte, frameBytes)
	frame[0], frame[1], frame[2], frame[3] = 0xff, 0xe3, 0x90, 0
	return frame
}

func id3v2Tag(version byte, body []byte) []byte {
	tag := make([]byte, 10, 10+len(body))
	copy(tag, "ID3")
	tag[3] = version
	size := len(body)
	for index := 9; index >= 6; index-- {
		tag[index] = byte(size & 0x7f)
		size >>= 7
	}
	return append(tag, body...)
}

func id3v24Tag(body []byte, footer bool) []byte {
	flags := byte(0)
	footerBytes := 0
	if footer {
		flags = 0x10
		footerBytes = 10
	}
	tag := make([]byte, 10+len(body)+footerBytes)
	copy(tag, "ID3")
	tag[3], tag[5] = 4, flags
	size := len(body)
	for index := 9; index >= 6; index-- {
		tag[index] = byte(size & 0x7f)
		size >>= 7
	}
	copy(tag[10:], body)
	if footer {
		copy(tag[len(tag)-10:], tag[:10])
		copy(tag[len(tag)-10:], "3DI")
	}
	return tag
}
