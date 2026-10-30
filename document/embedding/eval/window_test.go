package eval

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/require"
)

func TestBoundedPassageWindowCentersMatchAndReturnsRuneOffsets(t *testing.T) {
	text := strings.Repeat("x", 100) + " Needle " + strings.Repeat("界", 100)
	start, end := BoundedPassageWindow(text, 101, 107, 30)
	got := string([]rune(text)[start:end])
	require.Equal(t, []int{86, 110}, []int{start, end})
	require.Equal(t, strings.Repeat("x", 14)+" Needle 界界", got)
	require.Contains(t, got, "Needle")
	require.LessOrEqual(t, len(got), 30)
	require.True(t, utf8.ValidString(got))
	start, end = BoundedPassageWindow("front remainder", 0, 0, 5)
	require.Equal(t, []int{0, 5}, []int{start, end})
	text = strings.Repeat("x", 100) + "needle" + strings.Repeat("x", 100)
	start, end = BoundedPassageWindow(text, 100, 106, 6)
	require.Equal(t, "needle", string([]rune(text)[start:end]))
}

func FuzzBoundedPassageWindow(f *testing.F) {
	f.Add("ascii needle 界 trailing", uint16(6), uint16(6), uint16(12))
	f.Add("界 x", uint16(0), uint16(1), uint16(4))
	f.Fuzz(func(t *testing.T, text string, rawStart, rawLength, rawLimit uint16) {
		if !utf8.ValidString(text) {
			return
		}
		limit := int(rawLimit % 1025)
		runes := []rune(text)
		focus := int(rawStart) % (len(runes) + 1)
		focusEnd := min(len(runes), focus+int(rawLength))
		start, end := BoundedPassageWindow(text, focus, focusEnd, limit)
		require.True(t, 0 <= start && start <= end && end <= len(runes))
		require.LessOrEqual(t, len(string(runes[start:end])), limit)
		if focusEnd > focus && len(string(runes[focus:focusEnd])) <= limit {
			require.LessOrEqual(t, start, focus)
			require.GreaterOrEqual(t, end, focusEnd)
		}
	})
}
