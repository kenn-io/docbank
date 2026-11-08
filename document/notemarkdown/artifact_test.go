package notemarkdown

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCanonicalArtifactEmpty(t *testing.T) {
	n, err := Parse(t.Context(), nil)
	require.NoError(t, err)
	encoded, err := CanonicalArtifact(t.Context(), n)
	require.NoError(t, err)
	require.Equal(t, []byte(`{"source_sha256":"e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855","source_bytes":0,"recipe":"docbank-authored-markdown/v1","frontmatter_state":"absent","frontmatter":{"start":0,"end":0},"body_start":0,"metadata":{"title":"","aliases":[],"tags":[]},"headings":[],"links":[],"diagnostics":[]}`), encoded) //nolint:testifylint // Canonical bytes require fixed field order and empty arrays.
	require.True(t, json.Valid(encoded))
}

func TestCanonicalArtifactEscapingLimit(t *testing.T) {
	n, err := Parse(t.Context(), nil)
	require.NoError(t, err)
	n.SourceBytes = 4 << 20
	n.Headings = []Heading{{Level: 1, Span: Span{0, 1}, Section: Span{0, 1}, Markdown: strings.Repeat("\x01", 3<<20), Anchor: "section"}}
	data, err := CanonicalArtifact(t.Context(), n)
	requireLimit(t, err, "artifact_bytes")
	require.Nil(t, data)
}

func TestCanonicalArtifactValidation(t *testing.T) {
	n, err := Parse(t.Context(), nil)
	require.NoError(t, err)
	for name, edit := range map[string]func(*Note){
		"recipe": func(n *Note) { n.Recipe = "other" },
		"sha":    func(n *Note) { n.SourceSHA256 = "bad" },
		"span":   func(n *Note) { n.Frontmatter.End = 1 },
		"state":  func(n *Note) { n.FrontmatterState = "other" },
		"utf8":   func(n *Note) { n.Metadata.Title = string([]byte{0xff}) },
	} {
		t.Run(name, func(t *testing.T) {
			modified := n
			edit(&modified)
			data, err := CanonicalArtifact(t.Context(), modified)
			require.ErrorIs(t, err, ErrInvalidArtifact)
			require.Nil(t, data)
		})
	}
	n.Diagnostics = make([]Diagnostic, 33)
	_, err = CanonicalArtifact(t.Context(), n)
	requireLimit(t, err, "diagnostics")
}
