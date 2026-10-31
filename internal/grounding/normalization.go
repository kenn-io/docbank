package grounding

import (
	"regexp"
	"strings"
	"time"
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
func skipHorizontalSpace(rs []rune, i int) int {
	for i < len(rs) && (rs[i] == ' ' || rs[i] == '\t' || rs[i] == '\u00a0' || rs[i] == '\u202f') {
		i++
	}
	return i
}

type normalizedFigure struct {
	text, kind string
	values     []string
}

type figureKey struct{ kind, value string }

func (f normalizedFigure) comparisonKeys() []figureKey {
	kind := ""
	if f.kind == "amount" {
		kind = "amount"
	}
	keys := []figureKey{{kind, f.text}}
	for _, value := range f.values {
		keys = append(keys, figureKey{f.kind, value})
		if f.kind == "amount" {
			code, number := amountParts(value)
			for symbol, codes := range symbolCurrencies {
				if code == symbol {
					for _, compatible := range strings.Fields(codes) {
						keys = append(keys, figureKey{"amount", compatible + " " + number})
					}
				} else if compatibleCurrency(code, symbol) {
					keys = append(keys, figureKey{"amount", symbol + " " + number})
				}
			}
		}
	}
	return keys
}

// Only consume a whole sequence of existing literal tokens. Unrecognized input
// remains exactly the same figure that the literal checker would have seen.
func normalizedFigures(text string) []normalizedFigure {
	text = string(util.UnescapePunctuations([]byte(text)))
	result := []normalizedFigure{}
	for _, segment := range strings.FieldsFunc(text, func(r rune) bool { return strings.ContainsRune(delimiters, r) || r == '\n' || r == '\r' }) {
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
			for start < tokens[i].end && edge(rs[start]) {
				r := rs[start]
				if strings.ContainsRune("-+±.−", r) && start+1 < len(rs) && (unicode.IsDigit(rs[start+1]) || unicode.Is(unicode.Sc, rs[start+1])) {
					break
				}
				if strings.ContainsRune("-+±−", r) && start+2 < len(rs) && rs[start+1] == '.' && unicode.IsDigit(rs[start+2]) {
					break
				}
				if strings.ContainsRune("-+±−", r) {
					next := start + 1
					_, code := currencyAt(rs, next)
					if code != "" || next < len(rs) && (digit(rs[next]) || rs[next] == '.' && next+1 < len(rs) && digit(rs[next+1])) {
						break
					}
				}
				start++
			}
			matched := false
			safe := 0
			for kind, parse := range []func([]rune) (int, []string){dateAt, amountAt} {
				end, values := parse(rs[start:])
				if end > 0 {
					end += start
				}
				if end > safe {
					safe = end
				}
				if len(values) == 0 {
					continue
				}
				for j := i; j < len(tokens) && tokens[j].start < end; j++ {
					token := rs[tokens[j].start:tokens[j].end]
					trimmedEnd := tokens[j].start + len([]rune(strings.TrimRightFunc(string(token), edge)))
					if end == trimmedEnd {
						result = append(result, normalizedFigure{string(rs[start:end]), []string{"date", "amount"}[kind], values})
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

var timestampSyntax = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}(\.[0-9]+)?(Z|[+-]([01][0-9]|2[0-3]):[0-5][0-9])$`)

// A complete RFC 3339 timestamp can supply its written calendar date, but an
// answer timestamp itself remains a literal figure so its time is not lost.
func timestampDate(token string) string {
	if !timestampSyntax.MatchString(token) {
		return ""
	}
	if _, err := time.Parse(time.RFC3339, token); err != nil {
		return ""
	}
	return token[:10]
}
