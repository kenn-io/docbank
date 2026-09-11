package suppliedtranscript

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/media/mediatest"
)

func TestProviderRendersSuppliedTranscriptAsAudioEvidence(t *testing.T) {
	data := mediatest.WAV()
	source := &stubSource{transcript: document.SuppliedTranscript{
		Provider: "beeper", Text: "The shipment arrives at dock seven.",
	}}
	provider, err := New(Profile{
		Source: source, SourceBinding: strings.Repeat("a", 64),
		MaxDocumentChars: 100,
	})
	require.NoError(t, err)
	upload := newTestUpload(data)
	authorization := testAuthorization(provider.Descriptor(), upload.Metadata())

	result, err := provider.Render(t.Context(), upload, authorization)
	require.NoError(t, err)
	assert.Equal(t, "audio", result.Evidence.Family)
	assert.Equal(t, document.EvidenceDegradedProvenance, result.Evidence.Completeness)
	assert.Equal(t, document.EvidenceUnitGeneric, result.Evidence.UnitKind)
	require.Len(t, result.Evidence.Units, 1)
	assert.Equal(t, source.transcript.Text, result.Evidence.Units[0].Text)
	require.Len(t, result.Artifacts, 1)
	assert.Equal(t, document.EvidenceArtifactTranscript, result.Artifacts[0].Role)
	assert.Equal(t, result.Artifacts[0].SHA256, result.Evidence.Artifacts[0].SHA256)
	assert.Equal(t, upload.Metadata().SHA256, result.Receipt.SourceSHA256)
	assert.Equal(t, int64(len(result.Artifacts[0].Payload)), result.Receipt.Usage.OutputBytes)
	assert.Equal(t, []string{upload.Metadata().SHA256}, source.calls)
}

func TestProviderBindsSourceIntoDescriptorIdentity(t *testing.T) {
	profile := Profile{
		Source:        &stubSource{transcript: document.SuppliedTranscript{Provider: "beeper", Text: "words"}},
		SourceBinding: strings.Repeat("a", 64), MaxDocumentChars: 100,
	}
	first, err := New(profile)
	require.NoError(t, err)
	repeat, err := New(profile)
	require.NoError(t, err)
	otherProfile := profile
	otherProfile.SourceBinding = strings.Repeat("b", 64)
	second, err := New(otherProfile)
	require.NoError(t, err)

	assert.Equal(t, first.Descriptor(), repeat.Descriptor())
	assert.NotEqual(t, first.Descriptor().PolicyFingerprint, second.Descriptor().PolicyFingerprint)
	assert.NotEqual(t, first.Descriptor().Fingerprint, second.Descriptor().Fingerprint)
}

func TestProviderRejectsAuthorizationWithoutTranscriptRole(t *testing.T) {
	data := mediatest.WAV()
	for name, mutate := range map[string]func(*document.RenditionAuthorization){
		"role absent": func(authorization *document.RenditionAuthorization) {
			authorization.AllowedArtifactRoles = nil
		},
		"artifact count absent": func(authorization *document.RenditionAuthorization) {
			authorization.MaxArtifacts = 0
		},
		"artifact bytes absent": func(authorization *document.RenditionAuthorization) {
			authorization.MaxArtifactBytes = 0
		},
	} {
		t.Run(name, func(t *testing.T) {
			source := &stubSource{transcript: document.SuppliedTranscript{Provider: "beeper", Text: "words"}}
			provider, err := New(Profile{
				Source: source, SourceBinding: strings.Repeat("a", 64),
				MaxDocumentChars: 100,
			})
			require.NoError(t, err)
			upload := newTestUpload(data)
			authorization := testAuthorization(provider.Descriptor(), upload.Metadata())
			mutate(&authorization)
			_, err = provider.Render(t.Context(), upload, authorization)
			assertProviderCode(t, err, document.RenditionErrorPolicyRejected)
			assert.Empty(t, source.calls)
		})
	}
}

func TestProviderReportsMissingTranscriptAsUnsupportedInput(t *testing.T) {
	data := mediatest.WAV()
	source := &stubSource{}
	provider := newProvider(t, source)
	upload := newTestUpload(data)

	_, err := provider.Render(t.Context(), upload, testAuthorization(provider.Descriptor(), upload.Metadata()))
	assertProviderCode(t, err, document.RenditionErrorUnsupportedInput)
	assert.Equal(t, []string{upload.Metadata().SHA256}, source.calls)
}

func TestProviderIgnoresTranscriptsForOtherDigests(t *testing.T) {
	data := mediatest.WAV()
	otherDigest := strings.Repeat("b", 64)
	source := &stubSource{
		transcript: document.SuppliedTranscript{Provider: "beeper", Text: "other audio"},
		onlyDigest: otherDigest,
	}
	provider := newProvider(t, source)
	upload := newTestUpload(data)

	_, err := provider.Render(t.Context(), upload, testAuthorization(provider.Descriptor(), upload.Metadata()))
	assertProviderCode(t, err, document.RenditionErrorUnsupportedInput)
	assert.Equal(t, []string{upload.Metadata().SHA256}, source.calls)
}

func TestProviderClassifiesUnknownSourceFailuresAsTransient(t *testing.T) {
	for name, sourceErr := range map[string]error{
		"private error": privateSourceError{},
		"no progress":   io.ErrNoProgress,
		"permission":    fs.ErrPermission,
	} {
		t.Run(name, func(t *testing.T) {
			source := &stubSource{err: sourceErr}
			provider := newProvider(t, source)
			upload := newTestUpload(mediatest.WAV())
			_, err := provider.Render(t.Context(), upload, testAuthorization(provider.Descriptor(), upload.Metadata()))
			assertProviderCode(t, err, document.RenditionErrorTransient)
		})
	}

	source := &stubSource{}
	ctx, cancel := context.WithCancel(t.Context())
	source.cancel = cancel
	source.err = context.Canceled
	provider := newProvider(t, source)
	upload := newTestUpload(mediatest.WAV())
	_, err := provider.Render(ctx, upload, testAuthorization(provider.Descriptor(), upload.Metadata()))
	assertProviderCode(t, err, document.RenditionErrorCanceled)
}

func TestProviderPreservesClassifiedSourceFailures(t *testing.T) {
	permanent, err := document.NewRenditionProviderError(document.RenditionErrorAuthentication, 0, fs.ErrPermission)
	require.NoError(t, err)
	for name, sourceErr := range map[string]error{
		"direct":  permanent,
		"wrapped": fmt.Errorf("resolve transcript: %w", permanent),
	} {
		t.Run(name, func(t *testing.T) {
			provider := newProvider(t, &stubSource{err: sourceErr})
			upload := newTestUpload(mediatest.WAV())
			_, err := provider.Render(t.Context(), upload, testAuthorization(provider.Descriptor(), upload.Metadata()))
			require.Same(t, permanent, err)
			assert.False(t, document.IsRenditionProviderErrorRetryable(err))
		})
	}
}

func TestProviderRejectsInvalidProfiles(t *testing.T) {
	validSource := &stubSource{}
	base := Profile{
		Source: validSource, SourceBinding: strings.Repeat("a", 64),
		MaxDocumentChars: 100,
	}
	for name, profile := range map[string]Profile{
		"nil source":               {SourceBinding: base.SourceBinding, MaxDocumentChars: 100},
		"short source binding":     withProfile(base, func(p *Profile) { p.SourceBinding = "a" }),
		"uppercase source binding": withProfile(base, func(p *Profile) { p.SourceBinding = strings.Repeat("A", 64) }),
		"nonhex source binding":    withProfile(base, func(p *Profile) { p.SourceBinding = strings.Repeat("g", 64) }),
		"zero character bound":     withProfile(base, func(p *Profile) { p.MaxDocumentChars = 0 }),
		"negative character bound": withProfile(base, func(p *Profile) { p.MaxDocumentChars = -1 }),
	} {
		t.Run(name, func(t *testing.T) {
			provider, err := New(profile)
			require.Error(t, err)
			assert.Nil(t, provider)
		})
	}
}

func TestDescriptorIsImmutable(t *testing.T) {
	provider := newProvider(t, &stubSource{})
	descriptor := provider.Descriptor()
	descriptor.SupportedFormats[0].MediaType = "application/pdf"
	descriptor.ArtifactRoles[0] = document.EvidenceArtifactImage
	current := provider.Descriptor()
	assert.NotEqual(t, descriptor.SupportedFormats, current.SupportedFormats)
	assert.NotEqual(t, descriptor.ArtifactRoles, current.ArtifactRoles)
}

type stubSource struct {
	transcript document.SuppliedTranscript
	err        error
	onlyDigest string
	cancel     context.CancelFunc
	calls      []string
}

func (source *stubSource) Transcript(ctx context.Context, digest string) (document.SuppliedTranscript, error) {
	source.calls = append(source.calls, digest)
	if source.cancel != nil {
		source.cancel()
	}
	if source.err != nil {
		return document.SuppliedTranscript{}, source.err
	}
	if source.onlyDigest != "" && source.onlyDigest != digest {
		return document.SuppliedTranscript{}, nil
	}
	return source.transcript, nil
}

type privateSourceError struct{}

func (privateSourceError) Error() string { return "private source failure" }

func newProvider(t *testing.T, source Source) *Provider {
	t.Helper()
	provider, err := New(Profile{
		Source: source, SourceBinding: strings.Repeat("a", 64),
		MaxDocumentChars: 100,
	})
	require.NoError(t, err)
	return provider
}

func withProfile(base Profile, mutate func(*Profile)) Profile {
	profile := base
	mutate(&profile)
	return profile
}

type testUpload struct {
	reader   *bytes.Reader
	metadata document.AuthorizedUploadMetadata
}

func newTestUpload(data []byte) *testUpload {
	digest := sha256.Sum256(data)
	return &testUpload{
		reader: bytes.NewReader(data),
		metadata: document.AuthorizedUploadMetadata{
			Filename: "sample.wav", MediaFamily: "audio", MediaType: "audio/wav",
			ByteLength: int64(len(data)), SHA256: hex.EncodeToString(digest[:]),
			CapabilityRecordChecksum: strings.Repeat("2", 64),
			ProviderMetadataChecksum: strings.Repeat("3", 64),
			InputKind:                document.RenditionInputOriginalFile,
		},
	}
}

func (upload *testUpload) Read(buffer []byte) (int, error) {
	read, err := upload.reader.Read(buffer)
	if err != nil {
		return read, fmt.Errorf("read test upload: %w", err)
	}
	return read, nil
}
func (*testUpload) Close() error { return nil }
func (upload *testUpload) Metadata() document.AuthorizedUploadMetadata {
	return upload.metadata
}

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
		InputKind: metadata.InputKind, AllowedArtifactRoles: []document.EvidenceArtifactRole{
			document.EvidenceArtifactTranscript,
		}, MaxProviderMarkdownBytes: 0, MaxArtifactBytes: 1 << 20, MaxArtifacts: 1,
		MaxTotalResultBytes: 1 << 20,
		AuthorizedAt:        started.Format("2006-01-02T15:04:05.000000000Z"),
		ExpiresAt:           started.Add(10 * time.Minute).Format("2006-01-02T15:04:05.000000000Z"),
	}
}

func assertProviderCode(t *testing.T, err error, code document.RenditionErrorCode) {
	t.Helper()
	require.Error(t, err)
	providerErr, ok := errors.AsType[*document.RenditionProviderError](err)
	require.True(t, ok)
	assert.Equal(t, code, providerErr.Code())
}

var _ document.AuthorizedUpload = (*testUpload)(nil)
var _ Source = (*stubSource)(nil)
var _ io.ReadCloser = (*testUpload)(nil)
