package mediatest

import (
	"bytes"
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/media"
	"go.kenn.io/docbank/document/upload"
)

// AuthorizePDF inspects and seals a PDF with the synthetic password "open".
// It binds the authorization to the sealed metadata and closes the upload at cleanup.
func AuthorizePDF(t *testing.T, source []byte, metadata document.AuthorizedUploadMetadata, authorization *document.RenditionAuthorization) document.AuthorizedUpload {
	t.Helper()
	inspection, err := media.InspectCapability(bytes.NewReader(source), media.InspectionPolicy{
		Filename: metadata.Filename, DeclaredMediaType: "application/pdf",
		ExpectedBytes: int64(len(source)), ExpectedSHA256: metadata.SHA256,
		DescriptorFingerprint: authorization.DescriptorFingerprint,
		ProfileFingerprint:    strings.Repeat("b", 64), DisclosureFingerprint: strings.Repeat("c", 64),
		InputKind: document.RenditionInputOriginalFile, UnlockPDF: true, PDFPasswords: []string{"wrong", "open"},
		MaxSourceBytes: 1 << 20, MaxExpandedBytes: 1 << 20, MaxEntryBytes: 1 << 20, MaxEntries: 1000, MaxPages: 100,
		MaxNestingDepth: 1, MaxTextLines: 1000, MaxCharacters: 1 << 20, MaxRecords: 1000, MaxSlides: 100, MaxSheets: 100, MaxCells: 1000, MaxSpineItems: 100, MaxResources: 100, MaxPixels: 1000000, MaxFrames: 100, MaxDurationMS: 1000,
	})
	require.NoError(t, err)
	require.True(t, inspection.Eligible)
	sealed, err := upload.Authorize(t.Context(), upload.Source{Reader: io.NopCloser(bytes.NewReader(source)), Directory: t.TempDir()}, inspection, upload.UploadMetadata{Filename: metadata.Filename})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, sealed.Close()) })
	authorization.CapabilityRecordChecksum = sealed.Metadata().CapabilityRecordChecksum
	authorization.ProviderMetadataChecksum = sealed.Metadata().ProviderMetadataChecksum
	return sealed
}
