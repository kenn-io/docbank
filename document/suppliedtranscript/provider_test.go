package suppliedtranscript

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
	"go.kenn.io/docbank/document/media/mediatest"
)

func TestProviderRendersTranscriptForAuthorizedAudioDigest(t *testing.T) {
	var gotDigest string
	processingProfile := testProcessingProfile(100_000)
	transcript := document.SuppliedTranscript{Provider: "beeper", Text: "The shipment arrives at dock seven."}
	provider, err := New(Profile{ProcessingProfile: processingProfile, Source: sourceFunc(func(_ context.Context, digest string) (document.SuppliedTranscript, error) {
		gotDigest = digest
		return transcript, nil
	})})
	require.NoError(t, err)

	upload := newTestUpload(mediatest.WAV())
	result, err := document.RenderRendition(t.Context(), provider, upload,
		testAuthorization(provider.Descriptor(), upload.Metadata()))
	require.NoError(t, err)

	assert.Equal(t, upload.metadata.SHA256, gotDigest)
	assert.Equal(t, transcript.Text, result.Evidence.Units[0].Text)
	assert.Equal(t, document.EvidenceDegradedProvenance, result.Evidence.Completeness)
	require.Len(t, result.Artifacts, 1)
	assert.Equal(t, document.EvidenceArtifactTranscript, result.Artifacts[0].Role)
	assert.Equal(t, result.Artifacts[0].SHA256, result.Evidence.Artifacts[0].SHA256)
	assert.Equal(t, upload.metadata.SHA256, result.Receipt.SourceSHA256)
	assert.Equal(t, int64(len(upload.data)), result.Receipt.Usage.InputBytes)
	evidencePolicy, err := document.NewEvidencePolicyForProcessingProfile(processingProfile)
	require.NoError(t, err)
	expectedEvidence, expectedArtifact, err := document.BuildTranscriptSourceEvidenceV1(transcript, evidencePolicy)
	require.NoError(t, err)
	assert.Equal(t, expectedEvidence, result.Evidence)
	assert.Equal(t, expectedArtifact.Payload, result.Artifacts[0].Payload)
	assert.Equal(t, expectedArtifact.SHA256, result.Artifacts[0].SHA256)
}

func TestProviderDescriptorIsBoundedAndImmutable(t *testing.T) {
	provider, err := New(Profile{ProcessingProfile: testProcessingProfile(100_000), Source: sourceFunc(func(context.Context, string) (document.SuppliedTranscript, error) {
		return document.SuppliedTranscript{Provider: "beeper", Text: "text"}, nil
	})})
	require.NoError(t, err)

	descriptor := provider.Descriptor()
	assert.Equal(t, document.RenditionTrustLocalProcess, descriptor.TrustBoundary)
	assert.True(t, descriptor.ReturnsStructured)
	assert.False(t, descriptor.ReturnsMarkdown)
	assert.Equal(t, []document.RenditionFormatCapability{
		{MediaFamily: "audio", MediaType: "audio/mpeg", InputKind: document.RenditionInputOriginalFile},
		{MediaFamily: "audio", MediaType: "audio/wav", InputKind: document.RenditionInputOriginalFile},
	}, descriptor.SupportedFormats)
	assert.Equal(t, []document.EvidenceArtifactRole{document.EvidenceArtifactTranscript}, descriptor.ArtifactRoles)

	descriptor.SupportedFormats[0].MediaType = "application/pdf"
	descriptor.ArtifactRoles[0] = document.EvidenceArtifactStructured
	assert.NotEqual(t, descriptor.SupportedFormats, provider.Descriptor().SupportedFormats)
	assert.NotEqual(t, descriptor.ArtifactRoles, provider.Descriptor().ArtifactRoles)
}

func TestProviderDescriptorIncludesDeploymentFingerprint(t *testing.T) {
	firstProfile := testProcessingProfile(100_000)
	firstProfile.Rendition.DeploymentFingerprint = strings.Repeat("a", 64)
	secondProfile := firstProfile
	bindingCopy := *firstProfile.Rendition
	secondProfile.Rendition = &bindingCopy
	secondProfile.Rendition.DeploymentFingerprint = strings.Repeat("b", 64)

	first, err := New(Profile{ProcessingProfile: firstProfile, Source: sourceFunc(func(context.Context, string) (document.SuppliedTranscript, error) {
		return document.SuppliedTranscript{Provider: "beeper", Text: "first"}, nil
	})})
	require.NoError(t, err)
	second, err := New(Profile{ProcessingProfile: secondProfile, Source: sourceFunc(func(context.Context, string) (document.SuppliedTranscript, error) {
		return document.SuppliedTranscript{Provider: "beeper", Text: "second"}, nil
	})})
	require.NoError(t, err)

	assert.NotEqual(t, first.Descriptor().Fingerprint, second.Descriptor().Fingerprint)
	assert.Equal(t, firstProfile.Rendition.DeploymentFingerprint, first.DeploymentFingerprint())
	assert.Equal(t, secondProfile.Rendition.DeploymentFingerprint, second.DeploymentFingerprint())
}

func TestProviderRequiresRenditionBinding(t *testing.T) {
	profile := testProcessingProfile(100_000)
	profile.Rendition = nil
	_, err := New(Profile{ProcessingProfile: profile, Source: sourceFunc(func(context.Context, string) (document.SuppliedTranscript, error) {
		return document.SuppliedTranscript{}, nil
	})})
	require.ErrorContains(t, err, "rendition binding is required")
}

func TestProviderAcceptsDeclaredProcessingProfileBoundaries(t *testing.T) {
	for _, test := range []struct {
		name          string
		maxUnitRunes  int
		transcriptLen int
	}{
		{name: "minimum", maxUnitRunes: 1, transcriptLen: 1},
		{name: "configured maximum", maxUnitRunes: 100_000, transcriptLen: 100_000},
	} {
		t.Run(test.name, func(t *testing.T) {
			processingProfile := testProcessingProfile(test.maxUnitRunes)
			transcript := document.SuppliedTranscript{Provider: "beeper", Text: strings.Repeat("a", test.transcriptLen)}
			provider, err := New(Profile{
				ProcessingProfile: processingProfile,
				Source: sourceFunc(func(context.Context, string) (document.SuppliedTranscript, error) {
					return transcript, nil
				}),
			})
			require.NoError(t, err)

			upload := newTestUpload(mediatest.WAV())
			result, err := document.RenderRendition(t.Context(), provider, upload,
				testAuthorization(provider.Descriptor(), upload.Metadata()))
			require.NoError(t, err)
			require.Len(t, result.Evidence.Units, 1)
			assert.Equal(t, transcript.Text, result.Evidence.Units[0].Text)
		})
	}
}

func TestProviderReturnsExistingClassifiedResolverError(t *testing.T) {
	resolverErr, err := document.NewRenditionProviderError(document.RenditionErrorTransient, "", time.Second, nil)
	require.NoError(t, err)
	provider, err := New(Profile{ProcessingProfile: testProcessingProfile(100_000), Source: sourceFunc(func(context.Context, string) (document.SuppliedTranscript, error) {
		return document.SuppliedTranscript{}, resolverErr
	})})
	require.NoError(t, err)
	upload := newTestUpload(mediatest.WAV())

	_, gotErr := provider.Render(t.Context(), upload, testAuthorization(provider.Descriptor(), upload.Metadata()))
	assert.Same(t, resolverErr, gotErr)
	assert.True(t, document.IsRenditionProviderErrorRetryable(gotErr))
}

func TestProviderReportsMissingTranscriptAsUnsupportedInput(t *testing.T) {
	provider, err := New(Profile{ProcessingProfile: testProcessingProfile(100_000), Source: sourceFunc(func(context.Context, string) (document.SuppliedTranscript, error) {
		return document.SuppliedTranscript{}, nil
	})})
	require.NoError(t, err)
	upload := newTestUpload(mediatest.WAV())

	_, err = document.RenderRendition(t.Context(), provider, upload,
		testAuthorization(provider.Descriptor(), upload.Metadata()))
	providerErr, ok := errors.AsType[*document.RenditionProviderError](err)
	require.True(t, ok)
	assert.Equal(t, document.RenditionErrorUnsupportedInput, providerErr.Code())
}

func TestProviderUsesDeclaredProcessingProfileBoundary(t *testing.T) {
	processingProfile := testProcessingProfile(4)
	provider, err := New(Profile{
		ProcessingProfile: processingProfile,
		Source: sourceFunc(func(context.Context, string) (document.SuppliedTranscript, error) {
			return document.SuppliedTranscript{Provider: "beeper", Text: "five!"}, nil
		}),
	})
	require.NoError(t, err)
	upload := newTestUpload(mediatest.WAV())

	_, err = document.RenderRendition(t.Context(), provider, upload,
		testAuthorization(provider.Descriptor(), upload.Metadata()))
	providerErr, ok := errors.AsType[*document.RenditionProviderError](err)
	require.True(t, ok)
	assert.Equal(t, document.RenditionErrorMalformedEvidence, providerErr.Code())
}

func TestProviderRejectsWrongAuthorizedDigestBeforeSourceExecution(t *testing.T) {
	called := false
	provider, err := New(Profile{ProcessingProfile: testProcessingProfile(100_000), Source: sourceFunc(func(context.Context, string) (document.SuppliedTranscript, error) {
		called = true
		return document.SuppliedTranscript{Provider: "beeper", Text: "text"}, nil
	})})
	require.NoError(t, err)
	upload := newTestUpload(mediatest.WAV())
	authorization := testAuthorization(provider.Descriptor(), upload.Metadata())
	authorization.SourceSHA256 = strings.Repeat("0", 64)

	_, err = document.RenderRendition(t.Context(), provider, upload, authorization)
	require.Error(t, err)
	require.ErrorContains(t, err, "authorization does not match exact upload bytes")
	assert.False(t, called)
}

type sourceFunc func(context.Context, string) (document.SuppliedTranscript, error)

func (source sourceFunc) Transcript(ctx context.Context, digest string) (document.SuppliedTranscript, error) {
	return source(ctx, digest)
}

type testUpload struct {
	data     []byte
	reader   *bytes.Reader
	metadata document.AuthorizedUploadMetadata
}

func newTestUpload(data []byte) *testUpload {
	digest := sha256.Sum256(data)
	return &testUpload{data: data, reader: bytes.NewReader(data), metadata: document.AuthorizedUploadMetadata{
		Filename: "voice.wav", MediaFamily: "audio", MediaType: "audio/wav",
		ByteLength: int64(len(data)), SHA256: hex.EncodeToString(digest[:]),
		CapabilityRecordChecksum: strings.Repeat("2", 64), ProviderMetadataChecksum: strings.Repeat("3", 64),
		InputKind: document.RenditionInputOriginalFile,
	}}
}

func (upload *testUpload) Read(buffer []byte) (int, error) {
	read, err := upload.reader.Read(buffer)
	if err != nil {
		return read, fmt.Errorf("read test upload: %w", err)
	}
	return read, nil
}
func (*testUpload) Close() error                                       { return nil }
func (upload *testUpload) Metadata() document.AuthorizedUploadMetadata { return upload.metadata }

var _ document.AuthorizedUpload = (*testUpload)(nil)
var _ io.ReadCloser = (*testUpload)(nil)

func testAuthorization(
	descriptor document.RenditionDescriptor, metadata document.AuthorizedUploadMetadata,
) document.RenditionAuthorization {
	started := time.Now().UTC().Add(-time.Minute)
	return document.RenditionAuthorization{
		ProviderID: descriptor.ID, DescriptorFingerprint: descriptor.Fingerprint,
		PolicyFingerprint: descriptor.PolicyFingerprint, RenditionRequestFingerprint: strings.Repeat("4", 64),
		SourceSHA256: metadata.SHA256, SourceBytes: metadata.ByteLength,
		CapabilityRecordChecksum: metadata.CapabilityRecordChecksum, ProviderMetadataChecksum: metadata.ProviderMetadataChecksum,
		MediaFamily: metadata.MediaFamily, MediaType: metadata.MediaType, InputKind: metadata.InputKind,
		AllowedArtifactRoles: []document.EvidenceArtifactRole{document.EvidenceArtifactTranscript},
		MaxArtifactBytes:     1 << 20, MaxArtifacts: 1, MaxTotalResultBytes: 1 << 20,
		AuthorizedAt: started.Format(timestampForm), ExpiresAt: started.Add(10 * time.Minute).Format(timestampForm),
	}
}

func testProcessingProfile(maxUnitRunes int) document.ProcessingProfileV1 {
	return document.ProcessingProfileV1{
		EvidenceLexical: document.EvidenceLexicalPolicyV1{MaxUnitRunes: maxUnitRunes},
		Rendition:       &document.RenditionBindingV1{DeploymentFingerprint: strings.Repeat("0", 64), MaxUnits: 1},
	}
}
