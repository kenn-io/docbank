package document

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

func TestEnvelopeRenditionV1KeepsNavigationBodyRelative(t *testing.T) {
	body := []byte("# First\n\nAlpha\n\n---\n\n# Second\n\nBeta\n")
	rendition := RenditionV1{ContractVersion: RenditionContractV1,
		Completeness: EvidenceComplete, EvidenceChecksum: frontmatterHash("evidence"),
		Markdown: body, MarkdownChecksum: checksumBytes(body),
		Units: []NormalizedUnitV1{
			{EvidenceUnitID: "page:000000", Order: 0, Text: "# First\n\nAlpha",
				Locator: EvidenceLocatorV1{Kind: EvidenceLocatorPage, IndexOrigin: EvidenceIndexOriginZero}},
			{EvidenceUnitID: "page:000001", Order: 1, Text: "# Second\n\nBeta",
				Locator: EvidenceLocatorV1{Kind: EvidenceLocatorPage, IndexOrigin: EvidenceIndexOriginZero}},
		}}
	rendition.Checksum = renditionChecksum(rendition)

	got, frontmatter, err := EnvelopeRenditionV1(rendition, RenditionEnvelopeV1{
		BuildID: frontmatterHash("build"), SourceSHA256: frontmatterHash("source"),
		SourceFormat: "pdf", SourceMediaType: "application/pdf",
		RenditionRequestFingerprint: frontmatterHash("request"),
		EvidenceLexicalFingerprint:  frontmatterHash("lexical"),
		NormalizedEvidenceContract:  NormalizedEvidenceContractV1, UnitKind: EvidenceUnitPage,
	})
	require.NoError(t, err)
	require.True(t, bytes.HasPrefix(got.Markdown,
		[]byte("---\ndocbank:\n  contract: \"docbank-sanitized-markdown/v1\"\n")))
	parts := bytes.SplitN(got.Markdown, []byte("---\n"), 3)
	require.Len(t, parts, 3)
	require.Equal(t, body, parts[2])
	require.Equal(t, checksumBytes(body), frontmatter.Rendition.BodySHA256)
	for _, entry := range frontmatter.Navigation.Entries {
		require.Less(t, entry.Byte, len(body))
		require.Equal(t, entry.Line, 1+bytes.Count(body[:entry.Byte], []byte{'\n'}))
		marker := strings.TrimPrefix(entry.Key, "page:")
		if marker == "000000" {
			require.True(t, bytes.HasPrefix(body[entry.Byte:], []byte("# First")))
		} else {
			require.True(t, bytes.HasPrefix(body[entry.Byte:], []byte("# Second")))
		}
	}

	t.Run("image text matching its notice", func(t *testing.T) {
		evidence := normalizeRenditionEvidence(t, SourceEvidenceV1{ContractVersion: SourceEvidenceContractV1, Completeness: EvidenceDegradedProvenance, Family: "image", UnitKind: EvidenceUnitGeneric,
			Omissions: []SourceEvidenceOmissionV1{{Kind: EvidenceOmissionField, Field: "natural_provenance", Reason: "unknown"}},
			Units:     []SourceEvidenceUnitV1{{Order: 0, HeadingPath: []string{ImageUnderstandingHeading, "Visible text"}, Text: "image", Locator: SourceEvidenceLocatorV1{Kind: EvidenceLocatorGeneric, IndexOrigin: EvidenceIndexOriginNone, Name: "model"}}}})
		policy, err := NewRenditionPolicy(RenditionLimits{MaxDocumentChars: 10_000, MaxUnitRunes: 10_000, MaxSegmentRunes: 1000})
		require.NoError(t, err)
		imageRendition, err := BuildRenditionV1(evidence, policy)
		require.NoError(t, err)
		enveloped, _, err := EnvelopeRenditionV1(imageRendition, RenditionEnvelopeV1{BuildID: frontmatterHash("build"), SourceSHA256: frontmatterHash("source"), SourceFormat: "png", SourceMediaType: "image/png", RenditionRequestFingerprint: frontmatterHash("request"), EvidenceLexicalFingerprint: frontmatterHash("lexical"), NormalizedEvidenceContract: NormalizedEvidenceContractV1, UnitKind: EvidenceUnitGeneric})
		require.NoError(t, err)
		front, imageBody, err := ParseRenditionFrontMatterV1(enveloped.Markdown)
		require.NoError(t, err)
		require.Len(t, front.Navigation.Entries, 1)
		textStart := bytes.Index(imageBody, []byte("\n\nimage\n"))
		require.NotEqual(t, -1, textStart)
		require.Equal(t, textStart+2, front.Navigation.Entries[0].Byte)
		require.Equal(t, 1+bytes.Count(imageBody[:textStart+2], []byte("\n")), front.Navigation.Entries[0].Line)
	})

	parsed, parsedBody, err := ParseRenditionFrontMatterV1(got.Markdown)
	require.NoError(t, err)
	require.Equal(t, frontmatter, parsed)
	require.Equal(t, body, parsedBody)
}

func TestEnvelopeRenditionV1AcceptsTimedNavigation(t *testing.T) {
	evidence := normalizeRenditionEvidence(t, SourceEvidenceV1{
		ContractVersion: SourceEvidenceContractV1, Family: "audio",
		Completeness: EvidencePartial, UnitKind: EvidenceUnitSegment,
		Omissions: []SourceEvidenceOmissionV1{{Kind: EvidenceOmissionField,
			Field: "non_speech_content", Reason: "speech transcript does not describe non-speech content"}},
		Units: []SourceEvidenceUnitV1{{Order: 0, Text: "synthetic cue",
			Locator: SourceEvidenceLocatorV1{Kind: EvidenceLocatorSegment,
				IndexOrigin: EvidenceIndexOriginZero, Start: 1000, End: 2000}}},
	})
	policy, err := NewRenditionPolicy(testRenditionLimits(1000))
	require.NoError(t, err)
	rendition, err := BuildRenditionV1(evidence, policy)
	require.NoError(t, err)
	rendered, _, err := EnvelopeRenditionV1(rendition, RenditionEnvelopeV1{
		BuildID: frontmatterHash("build"), SourceSHA256: frontmatterHash("source"),
		SourceFormat: "wav", SourceMediaType: "audio/wav",
		RenditionRequestFingerprint: frontmatterHash("request"),
		EvidenceLexicalFingerprint:  frontmatterHash("lexical"),
		NormalizedEvidenceContract:  NormalizedEvidenceContractV1, UnitKind: EvidenceUnitSegment,
	})
	require.NoError(t, err)

	frontmatter, body, err := ParseRenditionFrontMatterV1(rendered.Markdown)
	require.NoError(t, err)
	require.Equal(t, "synthetic cue\n", string(body))
	require.Equal(t, EvidenceUnitSegment, frontmatter.Document.UnitKind)
	require.Equal(t, []RenditionNavigationEntryV1{{
		Key: evidence.Units[0].ID, Kind: EvidenceLocatorSegment, Line: 1, Byte: 0,
	}}, frontmatter.Navigation.Entries)
}

func TestParseRenditionFrontMatterV1RejectsCorruptBodyAndNavigation(t *testing.T) {
	body := []byte("# First\n\nAlpha\n")
	rendition := RenditionV1{ContractVersion: RenditionContractV1,
		Completeness: EvidenceComplete, EvidenceChecksum: frontmatterHash("evidence"),
		Markdown: body, MarkdownChecksum: checksumBytes(body),
		Units: []NormalizedUnitV1{{EvidenceUnitID: "page:000000", Order: 0, Text: string(body),
			Locator: EvidenceLocatorV1{Kind: EvidenceLocatorPage, IndexOrigin: EvidenceIndexOriginZero}}}}
	rendered, _, err := EnvelopeRenditionV1(rendition, RenditionEnvelopeV1{
		BuildID: frontmatterHash("build"), SourceSHA256: frontmatterHash("source"),
		SourceFormat: "pdf", SourceMediaType: "application/pdf",
		RenditionRequestFingerprint: frontmatterHash("request"),
		EvidenceLexicalFingerprint:  frontmatterHash("lexical"),
		NormalizedEvidenceContract:  NormalizedEvidenceContractV1, UnitKind: EvidenceUnitPage})
	require.NoError(t, err)

	corrupt := append([]byte(nil), rendered.Markdown...)
	corrupt[len(corrupt)-2] ^= 1
	_, _, err = ParseRenditionFrontMatterV1(corrupt)
	require.ErrorContains(t, err, "body SHA-256")

	badNavigation := bytes.Replace(rendered.Markdown, []byte("byte: 0"), []byte("byte: 999999"), 1)
	_, _, err = ParseRenditionFrontMatterV1(badNavigation)
	require.Error(t, err)
}

func TestEnvelopeRenditionV1FrontMatterNeverCarriesHTMLMarkup(t *testing.T) {
	heading := "# <script>alert(1)</script> & <img src=x onerror=alert(1)>"
	body := []byte(heading + "\n\nAlpha\n")
	rendition := RenditionV1{ContractVersion: RenditionContractV1,
		Completeness: EvidenceComplete, EvidenceChecksum: frontmatterHash("evidence"),
		Markdown: body, MarkdownChecksum: checksumBytes(body),
		Units: []NormalizedUnitV1{{EvidenceUnitID: "page:000000", Text: string(body[:len(body)-1]),
			HeadingPath: []string{heading[2:]},
			Locator:     EvidenceLocatorV1{Kind: EvidenceLocatorPage, IndexOrigin: EvidenceIndexOriginZero}}}}
	rendition.Checksum = renditionChecksum(rendition)

	got, frontmatter, err := EnvelopeRenditionV1(rendition, RenditionEnvelopeV1{
		BuildID: frontmatterHash("build"), SourceSHA256: frontmatterHash("source"),
		SourceFormat: "x<b>", SourceMediaType: "text/plain",
		RenditionRequestFingerprint: frontmatterHash("request"),
		EvidenceLexicalFingerprint:  frontmatterHash("lexical"),
		NormalizedEvidenceContract:  NormalizedEvidenceContractV1, UnitKind: EvidenceUnitPage,
	})
	require.NoError(t, err)
	header := got.Markdown[:len(got.Markdown)-len(body)]
	require.NotContains(t, string(header), "<")
	require.NotContains(t, string(header), ">")
	require.NotContains(t, string(header), "&")
	require.Contains(t, string(header), `title: "\u003cscript\u003ealert(1)\u003c/script\u003e \u0026 `)
	require.Equal(t, heading[2:], frontmatter.Navigation.Entries[0].Title)
	var parsed struct {
		Docbank struct {
			Source struct {
				Format string `yaml:"format"`
			} `yaml:"source"`
			Navigation struct {
				Entries []struct {
					Title string `yaml:"title"`
				} `yaml:"entries"`
			} `yaml:"navigation"`
		} `yaml:"docbank"`
	}
	require.NoError(t, yaml.Unmarshal(bytes.TrimSuffix(header, []byte("---\n")), &parsed))
	require.Equal(t, heading[2:], parsed.Docbank.Navigation.Entries[0].Title)
	require.Equal(t, "x<b>", parsed.Docbank.Source.Format)
}

func frontmatterHash(value string) string {
	digest := sha256.Sum256([]byte(value))
	return hex.EncodeToString(digest[:])
}

func TestOmissionOnlyPDFEnvelope(t *testing.T) {
	source := SourceEvidenceV1{ContractVersion: SourceEvidenceContractV1, Family: "pdf", UnitKind: EvidenceUnitPage, Completeness: EvidencePartial,
		Omissions: []SourceEvidenceOmissionV1{{Kind: EvidenceOmissionUnit, Reason: "no_text_layer", Locator: &SourceEvidenceLocatorV1{Kind: EvidenceLocatorPage, IndexOrigin: EvidenceIndexOriginOne, Start: 1, End: 1}}},
	}
	normalized := normalizeRenditionEvidence(t, source)
	policy, err := NewRenditionPolicy(RenditionLimits{MaxDocumentChars: 1000, MaxUnitRunes: 1000, MaxSegmentRunes: 100})
	require.NoError(t, err)
	rendition, err := BuildRenditionV1(normalized, policy)
	require.NoError(t, err)
	require.Empty(t, rendition.Markdown)
	require.Equal(t, "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855", rendition.MarkdownChecksum)
	envelope := RenditionEnvelopeV1{BuildID: strings.Repeat("b", 64), SourceSHA256: "85bcc1ce42ddbdbb384839d9d7c947d3d811a52ea5bf63617ac32a3bc88053c5", SourceFormat: "pdf", SourceMediaType: "application/pdf", RenditionRequestFingerprint: strings.Repeat("e", 64), EvidenceLexicalFingerprint: strings.Repeat("f", 64), NormalizedEvidenceContract: NormalizedEvidenceContractV1, UnitKind: EvidenceUnitPage}
	got, front, err := EnvelopeRenditionV1(rendition, envelope)
	require.NoError(t, err)
	fixture, err := os.ReadFile("../frontend/src/testdata/omission-only-pdf.md")
	require.NoError(t, err)
	require.Equal(t, string(fixture), string(got.Markdown))
	parsed, body, err := ParseRenditionFrontMatterV1(got.Markdown)
	require.NoError(t, err)
	require.Equal(t, front, parsed)
	require.Empty(t, body)
	require.Zero(t, parsed.Document.UnitCount)
	require.Empty(t, parsed.Navigation.Entries)
	require.True(t, parsed.Navigation.Complete)
	envelope.SourceFormat = "epub"
	_, _, err = EnvelopeRenditionV1(rendition, envelope)
	require.Error(t, err)
	_, _, err = ParseRenditionFrontMatterV1(append(got.Markdown, []byte("unexpected")...))
	require.ErrorContains(t, err, "body and unit count disagree")
}
