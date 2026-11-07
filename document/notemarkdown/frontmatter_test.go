package notemarkdown

import (
	"bytes"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"hegel.dev/go/hegel"
)

func TestFrontmatterBOMCRLF(t *testing.T) {
	src := []byte("\ufeff---\r\ntitle: Café\r\naliases: [A, B]\r\ntags: [x]\r\n...\r\n# H\r\n")
	n, err := Parse(t.Context(), src)
	require.NoError(t, err)
	require.Equal(t, "valid", n.FrontmatterState)
	require.Equal(t, Metadata{Title: "Café", Aliases: []string{"A", "B"}, Tags: []string{"x"}}, n.Metadata)
	require.Equal(t, bytes.Index(src, []byte("# H")), n.BodyStart)
	require.Equal(t, 3, n.Frontmatter.Start)
	require.Equal(t, n.BodyStart, n.Frontmatter.End)
	require.Len(t, n.Headings, 1)
}

func TestFrontmatterTrustGroup(t *testing.T) {
	for name, header := range map[string]string{
		"duplicate": "title: A\ntitle: B", "root": "[a, b]", "number": "title: 42",
		"boolean": "title: true", "timestamp": "title: 2026-01-02", "null": "title: null",
		"list entry": "title: okay\ntags: [x, 2]", "alias": "other: &x A\naliases: *x",
		"merge": "<<: {title: A}", "tag": "title: !custom A", "syntax": "title: [",
		"nested duplicate": "other: {a: x, a: y}",
	} {
		t.Run(name, func(t *testing.T) {
			n, err := Parse(t.Context(), []byte("---\n"+header+"\n---\n# H\n"))
			require.NoError(t, err)
			require.Equal(t, "invalid", n.FrontmatterState)
			require.Equal(t, Metadata{}, n.Metadata)
			require.NotEmpty(t, n.Diagnostics)
			require.Equal(t, "invalid_frontmatter", n.Diagnostics[0].Code)
			require.Len(t, n.Headings, 1)
		})
	}
	n, err := Parse(t.Context(), []byte("---\nunknown: {x: [1, false]}\ndocbank: {contract: fake}\ntitle: 'A: B'\naliases:\n  - one\n  - 'two'\ntags: tag\n---\n"))
	require.NoError(t, err)
	require.Equal(t, "valid", n.FrontmatterState)
	require.Equal(t, Metadata{Title: "A: B", Aliases: []string{"one", "two"}, Tags: []string{"tag"}}, n.Metadata)
}

func TestFrontmatterScalars(t *testing.T) {
	for _, tc := range []struct{ header, title string }{
		{`title: "a\tb\u00e9"`, "a\tbé"}, {"title: 'it''s good'", "it's good"},
		{"title: |-\n  first\n  second", "first\nsecond"},
		{"title: >-\n  first\n  second", "first second"},
		{"title: one # comment", "one"}, {"title: \"true\"", "true"},
	} {
		n, err := Parse(t.Context(), []byte("---\n"+tc.header+"\n---\n"))
		require.NoError(t, err)
		require.Equal(t, "valid", n.FrontmatterState, tc.header)
		require.Equal(t, tc.title, n.Metadata.Title, tc.header)
	}
}

func TestFrontmatterUnclosed(t *testing.T) {
	n, err := Parse(t.Context(), []byte("---\ntitle: X\n# H\n"))
	require.NoError(t, err)
	require.Equal(t, "invalid", n.FrontmatterState)
	require.Zero(t, n.BodyStart)
	require.Equal(t, "unclosed_frontmatter", n.Diagnostics[0].Code)
	require.Len(t, n.Headings, 1)
	n, err = Parse(t.Context(), []byte("intro\n\n---\ntitle: X\n---\n"))
	require.NoError(t, err)
	require.Equal(t, "absent", n.FrontmatterState)
}

func TestFrontmatterResourceLimits(t *testing.T) {
	for _, tc := range []struct{ resource, header string }{
		{"frontmatter_bytes", "unknown: " + strings.Repeat("a", 64<<10)},
		{"yaml_depth", "unknown: " + strings.Repeat("[", 33) + "x" + strings.Repeat("]", 33)},
		{"yaml_nodes", "unknown: [" + strings.Repeat("x,", 8193) + "x]"},
		{"aliases", "aliases: [" + strings.Repeat("x,", 256) + "x]"},
		{"tags", "tags: [" + strings.Repeat("x,", 256) + "x]"},
		{"title_bytes", "title: " + strings.Repeat("a", 4097)},
		{"metadata_value_bytes", "aliases: " + strings.Repeat("a", 1025)},
		{"metadata_value_bytes", "tags: " + strings.Repeat("a", 1025)},
	} {
		t.Run(tc.resource, func(t *testing.T) {
			n, err := Parse(t.Context(), []byte("---\n"+tc.header+"\n---\n# H\n"))
			requireLimit(t, err, tc.resource)
			require.Equal(t, Note{}, n)
		})
	}
}

func TestFrontmatterFlowWorkBudget(t *testing.T) {
	b := &budget{work: maxWork - 8192}
	_, err := parseYAML(t.Context(), []byte("unknown: [\""+strings.Repeat("a\n", 200)+"\"]"), b)
	requireLimit(t, err, "work_steps")
}

func TestPropertyFrontmatterUnicodeAndLineEndings(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		count := hegel.Draw(ht, hegel.Integers[int](1, 128))
		title := strings.Repeat("é", count)
		lf := []byte("---\ntitle: '" + title + "'\n---\n# H\n")
		crlf := bytes.ReplaceAll(lf, []byte("\n"), []byte("\r\n"))
		a, err := Parse(ht.Context(), lf)
		require.NoError(ht, err)
		b, err := Parse(ht.Context(), crlf)
		require.NoError(ht, err)
		require.Equal(ht, title, a.Metadata.Title)
		require.Equal(ht, a.Metadata, b.Metadata)
		require.NotEqual(ht, a.SourceSHA256, b.SourceSHA256)
		require.Equal(ht, "# H\r\n", string(crlf[b.Headings[0].Span.Start:b.Headings[0].Span.End]))
	}, hegel.WithTestCases(1000))
}
