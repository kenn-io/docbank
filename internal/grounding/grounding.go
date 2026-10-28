// Package grounding compares answer figures with saved citations.
package grounding

import (
	"errors"
	"regexp"
	"strconv"
	"strings"
	"unicode"
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

var markers = regexp.MustCompile(`\[[0-9]{1,2}\]`)
var escapes = regexp.MustCompile(`\\([[:punct:]])`)

func edge(r rune) bool {
	return !unicode.IsLetter(r) && !unicode.IsDigit(r) && !unicode.IsMark(r) && !unicode.Is(unicode.Sc, r) && r != '%'
}
func split(r rune) bool { return unicode.IsSpace(r) || r == '\u2014' || r == '\u2015' }

func extract(text string) []string {
	rs := []rune(escapes.ReplaceAllString(text, "$1"))
	figures := []string{}
	for i := 0; i < len(rs); {
		start, digits := i, 0
		for i < len(rs) {
			r := rs[i]
			if split(r) {
				// Only a single grouping space can join a three-digit group.
				end := i + 1
				for end < len(rs) && end < i+5 && unicode.IsDigit(rs[end]) {
					end++
				}
				if (r == ' ' || r == '\u00a0' || r == '\u202f') && digits >= 1 && digits <= 3 && end == i+4 {
					i, digits = end, 3
					continue
				}
				break
			}
			if unicode.IsDigit(r) {
				digits++
			} else {
				digits = 0
			}
			i++
		}
		token := rs[start:i]
		for len(token) > 0 && edge(token[0]) {
			if (token[0] == '-' || token[0] == '.') && len(token) > 1 && (unicode.IsDigit(token[1]) || unicode.Is(unicode.Sc, token[1])) {
				break
			}
			token = token[1:]
		}
		value := strings.TrimRightFunc(string(token), edge)
		if strings.ContainsFunc(value, unicode.IsDigit) {
			figures = append(figures, value)
		}
		i++
	}
	return figures
}
func Prepare(answer string) (Answer, error) {
	a := Answer{markers: []int{}}
	answer = markers.ReplaceAllStringFunc(answer, func(m string) string {
		n, _ := strconv.Atoi(m[1 : len(m)-1])
		a.markers = append(a.markers, n)
		return ""
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
	for _, c := range citations {
		status := c.Err
		if status == "" {
			status = "resolved"
			for _, value := range extract(c.Text) {
				values[value] = true
			}
		} else {
			unavailable = true
		}
		report.Citations = append(report.Citations, CitationResult{Index: c.Index, Status: status, TextSHA256: c.TextSHA256})
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
