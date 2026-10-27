package grounding

import (
	"regexp"
	"strings"
	"unicode"

	"golang.org/x/text/currency"
	"golang.org/x/text/width"
)

type Figure struct {
	Kind     string   `json:"kind"`
	Start    int      `json:"start"`
	End      int      `json:"end"`
	Text     string   `json:"text"`
	Values   []string `json:"values"`
	Status   string   `json:"status"`
	FoundIn  []int    `json:"found_in"`
	identity string
}

type normalized struct {
	runes        []rune
	starts, ends []int
}

func normalize(s string, escapes []bool) normalized {
	original := []rune(s)
	n := normalized{}
	for i := 0; i < len(original); i++ {
		start := i
		if original[i] == '\\' && (escapes == nil || escapes[i]) && i+1 < len(original) && original[i+1] < 128 && (unicode.IsPunct(original[i+1]) || unicode.IsSymbol(original[i+1])) {
			i++
		}
		r := width.Fold.String(string(original[i]))
		for _, c := range r {
			if c == '−' {
				c = '-'
			}
			n.runes = append(n.runes, c)
			n.starts = append(n.starts, start)
			n.ends = append(n.ends, i+1)
		}
	}
	return n
}

var spacedID = regexp.MustCompile(`^[A-Za-z]{1,5} [0-9]{1,2}[A-Za-z]{1,2}$`)
var numericID = regexp.MustCompile(`^[0-9]+(?:-[0-9]+){2,}$|^[0-9]+-[0-9]+$`)

func isASCIIAlpha(r rune) bool { return r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' }
func digit(r rune) bool        { return r >= '0' && r <= '9' }
func wordRune(r rune) bool     { return unicode.IsLetter(r) || unicode.IsMark(r) || unicode.IsNumber(r) }
func idChar(r rune) bool       { return wordRune(r) || strings.ContainsRune("-/._", r) }
func separator(r rune) bool {
	return r == ',' || r == '.' || r == '\u00a0' || r == '\u202f' || r == '\''
}
func horizontalSpace(r rune) bool {
	return r == ' ' || r == '\t' || r == '\u00a0' || r == '\u202f'
}
func ellipsisFigure(rs []rune, start, end int) bool {
	if end <= start {
		return false
	}
	if digit(rs[end-1]) {
		return true
	}
	_, unit := numericUnit(rs[start:end])
	return unit
}

func trimFigureEnd(rs []rune, start, end int, numeric bool) int {
	dots := end
	for dots > start && rs[dots-1] == '.' {
		dots--
	}
	if numeric && end-dots >= 3 && ellipsisFigure(rs, start, dots) {
		end = dots
	}
	for end > start && (strings.ContainsRune("-/. _,", rs[end-1]) || rs[end-1] == '\u00a0' || rs[end-1] == '\u202f' || rs[end-1] == '\'') {
		if numeric && end-start >= 2 && strings.ContainsRune(",.'", rs[end-1]) && strings.ContainsRune(",.'", rs[end-2]) {
			break
		}
		end--
	}
	return end
}
func hasNumericComponent(rs []rune) bool {
	for _, r := range rs {
		if wordRune(r) && !digit(r) {
			return true
		}
	}
	token := string(rs)
	if !numericID.MatchString(token) {
		return false
	}
	digits := 0
	for _, r := range rs {
		if digit(r) {
			digits++
		}
	}
	return strings.Count(token, "-") >= 2 || digits >= 6
}
func numericUnit(token []rune) (int, bool) {
	end := len(token)
	for end > 0 && isASCIIAlpha(token[end-1]) {
		end--
	}
	if end == 0 || len(token)-end == 0 || len(token)-end > 3 {
		return end, false
	}
	for _, r := range token[:end] {
		if !digit(r) && !separator(r) {
			return end, false
		}
	}
	return end, true
}

func classifyNumericToken(token []rune, numeric bool) (int, bool, bool) {
	end, unit := numericUnit(token)
	malformed := false
	if numeric && len(token) > 2 && token[len(token)-1] == '.' && token[len(token)-2] == '.' {
		_, malformed = numericUnit(token[:len(token)-2])
	}
	return end, numeric && unit, malformed
}

func numericPartEnd(rs []rune, start int) int {
	end := start
	for end < len(rs) && (digit(rs[end]) || separator(rs[end])) {
		if rs[end] == '\u00a0' || rs[end] == '\u202f' || rs[end] == '\'' {
			next := end
			for next < len(rs) && (rs[next] == '\u00a0' || rs[next] == '\u202f' || rs[next] == '\'') {
				next++
			}
			if next == len(rs) || !digit(rs[next]) {
				break
			}
			end = next
		}
		end++
	}
	return end
}

func compactCurrencyNumber(rs []rune, start int) bool {
	if start >= len(rs) || !digit(rs[start]) && (rs[start] != '.' || start+1 >= len(rs) || !digit(rs[start+1])) {
		return false
	}
	end := numericPartEnd(rs, start)
	unitEnd := end
	for unitEnd < len(rs) && isASCIIAlpha(rs[unitEnd]) {
		unitEnd++
	}
	if unitEnd > end {
		if _, ok := numericUnit(rs[start:unitEnd]); !ok {
			return false
		}
		end = unitEnd
	}
	for end < len(rs) && idChar(rs[end]) && !wordRune(rs[end]) {
		end++
	}
	return end == len(rs) || !wordRune(rs[end])
}

func extract(s string) []Figure { return extractWithEscapes(s, nil) }

func extractWithEscapes(s string, escapes []bool) []Figure {
	n := normalize(s, escapes)
	rs := n.runes
	original := []rune(s)
	out := []Figure{}
	add := func(kind string, a, b int, values []string) {
		if len(values) == 0 {
			return
		}
		start, end := n.starts[a], n.ends[b-1]
		out = append(out, Figure{Kind: kind, Start: start, End: end, Text: string(original[start:end]), Values: values, FoundIn: []int{}})
	}
	for i := 0; i < len(rs); {
		signEnd := i
		if rs[i] == '-' && (i == 0 || !digit(rs[i-1])) {
			j := i + 1
			for j < len(rs) && horizontalSpace(rs[j]) {
				j++
			}
			k := j
			for k < len(rs) && isASCIIAlpha(rs[k]) {
				k++
			}
			if k < len(rs) && unicode.Is(unicode.Sc, rs[k]) {
				j = k
			}
			if j < len(rs) && unicode.Is(unicode.Sc, rs[j]) {
				j++
			} else if j+3 < len(rs) && (horizontalSpace(rs[j+3]) || compactCurrencyNumber(rs, j+3)) {
				if _, err := currency.ParseISO(string(rs[j : j+3])); err == nil {
					j += 3
				}
			}
			for j < len(rs) && horizontalSpace(rs[j]) {
				j++
			}
			if j < len(rs) && (digit(rs[j]) || rs[j] == '.' && j+1 < len(rs) && digit(rs[j+1])) {
				signEnd = j
			}
		}
		leadingDecimal := rs[i] == '.' && i+1 < len(rs) && digit(rs[i+1])
		if !wordRune(rs[i]) && !leadingDecimal && signEnd == i {
			i++
			continue
		}
		a := i
		if signEnd != i {
			i = signEnd
		}
		start := i
		numericStart := digit(rs[start]) || rs[start] == '.' && start+1 < len(rs) && digit(rs[start+1])
		for i < len(rs) && idChar(rs[i]) {
			i++
		}
		// Extend short model prefixes when the suffix includes letters.
		if i < len(rs) && rs[i] == ' ' && i+1 < len(rs) && digit(rs[i+1]) {
			j := i + 1
			for j < len(rs) && wordRune(rs[j]) {
				j++
			}
			_, currencyErr := currency.ParseISO(string(rs[start:i]))
			k := j
			for k < len(rs) && (strings.ContainsRune("-/._,", rs[k]) || rs[k] == '\u00a0' || rs[k] == '\u202f' || rs[k] == '\'') {
				k++
			}
			hasContinuation := k > j && k < len(rs) && wordRune(rs[k])
			if currencyErr != nil && !hasContinuation && spacedID.MatchString(string(rs[start:j])) {
				i = j
			}
		}
		// Numeric punctuation and digit-group separators belong to one maximal run.
		if numericStart && !hasNumericComponent(rs[start:i]) {
			for i < len(rs) && (digit(rs[i]) || separator(rs[i])) {
				if rs[i] == '\u00a0' || rs[i] == '\u202f' || rs[i] == '\'' {
					j := i
					for j < len(rs) && (rs[j] == '\u00a0' || rs[j] == '\u202f' || rs[j] == '\'') {
						j++
					}
					if j == len(rs) || !digit(rs[j]) {
						break
					}
					i = j
				}
				i++
			}
			for i < len(rs) && idChar(rs[i]) {
				i++
			}
		}
		// Trim punctuation that cannot end an identifier or numeric token.
		i = trimFigureEnd(rs, start, i, numericStart)
		if i == start {
			i = start + 1
			continue
		}
		token := string(rs[start:i])
		letters, digits := 0, 0
		unicodeWord := false
		for _, r := range token {
			if r > 127 && wordRune(r) {
				unicodeWord = true
			}
			if isASCIIAlpha(r) {
				letters++
			}
			if digit(r) {
				digits++
			}
		}
		tr := []rune(token)
		numberEnd, unit, malformed := classifyNumericToken(tr, numericStart)
		if digits == 0 && unicodeWord {
			continue
		}
		isID := digits > 0 && !malformed && (unicodeWord || letters > 0 && !unit)
		if numericID.MatchString(token) {
			groups := strings.Count(token, "-") + 1
			isID = groups >= 3 || digits >= 6
			if !isID {
				i = start + strings.IndexRune(token, '-')
				token = string(rs[start:i])
			}
		}
		identity := digitIdentity(rs, a, start, i, isID)
		if isID {
			var b strings.Builder
			for _, r := range token {
				if wordRune(r) {
					b.WriteRune(simpleFold(r))
				}
			}
			add("identifier", a, i, []string{b.String()})
			out[len(out)-1].identity = identity
			continue
		}
		if letters > 0 && !unit && !malformed {
			continue
		}
		if unit {
			number := string(tr[:numberEnd])
			if a < start {
				number = "-" + number
			}
			values := numberValues(number)
			if len(values) == 0 {
				values = []string{number}
			}
			add("number", a, i, values)
			continue
		}
		if a < start {
			token = "-" + token
		}
		values := numberValues(token)
		if len(values) == 0 {
			values = []string{token}
		}
		add("number", a, i, values)
		out[len(out)-1].identity = identity
	}
	return out
}
func simpleFold(r rune) rune {
	canonical := r
	for next := unicode.SimpleFold(r); next != r; next = unicode.SimpleFold(next) {
		if next < canonical {
			canonical = next
		}
	}
	return unicode.ToLower(canonical)
}

func digitIdentity(rs []rune, a, start, end int, isID bool) string {
	if a != start {
		return ""
	}
	j := a - 1
	for j >= 0 && horizontalSpace(rs[j]) {
		j--
	}
	if j >= 0 && (rs[j] == '+' || rs[j] == '-' || unicode.Is(unicode.Sc, rs[j])) {
		return ""
	}
	k := j
	for k >= 0 && isASCIIAlpha(rs[k]) {
		k--
	}
	if j-k == 3 {
		if _, err := currency.ParseISO(string(rs[k+1 : j+1])); err == nil {
			return ""
		}
	}
	j = end
	for j < len(rs) && horizontalSpace(rs[j]) {
		j++
	}
	if j < len(rs) && unicode.Is(unicode.Sc, rs[j]) {
		return ""
	}
	k = j
	for k < len(rs) && isASCIIAlpha(rs[k]) {
		k++
	}
	if k-j == 3 {
		if _, err := currency.ParseISO(string(rs[j:k])); err == nil {
			return ""
		}
	}
	var b strings.Builder
	for _, r := range rs[start:end] {
		if digit(r) {
			b.WriteRune(r)
		} else if r != '-' || !isID {
			return ""
		}
	}
	return b.String()
}

func canonical(integer, fraction string, neg bool) string {
	integer = strings.TrimLeft(integer, "0")
	if integer == "" {
		integer = "0"
	}
	fraction = strings.TrimRight(fraction, "0")
	if fraction != "" {
		integer += "." + fraction
	}
	if neg && integer != "0" {
		integer = "-" + integer
	}
	return integer
}
func numberValues(s string) []string {
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")
	if s == "" {
		return nil
	}
	for _, r := range s {
		if !digit(r) && !separator(r) {
			return nil
		}
	}
	if !strings.ContainsAny(s, ",.\u00a0\u202f'") {
		if len(s) >= 2 && s[0] == '0' {
			if neg {
				return []string{"-" + s}
			}
			return []string{s}
		}
		return []string{canonical(s, "", neg)}
	}
	// Non-decimal grouping separators must delimit groups of three.
	for _, sep := range []string{"\u00a0", "\u202f", "'"} {
		if strings.Contains(s, sep) {
			decimalSuffix := ""
			if pos := strings.IndexAny(s, ",."); pos >= 0 {
				decimalSuffix = s[pos:]
				s = s[:pos]
			}
			parts := strings.Split(s, sep)
			for i, p := range parts {
				if len(p) < 1 || len(p) > 3 || i > 0 && len(p) != 3 {
					return nil
				}
				for _, r := range p {
					if !digit(r) {
						return nil
					}
				}
			}
			s = strings.Join(parts, "") + decimalSuffix
		}
	}
	// Group separators are permitted only in the validated integer groups.
	for _, r := range s {
		if !digit(r) && r != ',' && r != '.' {
			return nil
		}
	}
	commas, dots := strings.Count(s, ","), strings.Count(s, ".")
	if commas > 0 && dots > 0 {
		decimal := "."
		if strings.LastIndex(s, ",") > strings.LastIndex(s, ".") {
			decimal = ","
		}
		group := ","
		if decimal == "," {
			group = "."
		}
		parts := strings.Split(s, decimal)
		if len(parts) != 2 {
			return nil
		}
		if parts[1] == "" {
			return nil
		}
		for _, r := range parts[1] {
			if !digit(r) {
				return nil
			}
		}
		g := strings.Split(parts[0], group)
		if len(g[0]) < 1 || len(g[0]) > 3 {
			return nil
		}
		for _, p := range g[1:] {
			if len(p) != 3 {
				return nil
			}
		}
		return []string{canonical(strings.Join(g, ""), parts[1], neg)}
	}
	sep := ","
	if dots > 0 {
		sep = "."
	}
	parts := strings.Split(s, sep)
	if len(parts) == 1 {
		return []string{canonical(s, "", neg)}
	}
	grouped := len(parts[0]) >= 1 && len(parts[0]) <= 3
	for _, p := range parts[1:] {
		if len(p) != 3 {
			grouped = false
		}
	}
	if len(parts) > 2 {
		if !grouped {
			return nil
		}
		return []string{canonical(strings.Join(parts, ""), "", neg)}
	}
	if parts[1] == "" {
		return nil
	}
	dec := canonical(parts[0], parts[1], neg)
	if grouped {
		v := canonical(strings.Join(parts, ""), "", neg)
		if v != dec {
			return []string{v, dec}
		}
	}
	return []string{dec}
}
