package grounding

import (
	"strings"
	"unicode"

	"github.com/yuin/goldmark/util"
)

func digit(r rune) bool     { return r >= '0' && r <= '9' }
func wordRune(r rune) bool  { return unicode.IsLetter(r) || unicode.IsMark(r) || unicode.IsNumber(r) }
func separator(r rune) bool { return r == ',' || r == '.' }
func skipSpace(rs []rune, i int) int {
	for i < len(rs) && unicode.IsSpace(rs[i]) {
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
		for i := 0; i < len(tokens); {
			start := tokens[i].start
			start += trimStart(rs[start:tokens[i].end])
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
				continue
			}
			next := i + 1
			for next < len(tokens) && tokens[next].end <= safe {
				next++
			}
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
