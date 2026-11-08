package plaintext

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/media/mediatest"
)

func TestProviderRendersExactUTF8AsOneGenericUnit(t *testing.T) {
	provider, err := New(Profile{MaxDocumentBytes: 1024})
	require.NoError(t, err)
	source := []byte("alpha\nβeta\n")
	upload := newTestUpload(source)
	authorization := mediatest.Authorization(provider.Descriptor(), upload.Metadata())

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
				mediatest.Authorization(provider.Descriptor(), upload.Metadata()))
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
	authorization := mediatest.Authorization(provider.Descriptor(), upload.Metadata())

	_, err = document.RenderRendition(t.Context(), provider, upload, authorization)
	require.ErrorContains(t, err, "byte length")
	assert.Zero(t, upload.Reads)
}

func TestProviderEnforcesProfileSizeBeforeReading(t *testing.T) {
	provider, err := New(Profile{MaxDocumentBytes: 4})
	require.NoError(t, err)
	upload := newTestUpload([]byte("alpha"))

	_, err = provider.Render(t.Context(), upload,
		mediatest.Authorization(provider.Descriptor(), upload.Metadata()))
	require.Error(t, err)
	providerErr, ok := errors.AsType[*document.RenditionProviderError](err)
	require.True(t, ok)
	assert.Equal(t, document.RenditionErrorPolicyRejected, providerErr.Code())
	assert.Zero(t, upload.Reads)
}

func TestProviderHonorsCancellationBeforeReading(t *testing.T) {
	provider, err := New(Profile{MaxDocumentBytes: 1024})
	require.NoError(t, err)
	upload := newTestUpload([]byte("alpha"))
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	_, err = provider.Render(ctx, upload,
		mediatest.Authorization(provider.Descriptor(), upload.Metadata()))
	require.ErrorIs(t, err, context.Canceled)
	providerErr, ok := errors.AsType[*document.RenditionProviderError](err)
	require.True(t, ok)
	assert.Equal(t, document.RenditionErrorCanceled, providerErr.Code())
	assert.Zero(t, upload.Reads)
}

func TestProviderRejectsUploadByteSubstitution(t *testing.T) {
	provider, err := New(Profile{MaxDocumentBytes: 1024})
	require.NoError(t, err)
	upload := newTestUpload([]byte("omega"))
	metadata := upload.Metadata()
	want := sha256.Sum256([]byte("alpha"))
	metadata.SHA256 = hex.EncodeToString(want[:])
	upload.MetadataValue = metadata

	_, err = provider.Render(t.Context(), upload,
		mediatest.Authorization(provider.Descriptor(), upload.Metadata()))
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

func newTestUpload(data []byte) *mediatest.Upload {
	return mediatest.NewUpload(data, document.AuthorizedUploadMetadata{Filename: "notes.txt", MediaFamily: "text", MediaType: "text/plain"})
}
