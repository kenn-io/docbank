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

func TestReviewContainerMultilineDefinitionsBeforeBlocks(t *testing.T) {
	src := "> [long\n> label]: note.md\n> <span>\n> [hidden](ghost.md)\n\n[use][long label]\n"
	n, err := Parse(t.Context(), []byte(src))
	require.NoError(t, err)
	require.Equal(t, []string{"note.md"}, linkTargets(n.Links))
}

func TestReviewInvalidFenceCannotInterruptContainer(t *testing.T) {
	src := "> [link\n```bad`info\n> label](note.md)\n"
	n, err := Parse(t.Context(), []byte(src))
	require.NoError(t, err)
	require.Equal(t, []string{"note.md"}, linkTargets(n.Links))
}

func TestReviewBlockScalarContentTab(t *testing.T) {
	src := "---\ntitle: Good\nunknown: |-\n  \tcontent\n---\n"
	n, err := Parse(t.Context(), []byte(src))
	require.NoError(t, err)
	require.Equal(t, "valid", n.FrontmatterState)
	require.Equal(t, "Good", n.Metadata.Title)
}

func TestReviewRepeatedContainerDefinitionsBudget(t *testing.T) {
	src := strings.Repeat("> [long\n> label]: n.md\n> 2. item\n\n", 600)
	_, err := Parse(t.Context(), []byte(src))
	require.NoError(t, err)
}

func TestReviewDefinitionBeforeRootHeading(t *testing.T) {
	n, err := Parse(t.Context(), []byte("> [r]: target.md\nHeading\n===\n"))
	require.NoError(t, err)
	require.Len(t, n.Headings, 1)
	require.Equal(t, "Heading", n.Headings[0].Text)
}

func TestReviewCompactMappingInUnknownSequence(t *testing.T) {
	src := "---\ntitle: Good\nextra:\n  - name: Other\n    kind: test\n---\n"
	n, err := Parse(t.Context(), []byte(src))
	require.NoError(t, err)
	require.Equal(t, "valid", n.FrontmatterState)
	require.Equal(t, "Good", n.Metadata.Title)
}

func TestReviewOrdinaryContainerParagraphBudget(t *testing.T) {
	src := "> [ordinary] prose\n" + strings.Repeat("> 2. item\n", 600)
	_, err := Parse(t.Context(), []byte(src))
	require.NoError(t, err)
}

func TestReviewReferenceBeforeIndentedCode(t *testing.T) {
	for _, src := range []string{
		"[r]: target.md\n    [hidden](ghost.md)\n",
		"> [r]: target.md\n>     [hidden](ghost.md)\n",
	} {
		n, err := Parse(t.Context(), []byte(src))
		require.NoError(t, err, src)
		require.Empty(t, n.Links, src)
	}
}

func TestReviewSequenceFlowAndCompactMappingSpacing(t *testing.T) {
	src := "---\ntitle: Good\nextra:\n  - {name: Other}\n  -   name: More\n      kind: test\n---\n"
	n, err := Parse(t.Context(), []byte(src))
	require.NoError(t, err)
	require.Equal(t, "valid", n.FrontmatterState)
	require.Equal(t, "Good", n.Metadata.Title)
}

func TestReviewGrowingDefinitionLikeParagraphBudget(t *testing.T) {
	for _, start := range []string{"> [unfinished\n", "> [ref]: note.md\n> prose\n"} {
		src := start + strings.Repeat("> 2. item\n", 1200)
		_, err := Parse(t.Context(), []byte(src))
		require.NoError(t, err)
	}
}

func TestReviewReferenceBeforeMixedIndent(t *testing.T) {
	for _, src := range []string{
		"[r]: note.md\n  \t[hidden](ghost.md)\n",
		"> > [r]: note.md\n> >     [hidden](ghost.md)\n",
		"- [r]: note.md\n      [hidden](ghost.md)\n",
		"[r]: note.md\n    \"unclosed [hidden](ghost.md)\n",
	} {
		n, err := Parse(t.Context(), []byte(src))
		require.NoError(t, err, src)
		require.Empty(t, n.Links, src)
	}
}

func TestReviewIncompleteDefinitionSuffixBudget(t *testing.T) {
	for _, start := range []string{"> [r]: note.md \"unfinished\n", "> [r]: note.md\n> [unfinished\n"} {
		_, err := Parse(t.Context(), []byte(start+strings.Repeat("> 2. item\n", 1200)))
		require.NoError(t, err)
	}
}

func TestReviewIndentedReferenceTitle(t *testing.T) {
	src := "[r]: note.md\n    \"title\n[hidden](ghost.md)\"\n\n[use][r]\n"
	n, err := Parse(t.Context(), []byte(src))
	require.NoError(t, err)
	require.Equal(t, []string{"note.md"}, linkTargets(n.Links))
}

func TestReviewLazySetextDoesNotEndParagraph(t *testing.T) {
	n, err := Parse(t.Context(), []byte("> [a\n===\n> b](note.md)\n"))
	require.NoError(t, err)
	require.Equal(t, []string{"note.md"}, linkTargets(n.Links))
}

func TestReviewInvalidContinuationTitleDoesNotHideCode(t *testing.T) {
	src := "[r]: note.md\n    \"title\" trailing [hidden](ghost.md)\n"
	n, err := Parse(t.Context(), []byte(src))
	require.NoError(t, err)
	require.Empty(t, n.Links)
}

func TestReviewInvalidDestinationDoesNotRepeatWork(t *testing.T) {
	src := "> [r]: <unfinished\n" + strings.Repeat("> 2. item\n", 1200)
	_, err := Parse(t.Context(), []byte(src))
	require.NoError(t, err)
}

func TestReviewTitleLookaheadStopsAtHeading(t *testing.T) {
	src := "[r]: note.md\n    \"unclosed [hidden](ghost.md)\n# Heading\nend\"\n"
	n, err := Parse(t.Context(), []byte(src))
	require.NoError(t, err)
	require.Empty(t, n.Links)
	require.Len(t, n.Headings, 1)
}

func TestReviewInvalidReferenceTailBudget(t *testing.T) {
	for _, start := range []string{"> [r]: note.md trailing\n", "> [r]: note.md \"title\" trailing\n"} {
		_, err := Parse(t.Context(), []byte(start+strings.Repeat("> 2. item\n", 1200)))
		require.NoError(t, err)
	}
}

func TestReviewTitleLookaheadStopsAtFenceAndContainerExit(t *testing.T) {
	for _, src := range []string{
		"[r]: note.md\n    \"unclosed [hidden](ghost.md)\n```\nend\"\n```\n",
		"> [r]: note.md\n>     \"unclosed [hidden](ghost.md)\n# Outside\nend\"\n",
	} {
		n, err := Parse(t.Context(), []byte(src))
		require.NoError(t, err, src)
		require.Empty(t, n.Links, src)
	}
}

func TestReviewRepeatedDefinitionsWithTitlesBudget(t *testing.T) {
	src := strings.Repeat("> [r]: note.md\n>     \"title\"\n", 600) + "\n[use][r]\n"
	n, err := Parse(t.Context(), []byte(src))
	require.NoError(t, err)
	require.Equal(t, []string{"note.md"}, linkTargets(n.Links))
}
