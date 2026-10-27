package store

import (
	"database/sql"
	"encoding/json/v2"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/openaicompat"
	"testing"
)

func TestEmbeddingLegacyProfilesReuseOnlyMatchingProducer(t *testing.T) {
	t.Parallel()
	s, version, profile, _ := newEmbeddingCatalogFixture(t)
	descriptor := embeddingCatalogDescriptor()
	descriptor.ID = openaicompat.ProviderID
	descriptor.TrustBoundary = document.EmbeddingTrustOperatorNetwork
	descriptor.InputKinds = []document.EmbeddingInputKind{document.EmbeddingInputRenditionChunk}
	descriptor.DocumentFormatter, descriptor.QueryFormatter = openaicompat.DocumentFormatterV1, openaicompat.QueryFormatterV1
	adapter := openaicompat.Profile{Origin: "http://127.0.0.1:11434", Descriptor: descriptor, ModelInput: descriptor.ModelInput, DeploymentEpoch: descriptor.ModelRevision}
	policy, err := openaicompat.PolicyFingerprint(adapter)
	require.NoError(t, err)
	descriptor.PolicyFingerprint = policy
	descriptor, err = document.NewEmbeddingDescriptor(descriptor)
	require.NoError(t, err)
	var portable document.ProcessingProfileV1
	require.NoError(t, json.Unmarshal(profile.CanonicalProfile, &portable))
	portable.Embeddings = []document.EmbeddingBindingV1{workerProfileEmbeddingBinding(t, profile, "chunk")}
	for i := range portable.Embeddings {
		portable.Embeddings[i].Descriptor = document.ProviderDescriptorV1{ID: descriptor.ID, Fingerprint: descriptor.Fingerprint}
		portable.Embeddings[i].TrustBoundary = string(descriptor.TrustBoundary)
		portable.Embeddings[i].DocumentFormatter, portable.Embeddings[i].QueryFormatter = descriptor.DocumentFormatter, descriptor.QueryFormatter
	}
	canonical, fingerprints, err := document.CanonicalProfile(portable)
	require.NoError(t, err)
	profile.CanonicalProfile, profile.Fingerprint = canonical, fingerprints.Profile
	profile.RetentionDisclosureFingerprint = fingerprints.RetentionDisclosure
	require.NoError(t, s.withStorageTx(t.Context(), func(tx *sql.Tx) error { return ensureProcessingProfileTx(t.Context(), tx, profile) }))
	build := catalogRenditionBuild(s, profile)
	attachment := RenditionAttachmentRecord{ID: testSHA256([]byte("openai-compatible-attachment")), VaultID: s.VaultID(), ContentVersionID: version, BuildID: build.ID, Profile: profile, AttachedAt: embeddingCatalogTime}
	require.NoError(t, publishRenditionForTest(t, s, attachment, embeddingCatalogTime, testSHA256([]byte("openai-compatible-lexical"))))
	retained := embeddingSetFixture(s, version, profile.Fingerprint, document.EmbeddingInputRenditionChunk, "chunk", attachment.ID)
	retained.VectorSpace.Descriptor = descriptor
	retained, err = normalizeEmbeddingSetRecord(retained)
	require.NoError(t, err)
	require.NoError(t, s.StageEmbeddingSet(t.Context(), retained))
	work := EmbeddingJobWork{ContentVersionID: version, Binding: workerProfileEmbeddingBinding(t, profile, "chunk"), Descriptor: retained.VectorSpace.Descriptor, VectorSpaceID: testSHA256([]byte("new-space")), EmbeddingInputFingerprint: retained.EmbeddingInputFingerprint, InputGeneration: retained.InputGeneration}
	found, err := s.FindReusableEmbeddingSet(t.Context(), work)
	require.NoError(t, err)
	require.Equal(t, retained.ID, found.ID)
	work.Descriptor.ModelRevision = "other-revision"
	work.Descriptor, err = document.NewEmbeddingDescriptor(work.Descriptor)
	require.NoError(t, err)
	_, err = s.FindReusableEmbeddingSet(t.Context(), work)
	require.ErrorIs(t, err, ErrNotFound)
}
