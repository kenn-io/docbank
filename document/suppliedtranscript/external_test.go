package suppliedtranscript_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/media"
	"go.kenn.io/docbank/document/media/mediatest"
	"go.kenn.io/docbank/document/suppliedtranscript"
	"go.kenn.io/docbank/document/upload"
)

func TestSuppliedAudioTranscriptRendersThroughAuthorizedUpload(t *testing.T) {
	transcript := document.SuppliedTranscript{Provider: "beeper", Text: "The shipment arrives at dock seven."}

	for name, testCase := range map[string]struct {
		data     []byte
		filename string
		media    string
	}{
		"wav": {data: mediatest.WAV(), filename: "sample.wav", media: "audio/wav"},
		"mp3": {data: mediatest.MP3(), filename: "sample.mp3", media: "audio/mpeg"},
	} {
		t.Run(name, func(t *testing.T) {
			source := &testSource{transcript: transcript}
			provider, err := suppliedtranscript.New(suppliedtranscript.Profile{
				Source: source, SourceBinding: strings.Repeat("a", 64),
				MaxAudioBytes: suppliedtranscript.MaxAudioBytes, MaxDocumentChars: 100,
			})
			require.NoError(t, err)
			policy := inspectionPolicy(testCase.data, testCase.filename, testCase.media)
			record, err := media.InspectCapability(bytes.NewReader(testCase.data), policy)
			require.NoError(t, err)
			require.True(t, record.Eligible)
			assert.Equal(t, "audio", record.MediaFamily)
			authorized, err := upload.Authorize(t.Context(), upload.Source{
				Reader: io.NopCloser(bytes.NewReader(testCase.data)), Directory: t.TempDir(),
			}, record, upload.UploadMetadata{Filename: testCase.filename})
			require.NoError(t, err)
			metadata := authorized.Metadata()
			result, err := document.RenderRendition(t.Context(), provider, authorized,
				testAuthorization(provider.Descriptor(), metadata))
			require.NoError(t, err)
			require.Len(t, result.Evidence.Units, 1)
			assert.Equal(t, transcript.Text, result.Evidence.Units[0].Text)
			require.Len(t, result.Artifacts, 1)
			assert.Equal(t, result.Artifacts[0].SHA256, result.Evidence.Artifacts[0].SHA256)
			assert.Equal(t, metadata.SHA256, result.Receipt.SourceSHA256)
			assert.Equal(t, metadata.SHA256, source.calls[0])
		})
	}

	t.Run("identity", func(t *testing.T) {
		data := mediatest.WAV()
		filename, mediaType := "sample.wav", "audio/wav"
		source := &testSource{transcript: transcript}
		provider, err := suppliedtranscript.New(suppliedtranscript.Profile{
			Source: source, SourceBinding: strings.Repeat("a", 64),
			MaxAudioBytes: suppliedtranscript.MaxAudioBytes, MaxDocumentChars: 100,
		})
		require.NoError(t, err)
		record, err := media.InspectCapability(bytes.NewReader(data), inspectionPolicy(data, filename, mediaType))
		require.NoError(t, err)
		require.True(t, record.Eligible)
		assert.Equal(t, "audio", record.MediaFamily)
		authorized, err := upload.Authorize(t.Context(), upload.Source{
			Reader: io.NopCloser(bytes.NewReader(data)), Directory: t.TempDir(),
		}, record, upload.UploadMetadata{Filename: filename})
		require.NoError(t, err)
		result, err := document.RenderRendition(t.Context(), provider, authorized,
			testAuthorization(provider.Descriptor(), authorized.Metadata()))
		require.NoError(t, err)

		policy, err := document.NewEvidencePolicy(100)
		require.NoError(t, err)
		normalized, err := document.NormalizeEvidenceV1(result.Evidence, policy)
		require.NoError(t, err)
		_, actualChecksum, err := document.MarshalNormalizedEvidenceV1(normalized)
		require.NoError(t, err)
		expected, _, err := document.BuildTranscriptEvidenceV1(transcript, policy)
		require.NoError(t, err)
		_, expectedChecksum, err := document.MarshalNormalizedEvidenceV1(expected)
		require.NoError(t, err)
		assert.Equal(t, expectedChecksum, actualChecksum)
	})
}

type testSource struct {
	transcript document.SuppliedTranscript
	calls      []string
}

func (source *testSource) Transcript(_ context.Context, digest string) (document.SuppliedTranscript, error) {
	source.calls = append(source.calls, digest)
	return source.transcript, nil
}

func inspectionPolicy(data []byte, filename, mediaType string) media.InspectionPolicy {
	digest := sha256.Sum256(data)
	return media.InspectionPolicy{
		Filename: filename, DeclaredMediaType: mediaType,
		ExpectedBytes: int64(len(data)), ExpectedSHA256: hex.EncodeToString(digest[:]),
		DescriptorFingerprint: strings.Repeat("b", 64), ProfileFingerprint: strings.Repeat("c", 64),
		DisclosureFingerprint: strings.Repeat("d", 64), InputKind: document.RenditionInputOriginalFile,
		MaxSourceBytes: 1 << 20, MaxExpandedBytes: 1 << 20, MaxEntryBytes: 1 << 20,
		MaxEntries: 100, MaxNestingDepth: 1, MaxTextLines: 1_000, MaxCharacters: 1 << 20,
		MaxRecords: 100, MaxPages: 100, MaxSlides: 100, MaxSheets: 100, MaxCells: 10_000,
		MaxSpineItems: 1_000, MaxResources: 10_000, MaxPixels: 1 << 20, MaxFrames: 100,
		MaxDurationMS: 1_000,
	}
}

func testAuthorization(
	descriptor document.RenditionDescriptor, metadata document.AuthorizedUploadMetadata,
) document.RenditionAuthorization {
	started := time.Now().UTC().Add(-time.Minute)
	return document.RenditionAuthorization{
		ProviderID: descriptor.ID, DescriptorFingerprint: descriptor.Fingerprint,
		PolicyFingerprint:           descriptor.PolicyFingerprint,
		RenditionRequestFingerprint: strings.Repeat("e", 64),
		SourceSHA256:                metadata.SHA256, SourceBytes: metadata.ByteLength,
		CapabilityRecordChecksum: metadata.CapabilityRecordChecksum,
		ProviderMetadataChecksum: metadata.ProviderMetadataChecksum,
		MediaFamily:              metadata.MediaFamily, MediaType: metadata.MediaType, InputKind: metadata.InputKind,
		AllowedArtifactRoles: []document.EvidenceArtifactRole{document.EvidenceArtifactTranscript},
		MaxArtifactBytes:     1 << 20, MaxArtifacts: 1, MaxTotalResultBytes: 1 << 20,
		AuthorizedAt: started.Format("2006-01-02T15:04:05.000000000Z"),
		ExpiresAt:    started.Add(10 * time.Minute).Format("2006-01-02T15:04:05.000000000Z"),
	}
}
