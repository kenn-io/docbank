package notemarkdown

import (
	"bytes"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"hegel.dev/go/hegel"
)

// Splitting physical container lines loses valid logical links. Merging raw
// spans instead would leak the next line's container prefix into the syntax.
func TestContainerMultilineLinks(t *testing.T) {
	for _, tc := range []struct {
		name, source, use, raw, target string
	}{
		{"quote label", "> [multí\n> label](a&amp;amp;.md)\n", "[multí\n> label](a&amp;amp;.md)", "a&amp;amp;.md", "a&amp;.md"},
		{"list destination", "- [x](\n  <next.md>\n  )\n", "[x](\n  <next.md>\n  )", "next.md", "next.md"},
		{"nested CRLF", "> - [multi\r\n>   label](next.md)\r\n", "[multi\r\n>   label](next.md)", "next.md", "next.md"},
		{"quote reference", "> [r]:\n>   <next.md>\n>   \"[hidden](title.md)\"\n\n[x][r]\n", "[x][r]", "next.md", "next.md"},
		{"list reference", "- [r]:\n  next.md\n\n[x][r]\n", "[x][r]", "next.md", "next.md"},
		{"reference label", "> [first\n> second][r]\n\n[r]: next.md\n", "[first\n> second][r]", "next.md", "next.md"},
		{"collapsed label", "> [first\n> second][]\n\n[first second]: next.md\n", "[first\n> second][]", "next.md", "next.md"},
		{"lazy continuation", "> [first\nsecond](next.md)\n", "[first\nsecond](next.md)", "next.md", "next.md"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			src := []byte(tc.source)
			saved := bytes.Clone(src)
			n, err := Parse(t.Context(), src, Limits{})
			require.NoError(t, err)
			require.Equal(t, saved, src)
			require.Equal(t, []string{tc.target}, linkTargets(n.Links))
			link := n.Links[0]
			require.Equal(t, tc.use, string(src[linkSpan(link).Start:linkSpan(link).End]))
		})
	}
}

func TestContainerMultilineBoundaries(t *testing.T) {
	for _, source := range []string{
		"- [not\n- linked](bad.md)\n",
		"> [not\n\n> linked](bad.md)\n",
		"> [not\n> ```\n> linked](bad.md)\n> ```\n",
		"> [not\n> <div>\n> linked](bad.md)\n",
		"> `[hidden\n> label](bad.md)`\n",
	} {
		n, err := Parse(t.Context(), []byte(source), Limits{})
		require.NoError(t, err)
		require.Empty(t, n.Links, source)
	}
	// Deleting prefixes must not shift subsequent root links or headings.
	src := []byte("> [a\n> b](one.md)\n\n# Root [two](two.md)\n")
	n, err := Parse(t.Context(), src, Limits{})
	require.NoError(t, err)
	require.Equal(t, []string{"one.md", "two.md"}, linkTargets(n.Links))
	require.Equal(t, "[two](two.md)", string(src[linkSpan(n.Links[1]).Start:linkSpan(n.Links[1]).End]))
	require.Len(t, n.Headings, 1)
	require.Equal(t, "Root [two](two.md)", n.Headings[0].Text)
	require.Equal(t, "# Root [two](two.md)\n", string(src[headingSpan(n.Headings[0]).Start:headingSpan(n.Headings[0]).End]))
}

func TestPropertyContainerMultilineLinks(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		prefix := hegel.Draw(ht, hegel.SampledFrom([]string{"> ", "> > ", "- ", "> - "}))
		continued := strings.ReplaceAll(prefix, "-", " ")
		label := strings.Repeat("é", hegel.Draw(ht, hegel.Integers[int](1, 20)))
		src := []byte(prefix + "[" + label + "\n" + continued + "tail](n&amp;amp;.md)\n")
		n, err := Parse(ht.Context(), src, Limits{})
		require.NoError(ht, err)
		require.Equal(ht, []string{"n&amp;.md"}, linkTargets(n.Links))
		require.Equal(ht, "["+label+"\n"+continued+"tail](n&amp;amp;.md)", string(src[linkSpan(n.Links[0]).Start:linkSpan(n.Links[0]).End]))
	}, hegel.WithTestCases(1000))
}

func TestContainerReferenceLabelLimit(t *testing.T) {
	// 999 authored label bytes after container removal: prefixes are syntax,
	// and must not count toward the CommonMark reference-label bound.
	first, last := strings.Repeat("a", 498), strings.Repeat("b", 500)
	src := []byte("> [" + first + "\n> " + last + "][]\n\n[" + first + " " + last + "]: next.md\n")
	n, err := Parse(t.Context(), src, Limits{})
	require.NoError(t, err)
	require.Equal(t, []string{"next.md"}, linkTargets(n.Links))
}

// Container and HTML boundaries must hide only the appropriate occurrences.
func TestHTMLAndQuoteLinkBoundaries(t *testing.T) {
	for _, tc := range []struct{ name, source string }{
		{"unprefixed blank ends quote", "> ```\n> [hidden](bad.md)\n\n> [shown](good.md)\n"},
		{"prefixed blank retains fence", "> ```\n> [hidden](bad.md)\n>\n> [hidden](bad.md)\n> ```\n\n[shown](good.md)\n"},
		{"type6 trailing content", "<div class=\"x\">text\n[hidden](bad.md)\n\n[shown](good.md)\n"},
		{"type6 closing tag", "</div> text\n[hidden](bad.md)\n\n[shown](good.md)\n"},
		{"source is not type6", "paragraph\n<source>\n[shown](good.md)\n"},
		{"search is type6", "paragraph\n<search>text\n[hidden](bad.md)\n\n[shown](good.md)\n"},
		{"type4 ignores blank until terminator", "<!doctype html\n\n[hidden](bad.md)\n>\n[shown](good.md)\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			n, err := Parse(t.Context(), []byte(tc.source), Limits{})
			require.NoError(t, err)
			require.Equal(t, []string{"good.md"}, linkTargets(n.Links))
		})
	}
}
