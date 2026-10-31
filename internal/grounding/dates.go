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
	regexp.MustCompile(`^([0-9]{4})([-/])([0-9]{2})([-/])([0-9]{2})`),
	regexp.MustCompile(`^([0-9]{2})([/.])([0-9]{2})([/.])([0-9]{4})`),
	regexp.MustCompile(`(?i)^([A-Za-z]+\.?)[\t-\r \x{0085}\p{Z}]+(?:([0-9]{1,2}),?[\t-\r \x{0085}\p{Z}]+)?([0-9]{4})`),
	regexp.MustCompile(`(?i)^([0-9]{1,2})[\t-\r \x{0085}\p{Z}]+([A-Za-z]+\.?)[\t-\r \x{0085}\p{Z}]+([0-9]{4})`),
	regexp.MustCompile(`^([0-9]{4})-([0-9]{2})`),
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
	text := string(rs[:end])
	for kind, pattern := range datePatterns {
		m := pattern.FindStringSubmatch(text)
		if m == nil {
			continue
		}
		end := len([]rune(m[0]))
		value := ""
		add := func(y, m, d int) {
			if y < 1 || m < 1 || m > 12 {
				return
			}
			dateValue := fmt.Sprintf("%04d-%02d", y, m)
			if d != -1 {
				date := time.Date(y, time.Month(m), d, 0, 0, 0, 0, time.UTC)
				if d < 1 || date.Year() != y || int(date.Month()) != m || date.Day() != d {
					return
				}
				dateValue += fmt.Sprintf("-%02d", d)
			}
			value = dateValue
		}
		num := func(s string) int { v, _ := strconv.Atoi(s); return v }
		switch kind {
		case 0:
			if m[2] != m[4] {
				continue
			}
			add(num(m[1]), num(m[3]), num(m[5]))
		case 1:
			if m[2] != m[4] {
				continue
			}
			day, month := num(m[1]), num(m[3])
			if day <= 12 && month <= 12 && day != month {
				return end, ""
			}
			if month > 12 {
				day, month = month, day
			}
			add(num(m[5]), month, day)
		case 2:
			month := englishMonth(m[1])
			if month == 0 {
				continue
			}
			if m[2] == "" {
				add(num(m[3]), month, -1)
			} else if num(m[2]) > 0 {
				add(num(m[3]), month, num(m[2]))
			}
		case 3:
			month := englishMonth(m[2])
			if month == 0 {
				continue
			}
			if num(m[1]) > 0 {
				add(num(m[3]), month, num(m[1]))
			}
		case 4:
			add(num(m[1]), num(m[2]), -1)
		}
		// A matched but impossible date falls through to its component numbers.
		return end, value
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
