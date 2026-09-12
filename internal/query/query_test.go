package query

import (
	_ "embed"
	"encoding/json/v2"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

//go:embed testdata/identity-vectors.json
var identityFixtureJSON []byte

type identityFixture struct {
	Format               string           `json:"format"`
	Queries              []identityVector `json:"queries"`
	HighlightSets        []identityVector `json:"highlight_sets"`
	InvalidQueries       []invalidVector  `json:"invalid_queries"`
	InvalidHighlightSets []invalidVector  `json:"invalid_highlight_sets"`
	Bounds               boundaryFixture  `json:"bounds"`
	MediaCases           []mediaCase      `json:"media_cases"`
}

type identityVector struct {
	Name          string `json:"name"`
	InputJSON     string `json:"input_json"`
	CanonicalUTF8 string `json:"canonical_utf8"`
	Fingerprint   string `json:"fingerprint"`
}

type invalidVector struct {
	Name     string `json:"name"`
	JSONText string `json:"json_text"`
}

type boundaryFixture struct {
	OversizedQueryPathASCIIBytes    int `json:"oversized_query_path_ascii_bytes"`
	HighlightMaxTerms               int `json:"highlight_max_terms"`
	HighlightMaxTermScalars         int `json:"highlight_max_term_scalars"`
	OversizedHighlightAstralScalars int `json:"oversized_highlight_astral_scalars"`
}

type mediaCase struct {
	Name      string `json:"name"`
	MediaType string `json:"media_type"`
	Filename  string `json:"filename"`
	Family    string `json:"family"`
}

func loadIdentityFixture(t *testing.T) identityFixture {
	t.Helper()
	var fixture identityFixture
	require.NoError(t, json.Unmarshal(identityFixtureJSON, &fixture, json.RejectUnknownMembers(true)))
	require.Equal(t, "docbank-query-identity-v1", fixture.Format)
	return fixture
}

func TestQueryIdentityVectors(t *testing.T) {
	for _, vector := range loadIdentityFixture(t).Queries {
		t.Run(vector.Name, func(t *testing.T) {
			value, err := Parse([]byte(vector.InputJSON))
			require.NoError(t, err)
			encoded, fingerprint, err := CanonicalWithFingerprint(value)
			require.NoError(t, err)
			assert.Equal(t, vector.CanonicalUTF8, string(encoded))
			assert.Equal(t, vector.Fingerprint, fingerprint)
		})
	}
}

func TestQueryRejectsInvalidSharedInputs(t *testing.T) {
	for _, testCase := range loadIdentityFixture(t).InvalidQueries {
		t.Run(testCase.Name, func(t *testing.T) {
			_, err := Parse([]byte(testCase.JSONText))
			require.Error(t, err)
		})
	}
}

func TestQueryNormalizesOptionalNullsAndFlags(t *testing.T) {
	value, err := Parse([]byte(`{"filters":{"paths":null,"modified_after":null,"size_min":null,"no_tags":true,"exclude_tag_ids":["00000000-0000-4000-8000-000000000001"]}}`))
	require.NoError(t, err)
	encoded, err := Canonical(value)
	require.NoError(t, err)
	assert.Equal(t, `{"filters":{"exclude_tag_ids":["00000000-0000-4000-8000-000000000001"],"no_tags":true},"mode":"lexical","sort":{"direction":"asc","field":"name"},"syntax":"simple","text":"","v":1}`, string(encoded)) //nolint:testifylint // Exact bytes are the contract.

	_, err = Parse([]byte(`{"filters":{"no_tags":true,"tag_ids":["00000000-0000-4000-8000-000000000001"]}}`))
	require.Error(t, err)
}

func TestQueryAcceptsEscapedAstralUnicode(t *testing.T) {
	value, err := Parse([]byte(`{"text":"\ud83d\ude00"}`))
	require.NoError(t, err)
	assert.Equal(t, "😀", value.Text)
}

func TestQueryRejectsInvalidFilterValues(t *testing.T) {
	tests := map[string]string{
		"relative path":             `{"filters":{"paths":["relative"]}}`,
		"dot path segment":          `{"filters":{"paths":["/one/../two"]}}`,
		"root trailing slash":       `{"filters":{"paths":["/one/"]}}`,
		"wrong UUID version":        `{"filters":{"tag_ids":["00000000-0000-3000-8000-000000000001"]}}`,
		"uppercase UUID":            `{"filters":{"tag_ids":["00000000-0000-4000-8000-00000000000A"]}}`,
		"unknown media family":      `{"filters":{"media_families":["future"]}}`,
		"parameterized MIME":        `{"filters":{"mime_types":["text/plain; charset=utf-8"]}}`,
		"wildcard MIME":             `{"filters":{"mime_types":["text/*"]}}`,
		"uppercase MIME":            `{"filters":{"mime_types":["Text/Plain"]}}`,
		"leading-dot extension":     `{"filters":{"extensions":[".pdf"]}}`,
		"unknown coverage":          `{"filters":{"text_coverage":["pending"]}}`,
		"zero version":              `{"v":0}`,
		"empty syntax":              `{"syntax":""}`,
		"empty sort field":          `{"sort":{"field":""}}`,
		"empty timestamp":           `{"filters":{"modified_after":""}}`,
		"inverted times":            `{"filters":{"modified_after":"2026-09-10T00:00:00Z","modified_before":"2026-09-09T00:00:00Z"}}`,
		"equal times":               `{"filters":{"modified_after":"2026-09-09T00:00:00Z","modified_before":"2026-09-09T00:00:00Z"}}`,
		"inverted sizes":            `{"filters":{"size_min":2,"size_max":1}}`,
		"negative size":             `{"filters":{"size_min":-1}}`,
		"too many supplied entries": `{"filters":{"extensions":[` + strings.Repeat(`"txt",`, 32) + `"txt"]}}`,
	}
	for name, raw := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := Parse([]byte(raw))
			require.Error(t, err)
		})
	}
}

func TestQueryEnforcesInputTextAndDepthBounds(t *testing.T) {
	fixture := loadIdentityFixture(t)
	_, err := Parse([]byte(strings.Repeat(" ", 128<<10) + `{}`))
	require.Error(t, err)

	_, err = Parse([]byte(`{"text":"` + strings.Repeat("😀", 8193) + `"}`))
	require.Error(t, err)

	_, err = Parse([]byte(`{"filters":{"paths":["/` + strings.Repeat("x", fixture.Bounds.OversizedQueryPathASCIIBytes) + `"]}}`))
	require.Error(t, err)

	_, err = Parse([]byte(`{"filters":{"paths":[[[[[[[[[[[[[[[[["/deep"]]]]]]]]]]]]]]]]]}}`))
	require.Error(t, err)
}

func TestCanonicalRejectsInvalidProgrammaticQuery(t *testing.T) {
	_, err := Canonical(Query{V: 1, Text: string([]byte{0xff})})
	require.Error(t, err)
}
