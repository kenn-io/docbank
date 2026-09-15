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
	for _, test := range []struct {
		name, filename, mediaType string
		content                   []byte
		maximum                   int64
		accepted                  bool
	}{
		{"wav", "call.wav", "audio/wav", mediatest.WAV(), 1024, true},
		{"text", "notes.txt", "text/plain", []byte("synthetic notes"), 1024, false},
		{"oversized", "call.wav", "audio/wav", mediatest.WAV(), 1, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newPublicationFixture(t)
			written, err := fixture.blobs.WriteDetailedContext(t.Context(), bytes.NewReader(test.content))
			require.NoError(t, err)
			node, err := fixture.catalog.CreateFile(t.Context(), fixture.catalog.RootID(), test.filename,
				written.Hash, written.Size, test.mediaType, processingBlobPhysical(t, written))
			require.NoError(t, err)
			service, err := NewService(ServiceConfig{Catalog: fixture.catalog, Blobs: fixture.blobs,
				Gate: newWorkerTestGate(), SpoolDirectory: t.TempDir(), MediaMaxBytes: test.maximum,
			})
			require.NoError(t, err)
			_, err = service.SubmitSuppliedMedia(t.Context(), SuppliedMediaRequest{
				OperationID: "00000000-0000-4000-8000-000000000701", Filename: test.filename,
				MediaType: test.mediaType, SHA256: written.Hash, ByteLength: written.Size,
				ExistingContentVersionID: node.CurrentVersionID,
				Occurrence:               MediaOccurrenceInput{Ref: "message", Revision: "1"},
			})
			if test.accepted {
				require.NoError(t, err)
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
	_, err = service.EnqueueAuthorized(t.Context(), selector, plan.Fingerprint, store.ProviderOperationAuthorizationRequest{})
	require.ErrorContains(t, err, "rendition")
}
