package notemarkdown

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	_ "time/tzdata"

	"github.com/stretchr/testify/require"
)

func TestFrontmatterDecodedFields(t *testing.T) {
	src := []byte("\ufeff---\r\ntitle: Sample\r\naliases: [Alias, Other]\r\ntags: tag\r\nflag: true\r\nnumber: 0x2a\r\nfloat: 1.2500\r\nday: 2026-01-02\r\ntime: 2026-01-02T03:04:05+02:00\r\nlist: [one, 2, false, null, [nested], {key: value}]\r\nmap: {key: value}\r\n...\r\n# Body\r\n")
	n, err := Parse(t.Context(), src, Limits{})
	require.NoError(t, err)
	require.NotNil(t, n.Frontmatter)
	fm := n.Frontmatter
	require.Equal(t, 3, fm.Start)
	require.Equal(t, bytes.Index(src, []byte("# Body")), fm.End)
	require.Equal(t, fm.End, n.BodyStart)
	require.Equal(t, "Sample", n.Title)
	require.Equal(t, []string{"Alias", "Other"}, fm.Aliases)
	require.Equal(t, []string{"tag"}, fm.Tags)
	require.Equal(t, "2026-01-02T00:00:00Z", fm.Raw["day"])
	require.Equal(t, "2026-01-02T03:04:05+02:00", fm.Raw["time"])
	require.Contains(t, fm.Fields, Field{Key: "number", Value: "42", Ordinal: 0})
	require.Contains(t, fm.Fields, Field{Key: "float", Value: "1.25", Ordinal: 0})
	require.Contains(t, fm.Fields, Field{Key: "flag", Value: "true", Ordinal: 0})
	require.Contains(t, fm.Fields, Field{Key: "list", Value: "false", Ordinal: 2})
	for _, f := range fm.Fields {
		require.NotEqual(t, "map", f.Key)
		require.NotEqual(t, 4, f.Ordinal)
		require.NotEqual(t, 5, f.Ordinal)
	}
	_, err = json.Marshal(fm.Raw)
	require.NoError(t, err)
}

func TestFrontmatterAliasesMergesAndJSON(t *testing.T) {
	src := "---\nfirst: &first {title: First, count: 1}\nsecond: &second {title: Second, other: true}\n<<: [*first, *second]\ntitle: Explicit\ncopy: *first\n---\n# Body\n"
	n, err := Parse(t.Context(), []byte(src), Limits{})
	require.NoError(t, err)
	require.Equal(t, "Explicit", n.Title)
	require.Equal(t, true, n.Frontmatter.Raw["other"])
	require.Equal(t, 1, n.Frontmatter.Raw["count"])
	_, err = json.Marshal(n.Frontmatter.Raw)
	require.NoError(t, err)
	n, err = Parse(t.Context(), []byte(strings.ReplaceAll(src, "title: Explicit\n", "")), Limits{})
	require.NoError(t, err)
	require.Equal(t, "First", n.Title)
	n, err = Parse(t.Context(), []byte(src), Limits{MaxFrontmatterKeys: 4})
	requireLimit(t, err, "frontmatter_keys")
	require.Equal(t, Note{}, n)
}

func TestFrontmatterDiagnosticsLeaveBodyReadable(t *testing.T) {
	for _, header := range []string{"title: [bad", "a: &a [*a]", "a: .nan", "a: .inf", "a: {1: value}", "a: !custom value", "title: first\ntitle: second", "a: *missing"} {
		t.Run(header, func(t *testing.T) {
			n, err := Parse(t.Context(), []byte("---\n"+header+"\n---\n# Body\n[[Target]]\n"), Limits{MaxDiagnostics: 1})
			require.NoError(t, err)
			require.Nil(t, n.Frontmatter.Raw)
			require.Empty(t, n.Frontmatter.Fields)
			require.Len(t, n.Diagnostics, 1)
			require.Equal(t, "Body", n.Title)
			require.Equal(t, []string{"Target"}, linkTargets(n.Links))
		})
	}
	n, err := Parse(t.Context(), []byte("---\n# Body\n[[Target]]\n"), Limits{})
	require.NoError(t, err)
	require.Equal(t, "unclosed_frontmatter", n.Diagnostics[0].Code)
	require.Equal(t, "Body", n.Title)
	for _, src := range []string{"---\n---\n", "---\n# comment\n---\n"} {
		n, err := Parse(t.Context(), []byte(src), Limits{})
		require.NoError(t, err)
		require.Equal(t, map[string]any{}, n.Frontmatter.Raw)
	}
	n, err = Parse(t.Context(), []byte("\n---\n# Body\n"), Limits{})
	require.NoError(t, err)
	require.Nil(t, n.Frontmatter)
}

func TestFrontmatterRejectsTrailingYAML(t *testing.T) {
	for _, tc := range []struct {
		name, trailing string
	}{
		{"valid extra document", "--- # next\ntags: x"},
		{"invalid extra document", "--- # next\ntags: [x"},
		{"malformed post-document content", "... # end\nb: x"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			header := "---\ntitle: A\n" + tc.trailing + "\n---\n"
			n, err := Parse(t.Context(), []byte(header+"# Body\n[[Target]]\n"), Limits{})
			require.NoError(t, err)
			require.NotNil(t, n.Frontmatter)
			require.Equal(t, 0, n.Frontmatter.Start)
			require.Equal(t, len(header), n.Frontmatter.End)
			require.Equal(t, len(header), n.BodyStart)
			require.Equal(t, "Body", n.Title, "trailing YAML must invalidate the authored title")
			require.Equal(t, "h1", n.TitleSource)
			require.Nil(t, n.Frontmatter.Raw)
			require.Nil(t, n.Frontmatter.Fields)
			require.Nil(t, n.Frontmatter.Aliases)
			require.Nil(t, n.Frontmatter.Tags)
			require.Equal(t, []Diagnostic{{Code: "invalid_frontmatter", Span: Span{0, len(header)}}}, n.Diagnostics)
			require.Len(t, n.Headings, 1)
			require.Equal(t, "Body", n.Headings[0].Text)
			require.Equal(t, len(header), n.Headings[0].Start)
			require.Equal(t, []string{"Target"}, linkTargets(n.Links))
		})
	}
}

func TestYAMLAliasExpansionIsBounded(t *testing.T) {
	src := "---\na: &a [" + strings.Repeat("x,", 99) + "x]\nb: [" + strings.Repeat("*a,", 99) + "*a]\n---\n# Body\n"
	n, err := Parse(t.Context(), []byte(src), Limits{})
	requireLimit(t, err, "yaml_nodes")
	require.Equal(t, Note{}, n)
}
