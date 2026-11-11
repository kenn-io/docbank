package notemarkdown

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestMultilineSetextRawSpan(t *testing.T) {
	src := []byte("some paragraph\n2. item\nSetext\n=====\n")
	n, err := Parse(t.Context(), src, Limits{})
	require.NoError(t, err)
	require.Len(t, n.Headings, 1)
	require.Equal(t, 1, n.Headings[0].Level)
	require.Equal(t, "some paragraph 2. item Setext", n.Headings[0].Text)
	require.Equal(t, Span{0, len(src)}, headingSpan(n.Headings[0]))
	require.Equal(t, Span{0, len(src)}, headingSection(n.Headings[0]))
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
