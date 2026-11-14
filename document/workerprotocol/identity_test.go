package workerprotocol

import (
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
)

func TestSubmissionKeyV1Golden(t *testing.T) {
	metadata := document.AuthorizedUploadMetadata{Filename: "sample.pdf", MediaFamily: "pdf", MediaType: "application/pdf", ByteLength: 3, SHA256: "abc", CapabilityRecordChecksum: "cap", ProviderMetadataChecksum: "meta", InputKind: document.RenditionInputOriginalFile}
	authorization := document.RenditionAuthorization{ProviderID: ProviderID, SourceSHA256: "abc", SourceBytes: 3, CapabilityRecordChecksum: "cap", ProviderMetadataChecksum: "meta", MediaFamily: "pdf", MediaType: "application/pdf", InputKind: document.RenditionInputOriginalFile, DiscloseFilename: true}
	key, err := SubmissionKey(metadata, authorization, [][2]string{{"to_formats", "json"}})
	require.NoError(t, err)
	require.Equal(t, "75f0a709ed770a727db6b825466e446a8df15f5b2a2fba5ec8ba1a0e10eddb7c", key)
}
