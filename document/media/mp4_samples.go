package media

import (
	"encoding/binary"
	"math"
	"sort"
)

const maxMP4MappedSamples = 10_000
const maxMP4InspectedPictures = 10_000

type mp4ByteRange struct {
	start, end uint64
}

type mp4SampleRange struct {
	mp4ByteRange

	descriptionIndex uint32
}

type mp4SampleToChunk struct {
	firstChunk, samplesPerChunk, descriptionIndex uint32
}

func mp4MediaDataRanges(data []byte) ([]mp4ByteRange, bool, bool) {
	ranges := make([]mp4ByteRange, 0)
	bounded := true
	for offset := 0; offset < len(data); {
		headerLen, size, ok := mp4BoxHeader(data, offset)
		if !ok {
			return nil, false, false
		}
		if string(data[offset+4:offset+8]) == "mdat" {
			if len(ranges) == maxMP4MappedSamples {
				ranges = nil
				bounded = false
			} else if bounded {
				ranges = append(ranges, mp4ByteRange{
					start: uint64(offset + headerLen), //nolint:gosec // bounded by the input slice
					end:   uint64(offset + size),      //nolint:gosec // bounded by the input slice
				})
			}
		}
		offset += size
	}
	return ranges, bounded, true
}

func parseSTSC(payload []byte, track *mp4TrackInfo) bool {
	if len(payload) < 8 || payload[0] != 0 || payload[1] != 0 || payload[2] != 0 || payload[3] != 0 {
		return false
	}
	track.stscCount++
	if track.stscCount != 1 {
		return false
	}
	count := binary.BigEndian.Uint32(payload[4:8])
	if uint64(len(payload)) != 8+uint64(count)*12 {
		return false
	}
	retain := count <= maxMP4MappedSamples
	if retain {
		track.sampleToChunks = make([]mp4SampleToChunk, 0, count)
	}
	var previousFirst uint32
	for offset := 8; offset < len(payload); offset += 12 {
		entry := mp4SampleToChunk{
			firstChunk:       binary.BigEndian.Uint32(payload[offset : offset+4]),
			samplesPerChunk:  binary.BigEndian.Uint32(payload[offset+4 : offset+8]),
			descriptionIndex: binary.BigEndian.Uint32(payload[offset+8 : offset+12]),
		}
		if entry.firstChunk == 0 || entry.samplesPerChunk == 0 || entry.descriptionIndex == 0 ||
			offset == 8 && entry.firstChunk != 1 || previousFirst >= entry.firstChunk {
			return false
		}
		if retain {
			track.sampleToChunks = append(track.sampleToChunks, entry)
		}
		previousFirst = entry.firstChunk
	}
	return true
}

func parseChunkOffsets(kind string, payload []byte, track *mp4TrackInfo) bool {
	if len(payload) < 8 || payload[0] != 0 || payload[1] != 0 || payload[2] != 0 || payload[3] != 0 {
		return false
	}
	track.chunkOffsetCount++
	if track.chunkOffsetCount != 1 {
		return false
	}
	count := binary.BigEndian.Uint32(payload[4:8])
	entryBytes := uint64(4)
	if kind == "co64" {
		entryBytes = 8
	}
	if uint64(len(payload)) != 8+uint64(count)*entryBytes {
		return false
	}
	retain := count <= maxMP4MappedSamples
	if retain {
		track.chunkOffsets = make([]uint64, 0, count)
	}
	for offset := 8; offset < len(payload); offset += int(entryBytes) {
		value := uint64(binary.BigEndian.Uint32(payload[offset : offset+4]))
		if kind == "co64" {
			value = binary.BigEndian.Uint64(payload[offset : offset+8])
		}
		if retain {
			track.chunkOffsets = append(track.chunkOffsets, value)
		}
	}
	return true
}

func (track *mp4TrackInfo) proveSampleRanges(info *mp4Info) bool {
	if len(info.sampleRanges) >= maxMP4MappedSamples {
		return false
	}
	remainingSamples := maxMP4MappedSamples - len(info.sampleRanges)
	if track.stscCount != 1 || track.chunkOffsetCount != 1 || len(track.sampleToChunks) == 0 ||
		len(track.chunkOffsets) == 0 || !track.sampleDescriptionsBounded || !info.mediaDataBounded || track.sampleCount == 0 ||
		track.sampleCount > maxMP4MappedSamples ||
		track.sampleCount > uint64(remainingSamples) || //nolint:gosec // range count never exceeds the fixed bound
		uint64(track.sampleToChunks[len(track.sampleToChunks)-1].firstChunk) > uint64(len(track.chunkOffsets)) {
		return false
	}
	sampleIndex := uint64(0)
	toChunkIndex := 0
	// State belongs to this inspection, track and sample description only.
	vp9States := make(map[uint32]*mp4VP9State)
	av1States := make(map[uint32]*mp4AV1State)
	for chunkIndex, chunkOffset := range track.chunkOffsets {
		chunkNumber := uint32(chunkIndex + 1)
		for toChunkIndex+1 < len(track.sampleToChunks) &&
			track.sampleToChunks[toChunkIndex+1].firstChunk <= chunkNumber {
			toChunkIndex++
		}
		mapping := track.sampleToChunks[toChunkIndex]
		if mapping.firstChunk > chunkNumber || mapping.descriptionIndex > track.sampleDescriptionCount ||
			sampleIndex > track.sampleCount || uint64(mapping.samplesPerChunk) > track.sampleCount-sampleIndex {
			return false
		}
		configuration := track.sampleDescriptions[mapping.descriptionIndex-1]
		cursor := chunkOffset
		for range mapping.samplesPerChunk {
			size, ok := track.sampleSize(sampleIndex)
			if !ok || size == 0 || math.MaxUint64-cursor < size {
				return false
			}
			sampleRange := mp4SampleRange{
				start: cursor, end: cursor + size,
				descriptionIndex: mapping.descriptionIndex,
			}
			if !mp4RangeInsideMediaData(sampleRange.mp4ByteRange, info.mediaDataRanges) ||
				sampleRange.end > uint64(len(info.source)) {
				return false
			}
			start := int(sampleRange.start) //nolint:gosec // bounded by source length above
			end := int(sampleRange.end)     //nolint:gosec // bounded by source length above
			remainingPictures := maxMP4InspectedPictures - info.frameCount - track.inspectedPictures
			if remainingPictures <= 0 {
				return false
			}
			var valid bool
			var dimensions mp4SampleDimensions
			switch configuration.codec {
			case "vp9":
				state := vp9States[mapping.descriptionIndex]
				if state == nil {
					state, valid = newMP4VP9State(configuration.configuration)
					if !valid {
						return false
					}
					vp9States[mapping.descriptionIndex] = state
				}
				state.pictureBudget = remainingPictures
				dimensions, valid = state.inspectSample(info.source[start:end])
			case "av1":
				state := av1States[mapping.descriptionIndex]
				if state == nil {
					state, valid = newMP4AV1State(configuration.configuration)
					if !valid {
						return false
					}
					av1States[mapping.descriptionIndex] = state
				}
				state.pictureBudget = remainingPictures
				dimensions, valid = state.inspectSample(info.source[start:end])
			default:
				valid = validMP4CodecSample(configuration, info.source[start:end])
				dimensions.frames = 1
			}
			if !valid {
				return false
			}
			if dimensions.frames <= 0 || dimensions.frames > remainingPictures {
				return false
			}
			track.inspectedPictures += dimensions.frames
			track.codedWidth = max(track.codedWidth, dimensions.width)
			track.codedHeight = max(track.codedHeight, dimensions.height)
			info.sampleRanges = append(info.sampleRanges, sampleRange)
			cursor += size
			sampleIndex++
		}
	}
	return sampleIndex == track.sampleCount
}

func (track *mp4TrackInfo) sampleSize(index uint64) (uint64, bool) {
	if index >= track.sampleCount {
		return 0, false
	}
	if track.defaultSampleSize != 0 {
		return uint64(track.defaultSampleSize), true
	}
	switch track.sampleSizeFieldBits {
	case 4:
		byteIndex := index / 2
		if byteIndex >= uint64(len(track.sampleSizeTable)) {
			return 0, false
		}
		value := track.sampleSizeTable[byteIndex]
		if index%2 == 0 {
			return uint64(value >> 4), true
		}
		return uint64(value & 0x0f), true
	case 8:
		if index >= uint64(len(track.sampleSizeTable)) {
			return 0, false
		}
		return uint64(track.sampleSizeTable[index]), true
	case 16:
		offset := index * 2
		if offset+2 > uint64(len(track.sampleSizeTable)) {
			return 0, false
		}
		return uint64(binary.BigEndian.Uint16(track.sampleSizeTable[offset : offset+2])), true
	case 32:
		offset := index * 4
		if offset+4 > uint64(len(track.sampleSizeTable)) {
			return 0, false
		}
		return uint64(binary.BigEndian.Uint32(track.sampleSizeTable[offset : offset+4])), true
	default:
		return 0, false
	}
}

func mp4RangeInsideMediaData(sample mp4ByteRange, mediaData []mp4ByteRange) bool {
	if sample.start >= sample.end {
		return false
	}
	index := sort.Search(len(mediaData), func(index int) bool {
		return mediaData[index].end > sample.start
	})
	if index == len(mediaData) {
		return false
	}
	bounds := mediaData[index]
	return sample.start >= bounds.start && sample.end <= bounds.end
}

func mp4SampleRangesDoNotOverlap(samples []mp4SampleRange) bool {
	if len(samples) == 0 {
		return false
	}
	sort.Slice(samples, func(left, right int) bool {
		if samples[left].start == samples[right].start {
			return samples[left].end < samples[right].end
		}
		return samples[left].start < samples[right].start
	})
	for index := 1; index < len(samples); index++ {
		if samples[index].start < samples[index-1].end {
			return false
		}
	}
	return true
}

func canonicalMP4Container(data []byte) string {
	if len(data) >= 12 && string(data[8:12]) == "qt  " {
		return "quicktime"
	}
	return "mp4"
}
