// Package grounding compares answer figures with saved citations.
package grounding

import (
	"errors"
	"regexp"
	"strconv"
	"strings"
	"unicode"

	"github.com/yuin/goldmark/util"
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

var markers = regexp.MustCompile(`\[[0-9]{1,3}\]`)

func edge(r rune) bool {
	return !unicode.IsLetter(r) && !unicode.IsDigit(r) && !unicode.IsMark(r) && !unicode.Is(unicode.Sc, r) && r != '%'
}

// delimiters never occur inside a figure; they cover dashes and Markdown and HTML syntax.
const delimiters = "\u2014\u2015[]()<>|*~`=;"

func split(r rune) bool { return unicode.IsSpace(r) || strings.ContainsRune(delimiters, r) }

// trim drops edge punctuation, keeping a sign or decimal point before a digit or currency symbol.
func trim(rs []rune) string {
	i := 0
	for i < len(rs) && edge(rs[i]) {
		if strings.ContainsRune("-+±.\u2212", rs[i]) && i+1 < len(rs) && (unicode.IsDigit(rs[i+1]) || unicode.Is(unicode.Sc, rs[i+1])) {
			break
		}
		if strings.ContainsRune("-+±\u2212", rs[i]) && i+2 < len(rs) && rs[i+1] == '.' && unicode.IsDigit(rs[i+2]) {
			break
		}
		i++
	}
	return strings.TrimRightFunc(string(rs[i:]), edge)
}

func extract(text string) []string {
	text = string(util.UnescapePunctuations([]byte(text)))
	figures := []string{}
	for _, token := range strings.FieldsFunc(text, split) {
		if value := trim([]rune(token)); strings.ContainsFunc(value, unicode.IsDigit) {
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
