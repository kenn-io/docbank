package termtext

import (
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/charmbracelet/x/ansi"
)

const replacement = "\ufffd"

// StripANSI removes terminal escape sequences from s without otherwise
// sanitizing controls, tabs, or newlines. Invalid UTF-8 bytes are replaced
// with U+FFFD.
func StripANSI(s string) string {
	return ansi.Strip(validUTF8(s))
}

// SanitizeBlock prepares untrusted text for multiline terminal output. It
// removes terminal escape sequences, Unicode controls, and format characters,
// while preserving tabs and newlines. Invalid UTF-8 bytes are replaced with
// U+FFFD.
func SanitizeBlock(s string) string {
	return sanitize(s, false, false)
}

// SanitizeLine prepares untrusted text for a single terminal line. It applies
// SanitizeBlock's safety rules and replaces tabs and newlines with one space.
func SanitizeLine(s string) string {
	return sanitize(s, true, false)
}

// SanitizeStyledBlock prepares trusted renderer output for a terminal. It
// preserves only complete SGR color and attribute sequences, removing other
// terminal escape sequences, Unicode controls, and format characters. Tabs
// and newlines are preserved.
//
// Do not use this function to claim that arbitrary styled text is safe or
// faithful: SGR can still conceal or visually alter text. Use SanitizeBlock
// for untrusted source content.
func SanitizeStyledBlock(s string) string {
	return sanitize(s, false, true)
}

func sanitize(s string, singleLine, preserveSGR bool) string {
	s = validUTF8(s)
	if s == "" {
		return ""
	}

	var b strings.Builder
	b.Grow(len(s))
	state := ansi.NormalState

	for len(s) > 0 {
		seq, _, n, nextState := ansi.DecodeSequence(s, state, nil)
		if n <= 0 {
			_, n = utf8.DecodeRuneInString(s)
			seq = s[:n]
			nextState = ansi.NormalState
		}

		if isEscapeSequence(seq) {
			if preserveSGR && isSGR(seq, nextState) {
				b.WriteString(seq)
			}
		} else {
			writeSafeText(&b, seq, singleLine)
		}

		s = s[n:]
		state = nextState
	}

	return b.String()
}

func isEscapeSequence(seq string) bool {
	return ansi.HasEscPrefix(seq) ||
		ansi.HasCsiPrefix(seq) ||
		ansi.HasOscPrefix(seq) ||
		ansi.HasDcsPrefix(seq) ||
		ansi.HasSosPrefix(seq) ||
		ansi.HasPmPrefix(seq) ||
		ansi.HasApcPrefix(seq)
}

func isSGR(seq string, state byte) bool {
	if state != ansi.NormalState || !ansi.HasCsiPrefix(seq) {
		return false
	}
	start := 1
	if ansi.HasEscPrefix(seq) {
		start = 2
	}
	if len(seq) <= start || seq[len(seq)-1] != 'm' {
		return false
	}
	for _, b := range []byte(seq[start : len(seq)-1]) {
		if (b < '0' || b > '9') && b != ';' && b != ':' {
			return false
		}
	}
	return true
}

func writeSafeText(b *strings.Builder, s string, singleLine bool) {
	for _, r := range s {
		switch r {
		case '\n', '\t':
			if singleLine {
				b.WriteByte(' ')
			} else {
				b.WriteRune(r)
			}
		default:
			if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
				continue
			}
			b.WriteRune(r)
		}
	}
}

// DisplayWidth reports the number of terminal cells occupied by the
// single-line string s. ANSI sequences have zero width, wide characters and
// grapheme clusters are accounted for, and invalid UTF-8 bytes count as U+FFFD.
func DisplayWidth(s string) int {
	return ansi.StringWidth(validUTF8(s))
}

// Truncate returns s shortened to at most width terminal cells. If truncation
// is needed, tail is included within width; a tail wider than width produces
// an empty string. ANSI styling is preserved and wide graphemes are not split.
func Truncate(s string, width int, tail string) string {
	if width <= 0 {
		return ""
	}
	return ansi.Truncate(validUTF8(s), width, validUTF8(tail))
}

// Wrap splits s into terminal lines no wider than width cells, preferring word
// boundaries and preserving existing line breaks and ANSI styling. When width
// is not positive, Wrap only splits existing lines; callers own any default
// width policy.
func Wrap(s string, width int) []string {
	s = validUTF8(s)
	if width <= 0 {
		return strings.Split(s, "\n")
	}
	return strings.Split(ansi.Wrap(s, width, ""), "\n")
}

func validUTF8(s string) string {
	return strings.ToValidUTF8(s, replacement)
}
