package markdownrender

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/tui/termtext"
)

func TestRenderLinesSanitizesStoredControlSequences(t *testing.T) {
	assert := assert.New(t)
	lines, err := RenderLines(
		"## Steps\n\n- run `kata show`\x1b]2;unsafe\x07",
		Options{Width: 40},
	)
	require.NoError(t, err)
	got := strings.Join(lines, "\n")
	assert.Contains(termtext.StripANSI(got), "Steps")
	assert.Contains(termtext.StripANSI(got), "kata show")
	assert.NotContains(got, "unsafe")
	assert.NotContains(got, "## Steps")
}

func TestRenderLinesSanitizesDecodedControlEntities(t *testing.T) {
	assert := assert.New(t)
	lines, err := RenderLines(
		"**safe**&#27;]52;c;clipboard&#7;&#x202E;spoof",
		Options{Width: 40},
	)
	require.NoError(t, err)
	got := strings.Join(lines, "\n")
	assert.Contains(got, "\x1b[1m")
	assert.NotContains(got, "\x1b]52;")
	assert.NotContains(got, "\x07")
	assert.NotContains(got, "clipboard")
	assert.NotContains(got, "\u202e")
	assert.Contains(got, "spoof")
}

func TestRenderLinesSanitizesSemicolonlessControlEntities(t *testing.T) {
	lines, err := RenderLines(
		"before&#27[31mred&#27[0mafter", Options{Width: 80},
	)
	require.NoError(t, err)
	assert.Equal(t, []string{"beforeredafter"}, lines)
}

func TestTerminalRendererSanitizesDecodedControlEntities(t *testing.T) {
	got, err := renderMarkdownDocument(
		"before&#27;[31mred&#27;[0mafter", Options{Width: 80},
	)
	require.NoError(t, err)
	assert.Equal(t, "before[31mred[0mafter\n", got)
	assert.NotContains(t, got, "\x1b")
}

func TestRenderLinesRejectsDecodedConceal(t *testing.T) {
	assert := assert.New(t)
	lines, err := RenderLines(
		"before&#27;[8mvisible&#27;[31mred",
		Options{Width: 80},
	)
	require.NoError(t, err)
	got := strings.Join(lines, "\n")

	assert.Equal("beforevisiblered", termtext.StripANSI(got))
	assert.NotContains(got, "\x1b[8m")
	assert.NotContains(got, "\x1b[31m")
}

func TestTerminalRendererFormatsIssueMarkdown(t *testing.T) {
	assert := assert.New(t)
	background := "236"
	input := "## Steps\n\n" +
		"> Keep context\n\n" +
		"1. **Open** [issue](https://example.com)\n" +
		"2. [x] Comment with `kata comment`\n\n" +
		"```go\nfmt.Println(\"ok\")\n```\n\n" +
		"| Field | Value |\n| --- | --- |\n| Status | open |\n\n" +
		"![diagram](https://example.com/diagram.png)\n"
	got, err := renderMarkdownDocument(
		input, Options{Width: 80, CodeBlockBackground: &background},
	)
	require.NoError(t, err)

	stripped := termtext.StripANSI(strings.TrimSpace(got))
	for _, sub := range []string{
		"Steps", "| Keep context",
		"1. Open issue", "2. [x] Comment with", "`kata comment`",
		"fmt.Println(\"ok\")",
		"╭", "╰", "│Field", "│Value", "│Status", "│open",
		"[image: diagram]", "https://example.com/diagram.png",
	} {
		assert.Contains(stripped, sub)
	}
	assert.Contains(got, "\x1b[1mSteps\x1b[22m")
	assert.Contains(got, "issue \x1b[4mhttps://example.com\x1b[24m")
	assert.Contains(got, "\x1b[48;5;236mfmt.Println(\"ok\")\x1b[49m")
}

func TestTerminalRendererRendersTableWithBorders(t *testing.T) {
	assert := assert.New(t)
	input := "| Name | Count |\n| --- | --- |\n| alpha | 1 |\n| beta | 2 |\n"
	got, err := renderMarkdownDocument(input, Options{Width: 40})
	require.NoError(t, err)
	stripped := termtext.StripANSI(got)
	assert.Contains(stripped, "╭")
	assert.Contains(stripped, "╰")
	assert.Contains(stripped, "│")
	assert.Contains(stripped, "├")
	assert.Contains(stripped, "Name")
	assert.Contains(stripped, "alpha")
	assert.Contains(stripped, "beta")
	assert.NotContains(stripped, "| --- |")
}

func TestTerminalRendererRendersTableAtWidth(t *testing.T) {
	input := "| A | B |\n| --- | --- |\n| x | y |\n"
	got, err := renderMarkdownDocument(input, Options{Width: 20})
	require.NoError(t, err)
	for line := range strings.SplitSeq(got, "\n") {
		if line != "" {
			assert.LessOrEqual(t, ansi.StringWidth(line), 20)
		}
	}
}

func TestTerminalRendererKeepsHeadingBoldAfterNestedStrong(t *testing.T) {
	got, err := renderMarkdownDocument(
		"## Before **nested** after\n", Options{Width: 80},
	)
	require.NoError(t, err)
	assert.Equal(t, "\x1b[1mBefore nested after\x1b[22m\n", got)
}

func TestTerminalRendererFormatsDefinitionLists(t *testing.T) {
	got, err := renderMarkdownDocument(
		"Term\n: Definition\n", Options{Width: 80},
	)
	require.NoError(t, err)
	assert.Equal(t, "Term\nDefinition\n", got)
}

func TestTerminalRendererKeepsLinkAndImageDestinationsVisible(t *testing.T) {
	lines, err := RenderLines(
		"[issue](https://example.com/issues/1)\n\n"+
			"![diagram](https://example.com/diagram.png)\n",
		Options{Width: 80},
	)
	require.NoError(t, err)
	assert.Equal(t,
		"issue https://example.com/issues/1\n\n"+
			"[image: diagram] https://example.com/diagram.png",
		termtext.StripANSI(strings.Join(lines, "\n")),
	)
}

func TestTerminalRendererDoesNotAddMailtoToAutolinkedEmail(t *testing.T) {
	got, err := renderMarkdownDocument(
		"<user@example.com>\n", Options{Width: 80},
	)
	require.NoError(t, err)
	assert.Equal(t, "user@example.com\n", got)
}

func TestTerminalRendererKeepsHTMLBlockText(t *testing.T) {
	got, err := renderMarkdownDocument(
		"<div>hello <strong>world</strong></div>\n", Options{Width: 80},
	)
	require.NoError(t, err)
	assert.Equal(t, "hello world\n", got)
}

func TestTerminalRendererPreservesLiteralLessThanInHTMLBlock(t *testing.T) {
	got, err := renderMarkdownDocument(
		"<div>1 < 2</div>\n", Options{Width: 80},
	)
	require.NoError(t, err)
	assert.Equal(t, "1 < 2\n", got)
}

func TestTerminalRendererPreservesInlineHTMLBreak(t *testing.T) {
	got, err := renderMarkdownDocument(
		"before<br>after\n", Options{Width: 80},
	)
	require.NoError(t, err)
	assert.Equal(t, "before\nafter\n", got)
}

func TestTerminalRendererPreservesCodeSpanWhitespaceAndEntities(t *testing.T) {
	got, err := renderMarkdownDocument(
		"`a  &amp; b`\n", Options{Width: 80},
	)
	require.NoError(t, err)
	assert.Equal(t, "`a  &amp; b`\n", got)
}

func TestTerminalRendererWrapsBlockquoteWithinPrefixWidth(t *testing.T) {
	lines, err := RenderLines("> one two three four\n", Options{Width: 10})
	require.NoError(t, err)
	assert.Equal(t, []string{"| one two", "| three", "| four"}, lines)
}

func TestTerminalRendererKeepsQuotePrefixOnWrappedCode(t *testing.T) {
	lines, err := RenderLines(
		"> ```\n> abcdefghijkl\n> ```\n", Options{Width: 8},
	)
	require.NoError(t, err)
	assert.Equal(t, []string{"| abcdef", "| ghijkl"}, lines)
}

func TestTerminalRendererKeepsPrefixesOnHardWrappedContent(t *testing.T) {
	tests := []struct {
		name     string
		markdown string
		width    int
		want     []string
	}{
		{"blockquote", "> abcdefghijkl\n", 8, []string{"| abcdef", "| ghijkl"}},
		{"list", "- abcdefghijkl\n", 8, []string{"- abcdef", "  ghijkl"}},
		{"task", "- [ ] abcdefghijkl\n", 10, []string{"[ ] abcdef", "    ghijkl"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			lines, err := RenderLines(test.markdown, Options{Width: test.width})
			require.NoError(t, err)
			assert.Equal(t, test.want, lines)
		})
	}
}

func TestTerminalRendererKeepsBlockquotePrefixesInsideList(t *testing.T) {
	lines, err := RenderLines("- > abcdefghijkl\n", Options{Width: 10})
	require.NoError(t, err)
	assert.Equal(t, []string{"- | abcdef", "  | ghijkl"}, lines)
}

func TestTerminalRendererKeepsListIndentOnWrappedCodeBlock(t *testing.T) {
	lines, err := RenderLines(
		"- ```\n  abcdefghijkl\n  ```\n", Options{Width: 10},
	)
	require.NoError(t, err)
	assert.Equal(t, []string{"- abcdefgh", "  ijkl"}, lines)
}

func TestTerminalRendererPreservesLeadingSpacesInListCodeBlock(t *testing.T) {
	lines, err := RenderLines(
		"- ```\n  first\n    indented\n  ```\n", Options{Width: 20},
	)
	require.NoError(t, err)
	assert.Equal(t, []string{"- first", "    indented"}, lines)
}

func TestTerminalRendererKeepsHTMLBlockTextInsideList(t *testing.T) {
	lines, err := RenderLines(
		"- <div>\n  hello world\n  </div>\n", Options{Width: 10},
	)
	require.NoError(t, err)
	assert.Equal(t, []string{"- hello wo", "  rld"}, lines)
}

func TestTerminalRendererUsesCheckboxAsTaskMarker(t *testing.T) {
	got, err := renderMarkdownDocument(
		"- [x] done\n- [ ] pending\n", Options{Width: 80},
	)
	require.NoError(t, err)
	assert.Equal(t, "[x] done\n[ ] pending\n", got)
}

func TestTerminalRendererAlignsTaskContinuationAfterCheckbox(t *testing.T) {
	lines, err := RenderLines(
		"- [ ] one two three four\n", Options{Width: 12},
	)
	require.NoError(t, err)
	assert.Equal(t, []string{"[ ] one two", "    three", "    four"}, lines)
}

func TestTerminalRendererAlignsNestedListToParentContinuation(t *testing.T) {
	lines, err := RenderLines(
		"10. parent\n    - child one two three\n", Options{Width: 14},
	)
	require.NoError(t, err)
	assert.Equal(t, []string{
		"10. parent",
		"    - child",
		"      one two",
		"      three",
	}, lines)
}

func TestTerminalRendererPreservesListParagraphBoundaries(t *testing.T) {
	lines, err := RenderLines(
		"- first paragraph\n\n  second paragraph\n", Options{Width: 80},
	)
	require.NoError(t, err)
	assert.Equal(t, []string{
		"- first paragraph",
		"",
		"  second paragraph",
	}, lines)
}

func TestTerminalRendererPreservesOrderedTaskMarker(t *testing.T) {
	got, err := renderMarkdownDocument(
		"2. [x] done\n3. [ ] pending\n", Options{Width: 80},
	)
	require.NoError(t, err)
	assert.Equal(t, "2. [x] done\n3. [ ] pending\n", got)
}

func TestANSIWrappedLinesPreservesVisibleContent(t *testing.T) {
	rendered := "\x1b[31mカタabcdef\x1b[0m"
	lines := ANSIWrappedLines(rendered, 4)
	require.NotEmpty(t, lines)
	for _, line := range lines {
		assert.LessOrEqual(t, ansi.StringWidth(line), 4)
	}
	assert.Equal(t, "カタabcdef", termtext.StripANSI(strings.Join(lines, "")))
}

func TestANSIWrappedLinesAllowsOnlySGRControls(t *testing.T) {
	rendered := "\x1b[31mred\x1b[0m" +
		"\x1b]52;c;clipboard\x07" +
		"\x1b[2Jcleared" +
		"\x1bP1;2|dcs\x1b\\" +
		"\x1b_apc\x1b\\" +
		"\x07" +
		"\u009b2J" +
		"\u202e"

	got := strings.Join(ANSIWrappedLines(rendered, 80), "\n")

	assert.Equal(t, "\x1b[31mred\x1b[0mcleared2J", got)
}

func TestANSIWrappedLinesTerminatesUnclosedStyle(t *testing.T) {
	assert.Equal(t, []string{"\x1b[31mred\x1b[0m"}, ANSIWrappedLines("\x1b[31mred", 80))
}

func TestANSIWrappedLinesNormalizesOnlyOuterLineEndings(t *testing.T) {
	want := []string{"first", "", "second"}
	assert.Equal(t, want, ANSIWrappedLines("\nfirst\r\n\r\nsecond\r\n", 80))
	assert.Equal(t, want, ANSIWrappedLines("first\n\nsecond", 80))
}

func TestANSIWrappedLinesRemovesANSIOnlyOuterRowsWithoutLosingState(t *testing.T) {
	want := []string{"\x1b[31mfirst", "", "second\x1b[0m"}
	assert.Equal(t, want, ANSIWrappedLines("\x1b[31m\nfirst\n\nsecond\n\x1b[0m", 80))
}

func TestANSIWrappedLinesTrimsLastVisibleRowBeforeTrailingANSIState(t *testing.T) {
	assert.Equal(t, []string{"text\x1b[0m"}, ANSIWrappedLines("text   \n\x1b[0m", 80))
}

func TestANSIWrappedLinesRejectsUnsafeSGR(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "conceal",
			input: "before\x1b[8mhidden\x1b[0mafter",
			want:  "beforehidden\x1b[0mafter",
		},
		{
			name:  "blink",
			input: "before\x1b[5mblink\x1b[0mafter",
			want:  "beforeblink\x1b[0mafter",
		},
		{
			name:  "reverse",
			input: "before\x1b[7mreverse\x1b[0mafter",
			want:  "beforereverse\x1b[0mafter",
		},
		{
			name:  "mixed parameters",
			input: "before\x1b[1;7mmixed\x1b[0mafter",
			want:  "beforemixed\x1b[0mafter",
		},
		{
			name:  "colon syntax",
			input: "before\x1b[38:5:123mcolon\x1b[0mafter",
			want:  "beforecolon\x1b[0mafter",
		},
		{
			name:  "valid indexed color",
			input: "before\x1b[38;5;123mindexed\x1b[0mafter",
			want:  "before\x1b[38;5;123mindexed\x1b[0mafter",
		},
		{
			name:  "invalid indexed color",
			input: "before\x1b[38;5;256mindexed\x1b[0mafter",
			want:  "beforeindexed\x1b[0mafter",
		},
		{
			name:  "valid RGB color",
			input: "before\x1b[38;2;255;0;1mrgb",
			want:  "before\x1b[38;2;255;0;1mrgb\x1b[0m",
		},
		{
			name:  "invalid RGB color",
			input: "before\x1b[38;2;255;0;256mrgb\x1b[0mafter",
			want:  "beforergb\x1b[0mafter",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assert := assert.New(t)
			got := strings.Join(ANSIWrappedLines(test.input, 80), "\n")
			assert.Equal(test.want, got)
		})
	}
}

func TestRenderOptions(t *testing.T) {
	background := "236"
	emptyBackground := ""

	assert.Equal(t, "236", CodeBlockBackground(true))
	assert.Equal(t, "252", CodeBlockBackground(false))

	tests := []struct {
		name       string
		markdown   string
		opts       Options
		maxWidth   int
		wantText   string
		contains   string
		notContain string
	}{
		{
			name:     "zero width uses the width floor",
			markdown: "abcd",
			opts:     Options{Width: 0},
			maxWidth: 1,
			wantText: "abcd",
		},
		{
			name:     "negative width uses the width floor",
			markdown: "abcd",
			opts:     Options{Width: -2},
			maxWidth: 1,
			wantText: "abcd",
		},
		{
			name:       "nil background",
			markdown:   "```\ncode\n```",
			opts:       Options{Width: 80},
			wantText:   "code",
			notContain: "\x1b[48;5;",
		},
		{
			name:       "empty background",
			markdown:   "```\ncode\n```",
			opts:       Options{Width: 80, CodeBlockBackground: &emptyBackground},
			wantText:   "code",
			notContain: "\x1b[48;5;",
		},
		{
			name:     "set background",
			markdown: "```\ncode\n```",
			opts:     Options{Width: 80, CodeBlockBackground: &background},
			wantText: "code",
			contains: "\x1b[48;5;236mcode\x1b[49m",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assert := assert.New(t)
			lines, err := RenderLines(test.markdown, test.opts)
			require.NoError(t, err)
			got := strings.Join(lines, "\n")
			assert.Equal(test.wantText, termtext.StripANSI(strings.Join(lines, "")))
			if test.maxWidth > 0 {
				for _, line := range lines {
					assert.LessOrEqual(ansi.StringWidth(line), test.maxWidth)
				}
			}
			if test.contains != "" {
				assert.Contains(got, test.contains)
			}
			if test.notContain != "" {
				assert.NotContains(got, test.notContain)
			}
		})
	}

	lines, err := RenderLines(" \n\t", Options{Width: 80})
	require.NoError(t, err)
	assert.Nil(t, lines)
}
