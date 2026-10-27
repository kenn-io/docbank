package answercard

import (
	"slices"
	"strings"
	"unicode"

	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
)

// MatchThreshold favors precision; unmatched questions fall through to search.
const MatchThreshold = 0.75

// Score computes Dice similarity over searchable tokens, guarded by complete
// numeric expressions. Decimal and date components retain their order.
func Score(question, phrasing string) float64 {
	q, qn := scoreTokens(question)
	p, pn := scoreTokens(phrasing)
	if len(q) == 0 || len(p) == 0 || !slices.Equal(qn, pn) {
		return 0
	}
	common := 0
	for word := range q {
		if p[word] {
			common++
		}
	}
	return 2 * float64(common) / float64(len(q)+len(p))
}

func scoreTokens(s string) (map[string]bool, []string) {
	s = cases.Fold().String(norm.NFKC.String(s))
	tokens := map[string]bool{}
	for word := range strings.FieldsSeq(normalize(s)) {
		switch word {
		case "a", "an", "the", "is", "are", "was", "were", "do", "does", "did", "what", "when", "where", "how", "of", "to", "for", "and", "in", "on", "at":
			continue
		}
		tokens[word] = true
	}
	// Numeric runs preserve leading fractions and signs, plus separators
	// between digits, keeping decimals and dates as complete expressions.
	var numbers []string
	r := []rune(s)
	for i := 0; i < len(r); i++ {
		if !unicode.IsDigit(r[i]) {
			continue
		}
		start := i
		if start > 0 && strings.ContainsRune(".,", r[start-1]) {
			start--
		}
		if start > 0 && strings.ContainsRune("+-−", r[start-1]) {
			start--
		}
		for i+1 < len(r) {
			if unicode.IsDigit(r[i+1]) {
				i++
				continue
			}
			if i+2 < len(r) && strings.ContainsRune(".,/:-", r[i+1]) && unicode.IsDigit(r[i+2]) {
				i += 2
				continue
			}
			break
		}
		numbers = append(numbers, strings.ReplaceAll(string(r[start:i+1]), "−", "-"))
	}
	slices.Sort(numbers)
	numbers = slices.Compact(numbers)
	return tokens, numbers
}

// Numeric identity is part of a phrasing's deduplication key: punctuation
// that distinguishes the scorer's numbers must not erase an explicit alternate.
func phrasingKey(s string) string {
	_, numbers := scoreTokens(s)
	return normalize(s) + "\x00" + strings.Join(numbers, "\x00")
}

func (c Card) score(question string) (best float64, phrasing string, count int) {
	for _, p := range append([]string{c.Question}, c.Alternates...) {
		s := Score(question, p)
		if s >= MatchThreshold {
			count++
		}
		if s > best {
			best, phrasing = s, p
		}
	}
	return
}
