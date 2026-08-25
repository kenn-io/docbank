package document

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNormalizeDocumentPreservesEvidenceAndRemovesActiveContent(t *testing.T) {
	assert := assert.New(t)
	markdown := `# Damage report

The carton was **crushed**. [Safe](https://example.test/report?id=42)
and [unsafe](javascript:alert(1)).

![Photo evidence](data:image/png;base64,AAAA)

| Item | Count |
| --- | ---: |
| Carton | 3 |
| Pallet | 1 |

<script>private_executable_marker()</script>
<style>.hidden { display: none }</style>

## Inspection

- First edge
- Second edge

~~~go
package synthetic
~~~
`
	source := SourceDocument{Family: "pdf", UnitKind: "page", Units: []SourceUnit{{
		Index: 0, Markdown: markdown, Header: "Warehouse **A**", Footer: "Page 1",
		Dimensions: UnitDimensions{DPI: 200, Height: 2200, Width: 1700},
	}}}
	policy := testNormalizePolicy(t, 100_000)
	normalized, err := NormalizeDocument(source, policy)
	require.NoError(t, err)
	require.Len(t, normalized.Units, 1)
	unit := normalized.Units[0]
	assert.Contains(unit.Text, "# Damage report")
	assert.Contains(unit.Text, "carton was crushed")
	assert.Contains(unit.Text, "Safe (https://example.test/report?id=42)")
	assert.Contains(unit.Text, "unsafe")
	assert.NotContains(unit.Text, "javascript:")
	assert.Contains(unit.Text, "Photo evidence")
	assert.NotContains(unit.Text, "data:image")
	assert.Contains(unit.Text, "Item | Count")
	assert.Contains(unit.Text, "Carton | 3")
	assert.Contains(unit.Text, "Pallet | 1")
	assert.NotContains(unit.Text, "private_executable_marker")
	assert.NotContains(unit.Text, "display: none")
	assert.Contains(unit.Text, "```go\npackage synthetic\n```")
	assert.Contains(unit.Text, "Warehouse A")
	assert.Contains(unit.Text, "Page 1")
	assert.Equal("Warehouse A", unit.Header)
	assert.Equal("Page 1", unit.Footer)
	assert.Regexp(`^[0-9a-f]{64}$`, unit.Checksum)
	assert.Regexp(`^[0-9a-f]{64}$`, normalized.Checksum)
}

func TestNormalizeDocumentKeepsFrozenTableBoundaries(t *testing.T) {
	normalized, err := NormalizeDocument(SourceDocument{
		Family: "text", UnitKind: "section", Units: []SourceUnit{{
			Index: 0, Markdown: "before<table><tr><td>x</td></tr></table>after",
		}},
	}, testNormalizePolicy(t, 100_000))
	require.NoError(t, err)

	require.Len(t, normalized.Units, 1)
	assert.Equal(t, "before\nx\nafter", normalized.Units[0].Text)
	assert.Equal(t, "a8a611c408751feacca74b4da4d7842bfd0833e8ffe20406073a9b5c38c6a644", normalized.Units[0].Checksum)
	assert.Equal(t, "31a205248e1b22cd02ded0386ad37ba1684ff7afa7c6820411d7f71334106546", normalized.Checksum)
}

func TestNormalizeDocumentRejectsZeroPolicy(t *testing.T) {
	source := SourceDocument{
		Family: "text", UnitKind: "section",
		Units: []SourceUnit{{Index: 0, Markdown: "synthetic evidence"}},
	}

	_, err := NormalizeDocument(source, NormalizePolicy{})
	require.ErrorContains(t, err, "use NewNormalizePolicy")
}

func TestNormalizeAndValidateRejectInvalidDocumentIdentifiers(t *testing.T) {
	policy := testNormalizePolicy(t, 10_000)
	validSource := SourceDocument{
		Family: "text", UnitKind: "unit", Units: []SourceUnit{{Index: 0, Markdown: "evidence"}},
	}
	validNormalized, err := NormalizeDocument(validSource, policy)
	require.NoError(t, err)

	tests := []struct {
		name, family, unitKind, want string
	}{
		{
			name: "family invalid UTF-8", family: string([]byte{0xff}), unitKind: "unit", want: "invalid UTF-8",
		},
		{
			name: "unit kind invalid UTF-8", family: "text", unitKind: string([]byte{0xff}), want: "invalid UTF-8",
		},
		{
			name: "family control character", family: "text\nother", unitKind: "unit", want: "control character",
		},
		{
			name: "unit kind control character", family: "text", unitKind: "unit\tother", want: "control character",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			source := validSource
			source.Family = test.family
			source.UnitKind = test.unitKind
			_, err := NormalizeDocument(source, policy)
			require.ErrorContains(t, err, test.want)

			normalized := validNormalized
			normalized.Family = test.family
			normalized.UnitKind = test.unitKind
			err = ValidateNormalizedDocument(normalized)
			require.ErrorContains(t, err, test.want)
		})
	}
}

func TestNormalizeDocumentChecksumIncludesDocumentTruncation(t *testing.T) {
	policy := testNormalizePolicy(t, 3)
	complete, err := NormalizeDocument(SourceDocument{
		Family: "text", UnitKind: "unit",
		Units: []SourceUnit{{Index: 0, Markdown: "one"}},
	}, policy)
	require.NoError(t, err)
	truncated, err := NormalizeDocument(SourceDocument{
		Family: "text", UnitKind: "unit",
		Units: []SourceUnit{{Index: 0, Markdown: "one"}, {Index: 1, Markdown: "two"}},
	}, policy)
	require.NoError(t, err)

	assert.False(t, complete.Truncated)
	assert.True(t, truncated.Truncated)
	assert.Equal(t, complete.Units, truncated.Units)
	assert.Equal(t, complete.Chunks, truncated.Chunks)
	assert.NotEqual(t, complete.Checksum, truncated.Checksum)
}

func TestNormalizeDocumentPublishesHeaderAndFooterOnlyEvidence(t *testing.T) {
	source := SourceDocument{Family: "pdf", UnitKind: "page", Units: []SourceUnit{{
		Index: 0, Header: "Confidential **shipment**", Footer: "Dock 7",
	}}}

	normalized, err := NormalizeDocument(source, testNormalizePolicy(t, 100_000))
	require.NoError(t, err)
	require.Len(t, normalized.Units, 1)
	assert.Equal(t, "Confidential shipment\n\nDock 7", normalized.Units[0].Text)
	assert.Equal(t, "Confidential shipment", normalized.Units[0].Header)
	assert.Equal(t, "Dock 7", normalized.Units[0].Footer)
	require.Len(t, normalized.Chunks, 1)
	assert.Equal(t, normalized.Units[0].Text, normalized.Chunks[0].Text)
	assert.Equal(t, 0, normalized.Chunks[0].Spans[0].CharStart)
	assert.Equal(t, normalized.Units[0].CharCount, normalized.Chunks[0].Spans[0].CharEnd)
}

func TestNormalizeDocumentChunksHaveExactUnitSpansAndHeadingPaths(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	markdown := "# Alpha\n\n" + strings.Repeat("alpha evidence sentence. ", 8) +
		"\n\n## Beta\n\n" + strings.Repeat("beta evidence sentence. ", 8)
	policy := testNormalizePolicy(t, 10_000)
	policy.maxChunkRunes = 100
	policy.chunkOverlap = 10
	source := SourceDocument{Family: "word", UnitKind: "page", Units: []SourceUnit{{Index: 0, Markdown: markdown}}}

	first, err := NormalizeDocument(source, policy)
	require.NoError(err)
	second, err := NormalizeDocument(source, policy)
	require.NoError(err)
	assert.Equal(first, second)
	require.Greater(len(first.Chunks), 1)

	runes := []rune(first.Units[0].Text)
	for ordinal, chunk := range first.Chunks {
		assert.Equal(ordinal, chunk.Ordinal)
		require.Len(chunk.Spans, 1)
		span := chunk.Spans[0]
		assert.Equal(chunk.Text, string(runes[span.CharStart:span.CharEnd]))
		assert.Equal(utf8.RuneCountInString(chunk.Text), chunk.CharCount)
		assert.Regexp(`^[0-9a-f]{64}$`, chunk.Checksum)
	}
	assert.Equal([]string{"Alpha"}, first.Chunks[0].HeadingPath)
	assert.Contains(first.Chunks[len(first.Chunks)-1].HeadingPath, "Beta")
}

func TestNormalizeDocumentBoundsUnicodeAndRejectsImpossibleUnits(t *testing.T) {
	assert := assert.New(t)
	policy := testNormalizePolicy(t, 6)
	policy.maxUnitChars = 6
	policy.maxChunkRunes = 3
	policy.chunkOverlap = 1
	source := SourceDocument{Family: "text", UnitKind: "section", Units: []SourceUnit{{Index: 0, Markdown: "é日🙂abcdef"}}}
	normalized, err := NormalizeDocument(source, policy)
	require.NoError(t, err)
	assert.Equal("é日🙂abc", normalized.Units[0].Text)
	assert.True(normalized.Units[0].Truncated)
	assert.True(normalized.Truncated)
	assert.True(utf8.ValidString(normalized.Units[0].Text))

	source.Units = append(source.Units, SourceUnit{Index: 3, Markdown: "later"})
	_, err = NormalizeDocument(source, policy)
	require.ErrorContains(t, err, "noncontiguous index")

	source.Units = []SourceUnit{{Index: 0, Markdown: "x", Dimensions: UnitDimensions{Width: -1}}}
	_, err = NormalizeDocument(source, policy)
	require.ErrorContains(t, err, "invalid dimensions")
}

func TestNormalizeDocumentValidatesUnitsAfterCharacterBudget(t *testing.T) {
	policy := testNormalizePolicy(t, 1)
	tests := []struct {
		name    string
		invalid SourceUnit
		want    string
	}{
		{name: "index", invalid: SourceUnit{Index: 3, Markdown: "z"}, want: "noncontiguous index"},
		{name: "dimensions", invalid: SourceUnit{Index: 2, Markdown: "z", Dimensions: UnitDimensions{Width: -1}}, want: "invalid dimensions"},
		{name: "UTF-8", invalid: SourceUnit{Index: 2, Markdown: string([]byte{0xff})}, want: "invalid UTF-8"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			source := SourceDocument{Family: "text", UnitKind: "page", Units: []SourceUnit{
				{Index: 0, Markdown: "x"},
				{Index: 1, Markdown: "y"},
				test.invalid,
			}}

			_, err := NormalizeDocument(source, policy)
			require.ErrorContains(t, err, test.want)
		})
	}
}

func TestNormalizeDocumentRetainsEmptyUnitAtExactCharacterBudget(t *testing.T) {
	dimensions := UnitDimensions{DPI: 200, Height: 1200, Width: 800}
	source := SourceDocument{Family: "pdf", UnitKind: "page", Units: []SourceUnit{
		{Index: 0, Markdown: "x"},
		{Index: 1, Dimensions: dimensions},
	}}

	normalized, err := NormalizeDocument(source, testNormalizePolicy(t, 1))
	require.NoError(t, err)
	assert.False(t, normalized.Truncated)
	require.Len(t, normalized.Units, 2)
	assert.Equal(t, "page:000001", normalized.Units[1].SourceKey)
	assert.Equal(t, dimensions, normalized.Units[1].Dimensions)
	assert.Empty(t, normalized.Units[1].Text)
	assert.False(t, normalized.Units[1].Truncated)
}

func TestNormalizeDocumentCapsChunksWithoutInventingSpans(t *testing.T) {
	assert := assert.New(t)
	policy := testNormalizePolicy(t, 100_000)
	policy.maxChunkRunes = 20
	policy.chunkOverlap = 2
	policy.maxChunks = 2
	source := SourceDocument{Family: "text", UnitKind: "section", Units: []SourceUnit{{
		Index: 0, Markdown: strings.Repeat("bounded evidence ", 20),
	}}}

	normalized, err := NormalizeDocument(source, policy)
	require.NoError(t, err)
	assert.Len(normalized.Chunks, 2)
	assert.True(normalized.Truncated)
	for _, chunk := range normalized.Chunks {
		assert.NotEmpty(chunk.Text)
		assert.Len(chunk.Spans, 1)
	}
}

func TestNormalizeDocumentBoundsHeadingProvenanceToRetainedText(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	policy := testNormalizePolicy(t, 12)
	policy.maxUnitChars = 12
	policy.maxChunkRunes = 12
	policy.chunkOverlap = 2
	source := SourceDocument{Family: "text", UnitKind: "section", Units: []SourceUnit{{
		Index: 0, Markdown: "# retained-heading-content-that-is-cut\n\nbody",
	}}}

	normalized, err := NormalizeDocument(source, policy)
	require.NoError(err)
	require.Len(normalized.Units, 1)
	unit := normalized.Units[0]
	require.Len(unit.HeadingMarks, 1)
	assert.Equal("# retained-h", unit.Text)
	assert.Equal([]string{"retained-h"}, unit.HeadingMarks[0].Path)
	assert.NotContains(unit.HeadingMarks[0].Path[0], "content")
}

func TestNormalizeDocumentPreservesInlineAdjacencyAndTaskState(t *testing.T) {
	assert := assert.New(t)
	policy := testNormalizePolicy(t, 10_000)
	source := SourceDocument{Family: "text", UnitKind: "section", Units: []SourceUnit{{
		Index:    0,
		Markdown: "pre**condition**ed and `identifier`\n\n- [x] shipped\n- [ ] pending",
	}}}

	normalized, err := NormalizeDocument(source, policy)
	require.NoError(t, err)
	text := normalized.Units[0].Text
	assert.Contains(text, "preconditioned")
	assert.NotContains(text, "pre condition ed")
	assert.Contains(text, "- [x] shipped")
	assert.Contains(text, "- [ ] pending")
}

func TestNormalizeDocumentPreservesSoftBreakSeparation(t *testing.T) {
	policy := testNormalizePolicy(t, 10_000)
	source := SourceDocument{Family: "text", UnitKind: "section", Units: []SourceUnit{{
		Index: 0, Markdown: "alpha\nbeta",
	}}}

	normalized, err := NormalizeDocument(source, policy)
	require.NoError(t, err)
	assert.Equal(t, "alpha beta", normalized.Units[0].Text)
}

func TestNormalizeDocumentPreservesUnicodeWhitespaceSeparation(t *testing.T) {
	policy := testNormalizePolicy(t, 10_000)
	source := SourceDocument{Family: "text", UnitKind: "page", Units: []SourceUnit{{
		Index: 0, Markdown: "alpha\u2028beta\u2029gamma\u0085delta\fepsilon\u00a0zeta",
	}}}

	normalized, err := NormalizeDocument(source, policy)
	require.NoError(t, err)
	assert.Equal(t, "alpha beta gamma delta epsilon zeta", normalized.Units[0].Text)
}

func TestNormalizeDocumentPreservesInertRawHTMLText(t *testing.T) {
	policy := testNormalizePolicy(t, 10_000)
	tests := []struct {
		name     string
		markdown string
		want     string
	}{
		{name: "line break", markdown: "alpha<br>beta", want: "alpha\nbeta"},
		{name: "block container", markdown: "<div>alpha <strong>beta</strong></div>", want: "alpha beta"},
		{name: "content after self-closing SVG child", markdown: "<svg><path/></svg>after", want: "after"},
		{name: "content after void SVG child", markdown: "<svg><br></svg>after", want: "after"},
		{name: "self-closing script syntax", markdown: "<script/>payload</script>safe", want: "safe"},
		{name: "self-closing style syntax", markdown: "<style/>payload</style>safe", want: "safe"},
		{name: "checkbox after soft break", markdown: "alpha\n<input type=\"checkbox\">beta", want: "alpha [ ] beta"},
		{name: "adjacent definition blocks", markdown: "<dl><dt>Term</dt><dd>Definition</dd></dl>", want: "Term\nDefinition"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			source := SourceDocument{Family: "text", UnitKind: "page", Units: []SourceUnit{{
				Index: 0, Markdown: test.markdown,
			}}}

			normalized, err := NormalizeDocument(source, policy)
			require.NoError(t, err)
			assert.Equal(t, test.want, normalized.Units[0].Text)
		})
	}
}

func TestNormalizeDocumentHandlesDeepMalformedExcludedHTML(t *testing.T) {
	policy := testNormalizePolicy(t, 100_000)
	markdown := "<svg>" + strings.Repeat("<g>", 4_096) +
		strings.Repeat("</missing>", 4_096) + "</svg>after"
	source := SourceDocument{Family: "text", UnitKind: "page", Units: []SourceUnit{{
		Index: 0, Markdown: markdown,
	}}}

	normalized, err := NormalizeDocument(source, policy)
	require.NoError(t, err)
	assert.Equal(t, "after", normalized.Units[0].Text)
}

func TestNormalizeDocumentPreservesSpaceBeforeInlineCode(t *testing.T) {
	policy := testNormalizePolicy(t, 10_000)
	source := SourceDocument{Family: "text", UnitKind: "page", Units: []SourceUnit{{
		Index: 0, Markdown: "Use `name` now.",
	}}}

	normalized, err := NormalizeDocument(source, policy)
	require.NoError(t, err)
	assert.Equal(t, "Use `name` now.", normalized.Units[0].Text)
}

func TestNormalizeDocumentKeepsPunctuationAfterInlineElements(t *testing.T) {
	policy := testNormalizePolicy(t, 10_000)
	tests := []struct {
		name     string
		markdown string
		want     string
	}{
		{name: "code", markdown: "`value `.", want: "`value `."},
		{name: "link", markdown: "[value ](https://example.com).", want: "value (https://example.com)."},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			source := SourceDocument{Family: "text", UnitKind: "page", Units: []SourceUnit{{
				Index: 0, Markdown: test.markdown,
			}}}

			normalized, err := NormalizeDocument(source, policy)
			require.NoError(t, err)
			assert.Equal(t, test.want, normalized.Units[0].Text)
		})
	}
}

func TestNormalizeDocumentBoundsHeadingPathsByLevel(t *testing.T) {
	tests := []struct {
		name     string
		markdown string
		limit    int
		want     []HeadingMark
	}{
		{
			name: "truncated child title retains parent", markdown: "# Parent\n\n## Child title\n\nbody", limit: 11,
			want: []HeadingMark{{CharOffset: 0, Path: []string{"Parent"}}, {CharOffset: 9, Path: []string{"Parent"}}},
		},
		{
			name: "empty top-level heading resets path", markdown: "# Parent\n\n## Child\n\n#\n\nafter", limit: 10_000,
			want: []HeadingMark{
				{CharOffset: 0, Path: []string{"Parent"}},
				{CharOffset: 9, Path: []string{"Parent", "Child"}},
				{CharOffset: 18, Path: []string{}},
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			policy := testNormalizePolicy(t, test.limit)
			policy.maxUnitChars = test.limit
			source := SourceDocument{Family: "text", UnitKind: "page", Units: []SourceUnit{{
				Index: 0, Markdown: test.markdown,
			}}}

			normalized, err := NormalizeDocument(source, policy)
			require.NoError(t, err)
			assert.Equal(t, test.want, normalized.Units[0].HeadingMarks)
		})
	}
}

func TestValidateNormalizedDocumentRejectsEmptyHeadingPathElements(t *testing.T) {
	policy := testNormalizePolicy(t, 10_000)
	normalized, err := NormalizeDocument(SourceDocument{
		Family: "text", UnitKind: "page", Units: []SourceUnit{{Index: 0, Markdown: "# Parent\n\nbody"}},
	}, policy)
	require.NoError(t, err)
	normalized.Units[0].HeadingMarks[0].Path = []string{"Parent", ""}

	err = ValidateNormalizedDocument(normalized)
	assert.ErrorContains(t, err, "invalid heading marks")
}

func TestValidateNormalizedDocumentRejectsChangedUnitProvenance(t *testing.T) {
	policy := testNormalizePolicy(t, 10_000)
	normalized, err := NormalizeDocument(SourceDocument{
		Family: "pdf", UnitKind: "page", Units: []SourceUnit{{
			Index: 0, Markdown: "# Parent\n\nbody",
			Dimensions: UnitDimensions{DPI: 200, Height: 1200, Width: 800},
		}},
	}, policy)
	require.NoError(t, err)

	tests := []struct {
		name   string
		mutate func(*NormalizedDocument)
	}{
		{
			name: "dimensions",
			mutate: func(changed *NormalizedDocument) {
				changed.Units[0].Dimensions.Width++
			},
		},
		{
			name: "heading mark",
			mutate: func(changed *NormalizedDocument) {
				changed.Units[0].HeadingMarks[0].Path[0] = "Changed"
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			changed := normalized
			changed.Units = append([]NormalizedUnit(nil), normalized.Units...)
			changed.Units[0].HeadingMarks = append([]HeadingMark(nil), normalized.Units[0].HeadingMarks...)
			changed.Units[0].HeadingMarks[0].Path = append([]string(nil), normalized.Units[0].HeadingMarks[0].Path...)
			test.mutate(&changed)

			err := ValidateNormalizedDocument(changed)
			assert.ErrorContains(t, err, "checksum is invalid")
		})
	}
}

func TestNormalizeDocumentPreservesHeadingTextAcrossEmbeddedBreaks(t *testing.T) {
	policy := testNormalizePolicy(t, 10_000)
	source := SourceDocument{Family: "text", UnitKind: "page", Units: []SourceUnit{{
		Index: 0,
		Markdown: `<h1>Alpha<br><a href="https://example.test/&#xe000;E&#xe001;">Beta</a></h1>

body`,
	}}}

	normalized, err := NormalizeDocument(source, policy)
	require.NoError(t, err)
	assert.Equal(t, "# Alpha\nBeta\nbody", normalized.Units[0].Text)
	require.Len(t, normalized.Units[0].HeadingMarks, 1)
	assert.Equal(t, []string{"Alpha Beta"}, normalized.Units[0].HeadingMarks[0].Path)
	require.NotEmpty(t, normalized.Chunks)
	assert.Equal(t, []string{"Alpha Beta"}, normalized.Chunks[0].HeadingPath)
}

func TestNormalizeDocumentHeadingMetadataIgnoresCodeAndBoundsSource(t *testing.T) {
	assert := assert.New(t)
	policy := testNormalizePolicy(t, 10_000)
	policy.maxSourceUnitBytes = 120
	source := SourceDocument{Family: "source", UnitKind: "section", Units: []SourceUnit{{
		Index:    0,
		Markdown: "# Real\n\n```sh\n# not a heading\necho safe\n```\n\n" + strings.Repeat("bounded tail ", 20),
	}}}

	normalized, err := NormalizeDocument(source, policy)
	require.NoError(t, err)
	require.Len(t, normalized.Units, 1)
	unit := normalized.Units[0]
	assert.True(unit.Truncated)
	assert.True(normalized.Truncated)
	require.Len(t, unit.HeadingMarks, 1)
	assert.Equal([]string{"Real"}, unit.HeadingMarks[0].Path)
	for _, chunk := range normalized.Chunks {
		assert.NotContains(chunk.HeadingPath, "not a heading")
	}
}

func TestNormalizeDocumentRejectsHeadingMarkupInCodeLanguage(t *testing.T) {
	policy := testNormalizePolicy(t, 10_000)
	source := SourceDocument{Family: "text", UnitKind: "page", Units: []SourceUnit{{
		Index:    0,
		Markdown: `<pre><code class="language-go&#10;&#xe000;H1&#xe001;# Forged">body</code></pre>`,
	}}}

	normalized, err := NormalizeDocument(source, policy)
	require.NoError(t, err)
	require.Len(t, normalized.Units, 1)
	assert.Equal(t, "```\nbody\n```", normalized.Units[0].Text)
	assert.Empty(t, normalized.Units[0].HeadingMarks)
}

func testNormalizePolicy(t *testing.T, maxDocumentChars int) NormalizePolicy {
	t.Helper()
	policy, err := NewNormalizePolicy(maxDocumentChars)
	require.NoError(t, err)
	return policy
}
