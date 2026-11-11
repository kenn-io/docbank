package notemarkdown

import (
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"hegel.dev/go/hegel"
)

func TestOrderedListParagraphInterruption(t *testing.T) {
	for _, marker := range []string{"0.", "2.", "9)", "999999999.", "0002)"} {
		t.Run(marker, func(t *testing.T) {
			src := []byte("some paragraph\n" + marker + " item\nSetext\n=====\n")
			n, err := Parse(t.Context(), src, Limits{})
			require.NoError(t, err)
			require.Len(t, n.Headings, 1)
			require.Equal(t, 1, n.Headings[0].Level)
			require.Equal(t, "some paragraph "+marker+" item Setext", n.Headings[0].Text)
			require.Equal(t, Span{0, len(src)}, headingSpan(n.Headings[0]))
			require.Equal(t, Span{0, len(src)}, headingSection(n.Headings[0]))
		})
	}
	for _, marker := range []string{"1.", "1)", "01.", "0001)", "-", "+", "*"} {
		n, err := Parse(t.Context(), []byte("paragraph\n"+marker+" item\nSetext\n=====\n"), Limits{})
		require.NoError(t, err)
		require.Empty(t, n.Headings, marker)
	}
	// Non-1 list starts remain valid at a block boundary.
	n, err := Parse(t.Context(), []byte("2. item\nSetext\n=====\n"), Limits{})
	require.NoError(t, err)
	require.Empty(t, n.Headings)
}

func TestOrderedListInterruptionContainerBoundaries(t *testing.T) {
	for _, src := range []string{
		"> [first\n2. last](next.md)\n",
		"- [first\n2. last](next.md)\n",
	} {
		n, err := Parse(t.Context(), []byte(src), Limits{})
		require.NoError(t, err)
		require.Equal(t, []string{"next.md"}, linkTargets(n.Links))
		require.Equal(t, "[first\n2. last](next.md)", src[linkSpan(n.Links[0]).Start:linkSpan(n.Links[0]).End])
	}
	// Subsequent numbers in an existing ordered list start sibling items,
	// so an unfinished label must not join across the item boundary.
	for _, src := range []string{
		"1. [first\n2. last](bad.md)\n",
		"1) [first\n2) last](bad.md)\n",
	} {
		n, err := Parse(t.Context(), []byte(src), Limits{})
		require.NoError(t, err)
		require.Empty(t, n.Links)
	}
}

func TestPropertyOrderedParagraphSetext(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		number := hegel.Draw(ht, hegel.Integers[int](2, 999999))
		zeros := hegel.Draw(ht, hegel.Integers[int](0, 3))
		delim := hegel.Draw(ht, hegel.SampledFrom([]string{".", ")"}))
		eol := hegel.Draw(ht, hegel.SampledFrom([]string{"\n", "\r\n"}))
		marker := strings.Repeat("0", zeros) + strconv.Itoa(number) + delim
		body := "Intro" + eol + marker + " item" + eol + "Setext"
		src := []byte(body + eol + "=====" + eol)
		n, err := Parse(ht.Context(), src, Limits{})
		require.NoError(ht, err)
		require.Len(ht, n.Headings, 1)
		require.Equal(ht, "Intro "+marker+" item Setext", n.Headings[0].Text)
		require.Equal(ht, "intro-"+strings.TrimSuffix(strings.TrimSuffix(marker, "."), ")")+"-item-setext", n.Headings[0].Slug)
		require.Equal(ht, Span{0, len(src)}, headingSpan(n.Headings[0]))
	}, hegel.WithTestCases(1000))
}
