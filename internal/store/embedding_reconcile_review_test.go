package store

import (
	"database/sql"
	"encoding/json/v2"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/openaicompat"
)

func checkEmbeddingLegacyProfilesReuseOnlyMatchingProducer(t *testing.T, open func(*testing.T) *Store) {
	t.Helper()
	t.Parallel()
	bge, err := document.NewModelInputContract(document.ModelInputContractConfig{Profile: document.ModelInputProfileBGEM3})
	require.NoError(t, err)
	for _, testCase := range []struct {
		name                    string
		modelInput              *document.ModelInputContract
		reusedAfterPolicyChange bool
	}{
		{name: "custom input reuses after endpoint move", reusedAfterPolicyChange: true},
		{name: "bge-m3 requires exact descriptor", modelInput: &bge},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			testLegacyProfileReuse(t, open(t), testCase.modelInput, testCase.reusedAfterPolicyChange)
		})
	}
}

func testLegacyProfileReuse(t *testing.T, s *Store, modelInput *document.ModelInputContract, reusedAfterPolicyChange bool) {
	t.Helper()
	s, version, profile, _ := seedEmbeddingCatalog(t, s)
	descriptor := embeddingCatalogDescriptor()
	if modelInput != nil {
		descriptor.ModelInput, descriptor.CompatibilityID = *modelInput, modelInput.CompatibilityID
	}
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
		portable.Embeddings[i].ModelInput, portable.Embeddings[i].CompatibilityID = descriptor.ModelInput, descriptor.CompatibilityID
	}
	canonical, fingerprints, err := document.CanonicalProfile(portable)
	require.NoError(t, err)
	profile.CanonicalProfile, profile.Fingerprint = canonical, fingerprints.Profile
	profile.RetentionDisclosureFingerprint = fingerprints.RetentionDisclosure
	require.NoError(t, s.withStorageTx(t.Context(), func(tx *sql.Tx) error { return ensureProcessingProfileTx(t.Context(), tx, profile) }))
	build := catalogRenditionBuild(s, profile)
	attachment := RenditionAttachmentRecord{ID: testSHA256([]byte("openai-compatible-attachment")), VaultID: s.VaultID(), ContentVersionID: version, BuildID: build.ID, Profile: profile, AttachedAt: embeddingCatalogTime}
	require.NoError(t, publishRenditionForTest(t, s, attachment, embeddingCatalogTime, testSHA256([]byte("openai-compatible-lexical"))))
	retained := embeddingSetFixture(t.Context(), s, version, profile.Fingerprint, document.EmbeddingInputRenditionChunk, "chunk", attachment.ID)
	retained.VectorSpace.Descriptor = descriptor
	retained, err = normalizeEmbeddingSetRecord(retained)
	require.NoError(t, err)
	require.NoError(t, s.StageEmbeddingSet(t.Context(), retained))
	binding := workerProfileEmbeddingBinding(t, profile, "chunk")
	consent := ProviderOperationAuthorizationRequest{Principal: "operator:producer-reuse", Scope: "embedding:producer-reuse", ProfileFingerprint: profile.Fingerprint, DisclosureFingerprint: binding.DisclosureFingerprint, InputClasses: []string{string(binding.InputKind)}, RetainedArtifactClasses: []string{"embedding_vector_set"}}
	_, err = s.GrantConsent(t.Context(), ProcessingConsentGrantRequest{Principal: consent.Principal, Scope: consent.Scope, ProfileFingerprint: consent.ProfileFingerprint, DisclosureFingerprint: consent.DisclosureFingerprint, InputClasses: consent.InputClasses, RetainedArtifactClasses: consent.RetainedArtifactClasses})
	require.NoError(t, err)
	job, err := s.EnqueueEmbeddingJob(t.Context(), EmbeddingJobRequest{ContentVersionID: version, Profile: profile, BindingID: binding.Name, Descriptor: descriptor, VectorSpaceID: testSHA256([]byte("new-space")), InputGeneration: retained.InputGeneration, Authorization: consent})
	require.NoError(t, err)
	_, work, claimed, err := s.ClaimEmbeddingWork(t.Context(), job.ID, "worker:producer-reuse", time.Now(), time.Minute, []string{descriptor.Fingerprint})
	require.NoError(t, err)
	require.True(t, claimed)
	found, err := s.FindReusableEmbeddingSet(t.Context(), work, "", "")
	require.NoError(t, err)
	require.Equal(t, retained.ID, found.ID)
	moved := work
	moved.Descriptor.PolicyFingerprint = testSHA256([]byte("moved-endpoint-policy"))
	moved.Descriptor, err = document.NewEmbeddingDescriptor(moved.Descriptor)
	require.NoError(t, err)
	found, err = s.FindReusableEmbeddingSet(t.Context(), moved, "", "")
	if reusedAfterPolicyChange {
		require.NoError(t, err)
		require.Equal(t, retained.ID, found.ID)
	} else {
		require.ErrorIs(t, err, ErrNotFound)
	}
	work.Descriptor.ModelRevision = "other-revision"
	work.Descriptor, err = document.NewEmbeddingDescriptor(work.Descriptor)
	require.NoError(t, err)
	_, err = s.FindReusableEmbeddingSet(t.Context(), work, "", "")
	require.ErrorIs(t, err, ErrNotFound)
}

func checkAutomaticVideoEmbeddingReconciliation(t *testing.T, open func(*testing.T) *Store) {
	t.Helper()
	t.Parallel()
	ctx := t.Context()
	s := open(t)
	descriptor := embeddingCatalogDescriptor()
	profile := catalogProcessingProfileWith(t, true, func(profile *document.ProcessingProfileV1) {
		binding := &profile.Embeddings[0]
		binding.Descriptor = document.ProviderDescriptorV1{ID: descriptor.ID, Fingerprint: descriptor.Fingerprint}
		binding.ModelInput, binding.ScalarEncoding = descriptor.ModelInput, descriptor.ScalarEncoding
		binding.TrustBoundary = string(descriptor.TrustBoundary)
		sibling := *binding
		sibling.Name, sibling.DisclosureFingerprint = "manual-only", fakeHash("e1")
		profile.Embeddings = append(profile.Embeddings, sibling)
	})
	require.NoError(t, s.withStorageTx(ctx, func(tx *sql.Tx) error { return ensureProcessingProfileTx(ctx, tx, profile) }))
	binding, fingerprints, err := embeddingBindingFromProfile(profile, "optional")
	require.NoError(t, err)
	sibling, _, err := embeddingBindingFromProfile(profile, "manual-only")
	require.NoError(t, err)
	add := func(name string, size int64, mime string) Node {
		t.Helper()
		node, err := s.CreateFile(ctx, s.RootID(), name, testSHA256([]byte(name)), size, mime)
		require.NoError(t, err)
		return node
	}
	first := add("first.mp4", 100, "video/mp4")
	second := add("second.mp4", 100, "video/mp4")
	excluded := add("excluded.mp4", 100, "video/mp4")
	asset, err := s.PhotoAssetForNode(ctx, excluded.ID)
	require.NoError(t, err)
	_, err = s.SetPhotoAssetExcluded(ctx, asset.ID, asset.Revision, true)
	require.NoError(t, err)
	oversized := add("oversized.mp4", binding.MaxInputBytes+1, "video/mp4")
	quicktime := add("video.mov", 100, "video/quicktime")
	photo := add("photo.jpg", 100, "image/jpeg")

	at := time.Now().UTC()
	key := profile.Fingerprint + "/" + binding.Name
	request := EmbeddingReconcileRequest{Limit: 1, At: at, DescriptorFingerprints: []string{descriptor.Fingerprint},
		Mutate: embeddingTestMutation, ImageProfiles: []ProcessingProfileRecord{profile},
		AutomaticVideoBindings: map[string]bool{key: true},
		VectorSpaces:           map[string]EmbeddingVectorSpaceRecord{key: {ID: fingerprints.VectorSpace[binding.Name], Descriptor: descriptor}}}
	sweep := func() int {
		t.Helper()
		pages := 0
		request.After, request.ImageAfter = "", ImageReconcilePosition{}
		for {
			page, err := s.ReconcileEmbeddingJobs(ctx, request)
			require.NoError(t, err)
			pages++
			request.After, request.ImageAfter = page.Next, page.ImageNext
			if page.ImageNext == (ImageReconcilePosition{}) {
				return pages
			}
			require.Less(t, pages, 3)
		}
	}
	grant := func(disclosure string, expiry *time.Time) {
		t.Helper()
		_, err := s.GrantConsent(ctx, ProcessingConsentGrantRequest{Principal: "operator:video", Scope: "embedding:video",
			ProfileFingerprint: profile.Fingerprint, DisclosureFingerprint: disclosure,
			InputClasses: []string{"original_file"}, RetainedArtifactClasses: []string{"embedding_vector_set"}, ExpiresAt: expiry})
		require.NoError(t, err)
	}
	jobs := func(node Node) []EmbeddingJobStatus {
		t.Helper()
		status, err := s.EmbeddingJobsForVersionProfile(ctx, node.CurrentVersionID, profile.Fingerprint)
		require.NoError(t, err)
		return status
	}
	grant(sibling.DisclosureFingerprint, nil)
	require.Equal(t, 2, sweep(), "video-only discovery progresses across a full page")
	require.Empty(t, jobs(first), "the sibling grant carries no automatic authority")
	expiry := at.Add(time.Hour)
	grant(binding.DisclosureFingerprint, &expiry)
	request.At = expiry
	sweep()
	require.Empty(t, jobs(first), "expired grant cannot admit work")
	request.At = time.Now().UTC()
	grant(binding.DisclosureFingerprint, nil)
	sweep()
	sweep()
	for _, node := range []Node{excluded, oversized, quicktime, photo} {
		require.Empty(t, jobs(node))
	}
	for _, node := range []Node{first, second} {
		status := jobs(node)
		require.Len(t, status, 1, "recovery may only schedule the opted-in binding")
		require.Equal(t, binding.Name, status[0].BindingID)
		require.True(t, status[0].AutomaticVideo)
		explicit, err := s.EmbeddingJobsForVersionProfileConsentSet(ctx, node.CurrentVersionID, profile.Fingerprint, status[0].AuthorizationGrantID)
		require.NoError(t, err)
		require.Empty(t, explicit, "the explicit build status excludes automatic jobs")
	}
	claim, work, ok, err := s.ClaimEmbeddingWork(ctx, jobs(first)[0].ID, "video-worker", time.Now(), time.Minute, request.DescriptorFingerprints)
	require.NoError(t, err)
	require.True(t, ok)
	require.True(t, work.InputGeneration.IsAutomaticVideo())
	asset, err = s.PhotoAssetForNode(ctx, first.ID)
	require.NoError(t, err)
	asset, err = s.SetPhotoAssetExcluded(ctx, asset.ID, asset.Revision, true)
	require.NoError(t, err)
	_, fence, err := s.BeginEmbeddingProviderEgress(ctx, claim, work, nil, time.Now())
	require.ErrorIs(t, err, ErrEmbeddingJobFenced)
	require.Nil(t, fence)
	require.NoError(t, s.AbandonEmbeddingWork(ctx, claim, time.Now()))
	sweep()
	require.Equal(t, "abandoned", jobs(first)[0].State, "ineligible displays cannot recover")
	_, err = s.SetPhotoAssetExcluded(ctx, asset.ID, asset.Revision, false)
	require.NoError(t, err)
	request.AutomaticVideoBindings = nil
	sweep()
	require.Equal(t, "abandoned", jobs(first)[0].State, "opt-out also fences retained automatic work")

	claim, work, ok, err = s.ClaimEmbeddingWork(ctx, jobs(second)[0].ID, "video-worker", time.Now(), time.Minute, request.DescriptorFingerprints)
	require.NoError(t, err)
	require.True(t, ok)
	_, err = s.RevokeConsent(ctx, ProcessingConsentRevocationRequest{Principal: "operator:video", Scope: "embedding:video"})
	require.NoError(t, err)
	_, fence, err = s.BeginEmbeddingProviderEgress(ctx, claim, work, nil, time.Now())
	require.ErrorIs(t, err, ErrProcessingConsentRevoked)
	require.Nil(t, fence)
	require.NoError(t, s.AbandonEmbeddingWork(ctx, claim, time.Now()))

	_, err = s.PurgeDerivatives(ctx, PurgeRequest{ContentVersionIDs: []string{first.CurrentVersionID}})
	require.NoError(t, err)
	grant(binding.DisclosureFingerprint, nil)
	request.At = time.Now().UTC()
	request.AutomaticVideoBindings = map[string]bool{key: true}
	sweep()
	for _, job := range jobs(first) {
		require.NotEqual(t, "queued", job.State, "purge suppression blocks a fresh automatic grant")
	}
	require.Len(t, jobs(second), 2, "new authorization may admit work for an unpurged display")
}
