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
	for _, p := range []struct{ prefix, code string }{{"US$", "USD"}, {"C$", "CAD"}, {"A$", "AUD"}} {
		end := start + len(p.prefix)
		if end <= len(rs) && strings.EqualFold(string(rs[start:end]), p.prefix) {
			return end, p.code
		}
	}
	if unicode.Is(unicode.Sc, rs[start]) {
		return start + 1, string(rs[start])
	}
	if start+3 <= len(rs) {
		code := string(rs[start : start+3])
		if _, err := currency.ParseISO(code); err == nil && code == strings.ToUpper(code) && (start+3 == len(rs) || (!wordRune(rs[start+3]) || digit(rs[start+3]))) {
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
	for _, sep := range []string{",", "\u00a0", "\u202f", "'"} {
		if !strings.Contains(integer, sep) {
			continue
		}
		groups := strings.Split(integer, sep)
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
	for _, r := range integer {
		if !digit(r) {
			return ""
		}
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
	// Recover before a maximal identifier or sign, while skipping any earlier
	// currency-only tokens. Retrying the same long symbol/code run is quadratic.
	i := 0
	negative, signed, positive, plusMinus := false, false, false, false
	classes := []string{}
	prefixEnd, identifierStart, signStart := 0, -1, -1
	failed := func() (int, string) {
		safe := prefixEnd
		if identifierStart >= 0 && identifierStart < safe {
			safe = identifierStart
		}
		if signStart >= 0 && signStart < safe {
			safe = signStart
		}
		if safe <= 0 {
			return 0, ""
		}
		return safe, ""
	}
	for i < len(rs) {
		if rs[i] == '-' || rs[i] == '−' || rs[i] == '+' || rs[i] == '±' {
			if signed {
				return failed()
			}
			negative, signed, positive, plusMinus = rs[i] == '-' || rs[i] == '−', true, rs[i] == '+', rs[i] == '±'
			signStart = i
			i++
			if i < len(rs) && unicode.IsSpace(rs[i]) {
				return failed()
			}
			if len(classes) > 0 {
				prefixEnd = i
			}
			continue
		}
		end, code := currencyAt(rs, i)
		if code == "" {
			break
		}
		// A joined ISO token belongs to the identifier scanner if the amount
		// fails. Inspect only its boundary; probing the remaining token at each
		// currency code would make USD.USD... quadratic.
		if identifierStart < 0 && end-i == 3 && isASCIIAlpha(rs[end-1]) && end < len(rs) && idChar(rs[end]) {
			identifierStart = i
		}
		classes = append(classes, code)
		i = skipSpace(rs, end)
		prefixEnd = i
	}
	numberStart := i
	if i >= len(rs) || (!digit(rs[i]) && (rs[i] != '.' || i+1 >= len(rs) || !digit(rs[i+1]))) {
		return failed()
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
		if i-numberStart >= 2 && strings.ContainsRune(",.'", rs[i-1]) && strings.ContainsRune(",.'", rs[i-2]) {
			return failed()
		}
		i--
	}
	numberEnd := i
	// Separate numeric words can also be grouped amounts in retained HTML.
	// Keep them literal, but leave adjacent dates and identifiers independent.
	spaced := false
	for {
		start := skipSpace(rs, i)
		end := start
		for end < len(rs) && (digit(rs[end]) || separator(rs[end])) {
			end++
		}
		if start == i || start == end || !digit(rs[start]) || !completeTokenTail(rs, end) {
			break
		}
		i, spaced = end, true
	}
	i, classes = suffixCurrencies(rs, i, classes)
	if spaced || len(classes) > 1 {
		return i, ""
	}
	if len(classes) == 0 {
		return failed()
	}
	code := classes[0]
	switch code {
	case "$":
		code = "USD"
	case "€":
		code = "EUR"
	case "£":
		code = "GBP"
	}
	value := amountValue(string(rs[numberStart:numberEnd]))
	if value == "" {
		return failed()
	}
	if negative {
		value = "-" + value
	}
	if positive {
		value = "+" + value
	}
	if plusMinus {
		value = "±" + value
	}
	return i, code + " " + value
}

// Keep only markers that end at a complete token; a partial suffix must not
// discard an amount already parsed before it.
func suffixCurrencies(rs []rune, i int, classes []string) (int, []string) {
	safe, count := i, len(classes)
	for {
		start := skipSpace(rs, i)
		end, code := currencyAt(rs, start)
		if code == "" {
			break
		}
		if end < len(rs) && !digit(rs[end]) && !unicode.IsSpace(rs[end]) && !unicode.Is(unicode.Sc, rs[end]) && !completeTokenTail(rs, end) {
			break
		}
		classes = append(classes, code)
		i = end
		if end == len(rs) || unicode.IsSpace(rs[end]) || edge(rs[end]) && completeTokenTail(rs, end) {
			safe, count = i, len(classes)
		}
	}
	return safe, classes[:count]
}

func completeTokenTail(rs []rune, end int) bool {
	for end < len(rs) && !unicode.IsSpace(rs[end]) {
		if !edge(rs[end]) {
			return false
		}
		end++
	}
	return true
}
