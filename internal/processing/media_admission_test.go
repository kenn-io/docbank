package processing

import (
	"bytes"
	"encoding/json/v2"
	"testing"

	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/media/mediatest"
	"go.kenn.io/docbank/internal/store"
)

func TestReferencedMediaUsesUploadEligibilityAndLimit(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name, filename, mediaType, retainedType string
		content                                 []byte
		maximum                                 int64
		accepted                                bool
	}{
		{"wav", "call.wav", "audio/wav", "audio/wav", mediatest.WAV(), 1024, true},
		{"declared WAV alias", "call.wav", "audio/x-wav", "audio/wav", mediatest.WAV(), 1024, true},
		{"retained WAV alias", "call.wav", "audio/wav", "audio/x-wav", mediatest.WAV(), 1024, true},
		{"both WAV aliases", "call.wav", "audio/x-wav", "audio/x-wav", mediatest.WAV(), 1024, true},
		{"WAV parameters", "call.wav", "Audio/X-WAV; rate=16000", "audio/wav; rate=16000", mediatest.WAV(), 1024, true},
		{"MP3 parameters", "call.mp3", "Audio/MPEG; rate=44100", "audio/mpeg; rate=44100", mediatest.MP3(), 1024, true},
		{"mismatched retained type", "call.wav", "audio/wav", "audio/mpeg", mediatest.WAV(), 1024, false},
		{"text", "notes.txt", "text/plain", "text/plain", []byte("synthetic notes"), 1024, false},
		{"oversized", "call.wav", "audio/wav", "audio/wav", mediatest.WAV(), 1, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newPublicationFixture(t)
			written, err := fixture.blobs.WriteDetailedContext(t.Context(), bytes.NewReader(test.content))
			require.NoError(t, err)
			node, err := fixture.catalog.CreateFile(t.Context(), fixture.catalog.RootID(), test.filename,
				written.Hash, written.Size, test.retainedType, processingBlobPhysical(t, written))
			require.NoError(t, err)
			service, err := NewService(ServiceConfig{Catalog: fixture.catalog, Blobs: fixture.blobs,
				Gate: newWorkerTestGate(), SpoolDirectory: t.TempDir(), MediaMaxBytes: test.maximum,
			})
			require.NoError(t, err)
			request := SuppliedMediaRequest{
				OperationID: "00000000-0000-4000-8000-000000000701", Filename: test.filename,
				MediaType: test.mediaType, SHA256: written.Hash, ByteLength: written.Size,
				ExistingContentVersionID: node.CurrentVersionID,
				Occurrence:               MediaOccurrenceInput{Ref: "message", Revision: "1"},
			}
			retained, err := service.SubmitSuppliedMedia(t.Context(), request)
			if test.accepted {
				require.NoError(t, err)
				require.Equal(t, node.CurrentVersionID, retained.ContentVersionID)
				// A new upload of the same bytes also reuses the retained version.
				request.OperationID = "00000000-0000-4000-8000-000000000704"
				request.ExistingContentVersionID = ""
				request.Content = bytes.NewReader(test.content)
				repeated, err := service.SubmitSuppliedMedia(t.Context(), request)
				require.NoError(t, err)
				retained.OperationID = request.OperationID
				require.Equal(t, retained, repeated)
			} else {
				require.Error(t, err)
				_, total, err := fixture.catalog.MediaSources(t.Context(), service.principal, 0, 10)
				require.NoError(t, err)
				require.Zero(t, total)
			}
		})
	}
}

func TestMediaRejectsMissingProfileAndConsentBeforeSealing(t *testing.T) {
	t.Parallel()
	fixture := newPublicationFixture(t)
	provider := newWorkerProvider(t)
	record := workerProcessingProfile(t, provider.Descriptor())
	var profile document.ProcessingProfileV1
	require.NoError(t, json.Unmarshal(record.CanonicalProfile, &profile))
	profile.Rendition.TrustBoundary = string(provider.Descriptor().TrustBoundary)
	service, err := NewService(ServiceConfig{Catalog: fixture.catalog, Blobs: fixture.blobs,
		Gate: newWorkerTestGate(), SpoolDirectory: t.TempDir(),
		Profiles: map[string]ProfileConfig{"speech": {Profile: profile, RenditionProvider: provider}}})
	require.NoError(t, err)
	for _, test := range []struct {
		profile string
		want    error
	}{{"missing", ErrProfileNotConfigured}, {"speech", ErrConsentRequired}} {
		t.Run(test.profile, func(t *testing.T) {
			raw := mediatest.WAV()
			_, err = service.SubmitSuppliedMedia(t.Context(), SuppliedMediaRequest{
				OperationID: "00000000-0000-4000-8000-000000000702", Filename: "call.wav",
				MediaType: "audio/wav", Content: bytes.NewReader(raw), SHA256: processingHash(string(raw)),
				ByteLength: int64(len(raw)), Occurrence: MediaOccurrenceInput{Ref: "message", Revision: "1"},
				Processing: &MediaProcessingRequest{Profile: test.profile},
			})
			require.ErrorIs(t, err, test.want)
		})
	}
}

func TestMediaEnqueueRejectsEmbeddingOnlyProfile(t *testing.T) {
	t.Parallel()
	fixture, fake, _, original := newRealEmbeddingWorker(t, document.EmbeddingInputOriginalFile)
	var profile document.ProcessingProfileV1
	require.NoError(t, json.Unmarshal(original.Profile.CanonicalProfile, &profile))
	profile.Rendition = nil
	profile.RetentionDisclosure.RetainSanitizedMarkdown = false
	profile.RetentionDisclosure.RetainProviderMarkdown = false
	profile.RetentionDisclosure.RetainTypedArtifacts = true
	provider := &embeddingWorkerProvider{runtime: fake.runtime, binding: original.BindingID, descriptor: original.Descriptor}
	service, err := NewService(ServiceConfig{Catalog: fixture.catalog, Blobs: fixture.blobs,
		Gate: newWorkerTestGate(), SpoolDirectory: t.TempDir(),
		Profiles: map[string]ProfileConfig{"direct": {Profile: profile,
			EmbeddingProviders: map[string]document.EmbeddingProvider{original.BindingID: provider}}}})
	require.NoError(t, err)
	version, err := fixture.catalog.ContentVersionByID(t.Context(), original.ContentVersionID)
	require.NoError(t, err)
	selector := Selector{NodeID: version.NodeID, ContentVersionID: version.ID, Profile: "direct"}
	plan, err := service.Plan(t.Context(), selector)
	require.NoError(t, err)
	_, err = service.EnqueueAuthorized(t.Context(), selector, mediaSourceBinding{}, plan.Fingerprint, store.ProviderOperationAuthorizationRequest{}, "")
	require.ErrorContains(t, err, "rendition")
}
