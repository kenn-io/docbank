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

type Citation struct {
	Text, Err, TextSHA256 string
	AtDocumentStart       bool
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
	figures []normalizedFigure
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
	return extractDecoded(string(util.UnescapePunctuations([]byte(text))))
}

func extractDecoded(text string) []string {
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
	a.figures = normalizedFigures(answer, false)
	if len(a.figures) > 512 || len(a.markers) > 512 {
		return Answer{}, errors.New("answer exceeds figure or marker limit")
	}
	return a, nil
}
func (a Answer) Check(citations []Citation) Report {
	report := Report{Citations: []CitationResult{}, Figures: []Figure{}, UnknownMarkers: []int{}}
	values, unavailable := map[string]bool{}, false
	var normalized []normalizedFigure
	for i, c := range citations {
		status := c.Err
		if status == "" {
			status = "resolved"
			for _, literal := range extract(citationText(&c)) {
				values[literal] = true
				if date := timestampDate(literal); date != "" {
					normalized = append(normalized, normalizedFigure{kind: "date", values: []string{date}})
				}
			}
			for _, value := range normalizedFigures(c.Text, true) {
				if value.kind == "" || value.kind == "bare" {
					value.values = bareNumberValues(value.text)
				}
				values[value.text] = true
				normalized = append(normalized, value)
			}
		} else {
			unavailable = true
		}
		report.Citations = append(report.Citations, CitationResult{Index: i + 1, Status: status, TextSHA256: c.TextSHA256})
	}
	missing := false
	for _, value := range a.figures {
		status := "found"
		if value.kind == "amount" || !values[value.text] {
			status = "missing"
			for _, source := range normalized {
				if value.kind == "date" && source.kind == "date" {
					for _, a := range value.values {
						for _, b := range source.values {
							if a == b {
								status = "found"
							}
						}
					}
				}
				if value.kind == "amount" {
					if source.kind == "" && source.text == value.text {
						status = "found"
					}
					for _, a := range value.values {
						ac, av := amountParts(a)
						if source.kind == "amount" {
							for _, b := range source.values {
								bc, bv := amountParts(b)
								if av == bv {
									if compatibleCurrency(ac, bc) {
										status = "found"
									} else if status != "found" {
										status = "value_only"
									}
								}
							}
						} else if (source.kind == "" || source.kind == "bare") && status != "found" {
							for _, number := range source.values {
								if number == av {
									status = "value_only"
								}
							}
						}
					}
				}
			}
			if status != "found" {
				missing = true
			}
		}
		report.Figures = append(report.Figures, Figure{Text: value.text, Status: status})
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
