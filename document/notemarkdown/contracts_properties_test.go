package notemarkdown

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"hegel.dev/go/hegel"
)

func artifactFixture() Note {
	return Note{SourceSHA256: strings.Repeat("a", 64), SourceBytes: 8, Recipe: Recipe, FrontmatterState: "absent",
		Metadata:    Metadata{Title: "Title", Aliases: []string{"Alias"}, Tags: []string{"tag"}},
		Headings:    []Heading{{Level: 1, Span: Span{0, 2}, Section: Span{0, 8}, Markdown: "M", Text: "M", Anchor: "m"}},
		Links:       []Link{{Span: Span{3, 8}, DestinationSpan: Span{4, 7}, Form: "markdown", RawTarget: "a.md", Target: "a.md"}},
		Diagnostics: []Diagnostic{{Code: "example", Span: Span{0, 0}}}}
}

func TestPropertyArtifactStandardJSON(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		text := hegel.Draw(ht, hegel.SampledFrom([]string{"", "é星e\u0301", "<>&\u2028\u2029", "\x00\x01\b\f\n\r\t\"\\"}))
		n := artifactFixture()
		n.Metadata.Title = text
		n.Headings[0].Text = text
		n.Headings[0].Markdown = text
		actual, err := CanonicalArtifact(ht.Context(), n)
		require.NoError(ht, err)
		var buf bytes.Buffer
		enc := json.NewEncoder(&buf)
		enc.SetEscapeHTML(false)
		require.NoError(ht, enc.Encode(n))
		require.Equal(ht, bytes.TrimSuffix(buf.Bytes(), []byte{'\n'}), actual)
		var decoded Note
		require.NoError(ht, json.Unmarshal(actual, &decoded))
		require.Equal(ht, n, decoded)
		saved := bytes.Clone(actual)
		actual[0] = '!'
		next, err := CanonicalArtifact(ht.Context(), n)
		require.NoError(ht, err)
		require.Equal(ht, saved, next)
	}, hegel.WithTestCases(1000))
}
func TestPropertyArtifactInvalidMutations(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		n := artifactFixture()
		edit := hegel.Draw(ht, hegel.SampledFrom([]func(*Note){
			func(n *Note) { n.Recipe = "unknown" }, func(n *Note) { n.SourceSHA256 = strings.Repeat("A", 64) },
			func(n *Note) { n.SourceBytes = -1 }, func(n *Note) { n.BodyStart = 9 }, func(n *Note) { n.Frontmatter.End = 9 },
			func(n *Note) { n.FrontmatterState = "unknown" }, func(n *Note) { n.Headings[0].Level = 7 },
			func(n *Note) { n.Headings[0].Span.Start = -1 }, func(n *Note) { n.Headings[0].Section.End = 0 },
			func(n *Note) { n.Headings[0].Anchor = "" }, func(n *Note) { n.Headings[0].Text = string([]byte{0xff}) },
			func(n *Note) { n.Links[0].Form = "unknown" }, func(n *Note) { n.Links[0].DestinationSpan.End = 9 },
			func(n *Note) { n.Diagnostics[0].Code = "Not_stable" }, func(n *Note) { n.Diagnostics[0].Span.End = 9 },
		}))
		edit(&n)
		data, err := CanonicalArtifact(ht.Context(), n)
		require.ErrorIs(ht, err, ErrInvalidArtifact)
		require.Nil(ht, data)
	}, hegel.WithTestCases(1000))
}
func TestPropertyExactOriginalHash(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		bom := hegel.Draw(ht, hegel.SampledFrom([]string{"", "\ufeff"}))
		eol := hegel.Draw(ht, hegel.SampledFrom([]string{"\n", "\r\n"}))
		text := hegel.Draw(ht, hegel.SampledFrom([]string{"", "é", "e\u0301", "星🦉"}))
		src := []byte(bom + "---" + eol + "title: '" + text + "'" + eol + "---" + eol + "# " + text + eol)
		saved := bytes.Clone(src)
		sum := sha256.Sum256(saved)
		n, err := Parse(ht.Context(), src)
		require.NoError(ht, err)
		require.Equal(ht, hex.EncodeToString(sum[:]), n.SourceSHA256)
		require.Equal(ht, len(saved), n.SourceBytes)
		require.Equal(ht, saved, src)
		assertOriginalSpan(ht, src, n.Frontmatter)
		for _, d := range n.Diagnostics {
			assertOriginalSpan(ht, src, d.Span)
		}
	}, hegel.WithTestCases(1000))
}
func TestPropertyParseToPlanSyntax(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		tc := hegel.Draw(ht, hegel.SampledFrom([]struct{ spelling, want string }{
			{"A&amp;B.md", "A&B.md"}, {`A\&B.md`, "A&B.md"}, {"A&#38;B.md", "A&B.md"},
			{"A%252F.md", "A%2F.md"}, {"A&#37;252F.md", "A%2F.md"}, {"A%23B.md", "A#B.md"},
			{"Caf%C3%A9.md", "Café.md"}, {"Cafe%CC%81.md", "Cafe\u0301.md"},
		}))
		n, err := Parse(ht.Context(), []byte("[x]("+tc.spelling+")"))
		require.NoError(ht, err)
		require.Len(ht, n.Links, 1)
		require.Equal(ht, tc.spelling, n.Links[0].RawTarget)
		q, err := PlanTarget("area/start.md", n.Links[0])
		require.NoError(ht, err)
		require.Equal(ht, TargetQuery{Kind: "note", Keys: []LookupKey{{"path", "area/" + tc.want}}}, q)
	}, hegel.WithTestCases(1000))
}
func TestPropertyResolverOrderedPathsAndDuplicates(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		reversed := hegel.Draw(ht, hegel.Booleans())
		complete := hegel.Draw(ht, hegel.Booleans())
		duplicates := hegel.Draw(ht, hegel.Integers[int](0, 6))
		rev := fmt.Sprintf("rev-星-%d", hegel.Draw(ht, hegel.Integers[int](0, 10000)))
		keys := []LookupKey{{"path", "A.md"}, {"path", "B.md"}, {"alias", "Shared"}, {"basename", "A"}}
		rows := []Candidate{{Binding: "a", Path: "A.md", Kind: "note", Keys: []LookupKey{keys[0], keys[2], keys[3]}}, {Binding: "b", Path: "B.md", Kind: "note", Keys: []LookupKey{keys[1], keys[2]}}}
		for range duplicates {
			rows = append(rows, rows[hegel.Draw(ht, hegel.Integers[int](0, 1))])
		}
		if reversed {
			keys[0], keys[1] = keys[1], keys[0]
		}
		for i := len(rows) - 1; i > 0; i-- {
			j := hegel.Draw(ht, hegel.Integers[int](0, i))
			rows[i], rows[j] = rows[j], rows[i]
		}
		got, err := Resolve(ht.Context(), TargetQuery{Kind: "note", Keys: keys}, CandidateSet{Complete: complete, NamespaceGeneration: rev, Candidates: rows})
		require.NoError(ht, err)
		want := Resolution{State: "pending", NamespaceGeneration: rev}
		if complete {
			want.State = "resolved"
			want.Binding = "a"
			if reversed {
				want.Binding = "b"
			}
		}
		require.Equal(ht, want, got)
	}, hegel.WithTestCases(1000))
}
func TestPropertyResolverTrailingInvalidEvidence(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		key := LookupKey{"path", "A.md"}
		valid := Candidate{Binding: "a", Path: "A.md", Kind: "note", Keys: []LookupKey{key}}
		bad := valid
		bad.Binding = "b"
		edit := hegel.Draw(ht, hegel.SampledFrom([]func(*Candidate){
			func(c *Candidate) { c.Binding = "nul\x00" }, func(c *Candidate) { c.Binding = string([]byte{0xff}) },
			func(c *Candidate) { c.Path = "B.md" }, func(c *Candidate) { c.Keys = []LookupKey{{"alias", "foreign"}} },
			func(c *Candidate) { c.Kind = "unknown" },
		}))
		edit(&bad)
		complete := hegel.Draw(ht, hegel.Booleans())
		got, err := Resolve(ht.Context(), TargetQuery{Kind: "note", Keys: []LookupKey{key}}, CandidateSet{Complete: complete, Candidates: []Candidate{valid, bad}})
		require.ErrorIs(ht, err, ErrInvalidCandidateSet)
		require.Equal(ht, Resolution{}, got)
		for _, kind := range []string{"external", "attachment", "unsupported", "invalid"} {
			got, err = Resolve(ht.Context(), TargetQuery{Kind: kind}, CandidateSet{Candidates: []Candidate{bad}})
			require.ErrorIs(ht, err, ErrInvalidCandidateSet)
			require.Equal(ht, Resolution{}, got)
		}
	}, hegel.WithTestCases(1000))
}
func TestResolverIndependentRowLimits(t *testing.T) {
	key := LookupKey{"alias", "Shared"}
	q := TargetQuery{Kind: "note", Keys: []LookupKey{key}}
	a := Candidate{Binding: "a", Path: "A.md", Kind: "note", Keys: []LookupKey{key}}
	rows := make([]Candidate, 8)
	for i := range rows {
		rows[i] = a
	}
	got, err := Resolve(t.Context(), q, CandidateSet{Complete: true, Candidates: rows})
	require.NoError(t, err)
	require.Equal(t, "a", got.Binding)
	_, err = Resolve(t.Context(), q, CandidateSet{Complete: true, Candidates: append([]Candidate{a}, rows...)})
	require.ErrorIs(t, err, ErrInvalidCandidateSet)
	rows[1] = Candidate{Binding: "b", Path: "B.md", Kind: "note", Keys: []LookupKey{key}}
	got, err = Resolve(t.Context(), q, CandidateSet{Complete: true, Candidates: rows})
	require.NoError(t, err)
	require.Equal(t, "ambiguous", got.State)
	got, err = Resolve(t.Context(), q, CandidateSet{Complete: false, Candidates: rows})
	require.NoError(t, err)
	require.Equal(t, "pending", got.State)
	rows[2] = Candidate{Binding: "c", Path: "C.md", Kind: "note", Keys: []LookupKey{key}}
	_, err = Resolve(t.Context(), q, CandidateSet{Complete: true, Candidates: rows[:3]})
	require.ErrorIs(t, err, ErrInvalidCandidateSet)
}
func TestPropertyFragmentTextAndCopies(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		hs := []Heading{{Level: 1, Span: Span{0, 2}, Section: Span{0, 10}, Text: "A", Anchor: "a"}, {Level: 2, Span: Span{3, 5}, Section: Span{3, 8}, Text: "Cafe\u0301", Anchor: "café"}, {Level: 1, Span: Span{8, 10}, Section: Span{8, 10}, Text: "!!!", Anchor: "section"}}
		tc := hegel.Draw(ht, hegel.SampledFrom([]struct {
			fragment string
			index    int
		}{{"A!", 0}, {"a", 0}, {"Café!", 1}, {"cafe\u0301", 1}, {"!!!", 2}, {"section", 2}}))
		got, err := ResolveFragment(ht.Context(), tc.fragment, hs)
		require.NoError(ht, err)
		require.Equal(ht, "resolved", got.State)
		require.Equal(ht, hs[tc.index].Span, *got.Heading)
		saved := hs[tc.index].Span
		got.Heading.End++
		require.Equal(ht, saved, hs[tc.index].Span)
	}, hegel.WithTestCases(1000))
}
func TestPropertyFragmentValidatesTrailingEvidence(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		hs := []Heading{{Level: 1, Span: Span{0, 2}, Section: Span{0, 4}, Text: "A", Anchor: "a"}, {Level: 1, Span: Span{2, 4}, Section: Span{2, 4}, Text: "B", Anchor: "b"}}
		edit := hegel.Draw(ht, hegel.SampledFrom([]func(*Heading){
			func(h *Heading) { h.Level = 0 }, func(h *Heading) { h.Anchor = "a" }, func(h *Heading) { h.Span.Start = -1 },
			func(h *Heading) { h.Text = string([]byte{0xff}) }, func(h *Heading) { h.Anchor = "bad\x00" },
		}))
		edit(&hs[1])
		got, err := ResolveFragment(ht.Context(), "a", hs)
		require.ErrorIs(ht, err, ErrInvalidInput)
		require.Equal(ht, FragmentResolution{}, got)
	}, hegel.WithTestCases(1000))
}
func TestFragmentUnusedHeadings(t *testing.T) {
	bad := []Heading{{Level: -1, Anchor: string([]byte{0xff})}}
	for _, tc := range []struct{ fragment, state string }{{"", "none"}, {"^block", "unsupported"}} {
		got, err := ResolveFragment(t.Context(), tc.fragment, bad)
		require.NoError(t, err)
		require.Equal(t, tc.state, got.State)
		_, err = ResolveFragment(t.Context(), tc.fragment, make([]Heading, 4097))
		requireLimit(t, err, "headings")
	}
	_, err := ResolveFragment(t.Context(), "\xff", nil)
	require.ErrorIs(t, err, ErrInvalidInput)
}
func TestPropertyFragmentAdmissionBeforeContent(t *testing.T) {
	giant := string([]byte{0xff}) + strings.Repeat("x", 8<<20)
	hegel.Test(t, func(ht *hegel.T) {
		field := hegel.Draw(ht, hegel.Integers[int](0, 2))
		h := Heading{Level: 1, Span: Span{0, 1}, Section: Span{0, 1}, Anchor: "a"}
		switch field {
		case 0:
			h.Anchor = giant
		case 1:
			h.Text = giant
		case 2:
			h.Markdown = giant
		}
		got, err := ResolveFragment(ht.Context(), "a", []Heading{h})
		requireLimit(ht, err, "derived_bytes")
		require.Equal(ht, FragmentResolution{}, got)
	}, hegel.WithTestCases(1000))
}
func TestFragmentValidationCancellation(t *testing.T) {
	h := Heading{Level: 1, Span: Span{0, 1}, Section: Span{0, 1}, Anchor: strings.Repeat("a", 3000) + string([]byte{0xff})}
	c := &cancellationChecks{Context: t.Context(), remaining: 3}
	got, err := ResolveFragment(c, "a", []Heading{h})
	require.ErrorIs(t, err, context.Canceled)
	require.Equal(t, FragmentResolution{}, got)
}

func TestPropertyHeadingSectionLevels(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		count := hegel.Draw(ht, hegel.Integers[int](1, 30))
		eol := hegel.Draw(ht, hegel.SampledFrom([]string{"\n", "\r\n"}))
		src := ""
		levels := make([]int, count)
		starts := make([]int, count)
		for i := range count {
			levels[i] = hegel.Draw(ht, hegel.Integers[int](1, 6))
			starts[i] = len(src)
			src += strings.Repeat("#", levels[i]) + " Heading" + eol + "body é" + eol
		}
		n, err := Parse(ht.Context(), []byte(src))
		require.NoError(ht, err)
		require.Len(ht, n.Headings, count)
		// Independent quadratic definition, rather than the production stack.
		for i, h := range n.Headings {
			end := len(src)
			for j := i + 1; j < count; j++ {
				if levels[j] <= levels[i] {
					end = starts[j]
					break
				}
			}
			require.Equal(ht, Span{starts[i], end}, h.Section)
			require.Equal(ht, levels[i], h.Level)
		}
	}, hegel.WithTestCases(1000))
}
func TestPropertyHeadingLiteralDelimiters(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		tc := hegel.Draw(ht, hegel.SampledFrom([]struct{ raw, want string }{
			{`\*literal\*`, `*literal*`}, {`\_literal\_`, `_literal_`}, {"*unclosed", "*unclosed"}, {"closed*", "closed*"}, {"`unclosed", "`unclosed"}, {"a_b_c", "a_b_c"}, {"~~strike~~", "~~strike~~"},
		}))
		n, err := Parse(ht.Context(), []byte("# "+tc.raw+"\n"))
		require.NoError(ht, err)
		require.Equal(ht, tc.want, n.Headings[0].Text)
	}, hegel.WithTestCases(1000))
}
func TestPropertyResolverFallbackKeyPermutation(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		keys := []LookupKey{{"basename", "Shared"}, {"alias", "Shared"}}
		if hegel.Draw(ht, hegel.Booleans()) {
			keys[0], keys[1] = keys[1], keys[0]
		}
		same := hegel.Draw(ht, hegel.Booleans())
		binding := "b"
		otherPath := "b/Shared.md"
		if same {
			binding = "a"
			otherPath = "a/Shared.md"
		}
		rows := []Candidate{{Binding: "a", Path: "a/Shared.md", Kind: "note", Keys: []LookupKey{keys[0]}}, {Binding: binding, Path: otherPath, Kind: "note", Keys: []LookupKey{keys[1]}}}
		if hegel.Draw(ht, hegel.Booleans()) {
			rows[0], rows[1] = rows[1], rows[0]
		}
		r, err := Resolve(ht.Context(), TargetQuery{Kind: "note", Keys: keys}, CandidateSet{Complete: true, Candidates: rows})
		require.NoError(ht, err)
		want := Resolution{State: "ambiguous"}
		if same {
			want = Resolution{State: "resolved", Binding: "a"}
		}
		require.Equal(ht, want, r)
	}, hegel.WithTestCases(1000))
}
func TestPropertyPlanFragmentDecodedOnce(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		tc := hegel.Draw(ht, hegel.SampledFrom([]struct{ raw, fragment, anchor string }{
			{"#A%20B", "A B", "a-b"}, {"#A%2520B", "A%20B", "a20b"}, {"#Caf%C3%A9", "Café", "café"},
		}))
		q, err := PlanTarget("A.md", Link{Form: "wiki", Target: tc.raw})
		require.NoError(ht, err)
		require.Equal(ht, tc.fragment, q.Fragment)
		hs := []Heading{{Level: 1, Span: Span{0, 1}, Section: Span{0, 1}, Text: tc.fragment, Anchor: tc.anchor}}
		r, err := ResolveFragment(ht.Context(), q.Fragment, hs)
		require.NoError(ht, err)
		require.Equal(ht, "resolved", r.State)
	}, hegel.WithTestCases(1000))
}
func TestTargetFilenameAndEmbedPolicy(t *testing.T) {
	for _, tc := range []struct{ target, kind string }{{".hidden", "attachment"}, {"name.", "attachment"}, {"Next.MD", "note"}, {"x:note", "invalid"}, {"xx:note", "external"}} {
		for _, form := range []string{"wiki", "markdown", "markdown_reference"} {
			q, err := PlanTarget("A.md", Link{Form: form, Target: tc.target})
			require.NoError(t, err)
			require.Equal(t, tc.kind, q.Kind)
			embedded, err := PlanTarget("A.md", Link{Form: form, Target: tc.target, Embed: true})
			require.NoError(t, err)
			require.Equal(t, q, embedded)
		}
	}
}
func TestResolveRejectsFalseBasename(t *testing.T) {
	key := LookupKey{"basename", "Shared"}
	r, err := Resolve(t.Context(), TargetQuery{Kind: "note", Keys: []LookupKey{key}}, CandidateSet{Complete: true, Candidates: []Candidate{{Binding: "a", Path: "Other.md", Kind: "note", Keys: []LookupKey{key}}}})
	require.ErrorIs(t, err, ErrInvalidCandidateSet)
	require.Equal(t, Resolution{}, r)
}

func TestPropertyRootFlowMapping(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		tc := hegel.Draw(ht, hegel.SampledFrom([]struct{ header, title string }{{"{}", ""}, {"{title: Good, tags: [x]}", "Good"}, {"{\n title: Good,\n aliases: [A, B]\n}", "Good"}}))
		n, err := Parse(ht.Context(), []byte("---\n"+tc.header+"\n---\n# Body\n"))
		require.NoError(ht, err)
		require.Equal(ht, "valid", n.FrontmatterState)
		require.Equal(ht, tc.title, n.Metadata.Title)
		require.Equal(ht, "Body", n.Headings[0].Text)
	}, hegel.WithTestCases(1000))
}

func TestPropertyHTMLDeclarationLetters(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		keyword := hegel.Draw(ht, hegel.SampledFrom([]string{"DOCTYPE", "doctype", "DoCtYpE"}))
		src := hegel.Draw(ht, hegel.SampledFrom([]string{"<!%s [x](a.md)>\n# Body\n", "# Before <!%s [x](a.md)> After\n"}))
		n, err := Parse(ht.Context(), []byte(fmt.Sprintf(src, keyword)))
		require.NoError(ht, err)
		require.Empty(ht, n.Links)
		want := "Body"
		if strings.HasPrefix(src, "#") {
			want = "Before After"
		}
		require.Equal(ht, want, n.Headings[0].Text)
	}, hegel.WithTestCases(1000))
}
func TestHTMLType7ContainerTransition(t *testing.T) {
	for _, tc := range []struct {
		source string
		links  int
	}{
		{"intro\n> <span>[notes](a.md)</span>\n", 1},
		{"intro\n> <span>\n> [notes](a.md)\n", 0},
		{"intro\n- <span>\n  [notes](a.md)\n", 0},
		{"intro\n<span>\n[notes](a.md)\n", 1},
	} {
		n, err := Parse(t.Context(), []byte(tc.source))
		require.NoError(t, err)
		require.Len(t, n.Links, tc.links, tc.source)
	}
}
