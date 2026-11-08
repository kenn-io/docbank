package notemarkdown

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestReviewEntityRecovery(t *testing.T) {
	n, err := Parse(t.Context(), []byte("# &bad *emphasis* &amp;\n\n[x](a&bad&amp;b)\n"))
	require.NoError(t, err)
	require.Equal(t, "&bad emphasis &", n.Headings[0].Text)
	require.Equal(t, []string{"a&bad&b"}, linkTargets(n.Links))
}

func TestReviewAutolinkInLinkLabel(t *testing.T) {
	n, err := Parse(t.Context(), []byte("[<https://example.test>](outer.md)"))
	require.NoError(t, err)
	require.Equal(t, []string{"https://example.test"}, linkTargets(n.Links))
}

func TestReviewFrontmatterScalars(t *testing.T) {
	n, err := Parse(t.Context(), []byte("---\ntitle: 2026-11-08 meeting's notes # comment\naliases:\n- one\n- two\ntags: x\n---\n"))
	require.NoError(t, err)
	require.Equal(t, "valid", n.FrontmatterState)
	require.Equal(t, Metadata{Title: "2026-11-08 meeting's notes", Aliases: []string{"one", "two"}, Tags: []string{"x"}}, n.Metadata)
}

func TestReviewRootFlowLineEndings(t *testing.T) {
	lf := "---\n{title: \"first\nsecond\"}\n---\n"
	a, err := Parse(t.Context(), []byte(lf))
	require.NoError(t, err)
	b, err := Parse(t.Context(), []byte(strings.ReplaceAll(lf, "\n", "\r\n")))
	require.NoError(t, err)
	require.Equal(t, "valid", a.FrontmatterState)
	require.Equal(t, a.Metadata, b.Metadata)
}

func TestReviewReferenceAngleDestination(t *testing.T) {
	n, err := Parse(t.Context(), []byte("[empty][a] [invalid][b]\n\n[a]: <>\n[b]: <x>\"title\"\n"))
	require.NoError(t, err)
	require.Equal(t, []string{""}, linkTargets(n.Links))
}

func TestReviewContainerBlankLines(t *testing.T) {
	n, err := Parse(t.Context(), []byte("> [unfinished\n>\n> ](bad.md)\n\n> <div>\n>\n> [valid](ok.md)\n"))
	require.NoError(t, err)
	require.Equal(t, []string{"ok.md"}, linkTargets(n.Links))
}

func TestReviewFlowComments(t *testing.T) {
	src := "---\naliases: [one, # ] ignored\n  \"two # literal\"]\n---\n"
	n, err := Parse(t.Context(), []byte(src))
	require.NoError(t, err)
	require.Equal(t, "valid", n.FrontmatterState)
	require.Equal(t, []string{"one", "two # literal"}, n.Metadata.Aliases)
}

func TestReviewHeadingHTMLAttributeBrackets(t *testing.T) {
	n, err := Parse(t.Context(), []byte("# [a <span title=\"]\">b</span>](target.md)\n"))
	require.NoError(t, err)
	require.Equal(t, "a b", n.Headings[0].Text)
}

func TestReviewSelfClosingScriptBlock(t *testing.T) {
	n, err := Parse(t.Context(), []byte("<script/>\n\n# Heading\n"))
	require.NoError(t, err)
	require.Len(t, n.Headings, 1)
}

func TestReviewSetextAndThematicPrecedence(t *testing.T) {
	n, err := Parse(t.Context(), []byte("Heading\n---\n\n* * *\n\n# Next\n"))
	require.NoError(t, err)
	require.Len(t, n.Headings, 2)
	require.Equal(t, "Heading", n.Headings[0].Text)
	require.Equal(t, 2, n.Headings[0].Level)
	require.Equal(t, "Next", n.Headings[1].Text)
}

func TestReviewContainerSetextEndsParagraph(t *testing.T) {
	n, err := Parse(t.Context(), []byte("> [unfinished\n> ---\n> ](bad.md)\n\n# Root\n"))
	require.NoError(t, err)
	require.Empty(t, n.Links)
	require.Len(t, n.Headings, 1)
	require.Equal(t, "Root", n.Headings[0].Text)
}

func TestReviewEmptyListCannotInterruptParagraph(t *testing.T) {
	n, err := Parse(t.Context(), []byte("Heading\n-\n\n# Next\n"))
	require.NoError(t, err)
	require.Len(t, n.Headings, 2)
	require.Equal(t, "Heading", n.Headings[0].Text)
	require.Equal(t, 2, n.Headings[0].Level)
}

func TestReviewMultilineReferenceLabel(t *testing.T) {
	n, err := Parse(t.Context(), []byte("[use][long label]\n\n[long\nlabel]: target.md\n"))
	require.NoError(t, err)
	require.Equal(t, []string{"target.md"}, linkTargets(n.Links))
}

func TestReviewMultilineReferenceTitle(t *testing.T) {
	n, err := Parse(t.Context(), []byte("[use][ref]\n\n[ref]: target.md \"first\n[hidden](wrong.md)\"\n"))
	require.NoError(t, err)
	require.Equal(t, []string{"target.md"}, linkTargets(n.Links))
}
