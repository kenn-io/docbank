package eval

import (
	"unicode"
	"unicode/utf8"
)

// BoundedPassageWindow returns rune offsets into valid UTF-8 text for an
// experimental byte-bounded window centered on a native match's rune span.
// A zero span starts at the head. Invalid UTF-8, span or budget returns empty.
func BoundedPassageWindow(text string, match, matchEnd, maxBytes int) (int, int) {
	if maxBytes <= 0 || !utf8.ValidString(text) {
		return 0, 0
	}
	runes := []rune(text)
	if match < 0 || matchEnd < match || matchEnd > len(runes) {
		return 0, 0
	}
	offsets := make([]int, 0, len(runes)+1)
	for offset := range text {
		offsets = append(offsets, offset)
	}
	offsets = append(offsets, len(text))
	if len(text) <= maxBytes {
		return 0, len(runes)
	}
	start := 0
	if matchEnd > match {
		target := max(0, offsets[match]-maxBytes/2)
		if offsets[matchEnd]-offsets[match] <= maxBytes {
			target = max(target, offsets[matchEnd]-maxBytes)
		}
		for start < len(runes) && offsets[start] < target {
			start++
		}
	}
	end := start
	for end < len(runes) && offsets[end+1]-offsets[start] <= maxBytes {
		end++
	}
	// Expand to nearby whitespace only when the extra runes fit the budget.
	for i := start - 1; i >= 0 && offsets[end]-offsets[i] <= maxBytes; i-- {
		if unicode.IsSpace(runes[i]) {
			start = i
			break
		}
	}
	for i := end; i < len(runes) && offsets[i+1]-offsets[start] <= maxBytes; i++ {
		if unicode.IsSpace(runes[i]) {
			end = i + 1
			break
		}
	}
	return start, end
}
