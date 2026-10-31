package grounding

import (
	"strings"
	"unicode"

	"github.com/yuin/goldmark/util"
)

func isASCIIAlpha(r rune) bool { return r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' }
func digit(r rune) bool        { return r >= '0' && r <= '9' }
func wordRune(r rune) bool     { return unicode.IsLetter(r) || unicode.IsMark(r) || unicode.IsNumber(r) }
func idChar(r rune) bool       { return wordRune(r) || strings.ContainsRune("-/._", r) }
func separator(r rune) bool {
	return r == ',' || r == '.' || r == '\u00a0' || r == '\u202f' || r == '\''
}
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
			for i < len(rs) {
				grouping := (rs[i] == '\u00a0' || rs[i] == '\u202f') && i > start && i+1 < len(rs) && (digit(rs[i-1]) || rs[i-1] == '\u00a0' || rs[i-1] == '\u202f') && (digit(rs[i+1]) || rs[i+1] == '\u00a0' || rs[i+1] == '\u202f')
				if unicode.IsSpace(rs[i]) && !grouping {
					break
				}
				i++
			}
			tokens = append(tokens, span{start, i})
		}
		for i := 0; i < len(tokens); {
			start := tokens[i].start
			start += trimStart(rs[start:tokens[i].end])
			// Parse signs before ISO codes without changing literal trimming.
			if start > tokens[i].start && strings.ContainsRune("-+±−", rs[start-1]) {
				if _, code := currencyAt(rs, start); code != "" {
					start--
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
