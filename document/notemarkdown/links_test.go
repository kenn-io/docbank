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
	n, err := Parse(t.Context(), src)
	require.NoError(t, err)
	require.Equal(t, []string{"next.md", "space note.md", "a(b).md", "../R.md", "C.md", "S.md", "pic.png", "other.md", "n.md"}, linkTargets(n.Links))
	for _, l := range n.Links {
		require.Equal(t, l.RawTarget, string(src[l.DestinationSpan.Start:l.DestinationSpan.End]))
		require.NotEmpty(t, string(src[l.Span.Start:l.Span.End]))
	}
	require.Equal(t, "markdown_reference", n.Links[3].Form)
	require.True(t, n.Links[6].Embed)
	src = []byte("[one][r] [two][r]\n\n[r]: next.md\n")
	n, err = Parse(t.Context(), src)
	require.NoError(t, err)
	require.Len(t, n.Links, 2)
	require.Equal(t, "[one][r]", string(src[n.Links[0].Span.Start:n.Links[0].Span.End]))
	require.Equal(t, n.Links[0].DestinationSpan, n.Links[1].DestinationSpan)
}

func TestLinksWikiForms(t *testing.T) {
	src := []byte("[[Note]] [[Note#Heading|label]] [[#Heading]] ![[image.png]] [[pipe\\|name|label]]")
	n, err := Parse(t.Context(), src)
	require.NoError(t, err)
	require.Equal(t, []string{"Note", "Note#Heading", "#Heading", "image.png", "pipe|name"}, linkTargets(n.Links))
	for _, l := range n.Links {
		require.Equal(t, "wiki", l.Form)
		require.Equal(t, l.RawTarget, string(src[l.DestinationSpan.Start:l.DestinationSpan.End]))
	}
	require.True(t, n.Links[3].Embed)
	require.Equal(t, "![[image.png]]", string(src[n.Links[3].Span.Start:n.Links[3].Span.End]))
}

func TestLinksExcludedRegions(t *testing.T) {
	src := []byte("---\ntitle: '[bad](yaml.md)'\n---\n`[bad](code.md)`\n\n```\n[bad](fence.md)\n```\n\n    [bad](indent.md)\n\n<div>\n[bad](html.md)\n</div>\n\n\\[bad](escape.md) \\[[bad]]\n\n- [list](list.md)\n\n> [quote](quote.md)\n\n[unused]: unused.md \"[bad](title.md)\"\n")
	n, err := Parse(t.Context(), src)
	require.NoError(t, err)
	require.Equal(t, []string{"list.md", "quote.md"}, linkTargets(n.Links))
}

func TestLinksSyntaxDecodeOnly(t *testing.T) {
	src := []byte(`[a](n\(x\).md?x=&amp;) [b](a%252Fb.md) [[&amp;%252F]]`)
	n, err := Parse(t.Context(), src)
	require.NoError(t, err)
	require.Equal(t, []string{"n(x).md?x=&", "a%252Fb.md", "&amp;%252F"}, linkTargets(n.Links))
	require.Equal(t, `n\(x\).md?x=&amp;`, n.Links[0].RawTarget)
}

func TestLinksRepeatedReferenceBudget(t *testing.T) {
	src := []byte(strings.Repeat("[x][r] ", 8192) + "\n\n[r]: " + strings.Repeat("a", 4096) + "\n")
	n, err := Parse(t.Context(), src)
	requireLimit(t, err, "derived_bytes")
	require.Equal(t, Note{}, n)
}

func TestLinksCountAndTargetLimits(t *testing.T) {
	n, err := Parse(t.Context(), []byte(strings.Repeat("[x](a.md) ", 8192)))
	require.NoError(t, err)
	require.Len(t, n.Links, 8192)
	n, err = Parse(t.Context(), []byte(strings.Repeat("[x](a.md) ", 8193)))
	requireLimit(t, err, "links")
	require.Equal(t, Note{}, n)
	n, err = Parse(t.Context(), []byte("[x]("+strings.Repeat("a", 4096)+")"))
	require.NoError(t, err)
	require.Len(t, n.Links, 1)
	n, err = Parse(t.Context(), []byte("[x]("+strings.Repeat("a", 4097)+")"))
	requireLimit(t, err, "target_bytes")
	require.Equal(t, Note{}, n)
}

func TestLinksNestedAndCodeRuns(t *testing.T) {
	n, err := Parse(t.Context(), []byte("`` ` [bad](x.md) `` [outer [inner](inner.md)](outer.md) [![alt](image.png)](note.md)"))
	require.NoError(t, err)
	require.Equal(t, []string{"inner.md", "note.md", "image.png"}, linkTargets(n.Links))
}

func TestPropertyLinkOccurrences(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		count := hegel.Draw(ht, hegel.Integers[int](1, 128))
		src := []byte(strings.Repeat("[x](note.md) ", count))
		n, err := Parse(ht.Context(), src)
		require.NoError(ht, err)
		require.Len(ht, n.Links, count)
		for i, l := range n.Links {
			require.Equal(ht, Span{Start: i * 13, End: i*13 + 12}, l.Span)
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
	n, err := Parse(t.Context(), src)
	require.NoError(t, err)
	require.Equal(t, []string{"next.md", "other.md", "https://example.invalid/x", "mailto:reader@example.invalid"}, linkTargets(n.Links))
}

func TestLinksDecodedTargetLimit(t *testing.T) {
	n, err := Parse(t.Context(), []byte("[x]("+strings.Repeat("&acE;", 800)+strings.Repeat("a", 96)+") [y](&amp;amp;)"))
	require.NoError(t, err)
	require.Len(t, n.Links[0].Target, 4096)
	require.Equal(t, "&amp;", n.Links[1].Target)
}

func TestLinksHostileWorkAndCancellation(t *testing.T) {
	// Repeated unmatched HTML openers must charge speculative rescans.
	n, err := Parse(t.Context(), []byte("text "+strings.Repeat("<x ", 12000)))
	requireLimit(t, err, "work_steps")
	require.Equal(t, Note{}, n)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = extractLinks(ctx, []byte("[x](next.md)"), []sourceBlock{{content: Span{0, 12}}}, &budget{})
	require.ErrorIs(t, err, context.Canceled)
}

func TestLinksRawHTMLSyntax(t *testing.T) {
	src := []byte("before <span title=\"> [bad](attribute.md)\">[good](good.md)</span>\n\n<script>\n[bad](script.md)\n</SCRIPT>\n[after](after.md)\n\n<span>[inline](inline.md)</span>\n")
	n, err := Parse(t.Context(), src)
	require.NoError(t, err)
	require.Equal(t, []string{"good.md", "after.md", "inline.md"}, linkTargets(n.Links))
}

func TestLinksAutolinkSyntax(t *testing.T) {
	src := []byte("text <https://example.invalid/&amp;> <not:space here> <1bad:x> <a@example.invalid> <not@@example.invalid>")
	n, err := Parse(t.Context(), src)
	require.NoError(t, err)
	require.Equal(t, []string{"https://example.invalid/&amp;", "mailto:a@example.invalid"}, linkTargets(n.Links))
}
