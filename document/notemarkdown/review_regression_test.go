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

func TestReviewSetextAfterReferenceDefinition(t *testing.T) {
	src := []byte("[ref]: target.md\nHeading\n---\n\n[use][ref]\n")
	n, err := Parse(t.Context(), src)
	require.NoError(t, err)
	require.Len(t, n.Headings, 1)
	require.Equal(t, "Heading", n.Headings[0].Text)
	require.Equal(t, []string{"target.md"}, linkTargets(n.Links))
	require.Equal(t, "target.md", string(src[n.Links[0].DestinationSpan.Start:n.Links[0].DestinationSpan.End]))
}

func TestReviewListTabResidualIndent(t *testing.T) {
	for _, src := range []string{
		"-\t  [hidden](bad.md)\n\n# Visible\n",
		">  -\t  [hidden](bad.md)\n\n# Visible\n",
	} {
		n, err := Parse(t.Context(), []byte(src))
		require.NoError(t, err, src)
		require.Empty(t, n.Links, src)
		require.Len(t, n.Headings, 1, src)
	}
}

func TestReviewListTabNormalPadding(t *testing.T) {
	n, err := Parse(t.Context(), []byte(" -\t[shown](one.md)\n\n>  -\t[shown](two.md)\n"))
	require.NoError(t, err)
	require.Equal(t, []string{"one.md", "two.md"}, linkTargets(n.Links))
}

func TestReviewDefinitionsAfterMultilineTitle(t *testing.T) {
	src := []byte("[a]: one.md \"first\nsecond\"\n[b]: two.md\n\n[x][a] [y][b]\n")
	n, err := Parse(t.Context(), src)
	require.NoError(t, err)
	require.Equal(t, []string{"one.md", "two.md"}, linkTargets(n.Links))
}

func TestReviewHeadingHTMLCommentBrackets(t *testing.T) {
	n, err := Parse(t.Context(), []byte("# [a <!-- [ -->b](target.md)\n"))
	require.NoError(t, err)
	require.Equal(t, "a b", n.Headings[0].Text)
}

func TestReviewFlowCommentDoesNotInjectValues(t *testing.T) {
	n, err := Parse(t.Context(), []byte("---\ntags: [one # , injected\n  , two]\n---\n"))
	require.NoError(t, err)
	require.Equal(t, "valid", n.FrontmatterState)
	require.Equal(t, []string{"one", "two"}, n.Metadata.Tags)
}

func TestReviewMarkdownNonbreakingSpace(t *testing.T) {
	n, err := Parse(t.Context(), []byte("[link\n\u00a0\nlabel](target.md)\n\nHeading\n---\u00a0\n"))
	require.NoError(t, err)
	require.Equal(t, []string{"target.md"}, linkTargets(n.Links))
	require.Empty(t, n.Headings)
}

func TestReviewStandaloneCRLines(t *testing.T) {
	src := []byte("---\rtitle: Sample\r---\r# A\r# B\r")
	n, err := Parse(t.Context(), src)
	require.NoError(t, err)
	require.Equal(t, "valid", n.FrontmatterState)
	require.Equal(t, "Sample", n.Metadata.Title)
	require.Len(t, n.Headings, 2)
	require.Equal(t, "# B\r", string(src[n.Headings[1].Span.Start:n.Headings[1].Span.End]))
}

func TestReviewEmptyContainerDoesNotStartParagraph(t *testing.T) {
	for _, prefix := range []string{">\n", "-\n"} {
		n, err := Parse(t.Context(), []byte(prefix+"Heading\n===\n"))
		require.NoError(t, err)
		require.Len(t, n.Headings, 1, prefix)
		require.Equal(t, "Heading", n.Headings[0].Text)
	}
}

func TestReviewEscapedBacktickPrefix(t *testing.T) {
	n, err := Parse(t.Context(), []byte("\\``[x](ghost.md)`"))
	require.NoError(t, err)
	require.Empty(t, n.Links)
}

func TestReviewDefinitionDoesNotInterruptList(t *testing.T) {
	n, err := Parse(t.Context(), []byte("[r]: target.md\n2. item\nSetext\n=====\n\n[use][r]\n"))
	require.NoError(t, err)
	require.Empty(t, n.Headings)
	require.Equal(t, []string{"target.md"}, linkTargets(n.Links))
}

func TestReviewReferenceShortcutIncompleteSuffix(t *testing.T) {
	n, err := Parse(t.Context(), []byte("[foo][\n\n[foo]: note.md\n"))
	require.NoError(t, err)
	require.Equal(t, []string{"note.md"}, linkTargets(n.Links))
}

func TestReviewUnicodeReferenceLabelLimit(t *testing.T) {
	label := strings.Repeat("é", 500)
	n, err := Parse(t.Context(), []byte("[use]["+label+"]\n\n["+label+"]: note.md\n"))
	require.NoError(t, err)
	require.Equal(t, []string{"note.md"}, linkTargets(n.Links))
}

func TestReviewEmptyDestinationWithTitle(t *testing.T) {
	n, err := Parse(t.Context(), []byte("[x]( \"my title\")"))
	require.NoError(t, err)
	require.Equal(t, []string{""}, linkTargets(n.Links))
}

func TestReviewShortHTMLComments(t *testing.T) {
	n, err := Parse(t.Context(), []byte("text <!--> [x](note.md) <!-- tail -->"))
	require.NoError(t, err)
	require.Equal(t, []string{"note.md"}, linkTargets(n.Links))
}

func TestReviewRawHTMLBlockEnd(t *testing.T) {
	n, err := Parse(t.Context(), []byte("<pre>\n</style>\n# Visible\n"))
	require.NoError(t, err)
	require.Len(t, n.Headings, 1)
}

func TestReviewYAMLPlainApostrophes(t *testing.T) {
	n, err := Parse(t.Context(), []byte("---\nowner's: value\naliases: [O'Brien]\ntitle: Good\n---\n"))
	require.NoError(t, err)
	require.Equal(t, "valid", n.FrontmatterState)
	require.Equal(t, []string{"O'Brien"}, n.Metadata.Aliases)
}

func TestReviewYAMLNullAndUnicode(t *testing.T) {
	n, err := Parse(t.Context(), []byte("---\n{title: Good, unknown: , aliases: [\u00a0Name\u00a0]}\n---\n"))
	require.NoError(t, err)
	require.Equal(t, "valid", n.FrontmatterState)
	require.Equal(t, []string{"\u00a0Name\u00a0"}, n.Metadata.Aliases)
}

func TestReviewQuoteBlankInsideListFence(t *testing.T) {
	n, err := Parse(t.Context(), []byte("> - ```\n>\n>   [hidden](ghost.md)\n>   ```\n"))
	require.NoError(t, err)
	require.Empty(t, n.Links)
}

func TestReviewQuoteTabIndent(t *testing.T) {
	n, err := Parse(t.Context(), []byte(">\t  [hidden](ghost.md)\n"))
	require.NoError(t, err)
	require.Empty(t, n.Links)
}

func TestReviewReferenceOnlyBeforeHTML(t *testing.T) {
	n, err := Parse(t.Context(), []byte("[r]: n.md\n<span>\n[x](ghost.md)\n"))
	require.NoError(t, err)
	require.Empty(t, n.Links)
}

func TestReviewDefinitionOnlySetext(t *testing.T) {
	n, err := Parse(t.Context(), []byte("[r]: target.md\n===\n---\n"))
	require.NoError(t, err)
	require.Len(t, n.Headings, 1)
	require.Equal(t, "===", n.Headings[0].Text)
}
