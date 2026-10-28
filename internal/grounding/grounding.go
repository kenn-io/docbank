// Package grounding compares answer figures with saved citations.
package grounding

import (
	"errors"
	"regexp"
	"strconv"
	"strings"
	"unicode"
)

type Citation struct{ Text, Err, TextSHA256 string }
type CitationResult struct {
	Index      int    `json:"index"`
	Status     string `json:"status"`
	TextSHA256 string `json:"text_sha256,omitempty"`
}
type Figure struct {
	Text   string `json:"text"`
	Status string `json:"status"`
}
type Report struct {
	Status         string           `json:"status"`
	Citations      []CitationResult `json:"citations"`
	Figures        []Figure         `json:"figures"`
	UnknownMarkers []int            `json:"unknown_markers"`
}
type Answer struct {
	figures []string
	markers []int
}

var markers = regexp.MustCompile(`\[[0-9]{1,6}\]`)
var escapes = regexp.MustCompile(`\\([[:punct:]])`)

func edge(r rune) bool {
	return !unicode.IsLetter(r) && !unicode.IsDigit(r) && !unicode.IsMark(r) && !unicode.Is(unicode.Sc, r) && r != '%'
}

// delimiters never occur inside a figure; they cover dashes and Markdown and HTML syntax.
const delimiters = "\u2014\u2015[]()<>|*~`=;"

func split(r rune) bool    { return unicode.IsSpace(r) || strings.ContainsRune(delimiters, r) }
func grouping(r rune) bool { return r == ' ' || r == '\u00a0' || r == '\u202f' }

// amount returns the trailing digit count of a token that so far holds only digits after an
// optional leading sign or currency symbol, or -1 once it holds anything else.
func amount(n int, r rune, first bool) int {
	switch {
	case n >= 0 && unicode.IsDigit(r):
		return n + 1
	case first && (strings.ContainsRune("+-\u2212", r) || unicode.Is(unicode.Sc, r)):
		return 0
	}
	return -1
}

// group reports exactly three digits starting at i.
func group(rs []rune, i int) bool {
	n := 0
	for i+n < len(rs) && n < 4 && unicode.IsDigit(rs[i+n]) {
		n++
	}
	return n == 3
}

// leading skips removable punctuation before a token's meaningful prefix.
func leading(rs []rune, i int) int {
	for i < len(rs) && !split(rs[i]) && edge(rs[i]) {
		if strings.ContainsRune("-.\u2212", rs[i]) && i+1 < len(rs) && (unicode.IsDigit(rs[i+1]) || unicode.Is(unicode.Sc, rs[i+1])) {
			break
		}
		i++
	}
	return i
}

// trim drops edge punctuation, keeping a sign or decimal point before a digit or currency symbol.
func trim(token []rune) string {
	return strings.TrimRightFunc(string(token[leading(token, 0):]), edge)
}

func extract(text string) []string {
	rs := []rune(escapes.ReplaceAllString(text, "$1"))
	figures := []string{}
	for i := 0; i < len(rs); i++ {
		start, n := leading(rs, i), 0
		for i = start; i < len(rs); i++ {
			r := rs[i]
			// A single grouping space joins a three-digit group to an amount.
			if grouping(r) && n >= 1 && n <= 3 && group(rs, i+1) {
				i, n = i+3, 3
				continue
			}
			// A - or / before a lowercase letter ends an amount, as in 5-year.
			if split(r) || n > 0 && (r == '-' || r == '/') && i+1 < len(rs) && unicode.IsLower(rs[i+1]) {
				break
			}
			n = amount(n, r, i == start)
		}
		if value := trim(rs[start:i]); strings.ContainsFunc(value, unicode.IsDigit) {
			figures = append(figures, value)
		}
	}
	return figures
}
func Prepare(answer string) (Answer, error) {
	a := Answer{markers: []int{}}
	answer = markers.ReplaceAllStringFunc(answer, func(m string) string {
		n, _ := strconv.Atoi(m[1 : len(m)-1])
		a.markers = append(a.markers, n)
		return "\n"
	})
	a.figures = extract(answer)
	if len(a.figures) > 512 || len(a.markers) > 512 {
		return Answer{}, errors.New("answer exceeds figure or marker limit")
	}
	return a, nil
}
func (a Answer) Check(citations []Citation) Report {
	report := Report{Citations: []CitationResult{}, Figures: []Figure{}, UnknownMarkers: []int{}}
	values, unavailable := map[string]bool{}, false
	for i, c := range citations {
		status := c.Err
		if status == "" {
			status = "resolved"
			for _, value := range extract(c.Text) {
				values[value] = true
			}
		} else {
			unavailable = true
		}
		report.Citations = append(report.Citations, CitationResult{Index: i + 1, Status: status, TextSHA256: c.TextSHA256})
	}
	missing := false
	for _, value := range a.figures {
		status := "found"
		if !values[value] {
			status, missing = "missing", true
		}
		report.Figures = append(report.Figures, Figure{Text: value, Status: status})
	}
	for _, n := range a.markers {
		if n < 1 || n > len(citations) {
			report.UnknownMarkers = append(report.UnknownMarkers, n)
		}
	}
	switch {
	case len(report.UnknownMarkers) > 0 || missing && !unavailable:
		report.Status = "ungrounded"
	case unavailable:
		report.Status = "incomplete"
	case len(a.figures) == 0:
		report.Status = "no_figures"
	default:
		report.Status = "grounded"
	}
	return report
}
