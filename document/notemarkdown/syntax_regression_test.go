package notemarkdown

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRawHeadingAndTargetEntityRecovery(t *testing.T) {
	n, err := Parse(t.Context(), []byte("# &bad *emphasis* &amp;\n\n[x](a&bad&amp;b)\n"), Limits{})
	require.NoError(t, err)
	require.Equal(t, "&bad *emphasis* &amp;", n.Headings[0].Text)
	require.Equal(t, []string{"a&bad&b"}, linkTargets(n.Links))
}

func TestAutolinkInLinkLabel(t *testing.T) {
	n, err := Parse(t.Context(), []byte("[<https://example.test>](outer.md)"), Limits{})
	require.NoError(t, err)
	require.Equal(t, []string{"https://example.test"}, linkTargets(n.Links))
}

func TestFrontmatterScalars(t *testing.T) {
	n, err := Parse(t.Context(), []byte("---\ntitle: 2026-11-08 meeting's notes # comment\naliases:\n- one\n- two\ntags: x\n---\n"), Limits{})
	require.NoError(t, err)
	require.Equal(t, "valid", frontmatterState(n))
	require.Equal(t, "2026-11-08 meeting's notes", n.Title)
	require.Equal(t, []string{"one", "two"}, n.Frontmatter.Aliases)
	require.Equal(t, []string{"x"}, n.Frontmatter.Tags)
}

func TestRootFlowLineEndings(t *testing.T) {
	lf := "---\n{title: \"first\nsecond\"}\n---\n"
	a, err := Parse(t.Context(), []byte(lf), Limits{})
	require.NoError(t, err)
	b, err := Parse(t.Context(), []byte(strings.ReplaceAll(lf, "\n", "\r\n")), Limits{})
	require.NoError(t, err)
	require.Equal(t, "valid", frontmatterState(a))
	require.Equal(t, a.Frontmatter.Raw, b.Frontmatter.Raw)
}

func TestReferenceAngleDestination(t *testing.T) {
	n, err := Parse(t.Context(), []byte("[empty][a] [invalid][b]\n\n[a]: <>\n[b]: <x>\"title\"\n"), Limits{})
	require.NoError(t, err)
	require.Equal(t, []string{""}, linkTargets(n.Links))
}

func TestContainerBlankLines(t *testing.T) {
	n, err := Parse(t.Context(), []byte("> [unfinished\n>\n> ](bad.md)\n\n> <div>\n>\n> [valid](ok.md)\n"), Limits{})
	require.NoError(t, err)
	require.Equal(t, []string{"ok.md"}, linkTargets(n.Links))
}

func TestFlowComments(t *testing.T) {
	src := "---\naliases: [one, # ] ignored\n  \"two # literal\"]\n---\n"
	n, err := Parse(t.Context(), []byte(src), Limits{})
	require.NoError(t, err)
	require.Equal(t, "valid", frontmatterState(n))
	require.Equal(t, []string{"one", "two # literal"}, n.Frontmatter.Aliases)
}

func TestHeadingHTMLAttributeBrackets(t *testing.T) {
	n, err := Parse(t.Context(), []byte("# [a <span title=\"]\">b</span>](target.md)\n"), Limits{})
	require.NoError(t, err)
	require.Equal(t, `[a <span title="]">b</span>](target.md)`, n.Headings[0].Text)
}

func TestSelfClosingScriptBlock(t *testing.T) {
	for _, tag := range []string{"script", "style", "pre", "textarea"} {
		src := "<" + tag + "/>\n[shown](shown.md)\n# Heading\n[also shown](next.md)\n"
		n, err := Parse(t.Context(), []byte(src), Limits{})
		require.NoError(t, err, tag)
		require.Empty(t, n.Links, tag)
		require.Empty(t, n.Headings, tag)
	}
	n, err := Parse(t.Context(), []byte("paragraph\n<script/>\n[shown](shown.md)\n"), Limits{})
	require.NoError(t, err)
	require.Empty(t, n.Links)
}

func TestContainerSetextEndsParagraph(t *testing.T) {
	n, err := Parse(t.Context(), []byte("> [unfinished\n> ---\n> ](bad.md)\n\n# Root\n"), Limits{})
	require.NoError(t, err)
	require.Empty(t, n.Links)
	require.Equal(t, []string{"[unfinished", "Root"}, headingTexts(n.Headings))
}

func TestMultilineReferenceLabel(t *testing.T) {
	n, err := Parse(t.Context(), []byte("[use][long label]\n\n[long\nlabel]: target.md\n"), Limits{})
	require.NoError(t, err)
	require.Equal(t, []string{"target.md"}, linkTargets(n.Links))
}

func TestMultilineReferenceTitle(t *testing.T) {
	n, err := Parse(t.Context(), []byte("[use][ref]\n\n[ref]: target.md \"first\n[hidden](wrong.md)\"\n"), Limits{})
	require.NoError(t, err)
	require.Equal(t, []string{"target.md"}, linkTargets(n.Links))
}

func TestSetextAfterReferenceDefinition(t *testing.T) {
	src := []byte("[ref]: target.md\nHeading\n---\n\n[use][ref]\n")
	n, err := Parse(t.Context(), src, Limits{})
	require.NoError(t, err)
	require.Len(t, n.Headings, 1)
	require.Equal(t, "Heading", n.Headings[0].Text)
	require.Equal(t, []string{"target.md"}, linkTargets(n.Links))
}

func TestListTabResidualIndent(t *testing.T) {
	for _, src := range []string{
		"-\t  [hidden](bad.md)\n\n# Visible\n",
		">  -\t  [hidden](bad.md)\n\n# Visible\n",
	} {
		n, err := Parse(t.Context(), []byte(src), Limits{})
		require.NoError(t, err, src)
		require.Empty(t, n.Links, src)
		require.Len(t, n.Headings, 1, src)
	}
}

func TestListTabNormalPadding(t *testing.T) {
	n, err := Parse(t.Context(), []byte(" -\t[shown](one.md)\n\n>  -\t[shown](two.md)\n"), Limits{})
	require.NoError(t, err)
	require.Equal(t, []string{"one.md", "two.md"}, linkTargets(n.Links))
}

func TestDefinitionsAfterMultilineTitle(t *testing.T) {
	src := []byte("[a]: one.md \"first\nsecond\"\n[b]: two.md\n\n[x][a] [y][b]\n")
	n, err := Parse(t.Context(), src, Limits{})
	require.NoError(t, err)
	require.Equal(t, []string{"one.md", "two.md"}, linkTargets(n.Links))
}

func TestHeadingHTMLCommentBrackets(t *testing.T) {
	n, err := Parse(t.Context(), []byte("# [a <!-- [ -->b](target.md)\n"), Limits{})
	require.NoError(t, err)
	require.Equal(t, "[a <!-- [ -->b](target.md)", n.Headings[0].Text)
}

func TestFlowCommentDoesNotInjectValues(t *testing.T) {
	n, err := Parse(t.Context(), []byte("---\ntags: [one # , injected\n  , two]\n---\n"), Limits{})
	require.NoError(t, err)
	require.Equal(t, "valid", frontmatterState(n))
	require.Equal(t, []string{"one", "two"}, n.Frontmatter.Tags)
}

func TestMarkdownNonbreakingSpace(t *testing.T) {
	n, err := Parse(t.Context(), []byte("[link\n\u00a0\nlabel](target.md)\n\nHeading\n---\u00a0\n"), Limits{})
	require.NoError(t, err)
	require.Equal(t, []string{"target.md"}, linkTargets(n.Links))
	require.Empty(t, n.Headings)
}

func TestStandaloneCRLines(t *testing.T) {
	src := []byte("---\rtitle: Sample\r---\r# A\r# B\r")
	n, err := Parse(t.Context(), src, Limits{})
	require.NoError(t, err)
	require.Equal(t, "valid", frontmatterState(n))
	require.Equal(t, "Sample", n.Title)
	require.Len(t, n.Headings, 2)
	require.Equal(t, "# B\r", string(src[headingSpan(n.Headings[1]).Start:headingSpan(n.Headings[1]).End]))
}

func TestEscapedBacktickPrefix(t *testing.T) {
	n, err := Parse(t.Context(), []byte("\\``[x](ghost.md)`"), Limits{})
	require.NoError(t, err)
	require.Empty(t, n.Links)
}

func TestDefinitionDoesNotInterruptList(t *testing.T) {
	n, err := Parse(t.Context(), []byte("[r]: target.md\n2. item\nSetext\n=====\n\n[use][r]\n"), Limits{})
	require.NoError(t, err)
	require.Empty(t, n.Headings)
	require.Equal(t, []string{"target.md"}, linkTargets(n.Links))
}

func TestReferenceShortcutIncompleteSuffix(t *testing.T) {
	n, err := Parse(t.Context(), []byte("[foo][\n\n[foo]: note.md\n"), Limits{})
	require.NoError(t, err)
	require.Equal(t, []string{"note.md"}, linkTargets(n.Links))
}

func TestUnicodeReferenceLabelLimit(t *testing.T) {
	label := strings.Repeat("é", 500)
	n, err := Parse(t.Context(), []byte("[use]["+label+"]\n\n["+label+"]: note.md\n"), Limits{})
	require.NoError(t, err)
	require.Equal(t, []string{"note.md"}, linkTargets(n.Links))
}

func TestEmptyDestinationWithTitle(t *testing.T) {
	n, err := Parse(t.Context(), []byte("[x]( \"my title\")"), Limits{})
	require.NoError(t, err)
	require.Equal(t, []string{""}, linkTargets(n.Links))
}

func TestShortHTMLComments(t *testing.T) {
	n, err := Parse(t.Context(), []byte("text <!--> [x](note.md) <!-- tail -->"), Limits{})
	require.NoError(t, err)
	require.Equal(t, []string{"note.md"}, linkTargets(n.Links))
}

func TestRawHTMLBlockEnd(t *testing.T) {
	n, err := Parse(t.Context(), []byte("<pre>\n</style>\n# Visible\n"), Limits{})
	require.NoError(t, err)
	require.Len(t, n.Headings, 1)
}

func TestYAMLPlainApostrophes(t *testing.T) {
	n, err := Parse(t.Context(), []byte("---\nowner's: value\naliases: [O'Brien]\ntitle: Good\n---\n"), Limits{})
	require.NoError(t, err)
	require.Equal(t, "valid", frontmatterState(n))
	require.Equal(t, []string{"O'Brien"}, n.Frontmatter.Aliases)
}

func TestYAMLNullAndUnicode(t *testing.T) {
	n, err := Parse(t.Context(), []byte("---\n{title: Good, unknown: , aliases: [\u00a0Name\u00a0]}\n---\n"), Limits{})
	require.NoError(t, err)
	require.Equal(t, "valid", frontmatterState(n))
	require.Equal(t, []string{"\u00a0Name\u00a0"}, n.Frontmatter.Aliases)
}

func TestQuoteBlankInsideListFence(t *testing.T) {
	n, err := Parse(t.Context(), []byte("> - ```\n>\n>   [hidden](ghost.md)\n>   ```\n"), Limits{})
	require.NoError(t, err)
	require.Empty(t, n.Links)
}

func TestQuoteTabIndent(t *testing.T) {
	n, err := Parse(t.Context(), []byte(">\t  [hidden](ghost.md)\n"), Limits{})
	require.NoError(t, err)
	require.Empty(t, n.Links)
}

func TestReferenceOnlyBeforeHTML(t *testing.T) {
	n, err := Parse(t.Context(), []byte("[r]: n.md\n<span>\n[x](ghost.md)\n"), Limits{})
	require.NoError(t, err)
	require.Empty(t, n.Links)
}

func TestDefinitionOnlySetext(t *testing.T) {
	n, err := Parse(t.Context(), []byte("[r]: target.md\n===\n---\n"), Limits{})
	require.NoError(t, err)
	require.Len(t, n.Headings, 1)
	require.Equal(t, "===", n.Headings[0].Text)
}

func TestContainerMultilineDefinitionsBeforeBlocks(t *testing.T) {
	src := "> [long\n> label]: note.md\n> <span>\n> [hidden](ghost.md)\n\n[use][long label]\n"
	n, err := Parse(t.Context(), []byte(src), Limits{})
	require.NoError(t, err)
	require.Equal(t, []string{"note.md"}, linkTargets(n.Links))
}

func TestInvalidFenceCannotInterruptContainer(t *testing.T) {
	src := "> [link\n```bad`info\n> label](note.md)\n"
	n, err := Parse(t.Context(), []byte(src), Limits{})
	require.NoError(t, err)
	require.Equal(t, []string{"note.md"}, linkTargets(n.Links))
}

func TestBlockScalarContentTab(t *testing.T) {
	src := "---\ntitle: Good\nunknown: |-\n  \tcontent\n---\n"
	n, err := Parse(t.Context(), []byte(src), Limits{})
	require.NoError(t, err)
	require.Equal(t, "valid", frontmatterState(n))
	require.Equal(t, "Good", n.Title)
}

func TestRepeatedContainerDefinitionsBudget(t *testing.T) {
	src := strings.Repeat("> [long\n> label]: n.md\n> 2. item\n\n", 600)
	_, err := Parse(t.Context(), []byte(src), Limits{})
	require.NoError(t, err)
}

func TestDefinitionBeforeRootHeading(t *testing.T) {
	n, err := Parse(t.Context(), []byte("> [r]: target.md\nHeading\n===\n"), Limits{})
	require.NoError(t, err)
	require.Len(t, n.Headings, 1)
	require.Equal(t, "Heading", n.Headings[0].Text)
}

func TestCompactMappingInUnknownSequence(t *testing.T) {
	src := "---\ntitle: Good\nextra:\n  - name: Other\n    kind: test\n---\n"
	n, err := Parse(t.Context(), []byte(src), Limits{})
	require.NoError(t, err)
	require.Equal(t, "valid", frontmatterState(n))
	require.Equal(t, "Good", n.Title)
}

func TestOrdinaryContainerParagraphBudget(t *testing.T) {
	src := "> [ordinary] prose\n" + strings.Repeat("> 2. item\n", 600)
	_, err := Parse(t.Context(), []byte(src), Limits{})
	require.NoError(t, err)
}

func TestReferenceBeforeIndentedCode(t *testing.T) {
	for _, src := range []string{
		"[r]: target.md\n    [hidden](ghost.md)\n",
		"> [r]: target.md\n>     [hidden](ghost.md)\n",
	} {
		n, err := Parse(t.Context(), []byte(src), Limits{})
		require.NoError(t, err, src)
		require.Empty(t, n.Links, src)
	}
}

func TestSequenceFlowAndCompactMappingSpacing(t *testing.T) {
	src := "---\ntitle: Good\nextra:\n  - {name: Other}\n  -   name: More\n      kind: test\n---\n"
	n, err := Parse(t.Context(), []byte(src), Limits{})
	require.NoError(t, err)
	require.Equal(t, "valid", frontmatterState(n))
	require.Equal(t, "Good", n.Title)
}

func TestGrowingDefinitionLikeParagraphBudget(t *testing.T) {
	for _, start := range []string{"> [unfinished\n", "> [ref]: note.md\n> prose\n"} {
		src := start + strings.Repeat("> 2. item\n", 1200)
		_, err := Parse(t.Context(), []byte(src), Limits{})
		require.NoError(t, err)
	}
}

func TestReferenceBeforeMixedIndent(t *testing.T) {
	for _, src := range []string{
		"[r]: note.md\n  \t[hidden](ghost.md)\n",
		"> > [r]: note.md\n> >     [hidden](ghost.md)\n",
		"- [r]: note.md\n      [hidden](ghost.md)\n",
		"[r]: note.md\n    \"unclosed [hidden](ghost.md)\n",
	} {
		n, err := Parse(t.Context(), []byte(src), Limits{})
		require.NoError(t, err, src)
		require.Empty(t, n.Links, src)
	}
}

func TestIncompleteDefinitionSuffixBudget(t *testing.T) {
	for _, start := range []string{"> [r]: note.md \"unfinished\n", "> [r]: note.md\n> [unfinished\n"} {
		_, err := Parse(t.Context(), []byte(start+strings.Repeat("> 2. item\n", 1200)), Limits{})
		require.NoError(t, err)
	}
}

func TestIndentedReferenceTitle(t *testing.T) {
	src := "[r]: note.md\n    \"title\n[hidden](ghost.md)\"\n\n[use][r]\n"
	n, err := Parse(t.Context(), []byte(src), Limits{})
	require.NoError(t, err)
	require.Equal(t, []string{"note.md"}, linkTargets(n.Links))
}

func TestLazySetextDoesNotEndParagraph(t *testing.T) {
	n, err := Parse(t.Context(), []byte("> [a\n===\n> b](note.md)\n"), Limits{})
	require.NoError(t, err)
	require.Equal(t, []string{"note.md"}, linkTargets(n.Links))
}

func TestInvalidContinuationTitleDoesNotHideCode(t *testing.T) {
	src := "[r]: note.md\n    \"title\" trailing [hidden](ghost.md)\n"
	n, err := Parse(t.Context(), []byte(src), Limits{})
	require.NoError(t, err)
	require.Empty(t, n.Links)
}

func TestInvalidDestinationDoesNotRepeatWork(t *testing.T) {
	src := "> [r]: <unfinished\n" + strings.Repeat("> 2. item\n", 1200)
	_, err := Parse(t.Context(), []byte(src), Limits{})
	require.NoError(t, err)
}

func TestTitleLookaheadStopsAtHeading(t *testing.T) {
	src := "[r]: note.md\n    \"unclosed [hidden](ghost.md)\n# Heading\nend\"\n"
	n, err := Parse(t.Context(), []byte(src), Limits{})
	require.NoError(t, err)
	require.Empty(t, n.Links)
	require.Len(t, n.Headings, 1)
}

func TestTitleLookaheadContainerAndIndent(t *testing.T) {
	for _, tc := range []struct {
		source string
		links  []string
	}{
		{"> [r]: n.md\n\"outside [x](x.md)\"\n", []string{"x.md"}},
		{"[r]: n.md\n    \"one\n    # literal\n[ghost](g.md)\"\n\n[use][r]\n", []string{"n.md"}},
		{"> [r]: n.md\n>     \"one\n>     # literal\n> [ghost](g.md)\"\n\n[use][r]\n", []string{"n.md"}},
	} {
		n, err := Parse(t.Context(), []byte(tc.source), Limits{})
		require.NoError(t, err, tc.source)
		require.Equal(t, tc.links, linkTargets(n.Links), tc.source)
	}
}

func TestRepeatedUnterminatedTitleLookaheadBudget(t *testing.T) {
	for _, title := range []string{"(", "\"", "'"} {
		src := strings.Repeat("[r]: note.md\n    "+title+"unfinished\n\n", 1200) + "[use][r]\n"
		n, err := Parse(t.Context(), []byte(src), Limits{})
		require.NoError(t, err, title)
		require.Equal(t, []string{"note.md"}, linkTargets(n.Links), title)
	}
}

func TestInvalidReferenceTailBudget(t *testing.T) {
	for _, start := range []string{"> [r]: note.md trailing\n", "> [r]: note.md \"title\" trailing\n"} {
		_, err := Parse(t.Context(), []byte(start+strings.Repeat("> 2. item\n", 1200)), Limits{})
		require.NoError(t, err)
	}
}

func TestTitleLookaheadStopsAtFenceAndContainerExit(t *testing.T) {
	for _, src := range []string{
		"[r]: note.md\n    \"unclosed [hidden](ghost.md)\n```\nend\"\n```\n",
		"> [r]: note.md\n>     \"unclosed [hidden](ghost.md)\n# Outside\nend\"\n",
	} {
		n, err := Parse(t.Context(), []byte(src), Limits{})
		require.NoError(t, err, src)
		require.Empty(t, n.Links, src)
	}
}

func TestRepeatedDefinitionsWithTitlesBudget(t *testing.T) {
	src := strings.Repeat("> [r]: note.md\n>     \"title\"\n", 600) + "\n[use][r]\n"
	n, err := Parse(t.Context(), []byte(src), Limits{})
	require.NoError(t, err)
	require.Equal(t, []string{"note.md"}, linkTargets(n.Links))
}
