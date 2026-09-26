package document

import (
	"bytes"
	"errors"
	"unicode/utf8"
)

// SectionPage returns an owned, codepoint-safe page from one exact section.
// The returned next byte is absolute in body; next == end means the section is
// complete.
func SectionPage(body []byte, start, end, budget int) ([]byte, int, error) {
	if budget < 1 {
		return nil, start, errors.New("positive byte budget required")
	}
	if !utf8.Valid(body) || start < 0 || end < start || end > len(body) {
		return nil, start, errors.New("invalid section range")
	}
	if start > 0 && !utf8.RuneStart(body[start]) ||
		end < len(body) && !utf8.RuneStart(body[end]) {
		return nil, start, errors.New("section range splits UTF-8")
	}
	if start == end {
		return []byte{}, end, nil
	}
	stop := start + min(budget, end-start)
	for stop > start && stop < end && !utf8.RuneStart(body[stop]) {
		stop--
	}
	if stop == start {
		return nil, start, errors.New("byte budget cannot fit next codepoint")
	}
	return bytes.Clone(body[start:stop]), stop, nil
}
