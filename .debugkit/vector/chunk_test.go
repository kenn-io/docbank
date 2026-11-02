package vector_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"go.kenn.io/kit/vector"
)

func TestSplit(t *testing.T) {
	tests := []struct {
		name    string
		content string
		opts    vector.SplitOptions
		want    []vector.Chunk
	}{
		{
			name:    "empty yields no chunks",
			content: "",
			opts:    vector.SplitOptions{MaxRunes: 4},
			want:    nil,
		},
		{
			name:    "Unicode whitespace yields no chunks",
			content: " \t\n\u2003",
			opts:    vector.SplitOptions{MaxRunes: 2},
			want:    nil,
		},
		{
			name:    "non-positive max returns single chunk",
			content: "hello world",
			opts:    vector.SplitOptions{MaxRunes: 0},
			want:    []vector.Chunk{{Index: 0, Text: "hello world"}},
		},
		{
			name:    "content shorter than max is one chunk",
			content: "abcd",
			opts:    vector.SplitOptions{MaxRunes: 8},
			want:    []vector.Chunk{{Index: 0, Text: "abcd"}},
		},
		{
			name:    "windows without overlap",
			content: "abcdefghij",
			opts:    vector.SplitOptions{MaxRunes: 5},
			want: []vector.Chunk{
				{Index: 0, Text: "abcde"},
				{Index: 1, Text: "fghij"},
			},
		},
		{
			name:    "windows with overlap",
			content: "abcdefghij",
			opts:    vector.SplitOptions{MaxRunes: 4, Overlap: 1},
			want: []vector.Chunk{
				{Index: 0, Text: "abcd"},
				{Index: 1, Text: "defg"},
				{Index: 2, Text: "ghij"},
			},
		},
		{
			name:    "whitespace-only windows are omitted",
			content: "abcd    efgh",
			opts:    vector.SplitOptions{MaxRunes: 4},
			want: []vector.Chunk{
				{Index: 0, Text: "abcd"},
				{Index: 2, Text: "efgh"},
			},
		},
		{
			// Zero width space, byte order mark, zero width joiner, soft
			// hyphen, and NUL: invisible, but none are unicode.IsSpace.
			name:    "invisible formatting and control runes yield no chunks",
			content: "\u200b\ufeff\u200d\u00ad\x00 ",
			opts:    vector.SplitOptions{MaxRunes: 2},
			want:    nil,
		},
		{
			name:    "invisible-only windows are omitted",
			content: "abcd\u200b\ufeff\u200d\u2060efgh",
			opts:    vector.SplitOptions{MaxRunes: 4},
			want: []vector.Chunk{
				{Index: 0, Text: "abcd"},
				{Index: 2, Text: "efgh"},
			},
		},
		{
			name:    "invisible runes beside real text keep the window",
			content: "\u200bhi\ufeff",
			opts:    vector.SplitOptions{MaxRunes: 4},
			want:    []vector.Chunk{{Index: 0, Text: "\u200bhi\ufeff"}},
		},
		{
			// Braille pattern blank and Hangul filler render as nothing but
			// belong to visible categories and carry meaning where they are
			// used, so the rule deliberately leaves them alone.
			name:    "blank-looking runes from visible categories are kept",
			content: "\u2800\u3164",
			opts:    vector.SplitOptions{MaxRunes: 4},
			want:    []vector.Chunk{{Index: 0, Text: "\u2800\u3164"}},
		},
		{
			name:    "overlap at or above max clamps to max-1",
			content: "abcdef",
			opts:    vector.SplitOptions{MaxRunes: 3, Overlap: 9},
			want: []vector.Chunk{
				{Index: 0, Text: "abc"},
				{Index: 1, Text: "bcd"},
				{Index: 2, Text: "cde"},
				{Index: 3, Text: "def"},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, vector.Split(tt.content, tt.opts))
		})
	}
}

func TestSplitDoesNotTearMultiByteRunes(t *testing.T) {
	assert := assert.New(t)
	// Each emoji is multiple bytes but one rune.
	chunks := vector.Split("😀😁😂🤣", vector.SplitOptions{MaxRunes: 2})

	assert.Equal([]vector.Chunk{
		{Index: 0, Text: "😀😁"},
		{Index: 1, Text: "😂🤣"},
	}, chunks)
}
