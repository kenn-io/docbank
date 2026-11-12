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
	require.Equal(t, []Link{{Form: "markdown", Raw: `[a <span title="]">b</span>](target.md)`, Target: "target.md", Label: `a <span title="]">b</span>`, Start: 2, End: 2 + len(`[a <span title="]">b</span>](target.md)`), Line: 1}}, n.Links)
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
	// Pin Goldmark v2.1.13's container-relative tab padding. The nested
	// row becomes a paragraph even though the root row is indented code.
	for _, tc := range []struct {
		source string
		links  []string
	}{
		{"-\t  [hidden](bad.md)\n\n# Visible\n", []string{}},
		{">  -\t  [hidden](bad.md)\n\n# Visible\n", []string{"bad.md"}},
	} {
		n, err := Parse(t.Context(), []byte(tc.source), Limits{})
		require.NoError(t, err, tc.source)
		require.Equal(t, tc.links, linkTargets(n.Links), tc.source)
		require.Equal(t, []string{"Visible"}, headingTexts(n.Headings), tc.source)
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
	require.Equal(t, []Link{{Form: "markdown", Raw: "[a <!-- [ -->b](target.md)", Target: "target.md", Label: "a <!-- [ -->b", Start: 2, End: 2 + len("[a <!-- [ -->b](target.md)"), Line: 1}}, n.Links)
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

func TestDefinitionDoesNotStartOrderedList(t *testing.T) {
	n, err := Parse(t.Context(), []byte("[r]: target.md\n2. item\nSetext\n=====\n\n[use][r]\n"), Limits{})
	require.NoError(t, err)
	require.Len(t, n.Headings, 1)
	require.Equal(t, "2. item Setext", n.Headings[0].Text)
	require.Equal(t, 1, n.Headings[0].Level)
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
	// Type 7 HTML cannot interrupt the reference-prefix paragraph.
	for _, tc := range []struct {
		source string
		links  []string
	}{
		{"[r]: n.md\n<span>\n[x](ghost.md)\n", []string{"ghost.md"}},
		{"[r]: n.md\n\n<span>\n[x](ghost.md)\n", []string{}},
	} {
		n, err := Parse(t.Context(), []byte(tc.source), Limits{})
		require.NoError(t, err)
		require.Equal(t, tc.links, linkTargets(n.Links), tc.source)
	}
}

func TestDefinitionOnlySetext(t *testing.T) {
	n, err := Parse(t.Context(), []byte("[r]: target.md\n===\n---\n"), Limits{})
	require.NoError(t, err)
	require.Len(t, n.Headings, 1)
	require.Equal(t, "===", n.Headings[0].Text)
}

func TestContainerMultilineDefinitionsBeforeBlocks(t *testing.T) {
	for _, tc := range []struct {
		separator string
		links     []string
	}{
		{"", []string{"note.md", "ghost.md"}},
		{">\n", []string{"note.md"}},
	} {
		src := "> [long\n> label]: note.md\n" + tc.separator + "> <span>\n> [hidden](ghost.md)\n\n[use][long label]\n"
		n, err := Parse(t.Context(), []byte(src), Limits{})
		require.NoError(t, err)
		require.Equal(t, tc.links, linkTargets(n.Links), src)
	}
}

func TestInvalidFenceCannotInterruptContainer(t *testing.T) {
	src := "> [link\n```bad`info\n> label](note.md)\n"
	n, err := Parse(t.Context(), []byte(src), Limits{})
	require.NoError(t, err)
	require.Equal(t, []string{"note.md"}, linkTargets(n.Links))
}

func TestBlockScalarContentTab(t *testing.T) {
	// Pin yaml.v3 v3.0.1 behavior: auto-detected indentation rejects a
	// first-content tab; an explicit indentation indicator admits it.
	for _, tc := range []struct {
		indicator string
		valid     bool
	}{
		{"|-", false}, {"|2-", true},
	} {
		src := "---\ntitle: Good\nunknown: " + tc.indicator + "\n  \tcontent\n---\n# Body\n"
		n, err := Parse(t.Context(), []byte(src), Limits{})
		require.NoError(t, err)
		require.Equal(t, []string{"Body"}, headingTexts(n.Headings))
		if tc.valid {
			require.Equal(t, "valid", frontmatterState(n))
			require.Equal(t, "Good", n.Title)
			require.Equal(t, "\tcontent", n.Frontmatter.Raw["unknown"])
			require.Empty(t, n.Diagnostics)
		} else {
			require.Equal(t, "invalid", frontmatterState(n))
			require.Nil(t, n.Frontmatter.Raw)
			require.Equal(t, "Body", n.Title)
			require.Equal(t, []Diagnostic{{Code: "invalid_frontmatter", Span: Span{0, strings.Index(src, "# Body")}}}, n.Diagnostics)
		}
	}
}

func TestRepeatedContainerDefinitionsBudget(t *testing.T) {
	src := strings.Repeat("> [long\n> label]: n.md\n> 2. item\n\n", 600)
	_, err := Parse(t.Context(), []byte(src), Limits{})
	require.NoError(t, err)
}

func TestDefinitionBeforeRootHeading(t *testing.T) {
	for _, tc := range []struct {
		separator string
		headings  []string
	}{
		{"", nil}, {"\n", []string{"Heading"}},
	} {
		src := "> [r]: target.md\n" + tc.separator + "Heading\n===\n"
		n, err := Parse(t.Context(), []byte(src), Limits{})
		require.NoError(t, err)
		require.Equal(t, tc.headings, headingTexts(n.Headings), src)
	}
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
	for _, tc := range []struct{ definition, blank, content string }{
		{"[r]: target.md\n", "\n", "    [hidden](ghost.md)\n"},
		{"> [r]: target.md\n", ">\n", ">     [hidden](ghost.md)\n"},
	} {
		for _, blank := range []string{"", tc.blank} {
			src := tc.definition + blank + tc.content
			n, err := Parse(t.Context(), []byte(src), Limits{})
			require.NoError(t, err, src)
			want := []string{"ghost.md"}
			if blank != "" {
				want = []string{}
			}
			require.Equal(t, want, linkTargets(n.Links), src)
		}
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
	for _, tc := range []struct{ definition, blank, content string }{
		{"[r]: note.md\n", "\n", "  \t[hidden](ghost.md)\n"},
		{"> > [r]: note.md\n", "> >\n", "> >     [hidden](ghost.md)\n"},
		{"- [r]: note.md\n", "\n", "      [hidden](ghost.md)\n"},
		{"[r]: note.md\n", "\n", "    \"unclosed [hidden](ghost.md)\n"},
	} {
		for _, blank := range []string{"", tc.blank} {
			src := tc.definition + blank + tc.content
			n, err := Parse(t.Context(), []byte(src), Limits{})
			require.NoError(t, err, src)
			want := []string{"ghost.md"}
			if blank != "" {
				want = []string{}
			}
			require.Equal(t, want, linkTargets(n.Links), src)
		}
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
	for _, blank := range []string{"", "\n"} {
		// Keep the invalid title text in the code-hiding companion too.
		src := "[r]: note.md\n" + blank + "    \"title\" trailing [hidden](ghost.md)\n"
		n, err := Parse(t.Context(), []byte(src), Limits{})
		require.NoError(t, err)
		want := []string{"ghost.md"}
		if blank != "" {
			want = []string{}
		}
		require.Equal(t, want, linkTargets(n.Links), src)
	}
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
	require.Equal(t, []string{"ghost.md"}, linkTargets(n.Links))
	require.Equal(t, []string{"Heading"}, headingTexts(n.Headings))
}

func TestTitleLookaheadContainerAndIndent(t *testing.T) {
	for _, tc := range []struct {
		source string
		links  []string
	}{
		{"> [r]: n.md\n\"outside [x](x.md)\"\n", []string{}},
		{"> [r]: n.md\n\n\"outside [x](x.md)\"\n", []string{"x.md"}},
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
	for _, tc := range []struct {
		source   string
		headings []string
	}{
		{"[r]: note.md\n    \"unclosed [hidden](ghost.md)\n```\nend\" [code](code.md)\n```\n", nil},
		{"> [r]: note.md\n>     \"unclosed [hidden](ghost.md)\n# Outside\nend\"\n", []string{"Outside"}},
	} {
		n, err := Parse(t.Context(), []byte(tc.source), Limits{})
		require.NoError(t, err, tc.source)
		require.Equal(t, []string{"ghost.md"}, linkTargets(n.Links), tc.source)
		require.Equal(t, tc.headings, headingTexts(n.Headings), tc.source)
	}
}

func TestRepeatedDefinitionsWithTitlesBudget(t *testing.T) {
	src := strings.Repeat("> [r]: note.md\n>     \"title\"\n", 600) + "\n[use][r]\n"
	n, err := Parse(t.Context(), []byte(src), Limits{})
	require.NoError(t, err)
	require.Equal(t, []string{"note.md"}, linkTargets(n.Links))
}
