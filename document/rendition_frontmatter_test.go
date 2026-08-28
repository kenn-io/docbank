package document

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
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

func frontmatterHash(value string) string {
	digest := sha256.Sum256([]byte(value))
	return hex.EncodeToString(digest[:])
}
