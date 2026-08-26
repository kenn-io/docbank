package document

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	extensionast "github.com/yuin/goldmark/extension/ast"
	goldmarkhtml "github.com/yuin/goldmark/renderer/html"
	"github.com/yuin/goldmark/text"
)

func TestBuildRenditionV1(t *testing.T) {
	evidence := normalizeRenditionEvidence(t, SourceEvidenceV1{
		ContractVersion: SourceEvidenceContractV1,
		Completeness:    EvidenceComplete,
		Family:          "pdf",
		UnitKind:        EvidenceUnitPage,
		Units: []SourceEvidenceUnitV1{
			{
				Order: 0,
				Locator: SourceEvidenceLocatorV1{
					Kind: EvidenceLocatorPage, IndexOrigin: EvidenceIndexOriginOne, Start: 1, End: 1,
				},
				Text: "# Damage report\n\nThe carton was **crushed**. [Safe](https://example.test/report?id=42) and [unsafe](javascript:alert(1)).\n\n![Photo evidence](data:image/png;base64,AAAA)\n\n| Item | Count |\n| --- | ---: |\n| Carton | 3 |\n| Pallet | 1 |\n\n<script>private_executable_marker()</script>\n<style>.hidden { display: none }</style>\n\n## Inspection\n\n- First edge\n- Second edge\n\n~~~go\npackage synthetic\n~~~",
			},
			{
				Order: 1,
				Locator: SourceEvidenceLocatorV1{
					Kind: EvidenceLocatorPage, IndexOrigin: EvidenceIndexOriginOne, Start: 2, End: 2,
				},
				Text: "# Follow-up\n\n[Allowed](https://example.test/follow-up)\n\n<iframe src=\"https://example.test/active\">active_markup()</iframe>",
			},
		},
	})
	policy, err := NewRenditionPolicy(testNormalizePolicy(t, 100_000))
	require.NoError(t, err)

	rendered, err := BuildRenditionV1(evidence, policy)
	require.NoError(t, err)
	again, err := BuildRenditionV1(evidence, policy)
	require.NoError(t, err)
	assert.Equal(t, rendered, again)

	want, err := os.ReadFile("testdata/rendition-v1.golden.md")
	require.NoError(t, err)
	assert.Equal(t, string(want), string(rendered.Markdown))
	assert.Equal(t, RenditionContractV1, rendered.ContractVersion)
	assert.Equal(t, evidence.Checksum, rendered.EvidenceChecksum)
	assert.Equal(t, EvidenceComplete, rendered.Completeness)
	assert.Empty(t, rendered.Warnings)
	assert.NotEmpty(t, rendered.Checksum)
	assert.NotEmpty(t, rendered.MarkdownChecksum)
	require.Len(t, rendered.Units, 2)
	require.Len(t, rendered.LexicalSegments, 2)

	for index, segment := range rendered.LexicalSegments {
		unit := rendered.Units[index]
		assert.Equal(t, unit.ID, segment.UnitID)
		assert.Equal(t, index, segment.Order)
		assert.Equal(t, string([]rune(unit.Text)[segment.CharStart:segment.CharEnd]), segment.Text)
		assert.Equal(t, utf8.RuneCountInString(segment.Text), segment.CharEnd-segment.CharStart)
		assert.NotEmpty(t, segment.Checksum)
	}

	markdown := string(rendered.Markdown)
	assert.Contains(t, markdown, "# Damage report")
	assert.Contains(t, markdown, "- First edge")
	assert.Contains(t, markdown, "| Item | Count |\n| --- | --- |")
	assert.Contains(t, markdown, "```go\npackage synthetic\n```")
	assert.Contains(t, markdown, "\n\n---\n\n")
	assert.Contains(t, markdown, "[Safe](<https://example.test/report?id=42>)")
	assert.NotContains(t, markdown, "private_executable_marker")
	assert.NotContains(t, markdown, "display: none")
	assert.NotContains(t, markdown, "data:image")
	assert.NotContains(t, markdown, "javascript:")
	assert.NotContains(t, markdown, "active_markup")
	assert.NotContains(t, markdown, "<iframe")
	assert.Contains(t, renderUnsafeMarkdown(t, rendered.Markdown), "<table>")
}

func TestNormalizedRenditionUnitChecksumIncludesEveryLocatorField(t *testing.T) {
	evidenceUnit := NormalizedEvidenceUnitV1{
		ID: "evidence_unit_test",
		Locator: EvidenceLocatorV1{
			Kind: EvidenceLocatorLine, IndexOrigin: EvidenceIndexOriginOne,
			Start: 1, End: 2, Name: "range-a",
		},
	}
	wantDifferentFrom := normalizedRenditionUnit(evidenceUnit, "text").Checksum

	for _, test := range []struct {
		name   string
		mutate func(*EvidenceLocatorV1)
	}{
		{name: "kind", mutate: func(locator *EvidenceLocatorV1) { locator.Kind = EvidenceLocatorRecord }},
		{name: "index origin", mutate: func(locator *EvidenceLocatorV1) { locator.IndexOrigin = EvidenceIndexOriginZero }},
		{name: "start", mutate: func(locator *EvidenceLocatorV1) { locator.Start = 2 }},
		{name: "end", mutate: func(locator *EvidenceLocatorV1) { locator.End = 3 }},
		{name: "name", mutate: func(locator *EvidenceLocatorV1) { locator.Name = "range-b" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			changed := evidenceUnit
			test.mutate(&changed.Locator)

			assert.NotEqual(t, wantDifferentFrom, normalizedRenditionUnit(changed, "text").Checksum)
		})
	}
}

func TestBuildRenditionV1BoundsLexicalSegmentsByRune(t *testing.T) {
	evidence := normalizeRenditionEvidence(t, SourceEvidenceV1{
		ContractVersion: SourceEvidenceContractV1,
		Completeness:    EvidenceComplete,
		Family:          "pdf",
		UnitKind:        EvidenceUnitPage,
		Units: []SourceEvidenceUnitV1{{Order: 0, Text: "ab😀cdéfgh",
			Locator: SourceEvidenceLocatorV1{
				Kind: EvidenceLocatorPage, IndexOrigin: EvidenceIndexOriginOne, Start: 1, End: 1,
			}}},
	})
	normalization := testNormalizePolicy(t, 100)
	normalization.maxChunkRunes = 4_000
	normalization.chunkOverlap = 0
	policy, err := NewRenditionPolicy(normalization, 4)
	require.NoError(t, err)

	rendered, err := BuildRenditionV1(evidence, policy)
	require.NoError(t, err)
	require.Len(t, rendered.LexicalSegments, 3)
	assert.Equal(t, []int{0, 4, 8}, []int{
		rendered.LexicalSegments[0].CharStart,
		rendered.LexicalSegments[1].CharStart,
		rendered.LexicalSegments[2].CharStart,
	})
	assert.Equal(t, []int{4, 8, 9}, []int{
		rendered.LexicalSegments[0].CharEnd,
		rendered.LexicalSegments[1].CharEnd,
		rendered.LexicalSegments[2].CharEnd,
	})
	var joined strings.Builder
	for _, segment := range rendered.LexicalSegments {
		assert.LessOrEqual(t, utf8.RuneCountInString(segment.Text), 4)
		joined.WriteString(segment.Text)
	}
	assert.Equal(t, rendered.Units[0].Text, joined.String())
}

func TestBuildRenditionV1DegradesGenericUnitsWithoutInventingProvenance(t *testing.T) {
	evidence := normalizeRenditionEvidence(t, SourceEvidenceV1{
		ContractVersion: SourceEvidenceContractV1,
		Completeness:    EvidenceDegradedProvenance,
		Family:          "text",
		UnitKind:        EvidenceUnitGeneric,
		Omissions: []SourceEvidenceOmissionV1{{
			Field: "provenance", Kind: EvidenceOmissionField, Reason: "source has no natural units",
		}},
		Units: []SourceEvidenceUnitV1{{
			Order:   0,
			Locator: SourceEvidenceLocatorV1{Kind: EvidenceLocatorGeneric, IndexOrigin: EvidenceIndexOriginNone},
			Text:    "plain **synthetic** text",
		}},
	})
	policy, err := NewRenditionPolicy(testNormalizePolicy(t, 100_000))
	require.NoError(t, err)

	rendered, err := BuildRenditionV1(evidence, policy)
	require.NoError(t, err)

	assert.Equal(t, "plain synthetic text\n", string(rendered.Markdown))
	require.Len(t, rendered.Warnings, 1)
	assert.Equal(t, "degraded_provenance", rendered.Warnings[0].Code)
	assert.NotContains(t, string(rendered.Markdown), "page")
}

func normalizeRenditionEvidence(t *testing.T, source SourceEvidenceV1) NormalizedEvidenceV1 {
	t.Helper()
	policy, err := NewEvidencePolicy(100_000)
	require.NoError(t, err)
	evidence, err := NormalizeEvidenceV1(source, policy)
	require.NoError(t, err)
	return evidence
}

func TestBuildRenditionV1RejectsInvalidEvidenceAndPolicy(t *testing.T) {
	_, err := BuildRenditionV1(NormalizedEvidenceV1{}, RenditionPolicy{})
	require.Error(t, err)
	assert.True(t, strings.Contains(err.Error(), "rendition") || strings.Contains(err.Error(), "evidence"))

	policy, err := NewRenditionPolicy(testNormalizePolicy(t, 100_000))
	require.NoError(t, err)
	_, err = BuildRenditionV1(NormalizedEvidenceV1{}, policy)
	require.ErrorContains(t, err, "validate rendition evidence")
}

func TestBuildRenditionV1DropsSelfClosingActiveHTMLWithoutDroppingFollowingText(t *testing.T) {
	evidence := normalizeRenditionEvidence(t, SourceEvidenceV1{
		ContractVersion: SourceEvidenceContractV1,
		Completeness:    EvidenceComplete,
		Family:          "text",
		UnitKind:        EvidenceUnitSection,
		Units: []SourceEvidenceUnitV1{{
			Order:   0,
			Locator: SourceEvidenceLocatorV1{Kind: EvidenceLocatorSection, IndexOrigin: EvidenceIndexOriginNone},
			Text:    "before <input type=\"text\" value=\"provider control\"> after",
		}},
	})
	policy, err := NewRenditionPolicy(testNormalizePolicy(t, 100_000))
	require.NoError(t, err)

	rendered, err := BuildRenditionV1(evidence, policy)
	require.NoError(t, err)
	assert.Equal(t, "before after\n", string(rendered.Markdown))
}

func TestBuildRenditionV1DropsMatchedInlineActiveHTMLBodies(t *testing.T) {
	for _, tag := range []string{"script", "style", "iframe", "textarea"} {
		t.Run(tag, func(t *testing.T) {
			source := "before <" + tag + ">private_marker()</" + tag + "> after"
			rendered := buildRenditionMarkdownForTest(t, source, 100_000)

			assert.Equal(t, "before after\n", string(rendered.Markdown))
			assert.NotContains(t, rendered.Units[0].Text, "private_marker")
		})
	}
}

func TestBuildRenditionV1DropsBlockActiveHTMLBodiesAcrossBlankLines(t *testing.T) {
	rendered := buildRenditionMarkdownForTest(t, "before\n\n<iframe>\n\nprivate_marker\n\n</iframe>", 100_000)

	assert.Equal(t, "before\n", string(rendered.Markdown))
	assert.NotContains(t, rendered.Units[0].Text, "private_marker")
}

func TestBuildRenditionV1DropsGeneratedTextInsideActiveHTML(t *testing.T) {
	source := "before <iframe>hidden\n\n![private_image](https://example.test/image)\n\n- [x] private_task\n\nsuppressed</iframe> after"
	rendered := buildRenditionMarkdownForTest(t, source, 100_000)
	html := renderUnsafeMarkdown(t, rendered.Markdown)

	assert.NotContains(t, html, "private_image")
	assert.NotContains(t, html, "[x]")
}

func TestBuildRenditionV1DoesNotEmitStructuresOpenedInsideActiveHTML(t *testing.T) {
	for _, test := range []struct {
		name   string
		source string
	}{
		{name: "list", source: "<iframe>\n\n- hidden\n\n</iframe>"},
		{name: "table", source: "<iframe>\n\n| h |\n| --- |\n| hidden |\n\n</iframe>"},
		{name: "code", source: "<iframe>\n\n```text\nhidden\n```\n\n</iframe>"},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := buildRenditionMarkdownResultForTest(t, test.source, 100_000)

			require.ErrorContains(t, err, "evidence produced no readable text")
		})
	}
}

func TestBuildRenditionV1TreatsSelfClosingActiveSyntaxAsAnOpener(t *testing.T) {
	for _, tag := range []string{"script", "iframe"} {
		t.Run(tag, func(t *testing.T) {
			source := "before <" + tag + "/>private_marker()</" + tag + "> after"
			rendered := buildRenditionMarkdownForTest(t, source, 100_000)

			assert.Equal(t, "before after\n", string(rendered.Markdown))
		})
	}
}

func TestBuildRenditionV1DropsMalformedNestedActiveHTMLBodies(t *testing.T) {
	rendered := buildRenditionMarkdownForTest(t, "before <iframe>secret<textarea>x</iframe> after", 100_000)

	assert.Equal(t, "before after\n", string(rendered.Markdown))
	assert.NotContains(t, rendered.Units[0].Text, "secret")
	assert.NotContains(t, rendered.Units[0].Text, "x")
}

func TestBuildRenditionV1PreservesGeneratedStructureCrossedByActiveHTML(t *testing.T) {
	for _, test := range []struct {
		name   string
		source string
		want   string
	}{
		{name: "list", source: "- kept <iframe>hidden\n\nsuppressed</iframe> after", want: "- kept\n\nafter\n"},
		{name: "table", source: "| value |\n| --- |\n| kept <iframe>hidden |\n\nsuppressed</iframe> after", want: "| value |\n| --- |\n| kept |\nafter\n"},
		{name: "heading", source: "# kept <iframe>hidden\n\nsuppressed</iframe> after", want: "# kept\nafter\n"},
		{name: "link", source: "[kept <iframe>hidden](https://example.test)\n\nsuppressed</iframe> after", want: "[kept](<https://example.test>)\n\nafter\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			rendered := buildRenditionMarkdownForTest(t, test.source, 100_000)

			assert.Equal(t, test.want, string(rendered.Markdown))
			assert.NotContains(t, rendered.Units[0].Text, "hidden")
			assert.NotContains(t, rendered.Units[0].Text, "suppressed")
		})
	}
}

func TestBuildRenditionV1PreservesParserGeneratedTaskCheckboxes(t *testing.T) {
	evidence := normalizeRenditionEvidence(t, SourceEvidenceV1{
		ContractVersion: SourceEvidenceContractV1,
		Completeness:    EvidenceComplete,
		Family:          "text",
		UnitKind:        EvidenceUnitSection,
		Units: []SourceEvidenceUnitV1{{
			Order:   0,
			Locator: SourceEvidenceLocatorV1{Kind: EvidenceLocatorSection, IndexOrigin: EvidenceIndexOriginNone},
			Text:    "- [x] shipped\n- [ ] pending",
		}},
	})
	policy, err := NewRenditionPolicy(testNormalizePolicy(t, 100_000))
	require.NoError(t, err)

	rendered, err := BuildRenditionV1(evidence, policy)
	require.NoError(t, err)
	assert.Equal(t, "- \\[x\\] shipped\n- \\[ \\] pending\n", string(rendered.Markdown))
}

func TestBuildRenditionV1DropsProviderSuppliedCheckboxControls(t *testing.T) {
	evidence := normalizeRenditionEvidence(t, SourceEvidenceV1{
		ContractVersion: SourceEvidenceContractV1,
		Completeness:    EvidenceComplete,
		Family:          "text",
		UnitKind:        EvidenceUnitSection,
		Units: []SourceEvidenceUnitV1{{
			Order:   0,
			Locator: SourceEvidenceLocatorV1{Kind: EvidenceLocatorSection, IndexOrigin: EvidenceIndexOriginNone},
			Text:    "alpha <input type=\"checkbox\" checked> beta",
		}},
	})
	policy, err := NewRenditionPolicy(testNormalizePolicy(t, 100_000))
	require.NoError(t, err)

	rendered, err := BuildRenditionV1(evidence, policy)
	require.NoError(t, err)
	assert.Equal(t, "alpha beta\n", string(rendered.Markdown))
}

func TestBuildRenditionV1AppliesDocumentCharacterLimit(t *testing.T) {
	evidence := normalizeRenditionEvidence(t, SourceEvidenceV1{
		ContractVersion: SourceEvidenceContractV1,
		Completeness:    EvidenceComplete,
		Family:          "text",
		UnitKind:        EvidenceUnitSection,
		Units: []SourceEvidenceUnitV1{{
			Order:   0,
			Locator: SourceEvidenceLocatorV1{Kind: EvidenceLocatorSection, IndexOrigin: EvidenceIndexOriginNone},
			Text:    "é日🙂abcdef",
		}},
	})
	normalization := testNormalizePolicy(t, 6)
	normalization.maxUnitChars = 100
	policy, err := NewRenditionPolicy(normalization)
	require.NoError(t, err)

	rendered, err := BuildRenditionV1(evidence, policy)
	require.NoError(t, err)
	assert.Equal(t, "é日🙂abc\n", string(rendered.Markdown))
	require.Len(t, rendered.Warnings, 1)
	assert.Equal(t, "truncated", rendered.Warnings[0].Code)
}

func TestBuildRenditionV1SerializesDecodedTextAsInertMarkdown(t *testing.T) {
	evidence := normalizeRenditionEvidence(t, SourceEvidenceV1{
		ContractVersion: SourceEvidenceContractV1,
		Completeness:    EvidenceComplete,
		Family:          "text",
		UnitKind:        EvidenceUnitSection,
		Units: []SourceEvidenceUnitV1{{
			Order:   0,
			Locator: SourceEvidenceLocatorV1{Kind: EvidenceLocatorSection, IndexOrigin: EvidenceIndexOriginNone},
			Text: "\\[escaped](javascript:alert(1))\n\n" +
				"&lt;iframe src=\"https://example.test/active\"&gt;active_markup()&lt;/iframe&gt;\n\n" +
				"~~~go\nfirst\n```\n[escaped_code_fence](javascript:alert(1))\n~~~",
		}},
	})
	policy, err := NewRenditionPolicy(testNormalizePolicy(t, 100_000))
	require.NoError(t, err)

	rendered, err := BuildRenditionV1(evidence, policy)
	require.NoError(t, err)
	html := renderUnsafeMarkdown(t, rendered.Markdown)

	assert.NotContains(t, html, `href="javascript:`)
	assert.NotContains(t, html, "<iframe")
	assert.Contains(t, html, "escaped")
	assert.Contains(t, html, "active_markup()")
	assert.Contains(t, string(rendered.Markdown), "````go")
	assert.Contains(t, string(rendered.Markdown), "[escaped_code_fence](javascript:alert(1))")
}

func TestBuildRenditionV1WarnsWhenSourceByteLimitTruncates(t *testing.T) {
	evidence := normalizeRenditionEvidence(t, SourceEvidenceV1{
		ContractVersion: SourceEvidenceContractV1,
		Completeness:    EvidenceComplete,
		Family:          "text",
		UnitKind:        EvidenceUnitSection,
		Units: []SourceEvidenceUnitV1{{
			Order:   0,
			Locator: SourceEvidenceLocatorV1{Kind: EvidenceLocatorSection, IndexOrigin: EvidenceIndexOriginNone},
			Text:    "abcdef",
		}},
	})
	normalization := testNormalizePolicy(t, 100_000)
	normalization.maxSourceUnitBytes = 4
	policy, err := NewRenditionPolicy(normalization)
	require.NoError(t, err)

	rendered, err := BuildRenditionV1(evidence, policy)
	require.NoError(t, err)
	assert.Equal(t, "abcd\n", string(rendered.Markdown))
	require.Len(t, rendered.Warnings, 1)
	assert.Equal(t, "truncated", rendered.Warnings[0].Code)
}

func TestBuildRenditionV1PadsAsymmetricInlineCodeBackticks(t *testing.T) {
	evidence := normalizeRenditionEvidence(t, SourceEvidenceV1{
		ContractVersion: SourceEvidenceContractV1,
		Completeness:    EvidenceComplete,
		Family:          "text",
		UnitKind:        EvidenceUnitSection,
		Units: []SourceEvidenceUnitV1{{
			Order:   0,
			Locator: SourceEvidenceLocatorV1{Kind: EvidenceLocatorSection, IndexOrigin: EvidenceIndexOriginNone},
			Text: "`` `<iframe src=\"https://example.test/active\">active</iframe> " +
				"[unsafe](javascript:alert(1))``` ``",
		}},
	})
	policy, err := NewRenditionPolicy(testNormalizePolicy(t, 100_000))
	require.NoError(t, err)

	rendered, err := BuildRenditionV1(evidence, policy)
	require.NoError(t, err)
	html := renderUnsafeMarkdown(t, rendered.Markdown)

	assert.NotContains(t, html, "<iframe")
	assert.NotContains(t, html, `href="javascript:`)
	assert.Contains(t, html, "active")
}

func TestBuildRenditionV1RejectsStructuralCharactersInSafeLinkDestinations(t *testing.T) {
	evidence := normalizeRenditionEvidence(t, SourceEvidenceV1{
		ContractVersion: SourceEvidenceContractV1,
		Completeness:    EvidenceComplete,
		Family:          "text",
		UnitKind:        EvidenceUnitSection,
		Units: []SourceEvidenceUnitV1{{
			Order:   0,
			Locator: SourceEvidenceLocatorV1{Kind: EvidenceLocatorSection, IndexOrigin: EvidenceIndexOriginNone},
			Text:    `<a href="https://example.test/?q=&lt;iframe&gt;">safe</a>`,
		}},
	})
	policy, err := NewRenditionPolicy(testNormalizePolicy(t, 100_000))
	require.NoError(t, err)

	rendered, err := BuildRenditionV1(evidence, policy)
	require.NoError(t, err)
	html := renderUnsafeMarkdown(t, rendered.Markdown)

	assert.NotContains(t, string(rendered.Markdown), "https://example.test/?q=")
	assert.NotContains(t, html, "<iframe")
	assert.NotContains(t, html, `href="https://example.test`)
	assert.Contains(t, html, "safe")
}

func TestNormalizeDocumentKeepsFrozenStructuralLinkText(t *testing.T) {
	normalized, err := NormalizeDocument(SourceDocument{
		Family: "text", UnitKind: "section", Units: []SourceUnit{{
			Index: 0, Markdown: `<a href="https://example.test/?q=&lt;iframe&gt;">safe</a>`,
		}},
	}, testNormalizePolicy(t, 100_000))
	require.NoError(t, err)
	assert.Equal(t, "safe (https://example.test/?q=<iframe>)", normalized.Units[0].Text)
}

func TestBuildRenditionV1TruncatesStructuralMarkdownWithoutExposingCode(t *testing.T) {
	evidence := normalizeRenditionEvidence(t, SourceEvidenceV1{
		ContractVersion: SourceEvidenceContractV1,
		Completeness:    EvidenceComplete,
		Family:          "text",
		UnitKind:        EvidenceUnitSection,
		Units: []SourceEvidenceUnitV1{{
			Order:   0,
			Locator: SourceEvidenceLocatorV1{Kind: EvidenceLocatorSection, IndexOrigin: EvidenceIndexOriginNone},
			Text: "prefix `inline <iframe src=\"https://example.test/active\"> " +
				"[unsafe](javascript:alert(1))` suffix\n\n~~~text\n" +
				"<iframe src=\"https://example.test/block\">\n[unsafe](javascript:alert(1))\n~~~",
		}},
	})
	for limit := 1; limit < 110; limit++ {
		t.Run("unit-"+strconv.Itoa(limit), func(t *testing.T) {
			normalization := testNormalizePolicy(t, 100_000)
			normalization.maxUnitChars = limit
			assertTruncatedRenditionIsInert(t, evidence, normalization)
		})
		t.Run("document-"+strconv.Itoa(limit), func(t *testing.T) {
			normalization := testNormalizePolicy(t, limit)
			normalization.maxUnitChars = 100_000
			assertTruncatedRenditionIsInert(t, evidence, normalization)
		})
	}
}

func TestBuildRenditionV1KeepsCodeAndTablesInTheirOriginalContexts(t *testing.T) {
	evidence := normalizeRenditionEvidence(t, SourceEvidenceV1{
		ContractVersion: SourceEvidenceContractV1,
		Completeness:    EvidenceComplete,
		Family:          "text",
		UnitKind:        EvidenceUnitSection,
		Units: []SourceEvidenceUnitV1{{
			Order:   0,
			Locator: SourceEvidenceLocatorV1{Kind: EvidenceLocatorSection, IndexOrigin: EvidenceIndexOriginNone},
			Text: "~~~text\na | b\n~~~\n\noutside `a | b`\n\n" +
				"| Name | Code |\n| --- | --- |\n| left | `a | b` |",
		}},
	})
	policy, err := NewRenditionPolicy(testNormalizePolicy(t, 100_000))
	require.NoError(t, err)

	rendered, err := BuildRenditionV1(evidence, policy)
	require.NoError(t, err)
	html := renderUnsafeMarkdown(t, rendered.Markdown)

	assert.Contains(t, html, "<pre><code")
	assert.Contains(t, html, "a | b")
	assert.Contains(t, html, "<table>")
	assert.Equal(t, 2, strings.Count(html, "<th>"))
	assert.Equal(t, 2, strings.Count(html, "<td>"))
}

func TestBuildRenditionV1KeepsFencedCodeInsideItsTableCell(t *testing.T) {
	evidence := normalizeRenditionEvidence(t, SourceEvidenceV1{
		ContractVersion: SourceEvidenceContractV1,
		Completeness:    EvidenceComplete,
		Family:          "text",
		UnitKind:        EvidenceUnitSection,
		Units: []SourceEvidenceUnitV1{{
			Order:   0,
			Locator: SourceEvidenceLocatorV1{Kind: EvidenceLocatorSection, IndexOrigin: EvidenceIndexOriginNone},
			Text:    `<table><tr><th>Name</th><th>Code</th></tr><tr><td>left</td><td><pre><code>a | b</code></pre></td></tr></table>`,
		}},
	})
	policy, err := NewRenditionPolicy(testNormalizePolicy(t, 100_000))
	require.NoError(t, err)

	rendered, err := BuildRenditionV1(evidence, policy)
	require.NoError(t, err)
	html := renderUnsafeMarkdown(t, rendered.Markdown)

	assert.Equal(t, 1, strings.Count(html, "<table>"))
	assert.Equal(t, 2, strings.Count(html, "<th>"))
	assert.Equal(t, 2, strings.Count(html, "<td>"))
	assert.Contains(t, html, "<code>a | b</code>")
}

func TestBuildRenditionV1EncodesSafeLinksInEveryMarkdownContext(t *testing.T) {
	link := "http://127.0.0.1:8080/a*(b)_c|d?x=(y)*_"
	local := "http://localhost:8080/a*(b)_c|d?x=(y)*_"
	tableLink := strings.ReplaceAll(link, "|", "&#124;")
	evidence := normalizeRenditionEvidence(t, SourceEvidenceV1{
		ContractVersion: SourceEvidenceContractV1,
		Completeness:    EvidenceComplete,
		Family:          "text",
		UnitKind:        EvidenceUnitSection,
		Units: []SourceEvidenceUnitV1{{
			Order:   0,
			Locator: SourceEvidenceLocatorV1{Kind: EvidenceLocatorSection, IndexOrigin: EvidenceIndexOriginNone},
			Text: `<a href="` + link + `">IP</a> <a href="` + local + `">local</a>

| Link | Value |
| --- | --- |
| <a href="` + tableLink + `">cell</a> | safe |`,
		}},
	})
	policy, err := NewRenditionPolicy(testNormalizePolicy(t, 100_000))
	require.NoError(t, err)

	rendered, err := BuildRenditionV1(evidence, policy)
	require.NoError(t, err)
	html := renderUnsafeMarkdown(t, rendered.Markdown)

	assert.Contains(t, string(rendered.Markdown), "%2A")
	assert.Contains(t, string(rendered.Markdown), "%28")
	assert.Contains(t, string(rendered.Markdown), "%5F")
	assert.Contains(t, string(rendered.Markdown), "%7C")
	assert.Contains(t, html, `href="http://127.0.0.1:8080/a%2A%28b%29%5Fc%7Cd?x=%28y%29%2A%5F"`)
	assert.Contains(t, html, `href="http://localhost:8080/a%2A%28b%29%5Fc%7Cd?x=%28y%29%2A%5F"`)
	assert.Contains(t, html, "<table>")
	assert.Equal(t, 3, strings.Count(html, "<a href="))
}

func TestBuildRenditionV1KeepsNestedLinkCodeInertAtEveryLimit(t *testing.T) {
	evidence := normalizeRenditionEvidence(t, SourceEvidenceV1{
		ContractVersion: SourceEvidenceContractV1,
		Completeness:    EvidenceComplete,
		Family:          "text",
		UnitKind:        EvidenceUnitSection,
		Units: []SourceEvidenceUnitV1{{
			Order:   0,
			Locator: SourceEvidenceLocatorV1{Kind: EvidenceLocatorSection, IndexOrigin: EvidenceIndexOriginNone},
			Text: `prefix <a href="https://example.test/safe"><pre><code>&lt;iframe src="https://example.test/active"&gt;` +
				`[unsafe](javascript:alert(1))&lt;/iframe&gt;</code></pre></a>` + strings.Repeat(" tail", 40),
		}},
	})
	for limit := 1; limit < 160; limit++ {
		t.Run("unit-"+strconv.Itoa(limit), func(t *testing.T) {
			normalization := testNormalizePolicy(t, 100_000)
			normalization.maxUnitChars = limit
			assertNestedLinkCodeIsInert(t, evidence, normalization)
		})
		t.Run("document-"+strconv.Itoa(limit), func(t *testing.T) {
			normalization := testNormalizePolicy(t, limit)
			normalization.maxUnitChars = 100_000
			assertNestedLinkCodeIsInert(t, evidence, normalization)
		})
	}
}

func TestBuildRenditionV1KeepsProviderTextFromCreatingMarkdownBlocks(t *testing.T) {
	evidence := normalizeRenditionEvidence(t, SourceEvidenceV1{
		ContractVersion: SourceEvidenceContractV1,
		Completeness:    EvidenceComplete,
		Family:          "text",
		UnitKind:        EvidenceUnitSection,
		Units: []SourceEvidenceUnitV1{{
			Order:   0,
			Locator: SourceEvidenceLocatorV1{Kind: EvidenceLocatorSection, IndexOrigin: EvidenceIndexOriginNone},
			Text: "<p>&#45; entity item</p><p>\\- escaped item</p><p>&#42;&#42;&#42;</p>" +
				"<p>&#49;&#46; entity ordered</p><p>\\1\\. escaped ordered</p>" +
				"<p>&gt; entity quote</p><p>\\> escaped quote</p>" +
				"<p>&#35; entity heading</p><p>\\# escaped heading</p>" +
				"<p>&#96;&#96;&#96; entity fence</p><p>\\`\\`\\` escaped fence</p>",
		}},
	})
	policy, err := NewRenditionPolicy(testNormalizePolicy(t, 100_000))
	require.NoError(t, err)

	rendered, err := BuildRenditionV1(evidence, policy)
	require.NoError(t, err)
	html := renderUnsafeMarkdown(t, rendered.Markdown)

	assert.NotContains(t, html, "<ul>")
	assert.NotContains(t, html, "<ol>")
	assert.NotContains(t, html, "<hr>")
	assert.NotContains(t, html, "<blockquote>")
	assert.NotContains(t, html, "<h1>")
	assert.NotContains(t, html, "<pre><code>")
	assert.Contains(t, html, "entity item")
	assert.Contains(t, html, "entity fence")
}

func TestBuildRenditionV1PreservesEscapedPipeInsideTableInlineCode(t *testing.T) {
	evidence := normalizeRenditionEvidence(t, SourceEvidenceV1{
		ContractVersion: SourceEvidenceContractV1,
		Completeness:    EvidenceComplete,
		Family:          "text",
		UnitKind:        EvidenceUnitSection,
		Units: []SourceEvidenceUnitV1{{
			Order:   0,
			Locator: SourceEvidenceLocatorV1{Kind: EvidenceLocatorSection, IndexOrigin: EvidenceIndexOriginNone},
			Text:    "| Name | Code |\n| --- | --- |\n| left | `a \\| b` |",
		}},
	})
	policy, err := NewRenditionPolicy(testNormalizePolicy(t, 100_000))
	require.NoError(t, err)

	rendered, err := BuildRenditionV1(evidence, policy)
	require.NoError(t, err)
	html := renderUnsafeMarkdown(t, rendered.Markdown)

	assert.Equal(t, 1, strings.Count(html, "<table>"))
	assert.Equal(t, 2, strings.Count(html, "<th>"))
	assert.Equal(t, 2, strings.Count(html, "<td>"))
	assert.Contains(t, html, "<td><code>a | b</code></td>")
}

func TestBuildRenditionV1KeepsExactFitBudgetsAcrossBlocksAndUnits(t *testing.T) {
	evidence := normalizeRenditionEvidence(t, SourceEvidenceV1{
		ContractVersion: SourceEvidenceContractV1,
		Completeness:    EvidenceComplete,
		Family:          "text",
		UnitKind:        EvidenceUnitSection,
		Units: []SourceEvidenceUnitV1{
			{
				Order:   0,
				Locator: SourceEvidenceLocatorV1{Kind: EvidenceLocatorSection, IndexOrigin: EvidenceIndexOriginNone},
				Text:    "first\n\nsecond",
			},
			{
				Order:   1,
				Locator: SourceEvidenceLocatorV1{Kind: EvidenceLocatorSection, IndexOrigin: EvidenceIndexOriginNone},
				Text:    "later",
			},
		},
	})
	normalization := testNormalizePolicy(t, 5)
	normalization.maxUnitChars = 5
	policy, err := NewRenditionPolicy(normalization)
	require.NoError(t, err)

	rendered, err := BuildRenditionV1(evidence, policy)
	require.NoError(t, err)

	require.Len(t, rendered.Units, 2)
	assert.Equal(t, "first", rendered.Units[0].Text)
	assert.Empty(t, rendered.Units[1].Text)
	assert.Equal(t, "first\n", string(rendered.Markdown))
	assert.LessOrEqual(t, utf8.RuneCountInString(rendered.Units[0].Text), normalization.maxUnitChars)
	assert.LessOrEqual(t, utf8.RuneCountInString(rendered.Units[1].Text), normalization.maxUnitChars)
	assert.LessOrEqual(t, utf8.RuneCountInString(strings.TrimSuffix(string(rendered.Markdown), "\n")), normalization.maxDocumentChars)
	assert.Contains(t, rendered.Warnings, RenditionWarningV1{Code: "truncated"})
}

func TestBuildRenditionV1ChargesAggregateSeparators(t *testing.T) {
	evidence := normalizeRenditionEvidence(t, SourceEvidenceV1{ContractVersion: SourceEvidenceContractV1, Completeness: EvidenceComplete, Family: "text", UnitKind: EvidenceUnitSection, Units: []SourceEvidenceUnitV1{
		{Order: 0, Locator: SourceEvidenceLocatorV1{Kind: EvidenceLocatorSection, IndexOrigin: EvidenceIndexOriginNone}, Text: "one"},
		{Order: 1, Locator: SourceEvidenceLocatorV1{Kind: EvidenceLocatorSection, IndexOrigin: EvidenceIndexOriginNone}, Text: "two"},
	}})
	for limit := 1; limit < 16; limit++ {
		policy, err := NewRenditionPolicy(testNormalizePolicy(t, limit))
		require.NoError(t, err)
		rendered, err := BuildRenditionV1(evidence, policy)
		require.NoError(t, err)
		assert.LessOrEqual(t, utf8.RuneCountInString(strings.TrimSuffix(string(rendered.Markdown), "\n")), limit)
		if limit < 13 {
			assert.Contains(t, rendered.Warnings, RenditionWarningV1{Code: "truncated"})
		}
	}
}

func TestBuildRenditionV1FinalizesTruncatedHTMLContexts(t *testing.T) {
	for _, source := range []string{`<a href="javascript:alert(1)">label`, `<code>code`, `<pre><code>block`, `<table><tr><td>cell`} {
		evidence := normalizeRenditionEvidence(t, SourceEvidenceV1{ContractVersion: SourceEvidenceContractV1, Completeness: EvidenceComplete, Family: "text", UnitKind: EvidenceUnitSection, Units: []SourceEvidenceUnitV1{{Order: 0, Locator: SourceEvidenceLocatorV1{Kind: EvidenceLocatorSection, IndexOrigin: EvidenceIndexOriginNone}, Text: source}}})
		for limit := 1; limit < len(source); limit++ {
			normalization := testNormalizePolicy(t, 10_000)
			normalization.maxSourceUnitBytes = limit
			policy, err := NewRenditionPolicy(normalization)
			require.NoError(t, err)
			rendered, err := BuildRenditionV1(evidence, policy)
			if err == nil {
				html := renderUnsafeMarkdown(t, rendered.Markdown)
				assert.NotContains(t, html, `href="javascript:`)
			}
		}
	}
}

func TestBuildRenditionV1PreservesTypedLists(t *testing.T) {
	evidence := normalizeRenditionEvidence(t, SourceEvidenceV1{ContractVersion: SourceEvidenceContractV1, Completeness: EvidenceComplete, Family: "text", UnitKind: EvidenceUnitSection, Units: []SourceEvidenceUnitV1{{Order: 0, Locator: SourceEvidenceLocatorV1{Kind: EvidenceLocatorSection, IndexOrigin: EvidenceIndexOriginNone}, Text: `<ol start="3"><li>third</li><li><p>loose one</p><p>loose two</p><ul><li>nested</li></ul></li></ol><ul><li>plain</li></ul>`}}})
	policy, err := NewRenditionPolicy(testNormalizePolicy(t, 100_000))
	require.NoError(t, err)
	rendered, err := BuildRenditionV1(evidence, policy)
	require.NoError(t, err)
	html := renderUnsafeMarkdown(t, rendered.Markdown)
	assert.Contains(t, string(rendered.Markdown), "3. third")
	assert.Contains(t, html, "<ol start=\"3\">")
	assert.Contains(t, html, "<ul>")
	assert.Contains(t, html, "loose two")
	assert.Contains(t, html, "nested")
}

func TestBuildRenditionV1KeepsTableListsInsideTheirOuterListItem(t *testing.T) {
	source := `<ul><li>outer<table><tr><td><ul><li>inner</li></ul></td></tr></table>tail</li></ul>`
	rendered := buildRenditionMarkdownForTest(t, source, 100_000)

	assert.Equal(t, "- outer\n\n  | inner |\n  | --- |\n\n  tail\n", string(rendered.Markdown))
	assert.Equal(t, `ul(tight=false)[item[paragraph("outer"),table,paragraph("tail")]]`, renditionMarkdownTopology(t, rendered.Markdown))
}

func TestBuildRenditionV1FlattensNestedTablesIntoOuterCell(t *testing.T) {
	source := `<table><tr><td>before<table><tr><td>inner</td></tr></table>after</td></tr></table>`
	rendered := buildRenditionMarkdownForTest(t, source, 100_000)

	assert.Equal(t, "| before inner after |\n| --- |\n", string(rendered.Markdown))
	assert.Equal(t, "table", renditionMarkdownTopology(t, rendered.Markdown))
}

func TestBuildRenditionV1ClosesImplicitTableCellsAndRows(t *testing.T) {
	source := `<table><tr><td>first<td>second<tr><td>third<td>fourth</table>`
	rendered := buildRenditionMarkdownForTest(t, source, 100_000)

	assert.Equal(t, "| first | second |\n| --- | --- |\n| third | fourth |\n", string(rendered.Markdown))
	assert.Equal(t, "table", renditionMarkdownTopology(t, rendered.Markdown))
}

func TestBuildRenditionV1SeparatesFlattenedTableCellBlocks(t *testing.T) {
	for _, test := range []struct {
		name   string
		source string
	}{
		{name: "paragraphs", source: `<table><tr><td><p>New</p><p>York</p></td></tr></table>`},
		{name: "list items", source: `<table><tr><td><ul><li>New</li><li>York</li></ul></td></tr></table>`},
	} {
		t.Run(test.name, func(t *testing.T) {
			rendered := buildRenditionMarkdownForTest(t, test.source, 100_000)

			assert.Equal(t, "| New York |\n| --- |\n", string(rendered.Markdown))
		})
	}
}

func TestBuildRenditionV1SeparatesIndependentTopLevelBlocks(t *testing.T) {
	for _, test := range []struct {
		name     string
		source   string
		topology string
	}{
		{name: "paragraphs", source: "one\n\ntwo", topology: `paragraph("one") | paragraph("two")`},
		{
			name: "tables",
			source: "| a |\n| --- |\n| one |\n\n" +
				"| b |\n| --- |\n| two |",
			topology: "table | table",
		},
		{
			name: "non-one ordered list after paragraph", source: "before\n\n3. third",
			topology: `paragraph("before") | ol(start=3,tight=true)[item[paragraph("third")]]`,
		},
		{
			name: "paragraph after list", source: "- item\n\nafter",
			topology: `ul(tight=true)[item[paragraph("item")]] | paragraph("after")`,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			rendered := buildRenditionMarkdownForTest(t, test.source, 100_000)

			assert.Equal(t, test.topology, renditionMarkdownTopology(t, rendered.Markdown))
		})
	}
}

func TestBuildRenditionV1PreservesExactListHierarchy(t *testing.T) {
	evidence := normalizeRenditionEvidence(t, SourceEvidenceV1{
		ContractVersion: SourceEvidenceContractV1,
		Completeness:    EvidenceComplete,
		Family:          "text",
		UnitKind:        EvidenceUnitSection,
		Units: []SourceEvidenceUnitV1{{
			Order:   0,
			Locator: SourceEvidenceLocatorV1{Kind: EvidenceLocatorSection, IndexOrigin: EvidenceIndexOriginNone},
			Text: `<ol start="0"><li><p>zero</p><ul><li>nested alpha</li><li>nested beta<ol start="4"><li>deep</li></ol></li></ul><p>after nested</p></li><li>one</li></ol>` +
				`<ul><li>tail</li><li>end</li></ul>`,
		}},
	})
	policy, err := NewRenditionPolicy(testNormalizePolicy(t, 100_000))
	require.NoError(t, err)

	rendered, err := BuildRenditionV1(evidence, policy)
	require.NoError(t, err)

	assert.Equal(t, "0. zero\n   - nested alpha\n   - nested beta\n\n     4. deep\n\n   after nested\n\n1. one\n- tail\n- end\n", string(rendered.Markdown))
	assert.Equal(t, `<ol start="0">
<li>
<p>zero</p>
<ul>
<li>
<p>nested alpha</p>
</li>
<li>
<p>nested beta</p>
<ol start="4">
<li>deep</li>
</ol>
</li>
</ul>
<p>after nested</p>
</li>
<li>
<p>one</p>
</li>
</ol>
<ul>
<li>tail</li>
<li>end</li>
</ul>
`, renderUnsafeMarkdown(t, rendered.Markdown))
}

func TestBuildRenditionV1PreservesBoundedListHierarchy(t *testing.T) {
	evidence := normalizeRenditionEvidence(t, SourceEvidenceV1{
		ContractVersion: SourceEvidenceContractV1,
		Completeness:    EvidenceComplete,
		Family:          "text",
		UnitKind:        EvidenceUnitSection,
		Units: []SourceEvidenceUnitV1{{
			Order:   0,
			Locator: SourceEvidenceLocatorV1{Kind: EvidenceLocatorSection, IndexOrigin: EvidenceIndexOriginNone},
			Text:    `<ol start="0"><li>zero<ul><li>nested alpha</li><li>nested beta</li></ul></li><li>one</li></ol>`,
		}},
	})
	normalization := testNormalizePolicy(t, 36)
	normalization.maxUnitChars = 36
	policy, err := NewRenditionPolicy(normalization)
	require.NoError(t, err)

	rendered, err := BuildRenditionV1(evidence, policy)
	require.NoError(t, err)

	assert.Equal(t, "0. zero\n   - nested alpha\n   - neste\n", string(rendered.Markdown))
	assert.Equal(t, `<ol start="0">
<li>zero
<ul>
<li>nested alpha</li>
<li>neste</li>
</ul>
</li>
</ol>
`, renderUnsafeMarkdown(t, rendered.Markdown))
	assert.Contains(t, rendered.Warnings, RenditionWarningV1{Code: "truncated"})
}

func TestBuildRenditionV1PreservesEveryListTopologyAtFullAndBoundedBudgets(t *testing.T) {
	tests := []struct {
		name            string
		source          string
		fullMarkdown    string
		boundedMarkdown string
		fullTopology    string
		boundedTopology string
	}{
		{
			name:            "loose single paragraph items",
			source:          `<ul><li><p>one</p></li><li><p>two</p></li></ul>`,
			fullMarkdown:    "- one\n\n- two",
			boundedMarkdown: "- one\n\n- tw",
			fullTopology:    `ul(tight=false)[item[paragraph("one")],item[paragraph("two")]]`,
			boundedTopology: `ul(tight=false)[item[paragraph("one")],item[paragraph("tw")]]`,
		},
		{
			name:            "empty item",
			source:          `<ul><li></li><li>filled</li></ul>`,
			fullMarkdown:    "-\n- filled",
			boundedMarkdown: "-\n- fil",
			fullTopology:    `ul(tight=true)[item[],item[paragraph("filled")]]`,
			boundedTopology: `ul(tight=true)[item[],item[paragraph("fil")]]`,
		},
		{
			name:            "nested only item",
			source:          `<ul><li><ul><li>nested</li></ul></li></ul>`,
			fullMarkdown:    "-\n  - nested",
			boundedMarkdown: "-\n  - nes",
			fullTopology:    `ul(tight=true)[item[ul(tight=true)[item[paragraph("nested")]]]]`,
			boundedTopology: `ul(tight=true)[item[ul(tight=true)[item[paragraph("nes")]]]]`,
		},
		{
			name:            "leading code item",
			source:          `<ul><li><pre><code class="language-go">code` + "\n" + `line</code></pre></li><li>tail</li></ul>`,
			fullMarkdown:    "- ```go\n  code\n  line\n  ```\n- tail",
			boundedMarkdown: "- ```go\n  code\n  line\n  ```\n- ta",
			fullTopology:    `ul(tight=true)[item[code("code\nline")],item[paragraph("tail")]]`,
			boundedTopology: `ul(tight=true)[item[code("code\nline")],item[paragraph("ta")]]`,
		},
		{
			name:            "leading table item",
			source:          `<ul><li><table><tr><th>h</th></tr><tr><td>v</td></tr></table></li><li>tail</li></ul>`,
			fullMarkdown:    "- | h |\n  | --- |\n  | v |\n- tail",
			boundedMarkdown: "- | h |\n  | --- |\n  | v |\n- ta",
			fullTopology:    `ul(tight=true)[item[table],item[paragraph("tail")]]`,
			boundedTopology: `ul(tight=true)[item[table],item[paragraph("ta")]]`,
		},
		{
			name:            "adjacent unordered roots",
			source:          `<ul><li>first</li></ul><ul><li>second</li></ul>`,
			fullMarkdown:    "- first\n* second",
			boundedMarkdown: "- first\n* se",
			fullTopology:    `ul(tight=true)[item[paragraph("first")]] | ul(tight=true)[item[paragraph("second")]]`,
			boundedTopology: `ul(tight=true)[item[paragraph("first")]] | ul(tight=true)[item[paragraph("se")]]`,
		},
		{
			name:            "adjacent ordered roots",
			source:          `<ol start="2"><li>two</li></ol><ol start="7"><li>seven</li></ol>`,
			fullMarkdown:    "2. two\n7) seven",
			boundedMarkdown: "2. two\n7) se",
			fullTopology:    `ol(start=2,tight=true)[item[paragraph("two")]] | ol(start=7,tight=true)[item[paragraph("seven")]]`,
			boundedTopology: `ol(start=2,tight=true)[item[paragraph("two")]] | ol(start=7,tight=true)[item[paragraph("se")]]`,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			evidence := normalizeRenditionEvidence(t, SourceEvidenceV1{
				ContractVersion: SourceEvidenceContractV1,
				Completeness:    EvidenceComplete,
				Family:          "text",
				UnitKind:        EvidenceUnitSection,
				Units: []SourceEvidenceUnitV1{{
					Order:   0,
					Locator: SourceEvidenceLocatorV1{Kind: EvidenceLocatorSection, IndexOrigin: EvidenceIndexOriginNone},
					Text:    test.source,
				}},
			})

			t.Run("full", func(t *testing.T) {
				policy, err := NewRenditionPolicy(testNormalizePolicy(t, 100_000))
				require.NoError(t, err)
				rendered, err := BuildRenditionV1(evidence, policy)
				require.NoError(t, err)
				assert.Equal(t, test.fullMarkdown+"\n", string(rendered.Markdown))
				assert.Equal(t, test.fullTopology, renditionMarkdownTopology(t, rendered.Markdown))
			})

			t.Run("bounded", func(t *testing.T) {
				normalization := testNormalizePolicy(t, len(test.boundedMarkdown))
				normalization.maxUnitChars = len(test.boundedMarkdown)
				policy, err := NewRenditionPolicy(normalization)
				require.NoError(t, err)
				rendered, err := BuildRenditionV1(evidence, policy)
				require.NoError(t, err)
				assert.Equal(t, test.boundedMarkdown+"\n", string(rendered.Markdown))
				assert.Equal(t, test.boundedTopology, renditionMarkdownTopology(t, rendered.Markdown))
				assert.Contains(t, rendered.Warnings, RenditionWarningV1{Code: "truncated"})
			})
		})
	}
}

func TestBuildRenditionV1PreservesStructuralOnlyLooseListAtEveryCutoff(t *testing.T) {
	evidence := normalizeRenditionEvidence(t, SourceEvidenceV1{
		ContractVersion: SourceEvidenceContractV1,
		Completeness:    EvidenceComplete,
		Family:          "text",
		UnitKind:        EvidenceUnitSection,
		Units: []SourceEvidenceUnitV1{{
			Order:   0,
			Locator: SourceEvidenceLocatorV1{Kind: EvidenceLocatorSection, IndexOrigin: EvidenceIndexOriginNone},
			Text:    "- # Heading\n\n  ```go\n  code\n  ```",
		}},
	})
	const fullMarkdown = "- # Heading\n\n  ```go\n  code\n  ```\n\n  <!-- -->"

	for limit := 1; limit <= len(fullMarkdown); limit++ {
		t.Run(strconv.Itoa(limit), func(t *testing.T) {
			normalization := testNormalizePolicy(t, limit)
			normalization.maxUnitChars = limit
			policy, err := NewRenditionPolicy(normalization)
			require.NoError(t, err)
			rendered, err := BuildRenditionV1(evidence, policy)
			if limit < 17 {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.LessOrEqual(t, utf8.RuneCountInString(strings.TrimSuffix(string(rendered.Markdown), "\n")), limit)

			heading := "Heading"
			if limit < 23 {
				heading = heading[:limit-16]
			}
			expected := `ul(tight=false)[item[heading(level=1,` + strconv.Quote(heading) + `)]]`
			if limit == len(fullMarkdown) {
				expected = `ul(tight=false)[item[heading(level=1,"Heading"),code("code")]]`
			}
			assert.Equal(t, expected, renditionMarkdownTopology(t, rendered.Markdown))
			assert.Contains(t, string(rendered.Markdown), "<!-- -->")
			if limit < len(fullMarkdown) {
				assert.Contains(t, rendered.Warnings, RenditionWarningV1{Code: "truncated"})
			}
		})
	}
}

func TestBuildRenditionV1PreservesMultiItemLooseListPrefixAtEveryCutoff(t *testing.T) {
	const (
		first        = "aaaaaaaaaaaaaaaaaaaaaaaa"
		second       = "second"
		fullMarkdown = "- " + first + "\n\n- " + second
		looseSuffix  = "\n\n  <!-- -->"
	)
	for limit := 1; limit <= len(fullMarkdown); limit++ {
		t.Run(strconv.Itoa(limit), func(t *testing.T) {
			rendered, err := buildRenditionMarkdownResultForTest(t, fullMarkdown, limit)
			if limit < 15 {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)

			var expectedMarkdown string
			var expectedTopology string
			if limit < 31 {
				retainedFirst := first[:limit-14]
				expectedMarkdown = "- " + retainedFirst + looseSuffix
				expectedTopology = `ul(tight=false)[item[paragraph(` + strconv.Quote(retainedFirst) + `)]]`
			} else {
				retainedSecond := second[:limit-30]
				expectedMarkdown = "- " + first + "\n\n- " + retainedSecond
				expectedTopology = `ul(tight=false)[item[paragraph("` + first + `")],item[paragraph(` + strconv.Quote(retainedSecond) + `)]]`
			}
			assert.Equal(t, expectedMarkdown+"\n", string(rendered.Markdown))
			assert.Equal(t, expectedTopology, renditionMarkdownTopology(t, rendered.Markdown))
			assert.LessOrEqual(t, utf8.RuneCountInString(strings.TrimSuffix(string(rendered.Markdown), "\n")), limit)
			if limit < len(fullMarkdown) {
				assert.Contains(t, rendered.Warnings, RenditionWarningV1{Code: "truncated"})
			}
		})
	}
}

func TestBuildRenditionV1SeparatesNestedSameKindListsAtEveryCutoff(t *testing.T) {
	tests := []struct {
		name         string
		source       string
		fullMarkdown string
		ordered      bool
	}{
		{
			name:         "unordered siblings",
			source:       `<ul><li><ul><li>one</li></ul><ul><li>two</li></ul></li></ul>`,
			fullMarkdown: "-\n  - one\n  * two",
		},
		{
			name:         "ordered siblings including second start one",
			source:       `<ul><li><ol start="4"><li>one</li></ol><ol start="1"><li>two</li></ol></li></ul>`,
			fullMarkdown: "-\n    4. one\n    1) two",
			ordered:      true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			evidence := normalizeRenditionEvidence(t, SourceEvidenceV1{
				ContractVersion: SourceEvidenceContractV1,
				Completeness:    EvidenceComplete,
				Family:          "text",
				UnitKind:        EvidenceUnitSection,
				Units: []SourceEvidenceUnitV1{{
					Order:   0,
					Locator: SourceEvidenceLocatorV1{Kind: EvidenceLocatorSection, IndexOrigin: EvidenceIndexOriginNone},
					Text:    test.source,
				}},
			})

			for limit := 1; limit <= len(test.fullMarkdown); limit++ {
				t.Run(strconv.Itoa(limit), func(t *testing.T) {
					normalization := testNormalizePolicy(t, limit)
					normalization.maxUnitChars = limit
					policy, err := NewRenditionPolicy(normalization)
					require.NoError(t, err)
					rendered, err := BuildRenditionV1(evidence, policy)
					require.NoError(t, err)
					assert.LessOrEqual(t, utf8.RuneCountInString(strings.TrimSuffix(string(rendered.Markdown), "\n")), limit)
					assert.Equal(t, expectedNestedSiblingMarkdown(limit, test.ordered)+"\n", string(rendered.Markdown))
					assert.Equal(t, expectedNestedSiblingTopology(limit, test.ordered), renditionMarkdownTopology(t, rendered.Markdown))
					if limit < len(test.fullMarkdown) {
						assert.Contains(t, rendered.Warnings, RenditionWarningV1{Code: "truncated"})
					}
				})
			}
		})
	}
}

func TestBuildRenditionV1RepresentsOrderedStartsWithoutOverflow(t *testing.T) {
	tests := []struct {
		name            string
		source          string
		fullMarkdown    string
		boundedMarkdown string
		fullTopology    string
		boundedTopology string
	}{
		{
			name:            "nine digit maximum",
			source:          `<ol start="999999999"><li>max</li></ol>`,
			fullMarkdown:    "999999999. max",
			boundedMarkdown: "999999999. m",
			fullTopology:    `ol(start=999999999,tight=true)[item[paragraph("max")]]`,
			boundedTopology: `ol(start=999999999,tight=true)[item[paragraph("m")]]`,
		},
		{
			name:            "ten digit input",
			source:          `<ol start="1000000000"><li>ten</li></ol>`,
			fullMarkdown:    `- 1000000000\. ten`,
			boundedMarkdown: `- 1000000000\. t`,
			fullTopology:    `ul(tight=true)[item[paragraph("1000000000\\. ten")]]`,
			boundedTopology: `ul(tight=true)[item[paragraph("1000000000\\. t")]]`,
		},
		{
			name:            "overflow edge",
			source:          `<ol start="18446744073709551615"><li>edge</li><li>next</li></ol>`,
			fullMarkdown:    "- 18446744073709551615\\. edge\n- 18446744073709551616\\. next",
			boundedMarkdown: "- 18446744073709551615\\. edge\n- 18446744073709551616\\. n",
			fullTopology:    `ul(tight=true)[item[paragraph("18446744073709551615\\. edge")],item[paragraph("18446744073709551616\\. next")]]`,
			boundedTopology: `ul(tight=true)[item[paragraph("18446744073709551615\\. edge")],item[paragraph("18446744073709551616\\. n")]]`,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			evidence := normalizeRenditionEvidence(t, SourceEvidenceV1{
				ContractVersion: SourceEvidenceContractV1,
				Completeness:    EvidenceComplete,
				Family:          "text",
				UnitKind:        EvidenceUnitSection,
				Units: []SourceEvidenceUnitV1{{
					Order:   0,
					Locator: SourceEvidenceLocatorV1{Kind: EvidenceLocatorSection, IndexOrigin: EvidenceIndexOriginNone},
					Text:    test.source,
				}},
			})

			for name, expected := range map[string]struct {
				markdown string
				topology string
			}{
				"full":    {markdown: test.fullMarkdown, topology: test.fullTopology},
				"bounded": {markdown: test.boundedMarkdown, topology: test.boundedTopology},
			} {
				t.Run(name, func(t *testing.T) {
					normalization := testNormalizePolicy(t, len(expected.markdown))
					normalization.maxUnitChars = len(expected.markdown)
					policy, err := NewRenditionPolicy(normalization)
					require.NoError(t, err)
					rendered, err := BuildRenditionV1(evidence, policy)
					require.NoError(t, err)
					assert.Equal(t, expected.markdown+"\n", string(rendered.Markdown))
					assert.Equal(t, expected.topology, renditionMarkdownTopology(t, rendered.Markdown))
				})
			}
		})
	}
}

func TestBuildRenditionV1KeepsRepresentableOrderedPrefixAtEveryCutoff(t *testing.T) {
	evidence := normalizeRenditionEvidence(t, SourceEvidenceV1{
		ContractVersion: SourceEvidenceContractV1,
		Completeness:    EvidenceComplete,
		Family:          "text",
		UnitKind:        EvidenceUnitSection,
		Units: []SourceEvidenceUnitV1{{
			Order:   0,
			Locator: SourceEvidenceLocatorV1{Kind: EvidenceLocatorSection, IndexOrigin: EvidenceIndexOriginNone},
			Text:    `<ol start="999999999"><li>nine</li><li>ten</li></ol>`,
		}},
	})
	const orderedPrefix = "999999999. nine"
	const degraded = "- 999999999\\. nine\n- 1000000000\\. ten"

	for limit := 1; limit <= len(degraded); limit++ {
		t.Run(strconv.Itoa(limit), func(t *testing.T) {
			normalization := testNormalizePolicy(t, limit)
			normalization.maxUnitChars = limit
			policy, err := NewRenditionPolicy(normalization)
			require.NoError(t, err)
			rendered, err := BuildRenditionV1(evidence, policy)
			if limit < 12 {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)

			expectedMarkdown := orderedPrefix
			switch {
			case limit <= len(orderedPrefix):
				expectedMarkdown = orderedPrefix[:limit]
			case limit >= 35:
				expectedMarkdown = degraded[:limit]
			}
			assert.Equal(t, expectedMarkdown+"\n", string(rendered.Markdown))
			assert.Equal(t, expectedOrderedCrossingTopology(limit), renditionMarkdownTopology(t, rendered.Markdown))
			if limit < len(degraded) {
				assert.Contains(t, rendered.Warnings, RenditionWarningV1{Code: "truncated"})
			}
		})
	}
}

func TestBuildRenditionV1CarriesOnlyParserListTightness(t *testing.T) {
	for _, test := range []struct {
		name     string
		source   string
		markdown string
		topology string
	}{
		{
			name:     "real tight Markdown",
			source:   "- one",
			markdown: "- one",
			topology: `ul(tight=true)[item[paragraph("one")]]`,
		},
		{
			name:     "real structural-only loose Markdown",
			source:   "- # Heading\n\n  ```go\n  code\n  ```",
			markdown: "- # Heading\n\n  ```go\n  code\n  ```\n\n  <!-- -->",
			topology: `ul(tight=false)[item[heading(level=1,"Heading"),code("code")]]`,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			rendered := buildRenditionMarkdownForTest(t, test.source, 100_000)
			assert.Equal(t, test.markdown+"\n", string(rendered.Markdown))
			assert.Equal(t, test.topology, renditionMarkdownTopology(t, rendered.Markdown))
		})
	}

	const plain = `<ul><li>one</li></ul>`
	const spoofed = `<ul data-docbank-rendition-tight="false"><li>one</li></ul>`
	for limit := 1; limit <= len("- one\n\n  <!-- -->"); limit++ {
		t.Run("spoof/"+strconv.Itoa(limit), func(t *testing.T) {
			plainRendition, plainErr := buildRenditionMarkdownResultForTest(t, plain, limit)
			spoofedRendition, spoofedErr := buildRenditionMarkdownResultForTest(t, spoofed, limit)
			assert.Equal(t, plainErr != nil, spoofedErr != nil)
			if plainErr != nil || spoofedErr != nil {
				return
			}
			assert.Equal(t, plainRendition.Markdown, spoofedRendition.Markdown)
			assert.Equal(t, plainRendition.Units[0].Text, spoofedRendition.Units[0].Text)
			assert.Equal(t, plainRendition.Warnings, spoofedRendition.Warnings)
			assert.Equal(t, renditionMarkdownTopology(t, plainRendition.Markdown), renditionMarkdownTopology(t, spoofedRendition.Markdown))
		})
	}
}

func TestBuildRenditionV1IsolatesRawHTMLTokenizerStateAtEveryBudget(t *testing.T) {
	tests := []struct {
		name     string
		neutral  string
		poisoned string
		markdown string
		topology string
	}{
		{
			name:     "plaintext before loose Markdown list",
			neutral:  "<div>poison</div>\n\n- # Heading\n\n  ```go\n  code\n  ```",
			poisoned: "<plaintext>poison\n\n- # Heading\n\n  ```go\n  code\n  ```",
			markdown: "poison\n- # Heading\n\n  ```go\n  code\n  ```\n\n  <!-- -->",
			topology: `paragraph("poison") | ul(tight=false)[item[heading(level=1,"Heading"),code("code")]]`,
		},
		{
			name:     "raw text between tight Markdown lists",
			neutral:  "- before\n\n# <span>middle</span>\n\n- after",
			poisoned: "- before\n\n# <xmp>middle\n\n- after",
			markdown: "- before\n# middle\n- after",
			topology: `ul(tight=true)[item[paragraph("before")]] | heading(level=1,"middle") | ul(tight=true)[item[paragraph("after")]]`,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			for limit := 1; limit <= len(test.markdown); limit++ {
				t.Run(strconv.Itoa(limit), func(t *testing.T) {
					neutral, neutralErr := buildRenditionMarkdownResultForTest(t, test.neutral, limit)
					poisoned, poisonedErr := buildRenditionMarkdownResultForTest(t, test.poisoned, limit)
					assert.Equal(t, neutralErr != nil, poisonedErr != nil)
					if neutralErr != nil || poisonedErr != nil {
						return
					}
					assert.Equal(t, neutral.Markdown, poisoned.Markdown)
					assert.Equal(t, neutral.Units[0].Text, poisoned.Units[0].Text)
					assert.Equal(t, neutral.Warnings, poisoned.Warnings)
					assert.NotContains(t, renderUnsafeMarkdown(t, poisoned.Markdown), "<plaintext")
					assert.NotContains(t, renderUnsafeMarkdown(t, poisoned.Markdown), "<xmp")
				})
			}

			rendered := buildRenditionMarkdownForTest(t, test.poisoned, len(test.markdown))
			assert.Equal(t, test.markdown+"\n", string(rendered.Markdown))
			assert.Equal(t, test.topology, renditionMarkdownTopology(t, rendered.Markdown))
		})
	}
}

func TestBuildRenditionV1IsolatesEveryRawHTMLSemanticScopeAtEveryBudget(t *testing.T) {
	inlineMarkdown := "- tight\n# boundary\n- # loose\n\n  ```go\n  code\n  ```\n\n  <!-- -->"
	inlineTopology := `ul(tight=true)[item[paragraph("tight")]] | heading(level=1,"boundary") | ul(tight=false)[item[heading(level=1,"loose"),code("code")]]`
	blockMarkdown := "- tight\n* # loose\n\n  ```go\n  code\n  ```\n\n  <!-- -->"
	blockTopology := `ul(tight=true)[item[paragraph("tight")]] | ul(tight=false)[item[heading(level=1,"loose"),code("code")]]`
	tags := []string{
		"<script>",
		"<textarea>",
		"<pre>",
		"<table>",
		`<a href="https://safe.example/path">`,
		"<code>",
	}
	for _, tag := range tags {
		name := strings.Trim(tag, "</>")
		t.Run("inline "+name, func(t *testing.T) {
			neutral := "- tight\n\n# boundary<span>\n\n- # loose\n\n  ```go\n  code\n  ```"
			poisoned := "- tight\n\n# boundary" + tag + "\n\n- # loose\n\n  ```go\n  code\n  ```"
			assertEquivalentRenditionAtEveryBudget(t, neutral, poisoned, inlineMarkdown, inlineTopology)
		})
		t.Run("block "+name, func(t *testing.T) {
			neutral := "- tight\n\n<div><span>\n\n- # loose\n\n  ```go\n  code\n  ```"
			poisoned := "- tight\n\n<div>" + tag + "\n\n- # loose\n\n  ```go\n  code\n  ```"
			assertEquivalentRenditionAtEveryBudget(t, neutral, poisoned, blockMarkdown, blockTopology)
		})
	}
}

func assertEquivalentRenditionAtEveryBudget(
	t *testing.T,
	neutral string,
	poisoned string,
	wantMarkdown string,
	wantTopology string,
) {
	t.Helper()
	for limit := 1; limit <= utf8.RuneCountInString(wantMarkdown); limit++ {
		neutralRendition, neutralErr := buildRenditionMarkdownResultForTest(t, neutral, limit)
		poisonedRendition, poisonedErr := buildRenditionMarkdownResultForTest(t, poisoned, limit)
		assert.Equal(t, neutralErr != nil, poisonedErr != nil, "limit %d", limit)
		if neutralErr != nil || poisonedErr != nil {
			continue
		}
		assert.Equal(t, neutralRendition.Markdown, poisonedRendition.Markdown, "limit %d", limit)
		assert.Equal(t, neutralRendition.Units[0].Text, poisonedRendition.Units[0].Text, "limit %d", limit)
		assert.Equal(t, neutralRendition.Warnings, poisonedRendition.Warnings, "limit %d", limit)
		unsafe := renderUnsafeMarkdown(t, poisonedRendition.Markdown)
		for _, active := range []string{"<script", "<textarea", "<a href="} {
			assert.NotContains(t, unsafe, active, "limit %d", limit)
		}
	}

	rendered := buildRenditionMarkdownForTest(t, poisoned, utf8.RuneCountInString(wantMarkdown))
	assert.Equal(t, wantMarkdown+"\n", string(rendered.Markdown))
	assert.Equal(t, wantTopology, renditionMarkdownTopology(t, rendered.Markdown))
}

func TestBuildRenditionV1ScalesLinearlyAtNearLimit(t *testing.T) {
	allocatedBytes := func(size int) int64 {
		t.Helper()
		input := strings.Repeat("a", size)
		result := testing.Benchmark(func(b *testing.B) {
			b.Helper()
			b.ReportAllocs()
			for b.Loop() {
				_, _, _, err := sanitizeRenditionMarkdown(input, 1_000, 4_000_000, size)
				if err != nil {
					b.Fatal(err)
				}
			}
		})
		return result.AllocedBytesPerOp()
	}
	smallBytes := allocatedBytes(4_096)
	largeBytes := allocatedBytes(8_192)
	require.Less(t, largeBytes, smallBytes*3, "doubling input must not produce quadratic allocated bytes")

	const size = 900_000
	source := strings.Repeat("a", size)
	evidencePolicy, err := NewEvidencePolicy(size)
	require.NoError(t, err)
	evidence, err := NormalizeEvidenceV1(SourceEvidenceV1{
		ContractVersion: SourceEvidenceContractV1,
		Completeness:    EvidenceComplete,
		Family:          "text",
		UnitKind:        EvidenceUnitSection,
		Units: []SourceEvidenceUnitV1{{
			Order:   0,
			Locator: SourceEvidenceLocatorV1{Kind: EvidenceLocatorSection, IndexOrigin: EvidenceIndexOriginNone},
			Text:    source,
		}},
	}, evidencePolicy)
	require.NoError(t, err)
	normalization := testNormalizePolicy(t, size)
	policy, err := NewRenditionPolicy(normalization)
	require.NoError(t, err)
	rendered, err := BuildRenditionV1(evidence, policy)
	require.NoError(t, err)
	expectedMarkdown := []byte(source + "\n")
	expectedDigest := sha256.Sum256(expectedMarkdown)
	assert.Equal(t, expectedMarkdown, rendered.Markdown)
	assert.Equal(t, source, rendered.Units[0].Text)
	var lexical strings.Builder
	for _, segment := range rendered.LexicalSegments {
		lexical.WriteString(segment.Text)
	}
	assert.Equal(t, source, lexical.String())
	assert.Equal(t, hex.EncodeToString(expectedDigest[:]), rendered.MarkdownChecksum)
}

func TestSerializeRenditionTreeUsesOneBudgetedLinearWalk(t *testing.T) {
	allocatedBytes := func(run func() (string, bool)) int64 {
		t.Helper()
		var value string
		result := testing.Benchmark(func(b *testing.B) {
			b.Helper()
			b.ReportAllocs()
			for b.Loop() {
				value, _ = run()
			}
		})
		require.NotNil(t, []byte(value))
		return result.AllocedBytesPerOp()
	}

	decimalList := func(digits, items int) renditionList {
		list := renditionList{ordered: true, start: strings.Repeat("9", digits), tight: true}
		for range items {
			list.items = append(list.items, renditionListItem{present: true, blocks: []renditionBlock{{
				kind: renditionParagraph, inlines: []renditionInline{{kind: renditionText, text: "x"}},
			}}})
		}
		return list
	}
	smallDecimal := decimalList(2_048, 32)
	largeDecimal := decimalList(4_096, 64)
	smallDecimalBytes := allocatedBytes(func() (string, bool) {
		return serializeRenditionList(smallDecimal, 8, false)
	})
	largeDecimalBytes := allocatedBytes(func() (string, bool) {
		return serializeRenditionList(largeDecimal, 8, false)
	})
	require.Less(t, largeDecimalBytes, smallDecimalBytes*3, "unadmitted decimal ordinals must not be preallocated")
	value, truncated := serializeRenditionList(largeDecimal, 8, false)
	assert.Empty(t, value)
	assert.True(t, truncated)

	firstText := strings.Repeat("a", 8_192)
	firstItem := renditionListItem{present: true, blocks: []renditionBlock{{
		kind: renditionParagraph, inlines: []renditionInline{{kind: renditionText, text: firstText}},
	}}}
	singleRepresentable := renditionList{ordered: true, start: "999999999", tight: true, items: []renditionListItem{firstItem}}
	crossing := singleRepresentable
	crossing.items = append(append([]renditionListItem(nil), crossing.items...), renditionListItem{
		present: true,
		blocks:  []renditionBlock{{kind: renditionParagraph, inlines: []renditionInline{{kind: renditionText, text: "x"}}}},
	})
	wantCrossing := "- 999999999\\. " + firstText + "\n- 1000000000\\. x"
	singleBytes := allocatedBytes(func() (string, bool) {
		return serializeRenditionList(singleRepresentable, len(wantCrossing)-1, false)
	})
	crossingBytes := allocatedBytes(func() (string, bool) {
		return serializeRenditionList(crossing, len(wantCrossing), false)
	})
	require.Less(t, crossingBytes, singleBytes*2, "ordered degradation must not rebuild admitted item subtrees")
	value, truncated = serializeRenditionList(crossing, len(wantCrossing), false)
	assert.Equal(t, wantCrossing, value)
	assert.False(t, truncated)
	evidence := normalizeRenditionEvidence(t, SourceEvidenceV1{
		ContractVersion: SourceEvidenceContractV1,
		Completeness:    EvidenceComplete,
		Family:          "text",
		UnitKind:        EvidenceUnitSection,
		Units: []SourceEvidenceUnitV1{{
			Order:   0,
			Locator: SourceEvidenceLocatorV1{Kind: EvidenceLocatorSection, IndexOrigin: EvidenceIndexOriginNone},
			Text:    `<ol start="999999999"><li>` + firstText + `</li><li>x</li></ol>`,
		}},
	})
	normalization := testNormalizePolicy(t, len(wantCrossing))
	normalization.maxUnitChars = len(wantCrossing)
	policy, err := NewRenditionPolicy(normalization)
	require.NoError(t, err)
	rendered, err := BuildRenditionV1(evidence, policy)
	require.NoError(t, err)
	wantMarkdown := []byte(wantCrossing + "\n")
	wantDigest := sha256.Sum256(wantMarkdown)
	assert.Equal(t, wantMarkdown, rendered.Markdown)
	assert.Equal(t, hex.EncodeToString(wantDigest[:]), rendered.MarkdownChecksum)

	looseTree := func(depth int) renditionList {
		block := renditionBlock{kind: renditionParagraph, inlines: []renditionInline{{kind: renditionText, text: "leaf"}}}
		for range depth - 1 {
			nested := renditionList{tight: false, items: []renditionListItem{{present: true, blocks: []renditionBlock{block}}}}
			block = renditionBlock{kind: renditionListBlock, list: &nested}
		}
		return renditionList{tight: false, items: []renditionListItem{{present: true, blocks: []renditionBlock{block}}}}
	}
	expectedLooseTree := func(depth int) string {
		var output strings.Builder
		for level := range depth {
			if level > 0 {
				output.WriteByte('\n')
			}
			output.WriteString(strings.Repeat(" ", level*2))
			output.WriteByte('-')
			if level == depth-1 {
				output.WriteString(" leaf")
			}
		}
		for level := depth; level > 0; level-- {
			output.WriteString("\n\n")
			output.WriteString(strings.Repeat(" ", level*2))
			output.WriteString("<!-- -->")
		}
		return output.String()
	}
	const smallDepth = 20
	const largeDepth = 40
	smallLoose := looseTree(smallDepth)
	largeLoose := looseTree(largeDepth)
	smallLooseMarkdown := expectedLooseTree(smallDepth)
	largeLooseMarkdown := expectedLooseTree(largeDepth)
	smallLooseBytes := allocatedBytes(func() (string, bool) {
		return serializeRenditionList(smallLoose, len(smallLooseMarkdown), false)
	})
	largeLooseBytes := allocatedBytes(func() (string, bool) {
		return serializeRenditionList(largeLoose, len(largeLooseMarkdown), false)
	})
	require.Less(
		t,
		largeLooseBytes*int64(len(smallLooseMarkdown))*2,
		smallLooseBytes*int64(len(largeLooseMarkdown))*3,
		"allocations per emitted byte must remain linear across fully admitted loose trees",
	)
	for _, test := range []struct {
		name     string
		list     renditionList
		markdown string
	}{{name: "small", list: smallLoose, markdown: smallLooseMarkdown}, {name: "large", list: largeLoose, markdown: largeLooseMarkdown}} {
		t.Run(test.name+" fully admitted", func(t *testing.T) {
			actual, actualTruncated := serializeRenditionList(test.list, len(test.markdown), false)
			require.False(t, actualTruncated)
			assert.Equal(t, test.markdown, actual)
			assert.Contains(t, actual, "leaf")
			expectedDigest := sha256.Sum256([]byte(test.markdown))
			actualDigest := sha256.Sum256([]byte(actual))
			assert.Equal(t, hex.EncodeToString(expectedDigest[:]), hex.EncodeToString(actualDigest[:]))
		})
	}

	nestedLink := func(depth int) []renditionInline {
		inline := renditionInline{kind: renditionText, text: "x"}
		for range depth {
			inline = renditionInline{
				kind: renditionLinkInline, destination: "https://safe.example/", children: []renditionInline{inline},
			}
		}
		return []renditionInline{inline}
	}
	smallLinks := nestedLink(64)
	largeLinks := nestedLink(128)
	smallLinkBytes := allocatedBytes(func() (string, bool) {
		return serializeRenditionInlines(smallLinks, 64, false)
	})
	largeLinkBytes := allocatedBytes(func() (string, bool) {
		return serializeRenditionInlines(largeLinks, 128, false)
	})
	require.Less(t, largeLinkBytes, smallLinkBytes*3, "nested link labels must serialize only once")
}

func TestBuildRenditionV1TruncatesExcessiveInlineNesting(t *testing.T) {
	const depth = 1_024
	source := `<div>` + strings.Repeat(`<a href="https://safe.example/">`, depth) +
		"marker" + strings.Repeat(`</a>`, depth) + `</div>`
	rendered := buildRenditionMarkdownForTest(t, source, 100_000)

	assert.Contains(t, string(rendered.Markdown), "marker")
	assert.Contains(t, rendered.Warnings, RenditionWarningV1{Code: "truncated"})
}

func expectedOrderedCrossingTopology(limit int) string {
	if limit < 35 {
		text := "nine"[:min(limit-11, len("nine"))]
		return `ol(start=999999999,tight=true)[item[paragraph(` + strconv.Quote(text) + `)]]`
	}
	secondText := "ten"[:limit-34]
	second := `paragraph(` + strconv.Quote(`1000000000\.`+" "+secondText) + `)`
	return `ul(tight=true)[item[paragraph("999999999\\. nine")],item[` + second + `]]`
}

func buildRenditionMarkdownForTest(t *testing.T, source string, limit int) RenditionV1 {
	t.Helper()
	rendered, err := buildRenditionMarkdownResultForTest(t, source, limit)
	require.NoError(t, err)
	return rendered
}

func buildRenditionMarkdownResultForTest(t *testing.T, source string, limit int) (RenditionV1, error) {
	t.Helper()
	evidence := normalizeRenditionEvidence(t, SourceEvidenceV1{
		ContractVersion: SourceEvidenceContractV1,
		Completeness:    EvidenceComplete,
		Family:          "text",
		UnitKind:        EvidenceUnitSection,
		Units: []SourceEvidenceUnitV1{{
			Order:   0,
			Locator: SourceEvidenceLocatorV1{Kind: EvidenceLocatorSection, IndexOrigin: EvidenceIndexOriginNone},
			Text:    source,
		}},
	})
	normalization := testNormalizePolicy(t, limit)
	normalization.maxUnitChars = limit
	policy, err := NewRenditionPolicy(normalization)
	require.NoError(t, err)
	return BuildRenditionV1(evidence, policy)
}

func expectedNestedSiblingTopology(limit int, ordered bool) string {
	topology := `ul(tight=true)[item[`
	if ordered {
		if limit >= 10 {
			topology += `ol(start=4,tight=true)[item[`
			one := "one"[:min(limit-9, len("one"))]
			topology += `paragraph(` + strconv.Quote(one) + `)`
			topology += `]]`
		}
		if limit >= 21 {
			topology += `,ol(start=1,tight=true)[item[`
			two := "two"[:limit-20]
			topology += `paragraph(` + strconv.Quote(two) + `)`
			topology += `]]`
		}
	} else {
		if limit >= 7 {
			topology += `ul(tight=true)[item[`
			one := "one"[:min(limit-6, len("one"))]
			topology += `paragraph(` + strconv.Quote(one) + `)`
			topology += `]]`
		}
		if limit >= 15 {
			topology += `,ul(tight=true)[item[`
			two := "two"[:limit-14]
			topology += `paragraph(` + strconv.Quote(two) + `)`
			topology += `]]`
		}
	}
	return topology + `]]`
}

func expectedNestedSiblingMarkdown(limit int, ordered bool) string {
	full := "-\n  - one\n  * two"
	firstStarts := 7
	firstComplete := 9
	secondStarts := 15
	if ordered {
		full = "-\n    4. one\n    1) two"
		firstStarts = 10
		firstComplete = 12
		secondStarts = 21
	}
	if limit < firstStarts {
		return "-"
	}
	if limit > firstComplete && limit < secondStarts {
		return full[:firstComplete]
	}
	return full[:limit]
}

func TestBuildRenditionV1FinalizesOpenTableCellContextsInPlace(t *testing.T) {
	prefix := `<table><tr><td>first</td><td><a href="javascript:alert(1)"><pre><code>retained inert`
	evidence := normalizeRenditionEvidence(t, SourceEvidenceV1{
		ContractVersion: SourceEvidenceContractV1,
		Completeness:    EvidenceComplete,
		Family:          "text",
		UnitKind:        EvidenceUnitSection,
		Units: []SourceEvidenceUnitV1{{
			Order:   0,
			Locator: SourceEvidenceLocatorV1{Kind: EvidenceLocatorSection, IndexOrigin: EvidenceIndexOriginNone},
			Text:    prefix + `</code></pre></a></td></tr></table> discarded`,
		}},
	})
	normalization := testNormalizePolicy(t, 100_000)
	normalization.maxSourceUnitBytes = len(prefix)
	policy, err := NewRenditionPolicy(normalization)
	require.NoError(t, err)

	rendered, err := BuildRenditionV1(evidence, policy)
	require.NoError(t, err)

	assert.Equal(t, "| first | retained inert |\n| --- | --- |\n", string(rendered.Markdown))
	assert.NotContains(t, renderUnsafeMarkdown(t, rendered.Markdown), `href="javascript:`)
	assert.Contains(t, rendered.Warnings, RenditionWarningV1{Code: "truncated"})
}

func TestBuildRenditionV1PrefixTruncatesRejectedLinkLabels(t *testing.T) {
	tests := []struct {
		name   string
		source string
		text   string
	}{
		{name: "text only", source: `<a href="javascript:alert(1)">textonly</a>`, text: "textonly"},
		{name: "code only", source: `<a href="javascript:alert(1)"><code>codeonly</code></a>`, text: "codeonly"},
		{name: "mixed", source: `<a href="javascript:alert(1)">pre<code>code</code>post</a>`, text: "precodepost"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			evidence := normalizeRenditionEvidence(t, SourceEvidenceV1{
				ContractVersion: SourceEvidenceContractV1,
				Completeness:    EvidenceComplete,
				Family:          "text",
				UnitKind:        EvidenceUnitSection,
				Units: []SourceEvidenceUnitV1{{
					Order:   0,
					Locator: SourceEvidenceLocatorV1{Kind: EvidenceLocatorSection, IndexOrigin: EvidenceIndexOriginNone},
					Text:    test.source,
				}},
			})
			for limit := 1; limit <= len(test.text); limit++ {
				t.Run(strconv.Itoa(limit), func(t *testing.T) {
					normalization := testNormalizePolicy(t, limit)
					normalization.maxUnitChars = limit
					policy, err := NewRenditionPolicy(normalization)
					require.NoError(t, err)

					rendered, err := BuildRenditionV1(evidence, policy)
					require.NoError(t, err)
					assert.Equal(t, test.text[:limit]+"\n", string(rendered.Markdown))
					assert.NotContains(t, renderUnsafeMarkdown(t, rendered.Markdown), `href="javascript:`)
				})
			}
		})
	}
}

func TestBuildRenditionV1PrefixTruncatesSafeLinkLabels(t *testing.T) {
	const source = `<a href="https://example.test/safe">a*b</a>`
	const full = `[a\*b](<https://example.test/safe>)`
	for _, test := range []struct {
		name  string
		limit int
		want  string
	}{
		{name: "one rune", limit: 1, want: "a"},
		{name: "before escaped punctuation", limit: 2, want: "a"},
		{name: "escaped punctuation", limit: 3, want: `a\*`},
		{name: "below complete link", limit: len(full) - 1, want: `a\*b`},
		{name: "complete link", limit: len(full), want: full},
	} {
		t.Run(test.name, func(t *testing.T) {
			rendered := buildRenditionMarkdownForTest(t, source, test.limit)
			assert.Equal(t, test.want+"\n", string(rendered.Markdown))
		})
	}
}

func assertTruncatedRenditionIsInert(t *testing.T, evidence NormalizedEvidenceV1, normalization NormalizePolicy) {
	t.Helper()
	policy, err := NewRenditionPolicy(normalization)
	require.NoError(t, err)
	rendered, err := BuildRenditionV1(evidence, policy)
	require.NoError(t, err)
	html := renderUnsafeMarkdown(t, rendered.Markdown)
	assert.NotContains(t, html, "<iframe")
	assert.NotContains(t, html, `href="javascript:`)
	assert.Contains(t, rendered.Warnings, RenditionWarningV1{Code: "truncated"})
}

func assertNestedLinkCodeIsInert(t *testing.T, evidence NormalizedEvidenceV1, normalization NormalizePolicy) {
	t.Helper()
	policy, err := NewRenditionPolicy(normalization)
	require.NoError(t, err)
	rendered, err := BuildRenditionV1(evidence, policy)
	require.NoError(t, err)
	html := renderUnsafeMarkdown(t, rendered.Markdown)
	assert.NotContains(t, html, "<iframe")
	assert.NotContains(t, html, `href="javascript:`)
	assert.NotContains(t, html, `href="https://example.test/safe"`)
	assert.Contains(t, rendered.Warnings, RenditionWarningV1{Code: "truncated"})
}

func renderUnsafeMarkdown(t *testing.T, markdown []byte) string {
	t.Helper()
	parser := goldmark.New(
		goldmark.WithExtensions(extension.GFM),
		goldmark.WithRendererOptions(goldmarkhtml.WithUnsafe()),
	)
	var rendered bytes.Buffer
	require.NoError(t, parser.Convert(markdown, &rendered))
	return rendered.String()
}

func renditionMarkdownTopology(t *testing.T, markdown []byte) string {
	t.Helper()
	parser := goldmark.New(goldmark.WithExtensions(extension.GFM)).Parser()
	document := parser.Parse(text.NewReader(markdown))
	roots := make([]string, 0, document.ChildCount())
	for child := document.FirstChild(); child != nil; child = child.NextSibling() {
		if block, ok := child.(*ast.HTMLBlock); ok && strings.TrimSpace(string(block.Lines().Value(markdown))) == "<!-- -->" {
			continue
		}
		roots = append(roots, renditionMarkdownNodeTopology(child, markdown))
	}
	return strings.Join(roots, " | ")
}

func renditionMarkdownNodeTopology(node ast.Node, source []byte) string {
	switch typed := node.(type) {
	case *ast.List:
		kind := "ul(tight=" + strconv.FormatBool(typed.IsTight) + ")"
		if typed.IsOrdered() {
			kind = "ol(start=" + strconv.Itoa(typed.Start) + ",tight=" + strconv.FormatBool(typed.IsTight) + ")"
		}
		children := renditionMarkdownChildTopologies(typed, source)
		return kind + "[" + strings.Join(children, ",") + "]"
	case *ast.ListItem:
		return "item[" + strings.Join(renditionMarkdownChildTopologies(typed, source), ",") + "]"
	case *ast.Paragraph, *ast.TextBlock:
		return "paragraph(" + strconv.Quote(string(node.Lines().Value(source))) + ")"
	case *ast.Heading:
		return "heading(level=" + strconv.Itoa(typed.Level) + "," + strconv.Quote(string(typed.Lines().Value(source))) + ")"
	case *ast.FencedCodeBlock:
		return "code(" + strconv.Quote(strings.TrimSuffix(string(typed.Lines().Value(source)), "\n")) + ")"
	case *extensionast.Table:
		return "table"
	default:
		return node.Kind().String()
	}
}

func renditionMarkdownChildTopologies(node ast.Node, source []byte) []string {
	children := make([]string, 0, node.ChildCount())
	for child := node.FirstChild(); child != nil; child = child.NextSibling() {
		if block, ok := child.(*ast.HTMLBlock); ok && strings.TrimSpace(string(block.Lines().Value(source))) == "<!-- -->" {
			continue
		}
		children = append(children, renditionMarkdownNodeTopology(child, source))
	}
	return children
}
