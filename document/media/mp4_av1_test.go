package media_test

import (
	"bytes"
	"encoding/binary"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document/media"
	"go.kenn.io/docbank/document/media/mediatest"
)

// These literal bit strings exercise AV1 dimension syntax. They deliberately
// make no assertion about entropy decoding; the encoded fixture is the control
// for ordinary encoder output. The sequence has no timing/model/frame IDs,
// no order hints, forced-off screen tools, 8-bit 4:2:0 and one operating point.
func av1Sequence(reduced bool, width, height uint64) []byte {
	bits := "00000" // profile 0, not still, ordinary header
	if reduced {
		bits = "00011"
	}
	if reduced {
		bits += "00000"
	} else {
		bits += "0000000" + "000000000000" + "00000"
	}
	bits += "01110111" // 8 bits each for dimension minus one
	start := len(bits)
	bits += "0000000000000000"
	if !reduced {
		bits += "0"
	} // frame_id_numbers_present_flag
	bits += "000" // superblock, filter intra, intra edge
	if !reduced {
		bits += "00000" + "00"
	} // inter tools, order hint, choose/force screen
	bits += "000"      // superres, cdef, restoration
	bits += "00000000" // high depth, mono, color description/range, chroma position, separate UV, film grain
	bits += "1"        // trailing bit
	payload := videoBitString(bits)
	setVideoBits(payload, start, 8, width-1)
	setVideoBits(payload, start+8, 8, height-1)
	return append([]byte{0x0a, byte(len(payload))}, payload...)
}

func videoBitString(bits string) []byte {
	data := make([]byte, (len(bits)+7)/8)
	for i, b := range bits {
		if b == '1' {
			data[i/8] |= 1 << (7 - i%8)
		}
	}
	return data
}

func av1Frame(bits string) []byte {
	// Two opaque bytes ensure the dimension prefix is not a type-only frame.
	payload := append(videoBitString(bits), 0, 0)
	return append([]byte{0x32, byte(len(payload))}, payload...)
}

func av1WithSequence(t *testing.T, sequence []byte, samples ...[]byte) []byte {
	t.Helper()
	data := mediatest.AV1MP4()
	oldLength := len(data)
	data = replaceMP4Box(t, data, "av1C", func([]byte) []byte { return append([]byte{0x81, 0, 0x0c, 0}, sequence...) })
	delta := len(data) - oldLength
	stsd := bytes.Index(data, []byte("stsd"))
	entry := stsd + bytes.Index(data[stsd:], []byte("av01"))
	for _, kind := range []int{stsd, entry} {
		size := binary.BigEndian.Uint32(data[kind-4 : kind])
		binary.BigEndian.PutUint32(data[kind-4:kind], uint32(int(size)+delta))
	}
	return modernVideoSamples(t, data, samples...)
}

func TestInspectCapabilityAV1EncodedInterFrames(t *testing.T) {
	t.Parallel()
	record := inspectModernVideo(t, mediatest.AV1InterMP4(), 4096)
	assert.True(t, record.Eligible, record.Reason)
	assert.Equal(t, int64(2), record.Measurements.Frames)
}

func TestInspectCapabilityAV1ShowableReferences(t *testing.T) {
	t.Parallel()
	key := av1Frame("00010000")
	// Hidden KEY, showable, resilient, no override, refresh slots 0 and 1.
	hiddenKey := av1Frame("00001100" + "00000011" + "0")
	nonshowableKey := av1Frame("00000100" + "00000011" + "0")
	inter := av1Frame("0011100" + "00000001" + "000000000000000000000" + "0")
	nonshowableInter := av1Frame("00100100" + "00000001" + "000000000000000000000" + "0")
	show0, show1, show7 := []byte{0x1a, 1, 0x88}, []byte{0x1a, 1, 0x98}, []byte{0x1a, 1, 0xf8}
	for _, tc := range []struct {
		name     string
		samples  [][]byte
		eligible bool
	}{
		{"showable hidden key", [][]byte{hiddenKey, show0}, true},
		{"showable hidden key alias", [][]byte{hiddenKey, show1}, true},
		{"shown key forbidden", [][]byte{key, show0}, false},
		{"nonshowable hidden key forbidden", [][]byte{nonshowableKey, show0}, false},
		{"nonshowable hidden inter forbidden", [][]byte{key, nonshowableInter, show0}, false},
		{"repeated key forbidden", [][]byte{hiddenKey, show0, show0}, false},
		{"repeated key alias forbidden", [][]byte{hiddenKey, show0, show1}, false},
		{"repeated key refreshed alias forbidden", [][]byte{hiddenKey, show0, show7}, false},
		{"new hidden key restores showability", [][]byte{hiddenKey, show0, hiddenKey, show1}, true},
		{"shown inter remains showable", [][]byte{key, inter, show0, show0}, true},
		{"show existing in frame OBU forbidden", [][]byte{hiddenKey, {0x32, 1, 0x88}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data := av1WithSequence(t, av1Sequence(false, 64, 64), tc.samples...)
			policy := inspectionPolicy(data, "clip.mp4", "video/mp4")
			policy.MaxPixels, policy.MaxFrames, policy.MaxDurationMS = 4096, int64(len(tc.samples)), 10000
			record, err := media.InspectCapability(bytes.NewReader(data), policy)
			require.NoError(t, err)
			assert.Equal(t, tc.eligible, record.Eligible, record.Reason)
			if tc.eligible {
				assert.Equal(t, int64(len(tc.samples)), record.Measurements.Frames)
				policy.MaxFrames--
				record, err = media.InspectCapability(bytes.NewReader(data), policy)
				require.NoError(t, err)
				assert.False(t, record.Eligible, "show-existing events must consume the picture budget")
			}
		})
	}
}

func TestInspectCapabilityAV1SequenceAndReferenceAuthority(t *testing.T) {
	t.Parallel()
	ordinary := av1Sequence(false, 64, 64)
	reduced := av1Sequence(true, 64, 64)
	key := av1Frame("00010000") // not existing, KEY, shown, cdf enabled, no size override, render same
	// INTER, shown, resilient, disable_cdf=0, override=0, refresh slot 0,
	// seven explicit ref indices (all slot 0), render unchanged.
	inter := av1Frame("0011100" + "00000001" + "000000000000000000000" + "0")
	larger := av1Sequence(false, 128, 64)
	for _, tc := range []struct {
		name     string
		sequence []byte
		samples  [][]byte
		pixels   int64
		eligible bool
	}{
		{"ordinary", ordinary, [][]byte{key}, 4096, true},
		{"reduced still", reduced, [][]byte{av1Frame("0000")}, 4096, true},
		{"explicit references", ordinary, [][]byte{key, inter}, 4096, true},
		{"missing references", ordinary, [][]byte{inter}, 4096, false},
		{"later sequence growth", ordinary, [][]byte{key, append(slices.Clone(larger), key...)}, 8192, true},
		{"later sequence limit", ordinary, [][]byte{key, append(slices.Clone(larger), key...)}, 8191, false},
		{"later sequence height growth", ordinary, [][]byte{key, append(av1Sequence(false, 64, 128), key...)}, 8192, true},
		{"sequence only sample", ordinary, [][]byte{ordinary}, 4096, false},
		{"frame only without sequence", nil, [][]byte{key}, 4096, false},
		{"in-band first sequence", nil, [][]byte{append(slices.Clone(ordinary), key...)}, 4096, true},
		{"duplicate config sequences", append(slices.Clone(ordinary), ordinary...), [][]byte{key}, 4096, false},
		{"oversized configuration", make([]byte, 65*1024), [][]byte{key}, 4096, false},
		{"excessive OBU count", ordinary, [][]byte{append(bytes.Repeat([]byte{0x12, 0}, 1024), key...)}, 4096, false},
		{"truncated sequence", ordinary[:len(ordinary)-1], [][]byte{key}, 4096, false},
		{"reserved OBU", ordinary, [][]byte{append(slices.Clone(key), 0x4a, 1, 0x80)}, 4096, false},
		{"tile list", ordinary, [][]byte{{0x42, 1, 0}}, 4096, false},
		{"truncated OBU length", ordinary, [][]byte{{0x32, 0x80}}, 4096, false},
		{"overlong OBU length", ordinary, [][]byte{{0x32, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0}}, 4096, false},
		{"frame type only", ordinary, [][]byte{{0x32, 0}}, 4096, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data := av1WithSequence(t, tc.sequence, tc.samples...)
			record := inspectModernVideo(t, data, tc.pixels)
			assert.Equal(t, tc.eligible, record.Eligible, record.Reason)
			if tc.eligible {
				assert.Equal(t, tc.pixels, record.Measurements.Pixels)
			}
		})
	}
}

func TestInspectCapabilityAV1MalformedConfigurationAndFrameOverrides(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		mutate func([]byte)
	}{
		{"marker", func(c []byte) { c[0] = 1 }},
		{"version", func(c []byte) { c[0] = 0x82 }},
		{"profile mismatch", func(c []byte) { c[1] |= 0x20 }},
		{"level mismatch", func(c []byte) { c[1] = 1 }},
		{"bit depth mismatch", func(c []byte) { c[2] |= 0x40 }},
		{"chroma mismatch", func(c []byte) { c[2] &^= 4 }},
		{"reserved configuration", func(c []byte) { c[3] = 0xe0 }},
		{"delay reserved", func(c []byte) { c[3] = 1 }},
		{"forbidden OBU bit", func(c []byte) { c[4] |= 0x80 }},
		{"reserved OBU bit", func(c []byte) { c[4] |= 1 }},
		{"missing OBU size", func(c []byte) { c[4] &^= 2 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data := mediatest.AV1MP4()
			tc.mutate(mp4TestBoxPayload(t, data, "av1C"))
			assert.False(t, inspectModernVideo(t, data, 4096).Eligible)
		})
	}
	sequence := av1Sequence(false, 64, 64)
	key := av1Frame("00010000")
	// KEY shown, disable_cdf=0, size_override=1, 8-bit width/height,
	// no render override. 128x64 exceeds the sequence's 64x64 maximum.
	oversized := av1Frame("000101" + "01111111" + "00111111" + "0")
	for _, tc := range []struct {
		name     string
		samples  [][]byte
		eligible bool
	}{
		{"override outside sequence", [][]byte{oversized}, false},
		{"shown key cannot show existing", [][]byte{key, {0x1a, 1, 0x88}}, false},
		{"missing show existing", [][]byte{{0x1a, 1, 0x88}}, false},
		{"sequence reset invalidates references", [][]byte{av1Frame("00001100" + "00000011" + "0"), append(av1Sequence(false, 128, 64), 0x1a, 1, 0x88)}, false},
		{"frame header without tiles", [][]byte{{0x1a, 3, 0x10, 0, 0}}, false},
		{"frame header and tiles unsupported", [][]byte{{0x1a, 3, 0x10, 0, 0, 0x22, 1, 0}}, false},
		{"partial tile group unsupported", [][]byte{{0x1a, 3, 0x10, 0, 0, 0x22, 1, 0x80}}, false},
		{"partial frame cannot authorize inter", [][]byte{{0x1a, 3, 0x10, 0, 0, 0x22, 1, 0x80}, av1Frame("0011100" + "00000001" + "000000000000000000000" + "0")}, false},
		{"frame header and tiles across samples", [][]byte{{0x1a, 3, 0x10, 0, 0}, {0x22, 1, 0}}, false},
		{"tiles after complete frame", [][]byte{append(slices.Clone(key), 0x22, 1, 0)}, false},
		{"orphan tiles", [][]byte{{0x22, 1, 0}}, false},
		{"empty orphan tiles", [][]byte{{0x22, 0}}, false},
		{"nonzero extension layer", [][]byte{{0x36, 8, 3, 0x10, 0, 0}}, false},
		{"final frame without length", [][]byte{{0x30, 0x10, 0, 0}}, true},
		{"padded LEB128 length", [][]byte{{0x32, 0x83, 0, 0x10, 0, 0}}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data := av1WithSequence(t, sequence, tc.samples...)
			assert.Equal(t, tc.eligible, inspectModernVideo(t, data, 8192).Eligible)
		})
	}
}

func TestInspectCapabilityAV1RejectsUnsupportedSequenceSyntax(t *testing.T) {
	t.Parallel()
	key := av1Frame("00010000")
	for _, tc := range []struct {
		name string
		bit  int
	}{
		{"timing and decoder model", 5}, {"multiple operating points", 11}, {"frame IDs", 53},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sequence := av1Sequence(false, 64, 64)
			setVideoBits(sequence[2:], tc.bit, 1, 1)
			assert.False(t, inspectModernVideo(t, av1WithSequence(t, sequence, key), 4096).Eligible)
		})
	}
}
