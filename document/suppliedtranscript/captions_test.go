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
	"go.kenn.io/docbank/document/media"
	"go.kenn.io/docbank/document/media/mediatest"
	"go.kenn.io/docbank/document/mediatranscript"
)

func TestCaptionProviderRendersTimedSuppliedEvidence(t *testing.T) {
	data := mediatest.H264AACMP4()
	srt := []byte("1\r\n00:00:01,250 --> 00:00:02,750\r\nalpha\r\n\r\n2\r\n00:00:02,500 --> 00:00:04,125\r\nbeta\r\n")
	provider, upload, authorization := newCaptionFixture(t, data, "video/mp4")
	source := &captionStub{caption: Caption{Provider: "loom", SRT: srt}}
	provider.source = source

	result, err := provider.Render(t.Context(), upload, authorization)
	require.NoError(t, err)
	assert.Equal(t, "video", result.Evidence.Family)
	assert.Equal(t, document.EvidenceUnitSegment, result.Evidence.UnitKind)
	require.Len(t, result.Evidence.Units, 2)
	assert.Equal(t, int64(2_500), result.Evidence.Units[1].Locator.Start)
	assert.Equal(t, int64(4_125), result.Evidence.Units[1].Locator.End)
	assert.Empty(t, result.Receipt.Warnings)
	require.Len(t, result.Artifacts, 1)
	artifact, err := mediatranscript.Unmarshal(result.Artifacts[0].Payload)
	require.NoError(t, err)
	assert.Equal(t, "supplied", artifact.Origin)
	assert.Equal(t, "loom", artifact.Provider)
	assert.Empty(t, artifact.Language)
	assert.Equal(t, []string{upload.Metadata().InputBinding}, source.bindings)
}

func TestCaptionProviderRejects(t *testing.T) {
	data := mediatest.H264AACMP4()
	tests := map[string]struct {
		caption Caption
		mutate  func(*testCaptionUpload, *document.RenditionAuthorization)
		code    document.RenditionErrorCode
	}{
		"missing binding": {
			caption: Caption{Provider: "loom", SRT: []byte("1\n00:00:00,000 --> 00:00:00,100\nx")},
			mutate: func(upload *testCaptionUpload, _ *document.RenditionAuthorization) {
				upload.metadata.InputBinding = ""
			}, code: document.RenditionErrorPolicyRejected,
		},
		"missing proof": {
			caption: Caption{Provider: "loom", SRT: []byte("1\n00:00:00,000 --> 00:00:00,100\nx")},
			mutate: func(upload *testCaptionUpload, _ *document.RenditionAuthorization) {
				upload.proof = document.UploadCapability{}
			}, code: document.RenditionErrorPolicyRejected,
		},
		"past duration": {
			caption: Caption{Provider: "loom", SRT: []byte("1\n00:00:20,000 --> 00:00:20,100\nx")},
			code:    document.RenditionErrorMalformedEvidence,
		},
		"malformed": {
			caption: Caption{Provider: "loom", SRT: []byte("1\n00:00:00.000 --> 00:00:00.100\nx")},
			code:    document.RenditionErrorMalformedEvidence,
		},
		"empty": {
			caption: Caption{Provider: "loom"}, code: document.RenditionErrorUnsupportedInput,
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			provider, upload, authorization := newCaptionFixture(t, data, "video/mp4")
			provider.source = &captionStub{caption: test.caption}
			if test.mutate != nil {
				test.mutate(upload, &authorization)
			}
			_, err := provider.Render(t.Context(), upload, authorization)
			assertCaptionCode(t, err, test.code)
		})
	}
}

func TestCaptionProviderSupportsWAVAndRejectsStaleProof(t *testing.T) {
	provider, upload, authorization := newCaptionFixture(t, mediatest.WAV(), "audio/wav")
	provider.source = &captionStub{caption: Caption{Provider: "loom", SRT: []byte(
		"1\n00:00:00,000 --> 00:00:00,005\naudio cue\n")}}
	result, err := provider.Render(t.Context(), upload, authorization)
	require.NoError(t, err)
	assert.Equal(t, "audio", result.Evidence.Family)

	authorization.CapabilityRecordChecksum = strings.Repeat("f", 64)
	_, err = provider.Render(t.Context(), upload, authorization)
	assertCaptionCode(t, err, document.RenditionErrorPolicyRejected)

	facts, _ := upload.proof.Facts()
	authorization.CapabilityRecordChecksum = facts.Checksum
	authorization.MaxArtifactBytes = 1
	_, err = provider.Render(t.Context(), upload, authorization)
	assertCaptionCode(t, err, document.RenditionErrorMalformedEvidence)
}

type captionStub struct {
	caption  Caption
	err      error
	bindings []string
}

func (source *captionStub) CaptionForBinding(_ context.Context, _ string, binding string) (Caption, error) {
	source.bindings = append(source.bindings, binding)
	return source.caption, source.err
}

type testCaptionUpload struct {
	reader   *bytes.Reader
	metadata document.AuthorizedUploadMetadata
	proof    document.UploadCapability
}

func (upload *testCaptionUpload) Read(buffer []byte) (int, error) {
	n, err := upload.reader.Read(buffer)
	if err != nil {
		return n, fmt.Errorf("read caption test upload: %w", err)
	}
	return n, nil
}
func (*testCaptionUpload) Close() error { return nil }
func (upload *testCaptionUpload) Metadata() document.AuthorizedUploadMetadata {
	return upload.metadata
}
func (upload *testCaptionUpload) CapabilityProof() document.UploadCapability { return upload.proof }

func newCaptionFixture(
	t *testing.T, data []byte, mediaType string,
) (*CaptionProvider, *testCaptionUpload, document.RenditionAuthorization) {
	t.Helper()
	provider, err := NewCaptions(CaptionProfile{
		Source: &captionStub{}, SourceBinding: strings.Repeat("a", 64), MaxDocumentChars: 100,
	})
	require.NoError(t, err)
	filename := mediaFilename(mediaType)
	policy := media.InspectionPolicy{
		Filename: filename, DeclaredMediaType: mediaType,
		ExpectedBytes: int64(len(data)), ExpectedSHA256: sha256Hex(data),
		DescriptorFingerprint: provider.Descriptor().Fingerprint,
		ProfileFingerprint:    strings.Repeat("b", 64), DisclosureFingerprint: strings.Repeat("c", 64),
		InputKind:      document.RenditionInputOriginalFile,
		MaxSourceBytes: 20 << 20, MaxExpandedBytes: 20 << 20, MaxEntryBytes: 20 << 20,
		MaxEntries: 100, MaxNestingDepth: 1, MaxTextLines: 1_000_000, MaxCharacters: 20 << 20,
		MaxRecords: 1_000_000, MaxPages: 100_000, MaxSlides: 100_000, MaxSheets: 100_000,
		MaxCells: 10_000_000, MaxSpineItems: 100_000, MaxResources: 1_000_000,
		MaxPixels: 1920 * 1088, MaxFrames: 18_000, MaxDurationMS: 300_000,
	}
	record, err := media.InspectCapability(bytes.NewReader(data), policy)
	require.NoError(t, err)
	require.True(t, record.Eligible, record.Reason)
	proof := record.UploadCapability()
	facts, local := proof.Facts()
	require.True(t, local)
	upload := &testCaptionUpload{reader: bytes.NewReader(data), proof: proof, metadata: document.AuthorizedUploadMetadata{
		Filename: filename, MediaFamily: facts.MediaFamily, MediaType: facts.MediaType,
		ByteLength: int64(len(data)), SHA256: sha256Hex(data), CapabilityRecordChecksum: facts.Checksum,
		ProviderMetadataChecksum: strings.Repeat("3", 64), InputKind: document.RenditionInputOriginalFile,
		InputBinding: strings.Repeat("d", 64),
	}}
	started := time.Now().UTC().Add(-time.Minute)
	authorization := document.RenditionAuthorization{
		ProviderID: provider.Descriptor().ID, DescriptorFingerprint: provider.Descriptor().Fingerprint,
		PolicyFingerprint: provider.Descriptor().PolicyFingerprint, RenditionRequestFingerprint: strings.Repeat("4", 64),
		SourceSHA256: upload.metadata.SHA256, SourceBytes: upload.metadata.ByteLength,
		CapabilityRecordChecksum: facts.Checksum, ProviderMetadataChecksum: upload.metadata.ProviderMetadataChecksum,
		MediaFamily: facts.MediaFamily, MediaType: facts.MediaType, InputKind: document.RenditionInputOriginalFile,
		AllowedArtifactRoles: []document.EvidenceArtifactRole{document.EvidenceArtifactTranscript},
		MaxArtifactBytes:     1 << 20, MaxArtifacts: 1, MaxTotalResultBytes: 1 << 20,
		AuthorizedAt: started.Format(providerTimestampForm), ExpiresAt: started.Add(10 * time.Minute).Format(providerTimestampForm),
	}
	return provider, upload, authorization
}

func mediaFilename(mediaType string) string {
	switch mediaType {
	case "audio/mpeg":
		return "sample.mp3"
	case "audio/wav":
		return "sample.wav"
	default:
		return "sample.mp4"
	}
}

const providerTimestampForm = "2006-01-02T15:04:05.000000000Z"

func sha256Hex(value []byte) string {
	digest := sha256.Sum256(value)
	return hex.EncodeToString(digest[:])
}

func assertCaptionCode(t *testing.T, err error, want document.RenditionErrorCode) {
	t.Helper()
	require.Error(t, err)
	providerErr, ok := errors.AsType[*document.RenditionProviderError](err)
	require.True(t, ok)
	assert.Equal(t, want, providerErr.Code())
}

var _ document.AuthorizedUpload = (*testCaptionUpload)(nil)
var _ io.ReadCloser = (*testCaptionUpload)(nil)
