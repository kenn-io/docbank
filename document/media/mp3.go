package media

import (
	"bytes"
	"math"
)

const maxID3v2TagBytes = 1 << 20

type mp3FrameHeader struct {
	version         byte
	sampleRate      uint64
	samples, length uint64
}

func inspectMP3(data []byte, policy InspectionPolicy) CapabilityRecord {
	record := CapabilityRecord{MediaFamily: "audio", MediaType: "audio/mpeg", Format: "mp3"}
	if policy.MaxDurationMS <= 0 {
		record.Reason = CapabilityReasonMalformed
		return record
	}
	audioStart, audioEnd, ok := mp3AudioBounds(data)
	if !ok {
		record.Reason = CapabilityReasonMalformed
		return record
	}
	var reference mp3FrameHeader
	var seenFrame bool
	var totalSamples uint64
	for offset := audioStart; offset < audioEnd; {
		frame, valid := parseMP3FrameHeader(data[offset:audioEnd])
		if !valid || seenFrame && (frame.version != reference.version || frame.sampleRate != reference.sampleRate) {
			record.Reason = CapabilityReasonMalformed
			return record
		}
		if !seenFrame {
			reference = frame
			seenFrame = true
		}
		remaining := uint64(audioEnd - offset) //nolint:gosec // bounded by the input slice
		if frame.length > remaining || math.MaxUint64-totalSamples < frame.samples {
			record.Reason = CapabilityReasonMalformed
			return record
		}
		totalSamples += frame.samples
		offset += int(frame.length) //nolint:gosec // frame length is proven within the remaining input
	}
	if !seenFrame {
		record.Reason = CapabilityReasonMalformed
		return record
	}
	durationMS, valid := mp3DurationMilliseconds(totalSamples, reference.sampleRate)
	if !valid {
		record.Reason = CapabilityReasonMalformed
		return record
	}
	record.Measurements.DurationMS = durationMS
	if durationMS > policy.MaxDurationMS {
		record.Reason = CapabilityReasonVisualBounds
		return record
	}
	record.Eligible, record.Reason = true, CapabilityReasonEligible
	return record
}

func mp3AudioBounds(data []byte) (int, int, bool) {
	start, end := 0, len(data)
	if bytes.HasPrefix(data, []byte("ID3")) {
		if len(data) < 10 {
			return 0, 0, false
		}
		version, revision, flags := data[3], data[4], data[5]
		var allowedFlags byte
		switch version {
		case 2:
			allowedFlags = 0xc0
		case 3:
			allowedFlags = 0xe0
		case 4:
			allowedFlags = 0xf0
		default:
			return 0, 0, false
		}
		if revision == 0xff || flags & ^allowedFlags != 0 {
			return 0, 0, false
		}
		tagSize := 0
		for _, value := range data[6:10] {
			if value&0x80 != 0 {
				return 0, 0, false
			}
			tagSize = tagSize<<7 | int(value)
		}
		if tagSize > maxID3v2TagBytes {
			return 0, 0, false
		}
		footerBytes := 0
		if version == 4 && flags&0x10 != 0 {
			footerBytes = 10
		}
		start = 10 + tagSize + footerBytes
		if start > end {
			return 0, 0, false
		}
		if footerBytes != 0 {
			footer := data[start-footerBytes : start : start]
			if !bytes.Equal(footer[:3], []byte("3DI")) || footer[3] != version || footer[4] != revision ||
				footer[5] != flags || !bytes.Equal(footer[6:10], data[6:10]) {
				return 0, 0, false
			}
		}
	}
	if end-start >= 128 && bytes.Equal(data[end-128:end-125], []byte("TAG")) {
		end -= 128
	}
	return start, end, start < end
}

func parseMP3FrameHeader(data []byte) (mp3FrameHeader, bool) {
	if len(data) < 4 || data[0] != 0xff || data[1]&0xe0 != 0xe0 {
		return mp3FrameHeader{}, false
	}
	versionID := (data[1] >> 3) & 0x03
	if versionID == 1 || (data[1]>>1)&0x03 != 1 || data[3]&0x03 == 2 {
		return mp3FrameHeader{}, false
	}
	bitrateIndex := (data[2] >> 4) & 0x0f
	sampleRateIndex := (data[2] >> 2) & 0x03
	if bitrateIndex == 0 || bitrateIndex == 15 || sampleRateIndex == 3 {
		return mp3FrameHeader{}, false
	}
	var sampleRate, bitrate, samples, coefficient uint64
	switch versionID {
	case 3:
		sampleRate = [...]uint64{44_100, 48_000, 32_000}[sampleRateIndex]
		bitrate = [...]uint64{32, 40, 48, 56, 64, 80, 96, 112, 128, 160, 192, 224, 256, 320}[bitrateIndex-1]
		samples, coefficient = 1_152, 144_000
	case 2:
		sampleRate = [...]uint64{22_050, 24_000, 16_000}[sampleRateIndex]
		bitrate = [...]uint64{8, 16, 24, 32, 40, 48, 56, 64, 80, 96, 112, 128, 144, 160}[bitrateIndex-1]
		samples, coefficient = 576, 72_000
	case 0:
		sampleRate = [...]uint64{11_025, 12_000, 8_000}[sampleRateIndex]
		bitrate = [...]uint64{8, 16, 24, 32, 40, 48, 56, 64, 80, 96, 112, 128, 144, 160}[bitrateIndex-1]
		samples, coefficient = 576, 72_000
	default:
		return mp3FrameHeader{}, false
	}
	length := coefficient*bitrate/sampleRate + uint64((data[2]>>1)&1)
	return mp3FrameHeader{version: versionID, sampleRate: sampleRate, samples: samples, length: length}, length >= 4
}

func mp3DurationMilliseconds(samples, sampleRate uint64) (int64, bool) {
	if samples == 0 || sampleRate == 0 || samples > math.MaxUint64/1_000 {
		return 0, false
	}
	scaledSamples := samples * 1_000
	milliseconds := scaledSamples / sampleRate
	if scaledSamples%sampleRate != 0 {
		milliseconds++
	}
	if milliseconds > math.MaxInt64 {
		return 0, false
	}
	return int64(milliseconds), true
}
