package document

import (
	"bytes"
	"io"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRenditionExecutionIdentityBindsEveryStableProviderAndOutputInput(t *testing.T) {
	descriptor := validRenditionDescriptor(t)
	metadata := validAuthorizedUploadMetadata()
	authorization := validRenditionAuthorization(descriptor, metadata)
	authorization.DiscloseFilename = true
	evidence, err := NewEvidencePolicy(100_000)
	require.NoError(t, err)
	normalization, err := NewNormalizePolicy(100_000)
	require.NoError(t, err)
	rendition, err := NewRenditionPolicy(normalization, 1_000)
	require.NoError(t, err)
	base, err := NewRenditionExecutionIdentityV1(metadata, authorization, evidence, rendition)
	require.NoError(t, err)
	_, baseFingerprint, err := CanonicalRenditionExecutionIdentityV1(base)
	require.NoError(t, err)

	mutations := map[string]func(*AuthorizedUploadMetadata, *RenditionAuthorization, *EvidencePolicy, *RenditionPolicy){
		"filename disclosure": func(_ *AuthorizedUploadMetadata, authorization *RenditionAuthorization, _ *EvidencePolicy, _ *RenditionPolicy) {
			authorization.DiscloseFilename = !authorization.DiscloseFilename
		},
		"filename": func(metadata *AuthorizedUploadMetadata, _ *RenditionAuthorization, _ *EvidencePolicy, _ *RenditionPolicy) {
			metadata.Filename = "renamed.pdf"
		},
		"capability checksum": func(metadata *AuthorizedUploadMetadata, authorization *RenditionAuthorization, _ *EvidencePolicy, _ *RenditionPolicy) {
			metadata.CapabilityRecordChecksum = sha256Hex([]byte("changed-capability"))
			authorization.CapabilityRecordChecksum = metadata.CapabilityRecordChecksum
		},
		"provider metadata checksum": func(metadata *AuthorizedUploadMetadata, authorization *RenditionAuthorization, _ *EvidencePolicy, _ *RenditionPolicy) {
			metadata.ProviderMetadataChecksum = sha256Hex([]byte("changed-provider-metadata"))
			authorization.ProviderMetadataChecksum = metadata.ProviderMetadataChecksum
		},
		"result limit": func(_ *AuthorizedUploadMetadata, authorization *RenditionAuthorization, _ *EvidencePolicy, _ *RenditionPolicy) {
			authorization.MaxTotalResultBytes++
		},
		"evidence policy": func(_ *AuthorizedUploadMetadata, _ *RenditionAuthorization, evidence *EvidencePolicy, _ *RenditionPolicy) {
			*evidence, _ = NewEvidencePolicy(99_999)
		},
		"normalization policy": func(_ *AuthorizedUploadMetadata, _ *RenditionAuthorization, _ *EvidencePolicy, rendition *RenditionPolicy) {
			normalization, _ := NewNormalizePolicy(99_999)
			*rendition, _ = NewRenditionPolicy(normalization, 1_000)
		},
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			changedMetadata := metadata
			changedAuthorization := authorization
			changedEvidence := evidence
			changedRendition := rendition
			mutate(&changedMetadata, &changedAuthorization, &changedEvidence, &changedRendition)
			identity, err := NewRenditionExecutionIdentityV1(
				changedMetadata, changedAuthorization, changedEvidence, changedRendition)
			require.NoError(t, err)
			_, fingerprint, err := CanonicalRenditionExecutionIdentityV1(identity)
			require.NoError(t, err)
			assert.NotEqual(t, baseFingerprint, fingerprint)
		})
	}

	authorization.AuthorizedAt = time.Now().UTC().Add(-2 * time.Minute).Format(renditionTimestampForm)
	authorization.ExpiresAt = time.Now().UTC().Add(5 * time.Minute).Format(renditionTimestampForm)
	same, err := NewRenditionExecutionIdentityV1(metadata, authorization, evidence, rendition)
	require.NoError(t, err)
	_, sameFingerprint, err := CanonicalRenditionExecutionIdentityV1(same)
	require.NoError(t, err)
	assert.Equal(t, baseFingerprint, sameFingerprint,
		"transient authorization windows must not split a genuinely shared build")
}

func TestRenditionExecutionIdentityOmitsWithheldFilename(t *testing.T) {
	descriptor := validRenditionDescriptor(t)
	metadata := validAuthorizedUploadMetadata()
	authorization := validRenditionAuthorization(descriptor, metadata)
	require.False(t, authorization.DiscloseFilename)
	evidence, err := NewEvidencePolicy(100_000)
	require.NoError(t, err)
	normalization, err := NewNormalizePolicy(100_000)
	require.NoError(t, err)
	rendition, err := NewRenditionPolicy(normalization, 1_000)
	require.NoError(t, err)

	first, err := NewRenditionExecutionIdentityV1(
		metadata, authorization, evidence, rendition)
	require.NoError(t, err)
	metadata.Filename = "renamed.pdf"
	second, err := NewRenditionExecutionIdentityV1(
		metadata, authorization, evidence, rendition)
	require.NoError(t, err)
	_, firstFingerprint, err := CanonicalRenditionExecutionIdentityV1(first)
	require.NoError(t, err)
	_, secondFingerprint, err := CanonicalRenditionExecutionIdentityV1(second)
	require.NoError(t, err)
	assert.Empty(t, first.Upload.Filename)
	assert.Empty(t, second.Upload.Filename)
	assert.Equal(t, firstFingerprint, secondFingerprint)

	first.Upload.Filename = "must-not-persist.pdf"
	snapshot, err := NewRenditionExecutionSnapshotV1(first, authorization)
	require.NoError(t, err)
	assert.Empty(t, snapshot.Identity.Upload.Filename)
	encoded, err := CanonicalRenditionExecutionSnapshotV1(snapshot)
	require.NoError(t, err)
	assert.NotContains(t, string(encoded), "must-not-persist.pdf")
}

func TestRenditionExecutionIdentityRequiresDisclosedFilename(t *testing.T) {
	descriptor := validRenditionDescriptor(t)
	metadata := validAuthorizedUploadMetadata()
	metadata.Filename = ""
	authorization := validRenditionAuthorization(descriptor, metadata)
	authorization.DiscloseFilename = true
	evidence, err := NewEvidencePolicy(100_000)
	require.NoError(t, err)
	normalization, err := NewNormalizePolicy(100_000)
	require.NoError(t, err)
	rendition, err := NewRenditionPolicy(normalization, 1_000)
	require.NoError(t, err)

	_, err = NewRenditionExecutionIdentityV1(
		metadata, authorization, evidence, rendition)
	require.ErrorContains(t, err, "disclosed filename")
}

func TestResumeRenditionUsesOriginalSealedAuthorizationWithoutUpload(t *testing.T) {
	descriptor := validRenditionDescriptor(t)
	metadata := validAuthorizedUploadMetadata()
	authorization := validRenditionAuthorization(descriptor, metadata)
	historical := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	authorization.AuthorizedAt = historical.Format(renditionTimestampForm)
	authorization.ExpiresAt = historical.Add(10 * time.Minute).Format(renditionTimestampForm)
	evidence, err := NewEvidencePolicy(100_000)
	require.NoError(t, err)
	normalization, err := NewNormalizePolicy(100_000)
	require.NoError(t, err)
	rendition, err := NewRenditionPolicy(normalization, 1_000)
	require.NoError(t, err)
	upload := &syntheticAuthorizedUpload{
		ReadCloser: io.NopCloser(bytes.NewReader([]byte("synthetic exact source"))),
		metadata:   metadata,
	}
	provider := &syntheticResumableRenditionProvider{
		descriptor: descriptor, result: validRenditionResult(descriptor, authorization)}
	snapshot, err := SealRenditionExecutionAt(
		historical.Add(time.Minute), provider, upload, authorization, evidence, rendition)
	require.NoError(t, err)
	canonical, err := CanonicalRenditionExecutionSnapshotV1(snapshot)
	require.NoError(t, err)
	restored, err := ParseRenditionExecutionSnapshotV1(canonical)
	require.NoError(t, err)

	result, err := ResumeRendition(
		t.Context(), provider, restored, RenditionResumeHandle{Value: "remote-job-1"}, nil)
	require.NoError(t, err)
	assert.Equal(t, provider.result, result)
	assert.True(t, provider.uploadWasNil)
	assert.Equal(t, "remote-job-1", provider.resume.Value)
	assert.Equal(t, authorization, restored.Authorization,
		"the original authorization interval and limits are durable resume authority")
}
