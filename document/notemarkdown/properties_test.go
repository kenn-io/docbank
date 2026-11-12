//go:build !darwin || arm64

package notemarkdown

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/require"
	"hegel.dev/go/hegel"
)

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

func TestPropertyParseDeterministicImmutableAndBounded(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		src := hegel.Draw(ht, hegel.Binary(0, 1024))
		structured := hegel.Draw(ht, hegel.SampledFrom([]bool{false, true}))
		if structured {
			label := strings.Repeat(hegel.Draw(ht, hegel.SampledFrom([]string{"a", "é", "星"})), hegel.Draw(ht, hegel.Integers[int](1, 32)))
			eol := hegel.Draw(ht, hegel.SampledFrom([]string{"\n", "\r\n", "\r"}))
			src = []byte("# " + label + eol + eol + "[" + label + "](note.md)" + eol)
		}
		before := bytes.Clone(src)
		a, ea := Parse(ht.Context(), src, Limits{})
		b, eb := Parse(ht.Context(), src, Limits{})
		require.Equal(ht, fmt.Sprint(ea), fmt.Sprint(eb))
		require.Equal(ht, a, b)
		require.Equal(ht, before, src)
		if structured {
			require.NoError(ht, ea)
			require.Len(ht, a.Headings, 1)
			require.Len(ht, a.Links, 1)
		}
		if ea != nil {
			require.Equal(ht, Note{}, a)
			return
		}
		for _, h := range a.Headings {
			assertOriginalSpan(ht, src, Span{h.Start, h.LineEnd})
			assertOriginalSpan(ht, src, Span{h.Start, h.SectionEnd})
			require.True(ht, bytes.HasPrefix(src[h.Start:h.SectionEnd], src[h.Start:h.LineEnd]))
		}
		for _, l := range a.Links {
			assertOriginalSpan(ht, src, Span{l.Start, l.End})
			require.Equal(ht, l.Raw, string(src[l.Start:l.End]))
		}
		for _, d := range a.Diagnostics {
			assertOriginalSpan(ht, src, d.Span)
		}
	}, hegel.WithTestCases(1000))
}

func TestPropertyOriginalOffsets(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		eol := hegel.Draw(ht, hegel.SampledFrom([]string{"\n", "\r\n", "\r"}))
		bom := hegel.Draw(ht, hegel.SampledFrom([]string{"", "\ufeff"}))
		prefix := hegel.Draw(ht, hegel.SampledFrom([]string{"", "> ", "- ", "> - "}))
		title := hegel.Draw(ht, hegel.SampledFrom([]string{"Café", "Cafe\u0301", "星", "A-1", "!!!", "🦉"}))
		count := hegel.Draw(ht, hegel.Integers[int](1, 32))
		src := []byte(bom + "---" + eol + "title: '" + title + "'" + eol + "---" + eol)
		var headings, links []Span
		for range count {
			start := len(src)
			src = append(src, []byte(prefix+"# "+title+eol)...)
			headings = append(headings, Span{start, len(src)})
			src = append(src, []byte(eol+"é ")...)
			start = len(src)
			src = append(src, []byte("[same][r]")...)
			links = append(links, Span{start, len(src)})
			src = append(src, []byte(eol+eol)...)
		}
		src = append(src, []byte("[r]: n%23x.md"+eol)...)
		n, err := Parse(ht.Context(), src, Limits{})
		require.NoError(ht, err)
		require.Equal(ht, title, n.Title)
		require.Len(ht, n.Headings, count)
		require.Len(ht, n.Links, count)
		for i, h := range n.Headings {
			require.Equal(ht, headings[i], Span{h.Start, h.LineEnd})
			require.Equal(ht, title, h.Text)
			require.True(ht, bytes.HasPrefix(src[h.Start:h.SectionEnd], src[h.Start:h.LineEnd]))
		}
		for i, l := range n.Links {
			require.Equal(ht, links[i], Span{l.Start, l.End})
			require.Equal(ht, "n#x.md", l.Target)
			require.Empty(ht, l.Fragment)
		}
	}, hegel.WithTestCases(1000))
}

func TestPropertyExcludedRegions(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		count := hegel.Draw(ht, hegel.Integers[int](1, 40))
		token := hegel.Draw(ht, hegel.SampledFrom([]string{"[x](a.md)", "[[A]]", "![[pic.png]]", "[x][r]"}))
		body := strings.Repeat(token+" ", count)
		for _, wrapper := range [][2]string{{"`", "`"}, {"```md\n", "\n```\n"}, {"<div>\n", "\n</div>\n"}, {"---\ntitle: '", "'\n---\n"}} {
			n, err := Parse(ht.Context(), []byte(wrapper[0]+body+wrapper[1]+"\n\n[r]: a.md\n"), Limits{})
			require.NoError(ht, err)
			require.Empty(ht, n.Links)
		}
	}, hegel.WithTestCases(1000))
}

func assertOriginalSpan(tb testing.TB, src []byte, s Span) {
	tb.Helper()
	require.GreaterOrEqual(tb, s.Start, 0)
	require.LessOrEqual(tb, s.Start, s.End)
	require.LessOrEqual(tb, s.End, len(src))
	require.True(tb, s.Start == len(src) || utf8.RuneStart(src[s.Start]))
	require.True(tb, s.End == len(src) || utf8.RuneStart(src[s.End]))
}
