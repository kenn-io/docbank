// Package grounding compares plain-text answer figures with saved citations.
package grounding

import (
	"errors"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

type Citation struct {
	Index                 int
	Text, Err, TextSHA256 string
}
type CitationResult struct {
	Index      int    `json:"index"`
	Status     string `json:"status"`
	TextSHA256 string `json:"text_sha256,omitempty"`
}
type Figure struct {
	Start     int    `json:"start"`
	End       int    `json:"end"`
	Text      string `json:"text"`
	value     string
	Citations []int  `json:"citations"`
	FoundIn   []int  `json:"found_in"`
	Status    string `json:"status"`
}
type Marker struct {
	Index int `json:"index"`
	Start int `json:"start"`
	End   int `json:"end"`
}
type Report struct {
	Status         string           `json:"status"`
	Citations      []CitationResult `json:"citations"`
	Figures        []Figure         `json:"figures"`
	UnknownMarkers []Marker         `json:"unknown_markers"`
}
type marker struct {
	start, end int
	indexes    []int
	adjacent   bool
}
type Answer struct {
	figures []Figure
	markers []marker
}

var controls = regexp.MustCompile(`(?i)(?:https?://|www\.)[^\s<>]+|[\pL\pN._%+\-]+@[\pL\pN.\-]+\.[\pL]{2,}|\[[0-9]{1,2}(?:, *[0-9]{1,2})*\]`)

// cursor decodes punctuation escapes while retaining literal code-point offsets.
// Lookahead copies this small cursor and reads at most four characters.
type cursor struct {
	rs  []rune
	pos int
}

func (c *cursor) peek() rune {
	if c.pos >= len(c.rs) {
		return 0
	}
	r := c.rs[c.pos]
	if r == '\\' && c.pos+1 < len(c.rs) && asciiPunctuation(c.rs[c.pos+1]) {
		r = c.rs[c.pos+1]
	}
	if unicode.Is(unicode.Pd, r) || r == '−' {
		return '-'
	}
	return r
}
func (c *cursor) take() rune {
	r := c.peek()
	if c.rs[c.pos] == '\\' && c.pos+1 < len(c.rs) && asciiPunctuation(c.rs[c.pos+1]) {
		c.pos++
	}
	c.pos++
	return r
}
func asciiPunctuation(r rune) bool {
	return r >= '!' && r <= '/' || r >= ':' && r <= '@' || r >= '[' && r <= '`' || r >= '{' && r <= '~'
}
func word(r rune) bool       { return unicode.IsLetter(r) || unicode.IsDigit(r) || unicode.IsMark(r) }
func affix(r rune) bool      { return unicode.Is(unicode.Sc, r) || r == '%' }
func groupSpace(r rune) bool { return r == ' ' || r == '\u00a0' || r == '\u202f' }
func fold(r rune) rune {
	smallest := r
	for next := unicode.SimpleFold(r); next != r; next = unicode.SimpleFold(next) {
		smallest = min(smallest, next)
	}
	return smallest
}

// atom consumes a complete digit grouping before the caller considers joiners.
func (c *cursor) atom(value *strings.Builder, hasDigit *bool) {
	digits := 0
	for unicode.IsDigit(c.peek()) {
		value.WriteRune(c.take())
		digits++
		*hasDigit = true
	}
	if digits > 0 && digits <= 3 {
		separator := c.peek()
		for c.peek() == ',' && separator == ',' || groupSpace(c.peek()) && groupSpace(separator) {
			next := *c
			next.take()
			groupStart, count := next.pos, 0
			for count < 3 && unicode.IsDigit(next.peek()) {
				next.take()
				count++
			}
			if count != 3 || unicode.IsDigit(next.peek()) {
				break
			}
			value.WriteString(string(c.rs[groupStart:next.pos]))
			*c = next
		}
	}
	for word(c.peek()) {
		r := c.take()
		*hasDigit = *hasDigit || unicode.IsDigit(r)
		value.WriteRune(fold(r))
	}
}

func extract(text string, cited bool) Answer {
	original := []rune(text)
	rs := slices.Clone(original)
	a := Answer{figures: []Figure{}, markers: []marker{}}
	if !cited {
		// Count disjoint UTF-8 spans once, rather than recounting prefixes.
		s := string(rs)
		byteEnd, runeEnd := 0, 0
		for _, span := range controls.FindAllStringIndex(s, -1) {
			start := runeEnd + utf8.RuneCountInString(s[byteEnd:span[0]])
			end := start + utf8.RuneCountInString(s[span[0]:span[1]])
			byteEnd, runeEnd = span[1], end
			if s[span[0]] == '[' {
				if span[1] < len(s) && s[span[1]] == '(' {
					continue
				}
				m := marker{start: start, end: end}
				if len(a.markers) > 0 {
					m.adjacent = strings.TrimSpace(string(original[a.markers[len(a.markers)-1].end:start])) == ""
				}
				for v := range strings.SplitSeq(s[span[0]+1:span[1]-1], ",") {
					n, _ := strconv.Atoi(strings.TrimSpace(v))
					m.indexes = append(m.indexes, n)
				}
				a.markers = append(a.markers, m)
			}
			for i := start; i < end; i++ {
				rs[i] = ' '
			}
		}
	}
	c := cursor{rs: rs}
	previous := rune(0)
	for c.pos < len(rs) {
		start := c.pos
		var value strings.Builder
		hasDigit := false
		// Affixes can surround the sign, as in $-5 and -$5.
		signed := c.peek() == '-' && !word(previous)
		if signed {
			value.WriteRune(c.take())
		}
		for affix(c.peek()) {
			value.WriteRune(c.take())
		}
		if !signed && c.peek() == '-' && !word(previous) {
			value.WriteRune(c.take())
		}
		if !word(c.peek()) {
			if c.pos == start {
				previous = c.take()
			} else {
				previous = rs[c.pos-1]
			}
			continue
		}
		c.atom(&value, &hasDigit)
		for strings.ContainsRune("-_/.", c.peek()) {
			next := c
			joiner := next.take()
			if !word(next.peek()) {
				break
			}
			c = next
			value.WriteRune(joiner)
			c.atom(&value, &hasDigit)
		}
		previous = rs[c.pos-1]
		for affix(c.peek()) {
			previous = c.take()
			value.WriteRune(previous)
		}
		if hasDigit {
			a.figures = append(a.figures, Figure{Start: start, End: c.pos, Text: string(original[start:c.pos]), value: value.String()})
		}
	}
	return a
}
func Prepare(answer string) (Answer, error) {
	a := extract(answer, false)
	count := 0
	for _, m := range a.markers {
		count += len(m.indexes)
	}
	if len(a.figures) > 512 || count > 512 {
		return Answer{}, errors.New("answer exceeds figure or marker limit")
	}
	return a, nil
}

func (a Answer) Check(citations []Citation) Report {
	report := Report{Status: "no_figures", Citations: []CitationResult{}, Figures: []Figure{}, UnknownMarkers: []Marker{}}
	sources, all := map[int]map[string]bool{}, []int{}
	for _, c := range citations {
		all = append(all, c.Index)
		status := c.Err
		if status == "" {
			status = "resolved"
			sources[c.Index] = map[string]bool{}
			for _, f := range extract(c.Text, true).figures {
				sources[c.Index][f.value] = true
			}
		}
		report.Citations = append(report.Citations, CitationResult{Index: c.Index, Status: status, TextSHA256: c.TextSHA256})
	}
	// Merge whitespace-adjacent markers once; figures advance through the groups.
	groups := []marker{}
	for _, m := range a.markers {
		if !m.adjacent {
			groups = append(groups, marker{start: m.start, indexes: []int{}})
		}
		last := &groups[len(groups)-1]
		for _, idx := range m.indexes {
			if idx < 1 || idx > len(citations) {
				report.UnknownMarkers = append(report.UnknownMarkers, Marker{Index: idx, Start: m.start, End: m.end})
			} else if !slices.Contains(last.indexes, idx) {
				last.indexes = append(last.indexes, idx)
			}
		}
	}
	group := 0
	for _, f := range a.figures {
		scope := all
		if len(groups) > 0 {
			for group < len(groups) && groups[group].start < f.End {
				group++
			}
			scope = []int{}
			if group < len(groups) {
				scope = groups[group].indexes
			}
		}
		f.Citations, f.FoundIn = scope, []int{}
		unavailable := len(f.Citations) == 0
		for _, idx := range f.Citations {
			unavailable = unavailable || sources[idx] == nil
			if sources[idx][f.value] {
				f.FoundIn = append(f.FoundIn, idx)
			}
		}
		f.Status = "missing"
		if len(f.FoundIn) > 0 {
			f.Status = "found"
		} else if unavailable {
			f.Status = "unverifiable"
		}
		switch {
		case f.Status == "missing":
			report.Status = "ungrounded"
		case f.Status == "unverifiable" && report.Status != "ungrounded":
			report.Status = "incomplete"
		case report.Status == "no_figures":
			report.Status = "grounded"
		}
		report.Figures = append(report.Figures, f)
	}
	if len(report.UnknownMarkers) > 0 {
		report.Status = "ungrounded"
	}
	return report
}
