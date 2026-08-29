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
	"reflect"
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

type syntheticResumableRenditionProvider struct {
	syntheticRenditionProvider

	resume                *RenditionResumeHandle
	checkpoint            RenditionResumeCheckpoint
	calls                 int
	ignoreCheckpointError bool
	uploadWasNil          bool
}

func (provider *syntheticResumableRenditionProvider) RenderResumable(
	_ context.Context, upload AuthorizedUpload, _ RenditionAuthorization,
	resume *RenditionResumeHandle, checkpoint RenditionResumeCheckpoint,
) (RenditionResult, error) {
	provider.calls++
	provider.uploadWasNil = upload == nil
	if resume != nil {
		resumeValue := *resume
		provider.resume = &resumeValue
	}
	provider.checkpoint = checkpoint
	if resume == nil {
		if err := checkpoint(RenditionResumeHandle{Value: "remote-job-1"}); err != nil &&
			!provider.ignoreCheckpointError {
			return RenditionResult{}, err
		}
	}
	return provider.result, provider.err
}

var _ ResumableRenditionProvider = (*syntheticResumableRenditionProvider)(nil)

type syntheticAuthorizedUpload struct {
	io.ReadCloser

	closeCalls int
	closeErr   error
	metadata   AuthorizedUploadMetadata
}

func (upload *syntheticAuthorizedUpload) Metadata() AuthorizedUploadMetadata {
	return upload.metadata
}

func (upload *syntheticAuthorizedUpload) Close() error {
	upload.closeCalls++
	if upload.closeErr != nil {
		return upload.closeErr
	}
	return upload.ReadCloser.Close()
}

var _ AuthorizedUpload = (*syntheticAuthorizedUpload)(nil)

type changingRenditionProvider struct {
	descriptors []RenditionDescriptor
	calls       int
}

type mutatingRenditionProvider struct {
	descriptor       RenditionDescriptor
	render           func(AuthorizedUpload, RenditionAuthorization) RenditionResult
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
	_ context.Context, upload AuthorizedUpload, authorization RenditionAuthorization,
) (RenditionResult, error) {
	return provider.render(upload, authorization), nil
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

	produced, err := RenderRendition(t.Context(), provider, upload, authorization)
	require.NoError(t, err)
	assert.Equal(t, result, produced)
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
			_, _, err := validateRenditionProviderRequestAt(
				time.Now().UTC(), provider, upload, authorization,
			)
			require.ErrorContains(t, err, testCase.want)
		})
	}
}

func TestRenditionProviderContractRejectsDuplicateAndOversizedOutputs(t *testing.T) {
	descriptor := validRenditionDescriptor(t)
	metadata := validAuthorizedUploadMetadata()
	authorization := validRenditionAuthorization(descriptor, metadata)
	authorization.MaxArtifacts = 2
	for _, testCase := range []struct {
		name   string
		mutate func(*RenditionResult)
		want   string
	}{
		{
			name: "duplicate artifact identity",
			mutate: func(result *RenditionResult) {
				result.Artifacts = append(result.Artifacts, result.Artifacts[0])
			},
			want: "artifact identity",
		},
		{
			name: "oversized artifact media type",
			mutate: func(result *RenditionResult) {
				result.Artifacts[0].MediaType = "application/" + strings.Repeat("x", maxRenditionMediaTypeBytes)
			},
			want: "media type",
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

func TestRenditionProviderContractRejectsArtifactCountBeforeResultProcessing(t *testing.T) {
	descriptor := validRenditionDescriptor(t)
	metadata := validAuthorizedUploadMetadata()
	authorization := validRenditionAuthorization(descriptor, metadata)
	authorization.MaxTotalResultBytes = 128
	result := validRenditionResult(descriptor, authorization)
	result.Artifacts = append(result.Artifacts, result.Artifacts[0])
	result.Evidence.ContractVersion = "malformed"

	require.ErrorContains(t,
		ValidateRenditionResult(descriptor, authorization, result),
		"artifact count",
	)
}

func TestRenditionProviderContractCountsArtifactMetadataAgainstTotal(t *testing.T) {
	descriptor := validRenditionDescriptor(t)
	metadata := validAuthorizedUploadMetadata()
	authorization := validRenditionAuthorization(descriptor, metadata)
	result := validRenditionResult(descriptor, authorization)
	encodedEvidence, err := canonicalJSON(result.Evidence)
	require.NoError(t, err)
	authorization.MaxTotalResultBytes = len(encodedEvidence) +
		len(result.ProviderMarkdown) + len(result.Artifacts[0].Payload)

	require.ErrorContains(t,
		ValidateRenditionResult(descriptor, authorization, result),
		"total result bytes",
	)
}

func TestRenditionProviderContractCountsReceiptAgainstTotal(t *testing.T) {
	descriptor := validRenditionDescriptor(t)
	metadata := validAuthorizedUploadMetadata()
	authorization := validRenditionAuthorization(descriptor, metadata)
	result := validRenditionResult(descriptor, authorization)
	encodedEvidence, err := canonicalJSON(result.Evidence)
	require.NoError(t, err)
	artifact := result.Artifacts[0]
	authorization.MaxTotalResultBytes = len(encodedEvidence) + len(result.ProviderMarkdown) +
		len(artifact.Role) + len(artifact.MediaType) + len(artifact.SHA256) + len(artifact.Payload)

	require.ErrorContains(t,
		ValidateRenditionResult(descriptor, authorization, result),
		"total result bytes",
	)
}

func TestRenditionProviderContractPreflightsTotalBeforeEvidenceValidation(t *testing.T) {
	descriptor := validRenditionDescriptor(t)
	metadata := validAuthorizedUploadMetadata()
	authorization := validRenditionAuthorization(descriptor, metadata)
	authorization.MaxTotalResultBytes = 128
	result := validRenditionResult(descriptor, authorization)
	result.Evidence.ContractVersion = "malformed"

	require.ErrorContains(t,
		ValidateRenditionResult(descriptor, authorization, result),
		"total result bytes",
	)
}

func TestRenditionProviderContractAcceptsRepeatedArtifactRoles(t *testing.T) {
	descriptor := validRenditionDescriptor(t)
	metadata := validAuthorizedUploadMetadata()
	authorization := validRenditionAuthorization(descriptor, metadata)
	authorization.MaxArtifacts = 2
	result := validRenditionResult(descriptor, authorization)

	payload := []byte(`{"synthetic":"second structured artifact"}`)
	digest := sha256.Sum256(payload)
	checksum := hex.EncodeToString(digest[:])
	result.Artifacts = append(result.Artifacts, RenditionArtifact{
		Role: EvidenceArtifactStructured, MediaType: "application/json",
		Payload: payload, SHA256: checksum,
	})
	result.Evidence.Artifacts = append(result.Evidence.Artifacts, SourceEvidenceArtifactV1{
		ProviderID: "provider-artifact-2", Pointer: "provider/structured-2.json",
		Role: EvidenceArtifactStructured, SHA256: checksum,
	})

	require.NoError(t, ValidateRenditionResult(descriptor, authorization, result))
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
			name: "rendition request mismatch",
			mutate: func(receipt *RenditionReceipt) {
				receipt.RenditionRequestFingerprint = strings.Repeat("9", 64)
			},
			want: "does not match",
		},
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

func TestRenditionProviderContractRejectsReceiptFromDifferentAuthorization(t *testing.T) {
	descriptor := validRenditionDescriptor(t)
	metadata := validAuthorizedUploadMetadata()
	authorization := validRenditionAuthorization(descriptor, metadata)
	result := validRenditionResult(descriptor, authorization)
	different := authorization
	different.CapabilityRecordChecksum = strings.Repeat("9", 64)

	require.ErrorContains(t,
		ValidateRenditionResult(descriptor, different, result),
		"receipt does not match authorization",
	)
}

func TestRenditionProviderContractDoesNotReflectInvalidArtifactRoles(t *testing.T) {
	descriptor := validRenditionDescriptor(t)
	descriptor.Fingerprint = ""
	descriptor.ArtifactRoles = []EvidenceArtifactRole{
		EvidenceArtifactRole(strings.Repeat("provider-controlled", 4_096)),
	}
	_, err := NewRenditionDescriptor(descriptor)
	require.EqualError(t, err, "descriptor artifact role is invalid")

	descriptor = validRenditionDescriptor(t)
	metadata := validAuthorizedUploadMetadata()
	authorization := validRenditionAuthorization(descriptor, metadata)
	result := validRenditionResult(descriptor, authorization)
	result.Artifacts[0].Role = EvidenceArtifactRole("provider-controlled-value")
	require.EqualError(t,
		ValidateRenditionResult(descriptor, authorization, result),
		"provider artifact role is not authorized",
	)
}

func TestRenditionProviderContractRejectsUnclassifiedErrors(t *testing.T) {
	require.ErrorContains(t,
		ValidateRenditionProviderError(errors.New("raw provider body with secret")),
		"unclassified",
	)
	cause := errors.New("raw provider body with secret")
	providerError, err := NewRenditionProviderError(
		RenditionErrorRateLimited, "provider detail is intentionally not rendered", 30*time.Second, cause,
	)
	require.NoError(t, err)
	require.NoError(t, ValidateRenditionProviderError(providerError))
	assert.True(t, IsRenditionProviderErrorRetryable(providerError))
	wrapped := fmt.Errorf("unsafe wrapper includes provider body: %w", providerError)
	require.ErrorContains(t, ValidateRenditionProviderError(wrapped), "unclassified")
	assert.False(t, IsRenditionProviderErrorRetryable(wrapped))
	assert.Equal(t, "rendition provider rate limit reached", providerError.Error())
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
	_, _, err = validateRenditionProviderRequestAt(now, provider, upload, authorization)
	require.ErrorContains(t, err, "not current")
	authorizedAt, err := parseRenditionTimestamp(authorization.AuthorizedAt)
	require.NoError(t, err)
	_, _, err = validateRenditionProviderRequestAt(
		authorizedAt.Add(-time.Nanosecond),
		provider, upload, authorization)
	require.ErrorContains(t, err, "not current")

	result := validRenditionResult(descriptor, authorization)
	result.Receipt.StartedAt = authorizedAt.Add(-time.Nanosecond).Format(renditionTimestampForm)
	require.ErrorContains(t, ValidateRenditionResult(descriptor, authorization, result),
		"outside the authorization interval")
	result = validRenditionResult(descriptor, authorization)
	result.Receipt.CompletedAt = now.Format(renditionTimestampForm)
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

func TestRenditionDescriptorRequiresStructuredEvidence(t *testing.T) {
	descriptor := validRenditionDescriptor(t)
	descriptor.Fingerprint = ""
	descriptor.ReturnsStructured = false
	descriptor.ReturnsMarkdown = true

	_, err := NewRenditionDescriptor(descriptor)
	require.ErrorContains(t, err, "structured evidence")
}

func TestRenditionProviderContractRejectsMutableBoundarySnapshots(t *testing.T) {
	descriptor := validRenditionDescriptor(t)
	metadata := validAuthorizedUploadMetadata()
	authorization := validRenditionAuthorization(descriptor, metadata)

	changedDescriptor := descriptor
	changedDescriptor.ID = "changed-provider"
	provider := &changingRenditionProvider{descriptors: []RenditionDescriptor{descriptor, changedDescriptor}}
	upload := &syntheticAuthorizedUpload{ReadCloser: io.NopCloser(bytes.NewReader(nil)), metadata: metadata}
	_, _, err := validateRenditionProviderRequestAt(time.Now().UTC(), provider, upload, authorization)
	require.ErrorContains(t, err, "descriptor changed")

	changedMetadata := metadata
	changedMetadata.Filename = "changed.pdf"
	provider = &changingRenditionProvider{descriptors: []RenditionDescriptor{descriptor, descriptor}}
	changingUpload := &changingAuthorizedUpload{
		ReadCloser: io.NopCloser(bytes.NewReader(nil)),
		metadata:   []AuthorizedUploadMetadata{metadata, changedMetadata},
	}
	_, _, err = validateRenditionProviderRequestAt(
		time.Now().UTC(), provider, changingUpload, authorization,
	)
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

	_, _, err := validateRenditionProviderRequestAt(time.Now().UTC(), provider, upload, authorization)
	require.ErrorContains(t, err, "descriptor changed")
}

func TestRenderRenditionKeepsSealedAuthorizationSeparateFromProvider(t *testing.T) {
	descriptor := validRenditionDescriptor(t)
	metadata := validAuthorizedUploadMetadata()
	authorization := validRenditionAuthorization(descriptor, metadata)
	provider := &mutatingRenditionProvider{descriptor: descriptor}
	provider.render = func(_ AuthorizedUpload, received RenditionAuthorization) RenditionResult {
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

func TestRenderRenditionOwnsValidatedResult(t *testing.T) {
	descriptor := validRenditionDescriptor(t)
	metadata := validAuthorizedUploadMetadata()
	authorization := validRenditionAuthorization(descriptor, metadata)
	provided := validRenditionResult(descriptor, authorization)
	expected, err := canonicalJSON(provided)
	require.NoError(t, err)
	provider := &mutatingRenditionProvider{descriptor: descriptor}
	provider.render = func(_ AuthorizedUpload, _ RenditionAuthorization) RenditionResult {
		return provided
	}
	upload := &syntheticAuthorizedUpload{
		ReadCloser: io.NopCloser(strings.NewReader("synthetic exact source")), metadata: metadata,
	}

	result, err := RenderRendition(t.Context(), provider, upload, authorization)
	require.NoError(t, err)
	provided.Evidence.Artifacts[0].Pointer = "provider/changed.json"
	provided.Evidence.Omissions[0].Reason = "changed"
	provided.Evidence.Units[0].Text = "changed"
	provided.ProviderMarkdown[0] = 'X'
	provided.Artifacts[0].Payload[0] = 'X'
	provided.Receipt.Warnings[0] = "changed"
	actual, err := canonicalJSON(result)
	require.NoError(t, err)
	assert.Equal(t, expected, actual)
}

func TestRenderRenditionOwnsUpload(t *testing.T) {
	t.Run("validation failure preserves close error", func(t *testing.T) {
		descriptor := validRenditionDescriptor(t)
		metadata := validAuthorizedUploadMetadata()
		authorization := validRenditionAuthorization(descriptor, metadata)
		authorization.ProviderID = "wrong-provider"
		closeErr := errors.New("synthetic close failure")
		upload := &syntheticAuthorizedUpload{
			ReadCloser: io.NopCloser(strings.NewReader("synthetic exact source")),
			closeErr:   closeErr,
			metadata:   metadata,
		}

		_, err := RenderRendition(
			t.Context(), syntheticRenditionProvider{descriptor: descriptor}, upload, authorization,
		)
		require.ErrorContains(t, err, "provider ID")
		require.ErrorIs(t, err, closeErr)
		assert.Equal(t, 1, upload.closeCalls)
	})

	t.Run("provider close remains exactly once", func(t *testing.T) {
		descriptor := validRenditionDescriptor(t)
		metadata := validAuthorizedUploadMetadata()
		authorization := validRenditionAuthorization(descriptor, metadata)
		provider := &mutatingRenditionProvider{descriptor: descriptor}
		provider.render = func(upload AuthorizedUpload, received RenditionAuthorization) RenditionResult {
			require.NoError(t, upload.Close())
			return validRenditionResult(descriptor, received)
		}
		upload := &syntheticAuthorizedUpload{
			ReadCloser: io.NopCloser(strings.NewReader("synthetic exact source")),
			metadata:   metadata,
		}

		_, err := RenderRendition(t.Context(), provider, upload, authorization)
		require.NoError(t, err)
		assert.Equal(t, 1, upload.closeCalls)
	})
}

func TestRenderRenditionEnforcesExactUploadBytes(t *testing.T) {
	exact := []byte("synthetic exact source")
	mismatch := bytes.Clone(exact)
	mismatch[0] = 'S'
	for _, testCase := range []struct {
		name         string
		source       []byte
		wantReceived []byte
		wantError    string
	}{
		{name: "exact", source: exact, wantReceived: exact},
		{name: "extra", source: append(bytes.Clone(exact), []byte("private tail")...), wantReceived: exact,
			wantError: "exceeds declared byte length"},
		{name: "short", source: exact[:len(exact)-1], wantReceived: exact[:len(exact)-1],
			wantError: "shorter than declared byte length"},
		{name: "mismatched hash", source: mismatch, wantReceived: mismatch,
			wantError: "SHA-256 does not match"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			descriptor := validRenditionDescriptor(t)
			metadata := validAuthorizedUploadMetadata()
			authorization := validRenditionAuthorization(descriptor, metadata)
			provider := &mutatingRenditionProvider{descriptor: descriptor}
			var received []byte
			provider.render = func(upload AuthorizedUpload, receivedAuthorization RenditionAuthorization) RenditionResult {
				received, _ = io.ReadAll(upload)
				return validRenditionResult(descriptor, receivedAuthorization)
			}
			upload := &syntheticAuthorizedUpload{
				ReadCloser: io.NopCloser(bytes.NewReader(testCase.source)), metadata: metadata,
			}

			_, err := RenderRendition(t.Context(), provider, upload, authorization)
			assert.Equal(t, testCase.wantReceived, received)
			if testCase.wantError == "" {
				require.NoError(t, err)
			} else {
				require.ErrorContains(t, err, testCase.wantError)
			}
			assert.Equal(t, 1, upload.closeCalls)
		})
	}
}

func TestRenderRenditionCancellationClosesBlockedUpload(t *testing.T) {
	descriptor := validRenditionDescriptor(t)
	metadata := validAuthorizedUploadMetadata()
	authorization := validRenditionAuthorization(descriptor, metadata)
	pipeReader, pipeWriter := io.Pipe()
	t.Cleanup(func() { _ = pipeWriter.Close() })
	upload := &syntheticAuthorizedUpload{ReadCloser: pipeReader, metadata: metadata}
	started := make(chan struct{})
	readDone := make(chan error, 1)
	provider := &mutatingRenditionProvider{descriptor: descriptor}
	provider.render = func(upload AuthorizedUpload, received RenditionAuthorization) RenditionResult {
		close(started)
		_, readErr := upload.Read(make([]byte, 1))
		readDone <- readErr
		return validRenditionResult(descriptor, received)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		_, err := RenderRendition(ctx, provider, upload, authorization)
		done <- err
	}()
	<-started
	cancel()

	select {
	case err := <-done:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(5 * time.Second):
		_ = pipeReader.Close()
		<-done
		t.Fatal("RenderRendition did not close a blocked upload after cancellation")
	}
	require.Error(t, <-readDone)
	assert.Equal(t, 1, upload.closeCalls)
}

func TestRenderRenditionEnforcesAuthorizationExpiryDuringExecution(t *testing.T) {
	descriptor := validRenditionDescriptor(t)
	metadata := validAuthorizedUploadMetadata()
	authorization := validRenditionAuthorization(descriptor, metadata)
	expiresAt := time.Now().UTC().Add(250 * time.Millisecond)
	authorization.ExpiresAt = expiresAt.Format(renditionTimestampForm)
	upload := &syntheticAuthorizedUpload{
		ReadCloser: io.NopCloser(strings.NewReader("synthetic exact source")), metadata: metadata,
	}
	readDone := make(chan error, 1)
	provider := &mutatingRenditionProvider{descriptor: descriptor}
	provider.render = func(upload AuthorizedUpload, received RenditionAuthorization) RenditionResult {
		time.Sleep(time.Until(expiresAt.Add(20 * time.Millisecond)))
		_, readErr := upload.Read(make([]byte, 1))
		readDone <- readErr
		return validRenditionResult(descriptor, received)
	}

	_, err := RenderRendition(t.Context(), provider, upload, authorization)
	require.ErrorContains(t, err, "authorization is not current")
	require.Error(t, <-readDone)
	assert.Equal(t, 1, upload.closeCalls)
}

func TestRenderRenditionEnforcesFilenameDisclosure(t *testing.T) {
	for _, testCase := range []struct {
		name           string
		sourceFilename string
		disclose       bool
		want           string
		wantError      string
	}{
		{name: "withheld", sourceFilename: "document.pdf", want: ""},
		{name: "already redacted", want: ""},
		{name: "disclosed", sourceFilename: "document.pdf", disclose: true, want: "document.pdf"},
		{name: "missing disclosed filename", disclose: true, wantError: "filename disclosure"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			descriptor := validRenditionDescriptor(t)
			metadata := validAuthorizedUploadMetadata()
			metadata.Filename = testCase.sourceFilename
			authorization := validRenditionAuthorization(descriptor, metadata)
			authorization.DiscloseFilename = testCase.disclose
			provider := &mutatingRenditionProvider{descriptor: descriptor}
			var received AuthorizedUploadMetadata
			var reflectedFilename string
			provider.render = func(upload AuthorizedUpload, receivedAuthorization RenditionAuthorization) RenditionResult {
				received = upload.Metadata()
				var inspect func(reflect.Value)
				inspect = func(value reflect.Value) {
					if reflectedFilename != "" || !value.IsValid() {
						return
					}
					if value.CanInterface() {
						if nested, ok := reflect.TypeAssert[AuthorizedUpload](value); ok {
							reflectedFilename = nested.Metadata().Filename
							if reflectedFilename != "" {
								return
							}
						}
					}
					switch value.Kind() {
					case reflect.Interface, reflect.Pointer:
						inspect(value.Elem())
					case reflect.Struct:
						for _, field := range value.Fields() {
							if field.CanInterface() {
								inspect(field)
							}
						}
					default:
					}
				}
				inspect(reflect.ValueOf(upload))
				return validRenditionResult(descriptor, receivedAuthorization)
			}
			upload := &syntheticAuthorizedUpload{
				ReadCloser: io.NopCloser(strings.NewReader("synthetic exact source")), metadata: metadata,
			}

			_, err := RenderRendition(t.Context(), provider, upload, authorization)
			if testCase.wantError != "" {
				require.ErrorContains(t, err, testCase.wantError)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, testCase.want, received.Filename)
			assert.Equal(t, testCase.want, reflectedFilename)
			assert.Equal(t, testCase.sourceFilename, upload.Metadata().Filename)
		})
	}
}

func TestRenditionProviderContractRejectsTypedNilBoundaryValues(t *testing.T) {
	descriptor := validRenditionDescriptor(t)
	metadata := validAuthorizedUploadMetadata()
	authorization := validRenditionAuthorization(descriptor, metadata)

	var provider *changingRenditionProvider
	upload := &syntheticAuthorizedUpload{ReadCloser: io.NopCloser(bytes.NewReader(nil)), metadata: metadata}
	assert.NotPanics(t, func() {
		_, _, err := validateRenditionProviderRequestAt(time.Now().UTC(), provider, upload, authorization)
		require.ErrorContains(t, err, "provider is required")
	})

	validProvider := syntheticRenditionProvider{descriptor: descriptor}
	var nilUpload *syntheticAuthorizedUpload
	assert.NotPanics(t, func() {
		_, _, err := validateRenditionProviderRequestAt(
			time.Now().UTC(), validProvider, nilUpload, authorization,
		)
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
		DiscloseFilename:         false,
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
	authorizationFingerprint, _ := authorization.Fingerprint()
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
			PolicyFingerprint:           authorization.PolicyFingerprint,
			RenditionRequestFingerprint: authorization.RenditionRequestFingerprint,
			AuthorizationFingerprint:    authorizationFingerprint,
			SourceSHA256:                authorization.SourceSHA256,
			OperationID:                 "operation-synthetic-1",
			StartedAt:                   authorizedAt.Add(time.Second).Format(renditionTimestampForm),
			CompletedAt:                 authorizedAt.Add(2 * time.Second).Format(renditionTimestampForm),
			Warnings:                    []string{"degraded_provenance"},
			Usage: RenditionUsage{
				Requests: 1, InputBytes: authorization.SourceBytes,
				OutputBytes: int64(len(payload) + len("synthetic evidence\n")), Units: 1,
			},
		},
	}
}
