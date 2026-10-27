package store

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"fmt"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/openaicompat"
)

func TestEmbeddingJobsDistinguishSelectedVectorSpace(t *testing.T) {
	t.Parallel()
	for _, testCase := range []struct {
		name, state string
		changed     bool
		enqueued    int
	}{
		{name: "same space", state: "completed"},
		{name: "changed space", state: "queued", changed: true, enqueued: 1},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			s, version, profile, _ := newEmbeddingCatalogFixture(t)
			request := embeddingJobTestRequest(t, s, version, profile, "configuration-round-trip")
			record := embeddingSetFixture(s, version, profile.Fingerprint, document.EmbeddingInputOriginalFile, request.BindingID, "")
			record.InputGeneration = request.InputGeneration
			legacy := publishReconcileFixture(t, s, request, record)
			request.VectorSpaceID = record.VectorSpace.ID
			if testCase.changed {
				request.VectorSpaceID = testSHA256([]byte("selected-v2-after-policy-round-trip"))
			}
			prepared := 0
			result, err := s.ReconcileEmbeddingJobs(t.Context(), EmbeddingReconcileRequest{At: time.Now(), Limit: 100, Mutate: embeddingTestMutation,
				Profiles: []ProcessingProfileRecord{profile}, Descriptors: []document.EmbeddingDescriptor{request.Descriptor},
				VectorSpaces: map[string]string{profile.Fingerprint + "/" + request.BindingID: request.VectorSpaceID},
				PrepareGeneration: func(_ context.Context, _ string, _ ProcessingProfileRecord, binding document.EmbeddingBindingV1) (EmbeddingInputGenerationRecord, error) {
					if binding.Name != request.BindingID {
						return EmbeddingInputGenerationRecord{}, ErrNotFound
					}
					prepared++
					return request.InputGeneration, nil
				},
			})
			require.NoError(t, err)
			require.Equal(t, testCase.enqueued, prepared)
			require.Equal(t, testCase.enqueued, result.Enqueued)
			if !testCase.changed {
				selected, err := s.EnqueueEmbeddingJob(t.Context(), request)
				require.NoError(t, err)
				require.Equal(t, legacy.ID, selected.ID)
			}
			var selectedSpace string
			require.NoError(t, s.db.QueryRow(`SELECT vector_space_id FROM embedding_jobs WHERE job_id=?`, legacy.ID).Scan(&selectedSpace))
			require.Equal(t, request.VectorSpaceID, selectedSpace)
			status, err := s.EmbeddingJobByID(t.Context(), legacy.ID)
			require.NoError(t, err)
			require.Equal(t, testCase.state, status.State)
		})
	}
}

func TestEmbeddingReconciliationUsesCurrentGenerationBeforeJobState(t *testing.T) {
	t.Parallel()
	for _, prepare := range []bool{false, true} {
		t.Run(strconv.FormatBool(prepare), func(t *testing.T) {
			t.Parallel()
			s, version, profile, attachment := newEmbeddingCatalogFixture(t)
			old := embeddingSetFixture(s, version, profile.Fingerprint, document.EmbeddingInputRenditionChunk, "chunk", attachment)
			old, err := normalizeEmbeddingSetRecord(old)
			require.NoError(t, err)
			binding := workerProfileEmbeddingBinding(t, profile, "chunk")
			consent := ProviderOperationAuthorizationRequest{Principal: "operator:current-generation", Scope: "embedding:chunk", ProfileFingerprint: profile.Fingerprint, DisclosureFingerprint: binding.DisclosureFingerprint, InputClasses: []string{string(binding.InputKind)}, RetainedArtifactClasses: []string{"embedding_vector_set"}}
			_, err = s.GrantConsent(t.Context(), ProcessingConsentGrantRequest{Principal: consent.Principal, Scope: consent.Scope, ProfileFingerprint: consent.ProfileFingerprint, DisclosureFingerprint: consent.DisclosureFingerprint, InputClasses: consent.InputClasses, RetainedArtifactClasses: consent.RetainedArtifactClasses})
			require.NoError(t, err)
			publishReconcileFixture(t, s, EmbeddingJobRequest{ContentVersionID: version, Profile: profile, BindingID: "chunk", Descriptor: old.VectorSpace.Descriptor, InputGeneration: old.InputGeneration, Authorization: consent}, old)
			build := catalogRenditionBuild(s, profile)
			build.ID = testSHA256([]byte("current-generation-build"))
			require.NoError(t, s.StageRenditionBuild(t.Context(), build))
			replacement := RenditionAttachmentRecord{VaultID: s.VaultID(), ContentVersionID: version, BuildID: build.ID, Profile: profile, AttachedAt: embeddingCatalogTime}
			var current EmbeddingSetRecord
			for i := 0; ; i++ {
				replacement.ID = testSHA256([]byte(fmt.Sprint("current-attachment-", i)))
				current = embeddingSetFixture(s, version, profile.Fingerprint, document.EmbeddingInputRenditionChunk, "chunk", replacement.ID)
				if current.InputGeneration.ID > old.InputGeneration.ID {
					break
				}
			}
			require.NoError(t, publishRenditionForTest(t, s, replacement, embeddingCatalogTime, testSHA256([]byte("current-generation-lexical"))))
			current, err = normalizeEmbeddingSetRecord(current)
			require.NoError(t, err)
			job, err := s.EnqueueEmbeddingJob(t.Context(), EmbeddingJobRequest{ContentVersionID: version, Profile: profile, BindingID: "chunk", Descriptor: current.VectorSpace.Descriptor, InputGeneration: current.InputGeneration, Authorization: consent})
			require.NoError(t, err)
			_, err = s.db.Exec(`UPDATE embedding_jobs SET state='abandoned' WHERE job_id=?`, job.ID)
			require.NoError(t, err)
			request := EmbeddingReconcileRequest{At: time.Now(), Limit: 100, Mutate: embeddingTestMutation, DescriptorFingerprints: []string{current.VectorSpace.Descriptor.Fingerprint},
				HydrateGeneration: func(_ context.Context, g EmbeddingInputGenerationRecord) (EmbeddingInputGenerationRecord, error) {
					return HydrateEmbeddingInputGeneration(g, current.InputGeneration.GenerationJSON, current.InputGeneration.EvidenceJSON)
				},
			}
			if prepare {
				request.Profiles = []ProcessingProfileRecord{profile}
				request.Descriptors = []document.EmbeddingDescriptor{current.VectorSpace.Descriptor}
				request.PrepareGeneration = func(_ context.Context, _ string, _ ProcessingProfileRecord, b document.EmbeddingBindingV1) (EmbeddingInputGenerationRecord, error) {
					if b.Name != "chunk" {
						return EmbeddingInputGenerationRecord{}, ErrNotFound
					}
					return current.InputGeneration, nil
				}
			}
			_, err = s.ReconcileEmbeddingJobs(t.Context(), request)
			require.NoError(t, err)
			status, err := s.EmbeddingJobByID(t.Context(), job.ID)
			require.NoError(t, err)
			require.Equal(t, "queued", status.State)
		})
	}
}

func TestEmbeddingReconciliationRuntimePolicyReplacementAuthority(t *testing.T) {
	t.Parallel()
	for _, testCase := range []struct {
		name, bindingID                          string
		sourceConfigured, purgeSource, unrelated bool
	}{
		{name: "optional", bindingID: "optional"},
		{name: "chunk", bindingID: "chunk"},
		{name: "concurrent endpoint", bindingID: "optional", sourceConfigured: true},
		{name: "purged source", bindingID: "optional", purgeSource: true},
		{name: "unrelated profile", bindingID: "optional", unrelated: true},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			bindingID := testCase.bindingID
			s, version, profile, attachment := newEmbeddingCatalogFixture(t)
			makeRequest := func(profile ProcessingProfileRecord, descriptor document.EmbeddingDescriptor, attachment string) EmbeddingJobRequest {
				binding := workerProfileEmbeddingBinding(t, profile, bindingID)
				record := embeddingSetFixture(s, version, profile.Fingerprint, binding.InputKind, bindingID, attachment)
				record, err := normalizeEmbeddingSetRecord(record)
				require.NoError(t, err)
				if binding.InputKind == document.EmbeddingInputOriginalFile {
					record.InputGeneration.ID = testSHA256([]byte(profile.Fingerprint + record.InputGeneration.ID))
				}
				consent := ProviderOperationAuthorizationRequest{Principal: "operator:runtime-policy", Scope: "embedding:" + bindingID, ProfileFingerprint: profile.Fingerprint, DisclosureFingerprint: binding.DisclosureFingerprint, InputClasses: []string{string(binding.InputKind)}, RetainedArtifactClasses: []string{"embedding_vector_set"}}
				_, err = s.GrantConsent(t.Context(), ProcessingConsentGrantRequest{Principal: consent.Principal, Scope: consent.Scope, ProfileFingerprint: consent.ProfileFingerprint, DisclosureFingerprint: consent.DisclosureFingerprint, InputClasses: consent.InputClasses, RetainedArtifactClasses: consent.RetainedArtifactClasses})
				require.NoError(t, err)
				return EmbeddingJobRequest{ContentVersionID: version, Profile: profile, BindingID: bindingID, Descriptor: descriptor, InputGeneration: record.InputGeneration, Authorization: consent}
			}
			if bindingID != "chunk" {
				attachment = ""
			}
			initialDescriptor := embeddingCatalogDescriptor()
			source := makeRequest(profile, initialDescriptor, attachment)
			_, err := s.EnqueueEmbeddingJob(t.Context(), source)
			require.NoError(t, err)
			if testCase.purgeSource {
				require.NoError(t, s.PutCurrentRenditionRoot(t.Context(), CurrentRenditionRoot{
					ID: "retained-policy-input", Kind: RenditionRootRetention, TargetKind: RenditionRootEmbeddingGeneration,
					TargetID: source.InputGeneration.ID, FencingToken: 1, RecordedAt: embeddingCatalogTime,
				}))
				_, err = s.PurgeDerivatives(t.Context(), PurgeRequest{ContentVersionIDs: []string{version}})
				require.NoError(t, err)
			}
			descriptor := source.Descriptor
			descriptor.PolicyFingerprint = testSHA256([]byte("new-endpoint-timeout-egress"))
			if testCase.unrelated {
				descriptor.ID = "synthetic-hosted"
				descriptor.TrustBoundary = document.EmbeddingTrustOperatorNetwork
			}
			descriptor, err = document.NewEmbeddingDescriptor(descriptor)
			require.NoError(t, err)
			var portable document.ProcessingProfileV1
			require.NoError(t, json.Unmarshal(profile.CanonicalProfile, &portable))
			for i := range portable.Embeddings {
				binding := &portable.Embeddings[i]
				binding.Descriptor = document.ProviderDescriptorV1{ID: descriptor.ID, Fingerprint: descriptor.Fingerprint}
				binding.TrustBoundary = string(descriptor.TrustBoundary)
				binding.DisclosureFingerprint = testSHA256([]byte("current-endpoint-disclosure"))
				binding.AuthorizationFingerprint = testSHA256([]byte("current-authorization-policy"))
				binding.CredentialBinding = "credential:current-endpoint"
				binding.MaxBatchItems = 64
				if binding.InputKind == document.EmbeddingInputRenditionChunk {
					binding.MaxInputTokens = 1024
				}
				binding.MaxInputBytes = 2 << 20
				binding.MaxResponseBytes = 2 << 20
			}
			canonical, fp, err := document.CanonicalProfile(portable)
			require.NoError(t, err)
			target := profile
			target.CanonicalProfile, target.Fingerprint = canonical, fp.Profile
			target.RenditionRequestFingerprint, target.EvidenceLexicalFingerprint = fp.RenditionRequest, fp.EvidenceLexical
			target.RetentionDisclosureFingerprint = fp.RetentionDisclosure
			require.NoError(t, s.withStorageTx(t.Context(), func(tx *sql.Tx) error { return ensureProcessingProfileTx(t.Context(), tx, target) }))
			if bindingID == "chunk" {
				build := catalogRenditionBuild(s, target)
				replacement := RenditionAttachmentRecord{ID: testSHA256([]byte("current-policy-attachment")), VaultID: s.VaultID(), ContentVersionID: version, BuildID: build.ID, Profile: target, AttachedAt: embeddingCatalogTime}
				require.NoError(t, publishRenditionForTest(t, s, replacement, embeddingCatalogTime, testSHA256([]byte("current-policy-lexical"))))
				attachment = replacement.ID
			}
			current := makeRequest(target, descriptor, attachment)
			prepared := 0
			prepare := func(_ context.Context, _ string, selected ProcessingProfileRecord, b document.EmbeddingBindingV1) (EmbeddingInputGenerationRecord, error) {
				if b.Name != current.BindingID {
					return EmbeddingInputGenerationRecord{}, ErrNotFound
				}
				if selected.Fingerprint == profile.Fingerprint {
					return source.InputGeneration, nil
				}
				prepared++
				return current.InputGeneration, nil
			}
			request := EmbeddingReconcileRequest{At: time.Now(), Limit: 100, Mutate: embeddingTestMutation, Profiles: []ProcessingProfileRecord{target}, Descriptors: []document.EmbeddingDescriptor{descriptor}, PrepareGeneration: prepare, HydrateGeneration: func(_ context.Context, g EmbeddingInputGenerationRecord) (EmbeddingInputGenerationRecord, error) {
				return HydrateEmbeddingInputGeneration(g, current.InputGeneration.GenerationJSON, current.InputGeneration.EvidenceJSON)
			}}
			if testCase.sourceConfigured {
				request.Profiles = append(request.Profiles, profile)
				request.Descriptors = append(request.Descriptors, initialDescriptor)
			}
			result, err := s.ReconcileEmbeddingJobs(t.Context(), request)
			require.NoError(t, err)
			if testCase.sourceConfigured || testCase.purgeSource || testCase.unrelated {
				require.Zero(t, prepared, "recovery must preserve concurrent selection and source purge authority")
				require.Zero(t, result.Enqueued)
				return
			}
			require.Equal(t, 1, result.Enqueued)
			jobs, err := s.EmbeddingJobsForVersionProfile(t.Context(), version, target.Fingerprint)
			require.NoError(t, err)
			require.Len(t, jobs, 1)
			require.Equal(t, "queued", jobs[0].State)
			claim, work, found, err := s.ClaimEmbeddingWork(t.Context(), jobs[0].ID, "worker:recovery", time.Now(), time.Minute, []string{descriptor.Fingerprint})
			require.NoError(t, err)
			require.True(t, found)
			require.Equal(t, source.Profile.Fingerprint, work.SourceProfileFingerprint)
			require.NoError(t, s.FailEmbeddingWork(t.Context(), claim, work, EmbeddingFailureInputRejected, EmbeddingAttemptReceipt{AttemptID: jobs[0].ID}, time.Now()))
			selected, err := s.EnqueueEmbeddingJob(t.Context(), current)
			require.NoError(t, err)
			require.Equal(t, jobs[0].ID, selected.ID, "explicit selection retries the existing target job")
			_, work, found, err = s.ClaimEmbeddingWork(t.Context(), selected.ID, "worker:explicit", time.Now().Add(time.Second), time.Minute, []string{descriptor.Fingerprint})
			require.NoError(t, err)
			require.True(t, found)
			require.Empty(t, work.SourceProfileFingerprint, "explicit selection admits provider fallback")
		})
	}
}

func TestEmbeddingReconciliationPurgeBeforeReplacementAdmission(t *testing.T) {
	t.Parallel()
	s, version, source, _ := newEmbeddingCatalogFixture(t)
	admitted := embeddingJobTestRequest(t, s, version, source, "purge-before-replacement")
	_, err := s.EnqueueEmbeddingJob(t.Context(), admitted)
	require.NoError(t, err)
	require.NoError(t, s.PutCurrentRenditionRoot(t.Context(), CurrentRenditionRoot{
		ID: "retained-admission-input", Kind: RenditionRootRetention, TargetKind: RenditionRootEmbeddingGeneration,
		TargetID: admitted.InputGeneration.ID, FencingToken: 1, RecordedAt: embeddingCatalogTime,
	}))
	descriptor := admitted.Descriptor
	descriptor.PolicyFingerprint = testSHA256([]byte("replacement-endpoint-policy"))
	descriptor, err = document.NewEmbeddingDescriptor(descriptor)
	require.NoError(t, err)
	var portable document.ProcessingProfileV1
	require.NoError(t, json.Unmarshal(source.CanonicalProfile, &portable))
	for i := range portable.Embeddings {
		portable.Embeddings[i].Descriptor.Fingerprint = descriptor.Fingerprint
	}
	canonical, fingerprints, err := document.CanonicalProfile(portable)
	require.NoError(t, err)
	target := source
	target.CanonicalProfile, target.Fingerprint = canonical, fingerprints.Profile
	target.RenditionRequestFingerprint, target.EvidenceLexicalFingerprint = fingerprints.RenditionRequest, fingerprints.EvidenceLexical
	target.RetentionDisclosureFingerprint = fingerprints.RetentionDisclosure
	binding := workerProfileEmbeddingBinding(t, target, admitted.BindingID)
	_, err = s.GrantConsent(t.Context(), ProcessingConsentGrantRequest{
		Principal: "operator:replacement", Scope: "embedding:replacement", ProfileFingerprint: target.Fingerprint,
		DisclosureFingerprint: binding.DisclosureFingerprint, InputClasses: []string{string(binding.InputKind)},
		RetainedArtifactClasses: []string{"embedding_vector_set"},
	})
	require.NoError(t, err)
	generation := admitted.InputGeneration
	generation.ID = testSHA256([]byte("replacement-admission-generation"))
	generation.ProcessingProfileFingerprint = target.Fingerprint
	purged := false
	result, err := s.ReconcileEmbeddingJobs(t.Context(), EmbeddingReconcileRequest{
		At: time.Now(), Limit: 100, Profiles: []ProcessingProfileRecord{target}, Descriptors: []document.EmbeddingDescriptor{descriptor},
		PrepareGeneration: func(ctx context.Context, _ string, _ ProcessingProfileRecord, selected document.EmbeddingBindingV1) (EmbeddingInputGenerationRecord, error) {
			if selected.Name != binding.Name {
				return EmbeddingInputGenerationRecord{}, ErrNotFound
			}
			var registered int
			require.NoError(t, s.db.QueryRow(`SELECT COUNT(*) FROM processing_profiles WHERE profile_fingerprint=?`, target.Fingerprint).Scan(&registered))
			require.Zero(t, registered, "the target has consent but has not admitted any work")
			_, err := s.PurgeDerivatives(ctx, PurgeRequest{ContentVersionIDs: []string{version}})
			require.NoError(t, err)
			purged = true
			return generation, nil
		},
		Mutate: embeddingTestMutation,
	})
	require.NoError(t, err)
	require.True(t, purged, "purge runs after discovery and immediately before admission")
	var jobs int
	require.NoError(t, s.db.QueryRow(`SELECT COUNT(*) FROM embedding_jobs WHERE content_version_id=? AND profile_fingerprint=?`, version, target.Fingerprint).Scan(&jobs))
	require.Zero(t, jobs, "the source purge must fence replacement work admitted under another profile")
	require.Zero(t, result.Enqueued)
}

func publishReconcileFixture(t *testing.T, s *Store, request EmbeddingJobRequest, record EmbeddingSetRecord) EmbeddingJob {
	t.Helper()
	job, err := s.EnqueueEmbeddingJob(t.Context(), request)
	require.NoError(t, err)
	at := time.Now()
	claim, work, found, err := s.ClaimEmbeddingWork(t.Context(), job.ID, "reconcile-fixture", at, time.Minute, []string{request.Descriptor.Fingerprint})
	require.NoError(t, err)
	require.True(t, found)
	require.NoError(t, s.StageEmbeddingSetWithLease(t.Context(), record, claim.AttemptID, claim.Epoch, at))
	prior, err := s.AuthorizeProviderOperation(t.Context(), request.Authorization)
	require.NoError(t, err)
	head := EmbeddingHeadRecord{Key: EmbeddingHeadKey{request.ContentVersionID, request.BindingID, work.Binding.InputKind}, SetID: record.ID, VectorSpaceID: work.VectorSpaceID, ProcessingProfileFingerprint: request.Profile.Fingerprint, PublishedAt: at.UTC().Format(timestampLayout), FencingToken: claim.Epoch}
	require.NoError(t, s.PublishEmbeddingWork(t.Context(), claim, work, head, prior, EmbeddingAttemptReceipt{AttemptID: job.ID}, at))
	return job
}

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
