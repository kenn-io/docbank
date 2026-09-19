package mediatranscript

import (
	"bytes"
	"errors"
	"strconv"
	"strings"
	"unicode/utf8"
)

const (
	// SubRipContractV1 identifies the strict SubRip grammar accepted by ParseSubRip.
	SubRipContractV1 = "subrip/v1"

	// MaxSubRipBytes bounds one caption file before parsing.
	MaxSubRipBytes = 16 << 20
)

var errInvalidSubRip = errors.New("invalid subrip caption")

// ParseSubRip maps one exact SubRip caption file to a supplied timed
// transcript. It never infers language, speakers, or timing.
func ParseSubRip(raw []byte, provider, language string) (ArtifactV1, error) {
	if len(raw) == 0 || len(raw) > MaxSubRipBytes || !utf8.Valid(raw) {
		return ArtifactV1{}, errInvalidSubRip
	}
	if bytes.Contains(raw, []byte{0}) {
		return ArtifactV1{}, errInvalidSubRip
	}
	text := string(raw)
	text = strings.TrimPrefix(text, "\ufeff")
	text = strings.ReplaceAll(text, "\r\n", "\n")
	if strings.ContainsRune(text, '\r') || text == "" {
		return ArtifactV1{}, errInvalidSubRip
	}

	lines := strings.Split(text, "\n")
	start := 0
	for start < len(lines) && isSubRipBlank(lines[start]) {
		start++
	}
	end := len(lines)
	for end > start && isSubRipBlank(lines[end-1]) {
		end--
	}
	if start == end {
		return ArtifactV1{}, errInvalidSubRip
	}

	segments := make([]Segment, 0, min(end-start, 256))
	var previousStart int64
	for position := start; position < end; {
		blockStart := position
		for position < end && !isSubRipBlank(lines[position]) {
			position++
		}
		block := lines[blockStart:position]
		segment, err := parseSubRipBlock(block, len(segments), previousStart)
		if err != nil {
			return ArtifactV1{}, errInvalidSubRip
		}
		previousStart = segment.StartMS
		segments = append(segments, segment)
		if len(segments) > maxSegments {
			return ArtifactV1{}, errInvalidSubRip
		}
		for position < end && isSubRipBlank(lines[position]) {
			position++
		}
	}

	artifact := ArtifactV1{
		ContractVersion: contractV1,
		Origin:          "supplied",
		Provider:        provider,
		Language:        language,
		Segments:        segments,
	}
	if _, _, err := Marshal(artifact); err != nil {
		return ArtifactV1{}, errInvalidSubRip
	}
	return artifact, nil
}

func parseSubRipBlock(lines []string, order int, previousStart int64) (Segment, error) {
	if len(lines) < 3 || !validSubRipIndex(lines[0]) {
		return Segment{}, errInvalidSubRip
	}
	startMS, endMS, ok := parseSubRipTiming(lines[1])
	if !ok || startMS < previousStart || endMS <= startMS {
		return Segment{}, errInvalidSubRip
	}
	text := strings.Join(lines[2:], "\n")
	if strings.TrimSpace(text) == "" {
		return Segment{}, errInvalidSubRip
	}
	return Segment{EndMS: endMS, Order: order, StartMS: startMS, Text: text}, nil
}

func validSubRipIndex(value string) bool {
	if len(value) == 0 || len(value) > 9 {
		return false
	}
	for _, char := range value {
		if char < '0' || char > '9' {
			return false
		}
	}
	index, err := strconv.ParseUint(value, 10, 64)
	return err == nil && index > 0
}

func parseSubRipTiming(value string) (int64, int64, bool) {
	value = strings.TrimRight(value, " \t")
	if len(value) != 29 {
		return 0, 0, false
	}
	if value[12:17] != " --> " || value[2] != ':' || value[5] != ':' || value[8] != ',' ||
		value[19] != ':' || value[22] != ':' || value[25] != ',' {
		return 0, 0, false
	}
	start, ok := parseSubRipClock(value[:12])
	if !ok {
		return 0, 0, false
	}
	end, ok := parseSubRipClock(value[17:])
	return start, end, ok
}

func parseSubRipClock(value string) (int64, bool) {
	if len(value) != 12 || value[2] != ':' || value[5] != ':' || value[8] != ',' {
		return 0, false
	}
	parts := [4]string{value[:2], value[3:5], value[6:8], value[9:]}
	values := [4]int64{}
	for index, part := range parts {
		for _, char := range part {
			if char < '0' || char > '9' {
				return 0, false
			}
		}
		parsed, err := strconv.ParseInt(part, 10, 64)
		if err != nil {
			return 0, false
		}
		values[index] = parsed
	}
	if values[0] > 23 || values[1] > 59 || values[2] > 59 || values[3] > 999 {
		return 0, false
	}
	return ((values[0]*60+values[1])*60+values[2])*1000 + values[3], true
}

func isSubRipBlank(value string) bool {
	return strings.Trim(value, " \t") == ""
}
