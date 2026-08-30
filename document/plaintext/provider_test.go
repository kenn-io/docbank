package plaintext

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/document"
)

func TestProviderRendersExactUTF8AsOneGenericUnit(t *testing.T) {
	provider, err := New(Profile{MaxDocumentBytes: 1024})
	require.NoError(t, err)
	source := []byte("alpha\nβeta\n")
	upload := newTestUpload(source)
	authorization := testAuthorization(provider.Descriptor(), upload.Metadata())

	result, err := document.RenderRendition(t.Context(), provider, upload, authorization)
	require.NoError(t, err)
	require.Len(t, result.Evidence.Units, 1)
	assert.Equal(t, string(source), result.Evidence.Units[0].Text)
	assert.Equal(t, document.EvidenceUnitGeneric, result.Evidence.UnitKind)
	assert.Equal(t, document.EvidenceLocatorGeneric, result.Evidence.Units[0].Locator.Kind)
	assert.Equal(t, document.EvidenceDegradedProvenance, result.Evidence.Completeness)
	assert.Equal(t, int64(len(source)), result.Receipt.Usage.InputBytes)
	assert.Equal(t, authorization.SourceSHA256, result.Receipt.SourceSHA256)

	policy, err := document.NewEvidencePolicy(1024)
	require.NoError(t, err)
	normalized, err := document.NormalizeEvidenceV1(result.Evidence, policy)
	require.NoError(t, err)
	_, checksum, err := document.MarshalNormalizedEvidenceV1(normalized)
	require.NoError(t, err)
	assert.Equal(t, "unit_2bd7c59368d78ac033cd5ca2b3cf879c4023c4564f5b4f3b05548f3414e4271f",
		normalized.Units[0].ID)
	assert.Equal(t, "a7242bb9ba14c427bfafabdc43e8171ea58c839d7bfec2f26ec802c7406939c7", checksum)
}

func TestProviderRejectsInvalidUTF8AndNUL(t *testing.T) {
	provider, err := New(Profile{MaxDocumentBytes: 1024})
	require.NoError(t, err)
	for _, testCase := range []struct {
		name string
		data []byte
	}{
		{name: "invalid UTF-8", data: []byte{0xff, 0xfe}},
		{name: "NUL", data: []byte("alpha\x00beta")},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			upload := newTestUpload(testCase.data)
			_, err := provider.Render(t.Context(), upload,
				testAuthorization(provider.Descriptor(), upload.Metadata()))
			require.Error(t, err)
			providerErr, ok := errors.AsType[*document.RenditionProviderError](err)
			require.True(t, ok)
			assert.Equal(t, document.RenditionErrorUnsupportedInput, providerErr.Code())
		})
	}
}

func TestProviderRejectsEmptyInputBeforeReading(t *testing.T) {
	provider, err := New(Profile{MaxDocumentBytes: 1024})
	require.NoError(t, err)
	upload := newTestUpload(nil)
	authorization := testAuthorization(provider.Descriptor(), upload.Metadata())

	_, err = document.RenderRendition(t.Context(), provider, upload, authorization)
	require.ErrorContains(t, err, "byte length")
	assert.Zero(t, upload.reads)
}

func TestProviderEnforcesProfileSizeBeforeReading(t *testing.T) {
	provider, err := New(Profile{MaxDocumentBytes: 4})
	require.NoError(t, err)
	upload := newTestUpload([]byte("alpha"))

	_, err = provider.Render(t.Context(), upload,
		testAuthorization(provider.Descriptor(), upload.Metadata()))
	require.Error(t, err)
	providerErr, ok := errors.AsType[*document.RenditionProviderError](err)
	require.True(t, ok)
	assert.Equal(t, document.RenditionErrorPolicyRejected, providerErr.Code())
	assert.Zero(t, upload.reads)
}

func TestProviderHonorsCancellationBeforeReading(t *testing.T) {
	provider, err := New(Profile{MaxDocumentBytes: 1024})
	require.NoError(t, err)
	upload := newTestUpload([]byte("alpha"))
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	_, err = provider.Render(ctx, upload,
		testAuthorization(provider.Descriptor(), upload.Metadata()))
	require.ErrorIs(t, err, context.Canceled)
	providerErr, ok := errors.AsType[*document.RenditionProviderError](err)
	require.True(t, ok)
	assert.Equal(t, document.RenditionErrorCanceled, providerErr.Code())
	assert.Zero(t, upload.reads)
}

func TestProviderRejectsUploadByteSubstitution(t *testing.T) {
	provider, err := New(Profile{MaxDocumentBytes: 1024})
	require.NoError(t, err)
	upload := newTestUpload([]byte("omega"))
	metadata := upload.Metadata()
	want := sha256.Sum256([]byte("alpha"))
	metadata.SHA256 = hex.EncodeToString(want[:])
	upload.metadata = metadata

	_, err = provider.Render(t.Context(), upload,
		testAuthorization(provider.Descriptor(), upload.Metadata()))
	require.Error(t, err)
	providerErr, ok := errors.AsType[*document.RenditionProviderError](err)
	require.True(t, ok)
	assert.Equal(t, document.RenditionErrorPolicyRejected, providerErr.Code())
}

func TestNewRejectsInvalidBoundsAndDescriptorIsImmutable(t *testing.T) {
	_, err := New(Profile{})
	require.ErrorContains(t, err, "max document bytes")
	_, err = New(Profile{MaxDocumentBytes: MaxDocumentBytes + 1})
	require.ErrorContains(t, err, "max document bytes")

	provider, err := New(Profile{MaxDocumentBytes: 1024})
	require.NoError(t, err)
	descriptor := provider.Descriptor()
	assert.Equal(t, document.RenditionTrustLocalProcess, descriptor.TrustBoundary)
	assert.True(t, descriptor.ReturnsStructured)
	assert.False(t, descriptor.ReturnsMarkdown)
	require.NotEmpty(t, descriptor.SupportedFormats)
	descriptor.SupportedFormats[0].MediaType = "application/pdf"
	assert.NotEqual(t, descriptor.SupportedFormats, provider.Descriptor().SupportedFormats)
}

type testUpload struct {
	reader   *bytes.Reader
	metadata document.AuthorizedUploadMetadata
	reads    int
}

func newTestUpload(data []byte) *testUpload {
	digest := sha256.Sum256(data)
	return &testUpload{
		reader: bytes.NewReader(data),
		metadata: document.AuthorizedUploadMetadata{
			Filename: "notes.txt", MediaFamily: "text", MediaType: "text/plain",
			ByteLength: int64(len(data)), SHA256: hex.EncodeToString(digest[:]),
			CapabilityRecordChecksum: strings.Repeat("2", 64),
			ProviderMetadataChecksum: strings.Repeat("3", 64),
			InputKind:                document.RenditionInputOriginalFile,
		},
	}
}

func (upload *testUpload) Read(buffer []byte) (int, error) {
	upload.reads++
	read, err := upload.reader.Read(buffer)
	if err != nil {
		return read, fmt.Errorf("read test upload: %w", err)
	}
	return read, nil
}

func (*testUpload) Close() error { return nil }

func (upload *testUpload) Metadata() document.AuthorizedUploadMetadata { return upload.metadata }

var _ document.AuthorizedUpload = (*testUpload)(nil)
var _ io.ReadCloser = (*testUpload)(nil)

func testAuthorization(
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
