// Package jsontext validates the lossless text boundary that encoding/json
// deliberately does not enforce.
package jsontext

import (
	"fmt"
	"unicode/utf8"
)

// Validate rejects byte sequences and escaped UTF-16 surrogate units that
// encoding/json would silently replace with U+FFFD. It intentionally is not a
// complete JSON syntax validator; callers still pass accepted input to their
// normal decoder.
func Validate(raw []byte, subject string) error {
	if !utf8.Valid(raw) {
		return fmt.Errorf("%s is not valid UTF-8", subject)
	}
	for i := 0; i < len(raw); i++ {
		if raw[i] != '"' {
			continue
		}
		for i++; i < len(raw) && raw[i] != '"'; i++ {
			if raw[i] != '\\' {
				continue
			}
			if i+1 >= len(raw) {
				return fmt.Errorf("%s contains an incomplete escape", subject)
			}
			if raw[i+1] != 'u' {
				i++
				continue
			}
			unit, ok := parseUTF16Unit(raw[i+2:])
			if !ok {
				return fmt.Errorf("%s contains an invalid Unicode escape", subject)
			}
			i += 5
			switch {
			case unit >= 0xd800 && unit <= 0xdbff:
				next := i + 1
				if next+6 > len(raw) || raw[next] != '\\' || raw[next+1] != 'u' {
					return fmt.Errorf("%s contains an unpaired UTF-16 surrogate escape", subject)
				}
				low, valid := parseUTF16Unit(raw[next+2:])
				if !valid || low < 0xdc00 || low > 0xdfff {
					return fmt.Errorf("%s contains an unpaired UTF-16 surrogate escape", subject)
				}
				i = next + 5
			case unit >= 0xdc00 && unit <= 0xdfff:
				return fmt.Errorf("%s contains an unpaired UTF-16 surrogate escape", subject)
			}
		}
	}
	return nil
}

func parseUTF16Unit(raw []byte) (uint16, bool) {
	if len(raw) < 4 {
		return 0, false
	}
	var value uint16
	for _, b := range raw[:4] {
		value <<= 4
		switch {
		case b >= '0' && b <= '9':
			value |= uint16(b - '0')
		case b >= 'a' && b <= 'f':
			value |= uint16(b-'a') + 10
		case b >= 'A' && b <= 'F':
			value |= uint16(b-'A') + 10
		default:
			return 0, false
		}
	}
	return value, true
}
