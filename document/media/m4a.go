package media

import (
	"bufio"
	"encoding/binary"
	"io"
	"math"
	"math/bits"
)

// Audio samples are streamed, never retained or decoded. The movie box holds
// the bounded track declarations and sample tables needed to prove duration
// and ownership. This ceiling also bounds table allocations in the MP4 parser.
const maxM4AMetadataBytes = int64(16 << 20)

// maxM4AChunks bounds the per-chunk ownership ranges retained for the overlap
// proof. Muxers interleave audio in chunks of roughly half a second or longer,
// so this admits a 24-hour recording with chunks as short as 85 ms.
const maxM4AChunks = 1 << 20

// m4aAudioFormat is the decoder frame geometry of one sample entry. A decoder
// produces samplesPerFrame PCM samples per stored sample, independent of the
// header durations, so it bounds how much audio the file really holds.
type m4aAudioFormat struct {
	qualified                   bool
	samplesPerFrame, sampleRate uint32
}

func inspectM4A(reader *bufio.Reader, policy InspectionPolicy) (CapabilityRecord, error) {
	record := CapabilityRecord{MediaFamily: "audio", MediaType: "audio/mp4", Format: "m4a", Reason: CapabilityReasonMalformed}
	info := mp4Info{audioInspection: true, localDataReferences: true, maxBoxes: policy.MaxEntries}
	var movie []byte
	var offset int64
	ftypCount := 0
	for offset < policy.ExpectedBytes {
		remaining := policy.ExpectedBytes - offset
		if remaining < 8 {
			return record, nil
		}
		var header [16]byte
		if _, err := io.ReadFull(reader, header[:8]); err != nil {
			return record, err
		}
		size := uint64(binary.BigEndian.Uint32(header[:4]))
		headerSize := int64(8)
		switch size {
		case 1:
			if _, err := io.ReadFull(reader, header[8:]); err != nil {
				return record, err
			}
			size = binary.BigEndian.Uint64(header[8:])
			headerSize = 16
		case 0:
			size = uint64(remaining)
		}
		if size < uint64(headerSize) || size > uint64(remaining) {
			return record, nil
		}
		boxSize := int64(size) //nolint:gosec // bounded by positive ExpectedBytes
		payloadSize := boxSize - headerSize
		kind := string(header[4:8])
		info.boxCount++
		if info.boxCount > info.maxBoxes {
			record.Reason = CapabilityReasonEntryCount
			return record, nil
		}
		switch kind {
		case "ftyp", "moov":
			if payloadSize > min(policy.MaxEntryBytes, maxM4AMetadataBytes) {
				record.Reason = CapabilityReasonEntryBytes
				return record, nil
			}
			payload := make([]byte, int(payloadSize)) // At most 16 MiB.
			if _, err := io.ReadFull(reader, payload); err != nil {
				return record, err
			}
			if kind == "ftyp" {
				ftypCount++
				if offset != 0 || ftypCount != 1 || len(payload) < 8 || len(payload)%4 != 0 || !m4aBrand(string(payload[:4])) {
					return record, nil
				}
			} else {
				info.moovCount++
				if info.moovCount != 1 {
					return record, nil
				}
				movie = payload
			}
		case "mdat":
			info.mdatCount++
			info.mediaDataRanges = append(info.mediaDataRanges, mp4ByteRange{start: uint64(offset + headerSize), end: uint64(offset + boxSize)}) // Nonnegative source offsets.
			if _, err := io.CopyN(io.Discard, reader, payloadSize); err != nil {
				return record, err
			}
		case "moof", "mvex":
			return record, nil
		case "free", "skip", "wide", "uuid":
			if _, err := io.CopyN(io.Discard, reader, payloadSize); err != nil {
				return record, err
			}
		default:
			// Unknown top-level authority is outside the qualified audio layout.
			return record, nil
		}
		offset += boxSize
	}
	if ftypCount != 1 || info.moovCount != 1 || info.mdatCount == 0 {
		return record, nil
	}
	if reason := qualifyM4AMovie(movie, &info); reason != CapabilityReasonEligible {
		record.Reason = reason
		return record, nil
	}
	info.resolveDuration()
	if !info.durationKnown || info.durationMS <= 0 || policy.MaxDurationMS <= 0 {
		return record, nil
	}
	record.Measurements.DurationMS = info.durationMS
	if info.durationMS > policy.MaxDurationMS {
		record.Reason = CapabilityReasonSemanticUnits
		return record, nil
	}
	record.Eligible, record.Reason = true, CapabilityReasonEligible
	return record, nil
}

// qualifyM4AMovie scans the movie box and proves that every track is
// qualified sound whose samples occupy disjoint local media-data bytes. It
// returns CapabilityReasonEligible when the movie qualifies.
func qualifyM4AMovie(movie []byte, info *mp4Info) CapabilityReason {
	if !scanMP4Boxes(movie, info, nil, "moov", 1) {
		if info.boxCount > info.maxBoxes {
			return CapabilityReasonEntryCount
		}
		return CapabilityReasonMalformed
	}
	if info.mvhdCount != 1 || len(info.tracks) == 0 || !info.localDataReferences || info.pictureTracks != 0 {
		return CapabilityReasonMalformed
	}
	chunks := 0
	for _, track := range info.tracks {
		chunks += len(track.chunkOffsets)
	}
	if chunks > maxM4AChunks {
		return CapabilityReasonEntryCount
	}
	info.sampleRanges = make([]mp4SampleRange, 0, chunks)
	for _, track := range info.tracks {
		if track.handlerType != "soun" || track.stsdCount != 1 || len(track.audioFormats) == 0 {
			return CapabilityReasonMalformed
		}
		for _, format := range track.audioFormats {
			if !format.qualified {
				return CapabilityReasonMalformed
			}
		}
		if !track.proveAudioRanges(info) {
			return CapabilityReasonMalformed
		}
	}
	if !mp4SampleRangesDoNotOverlap(info.sampleRanges) {
		return CapabilityReasonMalformed
	}
	return CapabilityReasonEligible
}

func m4aBrand(brand string) bool {
	switch brand {
	case "M4A ", "isom", "iso2", "mp41", "mp42":
		return true
	default:
		return false
	}
}

// proveAudioRanges checks sample-table coverage and local mdat ownership without
// reading codec payloads. Fixed-size samples are counted per chunk, so long
// recordings do not inherit the visual detector's 10,000-sample decode bound.
func (track *mp4TrackInfo) proveAudioRanges(info *mp4Info) bool {
	if track.stscCount != 1 || track.chunkOffsetCount != 1 || len(track.sampleToChunks) == 0 ||
		len(track.chunkOffsets) == 0 || track.sampleCount == 0 ||
		uint64(track.sampleToChunks[len(track.sampleToChunks)-1].firstChunk) > uint64(len(track.chunkOffsets)) {
		return false
	}
	var sampleIndex uint64
	mappingIndex := 0
	framesByDescription := make([]uint64, len(track.audioFormats))
	for chunkIndex, start := range track.chunkOffsets {
		chunkNumber := uint32(chunkIndex + 1) // Table allocation is bounded to 16 MiB.
		for mappingIndex+1 < len(track.sampleToChunks) && track.sampleToChunks[mappingIndex+1].firstChunk <= chunkNumber {
			mappingIndex++
		}
		mapping := track.sampleToChunks[mappingIndex]
		count := uint64(mapping.samplesPerChunk)
		if uint64(mapping.descriptionIndex) > uint64(len(framesByDescription)) || count > track.sampleCount-sampleIndex {
			return false
		}
		framesByDescription[mapping.descriptionIndex-1] += count
		size := count * uint64(track.defaultSampleSize) // uint32 * uint32 cannot overflow uint64
		if track.defaultSampleSize == 0 {
			for index := sampleIndex; index < sampleIndex+count; index++ {
				n, ok := track.sampleSize(index)
				if !ok || n == 0 || math.MaxUint64-size < n {
					return false
				}
				size += n
			}
		}
		if size == 0 || math.MaxUint64-start < size {
			return false
		}
		bounds := mp4ByteRange{start: start, end: start + size}
		if !mp4RangeInsideMediaData(bounds, info.mediaDataRanges) {
			return false
		}
		info.sampleRanges = append(info.sampleRanges, mp4SampleRange{mp4ByteRange: bounds, descriptionIndex: mapping.descriptionIndex})
		sampleIndex += count
	}
	return sampleIndex == track.sampleCount && track.recordDecodedDuration(info, framesByDescription)
}

// recordDecodedDuration adds the PCM length a decoder produces from the stored
// frames, so understated header durations cannot pass a duration cap. AAC
// frames decode to exactly samplesPerFrame samples; a final ALAC frame may be
// shorter, which makes this an upper bound for ALAC.
func (track *mp4TrackInfo) recordDecodedDuration(info *mp4Info, framesByDescription []uint64) bool {
	var milliseconds int64
	for index, frames := range framesByDescription {
		format := track.audioFormats[index]
		high, samples := bits.Mul64(frames, uint64(format.samplesPerFrame))
		if high != 0 {
			return false
		}
		value, ok := mp4DurationMilliseconds(samples, uint64(format.sampleRate))
		if !ok || value > math.MaxInt64-milliseconds {
			return false
		}
		milliseconds += value
	}
	info.mediaDurations = append(info.mediaDurations, mp4Duration{value: uint64(milliseconds), timescale: 1000})
	return true
}

func validM4ASampleEntry(kind string, entry []byte) m4aAudioFormat {
	if kind != "mp4a" && kind != "alac" {
		return m4aAudioFormat{}
	}
	// ISO AudioSampleEntry version zero, with finite channel/sample parameters.
	if len(entry) < 28 || binary.BigEndian.Uint16(entry[8:10]) != 0 ||
		binary.BigEndian.Uint16(entry[16:18]) == 0 || binary.BigEndian.Uint16(entry[18:20]) == 0 ||
		binary.BigEndian.Uint32(entry[24:28]) == 0 {
		return m4aAudioFormat{}
	}
	var format m4aAudioFormat
	configurations := 0
	for offset := 28; offset < len(entry); {
		header, size, ok := mp4BoxHeader(entry, offset)
		if !ok {
			return m4aAudioFormat{}
		}
		childKind := string(entry[offset+4 : offset+8])
		payload := entry[offset+header : offset+size]
		switch childKind {
		case "esds":
			if format = validAACDescriptor(payload); kind != "mp4a" || !format.qualified {
				return m4aAudioFormat{}
			}
			configurations++
		case "alac":
			if kind != "alac" || len(payload) != 28 || binary.BigEndian.Uint32(payload[:4]) != 0 ||
				binary.BigEndian.Uint32(payload[4:8]) == 0 || payload[8] != 0 ||
				(payload[9] != 16 && payload[9] != 20 && payload[9] != 24 && payload[9] != 32) ||
				payload[13] == 0 || payload[13] > 8 || binary.BigEndian.Uint32(payload[24:28]) == 0 {
				return m4aAudioFormat{}
			}
			format = m4aAudioFormat{
				qualified:       true,
				samplesPerFrame: binary.BigEndian.Uint32(payload[4:8]),
				sampleRate:      binary.BigEndian.Uint32(payload[24:28]),
			}
			configurations++
		case "btrt", "free": // optional bitrate or padding metadata
		default:
			return m4aAudioFormat{}
		}
		offset += size
	}
	if configurations != 1 {
		return m4aAudioFormat{}
	}
	return format
}

// m4aDescriptor extracts one MPEG-4 expandable-length descriptor without trusting
// its declared length. Four length bytes is the format's maximum.
func m4aDescriptor(data []byte, tag byte) (payload, rest []byte, ok bool) {
	if len(data) < 2 || data[0] != tag {
		return nil, nil, false
	}
	length := 0
	for index := 1; index <= 4 && index < len(data); index++ {
		length = length<<7 | int(data[index]&0x7f)
		if data[index]&0x80 == 0 {
			start := index + 1
			if length > len(data)-start {
				return nil, nil, false
			}
			return data[start : start+length], data[start+length:], true
		}
	}
	return nil, nil, false
}

func validAACDescriptor(data []byte) m4aAudioFormat {
	if len(data) < 4 || binary.BigEndian.Uint32(data[:4]) != 0 {
		return m4aAudioFormat{}
	}
	es, rest, ok := m4aDescriptor(data[4:], 3)
	if !ok || len(rest) != 0 || len(es) < 3 || es[2]&0xe0 != 0 {
		return m4aAudioFormat{}
	}
	decoder, rest, ok := m4aDescriptor(es[3:], 4)
	if !ok || len(decoder) < 13 || decoder[0] != 0x40 || decoder[1]>>2 != 5 {
		return m4aAudioFormat{}
	}
	// SLConfigDescriptor may follow the decoder configuration.
	if len(rest) > 0 {
		sl, tail, valid := m4aDescriptor(rest, 6)
		if !valid || len(tail) != 0 || len(sl) != 1 || sl[0] != 2 {
			return m4aAudioFormat{}
		}
	}
	config, tail, ok := m4aDescriptor(decoder[13:], 5)
	if !ok || len(tail) != 0 || len(config) < 2 {
		return m4aAudioFormat{}
	}
	return validAACAudioSpecificConfig(config)
}

// Validate the mandatory AudioSpecificConfig and GASpecificConfig fields, not
// just the codec tag. A flag that requires more bits must not turn a truncated
// decoder declaration into an eligible capability. The returned frame geometry
// uses the core sampling rate: an SBR frame doubles both the samples and the
// output rate, so its duration is unchanged.
func validAACAudioSpecificConfig(config []byte) m4aAudioFormat {
	reader := mp4BitReader{data: config}
	objectType, ok := reader.readBits(5)
	if !ok {
		return m4aAudioFormat{}
	}
	sampleRate, ok := readAACFrequency(&reader)
	if !ok {
		return m4aAudioFormat{}
	}
	channels, ok := reader.readBits(4)
	if !ok || channels == 0 || channels > 7 {
		return m4aAudioFormat{}
	} // ProgramConfigElement layouts are not qualified.
	explicitSBR := objectType == 5 || objectType == 29
	if explicitSBR {
		if _, ok := readAACFrequency(&reader); !ok {
			return m4aAudioFormat{}
		}
		objectType, ok = reader.readBits(5)
		if !ok {
			return m4aAudioFormat{}
		}
	}
	switch objectType {
	case 1, 2, 3, 4, 6, 17, 19, 20, 23:
	default:
		return m4aAudioFormat{}
	}
	shortFrames, ok := reader.readBits(1) // frameLengthFlag
	if !ok {
		return m4aAudioFormat{}
	}
	depends, ok := reader.readBits(1)
	if !ok || depends == 1 && !reader.skipBits(14) {
		return m4aAudioFormat{}
	}
	extension, ok := reader.readBits(1)
	if !ok {
		return m4aAudioFormat{}
	}
	if (objectType == 6 || objectType == 20) && !reader.skipBits(3) {
		return m4aAudioFormat{}
	} // layerNr
	if extension == 1 {
		if (objectType == 17 || objectType == 19 || objectType == 20 || objectType == 23) && !reader.skipBits(3) {
			return m4aAudioFormat{}
		}
		extension3, valid := reader.readBits(1)
		if !valid || extension3 != 0 {
			return m4aAudioFormat{}
		}
	}
	if objectType >= 17 {
		epConfig, valid := reader.readBits(2)
		if !valid || epConfig != 0 {
			return m4aAudioFormat{}
		} // Error-protection layouts require a separate qualification.
	}
	// Encoders can append the backward-compatible SBR sync extension.
	if !explicitSBR && len(config)*8-reader.bit >= 11 {
		sync, valid := reader.readBits(11)
		if !valid || sync != 0x2b7 {
			return m4aAudioFormat{}
		}
		extensionType, valid := reader.readBits(5)
		if !valid || extensionType != 5 {
			return m4aAudioFormat{}
		}
		present, valid := reader.readBits(1)
		if !valid {
			return m4aAudioFormat{}
		}
		if present == 1 {
			if _, valid := readAACFrequency(&reader); !valid {
				return m4aAudioFormat{}
			}
		}
		if present == 1 && len(config)*8-reader.bit >= 11 {
			psSync, valid := reader.readBits(11)
			if !valid || psSync != 0x548 || !reader.skipBits(1) {
				return m4aAudioFormat{}
			}
		}
	}
	remaining := len(config)*8 - reader.bit
	if remaining > 7 {
		return m4aAudioFormat{}
	}
	if padding, ok := reader.readBits(remaining); !ok || padding != 0 {
		return m4aAudioFormat{}
	}
	return m4aAudioFormat{qualified: true, samplesPerFrame: aacSamplesPerFrame(objectType, shortFrames == 1), sampleRate: sampleRate}
}

// aacSamplesPerFrame follows ISO/IEC 14496-3: AAC LD codes 512 or 480 samples
// per frame and the other qualified object types code 1024 or 960.
func aacSamplesPerFrame(objectType uint64, shortFrames bool) uint32 {
	switch {
	case objectType == 23 && shortFrames:
		return 480
	case objectType == 23:
		return 512
	case shortFrames:
		return 960
	default:
		return 1024
	}
}

// aacSamplingFrequencies is the ISO/IEC 14496-3 samplingFrequencyIndex table.
var aacSamplingFrequencies = [...]uint32{96000, 88200, 64000, 48000, 44100, 32000, 24000, 22050, 16000, 12000, 11025, 8000, 7350}

func readAACFrequency(reader *mp4BitReader) (uint32, bool) {
	index, ok := reader.readBits(4)
	if !ok {
		return 0, false
	}
	if index == 15 {
		frequency, valid := reader.readBits(24)
		return uint32(frequency), valid && frequency > 0 //nolint:gosec // a 24-bit field fits uint32
	}
	if index >= uint64(len(aacSamplingFrequencies)) {
		return 0, false
	}
	return aacSamplingFrequencies[index], true
}
