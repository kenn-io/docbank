package document

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type syntheticRenditionProvider struct {
	descriptor RenditionDescriptor
	result     RenditionResult
	err        error
}

func (provider syntheticRenditionProvider) Descriptor() RenditionDescriptor {
	return provider.descriptor
}

func (provider syntheticRenditionProvider) Render(
	context.Context, AuthorizedUpload, RenditionAuthorization,
) (RenditionResult, error) {
	return provider.result, provider.err
}

var _ RenditionProvider = syntheticRenditionProvider{}

type syntheticAuthorizedUpload struct {
	io.ReadCloser

	metadata AuthorizedUploadMetadata
}

func (upload *syntheticAuthorizedUpload) Metadata() AuthorizedUploadMetadata {
	return upload.metadata
}

var _ AuthorizedUpload = (*syntheticAuthorizedUpload)(nil)

type changingRenditionProvider struct {
	descriptors []RenditionDescriptor
	calls       int
}

type mutatingRenditionProvider struct {
	descriptor       RenditionDescriptor
	render           func(RenditionAuthorization) RenditionResult
	mutateDescriptor bool
	calls            int
}

func (provider *mutatingRenditionProvider) Descriptor() RenditionDescriptor {
	provider.calls++
	if provider.mutateDescriptor && provider.calls == 2 {
		provider.descriptor.ArtifactRoles[0] = EvidenceArtifactImage
	}
	return provider.descriptor
}

func (provider *mutatingRenditionProvider) Render(
	_ context.Context, _ AuthorizedUpload, authorization RenditionAuthorization,
) (RenditionResult, error) {
	return provider.render(authorization), nil
}

func (provider *changingRenditionProvider) Descriptor() RenditionDescriptor {
	descriptor := provider.descriptors[provider.calls]
	provider.calls++
	return descriptor
}

func (*changingRenditionProvider) Render(
	context.Context, AuthorizedUpload, RenditionAuthorization,
) (RenditionResult, error) {
	return RenditionResult{}, nil
}

type changingAuthorizedUpload struct {
	io.ReadCloser

	metadata []AuthorizedUploadMetadata
	calls    int
}

func (upload *changingAuthorizedUpload) Metadata() AuthorizedUploadMetadata {
	metadata := upload.metadata[upload.calls]
	upload.calls++
	return metadata
}

func TestRenditionProviderContractAcceptsExactBoundedResult(t *testing.T) {
	descriptor := validRenditionDescriptor(t)
	metadata := validAuthorizedUploadMetadata()
	authorization := validRenditionAuthorization(descriptor, metadata)
	result := validRenditionResult(descriptor, authorization)
	provider := syntheticRenditionProvider{descriptor: descriptor, result: result}
	upload := &syntheticAuthorizedUpload{
		ReadCloser: io.NopCloser(strings.NewReader("synthetic exact source")), metadata: metadata,
	}

	validated, err := ValidateRenditionProviderRequest(provider, upload, authorization)
	require.NoError(t, err)
	assert.Equal(t, descriptor, validated)
	produced, err := provider.Render(t.Context(), upload, authorization)
	require.NoError(t, err)
	require.NoError(t, ValidateRenditionResult(validated, authorization, produced))
}

func TestRenditionProviderContractRejectsInvalidDescriptorAndAuthorization(t *testing.T) {
	metadata := validAuthorizedUploadMetadata()
	for _, testCase := range []struct {
		name   string
		mutate func(*RenditionDescriptor, *RenditionAuthorization)
		want   string
	}{
		{
			name: "empty descriptor identity",
			mutate: func(descriptor *RenditionDescriptor, _ *RenditionAuthorization) {
				descriptor.ID = ""
			},
			want: "descriptor ID",
		},
		{
			name: "unsupported format",
			mutate: func(_ *RenditionDescriptor, authorization *RenditionAuthorization) {
				authorization.MediaFamily = "image"
				authorization.MediaType = "image/png"
			},
			want: "unsupported format",
		},
		{
			name: "mismatched descriptor authorization",
			mutate: func(_ *RenditionDescriptor, authorization *RenditionAuthorization) {
				authorization.DescriptorFingerprint = strings.Repeat("9", 64)
			},
			want: "descriptor fingerprint",
		},
		{
			name: "oversized result declaration",
			mutate: func(_ *RenditionDescriptor, authorization *RenditionAuthorization) {
				authorization.MaxTotalResultBytes = math.MaxInt64
			},
			want: "total result bytes",
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			descriptor := validRenditionDescriptor(t)
			authorization := validRenditionAuthorization(descriptor, metadata)
			testCase.mutate(&descriptor, &authorization)
			provider := syntheticRenditionProvider{descriptor: descriptor}
			upload := &syntheticAuthorizedUpload{
				ReadCloser: io.NopCloser(bytes.NewReader(nil)), metadata: metadata,
			}
			_, err := ValidateRenditionProviderRequest(provider, upload, authorization)
			require.ErrorContains(t, err, testCase.want)
		})
	}
}

func TestRenditionProviderContractRejectsDuplicateAndOversizedOutputs(t *testing.T) {
	descriptor := validRenditionDescriptor(t)
	metadata := validAuthorizedUploadMetadata()
	authorization := validRenditionAuthorization(descriptor, metadata)
	for _, testCase := range []struct {
		name   string
		mutate func(*RenditionResult)
		want   string
	}{
		{
			name: "duplicate artifact role",
			mutate: func(result *RenditionResult) {
				result.Artifacts = append(result.Artifacts, result.Artifacts[0])
			},
			want: "artifact role",
		},
		{
			name: "oversized provider Markdown",
			mutate: func(result *RenditionResult) {
				result.ProviderMarkdown = bytes.Repeat([]byte("x"), authorization.MaxProviderMarkdownBytes+1)
			},
			want: "provider Markdown",
		},
		{
			name: "artifact checksum mismatch",
			mutate: func(result *RenditionResult) {
				result.Artifacts[0].SHA256 = strings.Repeat("0", 64)
			},
			want: "artifact checksum",
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			result := validRenditionResult(descriptor, authorization)
			testCase.mutate(&result)
			require.ErrorContains(t,
				ValidateRenditionResult(descriptor, authorization, result), testCase.want)
		})
	}
}

func TestRenditionProviderContractRejectsUnauthorizedOrUnmatchedEvidenceArtifacts(t *testing.T) {
	descriptor := validRenditionDescriptor(t)
	metadata := validAuthorizedUploadMetadata()
	authorization := validRenditionAuthorization(descriptor, metadata)

	result := validRenditionResult(descriptor, authorization)
	result.Evidence.Artifacts[0].Role = EvidenceArtifactImage
	require.ErrorContains(t, ValidateRenditionResult(descriptor, authorization, result),
		"not authorized")

	result = validRenditionResult(descriptor, authorization)
	result.Evidence.Artifacts[0].SHA256 = strings.Repeat("9", 64)
	require.ErrorContains(t, ValidateRenditionResult(descriptor, authorization, result),
		"does not match")

	result = validRenditionResult(descriptor, authorization)
	result.Evidence.Artifacts = nil
	require.ErrorContains(t, ValidateRenditionResult(descriptor, authorization, result),
		"absent from source evidence")
}

func TestRenditionProviderContractRejectsUnsafeReceiptFields(t *testing.T) {
	descriptor := validRenditionDescriptor(t)
	metadata := validAuthorizedUploadMetadata()
	authorization := validRenditionAuthorization(descriptor, metadata)
	for _, testCase := range []struct {
		name   string
		mutate func(*RenditionReceipt)
		want   string
	}{
		{
			name: "provider body in warning",
			mutate: func(receipt *RenditionReceipt) {
				receipt.Warnings = []string{"raw body: {\"document\":\"secret\"}"}
			},
			want: "warning",
		},
		{
			name: "credential-shaped operation identity",
			mutate: func(receipt *RenditionReceipt) {
				receipt.OperationID = "Authorization: Bearer secret"
			},
			want: "operation ID",
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			result := validRenditionResult(descriptor, authorization)
			testCase.mutate(&result.Receipt)
			require.ErrorContains(t,
				ValidateRenditionResult(descriptor, authorization, result), testCase.want)
		})
	}
}

func TestRenditionProviderContractRejectsUnclassifiedErrors(t *testing.T) {
	require.ErrorContains(t,
		ValidateRenditionProviderError(errors.New("raw provider body with secret")),
		"unclassified",
	)
	cause := errors.New("raw provider body with secret")
	providerError, err := NewRenditionProviderError(
		RenditionErrorRateLimited, "provider rate limit", 30*time.Second, cause,
	)
	require.NoError(t, err)
	require.NoError(t, ValidateRenditionProviderError(providerError))
	assert.True(t, IsRenditionProviderErrorRetryable(providerError))
	wrapped := fmt.Errorf("unsafe wrapper includes provider body: %w", providerError)
	require.ErrorContains(t, ValidateRenditionProviderError(wrapped), "unclassified")
	assert.False(t, IsRenditionProviderErrorRetryable(wrapped))
	assert.NotContains(t, providerError.Error(), "secret")
	assert.ErrorIs(t, providerError, cause)
}

func TestRenditionProviderContractRejectsExpiredAuthorizationAndOutOfWindowReceipt(t *testing.T) {
	descriptor := validRenditionDescriptor(t)
	metadata := validAuthorizedUploadMetadata()
	upload := &syntheticAuthorizedUpload{
		ReadCloser: io.NopCloser(bytes.NewReader([]byte("synthetic exact source"))), metadata: metadata,
	}
	provider := syntheticRenditionProvider{descriptor: descriptor}
	authorization := validRenditionAuthorization(descriptor, metadata)
	now, err := parseRenditionTimestamp(authorization.ExpiresAt)
	require.NoError(t, err)
	_, err = ValidateRenditionProviderRequestAt(now, provider, upload, authorization)
	require.ErrorContains(t, err, "not current")
	authorizedAt, err := parseRenditionTimestamp(authorization.AuthorizedAt)
	require.NoError(t, err)
	_, err = ValidateRenditionProviderRequestAt(
		authorizedAt.Add(-time.Nanosecond),
		provider, upload, authorization)
	require.ErrorContains(t, err, "not current")

	result := validRenditionResult(descriptor, authorization)
	result.Receipt.StartedAt = authorizedAt.Add(-time.Nanosecond).Format(renditionTimestampForm)
	require.ErrorContains(t, ValidateRenditionResult(descriptor, authorization, result),
		"outside the authorization interval")
	result = validRenditionResult(descriptor, authorization)
	result.Receipt.CompletedAt = now.Add(time.Nanosecond).Format(renditionTimestampForm)
	require.ErrorContains(t, ValidateRenditionResult(descriptor, authorization, result),
		"outside the authorization interval")
}

func TestRenditionDescriptorCanonicalizesAndOwnsCollections(t *testing.T) {
	formats := []RenditionFormatCapability{
		{MediaFamily: "text", MediaType: "text/plain", InputKind: RenditionInputOriginalFile},
		{MediaFamily: "pdf", MediaType: "application/pdf", InputKind: RenditionInputOriginalFile},
	}
	roles := []EvidenceArtifactRole{EvidenceArtifactTranscript, EvidenceArtifactStructured}
	descriptor, err := NewRenditionDescriptor(RenditionDescriptor{
		ID: "synthetic-rendition", ContractVersion: RenditionProviderContractVersion,
		PolicyFingerprint: strings.Repeat("1", 64), TrustBoundary: RenditionTrustHostedProvider,
		SupportedFormats: formats, ReturnsStructured: true, ArtifactRoles: roles,
	})
	require.NoError(t, err)
	formats[0].MediaFamily = "mutated"
	roles[0] = EvidenceArtifactImage
	assert.Equal(t, "pdf", descriptor.SupportedFormats[0].MediaFamily)
	assert.Equal(t, EvidenceArtifactTranscript, descriptor.ArtifactRoles[0])

	reordered, err := NewRenditionDescriptor(RenditionDescriptor{
		ID: "synthetic-rendition", ContractVersion: RenditionProviderContractVersion,
		PolicyFingerprint: strings.Repeat("1", 64), TrustBoundary: RenditionTrustHostedProvider,
		SupportedFormats: []RenditionFormatCapability{
			{MediaFamily: "pdf", MediaType: "application/pdf", InputKind: RenditionInputOriginalFile},
			{MediaFamily: "text", MediaType: "text/plain", InputKind: RenditionInputOriginalFile},
		},
		ReturnsStructured: true,
		ArtifactRoles:     []EvidenceArtifactRole{EvidenceArtifactStructured, EvidenceArtifactTranscript},
	})
	require.NoError(t, err)
	assert.Equal(t, descriptor, reordered)
}

func TestRenditionProviderContractRejectsMutableBoundarySnapshots(t *testing.T) {
	descriptor := validRenditionDescriptor(t)
	metadata := validAuthorizedUploadMetadata()
	authorization := validRenditionAuthorization(descriptor, metadata)

	changedDescriptor := descriptor
	changedDescriptor.ID = "changed-provider"
	provider := &changingRenditionProvider{descriptors: []RenditionDescriptor{descriptor, changedDescriptor}}
	upload := &syntheticAuthorizedUpload{ReadCloser: io.NopCloser(bytes.NewReader(nil)), metadata: metadata}
	_, err := ValidateRenditionProviderRequest(provider, upload, authorization)
	require.ErrorContains(t, err, "descriptor changed")

	changedMetadata := metadata
	changedMetadata.Filename = "changed.pdf"
	provider = &changingRenditionProvider{descriptors: []RenditionDescriptor{descriptor, descriptor}}
	changingUpload := &changingAuthorizedUpload{
		ReadCloser: io.NopCloser(bytes.NewReader(nil)),
		metadata:   []AuthorizedUploadMetadata{metadata, changedMetadata},
	}
	_, err = ValidateRenditionProviderRequest(provider, changingUpload, authorization)
	require.ErrorContains(t, err, "upload metadata changed")
}

func TestRenditionProviderContractFreezesAliasedDescriptorSlices(t *testing.T) {
	descriptor := validRenditionDescriptor(t)
	metadata := validAuthorizedUploadMetadata()
	authorization := validRenditionAuthorization(descriptor, metadata)
	provider := &mutatingRenditionProvider{descriptor: descriptor, mutateDescriptor: true}
	upload := &syntheticAuthorizedUpload{
		ReadCloser: io.NopCloser(bytes.NewReader(nil)), metadata: metadata,
	}

	_, err := ValidateRenditionProviderRequest(provider, upload, authorization)
	require.ErrorContains(t, err, "descriptor changed")
}

func TestRenderRenditionKeepsSealedAuthorizationSeparateFromProvider(t *testing.T) {
	descriptor := validRenditionDescriptor(t)
	metadata := validAuthorizedUploadMetadata()
	authorization := validRenditionAuthorization(descriptor, metadata)
	provider := &mutatingRenditionProvider{descriptor: descriptor}
	provider.render = func(received RenditionAuthorization) RenditionResult {
		received.AllowedArtifactRoles[0] = EvidenceArtifactImage
		result := validRenditionResult(descriptor, received)
		result.Artifacts[0].Role = EvidenceArtifactImage
		result.Evidence.Artifacts[0].Role = EvidenceArtifactImage
		return result
	}
	upload := &syntheticAuthorizedUpload{
		ReadCloser: io.NopCloser(strings.NewReader("synthetic exact source")), metadata: metadata,
	}

	_, err := RenderRendition(t.Context(), provider, upload, authorization)
	require.ErrorContains(t, err, "not authorized")
	assert.Equal(t, []EvidenceArtifactRole{EvidenceArtifactStructured}, authorization.AllowedArtifactRoles,
		"provider mutation must not reach the caller or sealed validation snapshot")
}

func TestRenditionProviderContractRejectsTypedNilBoundaryValues(t *testing.T) {
	descriptor := validRenditionDescriptor(t)
	metadata := validAuthorizedUploadMetadata()
	authorization := validRenditionAuthorization(descriptor, metadata)

	var provider *changingRenditionProvider
	upload := &syntheticAuthorizedUpload{ReadCloser: io.NopCloser(bytes.NewReader(nil)), metadata: metadata}
	assert.NotPanics(t, func() {
		_, err := ValidateRenditionProviderRequest(provider, upload, authorization)
		require.ErrorContains(t, err, "provider is required")
	})

	validProvider := syntheticRenditionProvider{descriptor: descriptor}
	var nilUpload *syntheticAuthorizedUpload
	assert.NotPanics(t, func() {
		_, err := ValidateRenditionProviderRequest(validProvider, nilUpload, authorization)
		require.ErrorContains(t, err, "upload is required")
	})
}

func validRenditionDescriptor(t *testing.T) RenditionDescriptor {
	t.Helper()
	descriptor, err := NewRenditionDescriptor(RenditionDescriptor{
		ID:                "synthetic-rendition",
		ContractVersion:   RenditionProviderContractVersion,
		PolicyFingerprint: strings.Repeat("1", 64),
		TrustBoundary:     RenditionTrustHostedProvider,
		SupportedFormats: []RenditionFormatCapability{{
			MediaFamily: "pdf", MediaType: "application/pdf", InputKind: RenditionInputOriginalFile,
		}},
		ReturnsMarkdown:   true,
		ReturnsStructured: true,
		ArtifactRoles:     []EvidenceArtifactRole{EvidenceArtifactStructured},
	})
	require.NoError(t, err)
	return descriptor
}

func validAuthorizedUploadMetadata() AuthorizedUploadMetadata {
	source := []byte("synthetic exact source")
	digest := sha256.Sum256(source)
	return AuthorizedUploadMetadata{
		Filename:                 "document.pdf",
		MediaFamily:              "pdf",
		MediaType:                "application/pdf",
		ByteLength:               int64(len(source)),
		SHA256:                   hex.EncodeToString(digest[:]),
		CapabilityRecordChecksum: strings.Repeat("2", 64),
		ProviderMetadataChecksum: strings.Repeat("3", 64),
		InputKind:                RenditionInputOriginalFile,
	}
}

func validRenditionAuthorization(
	descriptor RenditionDescriptor, metadata AuthorizedUploadMetadata,

) RenditionAuthorization {
	authorizedAt := time.Now().UTC().Add(-time.Minute)
	expiresAt := authorizedAt.Add(10 * time.Minute)
	return RenditionAuthorization{
		ProviderID: descriptor.ID, DescriptorFingerprint: descriptor.Fingerprint,
		PolicyFingerprint:           descriptor.PolicyFingerprint,
		RenditionRequestFingerprint: strings.Repeat("4", 64),
		SourceSHA256:                metadata.SHA256, SourceBytes: metadata.ByteLength,
		CapabilityRecordChecksum: metadata.CapabilityRecordChecksum,
		ProviderMetadataChecksum: metadata.ProviderMetadataChecksum,
		MediaFamily:              metadata.MediaFamily, MediaType: metadata.MediaType, InputKind: metadata.InputKind,
		AllowedArtifactRoles:     []EvidenceArtifactRole{EvidenceArtifactStructured},
		MaxProviderMarkdownBytes: 1_024, MaxArtifactBytes: 1_024,
		MaxArtifacts: 1, MaxTotalResultBytes: 4_096,
		AuthorizedAt: authorizedAt.Format(renditionTimestampForm),
		ExpiresAt:    expiresAt.Format(renditionTimestampForm),
	}
}

func validRenditionResult(
	descriptor RenditionDescriptor, authorization RenditionAuthorization,
) RenditionResult {
	authorizedAt, _ := parseRenditionTimestamp(authorization.AuthorizedAt)
	payload := []byte(`{"synthetic":"structured"}`)
	digest := sha256.Sum256(payload)
	return RenditionResult{
		Evidence: SourceEvidenceV1{
			ContractVersion: SourceEvidenceContractV1,
			Completeness:    EvidenceDegradedProvenance,
			Family:          "pdf",
			Artifacts: []SourceEvidenceArtifactV1{{
				ProviderID: "provider-artifact-1", Pointer: "provider/structured.json",
				Role: EvidenceArtifactStructured, SHA256: hex.EncodeToString(digest[:]),
			}},
			UnitKind: EvidenceUnitGeneric,
			Omissions: []SourceEvidenceOmissionV1{{
				Kind: EvidenceOmissionField, Field: "natural_provenance",
				Reason: "synthetic provider returned generic evidence",
			}},
			Units: []SourceEvidenceUnitV1{{
				Order: 0, Text: "synthetic evidence",
				Locator: SourceEvidenceLocatorV1{
					Kind: EvidenceLocatorGeneric, IndexOrigin: EvidenceIndexOriginNone,
				},
			}},
		},
		ProviderMarkdown: []byte("synthetic evidence\n"),
		Artifacts: []RenditionArtifact{{
			Role: EvidenceArtifactStructured, MediaType: "application/json",
			Payload: payload, SHA256: hex.EncodeToString(digest[:]),
		}},
		Receipt: RenditionReceipt{
			ProviderID: descriptor.ID, DescriptorFingerprint: descriptor.Fingerprint,
			PolicyFingerprint: authorization.PolicyFingerprint,
			SourceSHA256:      authorization.SourceSHA256,
			OperationID:       "operation-synthetic-1",
			StartedAt:         authorizedAt.Add(time.Second).Format(renditionTimestampForm),
			CompletedAt:       authorizedAt.Add(2 * time.Second).Format(renditionTimestampForm),
			Warnings:          []string{"degraded_provenance"},
			Usage: RenditionUsage{
				Requests: 1, InputBytes: authorization.SourceBytes,
				OutputBytes: int64(len(payload) + len("synthetic evidence\n")), Units: 1,
			},
		},
	}
}
