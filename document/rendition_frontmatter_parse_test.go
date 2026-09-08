package document

import (
	"bytes"
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestEnvelopeRenditionV1RejectsMalformedProducerInput(t *testing.T) {
	validRendition := func() RenditionV1 {
		body := []byte("alpha\n\nbeta\n")
		result := RenditionV1{
			ContractVersion:  RenditionContractV1,
			Completeness:     EvidenceComplete,
			EvidenceChecksum: frontmatterHash("evidence"),
			Markdown:         body,
			MarkdownChecksum: checksumBytes(body),
			Units: []NormalizedUnitV1{{
				EvidenceUnitID: "unit:000000", Order: 0, Text: string(body),
				Locator: EvidenceLocatorV1{Kind: EvidenceLocatorPage, IndexOrigin: EvidenceIndexOriginOne},
			}},
		}
		result.Checksum = renditionChecksum(result)
		return result
	}

	tests := []struct {
		name   string
		mutate func(*RenditionV1, *RenditionEnvelopeV1)
	}{
		{name: "non-hex source digest", mutate: func(_ *RenditionV1, envelope *RenditionEnvelopeV1) {
			envelope.SourceSHA256 = strings.Repeat("g", 64)
		}},
		{name: "uppercase source digest", mutate: func(_ *RenditionV1, envelope *RenditionEnvelopeV1) {
			envelope.SourceSHA256 = strings.ToUpper(envelope.SourceSHA256)
		}},
		{name: "invalid unit kind", mutate: func(_ *RenditionV1, envelope *RenditionEnvelopeV1) {
			envelope.UnitKind = EvidenceUnitKind("chapter")
		}},
		{name: "invalid completeness", mutate: func(rendition *RenditionV1, _ *RenditionEnvelopeV1) {
			rendition.Completeness = EvidenceCompleteness("unknown")
		}},
		{name: "invalid locator kind", mutate: func(rendition *RenditionV1, _ *RenditionEnvelopeV1) {
			rendition.Units[0].Locator.Kind = EvidenceLocatorKind("chapter")
		}},
		{name: "empty navigation key", mutate: func(rendition *RenditionV1, _ *RenditionEnvelopeV1) {
			rendition.Units[0].EvidenceUnitID = ""
		}},
		{name: "invalid UTF-8 body", mutate: func(rendition *RenditionV1, _ *RenditionEnvelopeV1) {
			rendition.Markdown = []byte{0xff}
			rendition.MarkdownChecksum = checksumBytes(rendition.Markdown)
			rendition.Units[0].Text = string(rendition.Markdown)
		}},
		{name: "empty body", mutate: func(rendition *RenditionV1, _ *RenditionEnvelopeV1) {
			rendition.Markdown = nil
			rendition.MarkdownChecksum = checksumBytes(nil)
			rendition.Units[0].Text = ""
		}},
		{name: "wrong body authority hash", mutate: func(rendition *RenditionV1, _ *RenditionEnvelopeV1) {
			rendition.MarkdownChecksum = frontmatterHash("wrong")
		}},
		{name: "invalid UTF-8 title", mutate: func(_ *RenditionV1, envelope *RenditionEnvelopeV1) {
			envelope.Title = string([]byte{0xff})
		}},
		{name: "invalid UTF-8 navigation key", mutate: func(rendition *RenditionV1, _ *RenditionEnvelopeV1) {
			rendition.Units[0].EvidenceUnitID = string([]byte{0xff})
		}},
		{name: "duplicate navigation key", mutate: func(rendition *RenditionV1, _ *RenditionEnvelopeV1) {
			rendition.Units = []NormalizedUnitV1{
				{EvidenceUnitID: "duplicate", Text: "alpha", Locator: EvidenceLocatorV1{Kind: EvidenceLocatorPage}},
				{EvidenceUnitID: "duplicate", Text: "beta", Locator: EvidenceLocatorV1{Kind: EvidenceLocatorPage}},
			}
		}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			rendition := validRendition()
			envelope := validRenditionEnvelope()
			test.mutate(&rendition, &envelope)
			_, _, err := EnvelopeRenditionV1(rendition, envelope)
			require.Error(t, err)
		})
	}
}

func TestParseRenditionFrontMatterV1RejectsMalformedSemantics(t *testing.T) {
	body := []byte("éx\nbeta\n")
	base := validFrontmatterForBody(body)
	base.Document.UnitCount = 2
	base.Navigation.Entries = []RenditionNavigationEntryV1{
		{Key: "page:000000", Kind: EvidenceLocatorPage, Line: 1, Byte: 0},
		{Key: "page:000001", Kind: EvidenceLocatorPage, Line: 2, Byte: 4},
	}

	tests := []struct {
		name   string
		mutate func(*RenditionFrontMatterV1)
	}{
		{name: "non-hex digest", mutate: func(value *RenditionFrontMatterV1) {
			value.Rendition.BuildID = strings.Repeat("z", 64)
		}},
		{name: "uppercase digest", mutate: func(value *RenditionFrontMatterV1) {
			value.Rendition.BuildID = strings.ToUpper(value.Rendition.BuildID)
		}},
		{name: "invalid completeness", mutate: func(value *RenditionFrontMatterV1) {
			value.Rendition.Completeness = EvidenceCompleteness("unknown")
		}},
		{name: "invalid unit kind", mutate: func(value *RenditionFrontMatterV1) {
			value.Document.UnitKind = EvidenceUnitKind("chapter")
		}},
		{name: "excessive unit count", mutate: func(value *RenditionFrontMatterV1) {
			value.Document.UnitCount = maxEvidenceUnits + 1
			value.Navigation.Complete = false
		}},
		{name: "incomplete bounded navigation", mutate: func(value *RenditionFrontMatterV1) {
			value.Navigation.Complete = false
		}},
		{name: "complete over-limit navigation", mutate: func(value *RenditionFrontMatterV1) {
			value.Document.UnitCount = maxRenditionNavigationEntries + 1
		}},
		{name: "invalid locator kind", mutate: func(value *RenditionFrontMatterV1) {
			value.Navigation.Entries[0].Kind = EvidenceLocatorKind("chapter")
		}},
		{name: "wrong body hash", mutate: func(value *RenditionFrontMatterV1) {
			value.Rendition.BodySHA256 = frontmatterHash("wrong")
		}},
		{name: "empty navigation key", mutate: func(value *RenditionFrontMatterV1) {
			value.Navigation.Entries[0].Key = ""
		}},
		{name: "duplicate navigation key", mutate: func(value *RenditionFrontMatterV1) {
			value.Navigation.Entries[1].Key = value.Navigation.Entries[0].Key
		}},
		{name: "out-of-range byte", mutate: func(value *RenditionFrontMatterV1) {
			value.Navigation.Entries[1].Byte = len(body)
		}},
		{name: "decreasing byte", mutate: func(value *RenditionFrontMatterV1) {
			value.Navigation.Entries[0].Byte = 4
			value.Navigation.Entries[0].Line = 2
			value.Navigation.Entries[1].Byte = 0
			value.Navigation.Entries[1].Line = 1
		}},
		{name: "mid-rune byte", mutate: func(value *RenditionFrontMatterV1) {
			value.Navigation.Entries[0].Byte = 1
		}},
		{name: "wrong line", mutate: func(value *RenditionFrontMatterV1) {
			value.Navigation.Entries[1].Line = 3
		}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			value := base
			value.Navigation.Entries = append([]RenditionNavigationEntryV1(nil), base.Navigation.Entries...)
			test.mutate(&value)
			_, _, err := ParseRenditionFrontMatterV1(frontmatterDataForTest(t, value, body))
			require.Error(t, err)
		})
	}
}

func TestParseRenditionFrontMatterV1AcceptsMonotonicUnicodeNavigation(t *testing.T) {
	body := []byte("\nαlpha\nβeta\n")
	value := validFrontmatterForBody(body)
	value.Document.UnitCount = 2
	value.Navigation.Entries = []RenditionNavigationEntryV1{
		{Key: "line:000000", Kind: EvidenceLocatorLine, Line: 1, Byte: 0},
		{Key: "line:000001", Kind: EvidenceLocatorLine, Line: 3, Byte: 8},
	}

	parsed, parsedBody, err := ParseRenditionFrontMatterV1(frontmatterDataForTest(t, value, body))
	require.NoError(t, err)
	require.Equal(t, value, parsed)
	require.Equal(t, body, parsedBody)
}

func TestParseRenditionFrontMatterV1RejectsNonCanonicalEncoding(t *testing.T) {
	_, data := canonicalFrontmatterForTest(t)
	headerEnd := bytes.Index(data, []byte("\n---\n")) + len("\n---\n")
	header, body := data[:headerEnd], data[headerEnd:]
	digest := []byte(frontmatterHash("source"))

	tests := []struct {
		name string
		data []byte
	}{
		{name: "unknown key", data: bytes.Replace(data, []byte("docbank:\n"), []byte("docbank:\n  unknown: true\n"), 1)},
		{name: "duplicate key", data: bytes.Replace(data, []byte("  source:\n"), []byte("  contract: \"docbank-sanitized-markdown/v1\"\n  source:\n"), 1)},
		{name: "multiple YAML documents", data: bytes.Replace(data, []byte("\n---\n"), []byte("\n--- # second document\nother: true\n---\n"), 1)},
		{name: "alias", data: bytes.Replace(data, append([]byte{'"'}, digest...), append([]byte("&digest \""), digest...), 1)},
		{name: "unquoted scalar", data: bytes.Replace(data, []byte("contract: \"docbank-sanitized-markdown/v1\""), []byte("contract: docbank-sanitized-markdown/v1"), 1)},
		{name: "reordered fields", data: bytes.Replace(data,
			[]byte("    format: \"pdf\"\n    media_type: \"application/pdf\"\n"),
			[]byte("    media_type: \"application/pdf\"\n    format: \"pdf\"\n"), 1)},
		{name: "missing opening delimiter", data: data[4:]},
		{name: "missing closing delimiter", data: append(append([]byte(nil), header[:headerEnd-len("\n---\n")]...), body...)},
		{name: "truncated header", data: []byte("---\ndocbank:\n")},
		{name: "BOM", data: append([]byte{0xef, 0xbb, 0xbf}, data...)},
		{name: "CRLF", data: bytes.ReplaceAll(data, []byte{'\n'}, []byte{'\r', '\n'})},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, _, err := ParseRenditionFrontMatterV1(test.data)
			require.Error(t, err)
		})
	}
}

func TestParseRenditionFrontMatterV1BoundsDecodeErrors(t *testing.T) {
	marker := "ZXQLEAK42" + strings.Repeat("x", 512)
	tests := []struct {
		name string
		data []byte
	}{
		{name: "unknown key", data: []byte("---\ndocbank:\n  \"" + marker + "\": true\n---\n")},
		{name: "scalar value", data: []byte("---\ndocbank:\n  document:\n    unit_count: \"" + marker + "\"\n---\n")},
		{name: "duplicate key", data: []byte("---\ndocbank:\n  \"" + marker + "\": true\n  \"" + marker + "\": false\n---\n")},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			frontmatter, body, err := ParseRenditionFrontMatterV1(test.data)
			require.Error(t, err)
			require.Zero(t, frontmatter)
			require.Nil(t, body)
			require.NotContains(t, err.Error(), "ZXQ")
			require.LessOrEqual(t, len(err.Error()), 128)
		})
	}
}

func TestParseRenditionFrontMatterV1ReturnsBorrowedBody(t *testing.T) {
	_, data := canonicalFrontmatterForTest(t)
	_, body, err := ParseRenditionFrontMatterV1(data)
	require.NoError(t, err)
	original := body[0]
	body[0] ^= 1
	require.Equal(t, body[0], data[len(data)-len(body)])
	body[0] = original
}

func TestParseRenditionFrontMatterV1RejectsEmptyAndInvalidUTF8Body(t *testing.T) {
	value, data := canonicalFrontmatterForTest(t)
	headerEnd := len(data) - len("# First\n\nAlpha\n")
	header := data[:headerEnd]

	_, _, err := ParseRenditionFrontMatterV1(header)
	require.ErrorContains(t, err, "empty or not UTF-8")

	value.Rendition.BodySHA256 = checksumBytes([]byte{0xff})
	_, _, err = ParseRenditionFrontMatterV1(frontmatterDataForTest(t, value, []byte{0xff}))
	require.ErrorContains(t, err, "empty or not UTF-8")
}

func TestRenditionFrontMatterHeaderBound(t *testing.T) {
	body := []byte("body\n")
	value := validFrontmatterForBody(body)
	base, err := marshalRenditionFrontMatterV1(value)
	require.NoError(t, err)
	const titleSyntaxBytes = len("    title: \"\"\n")
	value.Document.Title = strings.Repeat("a", maxRenditionFrontMatterBytes-len(base)-titleSyntaxBytes)

	header, err := marshalRenditionFrontMatterV1(value)
	require.NoError(t, err)
	require.Len(t, header, maxRenditionFrontMatterBytes)
	parsed, parsedBody, err := ParseRenditionFrontMatterV1(append(append([]byte(nil), header...), body...))
	require.NoError(t, err)
	require.Equal(t, value, parsed)
	require.Equal(t, body, parsedBody)

	value.Document.Title += "a"
	_, err = marshalRenditionFrontMatterV1(value)
	require.ErrorContains(t, err, "byte bound")
}

func TestRenditionFrontMatterHeaderBoundIncludesEscapingExpansion(t *testing.T) {
	body := []byte("body\n")
	value := validFrontmatterForBody(body)
	value.Document.Title = strings.Repeat("\"", maxRenditionFrontMatterBytes/2)
	require.Less(t, len(value.Document.Title), maxRenditionFrontMatterBytes)
	_, err := marshalRenditionFrontMatterV1(value)
	require.ErrorContains(t, err, "byte bound")
}

func TestParseRenditionFrontMatterV1BoundsMissingClosureSearch(t *testing.T) {
	data := append([]byte("---\n"), bytes.Repeat([]byte{'a'}, maxRenditionFrontMatterBytes)...)
	data = append(data, []byte("\n---\nbody\n")...)
	_, _, err := ParseRenditionFrontMatterV1(data)
	require.ErrorContains(t, err, "closing delimiter is missing")
}

func TestEnvelopeRenditionV1BoundsNavigationEntries(t *testing.T) {
	for _, unitCount := range []int{1024, 1025} {
		t.Run(strconv.Itoa(unitCount), func(t *testing.T) {
			units := make([]NormalizedUnitV1, unitCount)
			texts := make([]string, unitCount)
			for index := range units {
				text := fmt.Sprintf("unit-%04d", index)
				texts[index] = text
				units[index] = NormalizedUnitV1{
					EvidenceUnitID: fmt.Sprintf("page:%06d", index), Order: index, Text: text,
					Locator: EvidenceLocatorV1{Kind: EvidenceLocatorPage, IndexOrigin: EvidenceIndexOriginOne},
				}
			}
			body := []byte(strings.Join(texts, "\n") + "\n")
			rendition := RenditionV1{ContractVersion: RenditionContractV1,
				Completeness: EvidenceComplete, EvidenceChecksum: frontmatterHash("evidence"),
				Markdown: body, MarkdownChecksum: checksumBytes(body), Units: units}
			rendition.Checksum = renditionChecksum(rendition)

			rendered, facts, err := EnvelopeRenditionV1(rendition, validRenditionEnvelope())
			require.NoError(t, err)
			require.Len(t, facts.Navigation.Entries, min(unitCount, maxRenditionNavigationEntries))
			require.Equal(t, unitCount <= maxRenditionNavigationEntries, facts.Navigation.Complete)
			_, parsedBody, err := ParseRenditionFrontMatterV1(rendered.Markdown)
			require.NoError(t, err)
			require.Equal(t, body, parsedBody)
		})
	}
}

func validFrontmatterForBody(body []byte) RenditionFrontMatterV1 {
	return RenditionFrontMatterV1{
		Contract: RenditionMarkdownContractV1,
		Source: RenditionMarkdownSourceV1{
			SHA256: frontmatterHash("source"), Format: "pdf", MediaType: "application/pdf",
		},
		Rendition: RenditionMarkdownBuildV1{
			BuildID: frontmatterHash("build"), RenditionRequestFingerprint: frontmatterHash("request"),
			EvidenceLexicalFingerprint: frontmatterHash("lexical"), NormalizedEvidenceContract: NormalizedEvidenceContractV1,
			BodySHA256: checksumBytes(body), Completeness: EvidenceComplete,
		},
		Document: RenditionMarkdownDocumentV1{UnitKind: EvidenceUnitPage, UnitCount: 1},
		Navigation: RenditionMarkdownNavigationV1{OffsetBase: RenditionNavigationOffsetBody, Complete: true,
			Entries: []RenditionNavigationEntryV1{{Key: "page:000000", Kind: EvidenceLocatorPage, Line: 1, Byte: 0}}},
	}
}

func canonicalFrontmatterForTest(t *testing.T) (RenditionFrontMatterV1, []byte) {
	t.Helper()
	body := []byte("# First\n\nAlpha\n")
	value := validFrontmatterForBody(body)
	return value, frontmatterDataForTest(t, value, body)
}

func frontmatterDataForTest(t *testing.T, value RenditionFrontMatterV1, body []byte) []byte {
	t.Helper()
	header, err := marshalRenditionFrontMatterV1(value)
	require.NoError(t, err)
	return append(append([]byte(nil), header...), body...)
}
