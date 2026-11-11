package notemarkdown

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRecipeSemanticGolden(t *testing.T) {
	// A recipe change requires an explicit new semantic fixture. All public
	// fields are pinned, including raw inline values and original byte offsets.
	goldens := map[string]Note{
		"docbank-note-markdown/v2": {
			Title: "**A** [B](x)", TitleSource: "h1", Lines: 3,
			Headings: []Heading{
				{Ordinal: 0, Level: 1, Text: "**A** [B](x)", Slug: "a-bx", Path: "**A** [B](x)", Start: 0, LineEnd: 15, SectionEnd: 20, Line: 1, SectionEndLine: 3},
				{Ordinal: 1, Level: 2, Text: "C", Slug: "c", Path: "**A** [B](x) > C", Start: 15, LineEnd: 20, SectionEnd: 20, Line: 2, SectionEndLine: 3},
			},
			Links: []Link{{Ordinal: 0, Form: "markdown", Raw: "[B](x)", Target: "x", Label: "B", Start: 8, End: 14, Line: 1}},
		},
	}
	want, ok := goldens[Recipe]
	require.True(t, ok, "add a semantic fixture for the new recipe")
	n, err := Parse(t.Context(), []byte("# **A** [B](x)\n## C\n"), Limits{})
	require.NoError(t, err)
	require.Equal(t, want, n)
}

func TestTargetComponentsDecodeOnce(t *testing.T) {
	for _, tc := range []struct{ authored, target, fragment string }{
		{"A%23B.md#H%23x", "A#B.md", "H#x"},
		{"%2523.md#H%2523", "%23.md", "H%23"},
		{"A:notes.md", "A:notes.md", ""},
		{"https://example.invalid/A%23B#H%23x", "https://example.invalid/A#B", "H#x"},
		{"a%zz.md#H%23x", "a%zz.md", "H#x"},
		{"a%23b.md#%00", "a#b.md", "%00"},
		{"a%23b.md#%FF", "a#b.md", "%FF"},
	} {
		for _, form := range []struct{ name, source string }{
			{"inline", "[x](" + tc.authored + ")"},
			{"reference", "[x][r]\n\n[r]: " + tc.authored},
			{"collapsed", "[r][]\n\n[r]: " + tc.authored},
			{"shortcut", "[r]\n\n[r]: " + tc.authored},
			{"image", "![x](" + tc.authored + ")"},
			{"wiki", "[[" + tc.authored + "]]"},
			{"embed", "![[" + tc.authored + "]]"},
		} {
			t.Run(form.name+"/"+tc.authored, func(t *testing.T) {
				n, err := Parse(t.Context(), []byte(form.source), Limits{})
				require.NoError(t, err)
				require.Len(t, n.Links, 1)
				require.Equal(t, tc.target, n.Links[0].Target)
				require.Equal(t, tc.fragment, n.Links[0].Fragment)
				require.Equal(t, n.Links[0].Raw, form.source[n.Links[0].Start:n.Links[0].End])
			})
		}
	}
	for _, authored := range []string{"https://example.invalid/A%23B#H%23x", "https://example.invalid/%2523#%2523"} {
		n, err := Parse(t.Context(), []byte("<"+authored+">"), Limits{})
		require.NoError(t, err)
		require.Len(t, n.Links, 1)
		if strings.Contains(authored, "A%23B") {
			require.Equal(t, "https://example.invalid/A#B", n.Links[0].Target)
			require.Equal(t, "H#x", n.Links[0].Fragment)
		} else {
			require.Equal(t, "https://example.invalid/%23", n.Links[0].Target)
			require.Equal(t, "%23", n.Links[0].Fragment)
		}
	}
}

func TestMarkdownSyntaxDecodesBeforeFragmentSplit(t *testing.T) {
	for _, destination := range []string{`A\#H%23x`, `A&#35;H%23x`} {
		n, err := Parse(t.Context(), []byte("[x]("+destination+")"), Limits{})
		require.NoError(t, err)
		require.Len(t, n.Links, 1)
		require.Equal(t, "A", n.Links[0].Target)
		require.Equal(t, "H#x", n.Links[0].Fragment)
	}
}

func TestRawLabelsAndFirstReferenceDefinition(t *testing.T) {
	src := "[**A** `B` &amp;][r] [[A%23B.md|**raw**  alias]] [[A%23B.md]]\n\n[r]: first%23name.md\n[r]: second.md\n"
	n, err := Parse(t.Context(), []byte(src), Limits{})
	require.NoError(t, err)
	require.Equal(t, []string{"first#name.md", "A#B.md", "A#B.md"}, linkTargets(n.Links))
	require.Equal(t, "**A** `B` &amp;", n.Links[0].Label)
	require.Equal(t, "**raw** alias", n.Links[1].Label)
	require.Equal(t, "A%23B.md", n.Links[2].Label)
}

func TestCommonMarkEmailLengths(t *testing.T) {
	for _, address := range []string{strings.Repeat("a", 65) + "@example.invalid", "a@" + strings.Repeat("abcdefghij.", 24) + "example.invalid"} {
		n, err := Parse(t.Context(), []byte("<"+address+">"), Limits{})
		require.NoError(t, err)
		require.Equal(t, []string{"mailto:" + address}, linkTargets(n.Links))
	}
}

func TestSimpleInputsWithinByteEnvelope(t *testing.T) {
	for _, src := range []string{
		strings.Repeat("\n", maxInput),
		"---\n" + strings.Repeat("# comment\n", 7000) + "---\n# Body\n",
		"---\n#" + strings.Repeat("x", maxInput-len("---\n#\n---\n# Body\n")) + "\n---\n# Body\n",
		"---\n" + strings.Repeat("# comment\n", 7000) + "# Body\n",
	} {
		n, err := Parse(t.Context(), []byte(src), Limits{})
		require.NoError(t, err)
		if strings.HasPrefix(src, "---") {
			require.Equal(t, "Body", n.Title)
		} else {
			require.Equal(t, maxInput+1, n.Lines)
		}
	}
}

func TestMappingKeysDoNotUseValueByteLimit(t *testing.T) {
	n, err := Parse(t.Context(), []byte("---\ntitle: x\nnested: {longkey: y}\n---\n# Body\n"), Limits{MaxValueBytes: 1})
	require.NoError(t, err)
	require.Equal(t, "x", n.Title)
	require.Equal(t, map[string]any{"longkey": "y"}, n.Frontmatter.Raw["nested"])
	n, err = Parse(t.Context(), []byte("---\ntitle: xx\n---\n# Body\n"), Limits{MaxValueBytes: 1})
	requireLimit(t, err, "frontmatter_value_bytes")
	require.Equal(t, Note{}, n)
}

func TestSingleHeadingWithinDefaultByteEnvelope(t *testing.T) {
	// Clone + compact view + raw text + conservative 3x NFC admission + Path
	// require about 7x input, even though there is only one ordinary heading.
	text := strings.Repeat("a", maxInput-len("# "))
	src := []byte("# " + text)
	n, err := Parse(t.Context(), src, Limits{})
	require.NoError(t, err)
	require.Equal(t, maxInput, len(src))
	require.Equal(t, text, n.Title)
	require.Equal(t, "h1", n.TitleSource)
	require.Len(t, n.Headings, 1)
	h := n.Headings[0]
	require.Equal(t, text, h.Text)
	require.Equal(t, text, h.Slug)
	require.Equal(t, text, h.Path)
	require.Equal(t, Span{0, maxInput}, Span{h.Start, h.LineEnd})
	require.Equal(t, maxInput, h.SectionEnd)
	require.Equal(t, 1, n.Lines)
	require.Empty(t, n.Links)
	require.Empty(t, n.Diagnostics)
}

func TestMalformedClosedFrontmatterDoesNotBecomeMarkdown(t *testing.T) {
	src := "---\n# Hidden\nbad: [unclosed\n[[Hidden]]\n---\n# Body\n[[Visible]]\n"
	n, err := Parse(t.Context(), []byte(src), Limits{MaxDiagnostics: 1})
	require.NoError(t, err)
	end := strings.Index(src, "# Body")
	require.Equal(t, end, n.BodyStart)
	require.Nil(t, n.Frontmatter.Raw)
	require.Equal(t, []Diagnostic{{Code: "invalid_frontmatter", Span: Span{0, end}}}, n.Diagnostics)
	require.Equal(t, []string{"Body"}, headingTexts(n.Headings))
	require.Equal(t, []string{"Visible"}, linkTargets(n.Links))
}
