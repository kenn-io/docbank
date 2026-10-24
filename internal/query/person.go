package query

import (
	"strings"
	"unicode"
)

// PersonNameStartsWithWord matches a literal prefix at word starts in a folded name.
// Letters, numbers, and combining marks belong to the same word.
func PersonNameStartsWithWord(name, prefix string) bool {
	if prefix == "" {
		return true
	}
	wordStart := true
	for offset, r := range name {
		if wordStart && strings.HasPrefix(name[offset:], prefix) {
			return true
		}
		wordStart = !unicode.IsLetter(r) && !unicode.IsNumber(r) && !unicode.IsMark(r)
	}
	return false
}
