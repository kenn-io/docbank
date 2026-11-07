package notemarkdown

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/require"
	"hegel.dev/go/hegel"
)

func TestPropertyParseDeterministicAndImmutable(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		src := hegel.Draw(ht, hegel.Binary(0, 1024))
		before := bytes.Clone(src)
		a, ea := Parse(ht.Context(), src)
		b, eb := Parse(ht.Context(), src)
		require.Equal(ht, fmt.Sprint(ea), fmt.Sprint(eb))
		require.Equal(ht, a, b)
		require.Equal(ht, before, src)
		if ea != nil {
			require.Equal(ht, Note{}, a)
			return
		}
		ba, err := CanonicalArtifact(ht.Context(), a)
		require.NoError(ht, err)
		bb, err := CanonicalArtifact(ht.Context(), b)
		require.NoError(ht, err)
		require.Equal(ht, ba, bb)
	}, hegel.WithTestCases(1000))
}

func TestPropertyParseSpans(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		eol := hegel.Draw(ht, hegel.SampledFrom([]string{"\n", "\r\n"}))
		prefix := hegel.Draw(ht, hegel.SampledFrom([]string{"", "\ufeff"}))
		title := hegel.Draw(ht, hegel.SampledFrom([]string{"Café", "Cafe\u0301", "星", "A-1", "!!!", "🦉"}))
		count := hegel.Draw(ht, hegel.Integers[int](1, 32))
		src := []byte(prefix + "---" + eol + "title: '" + title + "'" + eol + "---" + eol)
		var expectedUses, expectedHeadings []Span
		for range count {
			start := len(src)
			src = append(src, []byte("# "+title+eol)...)
			expectedHeadings = append(expectedHeadings, Span{start, len(src)})
			src = append(src, []byte("é ")...)
			start = len(src)
			src = append(src, []byte("[same][r]")...)
			expectedUses = append(expectedUses, Span{start, len(src)})
			src = append(src, []byte(eol+eol)...)
		}
		src = append(src, []byte("[r]: ")...)
		defStart := len(src)
		src = append(src, []byte("n%23x.md")...)
		def := Span{defStart, len(src)}
		src = append(src, []byte(eol)...)
		n, err := Parse(ht.Context(), src)
		require.NoError(ht, err)
		require.Equal(ht, title, n.Metadata.Title)
		require.Len(ht, n.Headings, count)
		require.Len(ht, n.Links, count)
		for i, h := range n.Headings {
			require.Equal(ht, expectedHeadings[i], h.Span)
			require.Equal(ht, title, h.Text)
			assertOriginalSpan(ht, src, h.Span)
			assertOriginalSpan(ht, src, h.Section)
		}
		for i, l := range n.Links {
			require.Equal(ht, expectedUses[i], l.Span)
			require.Equal(ht, def, l.DestinationSpan)
			require.Equal(ht, "n%23x.md", l.RawTarget)
			assertOriginalSpan(ht, src, l.Span)
			assertOriginalSpan(ht, src, l.DestinationSpan)
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

func TestPropertyExcludedRegions(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		count := hegel.Draw(ht, hegel.Integers[int](1, 40))
		token := hegel.Draw(ht, hegel.SampledFrom([]string{"[x](a.md)", "[[A]]", "![[pic.png]]", "[x][r]"}))
		body := strings.Repeat(token+" ", count)
		definitions := "\n\n[r]: a.md\n"
		for _, wrapper := range [][2]string{{"`", "`"}, {"```md\n", "\n```\n"}, {"<div>\n", "\n</div>\n"}, {"---\ntitle: '", "'\n---\n"}} {
			n, err := Parse(ht.Context(), []byte(wrapper[0]+body+wrapper[1]+definitions))
			require.NoError(ht, err)
			require.Empty(ht, n.Links)
		}
		for _, prefix := range []string{"", "- ", "> "} {
			n, err := Parse(ht.Context(), []byte(prefix+body+definitions))
			require.NoError(ht, err)
			require.Len(ht, n.Links, count)
		}
	}, hegel.WithTestCases(1000))
}

func TestPropertyAnchorsUnique(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		count := hegel.Draw(ht, hegel.Integers[int](1, 80))
		var src strings.Builder
		for range count {
			title := hegel.Draw(ht, hegel.SampledFrom([]string{"A", "A-1", "A!", "!!!", "星", "é", "e\u0301", "section"}))
			src.WriteString("# " + title + "\n")
		}
		n, err := Parse(ht.Context(), []byte(src.String()))
		require.NoError(ht, err)
		require.Len(ht, n.Headings, count)
		later, err := Parse(ht.Context(), []byte(src.String()+"# A\n# section\n"))
		require.NoError(ht, err)
		seen := map[string]bool{}
		for i, h := range n.Headings {
			require.False(ht, seen[h.Anchor])
			require.NotEmpty(ht, h.Anchor)
			seen[h.Anchor] = true
			require.Equal(ht, h.Anchor, later.Headings[i].Anchor)
		}
	}, hegel.WithTestCases(1000))
}

func TestPropertyTargetConfinement(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		depth := hegel.Draw(ht, hegel.Integers[int](0, 20))
		ups := hegel.Draw(ht, hegel.Integers[int](0, 24))
		source := strings.Repeat("dir/", depth) + "source.md"
		target := strings.Repeat("../", ups) + "é%252F.md"
		q, err := PlanTarget(source, Link{Form: "markdown", Target: target})
		require.NoError(ht, err)
		if ups > depth {
			require.Equal(ht, "invalid", q.Kind)
			require.Empty(ht, q.Keys)
		} else {
			require.Equal(ht, "note", q.Kind)
			require.Equal(ht, []LookupKey{{"path", strings.Repeat("dir/", depth-ups) + "é%2F.md"}}, q.Keys)
		}
		sep := hegel.Draw(ht, hegel.SampledFrom([]string{"%2f", "%2F", "%5c", "%5C", "%00"}))
		q, err = PlanTarget(source, Link{Form: "wiki", Target: "name" + sep + "child"})
		require.NoError(ht, err)
		require.Equal(ht, "invalid", q.Kind)
	}, hegel.WithTestCases(1000))
}

func TestPropertyResolverPermutation(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		complete := hegel.Draw(ht, hegel.SampledFrom([]bool{false, true}))
		count := hegel.Draw(ht, hegel.Integers[int](0, 4))
		q := TargetQuery{Kind: "note", Keys: []LookupKey{{"path", "area/X.md"}, {"basename", "X"}, {"alias", "X"}}}
		rows := []Candidate{}
		expectedBindings := map[string]bool{}
		exact := ""
		for i := range count {
			kind := hegel.Draw(ht, hegel.Integers[int](0, 2))
			binding := fmt.Sprintf("b%d", i%2)
			p := "other/X.md"
			if kind == 0 {
				p = "area/X.md"
				binding = "exact"
				exact = binding
			} else {
				expectedBindings[binding] = true
			}
			// Each binding must carry one consistent path regardless of the key.
			if binding == "b1" {
				p = "else/X.md"
			}
			rows = append(rows, Candidate{Binding: binding, Path: p, Kind: "note", Keys: []LookupKey{q.Keys[kind]}})
		}
		// Independent set oracle over the generated identities, not resolver helpers.
		state, binding := "missing", ""
		if !complete {
			state = "pending"
		} else if exact != "" {
			state = "resolved"
			binding = exact
		} else if len(expectedBindings) > 1 {
			state = "ambiguous"
		} else {
			for b := range expectedBindings {
				state = "resolved"
				binding = b
			}
		}
		set := CandidateSet{NamespaceGeneration: "rev", Complete: complete, Candidates: rows}
		got, err := Resolve(ht.Context(), q, set)
		require.NoError(ht, err)
		require.Equal(ht, Resolution{State: state, Binding: binding, NamespaceGeneration: "rev"}, got)
		for i, j := 0, len(rows)-1; i < j; i, j = i+1, j-1 {
			rows[i], rows[j] = rows[j], rows[i]
		}
		other, err := Resolve(ht.Context(), q, set)
		require.NoError(ht, err)
		require.Equal(ht, got, other)
	}, hegel.WithTestCases(1000))
}

func TestPropertyArtifactBoundary(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		count := hegel.Draw(ht, hegel.Integers[int](0, 128))
		token := hegel.Draw(ht, hegel.SampledFrom([]string{"[x](a.md)\n", "# é\n", "control\x01\x02\n", "[[&amp;]]\n"}))
		src := []byte(strings.Repeat(token, count))
		n, err := Parse(ht.Context(), src)
		require.NoError(ht, err)
		a, err := CanonicalArtifact(ht.Context(), n)
		require.NoError(ht, err)
		require.LessOrEqual(ht, len(a), 16<<20)
		require.True(ht, json.Valid(a))
		var decoded Note
		require.NoError(ht, json.Unmarshal(a, &decoded))
		b, err := CanonicalArtifact(ht.Context(), decoded)
		require.NoError(ht, err)
		require.Equal(ht, a, b)
	}, hegel.WithTestCases(1000))
}

func TestPropertyHeadingVisibleText(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		word := hegel.Draw(ht, hegel.SampledFrom([]string{"visible", "Café", "星", "in_word"}))
		marker := hegel.Draw(ht, hegel.SampledFrom([]string{"*", "**", "***", "_", "__"}))
		width := hegel.Draw(ht, hegel.Integers[int](1, 12))
		inner := strings.Repeat("`", width+1)
		markdown := marker + "[" + word + "](n(a).md)" + marker + " " + strings.Repeat("`", width) + "a" + inner + "b" + strings.Repeat("`", width)
		n, err := Parse(ht.Context(), []byte("# "+markdown+"\n"))
		require.NoError(ht, err)
		require.Equal(ht, word+" a"+inner+"b", n.Headings[0].Text)
	}, hegel.WithTestCases(1000))
}

func TestPropertyQuotedMetadataRoundTrip(t *testing.T) {
	// JSON string syntax is a supported subset of YAML double-quoted syntax.
	// The full Unicode character domain includes controls and line separators.
	hegel.Test(t, func(ht *hegel.T) {
		title := hegel.Draw(ht, hegel.Text().MaxSize(1024))
		quoted, err := json.Marshal(title)
		require.NoError(ht, err)
		n, err := Parse(ht.Context(), []byte("---\ntitle: "+string(quoted)+"\n---\n"))
		require.NoError(ht, err)
		require.Equal(ht, "valid", n.FrontmatterState)
		require.Equal(ht, title, n.Metadata.Title)
	}, hegel.WithTestCases(1000))
}

func TestPropertyRejectsInvalidOriginal(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		data := hegel.Draw(ht, hegel.Binary(0, 4096))
		// Append a guaranteed forbidden original byte, instead of discarding valid
		// generated cases or asserting only that arbitrary input does not crash.
		forbidden := hegel.Draw(ht, hegel.SampledFrom([]byte{0, 0xff, 0xc0}))
		data = append(data, forbidden)
		n, err := Parse(ht.Context(), data)
		require.ErrorIs(ht, err, ErrInvalidInput)
		require.Equal(ht, Note{}, n)
	}, hegel.WithTestCases(1000))
}

func TestPropertyEmptyATXHeadings(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		level := hegel.Draw(ht, hegel.Integers[int](1, 6))
		spaces := hegel.Draw(ht, hegel.Integers[int](1, 32))
		space := hegel.Draw(ht, hegel.SampledFrom([]string{" ", "\t"}))
		closing := hegel.Draw(ht, hegel.Integers[int](0, 8))
		src := []byte(strings.Repeat("#", level) + strings.Repeat(space, spaces) + strings.Repeat("#", closing) + "\n")
		var n Note
		var err error
		require.NotPanics(ht, func() { n, err = Parse(ht.Context(), src) })
		require.NoError(ht, err)
		require.Len(ht, n.Headings, 1)
		require.Empty(ht, n.Headings[0].Markdown)
		require.Empty(ht, n.Headings[0].Text)
		require.Equal(ht, "section", n.Headings[0].Anchor)
	}, hegel.WithTestCases(1000))
}

func TestPropertyHTMLClosingBlocksIgnoreBlankLines(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		pair := hegel.Draw(ht, hegel.SampledFrom([][2]string{{"<pre>", "</pre>"}, {"<SCRIPT>", "</script>"}, {"<style>", "</STYLE>"}, {"<textarea>", "</textarea>"}, {"<!--", "-->"}, {"<?work", "?>"}, {"<![CDATA[", "]]>"}}))
		blanks := hegel.Draw(ht, hegel.Integers[int](1, 20))
		src := []byte(pair[0] + "\ntext\n" + strings.Repeat("\n", blanks) + "# Hidden\n[[Hidden]]\n" + pair[1] + "\n# Visible\n[[Visible]]\n")
		n, err := Parse(ht.Context(), src)
		require.NoError(ht, err)
		require.Equal(ht, []string{"Visible"}, headingTexts(n.Headings))
		require.Equal(ht, []string{"Visible"}, linkTargets(n.Links))
	}, hegel.WithTestCases(1000))
}

// Removing per-token admission permits a small source to allocate more syntax
// nodes than the recipe allows, even when none of its tokens is emphasis.
func TestPropertyDisplayTokenAdmission(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		count := hegel.Draw(ht, hegel.Integers[int](2, 32))
		token := hegel.Draw(ht, hegel.SampledFrom([]string{"&", `\!`, "`x` ", "&amp;"}))
		src := []byte(strings.Repeat(token, count))
		b := &budget{nodes: maxNodes - count + 1}
		d := headingDisplay{ctx: ht.Context(), src: src, b: b}
		_, err := d.render(0, len(src), 0)
		requireLimit(ht, err, "ast_nodes")
		require.Equal(ht, maxNodes, b.nodes)
	}, hegel.WithTestCases(1000))
}
func TestDisplayTokenPublicLimit(t *testing.T) {
	n, err := Parse(t.Context(), []byte("# "+strings.Repeat("&", 300000)+"\n"))
	requireLimit(t, err, "ast_nodes")
	require.Equal(t, Note{}, n)
}

func TestPropertyContainerLifetime(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		depth := hegel.Draw(ht, hegel.Integers[int](1, 12))
		prefix := strings.Repeat("> ", depth)
		opener := hegel.Draw(ht, hegel.SampledFrom([]string{"```", "~~~", "<pre>", "<!--"}))
		src := []byte(prefix + opener + "\n" + prefix + "[hidden](ghost.md)\n\n# Visible\n[[Target]]\n")
		n, err := Parse(ht.Context(), src)
		require.NoError(ht, err)
		require.Equal(ht, []string{"Visible"}, headingTexts(n.Headings))
		require.Equal(ht, []string{"Target"}, linkTargets(n.Links))
	}, hegel.WithTestCases(1000))
}
func TestPropertyListRelativeCode(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		marker := hegel.Draw(ht, hegel.SampledFrom([]string{"- ", "123. ", "+ ", "9) "}))
		spaces := len(marker)
		indent := hegel.Draw(ht, hegel.Integers[int](0, 3))
		src := []byte(strings.Repeat(" ", indent) + marker + "[shown](real.md)\n\n" + strings.Repeat(" ", indent+spaces+4) + "[hidden](ghost.md)\n\n# Root\n[[Target]]\n")
		n, err := Parse(ht.Context(), src)
		require.NoError(ht, err)
		require.Equal(ht, []string{"Root"}, headingTexts(n.Headings))
		require.Equal(ht, []string{"real.md", "Target"}, linkTargets(n.Links))
	}, hegel.WithTestCases(1000))
}
func TestContainerParagraphContinuation(t *testing.T) {
	for _, src := range []string{"> paragraph\nlazy [[Shown]]\n\n# Root\n", "- paragraph\nlazy [[Shown]]\n\n# Root\n", "- ```\n  code\n\n# Root\n[[Shown]]\n", "> - ```\n>   code\n\n# Root\n[[Shown]]\n"} {
		n, err := Parse(t.Context(), []byte(src))
		require.NoError(t, err)
		require.Equal(t, []string{"Root"}, headingTexts(n.Headings))
		require.Equal(t, []string{"Shown"}, linkTargets(n.Links))
	}
}

func TestPropertyDefinitionBlockContext(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		name := hegel.Draw(ht, hegel.SampledFrom([]string{"r", "é", "Reference"}))
		prefix := hegel.Draw(ht, hegel.SampledFrom([]string{"# ", "## ", "paragraph\n", "> # ", "- # ", "`open\n"}))
		suffix := ""
		if prefix == "`open\n" {
			suffix = "\nclose`"
		}
		src := []byte(prefix + "[" + name + "]: ghost.md" + suffix + "\n\n[x][" + name + "]\n")
		n, err := Parse(ht.Context(), src)
		require.NoError(ht, err)
		require.Empty(ht, n.Links)
		for _, container := range []string{"", "> ", "- "} {
			good := []byte("paragraph\n\n" + container + "[" + name + "]: real.md\n\n[x][" + name + "]\n")
			n, err = Parse(ht.Context(), good)
			require.NoError(ht, err)
			require.Equal(ht, []string{"real.md"}, linkTargets(n.Links))
		}
	}, hegel.WithTestCases(1000))
}

func TestPropertyBlockPlainScalarMeaning(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		count := hegel.Draw(ht, hegel.Integers[int](1, 20))
		punctuation := hegel.Draw(ht, hegel.SampledFrom([]string{", ", "]", "}", "'", "\""}))
		value := "Alpha" + strings.Repeat(punctuation, count) + " Omega"
		quoted, err := json.Marshal(value)
		require.NoError(ht, err)
		for _, spelling := range []string{value, string(quoted)} {
			n, err := Parse(ht.Context(), []byte("---\ntitle: "+spelling+"\naliases: [Greeting]\n---\n# Body\n"))
			require.NoError(ht, err)
			require.Equal(ht, "valid", n.FrontmatterState)
			require.Equal(ht, Metadata{Title: value, Aliases: []string{"Greeting"}}, n.Metadata)
		}
	}, hegel.WithTestCases(1000))
}

func TestPropertyFoldedScalarMeaning(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		base := hegel.Draw(ht, hegel.Integers[int](1, 8))
		pad := strings.Repeat(" ", base)
		tc := hegel.Draw(ht, hegel.SampledFrom([]struct{ body, want string }{
			{"Alpha\n  Beta\nGamma\n", "Alpha\n  Beta\nGamma\n"},
			{"Alpha\n\nGamma\n", "Alpha\nGamma\n"},
			{"Alpha\n\n\nGamma\n", "Alpha\n\nGamma\n"},
			{"\nAlpha\nGamma\n", "\nAlpha Gamma\n"},
			{"Alpha\n  Beta\n\nGamma\n", "Alpha\n  Beta\n\nGamma\n"},
			{"Alpha\nGamma\n\n\n", "Alpha Gamma\n\n\n"},
		}))
		marker := hegel.Draw(ht, hegel.SampledFrom([]string{">", ">-", ">+"}))
		body := pad + strings.ReplaceAll(strings.TrimSuffix(tc.body, "\n"), "\n", "\n"+pad) + "\n"
		want := tc.want
		switch marker {
		case ">-":
			want = strings.TrimRight(want, "\n")
		case ">":
			want = strings.TrimRight(want, "\n") + "\n"
		}
		n, err := Parse(ht.Context(), []byte("---\naliases: "+marker+"\n"+body+"---\n"))
		require.NoError(ht, err)
		require.Equal(ht, "valid", n.FrontmatterState)
		require.Equal(ht, []string{want}, n.Metadata.Aliases)
	}, hegel.WithTestCases(1000))
}

func TestPropertyUnknownYAMLKeyTrust(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		key := hegel.Draw(ht, hegel.SampledFrom([]string{"*missing", "&anchor key", "!custom key", "<<", "null", "true", "42"}))
		before := hegel.Draw(ht, hegel.Booleans())
		extra := "extra: {" + key + ": value}\n"
		good := "title: Good\naliases: [Trusted]\n"
		header := extra + good
		if before {
			header = good + extra
		}
		n, err := Parse(ht.Context(), []byte("---\n"+header+"---\n# Body\n[[Target]]\n"))
		require.NoError(ht, err)
		require.Equal(ht, "invalid", n.FrontmatterState)
		require.Equal(ht, Metadata{}, n.Metadata)
		require.Equal(ht, []string{"Body"}, headingTexts(n.Headings))
		require.Equal(ht, []string{"Target"}, linkTargets(n.Links))
	}, hegel.WithTestCases(1000))
}

func TestPropertyArtifactAdmissionBeforeContent(t *testing.T) {
	base, err := Parse(t.Context(), nil)
	require.NoError(t, err)
	// Invalid UTF-8 makes the required ordering observable without timing or
	// allocation heuristics: oversized content is not inspected first.
	giant := string([]byte{0xff}) + strings.Repeat("x", 8<<20)
	hegel.Test(t, func(ht *hegel.T) {
		field := hegel.Draw(ht, hegel.Integers[int](0, 2))
		n := base
		n.SourceBytes = 1
		h := Heading{Level: 1, Span: Span{0, 1}, Section: Span{0, 1}, Anchor: "a"}
		switch field {
		case 0:
			h.Anchor = giant
		case 1:
			h.Text = giant
		case 2:
			h.Markdown = giant
		}
		n.Headings = []Heading{h}
		c := &cancellationChecks{Context: ht.Context(), remaining: 2}
		data, err := CanonicalArtifact(c, n)
		requireLimit(ht, err, "derived_bytes")
		require.Nil(ht, data)
		require.Equal(ht, 1, c.remaining)
	}, hegel.WithTestCases(1000))
}
func TestArtifactValidationCancellation(t *testing.T) {
	n, err := Parse(t.Context(), nil)
	require.NoError(t, err)
	n.SourceBytes = 1
	n.Headings = []Heading{{Level: 1, Span: Span{0, 1}, Section: Span{0, 1}, Anchor: strings.Repeat("a", 3000) + string([]byte{0xff})}}
	c := &cancellationChecks{Context: t.Context(), remaining: 3}
	data, err := CanonicalArtifact(c, n)
	require.ErrorIs(t, err, context.Canceled)
	require.Nil(t, data)
	require.Zero(t, c.remaining)
}

func TestPropertyPlannedKeysResolve(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		letter := byte(hegel.Draw(ht, hegel.Integers[int]('A', 'Z')))
		before := hegel.Draw(ht, hegel.Integers[int](0, 12))
		drive := string(letter) + ":foo.md"
		prefix := strings.Repeat("./", before)
		for _, tc := range []struct{ source, form, target string }{
			{"area/start.md", "markdown", prefix + "../" + drive},
			{"start.md", "wiki", "/" + prefix + drive},
		} {
			q, err := PlanTarget(tc.source, Link{Form: tc.form, Target: tc.target})
			require.NoError(ht, err)
			require.Equal(ht, "invalid", q.Kind)
			require.Empty(ht, q.Keys)
		}
		target := hegel.Draw(ht, hegel.SampledFrom([]string{"../é.md", "nested/../Note.md", "%252F.md", "/Root", "Caf%C3%A9", "A%23B.md", ".hidden.md"}))
		q, err := PlanTarget("area/start.md", Link{Form: "wiki", Target: target})
		require.NoError(ht, err)
		require.Equal(ht, "note", q.Kind)
		got, err := Resolve(ht.Context(), q, CandidateSet{Complete: true})
		require.NoError(ht, err)
		require.Equal(ht, "missing", got.State)
	}, hegel.WithTestCases(1000))
}

func TestPropertyExactSyntaxEntities(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		tc := hegel.Draw(ht, hegel.SampledFrom([]struct{ raw, want string }{
			{"&notit;", "&notit;"}, {"&amp;amp;", "&amp;"}, {"&amp", "&amp"}, {"&#87654321;", "&#87654321;"},
			{"&#x00000041;", "&#x00000041;"}, {"&#128;", "\u0080"}, {"&#0;", "\ufffd"}, {"&#xD800;", "\ufffd"},
			{"&acE;", "∾̳"}, {`\&amp;`, "&amp;"}, {"&#37;252F", "%252F"},
		}))
		n, err := Parse(ht.Context(), []byte("# "+tc.raw+"\n\n[x]("+tc.raw+")\n"))
		require.NoError(ht, err)
		require.Equal(ht, tc.want, n.Headings[0].Text)
		require.Equal(ht, tc.want, n.Links[0].Target)
	}, hegel.WithTestCases(1000))
}

func TestPropertyMetadataLimitBeatsTypeDiagnostic(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		field := hegel.Draw(ht, hegel.SampledFrom([]string{"aliases", "tags"}))
		oversized := hegel.Draw(ht, hegel.SampledFrom([]struct{ value, resource string }{
			{strings.Repeat("a", 1025), "metadata_value_bytes"}, {"[" + strings.Repeat("a,", 256) + "a]", field},
		}))
		n, err := Parse(ht.Context(), []byte("---\ntitle: 42\n"+field+": "+oversized.value+"\n---\n# Body\n"))
		requireLimit(ht, err, oversized.resource)
		require.Equal(ht, Note{}, n)
	}, hegel.WithTestCases(1000))
}
func TestPropertyQuotedLineFolding(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		spaces := hegel.Draw(ht, hegel.Integers[int](1, 8))
		pad := strings.Repeat(" ", spaces)
		tc := hegel.Draw(ht, hegel.SampledFrom([]struct{ value, want string }{
			{"'Alpha\n" + pad + "Beta'", "Alpha Beta"},
			{"\"Alpha\n" + pad + "Beta\"", "Alpha Beta"},
			{"'Alpha\n\n" + pad + "Beta'", "Alpha\nBeta"},
			{"\"Alpha\\\n" + pad + "Beta\"", "AlphaBeta"},
		}))
		n, err := Parse(ht.Context(), []byte("---\ntitle: "+tc.value+"\n---\n"))
		require.NoError(ht, err)
		require.Equal(ht, "valid", n.FrontmatterState)
		require.Equal(ht, tc.want, n.Metadata.Title)
	}, hegel.WithTestCases(1000))
}
