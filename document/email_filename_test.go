package document

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSafeEmailFilenameNormalizesWithoutBecomingAPath(t *testing.T) {
	tests := []struct{ input, want string }{
		{`a<>:"/\|?*b`, "a_________b"},
		{"cafe\u0301.txt", "café.txt"},
		{"NUL.tar.gz", "_NUL.tar.gz"},
		{"COM¹.txt", "_COM¹.txt"},
		{"lPt³.log", "_lPt³.log"},
		{"CON .txt", "_CON .txt"},
		{"COM0.txt", "COM0.txt"},
		{"COM10.txt", "COM10.txt"},
		{"\x00\x7f\u0085", "___"},
		{" . . ", "part-1-2"},
	}
	for _, test := range tests {
		got, err := SafeEmailFilename(test.input, "1.2")
		require.NoError(t, err)
		assert.Equal(t, test.want, got)
	}
}
func TestSafeEmailFilenameShortensOnRuneBoundary(t *testing.T) {
	tests := []struct {
		name, input, path, want string
	}{
		{"239 plus multibyte", strings.Repeat("a", 239) + "é", "1", strings.Repeat("a", 239)},
		{"trailing dot after truncation", strings.Repeat("a", 239) + ".tail", "1", strings.Repeat("a", 239)},
		{"reserved at cap", "CON." + strings.Repeat("a", 236), "1", "_CON." + strings.Repeat("a", 235)},
		{"longest valid fallback", strings.Repeat(".", 241), strings.Repeat("123456789.", 15) + "123456789", "part-" + strings.Repeat("123456789-", 15) + "123456789"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := SafeEmailFilename(test.input, test.path)
			require.NoError(t, err)
			assert.Equal(t, test.want, got)
			assert.LessOrEqual(t, len(got), 240)
			assert.True(t, utf8.ValidString(got))
		})
	}
	for _, size := range []int{239, 240, 241} {
		got, err := SafeEmailFilename(strings.Repeat("x", size), "1")
		require.NoError(t, err)
		assert.Len(t, got, min(size, 240))
	}
}
func TestSafeEmailFilenameRejectsInvalidInputs(t *testing.T) {
	_, err := SafeEmailFilename(string([]byte{0xff}), "1")
	require.Error(t, err)
	_, err = SafeEmailFilename("ok", "01")
	require.Error(t, err)
	_, err = SafeEmailFilename("ok", strings.Repeat("1.", 16)+"1")
	require.Error(t, err)
}
