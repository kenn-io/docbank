package vector

import "unicode"

// blank reports whether text holds nothing an embedding model can represent:
// it is empty, or every rune is whitespace, an invisible formatting rune, or a
// control character.
//
// Unicode whitespace alone is too narrow a rule. Zero-width and formatting
// runes reach a corpus routinely — U+200B ZERO WIDTH SPACE from copy-pasted
// web text, U+FEFF from a byte order mark that survived a concatenation,
// U+200D and U+00AD from templating and word processors — and none of them
// are unicode.IsSpace. A document made only of those looks non-empty to a
// provider, which then rejects the request, and one such document is enough
// to stall a fill that would otherwise complete. Deciding it here keeps the
// judgment in one place instead of in each caller's reading of each
// provider's error wording.
//
// The rule stays deliberately conservative. Runes that render as blank but
// belong to visible categories — U+2800 BRAILLE PATTERN BLANK, U+3164 HANGUL
// FILLER — carry meaning in the text that uses them and are left alone.
func blank(text string) bool {
	for _, r := range text {
		if !unicode.IsSpace(r) && !unicode.Is(unicode.Cf, r) && !unicode.Is(unicode.Cc, r) {
			return false
		}
	}
	return true
}
