package answercard

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"unicode/utf8"
)

const (
	rebindWindowChars = 16_000
	maxRebindChars    = 1 << 20
	maxRebindBytes    = 4 << 20
)

// scanExactQuote scans to EOF, carrying partial matches across window boundaries.
// KMP's fallback also counts overlapping occurrences without buffering the text.
func scanExactQuote(quote string, windowChars int, read func(int, int) (string, bool, error)) (int, string, error) {
	needle := []rune(quote)
	if !utf8.ValidString(quote) || len(needle) == 0 {
		return 0, "", errors.New("rebind_refused: empty or invalid quote")
	}
	if len(needle) > rebindWindowChars || windowChars < 1 || windowChars > rebindWindowChars {
		return 0, "", errors.New("rebind_refused: oversized quote or window")
	}
	fallback := make([]int, len(needle))
	for i, j := 1, 0; i < len(needle); i++ {
		for j > 0 && needle[i] != needle[j] {
			j = fallback[j-1]
		}
		if needle[i] == needle[j] {
			j++
		}
		fallback[i] = j
	}
	hash := sha256.New()
	offset, bytesRead, matched, count, start := 0, 0, 0, 0, 0
	for {
		text, eof, err := read(offset, windowChars)
		if err != nil {
			return 0, "", err
		}
		chars := utf8.RuneCountInString(text)
		if !utf8.ValidString(text) || chars > windowChars || (!eof && chars != windowChars) {
			return 0, "", errors.New("rebind_refused: invalid rendition window")
		}
		if chars > maxRebindChars-offset || len(text) > maxRebindBytes-bytesRead {
			return 0, "", errors.New("rebind_refused: oversized rendition")
		}
		_, _ = hash.Write([]byte(text))
		bytesRead += len(text)
		for _, r := range text {
			for matched > 0 && r != needle[matched] {
				matched = fallback[matched-1]
			}
			if r == needle[matched] {
				matched++
			}
			offset++
			if matched == len(needle) {
				start = offset - len(needle)
				count = min(count+1, 2)
				matched = fallback[matched-1]
			}
		}
		if eof {
			break
		}
		if offset == maxRebindChars || bytesRead == maxRebindBytes {
			return 0, "", errors.New("rebind_refused: oversized rendition")
		}
	}
	if count == 0 {
		return 0, "", errors.New("rebind_refused: missing quote")
	}
	if count != 1 {
		return 0, "", errors.New("rebind_refused: ambiguous quote")
	}
	return start, hex.EncodeToString(hash.Sum(nil)), nil
}
