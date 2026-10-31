package grounding

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
)

var datePatterns = []*regexp.Regexp{
	regexp.MustCompile(`^([0-9]{4})-([0-9]{2})-([0-9]{2})`),
	regexp.MustCompile(`(?i)^([A-Za-z]+\.?) +([0-9]{1,2}),? +([0-9]{4})`),
	regexp.MustCompile(`(?i)^([0-9]{1,2}) +([A-Za-z]+\.?) +([0-9]{4})`),
}

func dateAt(rs []rune) (int, string) {
	// Dates have at most three words. Bound the word lengths while allowing
	// whitespace throughout the caller's already bounded input.
	end := 0
	for range 3 {
		start := end
		for end < len(rs) && !unicode.IsSpace(rs[end]) && end-start < 10 {
			end++
		}
		if end < len(rs) && !unicode.IsSpace(rs[end]) {
			break
		}
		end = skipSpace(rs, end)
	}
	text := strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) {
			return ' '
		}
		return r
	}, string(rs[:end]))
	for kind, pattern := range datePatterns {
		m := pattern.FindStringSubmatch(text)
		if m == nil {
			continue
		}
		num := func(s string) int { v, _ := strconv.Atoi(s); return v }
		var y, month, day int
		switch kind {
		case 0:
			y, month, day = num(m[1]), num(m[2]), num(m[3])
		case 1:
			y, month, day = num(m[3]), englishMonth(m[1]), num(m[2])
		case 2:
			y, month, day = num(m[3]), englishMonth(m[2]), num(m[1])
		}
		if month == 0 {
			continue
		}
		end := len([]rune(m[0]))
		date := time.Date(y, time.Month(month), day, 0, 0, 0, 0, time.UTC)
		if y < 1 || date.Year() != y || int(date.Month()) != month || date.Day() != day {
			return end, ""
		}
		return end, fmt.Sprintf("%04d-%02d-%02d", y, month, day)
	}
	return 0, ""
}

func englishMonth(s string) int {
	s = strings.ToLower(s)
	if strings.HasSuffix(s, ".") {
		if len(s) != 4 && s != "sept." {
			return 0
		}
		s = strings.TrimSuffix(s, ".")
	}
	for i, month := range []string{"january", "february", "march", "april", "may", "june", "july", "august", "september", "october", "november", "december"} {
		if s == month || s == month[:3] || s == "sept" && month == "september" {
			return i + 1
		}
	}
	return 0
}
