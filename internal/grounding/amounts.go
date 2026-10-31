package grounding

import (
	"strings"
	"unicode"

	"golang.org/x/text/currency"
)

func currencyAt(rs []rune, start int) (int, string) {
	if start >= len(rs) {
		return start, ""
	}
	switch rs[start] {
	case '$':
		return start + 1, "USD"
	case '€':
		return start + 1, "EUR"
	case '£':
		return start + 1, "GBP"
	}
	if start+3 <= len(rs) {
		code := string(rs[start : start+3])
		if _, err := currency.ParseISO(code); err == nil && code == strings.ToUpper(code) {
			return start + 3, code
		}
	}
	return start, ""
}

// Amounts use comma groups and a decimal point. Other separator conventions
// remain literal rather than inferring a locale from the currency marker.
func amountValue(s string) string {
	integer, fraction, decimal := strings.Cut(s, ".")
	if decimal && (fraction == "" || len(fraction) > 2) {
		return ""
	}
	for _, r := range fraction {
		if !digit(r) {
			return ""
		}
	}
	if strings.Contains(integer, ",") {
		groups := strings.Split(integer, ",")
		if len(groups[0]) < 1 || len(groups[0]) > 3 {
			return ""
		}
		for _, group := range groups[1:] {
			if len(group) != 3 {
				return ""
			}
		}
		integer = strings.Join(groups, "")
	}
	integer = strings.TrimLeft(integer, "0")
	if integer == "" {
		integer = "0"
	}
	fraction = strings.TrimRight(fraction, "0")
	if fraction != "" {
		return integer + "." + fraction
	}
	return integer
}

func amountAt(rs []rune) (int, string) {
	i := 0
	signed := len(rs) > 0 && strings.ContainsRune("-+±\u2212", rs[0])
	if signed {
		i++
	}
	i, code := currencyAt(rs, i)
	prefix := code != ""
	i = skipSpace(rs, i)
	numberStart := i
	if i >= len(rs) || (!digit(rs[i]) && (rs[i] != '.' || i+1 >= len(rs) || !digit(rs[i+1]))) {
		return 0, ""
	}
	for i < len(rs) && (digit(rs[i]) || separator(rs[i])) {
		i++
	}
	// A terminal ellipsis is sentence punctuation, not a malformed number.
	dots := i
	for dots > numberStart && rs[dots-1] == '.' {
		dots--
	}
	if i-dots >= 3 && dots > numberStart && digit(rs[dots-1]) && (i == len(rs) || unicode.IsSpace(rs[i]) || edge(rs[i])) {
		i = dots
	}
	// Sentence punctuation and whitespace after the number are not part of it.
	for i > numberStart && !digit(rs[i-1]) {
		if i-numberStart >= 2 && separator(rs[i-1]) && separator(rs[i-2]) {
			return 0, ""
		}
		i--
	}
	numberEnd := i
	// A suffix belongs only to an amount without a prefix. Inspect the next
	// token itself, leaving any following figure for its own parse.
	if !prefix {
		start := skipSpace(rs, i)
		end, suffix := currencyAt(rs, start)
		if suffix == "" || end-start != 3 {
			return 0, ""
		}
		i, code = end, suffix
	}
	start := skipSpace(rs, i)
	end := start
	for end < len(rs) && !unicode.IsSpace(rs[end]) {
		end++
	}
	token := []rune(trim(rs[start:end]))
	if markerEnd, marker := currencyAt(token, 0); marker != "" && markerEnd == len(token) {
		if start == i {
			return end, ""
		}
		return i, ""
	}
	if signed {
		return i, ""
	}
	value := amountValue(string(rs[numberStart:numberEnd]))
	if value == "" {
		return 0, ""
	}
	return i, code + " " + value
}
