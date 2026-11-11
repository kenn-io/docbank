package notemarkdown

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestParseInputAndTitle(t *testing.T) {
	for _, tc := range []struct {
		src, title, source string
		lines              int
	}{
		{"", "", "", 1}, {"plain\r\ntext", "", "", 2},
		{"# First\n# Second\n", "First", "h1", 3},
		{"---\ntitle: Authored\n---\n# Heading\n", "Authored", "frontmatter", 5},
		{"---\ntitle: ''\n---\n# Heading", "Heading", "h1", 4},
		{"---\ntitle: 42\n---\n# Heading", "42", "frontmatter", 4},
		{"```\n# Hidden\n```\n# Shown", "Shown", "h1", 5},
	} {
		n, err := Parse(t.Context(), []byte(tc.src), Limits{})
		require.NoError(t, err)
		require.Equal(t, tc.title, n.Title)
		require.Equal(t, tc.source, n.TitleSource)
		require.Equal(t, tc.lines, n.Lines)
	}
	for _, src := range [][]byte{{0xff}, {'a', 0}, {0xef, 0xbb, 0xbf, 0xff}} {
		n, err := Parse(t.Context(), src, Limits{})
		require.ErrorIs(t, err, ErrInvalidInput)
		require.Equal(t, Note{}, n)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	n, err := Parse(ctx, []byte("# title"), Limits{})
	require.ErrorIs(t, err, context.Canceled)
	require.Equal(t, Note{}, n)
	require.Equal(t, "docbank-note-markdown/v2", Recipe)
}

func requireLimit(tb testing.TB, err error, resource string) {
	tb.Helper()
	var limit *LimitError
	require.ErrorAs(tb, err, &limit)
	require.ErrorIs(tb, err, ErrLimit)
	require.Equal(tb, resource, limit.Resource)
}

func frontmatterState(n Note) string {
	if n.Frontmatter == nil {
		return "absent"
	}
	if n.Frontmatter.Raw == nil {
		return "invalid"
	}
	return "valid"
}
func headingSpan(h Heading) Span    { return Span{h.Start, h.LineEnd} }
func headingSection(h Heading) Span { return Span{h.Start, h.SectionEnd} }
func linkSpan(l Link) Span          { return Span{l.Start, l.End} }
