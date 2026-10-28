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
	Kind      string   `json:"kind"`
	Start     int      `json:"start"`
	End       int      `json:"end"`
	Text      string   `json:"text"`
	Values    []string `json:"values"`
	Citations []int    `json:"citations"`
	FoundIn   []int    `json:"found_in"`
	Status    string   `json:"status"`
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
}
type Answer struct {
	figures []Figure
	markers []marker
}

var (
	ignored       = regexp.MustCompile(`(?i)(?:https?://|www\.)[^\s<>]+|[\pL\pN._%+\-]+@[\pL\pN.\-]+\.[\pL]{2,}`)
	ordinal       = regexp.MustCompile(`(?m)^[\t ]*[0-9]+[.)]\s`)
	markerPattern = regexp.MustCompile(`\[[0-9]{1,2}(?:, *[0-9]{1,2})*\]`)
	number        = regexp.MustCompile(`^[-−]?(?:[0-9]+|[0-9]{1,3}(?:,[0-9]{3})+|[0-9]{1,3}(?: [0-9]{3})+)(?:\.[0-9]+)?$|^[-−]?\.[0-9]+$`)
	groupedPrefix = regexp.MustCompile(`^[0-9]{1,3}(?: [0-9]{3})*$`)
)

func extract(text string, cited bool) Answer {
	if cited {
		rs := []rune(text)
		out := make([]rune, 0, len(rs))
		for i := 0; i < len(rs); i++ {
			if rs[i] == '\\' && i+1 < len(rs) && rs[i+1] < 128 && (unicode.IsPunct(rs[i+1]) || strings.ContainsRune("$+<=>^`|~", rs[i+1])) {
				i++
			}
			out = append(out, rs[i])
		}
		text = string(out)
	}
	rs := []rune(text)
	for i, r := range rs {
		if unicode.IsSpace(r) && r != '\n' && r != '\r' {
			rs[i] = ' '
		}
	}
	// Mask without moving the answer's code-point offsets.
	mask := func(re *regexp.Regexp) {
		s := string(rs)
		for _, span := range re.FindAllStringIndex(s, -1) {
			start := utf8.RuneCountInString(s[:span[0]])
			for i := start; i < start+utf8.RuneCountInString(s[span[0]:span[1]]); i++ {
				rs[i] = ' '
			}
		}
	}
	mask(ignored)
	mask(ordinal)
	for i, r := range rs {
		if unicode.IsSpace(r) {
			rs[i] = ' '
		}
	}
	original := []rune(text)
	a := Answer{figures: []Figure{}, markers: []marker{}}
	s := string(rs)
	for _, span := range markerPattern.FindAllStringIndex(s, -1) {
		if span[1] < len(s) && s[span[1]] == '(' {
			continue
		}
		start := utf8.RuneCountInString(s[:span[0]])
		end := start + utf8.RuneCountInString(s[span[0]:span[1]])
		m := marker{start: start, end: end}
		for v := range strings.SplitSeq(s[span[0]+1:span[1]-1], ",") {
			n, _ := strconv.Atoi(strings.TrimSpace(v))
			m.indexes = append(m.indexes, n)
		}
		a.markers = append(a.markers, m)
		for i := start; i < end; i++ {
			rs[i] = ' '
		}
	}
	for i := 0; i < len(rs); {
		start := i
		if rs[i] == '-' || rs[i] == '−' || rs[i] == '+' {
			i++
		}
		for i < len(rs) && unicode.Is(unicode.Sc, rs[i]) {
			i++
		}
		numberStart := i
		if i+1 < len(rs) && rs[i] == '.' && digit(rs[i+1]) {
			i++
		}
		if i >= len(rs) || !word(rs[i]) {
			i = start + 1
			continue
		}
		for i < len(rs) {
			if word(rs[i]) {
				i++
				continue
			}
			if i+1 < len(rs) && word(rs[i-1]) && word(rs[i+1]) && (strings.ContainsRune("-/_.", rs[i]) || rs[i] == ',' && digit(rs[i-1]) && digit(rs[i+1])) {
				i++
				continue
			}
			if rs[i] == ' ' && groupedPrefix.MatchString(string(rs[numberStart:i])) && i+3 < len(rs) && digit(rs[i+1]) && digit(rs[i+2]) && digit(rs[i+3]) && (i+4 == len(rs) || !word(rs[i+4])) {
				i += 4
				continue
			}
			break
		}
		token := string(rs[start:i])
		if !strings.ContainsFunc(token, unicode.IsDigit) {
			continue
		}
		kind, value := "identifier", strings.Map(func(r rune) rune {
			smallest := r
			for next := unicode.SimpleFold(r); next != r; next = unicode.SimpleFold(next) {
				if next < smallest {
					smallest = next
				}
			}
			return smallest
		}, token)
		if number.MatchString(token) {
			kind = "number"
			value = strings.ReplaceAll(strings.ReplaceAll(strings.ReplaceAll(token, ",", ""), " ", ""), "−", "-")
			sign := ""
			if strings.HasPrefix(value, "-") {
				sign, value = "-", value[1:]
			}
			if strings.HasPrefix(value, ".") {
				value = "0" + value
			}
			if strings.Contains(value, ".") {
				value = strings.TrimRight(strings.TrimRight(value, "0"), ".")
			}
			value = sign + value
		}
		a.figures = append(a.figures, Figure{Kind: kind, Start: start, End: i, Text: string(original[start:i]), Values: []string{value}})
	}
	return a
}
func digit(r rune) bool { return r >= '0' && r <= '9' }
func word(r rune) bool  { return unicode.IsLetter(r) || unicode.IsDigit(r) || unicode.IsMark(r) }

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
	report := Report{Status: "grounded", Citations: []CitationResult{}, Figures: []Figure{}, UnknownMarkers: []Marker{}}
	sources := map[int]map[string]bool{}
	all := []int{}
	for _, c := range citations {
		all = append(all, c.Index)
		status := "resolved"
		if c.Err != "" {
			status = c.Err
			report.Status = "incomplete"
		}
		report.Citations = append(report.Citations, CitationResult{Index: c.Index, Status: status, TextSHA256: c.TextSHA256})
		if c.Err != "" {
			continue
		}
		sources[c.Index] = map[string]bool{}
		for _, f := range extract(c.Text, true).figures {
			sources[c.Index][f.Kind+"\x00"+f.Values[0]] = true
		}
	}
	for _, m := range a.markers {
		for _, idx := range m.indexes {
			if !slices.Contains(all, idx) {
				report.UnknownMarkers = append(report.UnknownMarkers, Marker{Index: idx, Start: m.start, End: m.end})
			}
		}
	}
	for _, f := range a.figures {
		f.Citations, f.FoundIn = []int{}, []int{}
		if len(a.markers) == 0 {
			f.Citations = slices.Clone(all)
		} else {
			for _, m := range a.markers {
				if m.start < f.End {
					continue
				}
				for _, idx := range m.indexes {
					if slices.Contains(all, idx) && !slices.Contains(f.Citations, idx) {
						f.Citations = append(f.Citations, idx)
					}
				}
				break
			}
		}
		unavailable := len(f.Citations) == 0
		for _, idx := range f.Citations {
			if sources[idx] == nil {
				unavailable = true
			}
			if sources[idx][f.Kind+"\x00"+f.Values[0]] {
				f.FoundIn = append(f.FoundIn, idx)
			}
		}
		f.Status = "missing"
		if len(f.FoundIn) > 0 {
			f.Status = "found"
		} else if unavailable {
			f.Status = "unverifiable"
		}
		if f.Status == "missing" {
			report.Status = "ungrounded"
		} else if f.Status == "unverifiable" && report.Status != "ungrounded" {
			report.Status = "incomplete"
		}
		report.Figures = append(report.Figures, f)
	}
	if len(report.UnknownMarkers) > 0 {
		report.Status = "ungrounded"
	}
	if report.Status == "grounded" && len(report.Figures) == 0 {
		report.Status = "no_figures"
	}
	return report
}
