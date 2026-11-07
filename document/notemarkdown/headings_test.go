package notemarkdown

import (
	"bytes"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestHeadingByteSpansAndSections(t *testing.T) {
	for _, prefix := range []string{"", "\ufeff"} {
		for _, eol := range []string{"\n", "\r\n"} {
			src := []byte(prefix + strings.ReplaceAll("# A\nα\n## B\nβ\n# C\n", "\n", eol))
			n, err := Parse(t.Context(), src)
			require.NoError(t, err)
			require.Len(t, n.Headings, 3)
			for i, title := range []string{"# A", "## B", "# C"} {
				h := n.Headings[i]
				require.Equal(t, title+eol, string(src[h.Span.Start:h.Span.End]))
			}
			end := bytes.Index(src, []byte("# C"))
			require.Equal(t, end, n.Headings[0].Section.End)
			require.Equal(t, end, n.Headings[1].Section.End)
			require.Equal(t, len(src), n.Headings[2].Section.End)
		}
	}
}

func TestRootHeadingExclusions(t *testing.T) {
	src := []byte("---\ntitle: '# YAML'\n---\n# Root\n\n> # Quote\n\n- # List\n  ## Nested\n\n    # Indented\n\n```md\n# Fenced\n```\n\n<div>\n# HTML\n</div>\n\nSetext\n======\n")
	n, err := Parse(t.Context(), src)
	require.NoError(t, err)
	require.Equal(t, []string{"Root", "Setext"}, headingTexts(n.Headings))
	n, err = Parse(t.Context(), []byte("# **A** [B](next.md) `code` &amp; é\n"))
	require.NoError(t, err)
	require.Equal(t, "A B code & é", n.Headings[0].Text)
	require.Equal(t, "**A** [B](next.md) `code` &amp; é", n.Headings[0].Markdown)
}

func TestHeadingGlobalAnchorCollisions(t *testing.T) {
	n, err := Parse(t.Context(), []byte("# A\n# A\n# A-1\n# !!!\n# !!!\n# Cafe\u0301\n"))
	require.NoError(t, err)
	require.Equal(t, []string{"a", "a-1", "a-1-1", "section", "section-1", "café"}, anchors(n.Headings))
}

func TestHeadingResourceLimits(t *testing.T) {
	n, err := Parse(t.Context(), []byte(strings.Repeat("# H\n", 4097)))
	requireLimit(t, err, "headings")
	require.Equal(t, Note{}, n)
	n, err = Parse(t.Context(), []byte(strings.Repeat("> ", 129)+"text\n"))
	requireLimit(t, err, "ast_depth")
	require.Equal(t, Note{}, n)
}

func TestHeadingDelimiterDepthLimit(t *testing.T) {
	n, err := Parse(t.Context(), []byte("# "+strings.Repeat("[", 10000)+"\n"))
	requireLimit(t, err, "ast_depth")
	require.Equal(t, Note{}, n)
}

func anchors(headings []Heading) []string {
	var result []string
	for _, h := range headings {
		result = append(result, h.Anchor)
	}
	return result
}

func headingTexts(headings []Heading) []string {
	var result []string
	for _, h := range headings {
		result = append(result, h.Text)
	}
	return result
}
