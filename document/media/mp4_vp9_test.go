package media_test

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document/media"
	"go.kenn.io/docbank/document/media/mediatest"
)

func TestInspectCapabilityModernVideoEncodedPixelBounds(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, entry string
		fixture     func() []byte
		dimension   int64
	}{
		{"VP9", "vp09", mediatest.VP9MP4, 16}, {"AV1", "av01", mediatest.AV1MP4, 64},
	} {
		for _, underdeclare := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/underdeclared=%t", tc.name, underdeclare), func(t *testing.T) {
				data := tc.fixture()
				if underdeclare {
					underdeclareModernVideo(t, data, tc.entry)
				}
				record := inspectModernVideo(t, data, tc.dimension*tc.dimension)
				require.True(t, record.Eligible, record.Reason)
				assert.Equal(t, tc.dimension*tc.dimension, record.Measurements.Pixels)
				assert.Equal(t, int64(1), record.Measurements.Frames)
				record = inspectModernVideo(t, data, tc.dimension*tc.dimension-1)
				assert.False(t, record.Eligible)
				assert.Equal(t, media.CapabilityReasonVisualBounds, record.Reason)
			})
		}
	}
}

func TestInspectCapabilityVP9RejectsWrongLeadingMarkers(t *testing.T) {
	t.Parallel()
	for _, first := range []byte{0x42, 0xc2, 0x02} {
		t.Run(fmt.Sprintf("%02x", first), func(t *testing.T) {
			data := mediatest.VP9MP4()
			data[firstMP4SampleOffset(t, data)] = first
			assert.False(t, inspectModernVideo(t, data, 256).Eligible)
		})
	}
}

func inspectModernVideo(t *testing.T, data []byte, pixels int64) media.CapabilityRecord {
	t.Helper()
	policy := inspectionPolicy(data, "clip.mp4", "video/mp4")
	policy.MaxPixels = pixels
	policy.MaxFrames = 100
	policy.MaxDurationMS = 10_000
	record, err := media.InspectCapability(bytes.NewReader(data), policy)
	require.NoError(t, err)
	return record
}

func underdeclareModernVideo(t *testing.T, data []byte, kind string) {
	t.Helper()
	stsd := mp4TestBoxPayload(t, data, "stsd")
	entry := bytes.Index(stsd, []byte(kind))
	require.Positive(t, entry)
	binary.BigEndian.PutUint16(stsd[entry+28:entry+30], 1)
	binary.BigEndian.PutUint16(stsd[entry+30:entry+32], 1)
	tkhd := mp4TestBoxPayload(t, data, "tkhd")
	binary.BigEndian.PutUint32(tkhd[len(tkhd)-8:], 1<<16)
	binary.BigEndian.PutUint32(tkhd[len(tkhd)-4:], 1<<16)
}

func TestInspectCapabilityVP9StateAndSuperframes(t *testing.T) {
	t.Parallel()
	fixture := mediatest.VP9MP4()
	key := slices.Clone(fixture[firstMP4SampleOffset(t, fixture):])
	larger := slices.Clone(key)
	// Profile-0 key frame: sizes start at bit 36, each a 16-bit minus-one.
	setVideoBits(larger, 36, 16, 31)
	setVideoBits(larger, 52, 16, 31)
	for _, tc := range []struct {
		name     string
		samples  [][]byte
		pixels   int64
		eligible bool
	}{
		{"later key growth", [][]byte{key, larger}, 1024, true},
		{"later key limit", [][]byte{key, larger}, 1023, false},
		{"show existing", [][]byte{key, {0x88}}, 256, true},
		{"missing existing", [][]byte{{0x88}}, 256, false},
		{"superframe growth", [][]byte{vp9Superframe(key, larger)}, 1024, true},
		{"superframe limit", [][]byte{vp9Superframe(key, larger)}, 1023, false},
		{"superframe missing reference", [][]byte{vp9Superframe([]byte{0x88}, key)}, 256, false},
		{"truncated header", [][]byte{key[:9]}, 256, false},
		{"wrong sync", [][]byte{append([]byte{0x82, 0, 0, 0}, key[4:]...)}, 256, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data := modernVideoSamples(t, fixture, tc.samples...)
			record := inspectModernVideo(t, data, tc.pixels)
			assert.Equal(t, tc.eligible, record.Eligible, record.Reason)
			if tc.eligible {
				assert.Equal(t, tc.pixels, record.Measurements.Pixels)
			}
		})
	}
}

func TestInspectCapabilityVP9ConfigurationAndEveryFrame(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		mutate func([]byte)
	}{
		{"version", func(c []byte) { c[0] = 0 }},
		{"flags", func(c []byte) { c[3] = 1 }},
		{"profile", func(c []byte) { c[4] = 1 }},
		{"level", func(c []byte) { c[5] = 255 }},
		{"bit depth", func(c []byte) { c[6] = 0xa2 }},
		{"chroma", func(c []byte) { c[6] = 0x86 }},
		{"color range", func(c []byte) { c[6] |= 1 }},
		{"reserved primaries", func(c []byte) { c[7] = 255 }},
		{"reserved transfer", func(c []byte) { c[8] = 255 }},
		{"reserved matrix", func(c []byte) { c[9] = 255 }},
		{"initialization payload", func(c []byte) { c[11] = 1 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data := mediatest.VP9MP4()
			tc.mutate(mp4TestBoxPayload(t, data, "vpcC"))
			assert.False(t, inspectModernVideo(t, data, 256).Eligible)
		})
	}
	fixture := mediatest.VP9MP4()
	key := slices.Clone(fixture[firstMP4SampleOffset(t, fixture):])
	for _, tc := range []struct {
		name   string
		sample []byte
	}{
		{"second frame bad marker", vp9Superframe(key, append([]byte{0x42}, key[1:]...))},
		{"superframe sum", append(slices.Clone(key), 0xc0, 1, 0xc0)},
		{"missing superframe index", append(slices.Clone(key), 0xdf)},
		{"header only", key[:1]},
		{"show existing trailing bytes", []byte{0x88, 0}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data := modernVideoSamples(t, fixture, tc.sample)
			assert.False(t, inspectModernVideo(t, data, 256).Eligible)
		})
	}
}

func vp9Superframe(frames ...[]byte) []byte {
	var data []byte
	for _, frame := range frames {
		data = append(data, frame...)
	}
	marker := byte(0xc0 | len(frames) - 1)
	data = append(data, marker)
	for _, frame := range frames {
		data = append(data, byte(len(frame)))
	}
	return append(data, marker)
}

func TestInspectCapabilityVP9InterAndRefreshReferences(t *testing.T) {
	t.Parallel()
	fixture := mediatest.VP9MP4()
	key := slices.Clone(fixture[firstMP4SampleOffset(t, fixture):])
	// Profile 0, intra-only error-resilient frame refreshing only slot 0.
	intra := vp9StructuralFrame("10000101" + "1" + "010010011000001101000010" + "00000001" + "00000000000011110000000000001111" + "0")
	// Profile 0 inter frame: refresh slot 0, references all slot 0, inherit
	// the first reference's dimensions, same render size, switchable filter.
	inter := vp9StructuralFrame("10000111" + "00000001" + "000000000000" + "1" + "0" + "0" + "1")
	invalid := vp9StructuralFrame("10000111" + "00000001" + "001000000000" + "1" + "0" + "0" + "1")
	for _, tc := range []struct {
		name     string
		samples  [][]byte
		eligible bool
	}{
		{"key then inter", [][]byte{key, inter}, true},
		{"intra then inter", [][]byte{intra, inter}, true},
		{"inter without state", [][]byte{inter}, false},
		{"unrefreshed reference", [][]byte{intra, invalid}, false},
		{"unrefreshed show existing", [][]byte{intra, {0x89}}, false},
		{"refreshed show existing", [][]byte{intra, {0x88}}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			record := inspectModernVideo(t, modernVideoSamples(t, fixture, tc.samples...), 256)
			assert.Equal(t, tc.eligible, record.Eligible, record.Reason)
			if tc.eligible {
				assert.Equal(t, int64(len(tc.samples)), record.Measurements.Frames)
			}
		})
	}
}

func vp9StructuralFrame(prefix string) []byte {
	// Frame context index, zero loop filter/deltas/quantizer/segmentation,
	// single tile row, one-byte compressed header and one opaque tile byte.
	bits := prefix + "00" + "0000000000" + "00000000" + "000" + "0" + "0" + "0000000000000001"
	return append(videoBitString(bits), 0, 0)
}

func TestInspectCapabilityVP9ProfilesAndColorConsistency(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, profileBits, colorBits string
		profile, packed, matrix      byte
	}{
		{"profile 0", "00", "0000", 0, 0x82, 2},
		{"profile 1 RGB", "10", "1110", 1, 0x87, 0},
		{"profile 2 ten bit", "01", "00000", 2, 0xa2, 2},
		{"profile 2 twelve bit", "01", "10000", 2, 0xc2, 2},
		{"profile 3 RGB", "110", "01110", 3, 0xa7, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			prefix := "10" + tc.profileBits + "0011" + "010010011000001101000010" + tc.colorBits + "00000000000011110000000000001111" + "0"
			data := modernVideoSamples(t, mediatest.VP9MP4(), vp9StructuralFrame(prefix))
			config := mp4TestBoxPayload(t, data, "vpcC")
			config[4], config[6], config[9] = tc.profile, tc.packed, tc.matrix
			assert.True(t, inspectModernVideo(t, data, 256).Eligible)
			config[6] ^= 1
			assert.False(t, inspectModernVideo(t, data, 256).Eligible)
		})
	}
}

func setVideoBits(data []byte, start, count int, value uint64) {
	for i := range count {
		bit := start + i
		mask := byte(1 << (7 - bit%8))
		data[bit/8] &^= mask
		if value>>(count-1-i)&1 != 0 {
			data[bit/8] |= mask
		}
	}
}

func modernVideoSamples(t *testing.T, fixture []byte, samples ...[]byte) []byte {
	t.Helper()
	data := slices.Clone(fixture)
	binary.BigEndian.PutUint32(mp4TestBoxPayload(t, data, "stts")[8:12], uint32(len(samples)))
	binary.BigEndian.PutUint32(mp4TestBoxPayload(t, data, "stsc")[12:16], uint32(len(samples)))
	data = replaceMP4Box(t, data, "stsz", func([]byte) []byte {
		payload := make([]byte, 12)
		binary.BigEndian.PutUint32(payload[8:12], uint32(len(samples)))
		for _, sample := range samples {
			payload = binary.BigEndian.AppendUint32(payload, uint32(len(sample)))
		}
		return payload
	})
	start := bytes.Index(data, []byte("mdat")) + 4
	require.Greater(t, start, 4)
	data = data[:start]
	for _, sample := range samples {
		data = append(data, sample...)
	}
	binary.BigEndian.PutUint32(data[start-8:start-4], uint32(len(data)-start+8))
	binary.BigEndian.PutUint32(mp4TestBoxPayload(t, data, "stco")[8:12], uint32(start))
	return data
}
