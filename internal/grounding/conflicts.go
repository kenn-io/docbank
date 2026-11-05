package grounding

import (
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/rivo/uniseg"
)

type ComparedFigure struct {
	Passage int    `json:"passage"`
	Text    string `json:"text"`
	Value   string `json:"value"`
	Quote   string `json:"quote"`
}

type Conflict struct {
	Kind    string           `json:"kind"`
	Unit    string           `json:"unit"`
	Subject string           `json:"subject"`
	Figures []ComparedFigure `json:"figures"`
}

type Comparison struct {
	Status    string     `json:"status"`
	Conflicts []Conflict `json:"conflicts"`
	Compared  []bool     `json:"-"`
}

// Compare returns disagreements only between complete, unambiguous sentences
// with the same words around a supported figure. Passage references start at 1.
func Compare(query string, passages []string) Comparison {
	out := Comparison{Status: "NOT_COMPARABLE", Conflicts: []Conflict{}, Compared: make([]bool, len(passages))}
	type key struct{ kind, unit, subject string }
	groups := map[key][]ComparedFigure{}
	var order []key
	terms := comparisonWords(query)
	for i, passage := range passages {
		text := strings.TrimLeft(strings.TrimRight(decodeFigureText(passage), " \t…"), " \t\r\n…")
		local := map[key][]ComparedFigure{}
		var localOrder []key
		state, first := -1, true
		for text != "" {
			var sentence string
			sentence, text, state = uniseg.FirstSentenceInString(text, state)
			quote := strings.TrimSpace(sentence)
			initial, _ := utf8.DecodeRuneInString(quote)
			skip := first && unicode.IsLower(initial)
			first = false
			complete := strings.HasSuffix(quote, ".") || strings.HasSuffix(quote, "!") || strings.HasSuffix(quote, "?") ||
				strings.HasSuffix(sentence, "\n") || strings.HasSuffix(sentence, "\r")
			if skip || quote == "" || text == "" && !complete {
				continue
			}
			words := comparisonWords(quote)
			matches := len(terms) > 0
			for _, term := range terms {
				matches = matches && slices.ContainsFunc(words, func(word string) bool { return strings.HasPrefix(word, term) })
			}
			if !matches {
				continue
			}
			figures := decodedFigures(quote)
			if len(figures) != 1 {
				continue
			}
			figure := figures[0]
			before, after, _ := strings.Cut(quote, figure.text)
			kind, unit, value, suffix := comparisonValue(figure, after)
			if kind == "" {
				continue
			}
			subject := strings.Join(strings.FieldsFunc(strings.ToLower(before+" "+strings.TrimPrefix(after, suffix)), func(r rune) bool { return !unicode.IsLetter(r) }), " ")
			k := key{kind, unit, subject}
			if _, ok := local[k]; !ok {
				localOrder = append(localOrder, k)
			}
			local[k] = append(local[k], ComparedFigure{Passage: i + 1, Text: figure.text + suffix, Value: value, Quote: quote})
		}
		for _, k := range localOrder {
			figures := local[k]
			if slices.ContainsFunc(figures, func(f ComparedFigure) bool { return f.Value != figures[0].Value }) {
				continue
			}
			out.Compared[i] = true
			if _, ok := groups[k]; !ok {
				order = append(order, k)
			}
			groups[k] = append(groups[k], figures...)
		}
	}
	for _, k := range order {
		figures := groups[k]
		if !slices.ContainsFunc(figures, func(f ComparedFigure) bool { return f.Passage != figures[0].Passage }) {
			continue
		}
		out.Status = "NO_CONFLICT"
		if slices.ContainsFunc(figures, func(f ComparedFigure) bool { return f.Value != figures[0].Value }) {
			out.Conflicts = append(out.Conflicts, Conflict{Kind: k.kind, Unit: k.unit, Subject: k.subject, Figures: figures})
		}
	}
	if len(out.Conflicts) > 0 {
		out.Status = "CONFLICTING"
	}
	return out
}

func comparisonWords(text string) []string {
	return strings.FieldsFunc(strings.ToLower(text), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) })
}

func comparisonValue(figure normalizedFigure, after string) (kind, unit, value, suffix string) {
	if figure.kind == "amount" {
		unit, value, _ = strings.Cut(figure.value, " ")
		return "money", unit, value, ""
	}
	if figure.kind != "" {
		return
	}
	if strings.HasSuffix(figure.text, "%") {
		if value = amountValue(strings.TrimSuffix(figure.text, "%")); value != "" {
			return "percent", "%", value, ""
		}
		return "", "", "", ""
	}
	value = amountValue(figure.text)
	if value == "" {
		return "", "", "", ""
	}
	trimmed := strings.TrimLeft(after, " \t")
	if trimmed == after {
		return "", "", "", ""
	}
	word := strings.TrimLeftFunc(trimmed, unicode.IsLetter)
	suffix = after[:len(after)-len(word)]
	unit = strings.ToLower(trimmed[:len(trimmed)-len(word)])
	if unit == "percent" {
		return "percent", "%", value, suffix
	}
	unit = strings.TrimSuffix(unit, "s")
	switch unit {
	case "minute", "hour", "day", "week", "month", "year":
		return "duration", unit, value, suffix
	}
	return "", "", "", ""
}
