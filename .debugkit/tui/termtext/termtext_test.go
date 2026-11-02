package termtext

import (
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
)

func TestStripANSI(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "plain text", in: "plain", want: "plain"},
		{name: "CSI", in: "a\x1b[31mred\x1b[0mz", want: "aredz"},
		{name: "OSC terminated by BEL", in: "a\x1b]0;title\az", want: "az"},
		{name: "OSC terminated by ST", in: "a\x1b]8;;https://example.com\x1b\\link\x1b]8;;\x1b\\z", want: "alinkz"},
		{name: "DCS", in: "a\x1bPpayload\x1b\\z", want: "az"},
		{name: "preserves ordinary controls", in: "a\tb\nc", want: "a\tb\nc"},
		{name: "replaces invalid UTF-8", in: "a\xffb", want: "a\ufffdb"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, StripANSI(tt.in))
		})
	}
}

func TestSanitizeBlock(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "plain text", in: "Hello, \u4e16\u754c!", want: "Hello, \u4e16\u754c!"},
		{name: "strips CSI", in: "before\x1b[2Jafter", want: "beforeafter"},
		{name: "strips OSC", in: "before\x1b]0;title\aafter", want: "beforeafter"},
		{name: "strips DCS", in: "before\x1bPpayload\x1b\\after", want: "beforeafter"},
		{name: "strips bare ESC sequence", in: "before\x1bcafter", want: "beforeafter"},
		{name: "strips incomplete CSI", in: "before\x1b[31", want: "before"},
		{name: "strips incomplete OSC and payload", in: "before\x1b]0;title", want: "before"},
		{name: "strips C0 and C1 controls", in: "a\x00b\bc\rd\u0085e\x7f", want: "abcde"},
		{name: "strips bidi and format controls", in: "safe\u202eevil\u2066text\u2069", want: "safeeviltext"},
		{name: "preserves tabs and newlines", in: "one\ttwo\nthree", want: "one\ttwo\nthree"},
		{name: "replaces invalid UTF-8", in: "a\xffb\xc3", want: "a\ufffdb\ufffd"},
		{name: "empty", in: "", want: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := SanitizeBlock(tt.in)
			assert := assert.New(t)
			assert.Equal(tt.want, got)
			assert.True(utf8.ValidString(got), "result is not valid UTF-8: %q", got)
		})
	}
}

func TestSanitizeLine(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "plain text", in: "one line", want: "one line"},
		{name: "replaces tabs and newlines", in: "one\ttwo\nthree", want: "one two three"},
		{name: "strips carriage return", in: "fake\rreal", want: "fakereal"},
		{name: "strips escapes and format controls", in: "a\x1b[31mb\x1b[0m\u202ec", want: "abc"},
		{name: "replaces invalid UTF-8", in: "a\xffb", want: "a\ufffdb"},
		{name: "empty", in: "", want: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := SanitizeLine(tt.in)
			assert := assert.New(t)
			assert.Equal(tt.want, got)
			assert.NotContains(got, "\n")
			assert.NotContains(got, "\t")
		})
	}
}

func TestSanitizeStyledBlock(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "preserves SGR", in: "\x1b[1;31mbold red\x1b[0m", want: "\x1b[1;31mbold red\x1b[0m"},
		{name: "preserves colon-form SGR", in: "\x1b[38:2:1:2:3mcolor\x1b[0m", want: "\x1b[38:2:1:2:3mcolor\x1b[0m"},
		{name: "strips private SGR-like CSI", in: "a\x1b[?1mb", want: "ab"},
		{name: "strips SGR with intermediate", in: "a\x1b[1 mb", want: "ab"},
		{name: "strips non-SGR CSI", in: "a\x1b[2Jb", want: "ab"},
		{name: "strips OSC and DCS", in: "a\x1b]0;title\a\x1bPdata\x1b\\b", want: "ab"},
		{name: "strips controls and format runes", in: "a\rb\u202ec", want: "abc"},
		{name: "preserves tabs and newlines", in: "one\ttwo\nthree", want: "one\ttwo\nthree"},
		{name: "replaces invalid UTF-8", in: "a\xffb", want: "a\ufffdb"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, SanitizeStyledBlock(tt.in))
		})
	}
}

func TestDisplayWidth(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want int
	}{
		{name: "empty", in: "", want: 0},
		{name: "ASCII", in: "hello", want: 5},
		{name: "ignores ANSI", in: "\x1b[31mhello\x1b[0m", want: 5},
		{name: "wide runes", in: "\u65e5\u672c", want: 4},
		{name: "emoji grapheme", in: "\U0001f469\u200d\U0001f4bb", want: 2},
		{name: "invalid UTF-8 becomes replacement rune", in: "a\xffb", want: 3},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, DisplayWidth(tt.in))
		})
	}
}

func TestTruncate(t *testing.T) {
	tests := []struct {
		name  string
		in    string
		width int
		tail  string
		want  string
	}{
		{name: "zero width", in: "hello", width: 0, want: ""},
		{name: "negative width", in: "hello", width: -1, want: ""},
		{name: "exact width", in: "hello", width: 5, want: "hello"},
		{name: "shorter than width", in: "hi", width: 5, want: "hi"},
		{name: "adds tail within width", in: "abcdefgh", width: 6, tail: "...", want: "abc..."},
		{name: "tail wider than width", in: "abcdefgh", width: 2, tail: "...", want: ""},
		{name: "does not split wide rune", in: "a\u65e5b", width: 2, want: "a"},
		{name: "preserves ANSI", in: "\x1b[31mabcdef\x1b[0m", width: 3, want: "\x1b[31mabc\x1b[0m"},
		{name: "replaces invalid UTF-8", in: "a\xffbc", width: 3, want: "a\ufffdb"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Truncate(tt.in, tt.width, tt.tail)
			assert := assert.New(t)
			assert.Equal(tt.want, got)
			assert.LessOrEqual(DisplayWidth(got), max(tt.width, 0))
		})
	}
}

func TestWrap(t *testing.T) {
	tests := []struct {
		name  string
		in    string
		width int
		want  []string
	}{
		{name: "zero width preserves existing lines", in: "one\ntwo", width: 0, want: []string{"one", "two"}},
		{name: "negative width preserves existing lines", in: "one\ntwo", width: -1, want: []string{"one", "two"}},
		{name: "empty input is one empty line", in: "", width: 5, want: []string{""}},
		{name: "exact width", in: "hello", width: 5, want: []string{"hello"}},
		{name: "breaks at word boundary", in: "hello world", width: 7, want: []string{"hello", "world"}},
		{name: "breaks a long word", in: "abcdefgh", width: 3, want: []string{"abc", "def", "gh"}},
		{name: "wraps wide runes by cells", in: "\u3042\u3044\u3046\u3048", width: 4, want: []string{"\u3042\u3044", "\u3046\u3048"}},
		{name: "preserves blank source lines", in: "one\n\nthree", width: 10, want: []string{"one", "", "three"}},
		{name: "preserves ANSI styling", in: "\x1b[31mabcdef\x1b[0m", width: 3, want: []string{"\x1b[31mabc", "def\x1b[0m"}},
		{name: "replaces invalid UTF-8", in: "a\xffbc", width: 2, want: []string{"a\ufffd", "bc"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Wrap(tt.in, tt.width)
			assert := assert.New(t)
			assert.Equal(tt.want, got)
			for _, line := range got {
				if tt.width > 0 {
					assert.LessOrEqual(DisplayWidth(line), tt.width, "line %q exceeds width", line)
				}
			}
		})
	}
}
