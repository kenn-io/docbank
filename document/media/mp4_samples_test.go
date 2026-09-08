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

func TestInspectCapabilityAcceptsMappedQuickTimeH264(t *testing.T) {
	t.Parallel()
	data := mediatest.H264MOV()
	policy := inspectionPolicy(data, "clip.mov", "video/quicktime")
	policy.MaxPixels = 16 * 16
	policy.MaxFrames = 1
	policy.MaxDurationMS = 1_000

	record, err := media.InspectCapability(bytes.NewReader(data), policy)
	require.NoError(t, err)
	require.True(t, record.Eligible, record.Reason)
	assert.Equal(t, int64(1), record.Measurements.Frames)
}

func TestInspectCapabilityRejectsVideoWithoutSampleToChunkAuthority(t *testing.T) {
	t.Parallel()
	data := decodableAVCMP4(t)
	stsc := bytes.Index(data, []byte("stsc"))
	require.NotEqual(t, -1, stsc)
	copy(data[stsc:stsc+4], "free")
	policy := inspectionPolicy(data, "clip.mp4", "video/mp4")
	policy.MaxPixels = 16 * 16
	policy.MaxFrames = 2
	policy.MaxDurationMS = 1_000

	record, err := media.InspectCapability(bytes.NewReader(data), policy)
	require.NoError(t, err)
	assert.False(t, record.Eligible)
	assert.Equal(t, media.CapabilityReasonMalformed, record.Reason)
}

func TestInspectCapabilityRequiresCompleteMappedVideoAuthority(t *testing.T) {
	t.Parallel()
	for _, testCase := range []struct {
		name   string
		mutate func(*testing.T, []byte) []byte
	}{
		{name: "missing chunk offsets", mutate: renameMP4Box("stco", "free")},
		{name: "missing media data", mutate: renameMP4Box("mdat", "free")},
		{name: "chunk starts outside media data", mutate: func(t *testing.T, data []byte) []byte {
			t.Helper()
			stco := mp4TestBoxPayload(t, data, "stco")
			binary.BigEndian.PutUint32(stco[8:12], uint32(len(data)+1))
			return data
		}},
		{name: "sample ends outside media data", mutate: func(t *testing.T, data []byte) []byte {
			t.Helper()
			stsz := mp4TestBoxPayload(t, data, "stsz")
			binary.BigEndian.PutUint32(stsz[4:8], 6)
			return data
		}},
		{name: "zero sample length", mutate: func(t *testing.T, data []byte) []byte {
			t.Helper()
			data = replaceMP4Box(t, data, "stsz", func(payload []byte) []byte {
				replacement := slices.Clone(payload)
				binary.BigEndian.PutUint32(replacement[4:8], 0)
				return binary.BigEndian.AppendUint32(replacement, 0)
			})
			requireFirstChunkStartsAtMDAT(t, data)
			return data
		}},
		{name: "invalid description index", mutate: func(t *testing.T, data []byte) []byte {
			t.Helper()
			stsc := mp4TestBoxPayload(t, data, "stsc")
			binary.BigEndian.PutUint32(stsc[16:20], 2)
			return data
		}},
		{name: "not every declared sample is mapped", mutate: func(t *testing.T, _ []byte) []byte {
			t.Helper()
			data := mappedAVCMP4([]int{2}, "stsz", false)
			stsc := mp4TestBoxPayload(t, data, "stsc")
			binary.BigEndian.PutUint32(stsc[12:16], 1)
			return data
		}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			data := testCase.mutate(t, mappedAVCMP4([]int{1}, "stsz", false))
			assertVideoCapability(t, data, "clip.mp4", "video/mp4", 1, false)
		})
	}
}

func TestInspectCapabilityRejectsOverlappingVideoSamples(t *testing.T) {
	t.Parallel()
	data := mappedAVCMP4([]int{1, 1}, "stsz", false)
	first, second := nthMP4ChunkOffsets(t, data, 0), nthMP4ChunkOffsets(t, data, 1)
	copy(second[8:12], first[8:12])
	assertVideoCapability(t, data, "clip.mp4", "video/mp4", 2, false)
}

func TestInspectCapabilityRejectsUnusedSampleToChunkRun(t *testing.T) {
	t.Parallel()
	data := mappedAVCMP4([]int{1}, "stsz", false)
	data = replaceMP4Box(t, data, "stsc", func(payload []byte) []byte {
		replacement := slices.Clone(payload)
		binary.BigEndian.PutUint32(replacement[4:8], 2)
		replacement = binary.BigEndian.AppendUint32(replacement, 2)
		replacement = binary.BigEndian.AppendUint32(replacement, 1)
		return binary.BigEndian.AppendUint32(replacement, 1)
	})
	requireFirstChunkStartsAtMDAT(t, data)
	assertVideoCapability(t, data, "clip.mp4", "video/mp4", 1, false)
}

func TestInspectCapabilityRejectsDuplicateAndMisplacedMappingTables(t *testing.T) {
	t.Parallel()
	t.Run("duplicate stsc", func(t *testing.T) {
		data := mappedAVCMP4([]int{1}, "stsz", false)
		payload := slices.Clone(mp4TestBoxPayload(t, data, "stsc"))
		data = insertIntoMP4Box(t, data, "stbl", mediatest.Box("stsc", payload))
		assertVideoCapability(t, data, "clip.mp4", "video/mp4", 1, false)
	})
	t.Run("duplicate offset table", func(t *testing.T) {
		data := mappedAVCMP4([]int{1}, "stsz", false)
		payload := slices.Clone(mp4TestBoxPayload(t, data, "stco"))
		data = insertIntoMP4Box(t, data, "stbl", mediatest.Box("stco", payload))
		assertVideoCapability(t, data, "clip.mp4", "video/mp4", 1, false)
	})
	t.Run("misplaced stsc", func(t *testing.T) {
		data := append(mappedAVCMP4([]int{1}, "stsz", false), mediatest.Box("stsc", make([]byte, 8))...)
		assertVideoCapability(t, data, "clip.mp4", "video/mp4", 1, false)
	})
}

func TestInspectCapabilityAcceptsMappedSampleSizeEncodings(t *testing.T) {
	t.Parallel()
	for _, encoding := range []string{"stsz", "stsz-explicit", "stz2-4", "stz2-8", "stz2-16"} {
		t.Run(encoding, func(t *testing.T) {
			t.Parallel()
			assertVideoCapability(t, mappedAVCMP4([]int{2}, encoding, false), "clip.mp4", "video/mp4", 2, true)
		})
	}
	t.Run("co64", func(t *testing.T) {
		t.Parallel()
		assertVideoCapability(t, mappedAVCMP4([]int{2}, "stsz", true), "clip.mp4", "video/mp4", 2, true)
	})
}

func TestInspectCapabilityBoundsMappedSamplesAcrossVisualTracks(t *testing.T) {
	t.Parallel()
	for _, testCase := range []struct {
		name       string
		counts     []int
		wantFrames int64
		eligible   bool
	}{
		{name: "thirty three source frames are valid", counts: []int{33}, wantFrames: 33, eligible: true},
		{name: "file wide bound equality", counts: []int{5_000, 5_000}, wantFrames: 10_000, eligible: true},
		{name: "file wide bound overflow", counts: []int{5_000, 5_001}, wantFrames: 10_001, eligible: false},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			data := mappedAVCMP4(testCase.counts, "stsz", false)
			metadata, err := media.DetectBytes(data, "video/mp4")
			require.NoError(t, err, "metadata-only detection remains bounded and distinct")
			assert.Equal(t, int64(16), metadata.Width)
			assertVideoCapability(t, data, "clip.mp4", "video/mp4", testCase.wantFrames, testCase.eligible)
		})
	}
}

func TestInspectCapabilityDoesNotExpandOversizedPackedSampleTable(t *testing.T) {
	t.Parallel()
	data := mappedAVCMP4([]int{10_001}, "stz2-4", false)
	_, err := media.DetectBytes(data, "video/mp4")
	require.NoError(t, err)
	assertVideoCapability(t, data, "clip.mp4", "video/mp4", 10_001, false)
}

func TestInspectCapabilityValidatesEveryMappedLegacyCodecSample(t *testing.T) {
	t.Parallel()
	for _, testCase := range []struct {
		name      string
		data      func() []byte
		mediaType string
		filename  string
		mutate    func([]byte, int)
	}{
		{name: "H264 zero NAL length", data: mediatest.H264MOV, filename: "clip.mov", mediaType: "video/quicktime", mutate: func(data []byte, sample int) { clear(data[sample : sample+4]) }},
		{name: "H264 truncated NAL", data: mediatest.H264MOV, filename: "clip.mov", mediaType: "video/quicktime", mutate: func(data []byte, sample int) { binary.BigEndian.PutUint32(data[sample:sample+4], uint32(len(data))) }},
		{name: "H264 forbidden header", data: mediatest.H264MOV, filename: "clip.mov", mediaType: "video/quicktime", mutate: func(data []byte, sample int) { data[sample+4] |= 0x80 }},
		{name: "H264 no picture", data: mediatest.H264MOV, filename: "clip.mov", mediaType: "video/quicktime", mutate: func(data []byte, sample int) { data[sample+4] = data[sample+4]&0xe0 | 6 }},
		{name: "H264 in-band SPS", data: mediatest.H264MOV, filename: "clip.mov", mediaType: "video/quicktime", mutate: func(data []byte, sample int) { data[sample+4] = data[sample+4]&0xe0 | 7 }},
		{name: "H265 zero NAL length", data: mediatest.H265MP4, filename: "clip.mp4", mediaType: "video/mp4", mutate: func(data []byte, sample int) { clear(data[sample : sample+4]) }},
		{name: "H265 forbidden header", data: mediatest.H265MP4, filename: "clip.mp4", mediaType: "video/mp4", mutate: func(data []byte, sample int) { data[sample+4] |= 0x80 }},
		{name: "H265 invalid temporal id", data: mediatest.H265MP4, filename: "clip.mp4", mediaType: "video/mp4", mutate: func(data []byte, sample int) { data[sample+5] &^= 0x07 }},
		{name: "H265 no picture", data: mediatest.H265MP4, filename: "clip.mp4", mediaType: "video/mp4", mutate: func(data []byte, sample int) { data[sample+4] = 40 << 1 }},
		{name: "H265 in-band SPS", data: mediatest.H265MP4, filename: "clip.mp4", mediaType: "video/mp4", mutate: func(data []byte, sample int) { data[sample+4] = 33 << 1 }},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			data := testCase.data()
			sample := firstMP4SampleOffset(t, data)
			testCase.mutate(data, sample)
			assertVideoCapability(t, data, testCase.filename, testCase.mediaType, 1, false)
		})
	}
}

func TestInspectCapabilityRejectsTruncatedLegacyPictureSyntax(t *testing.T) {
	t.Parallel()
	for _, testCase := range []struct {
		name      string
		data      func(*testing.T, []byte) []byte
		sample    []byte
		filename  string
		mediaType string
	}{
		{
			name: "H264 picture header only", data: mappedAVCMP4WithSample,
			sample: mediatest.LengthPrefixedNAL(0x65), filename: "clip.mp4", mediaType: "video/mp4",
		},
		{
			name: "H264 truncated slice header", data: mappedAVCMP4WithSample,
			sample: mediatest.LengthPrefixedNAL(0x65, 0x80), filename: "clip.mp4", mediaType: "video/mp4",
		},
		{
			name: "H265 picture header only", data: replaceH265Sample,
			sample: mediatest.LengthPrefixedNAL(0x28, 0x01), filename: "clip.mp4", mediaType: "video/mp4",
		},
		{
			name: "H265 truncated slice header", data: replaceH265Sample,
			sample: mediatest.LengthPrefixedNAL(0x28, 0x01, 0x80), filename: "clip.mp4", mediaType: "video/mp4",
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			assertVideoCapability(t, testCase.data(t, testCase.sample), testCase.filename, testCase.mediaType, 1, false)
		})
	}
}

func TestInspectCapabilityAcceptsMappedLegacyCodecs(t *testing.T) {
	t.Parallel()
	assertVideoCapability(t, mediatest.H264MOV(), "clip.mov", "video/quicktime", 1, true)
	assertVideoCapability(t, mediatest.H265MP4(), "clip.mp4", "video/mp4", 1, true)
}

func TestInspectCapabilityRejectsHeaderOnlyVideo(t *testing.T) {
	t.Parallel()
	assertVideoCapability(t, mediatest.MP4(16, 16, 1_000), "clip.mp4", "video/mp4", 1, false)
}

func TestInspectCapabilityModernVideoCountsEveryPicture(t *testing.T) {
	t.Parallel()
	vp9 := mediatest.VP9MP4()
	vp9Key := slices.Clone(vp9[firstMP4SampleOffset(t, vp9):])
	av1 := mediatest.AV1MP4()
	av1Sample := slices.Clone(av1[firstMP4SampleOffset(t, av1):])
	// Skip the 13-byte sequence OBU to repeat the encoded frame OBU only.
	av1Frame := av1Sample[13:]
	for _, codec := range []struct {
		name    string
		fixture []byte
		sample  []byte
		pixels  int64
	}{
		{"VP9", vp9, vp9Superframe(vp9Key, vp9Key, vp9Key, vp9Key, vp9Key, vp9Key, vp9Key, vp9Key), 256},
		{"AV1", av1, bytes.Repeat(av1Frame, 8), 4096},
	} {
		for _, tc := range []struct {
			name     string
			samples  int
			limit    int64
			eligible bool
		}{
			{"all pictures", 1, 8, true}, {"limit minus one", 1, 7, false},
			{"global picture ceiling", 1250, 10000, true}, {"global picture ceiling exceeded", 1251, 10008, false},
		} {
			t.Run(codec.name+"/"+tc.name, func(t *testing.T) {
				samples := make([][]byte, tc.samples)
				for i := range samples {
					samples[i] = codec.sample
				}
				data := modernVideoSamples(t, codec.fixture, samples...)
				policy := inspectionPolicy(data, "clip.mp4", "video/mp4")
				policy.MaxPixels, policy.MaxFrames, policy.MaxDurationMS = codec.pixels, tc.limit, 2_000_000
				record, err := media.InspectCapability(bytes.NewReader(data), policy)
				require.NoError(t, err)
				assert.Equal(t, tc.eligible, record.Eligible, record.Reason)
				if tc.eligible {
					assert.Equal(t, int64(tc.samples*8), record.Measurements.Frames)
				}
			})
		}
	}
}

func TestInspectCapabilityModernVideoDescriptionStateIsIsolated(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		fixture func() []byte
		show    []byte
		pixels  int64
	}{
		{"VP9", mediatest.VP9MP4, []byte{0x88}, 256},
		{"AV1", func() []byte {
			return av1WithSequence(t, av1Sequence(false, 64, 64), av1Frame("00001100"+"00000011"+"0"))
		}, []byte{0x1a, 1, 0x88}, 4096},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := tc.fixture()
			key := slices.Clone(fixture[firstMP4SampleOffset(t, fixture):])
			for _, second := range [][]byte{key, tc.show} {
				data := modernVideoSamples(t, fixture, key, second)
				assert.True(t, inspectModernVideo(t, data, tc.pixels).Eligible, "same-description control must be valid")
				data = replaceMP4Box(t, data, "stsd", func(p []byte) []byte {
					result := append(slices.Clone(p), p[8:]...)
					binary.BigEndian.PutUint32(result[4:8], 2)
					return result
				})
				data = replaceMP4Box(t, data, "stsc", func([]byte) []byte {
					p := make([]byte, 8)
					binary.BigEndian.PutUint32(p[4:8], 2)
					for _, v := range []uint32{1, 1, 1, 2, 1, 2} {
						p = binary.BigEndian.AppendUint32(p, v)
					}
					return p
				})
				data = replaceMP4Box(t, data, "stco", func([]byte) []byte { p := make([]byte, 16); binary.BigEndian.PutUint32(p[4:8], 2); return p })
				start := bytes.Index(data, []byte("mdat")) + 4
				stco := mp4TestBoxPayload(t, data, "stco")
				binary.BigEndian.PutUint32(stco[8:12], uint32(start))
				binary.BigEndian.PutUint32(stco[12:16], uint32(start+len(key)))
				assert.Equal(t, bytes.Equal(second, key), inspectModernVideo(t, data, tc.pixels).Eligible)
			}
		})
	}
}

func TestInspectCapabilityModernVideoPictureBudgetSpansTracks(t *testing.T) {
	t.Parallel()
	for _, fixture := range []func() []byte{mediatest.VP9MP4, mediatest.AV1MP4} {
		original := fixture()
		key := slices.Clone(original[firstMP4SampleOffset(t, original):])
		superframe := bytes.Repeat(key[13:], 8)
		pixels := int64(4096)
		if bytes.Contains(original, []byte("vp09")) {
			superframe = vp9Superframe(key, key, key, key, key, key, key, key)
			pixels = 256
		}
		for _, count := range []int{625, 626} {
			samples := make([][]byte, count)
			for i := range samples {
				samples[i] = superframe
			}
			video := modernVideoSamples(t, original, samples...)
			data := appendModernVideoTrack(t, video, video)
			policy := inspectionPolicy(data, "clip.mp4", "video/mp4")
			policy.MaxPixels, policy.MaxFrames, policy.MaxDurationMS = pixels, 10016, 2_000_000
			record, err := media.InspectCapability(bytes.NewReader(data), policy)
			require.NoError(t, err)
			assert.Equal(t, count == 625, record.Eligible, record.Reason)
			if record.Eligible {
				assert.Equal(t, int64(10000), record.Measurements.Frames)
			}
		}
	}
}

func appendModernVideoTrack(t *testing.T, first, second []byte) []byte {
	t.Helper()
	second = slices.Clone(second)
	binary.BigEndian.PutUint32(mp4TestBoxPayload(t, second, "tkhd")[12:16], 2)
	moov := bytes.Index(first, []byte("moov")) - 4
	end := moov + int(binary.BigEndian.Uint32(first[moov:moov+4]))
	trackStart := bytes.Index(second, []byte("trak")) - 4
	trackEnd := trackStart + int(binary.BigEndian.Uint32(second[trackStart:trackStart+4]))
	track := second[trackStart:trackEnd]
	mdatStart := bytes.Index(second, []byte("mdat")) - 4
	result := slices.Concat(first[:end], track, first[end:], second[mdatStart:])
	binary.BigEndian.PutUint32(result[moov:moov+4], uint32(end-moov+len(track)))
	firstOffset := firstMP4SampleOffset(t, first) + len(track)
	secondOffset := len(first) + len(track) + 8
	binary.BigEndian.PutUint32(nthMP4ChunkOffsets(t, result, 0)[8:12], uint32(firstOffset))
	binary.BigEndian.PutUint32(nthMP4ChunkOffsets(t, result, 1)[8:12], uint32(secondOffset))
	return result
}

func TestInspectCapabilityRejectsContainerIdentityMismatch(t *testing.T) {
	t.Parallel()
	assertVideoCapability(t, mediatest.H264MOV(), "clip.mp4", "video/mp4", 1, false)
	assertVideoCapability(t, mediatest.H265MP4(), "clip.mov", "video/quicktime", 1, false)
}

func assertVideoCapability(t *testing.T, data []byte, filename, mediaType string, frames int64, eligible bool) {
	t.Helper()
	policy := inspectionPolicy(data, filename, mediaType)
	policy.MaxPixels = 16 * 16
	policy.MaxFrames = frames
	policy.MaxDurationMS = max(1_000, frames)
	record, err := media.InspectCapability(bytes.NewReader(data), policy)
	require.NoError(t, err)
	assert.Equal(t, eligible, record.Eligible, record.Reason)
	if eligible {
		assert.Equal(t, frames, record.Measurements.Frames)
	} else {
		assert.Equal(t, media.CapabilityReasonMalformed, record.Reason)
	}
}

func renameMP4Box(from, to string) func(*testing.T, []byte) []byte {
	return func(t *testing.T, data []byte) []byte {
		t.Helper()
		index := bytes.Index(data, []byte(from))
		require.NotEqual(t, -1, index)
		copy(data[index:index+4], to)
		return data
	}
}

func firstMP4SampleOffset(t *testing.T, data []byte) int {
	t.Helper()
	payload := mp4TestBoxPayload(t, data, "stco")
	offset := uint64(binary.BigEndian.Uint32(payload[8:12]))
	require.Less(t, offset, uint64(len(data)))
	return int(offset)
}

func requireFirstChunkStartsAtMDAT(t *testing.T, data []byte) {
	t.Helper()
	mdat := bytes.Index(data, []byte("mdat"))
	require.NotEqual(t, -1, mdat)
	stco := mp4TestBoxPayload(t, data, "stco")
	assert.Equal(t, uint32(mdat+4), binary.BigEndian.Uint32(stco[8:12]))
}

func nthMP4ChunkOffsets(t *testing.T, data []byte, occurrence int) []byte {
	t.Helper()
	const kind = "stco"
	searchStart := 0
	base := -1
	for range occurrence + 1 {
		relative := bytes.Index(data[searchStart:], []byte(kind))
		require.NotEqual(t, -1, relative)
		base = searchStart + relative
		searchStart = base + len(kind)
	}
	size := int(binary.BigEndian.Uint32(data[base-4 : base]))
	require.GreaterOrEqual(t, size, 8)
	require.LessOrEqual(t, base-4+size, len(data))
	return data[base+4 : base-4+size]
}

func mappedAVCMP4(sampleCounts []int, sizeEncoding string, co64 bool) []byte {
	return mappedAVCMP4WithSamples(sampleCounts, sizeEncoding, co64, mediatest.H264PictureSample())
}

func mappedAVCMP4WithSample(t *testing.T, sample []byte) []byte {
	t.Helper()
	return mappedAVCMP4WithSamples([]int{1}, "stsz", false, sample)
}

func mappedAVCMP4WithSamples(sampleCounts []int, sizeEncoding string, co64 bool, sample []byte) []byte {
	maximum := 0
	tracks := make([][]byte, 0, len(sampleCounts))
	for _, count := range sampleCounts {
		maximum = max(maximum, count)
		tracks = append(tracks, mappedAVCTrack(count, len(sample), sizeEncoding, co64))
	}
	ftyp := mediatest.Box("ftyp", append([]byte("isom"), make([]byte, 12)...))
	mvhd := make([]byte, 20)
	binary.BigEndian.PutUint32(mvhd[12:16], 1_000)
	binary.BigEndian.PutUint32(mvhd[16:20], uint32(maximum))
	moov := mediatest.Box("moov", slices.Concat(append([][]byte{mediatest.Box("mvhd", mvhd)}, tracks...)...))
	samples := make([]byte, 0, maximum*5)
	offsets := make([]uint64, len(sampleCounts))
	for index, count := range sampleCounts {
		offsets[index] = uint64(len(ftyp) + len(moov) + 8 + len(samples))
		for range count {
			samples = append(samples, sample...)
		}
	}
	data := slices.Concat(ftyp, moov, mediatest.Box("mdat", samples))
	for index, offset := range offsets {
		kind := "stco"
		if co64 {
			kind = "co64"
		}
		payload := nthMP4BoxPayloadForBuild(data, kind, index)
		if co64 {
			binary.BigEndian.PutUint64(payload[8:16], offset)
		} else {
			binary.BigEndian.PutUint32(payload[8:12], uint32(offset))
		}
	}
	return data
}

func mappedAVCTrack(count, sampleSize int, sizeEncoding string, co64 bool) []byte {
	tkhd := make([]byte, 84)
	binary.BigEndian.PutUint32(tkhd[20:24], uint32(count))
	binary.BigEndian.PutUint32(tkhd[76:80], 16<<16)
	binary.BigEndian.PutUint32(tkhd[80:84], 16<<16)
	entry := make([]byte, 78)
	binary.BigEndian.PutUint16(entry[24:26], 16)
	binary.BigEndian.PutUint16(entry[26:28], 16)
	entry = slices.Concat(entry, mediatest.Box("avcC", mediatest.AVCConfig(16, 16)))
	stsd := make([]byte, 0, 8+8+len(entry))
	stsd = append(stsd, make([]byte, 8)...)
	binary.BigEndian.PutUint32(stsd[4:8], 1)
	stsd = append(stsd, mediatest.Box("avc1", entry)...)
	stts := make([]byte, 16)
	binary.BigEndian.PutUint32(stts[4:8], 1)
	binary.BigEndian.PutUint32(stts[8:12], uint32(count))
	binary.BigEndian.PutUint32(stts[12:16], 1)
	stsc := make([]byte, 20)
	binary.BigEndian.PutUint32(stsc[4:8], 1)
	binary.BigEndian.PutUint32(stsc[8:12], 1)
	binary.BigEndian.PutUint32(stsc[12:16], uint32(count))
	binary.BigEndian.PutUint32(stsc[16:20], 1)
	sizeBox, sizeKind := sampleSizeBox(count, sampleSize, sizeEncoding)
	offsets := make([]byte, 12)
	binary.BigEndian.PutUint32(offsets[4:8], 1)
	offsetKind := "stco"
	if co64 {
		offsetKind = "co64"
		offsets = make([]byte, 16)
		binary.BigEndian.PutUint32(offsets[4:8], 1)
	}
	stbl := mediatest.Box("stbl", slices.Concat(
		mediatest.Box("stsd", stsd), mediatest.Box("stts", stts), mediatest.Box("stsc", stsc),
		mediatest.Box(sizeKind, sizeBox), mediatest.Box(offsetKind, offsets),
	))
	handler := make([]byte, 12)
	copy(handler[8:12], "vide")
	mdhd := make([]byte, 24)
	binary.BigEndian.PutUint32(mdhd[12:16], 1_000)
	binary.BigEndian.PutUint32(mdhd[16:20], uint32(count))
	mdia := mediatest.Box("mdia", slices.Concat(mediatest.Box("mdhd", mdhd), mediatest.Box("hdlr", handler), mediatest.Box("minf", stbl)))
	return mediatest.Box("trak", append(mediatest.Box("tkhd", tkhd), mdia...))
}

func sampleSizeBox(count, sampleSize int, encoding string) ([]byte, string) {
	switch encoding {
	case "stsz":
		payload := make([]byte, 12)
		binary.BigEndian.PutUint32(payload[4:8], uint32(sampleSize))
		binary.BigEndian.PutUint32(payload[8:12], uint32(count))
		return payload, "stsz"
	case "stsz-explicit":
		payload := make([]byte, 12+count*4)
		binary.BigEndian.PutUint32(payload[8:12], uint32(count))
		for index := range count {
			binary.BigEndian.PutUint32(payload[12+index*4:16+index*4], uint32(sampleSize))
		}
		return payload, "stsz"
	case "stz2-4":
		payload := make([]byte, 12+(count+1)/2)
		payload[7] = 4
		binary.BigEndian.PutUint32(payload[8:12], uint32(count))
		for index := range count {
			payload[12+index/2] |= byte(sampleSize << (4 * (1 - index%2)))
		}
		return payload, "stz2"
	case "stz2-8":
		payload := make([]byte, 12+count)
		payload[7] = 8
		binary.BigEndian.PutUint32(payload[8:12], uint32(count))
		for index := range count {
			payload[12+index] = byte(sampleSize)
		}
		return payload, "stz2"
	case "stz2-16":
		payload := make([]byte, 12+count*2)
		payload[7] = 16
		binary.BigEndian.PutUint32(payload[8:12], uint32(count))
		for index := range count {
			binary.BigEndian.PutUint16(payload[12+index*2:14+index*2], uint16(sampleSize))
		}
		return payload, "stz2"
	default:
		panic("unknown sample size encoding")
	}
}

func replaceH265Sample(t *testing.T, sample []byte) []byte {
	t.Helper()
	data := mediatest.H265MP4()
	start := firstMP4SampleOffset(t, data)
	mdatKind := bytes.LastIndex(data[:start], []byte("mdat"))
	require.GreaterOrEqual(t, mdatKind, 4)
	require.Equal(t, mdatKind+4, start)
	stsz := mp4TestBoxPayload(t, data, "stsz")
	binary.BigEndian.PutUint32(stsz[4:8], uint32(len(sample)))
	data = append(data[:start], sample...)
	binary.BigEndian.PutUint32(data[mdatKind-4:mdatKind], uint32(8+len(sample)))
	return data
}

func nthMP4BoxPayloadForBuild(data []byte, kind string, occurrence int) []byte {
	searchStart, base := 0, -1
	for range occurrence + 1 {
		relative := bytes.Index(data[searchStart:], []byte(kind))
		if relative < 0 {
			panic("missing synthetic box " + kind)
		}
		base = searchStart + relative
		searchStart = base + len(kind)
	}
	size := int(binary.BigEndian.Uint32(data[base-4 : base]))
	return data[base+4 : base-4+size]
}

func replaceMP4Box(t *testing.T, data []byte, kind string, replacement func([]byte) []byte) []byte {
	t.Helper()
	index := bytes.Index(data, []byte(kind))
	require.GreaterOrEqual(t, index, 4)
	start := index - 4
	oldSize := int(binary.BigEndian.Uint32(data[start:index]))
	payload := replacement(data[index+4 : start+oldSize])
	newBox := mediatest.Box(kind, payload)
	delta := len(newBox) - oldSize
	result := slices.Concat(data[:start], newBox, data[start+oldSize:])
	adjustAncestorMP4Sizes(t, result, kind, delta)
	adjustMP4ChunkOffsets(t, result, delta)
	return result
}

func insertIntoMP4Box(t *testing.T, data []byte, parent string, child []byte) []byte {
	t.Helper()
	index := bytes.Index(data, []byte(parent))
	require.GreaterOrEqual(t, index, 4)
	start := index - 4
	size := int(binary.BigEndian.Uint32(data[start:index]))
	insert := start + size
	result := slices.Concat(data[:insert], child, data[insert:])
	adjustAncestorMP4Sizes(t, result, parent, len(child))
	adjustMP4ChunkOffsets(t, result, len(child))
	return result
}

func adjustAncestorMP4Sizes(t *testing.T, data []byte, _ string, delta int) {
	t.Helper()
	for _, kind := range []string{"stbl", "minf", "mdia", "trak", "moov"} {
		index := bytes.Index(data, []byte(kind))
		require.GreaterOrEqual(t, index, 4)
		size := binary.BigEndian.Uint32(data[index-4 : index])
		binary.BigEndian.PutUint32(data[index-4:index], uint32(int(size)+delta))
	}
}

func adjustMP4ChunkOffsets(t *testing.T, data []byte, delta int) {
	t.Helper()
	searchStart := 0
	for {
		relative := bytes.Index(data[searchStart:], []byte("stco"))
		if relative < 0 {
			return
		}
		base := searchStart + relative
		payload := nthMP4BoxPayloadForBuild(data, "stco", bytes.Count(data[:base], []byte("stco")))
		offset := binary.BigEndian.Uint32(payload[8:12])
		binary.BigEndian.PutUint32(payload[8:12], uint32(int(offset)+delta))
		searchStart = base + 4
	}
}
