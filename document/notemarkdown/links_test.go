package notemarkdown

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"hegel.dev/go/hegel"
)

func TestLinksCommonMarkForms(t *testing.T) {
	src := []byte("[inline](next.md \"Title\")\n[angle](<space note.md>)\n[balanced](a(b).md)\n[full][ref] [collapsed][] [shortcut]\n![image](pic.png)\n[multi\nlabel](other.md)\n[multidest](\n n.md\n)\n\n[ref]: ../R.md\n[collapsed]: C.md\n[shortcut]: S.md\n[unused]: U.md\n")
	n, err := Parse(t.Context(), src, Limits{})
	require.NoError(t, err)
	require.Equal(t, []string{"next.md", "space note.md", "a(b).md", "../R.md", "C.md", "S.md", "pic.png", "other.md", "n.md"}, linkTargets(n.Links))
	for _, l := range n.Links {
		require.NotEmpty(t, string(src[linkSpan(l).Start:linkSpan(l).End]))
	}
	require.Equal(t, "reference", n.Links[3].Form)
	require.Equal(t, "image", n.Links[6].Form)
	src = []byte("[one][r] [two][r]\n\n[r]: next.md\n")
	n, err = Parse(t.Context(), src, Limits{})
	require.NoError(t, err)
	require.Len(t, n.Links, 2)
	require.Equal(t, "[one][r]", string(src[linkSpan(n.Links[0]).Start:linkSpan(n.Links[0]).End]))
}

func TestParenthesizedLinkTitles(t *testing.T) {
	for _, tc := range []struct {
		name, inline, reference string
		valid                   bool
	}{
		{"unescaped opener", `[x](foo (a(b)))`, `[r]: foo (a(b))`, false},
		{"unclosed nested", `[x](foo (a(b))`, `[r]: foo (a(b)`, false},
		{"empty", `[x](foo ())`, `[r]: foo ()`, true},
		{"escaped opener", `[x](foo (a\(b))`, `[r]: foo (a\(b)`, true},
		{"escaped closer", `[x](foo (a\)b))`, `[r]: foo (a\)b)`, true},
		{"escaped backslash", `[x](foo (a\\b))`, `[r]: foo (a\\b)`, true},
		{"multiline", "[x](foo (a\nb))", "[r]: foo (a\nb)", true},
		{"quoted parentheses", `[x](foo "a(b")`, `[r]: foo "a(b"`, true},
		{"missing close", `[x](foo (a)`, `[r]: foo (a`, false},
	} {
		for _, form := range []struct{ name, source, raw string }{
			{"inline", tc.inline, tc.inline},
			{"reference", "[x][r]\n\n" + tc.reference + "\n", "[x][r]"},
		} {
			t.Run(form.name+"/"+tc.name, func(t *testing.T) {
				n, err := Parse(t.Context(), []byte(form.source), Limits{})
				require.NoError(t, err)
				if !tc.valid {
					require.Empty(t, n.Links)
					return
				}
				require.Len(t, n.Links, 1)
				require.Equal(t, "foo", n.Links[0].Target)
				require.Equal(t, form.raw, n.Links[0].Raw)
				require.Equal(t, form.raw, form.source[n.Links[0].Start:n.Links[0].End])
			})
		}
	}
}

func TestLinksWikiForms(t *testing.T) {
	src := []byte("[[Note]] [[Note#Heading|label]] [[#Heading]] ![[image.png]] [[pipe\\|name|label]]")
	n, err := Parse(t.Context(), src, Limits{})
	require.NoError(t, err)
	require.Equal(t, []string{"Note", "Note", "", "image.png", "pipe|name"}, linkTargets(n.Links))
	for _, i := range []int{0, 1, 2, 4} {
		require.Equal(t, "wikilink", n.Links[i].Form)
	}
	require.Equal(t, "embed", n.Links[3].Form)
	require.Equal(t, "![[image.png]]", string(src[linkSpan(n.Links[3]).Start:linkSpan(n.Links[3]).End]))
}

func TestLinksExcludedRegions(t *testing.T) {
	src := []byte("---\ntitle: '[bad](yaml.md)'\n---\n`[bad](code.md)`\n\n```\n[bad](fence.md)\n```\n\n    [bad](indent.md)\n\n<div>\n[bad](html.md)\n</div>\n\n\\[bad](escape.md) \\[[bad]]\n\n- [list](list.md)\n\n> [quote](quote.md)\n\n[unused]: unused.md \"[bad](title.md)\"\n")
	n, err := Parse(t.Context(), src, Limits{})
	require.NoError(t, err)
	require.Equal(t, []string{"list.md", "quote.md"}, linkTargets(n.Links))
}

func TestLinksSyntaxDecodeOnly(t *testing.T) {
	src := []byte(`[a](n\(x\).md?x=&amp;) [b](a%252Fb.md) [[&amp;%252F]]`)
	n, err := Parse(t.Context(), src, Limits{})
	require.NoError(t, err)
	require.Equal(t, []string{"n(x).md?x=&", "a%2Fb.md", "&amp;%2F"}, linkTargets(n.Links))
}

func TestBareDestinationsRejectASCIIControls(t *testing.T) {
	for _, c := range []byte{1, 0x0b, 0x0c, 0x1f, 0x7f} {
		control := string([]byte{c})
		for _, src := range []string{
			"[a](x" + control + "y)",
			"[a][r]\n\n[r]: x" + control + "y\n",
		} {
			n, err := Parse(t.Context(), []byte(src), Limits{})
			require.NoError(t, err)
			require.Empty(t, n.Links)
		}
	}
	// The existing angle-destination contract remains unchanged.
	control := string([]byte{1})
	n, err := Parse(t.Context(), []byte("[a](<x"+control+"y>)"), Limits{})
	require.NoError(t, err)
	require.Equal(t, []string{"x" + control + "y"}, linkTargets(n.Links))
}

func TestBareDestinationSeparators(t *testing.T) {
	for _, separator := range []struct{ name, text string }{
		{"space", " "}, {"tab", "\t"}, {"LF", "\n"}, {"CRLF", "\r\n"}, {"CR", "\r"},
	} {
		for _, form := range []struct{ name, source, raw, kind string }{
			{"inline title", "[x](next.md#part" + separator.text + `"Title")`, "", "markdown"},
			{"inline end", "[x](next.md#part" + separator.text + ")", "", "markdown"},
			{"image title", "![x](next.md#part" + separator.text + `"Title")`, "", "image"},
			{"reference title", "[x][r]\n\n[r]: next.md#part" + separator.text + "\"Title\"\n", "[x][r]", "reference"},
		} {
			t.Run(separator.name+"/"+form.name, func(t *testing.T) {
				n, err := Parse(t.Context(), []byte(form.source), Limits{})
				require.NoError(t, err)
				require.Len(t, n.Links, 1)
				link := n.Links[0]
				require.Equal(t, "next.md", link.Target)
				require.Equal(t, "part", link.Fragment)
				require.Equal(t, "x", link.Label)
				require.Equal(t, form.kind, link.Form)
				raw := form.raw
				if raw == "" {
					raw = form.source
				}
				require.Equal(t, raw, link.Raw)
				require.Equal(t, raw, form.source[link.Start:link.End])
			})
		}
	}
}

func TestLinksRepeatedReferenceBudget(t *testing.T) {
	src := []byte(strings.Repeat("[x][r] ", 5000) + "\n\n[r]: " + strings.Repeat("a", 4096) + "\n")
	n, err := Parse(t.Context(), src, Limits{})
	requireLimit(t, err, "derived_bytes")
	require.Equal(t, Note{}, n)
}

func TestLinksCountAndTargetLimits(t *testing.T) {
	n, err := Parse(t.Context(), []byte(strings.Repeat("[x](a.md) ", 10000)), Limits{})
	require.NoError(t, err)
	require.Len(t, n.Links, 10000)
	n, err = Parse(t.Context(), []byte(strings.Repeat("[x](a.md) ", 10001)), Limits{})
	requireLimit(t, err, "links")
	require.Equal(t, Note{}, n)
	n, err = Parse(t.Context(), []byte("[x]("+strings.Repeat("a", 4096)+")"), Limits{})
	require.NoError(t, err)
	require.Len(t, n.Links, 1)
	n, err = Parse(t.Context(), []byte("[x]("+strings.Repeat("a", 4097)+")"), Limits{})
	requireLimit(t, err, "target_bytes")
	require.Equal(t, Note{}, n)
}

func TestLinksNestedAndCodeRuns(t *testing.T) {
	n, err := Parse(t.Context(), []byte("`` ` [bad](x.md) `` [outer [inner](inner.md)](outer.md) [![alt](image.png)](note.md)"), Limits{})
	require.NoError(t, err)
	require.Equal(t, []string{"inner.md", "note.md", "image.png"}, linkTargets(n.Links))
}

func TestPropertyLinkOccurrences(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		count := hegel.Draw(ht, hegel.Integers[int](1, 128))
		src := []byte(strings.Repeat("[x](note.md) ", count))
		n, err := Parse(ht.Context(), src, Limits{})
		require.NoError(ht, err)
		require.Len(ht, n.Links, count)
		for i, l := range n.Links {
			require.Equal(ht, Span{Start: i * 13, End: i*13 + 12}, linkSpan(l))
			require.Equal(ht, "note.md", l.Target)
		}
	}, hegel.WithTestCases(1000))
}

func linkTargets(links []Link) []string {
	result := make([]string, 0, len(links))
	for _, l := range links {
		result = append(result, l.Target)
	}
	return result
}

func TestLinksDefinitionContinuationAndAutolinks(t *testing.T) {
	src := []byte("[first][r] [second][s]\n\n[r]:\n  <next.md>\n  \"[unused](title.md)\"\n\n[s]: other.md\n  '[unused](title2.md)'\n\n<https://example.invalid/x> <reader@example.invalid>\n")
	n, err := Parse(t.Context(), src, Limits{})
	require.NoError(t, err)
	require.Equal(t, []string{"next.md", "other.md", "https://example.invalid/x", "mailto:reader@example.invalid"}, linkTargets(n.Links))
}

func TestLinksDecodedTargetLimit(t *testing.T) {
	n, err := Parse(t.Context(), []byte("[x]("+strings.Repeat("&acE;", 800)+strings.Repeat("a", 96)+") [y](&amp;amp;)"), Limits{})
	require.NoError(t, err)
	require.Len(t, n.Links[0].Target, 4096)
	require.Equal(t, "&amp;", n.Links[1].Target)
}

func TestLinksHostileWorkAndCancellation(t *testing.T) {
	// Repeated unmatched HTML openers must charge speculative rescans.
	n, err := Parse(t.Context(), []byte("text "+strings.Repeat("<x ", 24000)), Limits{})
	requireLimit(t, err, "work_steps")
	require.Equal(t, Note{}, n)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = extractLinks(ctx, []byte("[x](next.md)"), []sourceBlock{{content: Span{0, 12}}}, nil, &budget{})
	require.ErrorIs(t, err, context.Canceled)
}

func TestLinksRawHTMLSyntax(t *testing.T) {
	src := []byte("before <span title=\"> [bad](attribute.md)\">[good](good.md)</span>\n\n<script>\n[bad](script.md)\n</SCRIPT>\n[after](after.md)\n\n<span>[inline](inline.md)</span>\n")
	n, err := Parse(t.Context(), src, Limits{})
	require.NoError(t, err)
	require.Equal(t, []string{"good.md", "after.md", "inline.md"}, linkTargets(n.Links))
}

func TestLinksAutolinkSyntax(t *testing.T) {
	src := []byte("text <https://example.invalid/&amp;> <not:space here> <1bad:x> <a@example.invalid> <not@@example.invalid>")
	n, err := Parse(t.Context(), src, Limits{})
	require.NoError(t, err)
	require.Equal(t, []string{"https://example.invalid/&amp;", "mailto:a@example.invalid"}, linkTargets(n.Links))
}

func TestLinksP02LabelsFragmentsAndLiteralTargets(t *testing.T) {
	src := []byte("[**label**](A:notes.md#part) [[A:notes.md#Heading|shown]] ![[a%2Fb.md]] ![alt](img.png) <https://example.invalid/%C3%A9#x>\n")
	n, err := Parse(t.Context(), src, Limits{})
	require.NoError(t, err)
	require.Equal(t, []Link{
		{Ordinal: 0, Form: "markdown", Raw: "[**label**](A:notes.md#part)", Target: "A:notes.md", Fragment: "part", Label: "**label**", Start: 0, End: len("[**label**](A:notes.md#part)"), Line: 1},
	}, n.Links[:1])
	require.Equal(t, "A:notes.md", n.Links[1].Target)
	require.Equal(t, "Heading", n.Links[1].Fragment)
	require.Equal(t, "shown", n.Links[1].Label)
	require.Equal(t, "a/b.md", n.Links[2].Target)
	require.Equal(t, "alt", n.Links[3].Label)
	require.Equal(t, "https://example.invalid/é", n.Links[4].Target)
	require.Equal(t, "x", n.Links[4].Fragment)
	for _, l := range n.Links {
		require.Equal(t, l.Raw, string(src[l.Start:l.End]))
	}
}
