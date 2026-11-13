package notemarkdown

import (
	"bytes"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestHeadingByteSpansSectionsAndPaths(t *testing.T) {
	for _, prefix := range []string{"", "\ufeff"} {
		for _, eol := range []string{"\n", "\r\n", "\r"} {
			src := []byte(prefix + strings.ReplaceAll("# A\nα\n> ## B\n> β\n- ### C\n  γ\n# D\n", "\n", eol))
			n, err := Parse(t.Context(), src, Limits{})
			require.NoError(t, err)
			require.Equal(t, []string{"A", "B", "C", "D"}, headingTexts(n.Headings))
			for i, line := range []string{"# A", "> ## B", "- ### C", "# D"} {
				h := n.Headings[i]
				require.Equal(t, line+eol, string(src[h.Start:h.LineEnd]))
				require.Equal(t, i, h.Ordinal)
				require.Equal(t, i*2+1, h.Line)
				require.True(t, bytes.HasPrefix(src[h.Start:h.SectionEnd], src[h.Start:h.LineEnd]))
			}
			end := bytes.Index(src, []byte("# D"))
			for i := range 3 {
				require.Equal(t, end, n.Headings[i].SectionEnd)
				require.Equal(t, 7, n.Headings[i].SectionEndLine)
			}
			require.Equal(t, "A > B > C", n.Headings[2].Path)
			require.Equal(t, "D", n.Headings[3].Path)
			require.Equal(t, len(src), n.Headings[3].SectionEnd)
		}
	}
}

func TestHeadingContainersSetextAndSuppression(t *testing.T) {
	src := []byte("---\ntitle: '# YAML'\n---\n# Root\n\n> # Quote\n\n- # List\n  ## Nested\n\n```md\n# Fenced\n```\n\n<div>\n# HTML\n</div>\n\nSetext\n======\n\n> Nested Setext\n> ---\n\n    # Indented\n")
	n, err := Parse(t.Context(), src, Limits{})
	require.NoError(t, err)
	require.Equal(t, []string{"Root", "Quote", "List", "Nested", "Setext", "Nested Setext"}, headingTexts(n.Headings))
	h := n.Headings[5]
	require.Equal(t, "> Nested Setext\n> ---\n", string(src[h.Start:h.LineEnd]))
	n, err = Parse(t.Context(), []byte("# **A** [B](next.md) `code` &amp; é\n"), Limits{})
	require.NoError(t, err)
	require.Equal(t, "**A** [B](next.md) `code` &amp; é", n.Headings[0].Text)
	for _, src := range []string{"#", "### \n", "# ###\n"} {
		n, err := Parse(t.Context(), []byte(src), Limits{})
		require.NoError(t, err)
		require.Len(t, n.Headings, 1)
		require.Empty(t, n.Headings[0].Text)
		require.Equal(t, src, src[n.Headings[0].Start:n.Headings[0].LineEnd])
	}
}

func TestHeadingGlobalSlugCollisions(t *testing.T) {
	n, err := Parse(t.Context(), []byte("# A\n# A\n# A-1\n# !!!\n# !!!\n# Cafe\u0301\n# A\n# A-2\n# A\n# A-1\n"), Limits{})
	require.NoError(t, err)
	require.Equal(t, []string{"a", "a-1", "a-1-1", "section", "section-1", "café", "a-2", "a-2-1", "a-3", "a-1-2"}, anchors(n.Headings))
}
func anchors(headings []Heading) []string {
	var out []string
	for _, h := range headings {
		out = append(out, h.Slug)
	}
	return out
}
func headingTexts(headings []Heading) []string {
	var out []string
	for _, h := range headings {
		out = append(out, h.Text)
	}
	return out
}
