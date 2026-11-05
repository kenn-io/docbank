package grounding

import (
	"regexp"
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
	Figures []ComparedFigure `json:"figures"`
}

type Comparison struct {
	Status    string     `json:"status"`
	Conflicts []Conflict `json:"conflicts"`
	Compared  []bool     `json:"-"`
}

type Source struct {
	Text       string
	Start, End int
}

var (
	paragraphBreak = regexp.MustCompile("(?m)\\n[ \\t\\r]*\\n|^[ \\t]*(?:`{3,}|~{3,})[ \\t\\r]*(?:\\n|$)")
	hardWrap       = strings.NewReplacer("\n", " ", "\r", " ")
)

// Compare returns disagreements only between complete, unambiguous sentences
// with the same words around a supported figure. Passage references start at 1.
func Compare(sources []Source) Comparison {
	out := Comparison{Status: "NOT_COMPARABLE", Conflicts: []Conflict{}, Compared: make([]bool, len(sources))}
	type key struct{ kind, unit, subject string }
	groups := map[key][]ComparedFigure{}
	var order []key
	for i, source := range sources {
		local := map[key][]ComparedFigure{}
		var localOrder []key
		for _, sentence := range sourceSentences(source) {
			quote := decodeFigureText(strings.Join(strings.Fields(sentence), " "))
			end := strings.TrimRightFunc(quote, func(r rune) bool {
				return unicode.Is(unicode.Pe, r) || unicode.Is(unicode.Pf, r) || r == '\'' || r == '"'
			})
			complete := strings.HasSuffix(end, ".") || strings.HasSuffix(end, "!") || strings.HasSuffix(end, "?")
			if !complete || utf8.RuneCountInString(quote) > 512 {
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
			subject := strings.Join(strings.Fields(strings.ToLower(before+"#"+strings.TrimPrefix(after, suffix))), " ")
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
		for _, figure := range figures {
			out.Compared[figure.Passage-1] = true
		}
		out.Status = "NO_CONFLICT"
		if slices.ContainsFunc(figures, func(f ComparedFigure) bool { return f.Value != figures[0].Value }) {
			out.Conflicts = append(out.Conflicts, Conflict{Kind: k.kind, Unit: k.unit, Figures: figures})
		}
	}
	if len(out.Conflicts) > 0 {
		out.Status = "CONFLICTING"
	}
	return out
}

// sourceSentences keeps byte offsets while joining hard wraps within paragraphs.
// Bare code fences separate paragraphs without becoming sentence text.
func sourceSentences(source Source) []string {
	var sentences []string
	if source.Start < 0 || source.End <= source.Start || source.End > len(source.Text) {
		return sentences
	}
	start := 0
	breaks := paragraphBreak.FindAllStringIndex(source.Text, -1)
	breaks = append(breaks, []int{len(source.Text), len(source.Text)})
	for _, boundary := range breaks {
		text := hardWrap.Replace(source.Text[start:boundary[0]])
		offset, state := start, -1
		for text != "" {
			var sentence string
			sentence, text, state = uniseg.FirstSentenceInString(text, state)
			if offset < source.End && offset+len(sentence) > source.Start {
				sentences = append(sentences, sentence)
			}
			offset += len(sentence)
		}
		start = boundary[1]
	}
	return sentences
}

func comparisonValue(figure normalizedFigure, after string) (kind, unit, value, suffix string) {
	if figure.kind == "amount" {
		unit, value, _ = strings.Cut(figure.value, " ")
		return "money", unit, value, ""
	}
	if figure.kind != "" {
		return "", "", "", ""
	}
	if number, ok := strings.CutSuffix(figure.text, "%"); ok {
		if value = decimalValue(number); value != "" {
			return "percent", "%", value, ""
		}
		return "", "", "", ""
	}
	value = decimalValue(figure.text)
	if value == "" {
		return "", "", "", ""
	}
	trimmed := strings.TrimLeft(after, " \t")
	if trimmed == after {
		return "", "", "", ""
	}
	if strings.HasPrefix(trimmed, "%") {
		return "percent", "%", value, after[:len(after)-len(trimmed)+1]
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
