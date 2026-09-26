package document

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestReadRenditionPassageV1KeepsUTF8BoundariesAcrossBuffers(t *testing.T) {
	body := append(bytes.Repeat([]byte{'x'}, 32<<10-1), []byte("😀 evidence\n")...)
	rendered, frontmatter, err := EnvelopeRenditionV1(RenditionV1{
		ContractVersion: RenditionContractV1, Completeness: EvidenceComplete,
		EvidenceChecksum: frontmatterHash("evidence"), Markdown: body,
		MarkdownChecksum: checksumBytes(body),
		Units: []NormalizedUnitV1{{EvidenceUnitID: "page:000000", Text: string(body),
			Locator: EvidenceLocatorV1{Kind: EvidenceLocatorPage, IndexOrigin: EvidenceIndexOriginOne}}},
	}, RenditionEnvelopeV1{BuildID: frontmatterHash("build"),
		SourceSHA256: frontmatterHash("source"), SourceFormat: "pdf", SourceMediaType: "application/pdf",
		RenditionRequestFingerprint: frontmatterHash("request"),
		EvidenceLexicalFingerprint:  frontmatterHash("lexical"),
		NormalizedEvidenceContract:  NormalizedEvidenceContractV1, UnitKind: EvidenceUnitPage})
	require.NoError(t, err)
	start := 32<<10 - 1
	ref, err := NewPassageRefV1(PassageRefV1{
		VaultUID:         "11111111-1111-4111-8111-111111111111",
		DocumentUID:      "22222222-2222-4222-8222-222222222222",
		ContentVersionID: "33333333-3333-4333-8333-333333333333",
		SourceSHA256:     frontmatter.Source.SHA256, RenditionBuildID: frontmatter.Rendition.BuildID,
		AttachmentID: frontmatterHash("attachment"),
	}, body, start, start+len("😀 evidence"))
	require.NoError(t, err)
	got, quote, err := ReadRenditionPassageV1(bytes.NewReader(rendered.Markdown), int64(len(rendered.Markdown)), ref)
	require.NoError(t, err)
	require.Equal(t, frontmatter, got)
	require.Equal(t, "😀 evidence", string(quote))

	for _, changed := range []PassageRefV1{
		func() PassageRefV1 { changed := ref; changed.ByteStart++; return changed }(),
		func() PassageRefV1 { changed := ref; changed.ByteEnd = start + 1; return changed }(),
	} {
		_, _, err := ReadRenditionPassageV1(bytes.NewReader(rendered.Markdown), int64(len(rendered.Markdown)), changed)
		require.ErrorContains(t, err, "splits UTF-8")
	}

	frontmatter.Navigation.Entries[0].Byte = start + 1
	frontmatter.Navigation.Entries[0].Line = 1
	header, err := marshalRenditionFrontMatterV1(frontmatter)
	require.NoError(t, err)
	badNavigation := bytes.Clone(header)
	badNavigation = append(badNavigation, body...)
	_, _, err = ReadRenditionPassageV1(bytes.NewReader(badNavigation), int64(len(badNavigation)), ref)
	require.ErrorContains(t, err, "navigation is invalid")
}

func TestReadRenditionPassageV1BoundsQuoteBeforeReading(t *testing.T) {
	ref := PassageRefV1{
		Version:          PassageRefVersionV1,
		VaultUID:         "11111111-1111-4111-8111-111111111111",
		DocumentUID:      "22222222-2222-4222-8222-222222222222",
		ContentVersionID: "33333333-3333-4333-8333-333333333333",
		SourceSHA256:     frontmatterHash("source"), RenditionBuildID: frontmatterHash("build"),
		AttachmentID: frontmatterHash("attachment"), BodySHA256: frontmatterHash("body"),
		QuoteSHA256: frontmatterHash("quote"), ByteEnd: 256<<10 + 1,
	}
	_, _, err := ReadRenditionPassageV1(bytes.NewReader(nil), 1<<30, ref)
	require.ErrorContains(t, err, "passage exceeds byte bound")
}
