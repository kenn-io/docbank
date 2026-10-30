package grounding

import (
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

var datePatterns = []*regexp.Regexp{
	regexp.MustCompile(`^([0-9]{4})([-/])([0-9]{2})([-/])([0-9]{2})`),
	regexp.MustCompile(`^([0-9]{2})([/.])([0-9]{2})([/.])([0-9]{4})`),
	regexp.MustCompile(`(?i)^([A-Za-z]+\.?)[ \t\x{00a0}\x{202f}]+(?:([0-9]{1,2}),?[ \t\x{00a0}\x{202f}]+)?([0-9]{4})`),
	regexp.MustCompile(`(?i)^([0-9]{1,2})[ \t\x{00a0}\x{202f}]+([A-Za-z]+\.?)[ \t\x{00a0}\x{202f}]+([0-9]{4})`),
	regexp.MustCompile(`^([0-9]{4})-([0-9]{2})`),
}

// Token boundaries keep dates and amounts out of identifiers and longer values.
func figureBoundary(rs []rune, end int) bool {
	if end == len(rs) {
		return true
	}
	r := rs[end]
	if wordRune(r) {
		return false
	}
	if r == '_' {
		next := end
		for next < len(rs) && rs[next] == '_' {
			next++
		}
		return next == len(rs) || !wordRune(rs[next])
	}
	if (r == '\u00a0' || r == '\u202f') && end+1 < len(rs) && digit(rs[end+1]) {
		return false
	}
	if strings.ContainsRune("-/.,'", r) {
		next := end + 1
		for next < len(rs) && strings.ContainsRune("-/.,'", rs[next]) {
			next++
		}
		if next < len(rs) && wordRune(rs[next]) {
			return false
		}
	}
	return true
}

func iso8601TimeContinuation(rs []rune, start int) bool {
	if start+5 >= len(rs) || (rs[start] != 'T' && rs[start] != 't') {
		return false
	}
	return digit(rs[start+1]) && digit(rs[start+2]) && rs[start+3] == ':' && digit(rs[start+4]) && digit(rs[start+5])
}

func dateAt(rs []rune) (int, []string) {
	text := string(rs[:min(len(rs), 64)])
	for kind, pattern := range datePatterns {
		m := pattern.FindStringSubmatch(text)
		if m == nil {
			continue
		}
		end := len([]rune(m[0]))
		if !figureBoundary(rs, end) && !iso8601TimeContinuation(rs, end) {
			continue
		}
		values := []string{}
		add := func(y, m, d int) {
			if y < 1 || m < 1 || m > 12 {
				return
			}
			value := fmt.Sprintf("%04d-%02d", y, m)
			if d != -1 {
				date := time.Date(y, time.Month(m), d, 0, 0, 0, 0, time.UTC)
				if d < 1 || date.Year() != y || int(date.Month()) != m || date.Day() != d {
					return
				}
				value += fmt.Sprintf("-%02d", d)
			}
			if slices.Contains(values, value) {
				return
			}
			values = append(values, value)
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
			add(num(m[5]), num(m[3]), num(m[1]))
			add(num(m[5]), num(m[1]), num(m[3]))
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
		return end, values
	}
	return 0, nil
}

func englishMonth(s string) int {
	s = strings.ToLower(s)
	if strings.HasSuffix(s, ".") {
		if len(s) != 4 {
			return 0
		}
		s = strings.TrimSuffix(s, ".")
	}
	for i, month := range []string{"january", "february", "march", "april", "may", "june", "july", "august", "september", "october", "november", "december"} {
		if s == month || s == month[:3] {
			return i + 1
		}
	}
	return 0
}
