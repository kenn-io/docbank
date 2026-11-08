package mediatest

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/media"
	"go.kenn.io/docbank/document/upload"
)

// AuthorizePDF inspects and seals a PDF with the synthetic password "open".
// It binds the authorization to the sealed metadata and closes the upload at cleanup.
func AuthorizePDF(t *testing.T, source []byte, metadata document.AuthorizedUploadMetadata, authorization *document.RenditionAuthorization) document.AuthorizedUpload {
	t.Helper()
	inspection, err := media.InspectCapability(t.Context(), bytes.NewReader(source), media.InspectionPolicy{
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

// Upload provides a source with inspectable reads and replaceable metadata.
type Upload struct {
	reader        *bytes.Reader
	MetadataValue document.AuthorizedUploadMetadata
	Reads         int
}

// NewUpload binds synthetic authorization metadata to the supplied bytes.
func NewUpload(data []byte, metadata document.AuthorizedUploadMetadata) *Upload {
	digest := sha256.Sum256(data)
	metadata.ByteLength = int64(len(data))
	metadata.SHA256 = hex.EncodeToString(digest[:])
	metadata.CapabilityRecordChecksum = strings.Repeat("2", 64)
	metadata.ProviderMetadataChecksum = strings.Repeat("3", 64)
	metadata.InputKind = document.RenditionInputOriginalFile
	return &Upload{reader: bytes.NewReader(data), MetadataValue: metadata}
}

func (upload *Upload) Read(buffer []byte) (int, error) {
	upload.Reads++
	read, err := upload.reader.Read(buffer)
	if err != nil {
		return read, fmt.Errorf("read test upload: %w", err)
	}
	return read, nil
}

func (*Upload) Close() error { return nil }

func (upload *Upload) Metadata() document.AuthorizedUploadMetadata { return upload.MetadataValue }

var _ document.AuthorizedUpload = (*Upload)(nil)
var _ io.ReadCloser = (*Upload)(nil)

// Authorization permits one local provider operation using the source metadata.
func Authorization(
	descriptor document.RenditionDescriptor, metadata document.AuthorizedUploadMetadata,
) document.RenditionAuthorization {
	started := time.Now().UTC().Add(-time.Minute)
	return document.RenditionAuthorization{
		ProviderID: descriptor.ID, DescriptorFingerprint: descriptor.Fingerprint,
		PolicyFingerprint:           descriptor.PolicyFingerprint,
		RenditionRequestFingerprint: strings.Repeat("4", 64),
		SourceSHA256:                metadata.SHA256, SourceBytes: metadata.ByteLength,
		CapabilityRecordChecksum: metadata.CapabilityRecordChecksum,
		ProviderMetadataChecksum: metadata.ProviderMetadataChecksum,
		MediaFamily:              metadata.MediaFamily, MediaType: metadata.MediaType,
		InputKind: metadata.InputKind, MaxTotalResultBytes: 1 << 20,
		AuthorizedAt: started.Format("2006-01-02T15:04:05.000000000Z"),
		ExpiresAt:    started.Add(10 * time.Minute).Format("2006-01-02T15:04:05.000000000Z"),
	}
}
