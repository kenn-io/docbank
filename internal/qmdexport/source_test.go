package qmdexport

import (
	"bytes"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/document"
)

func TestSplitFrontmatterExcludesOnlyCanonicalDocbankEnvelope(t *testing.T) {
	// Mutation caught: generic YAML stripping would remove user-authored data,
	// while permissive envelope parsing would admit malformed Docbank claims.
	envelope, body, _ := syntheticCanonicalEnvelope(t)
	frontmatter, gotBody, err := splitFrontmatter(envelope)
	require.NoError(t, err)
	require.Equal(t, body, gotBody)
	require.Contains(t, frontmatter, "docbank:\n")
	require.NotContains(t, frontmatter, string(body))

	ordinary := [][]byte{
		[]byte("---\ntitle: ordinary\n---\nbody\n"),
		[]byte("---\n  docbank: nested\n---\nbody\n"),
		[]byte("---\n\nA horizontal rule follows.\n"),
		[]byte("body docbank-sanitized-markdown/v1\n"),
	}
	for _, markdown := range ordinary {
		frontmatter, gotBody, err := splitFrontmatter(markdown)
		require.NoError(t, err)
		require.Empty(t, frontmatter)
		require.Equal(t, markdown, gotBody)
	}
}

func TestSplitFrontmatterRejectsEveryMalformedClaim(t *testing.T) {
	// Mutation caught: recognizing a reserved header without calling the strict
	// canonical parser could silently turn corrupt authority into search text.
	envelope, _, _ := syntheticCanonicalEnvelope(t)
	crlf := bytes.ReplaceAll(envelope, []byte{'\n'}, []byte{'\r', '\n'})
	tests := map[string][]byte{
		"BOM":            append([]byte{0xef, 0xbb, 0xbf}, envelope...),
		"CRLF":           crlf,
		"quoted key":     []byte("---\n'docbank':\n  contract: \"docbank-sanitized-markdown/v1\"\n---\nbody\n"),
		"double key":     []byte("---\n\"docbank\":\n  contract: \"docbank-sanitized-markdown/v1\"\n---\nbody\n"),
		"unknown":        bytes.Replace(envelope, []byte("docbank:\n"), []byte("docbank:\n  unknown: true\n"), 1),
		"reordered":      bytes.Replace(envelope, []byte("    format: \"pdf\"\n    media_type: \"application/pdf\"\n"), []byte("    media_type: \"application/pdf\"\n    format: \"pdf\"\n"), 1),
		"unterminated":   []byte("---\ndocbank:\n  contract: \"docbank-sanitized-markdown/v1\"\nbody\n"),
		"oversized":      []byte("---\ndocbank:\n" + strings.Repeat("x", maxClaimedFrontmatterBytes) + "\n---\nbody\n"),
		"explicit token": []byte("---\ncontract: docbank-sanitized-markdown/v1\n---\nbody\n"),
		"document end":   bytes.Replace(envelope, []byte("\n---\n"), []byte("\n...\n"), 1),
	}
	for name, markdown := range tests {
		t.Run(name, func(t *testing.T) {
			frontmatter, body, err := splitFrontmatter(markdown)
			require.Error(t, err)
			require.Empty(t, frontmatter)
			require.Nil(t, body)
		})
	}
}

func TestSplitFrontmatterDetectionBudgetCoversEntireInputPrefix(t *testing.T) {
	// Mutation caught: applying the lexical budget only after the opening line
	// lets reserved markers beyond the total detection window claim authority.
	totalDetectionBytes := maxClaimedFrontmatterBytes + 1
	longFirstLine := append(bytes.Repeat([]byte{'x'}, totalDetectionBytes), []byte("\n---\ndocbank:\n")...)
	frontmatter, body, err := splitFrontmatter(longFirstLine)
	require.NoError(t, err)
	require.Empty(t, frontmatter)
	require.Equal(t, longFirstLine, body)

	tests := []struct {
		name       string
		opening    []byte
		linePrefix string
		marker     string
	}{
		{name: "root claim after LF opening", opening: []byte("---\n"), marker: "docbank:"},
		{name: "token after BOM and CRLF opening", opening: []byte("\xef\xbb\xbf---\r\n"),
			linePrefix: "contract: ", marker: document.RenditionMarkdownContractV1},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			markerEnd := totalDetectionBytes + len(test.opening)
			lineStart := markerEnd - len(test.marker) - len(test.linePrefix)
			fillerLength := lineStart - len(test.opening) - 1
			markdown := append([]byte(nil), test.opening...)
			markdown = append(markdown, bytes.Repeat([]byte{'x'}, fillerLength)...)
			markdown = append(markdown, '\n')
			markdown = append(markdown, test.linePrefix...)
			markdown = append(markdown, test.marker...)
			markdown = append(markdown, []byte("\n---\nbody\n")...)
			markerStart := bytes.Index(markdown, []byte(test.marker))
			require.Less(t, markerStart, totalDetectionBytes)
			require.Greater(t, markerStart+len(test.marker), totalDetectionBytes)

			frontmatter, body, err := splitFrontmatter(markdown)
			require.NoError(t, err)
			require.Empty(t, frontmatter)
			require.Equal(t, markdown, body)
		})
	}
}

func TestBuildConsumesProducerEnvelopeAndRejectsBodyMutation(t *testing.T) {
	// Mutation caught: stripping without canonical body verification would
	// accept an envelope whose outer CAS digest was updated after tampering.
	envelope, wantBody, wantFacts := syntheticCanonicalEnvelope(t)
	source := syntheticSource(1, "00000000-0000-4000-8000-000000000001", envelope)
	generation, err := Build(t.Context(), "synthetic", []Source{source}, syntheticReader{source.BlobSHA256: envelope}, Options{})
	require.NoError(t, err)
	require.Len(t, generation.Manifest.Entries, 1)
	entry := generation.Manifest.Entries[0]
	require.Equal(t, wantBody, generation.documents[entry.RelativePath])
	require.Equal(t, syntheticDigest(wantBody), entry.ExportedMarkdownSHA256)
	require.Contains(t, entry.Frontmatter, "offset_base: \"body\"")
	require.Contains(t, string(wantBody), "\n\n---\n\n")
	require.Contains(t, string(wantBody), "βeta")
	reconstructed := append([]byte("---\n"+entry.Frontmatter+"\n---\n"), wantBody...)
	parsedFacts, parsedBody, err := document.ParseRenditionFrontMatterV1(reconstructed)
	require.NoError(t, err)
	require.Equal(t, wantFacts, parsedFacts)
	require.Equal(t, wantBody, parsedBody)
	require.Equal(t, document.RenditionNavigationOffsetBody, parsedFacts.Navigation.OffsetBase)

	mutated := append([]byte(nil), envelope...)
	mutated[len(mutated)-2] ^= 1
	mutatedSource := syntheticSource(1, source.ContentVersionID, mutated)
	failed, err := Build(t.Context(), "synthetic", []Source{mutatedSource}, syntheticReader{mutatedSource.BlobSHA256: mutated}, Options{})
	require.ErrorContains(t, err, "body SHA-256")
	require.Zero(t, failed)
}

func syntheticCanonicalEnvelope(
	t *testing.T,
) ([]byte, []byte, document.RenditionFrontMatterV1) {
	t.Helper()
	evidencePolicy, err := document.NewEvidencePolicy(100_000)
	require.NoError(t, err)
	evidence, err := document.NormalizeEvidenceV1(document.SourceEvidenceV1{
		ContractVersion: document.SourceEvidenceContractV1,
		Completeness:    document.EvidenceComplete, Family: "pdf", UnitKind: document.EvidenceUnitPage,
		Units: []document.SourceEvidenceUnitV1{
			{Order: 0, HeadingPath: []string{"First"}, Text: "# First\n\nAlpha\n\n---\n\nInternal rule",
				Locator: document.SourceEvidenceLocatorV1{Kind: document.EvidenceLocatorPage, IndexOrigin: document.EvidenceIndexOriginOne, Start: 1, End: 1}},
			{Order: 1, HeadingPath: []string{"Second"}, Text: "# Second\n\nβeta",
				Locator: document.SourceEvidenceLocatorV1{Kind: document.EvidenceLocatorPage, IndexOrigin: document.EvidenceIndexOriginOne, Start: 2, End: 2}},
		},
	}, evidencePolicy)
	require.NoError(t, err)
	renderPolicy, err := document.NewRenditionPolicy(document.RenditionLimits{
		MaxDocumentChars: 100_000, MaxUnitRunes: 1000, MaxSegmentRunes: 4000,
	})
	require.NoError(t, err)
	bodyRendition, err := document.BuildRenditionV1(evidence, renderPolicy)
	require.NoError(t, err)
	body := append([]byte(nil), bodyRendition.Markdown...)
	enveloped, facts, err := document.EnvelopeRenditionV1(bodyRendition, document.RenditionEnvelopeV1{
		BuildID: syntheticDigest([]byte("build")), SourceSHA256: syntheticDigest([]byte("source")),
		SourceFormat: "pdf", SourceMediaType: "application/pdf",
		RenditionRequestFingerprint: syntheticDigest([]byte("request")),
		EvidenceLexicalFingerprint:  syntheticDigest([]byte("lexical")),
		NormalizedEvidenceContract:  document.NormalizedEvidenceContractV1,
		UnitKind:                    document.EvidenceUnitPage, Title: "Synthetic", Language: "en",
	})
	require.NoError(t, err)
	return enveloped.Markdown, body, facts
}
