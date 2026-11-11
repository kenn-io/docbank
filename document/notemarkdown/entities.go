package notemarkdown

import (
	"strconv"
	"unicode/utf8"

	"github.com/yuin/goldmark/v2/util"
)

// Candidates are bounded to 34 source bytes by both callers. CommonMark
// requires a semicolon and exact entity names; HTML's prefix repair differs.
func decodeEntity(raw string) string {
	if len(raw) < 3 || raw[0] != '&' || raw[len(raw)-1] != ';' {
		return raw
	}
	name := raw[1 : len(raw)-1]
	if name[0] != '#' {
		if entity, ok := util.LookUpHTML5EntityByName(name); ok {
			return string(entity.Characters)
		}
		return raw
	}
	digits := name[1:]
	base, limit := 10, 7
	if len(digits) > 0 && (digits[0] == 'x' || digits[0] == 'X') {
		digits = digits[1:]
		base, limit = 16, 6
	}
	if len(digits) == 0 || len(digits) > limit {
		return raw
	}
	for _, c := range digits {
		if (c < '0' || c > '9') && (base != 16 || (c < 'a' || c > 'f') && (c < 'A' || c > 'F')) {
			return raw
		}
	}
	value, err := strconv.ParseUint(digits, base, 32)
	if err != nil {
		return raw
	}
	if value > utf8.MaxRune {
		return string(utf8.RuneError)
	}
	r := rune(value)
	if r == 0 || !utf8.ValidRune(r) {
		r = utf8.RuneError
	}
	return string(r)
}
