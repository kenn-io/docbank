package grounding

import (
	"strings"
	"unicode"

	"github.com/yuin/goldmark/util"
)

func digit(r rune) bool     { return r >= '0' && r <= '9' }
func separator(r rune) bool { return r == ',' || r == '.' }

// Neighbor checks ignore edge punctuation and join only across spaces and tabs.
func amountGap(r rune) bool {
	return r == ' ' || r == '\t' || edge(r) && !unicode.IsSpace(r)
}

func skipSpace(rs []rune, i int, multiline bool) int {
	for i < len(rs) && (rs[i] == ' ' || rs[i] == '\t' || multiline && unicode.IsSpace(rs[i])) {
		i++
	}
	return i
}

type normalizedFigure struct {
	text, kind, value string
}

type figureKey struct{ kind, value string }

// Only consume a whole sequence of existing literal tokens. Unrecognized input
// remains exactly the same figure that the literal checker would have seen.
func normalizedFigures(text string) []normalizedFigure {
	text = string(util.UnescapePunctuations([]byte(text)))
	result := []normalizedFigure{}
	for _, segment := range strings.FieldsFunc(text, func(r rune) bool { return strings.ContainsRune(delimiters, r) }) {
		rs := []rune(segment)
		type span struct{ start, end int }
		var tokens []span
		for i := 0; i < len(rs); {
			if unicode.IsSpace(rs[i]) {
				i++
				continue
			}
			start := i
			for i < len(rs) && !unicode.IsSpace(rs[i]) {
				i++
			}
			tokens = append(tokens, span{start, i})
		}
		previousUnparsed := false
		for i := 0; i < len(tokens); {
			start := tokens[i].start
			start += trimStart(rs[start:tokens[i].end])
			// Literal trimming can discard a sign before an ISO code. Keep it
			// visible to normalization so signed amounts remain literal.
			for j := tokens[i].start; j < start; j++ {
				if strings.ContainsRune("-+±\u2212", rs[j]) {
					if _, marker := currencyAt(rs, j+1); marker != "" {
						start = j
						break
					}
				}
			}
			matched := false
			safe := 0
			for kind, parse := range []func([]rune) (int, string){dateAt, amountAt} {
				end, value := parse(rs[start:])
				if end > 0 {
					end += start
				}
				if end > safe {
					safe = end
				}
				// A standalone marker before an amount makes its currency ambiguous.
				if kind == 1 && previousUnparsed && strings.TrimFunc(string(rs[tokens[i-1].end:start]), amountGap) == "" {
					previous := []rune(trim(rs[tokens[i-1].start:tokens[i-1].end]))
					if markerEnd, marker := currencyAt(previous, 0); marker != "" && markerEnd == len(previous) {
						value = ""
					}
				}
				if value == "" {
					continue
				}
				for j := i; j < len(tokens) && tokens[j].start < end; j++ {
					token := rs[tokens[j].start:tokens[j].end]
					trimmedEnd := tokens[j].start + len([]rune(strings.TrimRightFunc(string(token), edge)))
					if end == trimmedEnd {
						result = append(result, normalizedFigure{string(rs[start:end]), []string{"date", "amount"}[kind], value})
						i = j + 1
						matched = true
						break
					}
				}
				if matched {
					break
				}
			}
			if matched {
				previousUnparsed = false
				continue
			}
			next := i + 1
			for next < len(tokens) && tokens[next].end <= safe {
				next++
			}
			previousUnparsed = next == i+1 && safe == 0
			for ; i < next; i++ {
				token := string(rs[tokens[i].start:tokens[i].end])
				for _, value := range extractDecoded(token) {
					result = append(result, normalizedFigure{text: value})
				}
			}
		}
	}
	return result
}
