package grounding

import (
	"regexp"
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
		value, layout := m[0], time.DateOnly
		if kind != 0 {
			month, day := m[1], m[2]
			if kind == 2 {
				month, day = m[2], m[1]
			}
			month = englishMonth(month)
			if month == "" {
				continue
			}
			value, layout = m[3]+"-"+month+"-"+day, "2006-Jan-2"
		}
		end := len([]rune(m[0]))
		date, err := time.Parse(layout, value)
		if err != nil || date.Year() < 1 {
			return end, ""
		}
		return end, date.Format(time.DateOnly)
	}
	return 0, ""
}

func englishMonth(s string) string {
	s = strings.ToLower(s)
	if strings.HasSuffix(s, ".") {
		if len(s) != 4 && s != "sept." {
			return ""
		}
		s = strings.TrimSuffix(s, ".")
	}
	for _, month := range []string{"january", "february", "march", "april", "may", "june", "july", "august", "september", "october", "november", "december"} {
		if s == month || s == month[:3] || s == "sept" && month == "september" {
			return month[:3]
		}
	}
	return ""
}
