package document

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
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
