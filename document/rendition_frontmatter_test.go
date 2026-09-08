package document

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestEnvelopeRenditionV1KeepsCanonicalNavigationBodyRelative(t *testing.T) {
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
	originalBody := bytes.Clone(body)
	originalUnits := append([]NormalizedUnitV1(nil), rendition.Units...)

	got, frontmatter, err := EnvelopeRenditionV1(rendition, validRenditionEnvelope())
	require.NoError(t, err)
	wantHeader := `---
docbank:
  contract: "docbank-sanitized-markdown/v1"
  source:
    sha256: "41cf6794ba4200b839c53531555f0f3998df4cbb01a4d5cb0b94e3ca5e23947d"
    format: "pdf"
    media_type: "application/pdf"
  rendition:
    build_id: "44575cf5b28512d75644bf54a517dcef304ff809fd511747621b4d64f19aac66"
    rendition_request_fingerprint: "1f58b9145b24d108d7ac38887338b3ea3229833b9c1e418250343f907bfd1047"
    evidence_lexical_fingerprint: "78f3f3423b346310eb58f4add60ff80181ec4c4b83fb663a6b072d6888b6b905"
    normalized_evidence_contract: "normalized-evidence/v1"
    body_sha256: "66bcca2e910d9d5a436ed6dd8926d2b1b3637cc023ed12b5d87e720ba4e3cf7c"
    completeness: "complete"
    truncated: false
  document:
    unit_kind: "page"
    unit_count: 2
  navigation:
    offset_base: "body"
    complete: true
    entries:
      - key: "page:000000"
        kind: "page"
        line: 1
        byte: 0
      - key: "page:000001"
        kind: "page"
        line: 7
        byte: 21
---
`
	require.Equal(t, wantHeader+string(body), string(got.Markdown))
	require.Equal(t, "66bcca2e910d9d5a436ed6dd8926d2b1b3637cc023ed12b5d87e720ba4e3cf7c", frontmatter.Rendition.BodySHA256)
	require.Equal(t, []RenditionNavigationEntryV1{
		{Key: "page:000000", Kind: EvidenceLocatorPage, Line: 1, Byte: 0},
		{Key: "page:000001", Kind: EvidenceLocatorPage, Line: 7, Byte: 21},
	}, frontmatter.Navigation.Entries)
	require.Equal(t, originalBody, rendition.Markdown)
	require.Equal(t, originalUnits, got.Units)
	require.Equal(t, checksumBytes(got.Markdown), got.MarkdownChecksum)
	require.Equal(t, renditionChecksum(got), got.Checksum)

	parsed, parsedBody, err := ParseRenditionFrontMatterV1(got.Markdown)
	require.NoError(t, err)
	require.Equal(t, frontmatter, parsed)
	require.Equal(t, body, parsedBody)
}

func TestEnvelopeRenditionV1RoundTripsRealNormalizedEvidence(t *testing.T) {
	evidence := normalizeRenditionEvidence(t, SourceEvidenceV1{
		ContractVersion: SourceEvidenceContractV1,
		Completeness:    EvidenceComplete,
		Family:          "pdf",
		UnitKind:        EvidenceUnitPage,
		Units: []SourceEvidenceUnitV1{
			{Order: 0, HeadingPath: []string{"First"},
				Locator: SourceEvidenceLocatorV1{Kind: EvidenceLocatorPage, IndexOrigin: EvidenceIndexOriginOne, Start: 1, End: 1},
				Text:    "# First\n\nAlpha\n\n---\n\nInternal rule"},
			{Order: 1, HeadingPath: []string{"Second"},
				Locator: SourceEvidenceLocatorV1{Kind: EvidenceLocatorPage, IndexOrigin: EvidenceIndexOriginOne, Start: 2, End: 2},
				Text:    "# Second\n\nβeta"},
		},
	})
	policy, err := NewRenditionPolicy(testRenditionLimits(100_000))
	require.NoError(t, err)
	bodyRendition, err := BuildRenditionV1(evidence, policy)
	require.NoError(t, err)
	wantBody := []byte("# First\nAlpha\n\nInternal rule\n\n---\n\n# Second\nβeta\n")
	require.Equal(t, wantBody, bodyRendition.Markdown)
	originalUnits := append([]NormalizedUnitV1(nil), bodyRendition.Units...)
	originalLexical := append([]LexicalSegmentV1(nil), bodyRendition.LexicalSegments...)
	originalBodyChecksum := bodyRendition.MarkdownChecksum
	originalRenditionChecksum := bodyRendition.Checksum
	require.Equal(t, frontmatterBytesHash(wantBody), originalBodyChecksum)

	enveloped, facts, err := EnvelopeRenditionV1(bodyRendition, validRenditionEnvelope())
	require.NoError(t, err)
	parsed, body, err := ParseRenditionFrontMatterV1(enveloped.Markdown)
	require.NoError(t, err)
	require.Equal(t, wantBody, body)
	require.Equal(t, facts, parsed)
	require.Equal(t, originalUnits, enveloped.Units)
	require.Equal(t, originalLexical, enveloped.LexicalSegments)
	require.Equal(t, originalBodyChecksum, facts.Rendition.BodySHA256)
	require.Equal(t, frontmatterBytesHash(enveloped.Markdown), enveloped.MarkdownChecksum)
	require.Equal(t, renditionChecksum(enveloped), enveloped.Checksum)
	require.NotEqual(t, originalRenditionChecksum, enveloped.Checksum)
	require.Contains(t, string(body), "\n\n---\n\n")
	require.Equal(t, []RenditionNavigationEntryV1{
		{Key: evidence.Units[0].ID, Kind: EvidenceLocatorPage, Title: "First", Line: 1, Byte: 0},
		{Key: evidence.Units[1].ID, Kind: EvidenceLocatorPage, Title: "Second", Line: 8, Byte: 35},
	}, facts.Navigation.Entries)
	header := string(enveloped.Markdown[:len(enveloped.Markdown)-len(body)])
	require.True(t, strings.HasPrefix(header, "---\ndocbank:\n  contract: \"docbank-sanitized-markdown/v1\"\n"))
	require.Contains(t, header, "    body_sha256: \""+originalBodyChecksum+"\"\n")
}

func TestParseRenditionFrontMatterV1RejectsCorruptBodyAndNavigation(t *testing.T) {
	body := []byte("# First\n\nAlpha\n")
	rendition := RenditionV1{ContractVersion: RenditionContractV1,
		Completeness: EvidenceComplete, EvidenceChecksum: frontmatterHash("evidence"),
		Markdown: body, MarkdownChecksum: checksumBytes(body),
		Units: []NormalizedUnitV1{{EvidenceUnitID: "page:000000", Order: 0, Text: string(body),
			Locator: EvidenceLocatorV1{Kind: EvidenceLocatorPage, IndexOrigin: EvidenceIndexOriginZero}}}}
	rendered, _, err := EnvelopeRenditionV1(rendition, validRenditionEnvelope())
	require.NoError(t, err)

	corrupt := append([]byte(nil), rendered.Markdown...)
	corrupt[len(corrupt)-2] ^= 1
	_, _, err = ParseRenditionFrontMatterV1(corrupt)
	require.ErrorContains(t, err, "body SHA-256")

	badNavigation := bytes.Replace(rendered.Markdown, []byte("byte: 0"), []byte("byte: 999999"), 1)
	_, _, err = ParseRenditionFrontMatterV1(badNavigation)
	require.Error(t, err)
}

func validRenditionEnvelope() RenditionEnvelopeV1 {
	return RenditionEnvelopeV1{
		BuildID: frontmatterHash("build"), SourceSHA256: frontmatterHash("source"),
		SourceFormat: "pdf", SourceMediaType: "application/pdf",
		RenditionRequestFingerprint: frontmatterHash("request"),
		EvidenceLexicalFingerprint:  frontmatterHash("lexical"),
		NormalizedEvidenceContract:  NormalizedEvidenceContractV1, UnitKind: EvidenceUnitPage,
	}
}

func frontmatterHash(value string) string {
	digest := sha256.Sum256([]byte(value))
	return hex.EncodeToString(digest[:])
}

func frontmatterBytesHash(value []byte) string {
	digest := sha256.Sum256(value)
	return hex.EncodeToString(digest[:])
}
