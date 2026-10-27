// Package grounding compares figures in an answer with cited text.
package grounding

import (
	"errors"
	"strconv"
	"unicode"
	"unicode/utf8"
)

type Citation struct {
	Index      int
	Text       string
	Err        string
	TextSHA256 string
}
type CitationResult struct {
	Index      int    `json:"index"`
	Status     string `json:"status"`
	TextSHA256 string `json:"text_sha256,omitempty"`
}
type Report struct {
	Status    string           `json:"status"`
	Citations []CitationResult `json:"citations"`
	Claims    []Claim          `json:"claims"`
}
type Claim struct {
	Start          int      `json:"start"`
	End            int      `json:"end"`
	Citations      []int    `json:"citations"`
	UnknownMarkers []int    `json:"unknown_markers"`
	Verdict        string   `json:"verdict"`
	Figures        []Figure `json:"figures"`
}
type marker struct {
	start, end int
	indexes    []int
	overflow   bool
}

func markdownEscaped(rs []rune, at int) bool {
	backslashes := 0
	for i := at - 1; i >= 0 && rs[i] == '\\'; i-- {
		backslashes++
	}
	return backslashes%2 != 0
}

func markers(s string) []marker { return scanMarkers(analyzeMarkdown(s).controls) }

func scanMarkers(rs []rune) []marker {
	out := []marker{}
	for i := 0; i < len(rs); i++ {
		if rs[i] != '[' || markdownEscaped(rs, i) {
			continue
		}
		if i+1 >= len(rs) || !digit(rs[i+1]) {
			continue
		}
		m := marker{start: i}
		j := i + 1
		for {
			start := j
			for j < len(rs) && digit(rs[j]) {
				j++
			}
			if start == j {
				break
			}
			v, err := strconv.Atoi(string(rs[start:j]))
			if err != nil {
				m.overflow = true
			} else {
				m.indexes = append(m.indexes, v)
			}
			for j < len(rs) && (rs[j] == ' ' || rs[j] == '\t') {
				j++
			}
			if j < len(rs) && rs[j] == ',' {
				j++
				for j < len(rs) && (rs[j] == ' ' || rs[j] == '\t') {
					j++
				}
				continue
			}
			if j < len(rs) && rs[j] == ']' && (j+1 == len(rs) || rs[j+1] != '(') {
				m.end = j + 1
				out = append(out, m)
				i = j
			}
			break
		}
	}
	return out
}
func maskedFigures(source markdownSource, ms []marker) []Figure {
	rs := append([]rune(nil), source.structural...)
	for _, m := range ms {
		for i := m.start; i < m.end; i++ {
			rs[i] = ' '
		}
	}
	projected := source.project(rs)
	f := extractWithEscapes(string(projected.runes), projected.escapePolicy(source.literal))
	for i := range f {
		f[i].Start, f[i].End = projected.positions[f[i].Start], projected.ends[f[i].End-1]
		f[i].Text = string(source.original[f[i].Start:f[i].End])
	}
	return f
}
func projectedClaimSpans(source markdownSource, fs []Figure, ms []marker) [][2]int {
	p := source.project([]rune(source.claimText()))
	figures := append([]Figure(nil), fs...)
	for i := range figures {
		figures[i].Start, figures[i].End = p.prefix[figures[i].Start], p.prefix[figures[i].End]
	}
	markers := append([]marker(nil), ms...)
	for i := range markers {
		markers[i].start, markers[i].end = p.prefix[markers[i].start], p.prefix[markers[i].end]
	}
	spans := claimSpans(string(p.runes), figures, markers)
	for i := range spans {
		a, b := p.positions[spans[i][0]], p.ends[spans[i][1]-1]
		for a > 0 && source.omitted[a-1] && (i == 0 || a-1 >= spans[i-1][1]) {
			a--
		}
		for b < len(source.original) && source.omitted[b] {
			b++
		}
		spans[i] = [2]int{a, b}
	}
	return spans
}

func startsMarker(ms []marker, at int) bool {
	for _, m := range ms {
		if m.start == at {
			return true
		}
	}
	return false
}
func sentenceEnd(rs []rune, i int, ms []marker) int {
	if rs[i] == '\n' || rs[i] == '\r' {
		return i + 1
	}
	if rs[i] != '.' && rs[i] != '!' && rs[i] != '?' {
		return 0
	}
	end := i + 1
	for end < len(rs) && (rs[end] == '\'' || rs[end] == '"' || unicode.Is(unicode.Pe, rs[end]) || unicode.Is(unicode.Pf, rs[end])) {
		end++
	}
	if end == len(rs) || unicode.IsSpace(rs[end]) || startsMarker(ms, end) {
		return end
	}
	return 0
}
func claimSpans(s string, fs []Figure, ms []marker) [][2]int {
	rs := []rune(s)
	spans := [][2]int{}
	start := 0
	inside := func(i int) bool {
		for _, f := range fs {
			if f.Start <= i && i < f.End {
				return true
			}
		}
		return false
	}
	for i := 0; i < len(rs); i++ {
		e := sentenceEnd(rs, i, ms)
		if e == 0 || inside(i) {
			continue
		}
		// Immediately following markers belong to the preceding sentence.
		if rs[i] != '\n' && rs[i] != '\r' {
			for {
				j := e
				for j < len(rs) && (rs[j] == ' ' || rs[j] == '\t') {
					j++
				}
				found := false
				for _, m := range ms {
					if m.start == j {
						e = m.end
						found = true
						break
					}
				}
				if !found {
					break
				}
			}
		}
		a, b := start, e
		for a < b && unicode.IsSpace(rs[a]) {
			a++
		}
		for b > a && unicode.IsSpace(rs[b-1]) {
			b--
		}
		if a < b {
			spans = append(spans, [2]int{a, b})
		}
		start = e
		i = e - 1
	}
	a := start
	for a < len(rs) && unicode.IsSpace(rs[a]) {
		a++
	}
	b := len(rs)
	for b > a && unicode.IsSpace(rs[b-1]) {
		b--
	}
	if a < b {
		spans = append(spans, [2]int{a, b})
	}
	merged := make([][2]int, 0, len(spans))
	for _, span := range spans {
		only := true
		for i := span[0]; i < span[1]; {
			if unicode.IsSpace(rs[i]) {
				i++
				continue
			}
			found := false
			for _, m := range ms {
				if m.start == i && m.end <= span[1] {
					i = m.end
					found = true
					break
				}
			}
			if !found {
				only = false
				break
			}
		}
		if only && len(merged) > 0 {
			merged[len(merged)-1][1] = span[1]
		} else {
			merged = append(merged, span)
		}
	}
	return merged
}
func Preflight(answer string) error {
	if n := utf8.RuneCountInString(answer); n < 1 || n > 16000 {
		return errors.New("answer length outside limit")
	}
	markdown := analyzeMarkdown(answer)
	ms := scanMarkers(markdown.controls)
	count := 0
	for _, m := range ms {
		if m.overflow {
			return errors.New("marker index overflow")
		}
		count += len(m.indexes)
	}
	if count > 512 {
		return errors.New("too many markers")
	}
	fs := maskedFigures(markdown, ms)
	if len(fs) > 512 {
		return errors.New("too many figures")
	}
	if len(projectedClaimSpans(markdown, fs, ms)) > 256 {
		return errors.New("too many claims")
	}
	return nil
}
func defaultScope(citations []Citation, markers []marker) []int {
	scope := []int{}
	if len(markers) == 0 {
		for _, citation := range citations {
			scope = append(scope, citation.Index)
		}
	}
	return scope
}

func Check(answer string, citations []Citation) Report {
	report := Report{Status: "grounded", Citations: []CitationResult{}, Claims: []Claim{}}
	source := map[int]map[string]bool{}
	failed := false
	for _, c := range citations {
		status := "resolved"
		if c.Err != "" {
			status = c.Err
			failed = true
		} else {
			source[c.Index] = map[string]bool{}
			markdown := analyzeMarkdown(c.Text)
			figures := maskedFigures(markdown, nil)
			for _, span := range markdown.ordinals {
				figures = append(figures, extract(string([]byte(c.Text)[span.Start:span.Stop]))...)
			}
			for _, f := range figures {
				for _, v := range f.Values {
					source[c.Index][f.Kind+"\x00"+v] = true
				}
				if f.identity != "" {
					source[c.Index]["digit\x00"+f.identity] = true
				}
			}
		}
		report.Citations = append(report.Citations, CitationResult{Index: c.Index, Status: status, TextSHA256: c.TextSHA256})
	}
	markdown := analyzeMarkdown(answer)
	ms := scanMarkers(markdown.controls)
	fs := maskedFigures(markdown, ms)
	for _, span := range projectedClaimSpans(markdown, fs, ms) {
		c := Claim{Start: span[0], End: span[1], Citations: defaultScope(citations, ms), UnknownMarkers: []int{}, Figures: []Figure{}}
		seen := map[int]bool{}
		hasMarkers := false
		for _, m := range ms {
			if m.start < span[0] || m.end > span[1] {
				continue
			}
			hasMarkers = true
			for _, idx := range m.indexes {
				if seen[idx] {
					continue
				}
				seen[idx] = true
				valid := false
				for _, cit := range citations {
					if cit.Index == idx {
						valid = true
						break
					}
				}
				if valid {
					c.Citations = append(c.Citations, idx)
				} else {
					c.UnknownMarkers = append(c.UnknownMarkers, idx)
				}
			}
		}
		for _, f := range fs {
			if f.Start < span[0] || f.End > span[1] {
				continue
			}
			f.Status = "missing"
			unavailable := !hasMarkers && len(ms) > 0
			for _, cit := range citations {
				if !hasMarkers && len(ms) > 0 {
					continue
				}
				if hasMarkers && !seen[cit.Index] {
					continue
				}
				if cit.Err != "" {
					unavailable = true
					continue
				}
				match := f.identity != "" && source[cit.Index]["digit\x00"+f.identity]
				for _, v := range f.Values {
					if source[cit.Index][f.Kind+"\x00"+v] {
						match = true
						break
					}
				}
				if match {
					f.FoundIn = append(f.FoundIn, cit.Index)
				}
			}
			if len(f.FoundIn) > 0 {
				f.Status = "found"
			} else if unavailable {
				f.Status = "unverifiable"
			}
			c.Figures = append(c.Figures, f)
		}
		c.Verdict = "grounded"
		if len(c.Figures) == 0 {
			c.Verdict = "no_figures"
		}
		for _, f := range c.Figures {
			if f.Status == "unverifiable" && c.Verdict != "ungrounded" {
				c.Verdict = "unverifiable"
			}
			if f.Status == "missing" {
				c.Verdict = "ungrounded"
			}
		}
		if len(c.UnknownMarkers) > 0 {
			c.Verdict = "ungrounded"
		}
		if c.Verdict == "ungrounded" {
			report.Status = "ungrounded"
		} else if c.Verdict == "unverifiable" && report.Status != "ungrounded" {
			report.Status = "incomplete"
		}
		report.Claims = append(report.Claims, c)
	}
	if failed && report.Status == "grounded" {
		report.Status = "incomplete"
	}
	return report
}
