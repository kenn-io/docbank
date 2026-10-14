package store

import (
	"errors"
	"strings"
	"unicode"
	"unicode/utf8"
)

// ErrFuzzyQuery reports opt-in discovery without query text.
var ErrFuzzyQuery = errors.New("fuzzy search requires query text")

type fuzzyQuery struct {
	components [][]string
	rooted     bool
	eligible   bool
}

type fuzzyRank struct{ edits, prefixes, suffix int }

func (r fuzzyRank) less(other fuzzyRank) bool {
	if r.edits != other.edits {
		return r.edits < other.edits
	}
	if r.prefixes != other.prefixes {
		return r.prefixes < other.prefixes
	}
	return r.suffix < other.suffix
}

// parseFuzzyQuery rejects only blank text. Text outside the supplemental
// syntax or bounds is ineligible, so ordinary search still answers it.
func parseFuzzyQuery(raw string) (fuzzyQuery, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return fuzzyQuery{}, ErrFuzzyQuery
	}
	if len(raw) > 256 || !fuzzyASCII(raw) {
		return fuzzyQuery{}, nil
	}
	q := fuzzyQuery{rooted: strings.HasPrefix(raw, "/")}
	path := strings.Contains(raw, "/")
	parts := strings.Split(strings.TrimPrefix(raw, "/"), "/")
	if len(parts) > 8 {
		return fuzzyQuery{}, nil
	}
	count := 0
	for _, part := range parts {
		words := fuzzyWords(part)
		if part == "." || part == ".." || len(words) == 0 {
			return fuzzyQuery{}, nil
		}
		count += len(words)
		for _, word := range words {
			if len(word) > 64 {
				return fuzzyQuery{}, nil
			}
			q.eligible = q.eligible || len(word) >= 5
		}
		q.components = append(q.components, words)
	}
	if count > 16 {
		return fuzzyQuery{}, nil
	}
	q.eligible = q.eligible || path
	return q, nil
}

func fuzzyASCII(s string) bool {
	for i := range len(s) {
		if s[i] >= utf8.RuneSelf {
			return false
		}
	}
	return true
}

// fuzzyComparable admits non-ASCII spaces, punctuation and symbols, such as
// the narrow no-break space in macOS screenshot names. SQLite's unicode61
// tokenizer and fuzzyWords both split words there, so the FTS anchor still
// selects every name the matcher accepts. Non-ASCII letters, digits and
// private-use characters are FTS token characters, and unicode61 removes
// combining diacritics inside a word; fuzzyWords would split either case
// differently, so those names stay out of scope.
func fuzzyComparable(s string) bool {
	for _, r := range s {
		if r < utf8.RuneSelf {
			continue
		}
		if unicode.In(r, unicode.Letter, unicode.Number, unicode.Mark, unicode.Co) {
			return false
		}
	}
	return true
}

func fuzzyAlpha(b byte) bool { return b >= 'a' && b <= 'z' }

func fuzzyWords(s string) []string {
	return strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return (r < 'a' || r > 'z') && (r < '0' || r > '9')
	})
}

// The anchor is a conservative superset: edited words keep their first
// character; short words must match cleanly. Every anchor is quoted literally.
func fuzzyAnchor(q fuzzyQuery) string {
	words := q.components[len(q.components)-1]
	anchors := make([]string, len(words))
	for i, w := range words {
		if len(w) >= 5 {
			anchors[i] = w[:1]
		} else {
			anchors[i] = w
		}
	}
	terms := make([]string, len(anchors))
	for i, a := range anchors {
		terms[i] = `"` + a + `"*`
	}
	return strings.Join(terms, " ")
}

// fuzzySuffixRulesOut reports whether the known root-first path suffix of a
// candidate whose full path exceeded a bound already proves it cannot match.
// An oversized ancestor stops the walk below a non-root node, and an exhausted
// walk leaves more components than any query has, so a rooted query never
// matches once the suffix is as long as the query. Otherwise the suffix must
// still match the query's trailing components within the edit budget.
func fuzzySuffixRulesOut(q fuzzyQuery, suffix []string) bool {
	if q.rooted && len(suffix) >= len(q.components) {
		return true
	}
	trailing := fuzzyQuery{components: q.components[max(0, len(q.components)-len(suffix)):]}
	_, ok := matchFuzzyComponents(trailing, suffix)
	return !ok
}

func matchFuzzyComponents(q fuzzyQuery, components []string) (fuzzyRank, bool) {
	if len(q.components) == 0 || len(components) < len(q.components) ||
		q.rooted && len(components) != len(q.components) {
		return fuzzyRank{}, false
	}
	// Track the best rank separately for each total edit budget. Repeated
	// query words can reuse a name word; choosing words never spends greedily.
	ranks := [2]fuzzyRank{}
	valid := [2]bool{true, false}
	start := len(components) - len(q.components)
	for i, words := range q.components {
		component := components[start+i]
		if !fuzzyComparable(component) {
			return fuzzyRank{}, false
		}
		candidateWords := fuzzyWords(component)
		for _, queryWord := range words {
			var next [2]fuzzyRank
			var nextValid [2]bool
			for _, candidate := range candidateWords {
				r, ok := matchFuzzyWord(queryWord, candidate)
				if !ok {
					continue
				}
				for edits := range 2 {
					total := edits + r.edits
					if !valid[edits] || total > 1 {
						continue
					}
					prior := ranks[edits]
					combined := fuzzyRank{total, prior.prefixes + r.prefixes, prior.suffix + r.suffix}
					if !nextValid[total] || combined.less(next[total]) {
						next[total] = combined
						nextValid[total] = true
					}
				}
			}
			ranks, valid = next, nextValid
		}
	}
	for i := range 2 {
		if valid[i] {
			return ranks[i], true
		}
	}
	return fuzzyRank{}, false
}

func matchFuzzyWord(query, candidate string) (fuzzyRank, bool) {
	rank := func(edits, consumed int) fuzzyRank {
		suffix := len(candidate) - consumed
		p := 0
		if suffix > 0 {
			p = 1
		}
		return fuzzyRank{edits, p, suffix}
	}
	if strings.HasPrefix(candidate, query) {
		return rank(0, len(query)), true
	}
	if len(query) < 5 || candidate == "" || query[0] != candidate[0] {
		return fuzzyRank{}, false
	}
	var best fuzzyRank
	found := false
	for length := len(query) - 1; length <= len(query)+1 && length <= len(candidate); length++ {
		if fuzzyOneEdit(query, candidate[:length]) {
			r := rank(1, length)
			if !found || r.less(best) {
				best = r
				found = true
			}
		}
	}
	return best, found
}

func fuzzyOneEdit(a, b string) bool {
	i := 0
	for i < len(a) && i < len(b) && a[i] == b[i] {
		i++
	}
	if i == 0 {
		return false
	}
	switch len(b) - len(a) {
	case 1:
		return i < len(b) && fuzzyAlpha(b[i]) && a[i:] == b[i+1:]
	case -1:
		return i < len(a) && fuzzyAlpha(a[i]) && a[i+1:] == b[i:]
	case 0:
		if i == len(a) {
			return false
		}
		if fuzzyAlpha(a[i]) && fuzzyAlpha(b[i]) && a[i+1:] == b[i+1:] {
			return true
		}
		return i+1 < len(a) && fuzzyAlpha(a[i]) && fuzzyAlpha(a[i+1]) &&
			a[i] == b[i+1] && a[i+1] == b[i] && a[i+2:] == b[i+2:]
	}
	return false
}
