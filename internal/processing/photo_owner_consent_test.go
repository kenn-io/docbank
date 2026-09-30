package processing

import (
	"bytes"
	"encoding/json/v2"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/media/mediatest"
	"go.kenn.io/docbank/internal/store"
)

func TestConsentBelongsToTheRequestPhotoOwner(t *testing.T) {
	fixture := newPublicationFixture(t)
	descriptor, err := document.NewRenditionDescriptor(document.RenditionDescriptor{
		ID: "synthetic.owner-consent-v1", ContractVersion: document.RenditionProviderContractVersion,
		PolicyFingerprint: processingHash("owner-consent-policy"),
		TrustBoundary:     document.RenditionTrustLocalProcess,
		SupportedFormats: []document.RenditionFormatCapability{{
			MediaFamily: "audio", MediaType: "audio/wav", InputKind: document.RenditionInputOriginalFile,
		}},
		ReturnsStructured: true,
		ArtifactRoles:     []document.EvidenceArtifactRole{document.EvidenceArtifactStructured},
	})
	require.NoError(t, err)
	provider := &mediaWorkerProvider{workerProvider: &workerProvider{descriptor: descriptor}}
	written, err := fixture.blobs.WriteDetailedContext(t.Context(), bytes.NewReader(mediatest.WAV()))
	require.NoError(t, err)
	node, err := fixture.catalog.CreateFile(t.Context(), fixture.catalog.RootID(), "source.wav",
		written.Hash, written.Size, "audio/wav", processingBlobPhysical(t, written))
	require.NoError(t, err)
	var portable document.ProcessingProfileV1
	require.NoError(t, json.Unmarshal(workerProcessingProfile(t, descriptor).CanonicalProfile, &portable))
	portable.Rendition.TrustBoundary = string(descriptor.TrustBoundary)
	embeddingFixture := newEmbeddingWorkerFixture(t)
	var embeddingProfile document.ProcessingProfileV1
	require.NoError(t, json.Unmarshal(embeddingFixture.profile.CanonicalProfile, &embeddingProfile))
	var direct document.EmbeddingBindingV1
	for _, binding := range embeddingProfile.Embeddings {
		if binding.InputKind == document.EmbeddingInputOriginalFile {
			direct = binding
		}
	}
	require.NotEmpty(t, direct.Name)
	portable.Embeddings = []document.EmbeddingBindingV1{direct}
	service, err := NewService(ServiceConfig{
		Catalog: fixture.catalog, Blobs: fixture.blobs, Gate: newWorkerTestGate(),
		SpoolDirectory: t.TempDir(), Principal: "operator:synthetic", Scope: "document-processing",
		Profiles: map[string]ProfileConfig{"speech": {Profile: portable, RenditionProvider: provider,
			EmbeddingProviders: map[string]document.EmbeddingProvider{direct.Name: &embeddingWorkerProvider{
				runtime: embeddingFixture.runtime, binding: direct.Name, descriptor: embeddingFixture.descriptor}},
			EmbeddingClassifiers: map[string]func(error) (EmbeddingProviderFailure, time.Duration){
				direct.Name: embeddingFixture.runtime.Classify},
		}},
	})
	require.NoError(t, err)
	firstID := enrollPhotoOwner(t, fixture.catalog, "First")
	firstCtx := store.WithPhotoOwner(t.Context(), firstID)
	secondCtx := store.WithPhotoOwner(t.Context(), enrollPhotoOwner(t, fixture.catalog, "Second"))

	selector := Selector{NodeID: node.ID, ContentVersionID: node.CurrentVersionID, Profile: "speech"}
	plan, err := service.Plan(firstCtx, selector)
	require.NoError(t, err)
	require.True(t, plan.ConsentRequired)
	_, err = service.GrantConsent(firstCtx, ConsentGrantRequest{Selector: selector, PlanFingerprint: plan.Fingerprint})
	require.NoError(t, err)
	plan, err = service.Plan(firstCtx, selector)
	require.NoError(t, err)
	require.False(t, plan.ConsentRequired)
	plan, err = service.Plan(secondCtx, selector)
	require.NoError(t, err)
	require.True(t, plan.ConsentRequired, "consent granted by one owner must not authorize another")
	plan, err = service.Plan(t.Context(), selector)
	require.NoError(t, err)
	require.True(t, plan.ConsentRequired, "the configured principal keeps its own grants")

	// The embedding grant belongs to the owner too, where embedding runs authorize.
	embedding := func(principal string) store.ProviderOperationAuthorizationRequest {
		return store.ProviderOperationAuthorizationRequest{Principal: principal, Scope: "document-processing",
			ProfileFingerprint: service.profiles["speech"].record.Fingerprint, DisclosureFingerprint: direct.DisclosureFingerprint,
			InputClasses: []string{string(direct.InputKind)}, RetainedArtifactClasses: []string{"embedding_vector_set"}}
	}
	_, err = fixture.catalog.AuthorizeProviderOperation(t.Context(), embedding("owner:"+firstID))
	require.NoError(t, err)
	_, err = fixture.catalog.AuthorizeProviderOperation(t.Context(), embedding("operator:synthetic"))
	require.Error(t, err, "an owner's embedding grant must not authorize the configured principal")
}
