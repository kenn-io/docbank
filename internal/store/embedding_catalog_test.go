package store

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/document"
	docsqlite "go.kenn.io/docbank/sqlite"
	"go.kenn.io/docbank/sqlite/modernc"
)

func TestEmbeddingCatalogChunkAndDirectFileHeadsCoexist(t *testing.T) {
	s, versionID, profile, attachmentID := newEmbeddingCatalogFixture(t)
	direct := embeddingSetFixture(s, versionID, profile.Fingerprint, document.EmbeddingInputOriginalFile, "optional", "")
	chunk := embeddingSetFixture(s, versionID, profile.Fingerprint, document.EmbeddingInputRenditionChunk, "chunk", attachmentID)
	require.NoError(t, s.StageEmbeddingSet(t.Context(), direct))
	require.NoError(t, s.StageEmbeddingSet(t.Context(), chunk))
	require.NoError(t, s.PublishEmbeddingHead(t.Context(), EmbeddingHeadRecord{
		FencingToken: 1,
		Key:          EmbeddingHeadKey{ContentVersionID: versionID, BindingID: "optional", InputKind: document.EmbeddingInputOriginalFile},
		SetID:        direct.ID, VectorSpaceID: direct.VectorSpace.ID,
		ProcessingProfileFingerprint: profile.Fingerprint, PublishedAt: embeddingCatalogTime,
	}))
	require.NoError(t, s.PublishEmbeddingHead(t.Context(), EmbeddingHeadRecord{
		FencingToken: 1,
		Key:          EmbeddingHeadKey{ContentVersionID: versionID, BindingID: "chunk", InputKind: document.EmbeddingInputRenditionChunk},
		SetID:        chunk.ID, VectorSpaceID: chunk.VectorSpace.ID,
		ProcessingProfileFingerprint: profile.Fingerprint, PublishedAt: embeddingCatalogTime,
	}))

	assert.Equal(t, direct.ID, embeddingHeadSetIDForTest(t, s, versionID, profile.Fingerprint,
		"optional", document.EmbeddingInputOriginalFile))
	assert.Equal(t, chunk.ID, embeddingHeadSetIDForTest(t, s, versionID, profile.Fingerprint,
		"chunk", document.EmbeddingInputRenditionChunk))
}

func TestEmbeddingCatalogDeduplicatesExactSetsButFencesAttachmentContext(t *testing.T) {
	s, versionID, profile, attachmentID := newEmbeddingCatalogFixture(t)
	record := embeddingSetFixture(s, versionID, profile.Fingerprint, document.EmbeddingInputRenditionChunk, "chunk", attachmentID)
	require.NoError(t, s.StageEmbeddingSet(t.Context(), record))
	require.NoError(t, s.StageEmbeddingSet(t.Context(), record))

	var sets, generations, vectorSets int
	require.NoError(t, s.db.QueryRow(`SELECT
		(SELECT COUNT(*) FROM embedding_sets),
		(SELECT COUNT(*) FROM embedding_input_generations),
		(SELECT COUNT(*) FROM embedding_vector_sets)`).Scan(&sets, &generations, &vectorSets))
	assert.Equal(t, 1, sets)
	assert.Equal(t, 1, generations)
	assert.Equal(t, 1, vectorSets)

	var secondVersion, buildID string
	require.NoError(t, s.db.QueryRow(`SELECT version_id FROM content_versions WHERE version_id<>? AND blob_hash=? ORDER BY version_id LIMIT 1`, versionID, catalogSourceHash).Scan(&secondVersion))
	require.NoError(t, s.db.QueryRow(`SELECT build_id FROM rendition_attachments WHERE attachment_id=?`, attachmentID).Scan(&buildID))
	secondAttachment := testSHA256([]byte("second-embedding-attachment"))
	secondAttachmentRecord := RenditionAttachmentRecord{
		ID: secondAttachment, VaultID: s.VaultID(), ContentVersionID: secondVersion,
		BuildID: buildID, Profile: profile, AttachedAt: embeddingCatalogTime,
	}
	require.NoError(t, publishRenditionForTest(
		t, s, secondAttachmentRecord, embeddingCatalogTime, testSHA256([]byte("second-embedding-lexical-generation")),
	))
	second := embeddingSetFixture(s, secondVersion, profile.Fingerprint, document.EmbeddingInputRenditionChunk, "chunk", secondAttachment)
	require.NoError(t, s.StageEmbeddingSet(t.Context(), second))
	assert.NotEqual(t, record.InputGeneration.ID, second.InputGeneration.ID)
	require.NoError(t, s.db.QueryRow(`SELECT COUNT(*) FROM embedding_input_generations`).Scan(&generations))
	assert.Equal(t, 2, generations, "attachment identities must not share catalog generation authority")
}

func TestHydrateEmbeddingInputGenerationRequiresExactCanonicalArtifact(t *testing.T) {
	s, versionID, profile, attachmentID := newEmbeddingCatalogFixture(t)
	record := embeddingSetFixture(s, versionID, profile.Fingerprint,
		document.EmbeddingInputRenditionChunk, "chunk", attachmentID)
	record, err := normalizeEmbeddingSetRecord(record)
	require.NoError(t, err)
	projection := record.InputGeneration
	artifact := append([]byte(nil), projection.GenerationJSON...)
	projection.GenerationJSON = nil

	hydrated, err := HydrateEmbeddingInputGeneration(projection, artifact, record.InputGeneration.EvidenceJSON)
	require.NoError(t, err)
	assert.Equal(t, record.InputGeneration, hydrated)

	corrupt := append([]byte(nil), artifact...)
	corrupt[len(corrupt)-1] ^= 1
	_, err = HydrateEmbeddingInputGeneration(projection, corrupt, record.InputGeneration.EvidenceJSON)
	require.ErrorContains(t, err, "exact artifact")
}

func TestEmbeddingCatalogLeasedStageAndPublishAreAtomicallyFenced(t *testing.T) {
	s, versionID, profile, _ := newEmbeddingCatalogFixture(t)
	record := embeddingSetFixture(s, versionID, profile.Fingerprint,
		document.EmbeddingInputOriginalFile, "optional", "")
	require.NoError(t, s.StageEmbeddingSet(t.Context(), record))

	root := CurrentRenditionRoot{
		ID: "embedding-worker-attempt", Kind: RenditionRootWorkerLease,
		TargetKind: RenditionRootEmbeddingGeneration, TargetID: record.InputGeneration.ID,
		FencingToken: 1, RecordedAt: embeddingCatalogTime,
		ExpiresAt: "2099-08-25T10:00:00.000000000Z",
	}
	require.NoError(t, s.PutCurrentRenditionRoot(t.Context(), root))
	at := time.Now().UTC()
	require.ErrorIs(t, s.StageEmbeddingSetWithLease(
		t.Context(), record, root.ID, root.FencingToken+1, at),
		ErrCurrentRenditionRootFenced)
	require.NoError(t, s.StageEmbeddingSetWithLease(
		t.Context(), record, root.ID, root.FencingToken, at))

	consent := ProviderOperationAuthorizationRequest{
		Principal: "operator:embedding-worker", Scope: "embedding:optional",
		ProfileFingerprint:      profile.Fingerprint,
		DisclosureFingerprint:   workerOptionalEmbeddingBinding(t, profile).DisclosureFingerprint,
		InputClasses:            []string{string(document.EmbeddingInputOriginalFile)},
		RetainedArtifactClasses: []string{"embedding_vector_set"},
	}
	_, err := s.GrantConsent(t.Context(), ProcessingConsentGrantRequest{
		Principal: consent.Principal, Scope: consent.Scope,
		ProfileFingerprint:      consent.ProfileFingerprint,
		DisclosureFingerprint:   consent.DisclosureFingerprint,
		InputClasses:            consent.InputClasses,
		RetainedArtifactClasses: consent.RetainedArtifactClasses,
	})
	require.NoError(t, err)
	prior, err := s.AuthorizeProviderOperation(t.Context(), consent)
	require.NoError(t, err)
	head := EmbeddingHeadRecord{
		FencingToken: 1,
		Key:          EmbeddingHeadKey{ContentVersionID: versionID, BindingID: record.BindingID, InputKind: record.InputKind},
		SetID:        record.ID, VectorSpaceID: record.VectorSpace.ID,
		ProcessingProfileFingerprint: profile.Fingerprint, PublishedAt: embeddingCatalogTime,
	}
	_, err = s.PublishEmbeddingHeadWithLease(t.Context(), head, consent, prior,
		root.ID, root.FencingToken+1, at)
	require.ErrorIs(t, err, ErrCurrentRenditionRootFenced)
	var count int
	require.NoError(t, s.db.QueryRow(`SELECT COUNT(*) FROM embedding_heads`).Scan(&count))
	require.Zero(t, count)

	_, err = s.PublishEmbeddingHeadWithLease(t.Context(), head, consent, prior,
		root.ID, root.FencingToken, at)
	require.NoError(t, err)
	assert.Equal(t, record.ID, embeddingHeadSetIDForTest(t, s, versionID, profile.Fingerprint, record.BindingID, record.InputKind))

	_, err = s.RevokeConsent(t.Context(), ProcessingConsentRevocationRequest{
		Principal: consent.Principal, Scope: consent.Scope,
	})
	require.NoError(t, err)
	_, err = s.PublishEmbeddingHeadWithLease(t.Context(), head, consent, prior,
		root.ID, root.FencingToken, at.Add(time.Second))
	require.ErrorIs(t, err, ErrProcessingConsentRevoked)
	assert.Equal(t, record.ID, embeddingHeadSetIDForTest(t, s, versionID, profile.Fingerprint, record.BindingID, record.InputKind))
}

func TestEmbeddingJobCatalogClaimsRetriesAndResumesDurably(t *testing.T) {
	s, versionID, profile, _ := newEmbeddingCatalogFixture(t)
	record := embeddingSetFixture(s, versionID, profile.Fingerprint,
		document.EmbeddingInputOriginalFile, "optional", "")
	binding := workerOptionalEmbeddingBinding(t, profile)
	consent := ProviderOperationAuthorizationRequest{
		Principal: "operator:embedding-worker", Scope: "embedding:optional",
		ProfileFingerprint: profile.Fingerprint, DisclosureFingerprint: binding.DisclosureFingerprint,
		InputClasses: []string{string(binding.InputKind)}, RetainedArtifactClasses: []string{"embedding_vector_set"},
	}
	_, err := s.GrantConsent(t.Context(), ProcessingConsentGrantRequest{
		Principal: consent.Principal, Scope: consent.Scope, ProfileFingerprint: consent.ProfileFingerprint,
		DisclosureFingerprint: consent.DisclosureFingerprint, InputClasses: consent.InputClasses,
		RetainedArtifactClasses: consent.RetainedArtifactClasses,
	})
	require.NoError(t, err)
	job, err := s.EnqueueEmbeddingJob(t.Context(), EmbeddingJobRequest{
		ContentVersionID: versionID, Profile: profile, BindingID: binding.Name,
		Descriptor: record.VectorSpace.Descriptor, InputGeneration: record.InputGeneration,
		Authorization: consent,
	})
	require.NoError(t, err)

	at := time.Now().UTC()
	_, _, found, err := s.ClaimNextEmbeddingWork(t.Context(), "unconfigured-worker", at, 5*time.Minute, []string{fakeHash("different-runtime")})
	require.NoError(t, err)
	require.False(t, found, "unconfigured runtimes must leave queued jobs untouched")
	claim, work, found, err := s.ClaimNextEmbeddingWork(t.Context(), "worker-a", at, 5*time.Minute, []string{record.VectorSpace.Descriptor.Fingerprint})
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, job.ID, claim.AttemptID)
	assert.Equal(t, record.InputGeneration.ID, work.InputGeneration.ID)
	_, _, found, err = s.ClaimNextEmbeddingWork(t.Context(), "worker-b", at, 5*time.Minute, []string{record.VectorSpace.Descriptor.Fingerprint})
	require.NoError(t, err)
	assert.False(t, found, "a live lease must exclude a concurrent worker")

	_, err = s.RenewEmbeddingWork(t.Context(), EmbeddingJobClaim{
		AttemptID: claim.AttemptID, Owner: claim.Owner, Epoch: claim.Epoch + 1,
	}, at.Add(time.Minute), 5*time.Minute)
	require.ErrorIs(t, err, ErrEmbeddingJobFenced)
	require.NoError(t, s.FailEmbeddingWork(t.Context(), claim, work,
		EmbeddingFailureProviderUnavailable, EmbeddingAttemptReceipt{AttemptID: claim.AttemptID,
			ProviderFingerprint: work.Descriptor.Fingerprint, ProfileFingerprint: profile.Fingerprint,
			BindingID: binding.Name, InputKind: binding.InputKind, Rows: 1, Dimensions: binding.Dimensions,
			ProviderCalls: 3, Retries: 2, Elapsed: time.Second, FailureCode: string(EmbeddingFailureProviderUnavailable)}, at.Add(time.Minute)))

	_, _, found, err = s.ClaimNextEmbeddingWork(t.Context(), "worker-b", at.Add(time.Minute), 5*time.Minute, []string{record.VectorSpace.Descriptor.Fingerprint})
	require.NoError(t, err)
	assert.False(t, found, "durable retry delay must prevent a hot loop")
	resumed, _, found, err := s.ClaimNextEmbeddingWork(t.Context(), "worker-b", at.Add(2*time.Minute), 5*time.Minute, []string{record.VectorSpace.Descriptor.Fingerprint})
	require.NoError(t, err)
	require.True(t, found)
	assert.Greater(t, resumed.Epoch, claim.Epoch)

	restored, err := Open(s.path, s.driver)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, restored.Close()) })
	_, _, found, err = restored.ClaimNextEmbeddingWork(t.Context(), "worker-c", at.Add(6*time.Minute), 5*time.Minute, []string{record.VectorSpace.Descriptor.Fingerprint})
	require.NoError(t, err)
	assert.False(t, found, "the unexpired resumed lease must survive daemon restart")
}

func TestEmbeddingJobCatalogClaimsExactRequestedJob(t *testing.T) {
	s, versionID, profile, _ := newEmbeddingCatalogFixture(t)
	firstRequest := embeddingJobTestRequest(t, s, versionID, profile, "target-first")
	secondRequest := embeddingJobTestRequest(t, s, versionID, profile, "target-second")
	first, err := s.EnqueueEmbeddingJob(t.Context(), firstRequest)
	require.NoError(t, err)
	second, err := s.EnqueueEmbeddingJob(t.Context(), secondRequest)
	require.NoError(t, err)

	claim, work, found, err := s.ClaimEmbeddingWork(t.Context(), second.ID,
		"target-worker", time.Now().UTC(), 5*time.Minute, []string{secondRequest.Descriptor.Fingerprint})
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, second.ID, claim.AttemptID)
	require.Equal(t, secondRequest.InputGeneration.ID, work.InputGeneration.ID)
	firstStatus, err := s.EmbeddingJobByID(t.Context(), first.ID)
	require.NoError(t, err)
	require.Equal(t, "queued", firstStatus.State)
}

func TestEmbeddingJobsRebuildFromPortableAuthorityAfterMetadataRestore(t *testing.T) {
	source, versionID, profile, _ := newEmbeddingCatalogFixture(t)
	record := embeddingSetFixture(source, versionID, profile.Fingerprint,
		document.EmbeddingInputOriginalFile, "optional", "")
	binding := workerOptionalEmbeddingBinding(t, profile)
	consent := ProviderOperationAuthorizationRequest{
		Principal: "operator:embedding-rebuild", Scope: "embedding:optional",
		ProfileFingerprint: profile.Fingerprint, DisclosureFingerprint: binding.DisclosureFingerprint,
		InputClasses: []string{string(binding.InputKind)}, RetainedArtifactClasses: []string{"embedding_vector_set"},
	}
	_, err := source.GrantConsent(t.Context(), ProcessingConsentGrantRequest{
		Principal: consent.Principal, Scope: consent.Scope, ProfileFingerprint: consent.ProfileFingerprint,
		DisclosureFingerprint: consent.DisclosureFingerprint, InputClasses: consent.InputClasses,
		RetainedArtifactClasses: consent.RetainedArtifactClasses,
	})
	require.NoError(t, err)
	request := EmbeddingJobRequest{ContentVersionID: versionID, Profile: profile, BindingID: binding.Name,
		Descriptor: record.VectorSpace.Descriptor, InputGeneration: record.InputGeneration, Authorization: consent}
	_, err = source.EnqueueEmbeddingJob(t.Context(), request)
	require.NoError(t, err)
	at := time.Now().UTC()
	_, _, found, err := source.ClaimNextEmbeddingWork(t.Context(), "worker-before-backup", at, 5*time.Minute, []string{request.Descriptor.Fingerprint})
	require.NoError(t, err)
	require.True(t, found)

	var metadata bytes.Buffer
	require.NoError(t, source.ExportMetadata(t.Context(), &metadata))
	assert.NotContains(t, metadata.String(), `"type":"embedding_job"`,
		"jobs are rebuildable operational state, not portable authority")
	target, err := Open(filepath.Join(t.TempDir(), "embedding-job-restore.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, target.Close()) })
	require.NoError(t, target.ImportMetadata(t.Context(), bytes.NewReader(metadata.Bytes())))
	var jobs int
	require.NoError(t, target.db.QueryRow(`SELECT COUNT(*) FROM embedding_jobs`).Scan(&jobs))
	assert.Zero(t, jobs)
	_, err = target.GrantConsent(t.Context(), ProcessingConsentGrantRequest{
		Principal: consent.Principal, Scope: consent.Scope, ProfileFingerprint: consent.ProfileFingerprint,
		DisclosureFingerprint: consent.DisclosureFingerprint, InputClasses: consent.InputClasses,
		RetainedArtifactClasses: consent.RetainedArtifactClasses,
	})
	require.NoError(t, err, "restore starts a new consent incarnation before jobs may rebuild")
	reconciled, err := target.ReconcileEmbeddingJobs(t.Context(), EmbeddingReconcileRequest{Mutate: embeddingTestMutation,
		At: time.Now().UTC(), Limit: 100,
		DescriptorFingerprints: []string{request.Descriptor.Fingerprint},
	})
	require.NoError(t, err)
	assert.Positive(t, reconciled.Enqueued)
	require.NoError(t, target.db.QueryRow(`SELECT COUNT(*) FROM embedding_jobs`).Scan(&jobs))
	assert.Equal(t, reconciled.Enqueued, jobs, "reconciliation deterministically rebuilds operational jobs")
	resumed, _, found, err := target.ClaimNextEmbeddingWork(t.Context(), "worker-after-restore", at.Add(6*time.Minute), 5*time.Minute, []string{request.Descriptor.Fingerprint})
	require.NoError(t, err)
	require.True(t, found)
	assert.Positive(t, resumed.Epoch,
		"restore rebuilds both the operational job and its vault-local lease sequence")
}

func TestEmbeddingJobReconciliationRequiresCurrentAuthorityAndFreshConsent(t *testing.T) {
	s, versionID, profile, _ := newEmbeddingCatalogFixture(t)
	request := embeddingJobTestRequest(t, s, versionID, profile, "reconcile")
	job, err := s.EnqueueEmbeddingJob(t.Context(), request)
	require.NoError(t, err)
	_, err = s.db.Exec(`DELETE FROM embedding_jobs WHERE job_id=?`, job.ID)
	require.NoError(t, err)

	result, err := s.ReconcileEmbeddingJobs(t.Context(), EmbeddingReconcileRequest{Mutate: embeddingTestMutation,
		At: time.Now().UTC(), Limit: 1,
		DescriptorFingerprints: []string{request.Descriptor.Fingerprint},
	})
	require.NoError(t, err)
	assert.Equal(t, 1, result.Examined)
	assert.Equal(t, 2, result.Enqueued)

	_, err = s.db.Exec(`DELETE FROM embedding_jobs`)
	require.NoError(t, err)
	_, err = s.RevokeConsent(t.Context(), ProcessingConsentRevocationRequest{
		Principal: request.Authorization.Principal, Scope: request.Authorization.Scope,
	})
	require.NoError(t, err)
	result, err = s.ReconcileEmbeddingJobs(t.Context(), EmbeddingReconcileRequest{Mutate: embeddingTestMutation,
		At: time.Now().UTC(), Limit: 1,
		DescriptorFingerprints: []string{request.Descriptor.Fingerprint},
	})
	require.NoError(t, err)
	assert.Zero(t, result.Enqueued)
}

func TestEmbeddingJobReconciliationRequiresExactChunkBindingPolicy(t *testing.T) {
	s, versionID, profile, attachmentID := newEmbeddingCatalogFixture(t)
	record := embeddingSetFixture(s, versionID, profile.Fingerprint,
		document.EmbeddingInputRenditionChunk, "chunk", attachmentID)
	var err error
	record, err = normalizeEmbeddingSetRecord(record)
	require.NoError(t, err)
	binding := workerProfileEmbeddingBinding(t, profile, "chunk")
	consent := ProviderOperationAuthorizationRequest{
		Principal: "operator:chunk-reconcile", Scope: "embedding:chunk",
		ProfileFingerprint: profile.Fingerprint, DisclosureFingerprint: binding.DisclosureFingerprint,
		InputClasses: []string{string(binding.InputKind)}, RetainedArtifactClasses: []string{"embedding_vector_set"},
	}
	_, err = s.GrantConsent(t.Context(), ProcessingConsentGrantRequest{
		Principal: consent.Principal, Scope: consent.Scope, ProfileFingerprint: consent.ProfileFingerprint,
		DisclosureFingerprint: consent.DisclosureFingerprint, InputClasses: consent.InputClasses,
		RetainedArtifactClasses: consent.RetainedArtifactClasses,
	})
	require.NoError(t, err)
	_, err = s.EnqueueEmbeddingJob(t.Context(), EmbeddingJobRequest{ContentVersionID: versionID,
		Profile: profile, BindingID: binding.Name, Descriptor: record.VectorSpace.Descriptor,
		InputGeneration: record.InputGeneration, Authorization: consent})
	require.NoError(t, err)
	_, err = s.db.Exec(`DELETE FROM embedding_jobs`)
	require.NoError(t, err)

	result, err := s.ReconcileEmbeddingJobs(t.Context(), EmbeddingReconcileRequest{Mutate: embeddingTestMutation,
		At: time.Now().UTC(), Limit: 100,
		DescriptorFingerprints: []string{record.VectorSpace.Descriptor.Fingerprint},
		HydrateGeneration: func(_ context.Context, generation EmbeddingInputGenerationRecord) (EmbeddingInputGenerationRecord, error) {
			return HydrateEmbeddingInputGeneration(generation, record.InputGeneration.GenerationJSON, record.InputGeneration.EvidenceJSON)
		},
	})
	require.NoError(t, err)
	assert.Equal(t, 1, result.Enqueued)
	var bindingID string
	require.NoError(t, s.db.QueryRow(`SELECT binding_id FROM embedding_jobs`).Scan(&bindingID))
	assert.Equal(t, "chunk", bindingID)

	// Purge can also win after discovery, while the caller hydrates E2 bytes.
	_, err = s.db.Exec(`DELETE FROM embedding_jobs`)
	require.NoError(t, err)
	hydrated := false
	result, err = s.ReconcileEmbeddingJobs(t.Context(), EmbeddingReconcileRequest{Mutate: embeddingTestMutation,
		At: time.Now().UTC(), Limit: 100, DescriptorFingerprints: []string{record.VectorSpace.Descriptor.Fingerprint},
		HydrateGeneration: func(ctx context.Context, generation EmbeddingInputGenerationRecord) (EmbeddingInputGenerationRecord, error) {
			hydrated = true
			_, err := s.PurgeDerivatives(ctx, PurgeRequest{ContentVersionIDs: []string{versionID}})
			if err != nil {
				return EmbeddingInputGenerationRecord{}, err
			}
			return HydrateEmbeddingInputGeneration(generation, record.InputGeneration.GenerationJSON, record.InputGeneration.EvidenceJSON)
		},
	})
	require.NoError(t, err)
	require.True(t, hydrated)
	require.Zero(t, result.Enqueued)
}

func TestEmbeddingJobLeaseCannotRenewAfterExpiry(t *testing.T) {
	s, versionID, profile, _ := newEmbeddingCatalogFixture(t)
	request := embeddingJobTestRequest(t, s, versionID, profile, "lease-expiry")
	_, err := s.EnqueueEmbeddingJob(t.Context(), request)
	require.NoError(t, err)
	at := time.Now().UTC()
	claim, work, found, err := s.ClaimNextEmbeddingWork(t.Context(), "worker-expiry", at, time.Minute, []string{request.Descriptor.Fingerprint})
	require.NoError(t, err)
	require.True(t, found)

	_, err = s.RenewEmbeddingWork(t.Context(), claim, at.Add(time.Minute), time.Minute)
	require.ErrorIs(t, err, ErrEmbeddingJobFenced)
	err = s.FailEmbeddingWork(t.Context(), claim, work, EmbeddingFailureInputRejected,
		EmbeddingAttemptReceipt{AttemptID: claim.AttemptID}, at.Add(time.Minute))
	require.ErrorIs(t, err, ErrEmbeddingJobFenced)
	resumed, _, found, err := s.ClaimNextEmbeddingWork(t.Context(), "worker-resume", at.Add(time.Minute), time.Minute, []string{request.Descriptor.Fingerprint})
	require.NoError(t, err)
	require.True(t, found)
	assert.Greater(t, resumed.Epoch, claim.Epoch)
}

func TestEmbeddingJobProviderUnavailableRetryBudgetIsDurable(t *testing.T) {
	s, versionID, profile, _ := newEmbeddingCatalogFixture(t)
	request := embeddingJobTestRequest(t, s, versionID, profile, "durable-retry-budget")
	_, err := s.EnqueueEmbeddingJob(t.Context(), request)
	require.NoError(t, err)
	at := time.Now().UTC()
	for attempt := 1; attempt <= embeddingJobMaxClaims; attempt++ {
		claim, work, found, err := s.ClaimNextEmbeddingWork(t.Context(), "worker-budget", at, time.Minute, []string{request.Descriptor.Fingerprint})
		require.NoError(t, err)
		require.True(t, found)
		require.Equal(t, int64(attempt), claim.Epoch)
		require.NoError(t, s.FailEmbeddingWork(t.Context(), claim, work,
			EmbeddingFailureProviderUnavailable, EmbeddingAttemptReceipt{
				AttemptID: claim.AttemptID, ProviderFingerprint: work.Descriptor.Fingerprint,
				ProfileFingerprint: profile.Fingerprint, BindingID: work.Binding.Name,
				InputKind: work.Binding.InputKind, ProviderCalls: 3, Retries: 2,
				FailureCode: string(EmbeddingFailureProviderUnavailable),
			}, at.Add(30*time.Second)))
		at = at.Add(2 * time.Minute)
	}
	_, _, found, err := s.ClaimNextEmbeddingWork(t.Context(), "worker-budget", at, time.Minute, []string{request.Descriptor.Fingerprint})
	require.NoError(t, err)
	assert.False(t, found)
	var state string
	require.NoError(t, s.db.QueryRow(`SELECT state FROM embedding_jobs`).Scan(&state))
	assert.Equal(t, "failed", state)
}

func TestEmbeddingJobExistingStaleHeadDoesNotSuppressReplacement(t *testing.T) {
	s, versionID, profile, _ := newEmbeddingCatalogFixture(t)
	old := embeddingSetFixture(s, versionID, profile.Fingerprint,
		document.EmbeddingInputOriginalFile, "optional", "")
	require.NoError(t, s.StageEmbeddingSet(t.Context(), old))
	require.NoError(t, s.PublishEmbeddingHead(t.Context(), EmbeddingHeadRecord{
		FencingToken: 1,
		Key:          EmbeddingHeadKey{ContentVersionID: versionID, BindingID: old.BindingID, InputKind: old.InputKind},
		SetID:        old.ID, VectorSpaceID: old.VectorSpace.ID,
		ProcessingProfileFingerprint: profile.Fingerprint, PublishedAt: embeddingCatalogTime,
	}))
	request := embeddingJobTestRequest(t, s, versionID, profile, "replacement-generation")
	_, err := s.EnqueueEmbeddingJob(t.Context(), request)
	require.NoError(t, err)

	claim, work, found, err := s.ClaimNextEmbeddingWork(t.Context(), "worker-replacement",
		time.Now().UTC(), time.Minute, []string{request.Descriptor.Fingerprint})
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, request.InputGeneration.ID, work.InputGeneration.ID)
	require.Greater(t, claim.Epoch, int64(1))
	require.NoError(t, s.FailEmbeddingWork(t.Context(), claim, work, EmbeddingFailureProviderUnavailable,
		EmbeddingAttemptReceipt{AttemptID: claim.AttemptID}, time.Now().UTC()))
	assert.Equal(t, old.ID, embeddingHeadSetIDForTest(t, s, versionID, profile.Fingerprint, old.BindingID, old.InputKind))
}

func TestVersionPruneRemovesQueuedEmbeddingJobGenerationAndRoot(t *testing.T) {
	s, firstVersion, profile, _ := newEmbeddingCatalogFixture(t)
	request := embeddingJobTestRequest(t, s, firstVersion, profile, "queued-prune")
	job, err := s.EnqueueEmbeddingJob(t.Context(), request)
	require.NoError(t, err)
	_, _, found, err := s.ClaimNextEmbeddingWork(t.Context(), "worker-prune",
		time.Now().UTC(), time.Hour, []string{request.Descriptor.Fingerprint})
	require.NoError(t, err)
	require.True(t, found)
	var nodeID, revision int64
	require.NoError(t, s.db.QueryRow(`SELECT id,revision FROM nodes WHERE current_version_id=?`, firstVersion).Scan(&nodeID, &revision))
	replacementHash := fakeHash("embedding-job-prune-replacement")
	require.NoError(t, s.withStorageTx(t.Context(), func(tx *sql.Tx) error {
		return s.EnsureBlobTx(tx, replacementHash, 24)
	}))
	updated, _, err := s.ReplaceContent(t.Context(), nodeID, revision, replacementHash, 24, "application/pdf")
	require.NoError(t, err)
	_, err = s.PruneContentVersions(t.Context(), nodeID, updated.Revision,
		VersionPruneSelector{VersionIDs: []string{firstVersion}}, true)
	require.NoError(t, err)
	_, err = s.PurgeDerivatives(t.Context(), PurgeRequest{})
	require.NoError(t, err)
	var remaining int
	require.NoError(t, s.db.QueryRow(`SELECT
		(SELECT COUNT(*) FROM embedding_jobs WHERE job_id=?) +
		(SELECT COUNT(*) FROM embedding_input_generations WHERE generation_id=?) +
		(SELECT COUNT(*) FROM current_rendition_roots WHERE root_id=?)`,
		job.ID, request.InputGeneration.ID, job.ID).Scan(&remaining))
	assert.Zero(t, remaining)
}

func embeddingJobTestRequest(t *testing.T, s *Store, versionID string,
	profile ProcessingProfileRecord, generationSeed string,
) EmbeddingJobRequest {
	t.Helper()
	const bindingID = "optional"
	record := embeddingSetFixture(s, versionID, profile.Fingerprint,
		document.EmbeddingInputOriginalFile, bindingID, "")
	record.InputGeneration.ID = testSHA256([]byte(generationSeed))
	binding := workerOptionalEmbeddingBinding(t, profile)
	consent := ProviderOperationAuthorizationRequest{
		Principal: "operator:" + generationSeed, Scope: "embedding:" + bindingID,
		ProfileFingerprint: profile.Fingerprint, DisclosureFingerprint: binding.DisclosureFingerprint,
		InputClasses: []string{string(binding.InputKind)}, RetainedArtifactClasses: []string{"embedding_vector_set"},
	}
	_, err := s.GrantConsent(t.Context(), ProcessingConsentGrantRequest{
		Principal: consent.Principal, Scope: consent.Scope, ProfileFingerprint: consent.ProfileFingerprint,
		DisclosureFingerprint: consent.DisclosureFingerprint, InputClasses: consent.InputClasses,
		RetainedArtifactClasses: consent.RetainedArtifactClasses,
	})
	require.NoError(t, err)
	return EmbeddingJobRequest{ContentVersionID: versionID, Profile: profile, BindingID: bindingID,
		Descriptor: record.VectorSpace.Descriptor, InputGeneration: record.InputGeneration, Authorization: consent}
}

func workerOptionalEmbeddingBinding(t *testing.T, profile ProcessingProfileRecord) document.EmbeddingBindingV1 {
	t.Helper()
	return workerProfileEmbeddingBinding(t, profile, "optional")
}

func workerProfileEmbeddingBinding(t *testing.T, profile ProcessingProfileRecord, name string) document.EmbeddingBindingV1 {
	t.Helper()
	var canonical document.ProcessingProfileV1
	require.NoError(t, json.Unmarshal(profile.CanonicalProfile, &canonical))
	for _, binding := range canonical.Embeddings {
		if binding.Name == name {
			return binding
		}
	}
	t.Fatalf("embedding binding %q not found", name)
	return document.EmbeddingBindingV1{}
}

func TestEmbeddingCatalogRejectsInvalidRowsAndPublicationFences(t *testing.T) {
	s, versionID, profile, _ := newEmbeddingCatalogFixture(t)
	base := embeddingSetFixture(s, versionID, profile.Fingerprint, document.EmbeddingInputOriginalFile, "optional", "")

	for _, testCase := range []struct {
		name   string
		mutate func(*EmbeddingSetRecord)
	}{
		{"corrupt payload", func(value *EmbeddingSetRecord) { value.VectorSet.Payload[len(value.VectorSet.Payload)-1] ^= 1 }},
		{"wrong vector space", func(value *EmbeddingSetRecord) { value.VectorSet.VectorSpaceID = fakeHash("wrong-space") }},
		{"checksum mismatch", func(value *EmbeddingSetRecord) { value.VectorSet.PayloadChecksum = fakeHash("wrong") }},
		{"identity mismatch", func(value *EmbeddingSetRecord) { value.VectorSet.ID = fakeHash("wrong-id") }},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			record := cloneEmbeddingSetRecord(base)
			testCase.mutate(&record)
			err := s.StageEmbeddingSet(t.Context(), record)
			require.Error(t, err)
		})
	}

	require.NoError(t, s.StageEmbeddingSet(t.Context(), base))
	for _, testCase := range []struct {
		name   string
		mutate func(*EmbeddingHeadRecord)
		want   string
	}{
		{"source", func(value *EmbeddingHeadRecord) { value.Key.ContentVersionID = "00000000-0000-4000-8000-000000000176" }, "stale source"},
		{"profile", func(value *EmbeddingHeadRecord) { value.ProcessingProfileFingerprint = fakeHash("missing-profile") }, "profile fingerprint"},
		{"space", func(value *EmbeddingHeadRecord) { value.VectorSpaceID = fakeHash("missing-space") }, "vector-space ID"},
		{"unfenced", func(value *EmbeddingHeadRecord) { value.FencingToken = 0 }, "fencing token"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			head := EmbeddingHeadRecord{
				FencingToken: 1,
				Key:          EmbeddingHeadKey{ContentVersionID: versionID, BindingID: "optional", InputKind: document.EmbeddingInputOriginalFile},
				SetID:        base.ID, VectorSpaceID: base.VectorSpace.ID,
				ProcessingProfileFingerprint: profile.Fingerprint, PublishedAt: embeddingCatalogTime,
			}
			testCase.mutate(&head)
			err := s.PublishEmbeddingHead(t.Context(), head)
			require.ErrorContains(t, err, testCase.want)
		})
	}
}

func TestEmbeddingCatalogRejectsLegacyGenerationShapeAsPolicyAuthority(t *testing.T) {
	s, versionID, profile, attachmentID := newEmbeddingCatalogFixture(t)
	record := embeddingSetFixture(s, versionID, profile.Fingerprint, document.EmbeddingInputRenditionChunk, "chunk", attachmentID)
	legacy := append([]byte(nil), record.InputGeneration.GenerationJSON[:len(record.InputGeneration.GenerationJSON)-1]...)
	legacy = append(legacy, []byte(`,"tokenizer_identity":{"name":"catalog-runes","revision":"v1"}}`)...)
	record.InputGeneration.GenerationJSON = legacy
	err := s.StageEmbeddingSet(t.Context(), record)
	require.ErrorContains(t, err, "decode embedding input generation")
}

func TestEmbeddingCatalogMetadataRoundTripsDeterministically(t *testing.T) {
	s, versionID, profile, attachmentID := newEmbeddingCatalogFixture(t)
	record := embeddingSetFixture(s, versionID, profile.Fingerprint, document.EmbeddingInputOriginalFile, "optional", "")
	require.NoError(t, s.StageEmbeddingSet(t.Context(), record))
	require.NoError(t, s.PublishEmbeddingHead(t.Context(), EmbeddingHeadRecord{
		FencingToken: 1,
		Key:          EmbeddingHeadKey{ContentVersionID: versionID, BindingID: "optional", InputKind: document.EmbeddingInputOriginalFile},
		SetID:        record.ID, VectorSpaceID: record.VectorSpace.ID,
		ProcessingProfileFingerprint: profile.Fingerprint, PublishedAt: embeddingCatalogTime,
	}))
	chunk := embeddingSetFixture(s, versionID, profile.Fingerprint, document.EmbeddingInputRenditionChunk, "chunk", attachmentID)
	require.NoError(t, s.StageEmbeddingSet(t.Context(), chunk))
	require.NoError(t, s.PublishEmbeddingHead(t.Context(), EmbeddingHeadRecord{
		FencingToken: 1,
		Key:          EmbeddingHeadKey{ContentVersionID: versionID, BindingID: "chunk", InputKind: document.EmbeddingInputRenditionChunk},
		SetID:        chunk.ID, VectorSpaceID: chunk.VectorSpace.ID,
		ProcessingProfileFingerprint: profile.Fingerprint, PublishedAt: embeddingCatalogTime,
	}))
	for _, root := range []CurrentRenditionRoot{
		{ID: "embedding-roundtrip-set", Kind: RenditionRootRetention,
			TargetKind: RenditionRootEmbeddingSet, TargetID: chunk.ID,
			FencingToken: 1, RecordedAt: embeddingCatalogTime},
		{ID: "embedding-roundtrip-generation", Kind: RenditionRootRetention,
			TargetKind: RenditionRootEmbeddingGeneration, TargetID: chunk.InputGeneration.ID,
			FencingToken: 1, RecordedAt: embeddingCatalogTime},
		{ID: "embedding-roundtrip-vector-set", Kind: RenditionRootRetention,
			TargetKind: RenditionRootEmbeddingVectorSet, TargetID: chunk.VectorSet.ID,
			FencingToken: 1, RecordedAt: embeddingCatalogTime},
		{ID: "embedding-roundtrip-payload", Kind: RenditionRootRetention,
			TargetKind: RenditionRootEmbeddingPayload, TargetID: chunk.VectorSet.PayloadBlobHash,
			FencingToken: 1, RecordedAt: embeddingCatalogTime},
	} {
		require.NoError(t, s.PutCurrentRenditionRoot(t.Context(), root))
	}

	var first, second bytes.Buffer
	require.NoError(t, s.ExportMetadata(t.Context(), &first))
	require.NoError(t, s.ExportMetadata(t.Context(), &second))
	assert.Equal(t, first.Bytes(), second.Bytes())
	assert.NotContains(t, first.String(), "synthetic source pdf")
	assert.NotContains(t, first.String(), "input text")

	restored := newTestStoreWithDriver(t, modernc.Driver{})
	require.NoError(t, restored.ImportMetadata(t.Context(), bytes.NewReader(first.Bytes())))
	var roundTrip bytes.Buffer
	require.NoError(t, restored.ExportMetadata(t.Context(), &roundTrip))
	assert.Equal(t, first.Bytes(), roundTrip.Bytes())
	assert.Equal(t, record.ID, embeddingHeadSetIDForTest(t, restored, versionID, profile.Fingerprint,
		"optional", document.EmbeddingInputOriginalFile))
	assert.Equal(t, chunk.ID, embeddingHeadSetIDForTest(t, restored, versionID, profile.Fingerprint,
		"chunk", document.EmbeddingInputRenditionChunk))
	var restoredRoots int
	require.NoError(t, restored.db.QueryRow(`SELECT COUNT(*) FROM current_rendition_roots
		WHERE root_id LIKE 'embedding-roundtrip-%' AND active=1`).Scan(&restoredRoots))
	assert.Equal(t, 4, restoredRoots)
}

func TestEmbeddingCatalogDerivativePurgeAndGCLeaveOriginalAuthority(t *testing.T) {
	s, versionID, profile, _ := newEmbeddingCatalogFixture(t)
	record := embeddingSetFixture(s, versionID, profile.Fingerprint, document.EmbeddingInputOriginalFile, "optional", "")
	require.NoError(t, s.StageEmbeddingSet(t.Context(), record))
	require.NoError(t, s.PublishEmbeddingHead(t.Context(), EmbeddingHeadRecord{
		FencingToken: 1,
		Key:          EmbeddingHeadKey{ContentVersionID: versionID, BindingID: "optional", InputKind: document.EmbeddingInputOriginalFile},
		SetID:        record.ID, VectorSpaceID: record.VectorSpace.ID,
		ProcessingProfileFingerprint: profile.Fingerprint, PublishedAt: embeddingCatalogTime,
	}))

	plan, err := s.DerivativeGCPlan(t.Context())
	require.NoError(t, err)
	assert.Empty(t, plan.EmbeddingSets, "an active embedding head must retain its complete set")

	require.NoError(t, s.withStorageTx(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.Exec(`DELETE FROM embedding_heads WHERE embedding_set_id=?`, record.ID)
		return err
	}))
	var nodeID, revision int64
	require.NoError(t, s.db.QueryRow(`SELECT id,revision FROM nodes WHERE current_version_id=?`, versionID).Scan(&nodeID, &revision))
	_, _, err = s.Trash(t.Context(), nodeID, revision)
	require.NoError(t, err)
	plan, err = s.DerivativeGCPlan(t.Context())
	require.NoError(t, err)
	require.Len(t, plan.EmbeddingSets, 1)
	assert.Equal(t, record.ID, plan.EmbeddingSets[0].SetID)
	assert.Equal(t, record.VectorSet.PayloadBlobHash, plan.EmbeddingSets[0].PayloadBlobHash)

	report, err := s.PurgeDerivatives(t.Context(), PurgeRequest{})
	require.NoError(t, err)
	assert.Equal(t, 1, report.RemovedEmbeddingSets)
	assert.Equal(t, 1, report.RemovedEmbeddingInputGenerations)
	assert.Equal(t, 1, report.RemovedEmbeddingVectorSets)
	assert.Contains(t, report.PhysicalDerivativeBlobsPendingGC, record.VectorSet.PayloadBlobHash)
	var versions, vectorRows int
	require.NoError(t, s.db.QueryRow(`SELECT
		(SELECT COUNT(*) FROM content_versions WHERE version_id=?),
		(SELECT COUNT(*) FROM embedding_vector_rows WHERE vector_set_id=?)`,
		versionID, record.VectorSet.ID).Scan(&versions, &vectorRows))
	assert.Equal(t, 1, versions, "derivative collection must not remove original content authority")
	assert.Zero(t, vectorRows)
}

func TestEmbeddingCatalogRootsRetainEveryAuthorityTransitively(t *testing.T) {
	testCases := []struct {
		name       string
		kind       CurrentRenditionRootKind
		targetKind CurrentRenditionTargetKind
		target     func(EmbeddingSetRecord) string
		lease      bool
	}{
		{"reader lease set", RenditionRootReaderLease, RenditionRootEmbeddingSet, func(value EmbeddingSetRecord) string { return value.ID }, true},
		{"worker lease generation", RenditionRootWorkerLease, RenditionRootEmbeddingGeneration, func(value EmbeddingSetRecord) string { return value.InputGeneration.ID }, true},
		{"backup set", RenditionRootBackupPin, RenditionRootEmbeddingSet, func(value EmbeddingSetRecord) string { return value.ID }, false},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			s, versionID, profile, _ := newEmbeddingCatalogFixture(t)
			record := embeddingSetFixture(s, versionID, profile.Fingerprint, document.EmbeddingInputOriginalFile, "optional", "")
			require.NoError(t, s.StageEmbeddingSet(t.Context(), record))
			var nodeID, revision int64
			require.NoError(t, s.db.QueryRow(`SELECT id,revision FROM nodes WHERE current_version_id=?`, versionID).Scan(&nodeID, &revision))
			_, _, err := s.Trash(t.Context(), nodeID, revision)
			require.NoError(t, err)
			root := CurrentRenditionRoot{
				ID: "embedding-root-" + strings.ReplaceAll(testCase.name, " ", "-"), Kind: testCase.kind,
				TargetKind: testCase.targetKind, TargetID: testCase.target(record), FencingToken: 1,
				RecordedAt: embeddingCatalogTime,
			}
			if testCase.lease {
				root.ExpiresAt = "2099-08-25T10:00:00.000000000Z"
			}
			require.NoError(t, s.PutCurrentRenditionRoot(t.Context(), root))
			plan, err := s.DerivativeGCPlan(t.Context())
			require.NoError(t, err)
			assert.Empty(t, plan.EmbeddingSets)
			report, err := s.PurgeDerivatives(t.Context(), PurgeRequest{})
			require.NoError(t, err)
			assert.Zero(t, report.RemovedEmbeddingSets)
			released, err := s.ReleaseCurrentRenditionRoot(t.Context(), root.ID, root.FencingToken)
			require.NoError(t, err)
			assert.True(t, released)
			plan, err = s.DerivativeGCPlan(t.Context())
			require.NoError(t, err)
			require.Len(t, plan.EmbeddingSets, 1)
			assert.Equal(t, record.ID, plan.EmbeddingSets[0].SetID)
		})
	}
	t.Run("expired lease", func(t *testing.T) {
		s, versionID, profile, _ := newEmbeddingCatalogFixture(t)
		record := embeddingSetFixture(s, versionID, profile.Fingerprint, document.EmbeddingInputOriginalFile, "optional", "")
		require.NoError(t, s.StageEmbeddingSet(t.Context(), record))
		var nodeID, revision int64
		require.NoError(t, s.db.QueryRow(`SELECT id,revision FROM nodes WHERE current_version_id=?`, versionID).Scan(&nodeID, &revision))
		_, _, err := s.Trash(t.Context(), nodeID, revision)
		require.NoError(t, err)
		root := CurrentRenditionRoot{
			ID: "expired-embedding-reader", Kind: RenditionRootReaderLease,
			TargetKind: RenditionRootEmbeddingSet, TargetID: record.ID, FencingToken: 1,
			RecordedAt: embeddingCatalogTime, ExpiresAt: "2020-08-25T10:00:00.000000000Z",
		}
		require.NoError(t, s.PutCurrentRenditionRoot(t.Context(), root))
		plan, err := s.DerivativeGCPlan(t.Context())
		require.NoError(t, err)
		require.Len(t, plan.EmbeddingSets, 1)
		assert.Contains(t, plan.ExpiredRootIDs, root.ID)
	})
}

func TestEmbeddingCatalogVersionPruneDeletesDirectAndChunkAuthority(t *testing.T) {
	s, firstVersion, profile, firstAttachment := newEmbeddingCatalogFixture(t)
	var versionID, buildID string
	require.NoError(t, s.db.QueryRow(`SELECT version_id FROM content_versions WHERE version_id<>? AND blob_hash=? ORDER BY version_id LIMIT 1`, firstVersion, catalogSourceHash).Scan(&versionID))
	require.NoError(t, s.db.QueryRow(`SELECT build_id FROM rendition_attachments WHERE attachment_id=?`, firstAttachment).Scan(&buildID))
	attachmentID := testSHA256([]byte("pruned-embedding-attachment"))
	attachment := RenditionAttachmentRecord{
		ID: attachmentID, VaultID: s.VaultID(), ContentVersionID: versionID, BuildID: buildID,
		Profile: profile, AttachedAt: embeddingCatalogTime,
	}
	require.NoError(t, publishRenditionForTest(
		t, s, attachment, embeddingCatalogTime, testSHA256([]byte("pruned-embedding-lexical-generation")),
	))
	records := []EmbeddingSetRecord{
		embeddingSetFixture(s, versionID, profile.Fingerprint, document.EmbeddingInputOriginalFile, "optional", ""),
		embeddingSetFixture(s, versionID, profile.Fingerprint, document.EmbeddingInputRenditionChunk, "chunk", attachmentID),
	}
	for _, record := range records {
		require.NoError(t, s.StageEmbeddingSet(t.Context(), record))
		require.NoError(t, s.PublishEmbeddingHead(t.Context(), EmbeddingHeadRecord{
			FencingToken: 1,
			Key:          EmbeddingHeadKey{ContentVersionID: versionID, BindingID: record.BindingID, InputKind: record.InputKind},
			SetID:        record.ID, VectorSpaceID: record.VectorSpace.ID,
			ProcessingProfileFingerprint: profile.Fingerprint, PublishedAt: embeddingCatalogTime,
		}))
	}
	var nodeID, revision int64
	require.NoError(t, s.db.QueryRow(`SELECT id,revision FROM nodes WHERE current_version_id=?`, versionID).Scan(&nodeID, &revision))
	replacementHash := fakeHash("e3")
	require.NoError(t, s.withStorageTx(t.Context(), func(tx *sql.Tx) error { return s.EnsureBlobTx(tx, replacementHash, 24) }))
	updated, _, err := s.ReplaceContent(t.Context(), nodeID, revision, replacementHash, 24, "application/pdf")
	require.NoError(t, err)
	result, err := s.PruneContentVersions(t.Context(), nodeID, updated.Revision, VersionPruneSelector{VersionIDs: []string{versionID}}, true)
	require.NoError(t, err)
	assert.Equal(t, 1, result.DeletedVersions)
	var remaining int
	require.NoError(t, s.db.QueryRow(`SELECT
		(SELECT COUNT(*) FROM content_versions WHERE version_id=?) +
		(SELECT COUNT(*) FROM embedding_sets WHERE content_version_id=?) +
		(SELECT COUNT(*) FROM rendition_attachments WHERE attachment_id=?)`,
		versionID, versionID, attachmentID).Scan(&remaining))
	assert.Zero(t, remaining)

	var generations, vectorSets int
	require.NoError(t, s.db.QueryRow(`SELECT
		(SELECT COUNT(*) FROM embedding_input_generations WHERE generation_id IN (?,?)),
		(SELECT COUNT(*) FROM embedding_vector_sets WHERE vector_set_id IN (?,?))`,
		records[0].InputGeneration.ID, records[1].InputGeneration.ID,
		records[0].VectorSet.ID, records[1].VectorSet.ID).Scan(&generations, &vectorSets))
	assert.Equal(t, 2, generations)
	assert.Equal(t, 2, vectorSets)
	evidenceHash := testSHA256(records[1].InputGeneration.EvidenceJSON)
	unreachable, err := s.UnreachableBlobs(t.Context())
	require.NoError(t, err)
	for _, blob := range unreachable {
		assert.NotEqual(t, evidenceHash, blob.Hash, "orphan generation still needs evidence for restore verification")
	}

	report, err := s.PurgeDerivatives(t.Context(), PurgeRequest{})
	require.NoError(t, err)
	assert.Equal(t, 2, report.RemovedEmbeddingInputGenerations)
	assert.Equal(t, 2, report.RemovedEmbeddingVectorSets)
	assert.Contains(t, report.PhysicalDerivativeBlobsPendingGC, evidenceHash)
}

func TestEmbeddingCatalogVersionPrunePreservesSharedAuthorityRoots(t *testing.T) {
	s, sourceVersion, profile, _ := newEmbeddingCatalogFixture(t)
	var deletedVersion string
	require.NoError(t, s.db.QueryRow(`SELECT version_id FROM content_versions WHERE version_id<>? AND blob_hash=? ORDER BY version_id LIMIT 1`, sourceVersion, catalogSourceHash).Scan(&deletedVersion))
	third, err := s.CreateFile(t.Context(), s.RootID(), "shared-embedding-consumer.pdf", catalogSourceHash, int64(len(catalogBlobContents[catalogSourceHash])), "application/pdf")
	require.NoError(t, err)
	record := embeddingSetFixture(s, sourceVersion, profile.Fingerprint, document.EmbeddingInputOriginalFile, "optional", "")
	require.NoError(t, s.StageEmbeddingSet(t.Context(), record))
	deletedSetID := fakeHash("deleted-shared-version-embedding-set")
	survivingSetID := fakeHash("surviving-shared-version-embedding-set")
	for setID, versionID := range map[string]string{deletedSetID: deletedVersion, survivingSetID: third.CurrentVersionID} {
		_, err = s.db.Exec(`INSERT INTO embedding_sets(
		embedding_set_id,vault_uid,binding_id,input_kind,content_version_id,
		profile_fingerprint,embedding_input_fingerprint,vector_space_id,
		input_generation_id,vector_set_id,created_at)
		SELECT ?,vault_uid,binding_id,input_kind,?,profile_fingerprint,
		embedding_input_fingerprint,vector_space_id,input_generation_id,vector_set_id,created_at
		FROM embedding_sets WHERE embedding_set_id=?`, setID, versionID, record.ID)
		require.NoError(t, err)
	}

	roots := []CurrentRenditionRoot{
		{ID: "deleted-set-pin", Kind: RenditionRootBackupPin, TargetKind: RenditionRootEmbeddingSet, TargetID: deletedSetID, FencingToken: 1, RecordedAt: embeddingCatalogTime},
		{ID: "shared-generation-lease", Kind: RenditionRootReaderLease, TargetKind: RenditionRootEmbeddingGeneration, TargetID: record.InputGeneration.ID, FencingToken: 1, RecordedAt: embeddingCatalogTime, ExpiresAt: "2099-08-25T10:00:00.000000000Z"},
		{ID: "shared-vector-pin", Kind: RenditionRootBackupPin, TargetKind: RenditionRootEmbeddingVectorSet, TargetID: record.VectorSet.ID, FencingToken: 1, RecordedAt: embeddingCatalogTime},
		{ID: "shared-payload-pin", Kind: RenditionRootRetention, TargetKind: RenditionRootEmbeddingPayload, TargetID: record.VectorSet.PayloadBlobHash, FencingToken: 1, RecordedAt: embeddingCatalogTime},
	}
	for _, root := range roots {
		require.NoError(t, s.PutCurrentRenditionRoot(t.Context(), root))
	}

	var deletedNodeID, deletedRevision int64
	require.NoError(t, s.db.QueryRow(`SELECT id,revision FROM nodes WHERE current_version_id=?`, deletedVersion).Scan(&deletedNodeID, &deletedRevision))
	replacementHash := fakeHash("shared-authority-replacement")
	require.NoError(t, s.withStorageTx(t.Context(), func(tx *sql.Tx) error { return s.EnsureBlobTx(tx, replacementHash, 24) }))
	updated, _, err := s.ReplaceContent(t.Context(), deletedNodeID, deletedRevision, replacementHash, 24, "application/pdf")
	require.NoError(t, err)
	_, err = s.PruneContentVersions(t.Context(), deletedNodeID, updated.Revision, VersionPruneSelector{VersionIDs: []string{deletedVersion}}, true)
	require.NoError(t, err)

	var survivingAuthorities, survivingRoots, deletedSetRoots int
	require.NoError(t, s.db.QueryRow(`SELECT
		(SELECT COUNT(*) FROM embedding_sets WHERE embedding_set_id=?) +
		(SELECT COUNT(*) FROM embedding_input_generations WHERE generation_id=?) +
		(SELECT COUNT(*) FROM embedding_vector_sets WHERE vector_set_id=?)`,
		survivingSetID, record.InputGeneration.ID, record.VectorSet.ID).Scan(&survivingAuthorities))
	require.Equal(t, 3, survivingAuthorities)
	require.NoError(t, s.db.QueryRow(`SELECT COUNT(*) FROM current_rendition_roots WHERE root_id IN (?,?,?)`,
		roots[1].ID, roots[2].ID, roots[3].ID).Scan(&survivingRoots))
	require.Equal(t, 3, survivingRoots)
	require.NoError(t, s.db.QueryRow(`SELECT COUNT(*) FROM current_rendition_roots WHERE root_id=?`, roots[0].ID).Scan(&deletedSetRoots))
	assert.Zero(t, deletedSetRoots)

	for _, versionID := range []string{sourceVersion, third.CurrentVersionID} {
		var nodeID, revision int64
		require.NoError(t, s.db.QueryRow(`SELECT id,revision FROM nodes WHERE current_version_id=?`, versionID).Scan(&nodeID, &revision))
		_, _, err = s.Trash(t.Context(), nodeID, revision)
		require.NoError(t, err)
	}
	plan, err := s.DerivativeGCPlan(t.Context())
	require.NoError(t, err)
	assert.Empty(t, plan.EmbeddingSets, "shared generation/vector/payload roots must retain their surviving consumer")
}

func TestEmbeddingCatalogMetadataRejectsCorruptionAndOversizedCounts(t *testing.T) {
	s, versionID, profile, _ := newEmbeddingCatalogFixture(t)
	record := embeddingSetFixture(s, versionID, profile.Fingerprint, document.EmbeddingInputOriginalFile, "optional", "")
	require.NoError(t, s.StageEmbeddingSet(t.Context(), record))
	var exported bytes.Buffer
	require.NoError(t, s.ExportMetadata(t.Context(), &exported))

	for _, testCase := range []struct {
		name   string
		mutate func(string) string
		want   string
	}{
		{"unknown contract", func(value string) string {
			return strings.Replace(value, EmbeddingVectorSpaceContractV1, "embedding-vector-space/v2", 1)
		}, "unsupported"},
		{"oversized count", func(value string) string {
			return strings.Replace(value, `"input_count":1`, `"input_count":100001`, 1)
		}, "exceeds bounds"},
		{"missing input row", func(value string) string {
			lines := strings.Split(value, "\n")
			for index, line := range lines {
				if strings.Contains(line, `"type":"embedding_generation_input"`) {
					return strings.Join(append(lines[:index], lines[index+1:]...), "\n")
				}
			}
			return value
		}, "count"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			target := newTestStore(t)
			err := target.ImportMetadata(t.Context(), strings.NewReader(testCase.mutate(exported.String())))
			require.ErrorContains(t, err, testCase.want)
			var sets int
			require.NoError(t, target.db.QueryRow(`SELECT COUNT(*) FROM embedding_sets`).Scan(&sets))
			assert.Zero(t, sets, "corrupt import must roll back atomically")
		})
	}
}

func TestEmbeddingCatalogSchemaCorruptionFailsClosedOnOpen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "catalog.db")
	s, err := Open(path)
	require.NoError(t, err)
	require.NoError(t, s.Close())
	driver := DefaultSQLiteDriver()
	db, err := driver.Open(path, docsqlite.OpenOptions{
		Access: docsqlite.ReadWriteExisting, TransactionMode: docsqlite.Immediate,
	})
	require.NoError(t, err)
	_, err = db.Exec(`ALTER TABLE embedding_sets ADD COLUMN unexpected TEXT`)
	require.NoError(t, err)
	require.NoError(t, db.Close())

	_, err = Open(path, driver)
	require.ErrorContains(t, err, "embedding catalog schema")
}

const embeddingCatalogTime = "2026-08-25T10:00:00.000000000Z"

func embeddingHeadSetIDForTest(
	t *testing.T, s *Store, versionID, profileFingerprint, bindingID string, kind EmbeddingInputKind,
) string {
	t.Helper()
	var setID string
	require.NoError(t, s.db.QueryRow(`SELECT embedding_set_id FROM embedding_heads
		WHERE content_version_id=? AND profile_fingerprint=? AND binding_id=? AND input_kind=?`,
		versionID, profileFingerprint, bindingID, kind).Scan(&setID))
	return setID
}

func newEmbeddingCatalogFixture(t *testing.T) (*Store, string, ProcessingProfileRecord, string) {
	t.Helper()
	s, versions := newRenditionCatalogFixture(t)
	profile := embeddingCatalogProfile(t)
	build := catalogRenditionBuild(s, profile)
	build.EvidenceChecksum = embeddingCatalogEvidence(t).Checksum
	require.NoError(t, s.StageRenditionBuild(t.Context(), build))
	attachment := RenditionAttachmentRecord{
		ID: catalogAttachmentFirst, VaultID: s.VaultID(), ContentVersionID: versions[0],
		BuildID: build.ID, Profile: profile, AttachedAt: embeddingCatalogTime,
	}
	require.NoError(t, publishRenditionForTest(
		t, s, attachment, embeddingCatalogTime, testSHA256([]byte("embedding-catalog-lexical-generation")),
	))
	return s, versions[0], profile, attachment.ID
}

func embeddingSetFixture(
	s *Store, versionID, profileFingerprint string, kind EmbeddingInputKind, bindingID, attachmentID string,
) EmbeddingSetRecord {
	seed := bindingID + string(kind) + versionID + attachmentID
	var canonicalProfile string
	if err := s.db.QueryRow(`SELECT canonical_profile FROM processing_profiles WHERE profile_fingerprint=?`, profileFingerprint).Scan(&canonicalProfile); err != nil {
		panic(err)
	}
	var profile document.ProcessingProfileV1
	if err := json.Unmarshal([]byte(canonicalProfile), &profile); err != nil {
		panic(err)
	}
	_, fingerprints, err := document.CanonicalProfile(profile)
	if err != nil {
		panic(err)
	}
	var binding document.EmbeddingBindingV1
	for _, candidate := range profile.Embeddings {
		if candidate.Name == bindingID {
			binding = candidate
			break
		}
	}
	descriptor := embeddingCatalogDescriptor()
	space := EmbeddingVectorSpaceRecord{
		ID: fingerprints.VectorSpace[bindingID], ContractVersion: EmbeddingVectorSpaceContractV1,
		Descriptor: descriptor,
	}
	generation := EmbeddingInputGenerationRecord{
		ID:              testSHA256([]byte(seed + "generation")),
		SourceVersionID: versionID, ProcessingProfileFingerprint: profileFingerprint,
		EvidenceFingerprint: testSHA256([]byte(seed + "evidence")), TokenizerFingerprint: testSHA256([]byte(seed + "tokenizer")),
		ChunkPolicyFingerprint: testSHA256([]byte(seed + "chunk-policy")), FormatterFingerprint: testSHA256([]byte(seed + "formatter")),
		AttachmentID: attachmentID, GenerationChecksum: catalogSourceHash,
		CreatedAt: embeddingCatalogTime,
	}
	if kind == document.EmbeddingInputRenditionChunk {
		generated, evidence := embeddingCatalogGeneration(profile, fingerprints.EvidenceLexical, attachmentID)
		generation.EvidenceJSON, _, err = document.MarshalNormalizedEvidenceV1(evidence)
		if err != nil {
			panic(err)
		}
		generation.ID = testSHA256([]byte("embedding-generation-attachment/v1\x00" + generated.Checksum + "\x00" + attachmentID))
		generation.GenerationJSON, err = document.MarshalEmbeddingInputGeneration(generated)
		if err != nil {
			panic(err)
		}
		generation.GenerationBlobHash = testSHA256(generation.GenerationJSON)
		generation.GenerationEncodedSize = int64(len(generation.GenerationJSON))
		generation.GenerationChecksum = generated.Checksum
		if err := s.withStorageTx(context.Background(), func(tx *sql.Tx) error {
			if err := s.EnsureBlobTx(tx, testSHA256(generation.EvidenceJSON), int64(len(generation.EvidenceJSON))); err != nil {
				return err
			}
			return s.EnsureBlobTx(tx, generation.GenerationBlobHash, generation.GenerationEncodedSize)
		}); err != nil {
			panic(err)
		}
		generation.Inputs = make([]EmbeddingInputReference, len(generated.Inputs))
		for index, input := range generated.Inputs {
			generation.Inputs[index] = EmbeddingInputReference{ID: input.Key, RenderedChecksum: input.Checksum}
		}
	} else {
		generation.Inputs = []EmbeddingInputReference{{ID: versionID, RenderedChecksum: catalogSourceHash}}
	}
	expectedKeys := make([]string, len(generation.Inputs))
	expectedChecksums := make([]string, len(generation.Inputs))
	if kind == document.EmbeddingInputRenditionChunk {
		generated, err := document.DecodeEmbeddingInputGeneration(generation.GenerationJSON, document.EmbeddingInputGenerationDecodeBounds{
			MaxEncodedBytes: int64(len(generation.GenerationJSON)), MaxInputs: len(generation.Inputs),
		})
		if err != nil {
			panic(err)
		}
		for index, input := range generated.Inputs {
			expectedKeys[index], expectedChecksums[index] = input.Key, input.Checksum
		}
	} else {
		expectedKeys[0], expectedChecksums[0] = versionID, catalogSourceHash
	}
	values := make([][]float64, len(expectedKeys))
	for index := range values {
		values[index] = []float64{float64(index + 1), 2, 3, 4, 5, 6, 7, 8}
	}
	canonicalVectors, err := document.NewVectorSetV1(document.VectorSetV1Input{
		VectorSpaceFingerprint: space.ID, Metric: binding.Metric, Normalization: binding.Normalization,
		Dimension: binding.Dimensions, InputKeys: expectedKeys, InputChecksums: expectedChecksums, Values: values,
	})
	if err != nil {
		panic(err)
	}
	payload, payloadChecksum, err := document.EncodeVectorSetV1(canonicalVectors)
	if err != nil {
		panic(err)
	}
	payloadHash := testSHA256(payload)
	if err := s.withStorageTx(context.Background(), func(tx *sql.Tx) error { return s.EnsureBlobTx(tx, payloadHash, int64(len(payload))) }); err != nil {
		panic(err)
	}
	vectorSet := EmbeddingVectorSetRecord{
		ID: payloadChecksum, ContractVersion: EmbeddingVectorSetContractV1, VectorSpaceID: space.ID,
		PayloadBlobHash: payloadHash, PayloadChecksum: payloadChecksum, Payload: payload,
	}
	return EmbeddingSetRecord{
		ID: testSHA256([]byte(seed + "set")), VaultID: s.VaultID(), BindingID: bindingID, InputKind: kind,
		ContentVersionID: versionID, ProcessingProfileFingerprint: profileFingerprint,
		EmbeddingInputFingerprint: fingerprints.EmbeddingInput[bindingID],
		VectorSpace:               space, InputGeneration: generation, VectorSet: vectorSet, CreatedAt: embeddingCatalogTime,
	}
}

type embeddingCatalogTokenizer struct{}

func (embeddingCatalogTokenizer) Identity() document.TokenizerIdentity {
	return document.TokenizerIdentity{Name: "catalog-runes", Revision: "v1"}
}

func (embeddingCatalogTokenizer) PrefixTokenCountsMonotonic() bool { return true }

func (embeddingCatalogTokenizer) Tokenize(text string, limit int) ([]document.TokenBoundary, error) {
	runes := []rune(text)
	if len(runes) > limit {
		return nil, document.ErrTokenizerLimit
	}
	result := make([]document.TokenBoundary, len(runes))
	for index := range runes {
		result[index] = document.TokenBoundary{Start: index, End: index + 1}
	}
	return result, nil
}

func embeddingCatalogEvidence(t *testing.T) document.NormalizedEvidenceV1 {
	t.Helper()
	policy, err := document.NewEvidencePolicy(4096)
	require.NoError(t, err)
	evidence, err := document.NormalizeEvidenceV1(document.SourceEvidenceV1{
		ContractVersion: document.SourceEvidenceContractV1, Completeness: document.EvidenceComplete,
		Family: "pdf", UnitKind: document.EvidenceUnitPage,
		Units: []document.SourceEvidenceUnitV1{
			{Order: 0, Text: "Synthetic evidence", Locator: document.SourceEvidenceLocatorV1{Kind: document.EvidenceLocatorPage, IndexOrigin: document.EvidenceIndexOriginOne, Start: 1, End: 1}},
			{Order: 1, Text: "Second evidence", Locator: document.SourceEvidenceLocatorV1{Kind: document.EvidenceLocatorPage, IndexOrigin: document.EvidenceIndexOriginOne, Start: 2, End: 2}},
		},
	}, policy)
	require.NoError(t, err)
	return evidence
}

func embeddingCatalogDescriptor() document.EmbeddingDescriptor {
	contract, err := document.NewModelInputContract(document.ModelInputContractConfig{
		Profile: document.ModelInputProfileCustom, CompatibilityID: "synthetic-space",
		Document: document.ModelInputEncoder{Mode: document.ModelInputModeText, Template: "document: {{content}}"},
		Query:    document.ModelInputEncoder{Mode: document.ModelInputModeText, Template: "query: {{content}}"},
	})
	if err != nil {
		panic(err)
	}
	descriptor, err := document.NewEmbeddingDescriptor(document.EmbeddingDescriptor{
		ID: "synthetic-embedding", ContractVersion: document.EmbeddingProviderContractVersion,
		PolicyFingerprint: fakeHash("e1"), TrustBoundary: document.EmbeddingTrustLocalProcess,
		Model: "synthetic-v1", ModelRevision: "2026-08-25", Dimension: 8,
		Metric: document.VectorMetricCosine, Normalization: document.VectorNormalizationNone,
		ScalarEncoding: "float32", DocumentFormatter: "document/v1", QueryFormatter: "query/v1",
		InputKinds:      []document.EmbeddingInputKind{document.EmbeddingInputOriginalFile, document.EmbeddingInputRenditionChunk},
		CompatibilityID: "synthetic-space", SupportsTextQuery: true, ModelInput: contract,
		SupportedRequestModes: []document.ModelInputMode{document.ModelInputModeText},
	})
	if err != nil {
		panic(err)
	}
	return descriptor
}

func embeddingCatalogProfile(t *testing.T) ProcessingProfileRecord {
	t.Helper()
	descriptor := embeddingCatalogDescriptor()
	base := catalogProcessingProfile(t, false)
	var profile document.ProcessingProfileV1
	require.NoError(t, json.Unmarshal(base.CanonicalProfile, &profile))
	makeBinding := func(name string, activation document.EmbeddingActivation, kind document.EmbeddingInputKind) document.EmbeddingBindingV1 {
		binding := document.EmbeddingBindingV1{
			Activation: activation, AuthorizationFingerprint: fakeHash("d1"), CompatibilityID: descriptor.CompatibilityID,
			CredentialBinding: "credential:catalog-embedding", Descriptor: document.ProviderDescriptorV1{ID: descriptor.ID, Fingerprint: descriptor.Fingerprint},
			Dimensions: descriptor.Dimension, DisclosureFingerprint: fakeHash("d3"), DocumentFormatter: descriptor.DocumentFormatter,
			InputKind: kind, MaxBatchItems: 128, MaxInputBytes: 1 << 20, MaxResponseBytes: 1 << 20,
			Metric: descriptor.Metric, Model: descriptor.Model, ModelInput: descriptor.ModelInput, Name: name,
			Normalization: descriptor.Normalization, QueryFormatter: descriptor.QueryFormatter,
			ScalarEncoding: descriptor.ScalarEncoding, TrustBoundary: string(descriptor.TrustBoundary),
		}
		if kind == document.EmbeddingInputRenditionChunk {
			binding.MaxInputTokens = 512
			binding.Chunk = &document.EmbeddingChunkPolicyV1{ContextFingerprint: fakeHash("d4"), Formatter: "evidence-text/v1", MaxTokens: 128, OverlapTokens: 0, Tokenizer: "catalog-runes", TokenizerRevision: "v1", TruncationPolicy: document.TruncationPolicyReject}
		}
		return binding
	}
	profile.Embeddings = []document.EmbeddingBindingV1{
		makeBinding("optional", document.EmbeddingOptional, document.EmbeddingInputOriginalFile),
		makeBinding("required", document.EmbeddingRequired, document.EmbeddingInputOriginalFile),
		makeBinding("chunk", document.EmbeddingOptional, document.EmbeddingInputRenditionChunk),
		makeBinding("chunk-alt", document.EmbeddingOptional, document.EmbeddingInputRenditionChunk),
	}
	profile.Embeddings[3].Chunk.MaxTokens = 64
	canonical, fingerprints, err := document.CanonicalProfile(profile)
	require.NoError(t, err)
	return ProcessingProfileRecord{
		Fingerprint: fingerprints.Profile, CanonicalProfile: canonical,
		RenditionRequestFingerprint: fingerprints.RenditionRequest, EvidenceLexicalFingerprint: fingerprints.EvidenceLexical,
		RetentionDisclosureFingerprint: fingerprints.RetentionDisclosure,
		AttachmentPolicyFingerprint:    profile.RetentionDisclosure.AttachmentPolicyFingerprint,
		ConsentFingerprint:             profile.RetentionDisclosure.ConsentFingerprint,
		RenditionDisclosureFingerprint: profile.Rendition.DisclosureFingerprint,
		TrustBoundary:                  profile.RetentionDisclosure.TrustBoundary,
	}
}

func embeddingCatalogGeneration(profile document.ProcessingProfileV1, lexicalFingerprint, attachmentID string) (document.EmbeddingInputGeneration, document.NormalizedEvidenceV1) {
	policy, err := document.NewEvidencePolicy(4096)
	if err != nil {
		panic(err)
	}
	evidence, err := document.NormalizeEvidenceV1(document.SourceEvidenceV1{
		ContractVersion: document.SourceEvidenceContractV1, Completeness: document.EvidenceComplete,
		Family: "pdf", UnitKind: document.EvidenceUnitPage,
		Units: []document.SourceEvidenceUnitV1{
			{Order: 0, Text: "Synthetic evidence", Locator: document.SourceEvidenceLocatorV1{Kind: document.EvidenceLocatorPage, IndexOrigin: document.EvidenceIndexOriginOne, Start: 1, End: 1}},
			{Order: 1, Text: "Second evidence", Locator: document.SourceEvidenceLocatorV1{Kind: document.EvidenceLocatorPage, IndexOrigin: document.EvidenceIndexOriginOne, Start: 2, End: 2}},
		},
	}, policy)
	if err != nil {
		panic(err)
	}
	binding := profile.Embeddings[0]
	for _, candidate := range profile.Embeddings {
		if candidate.Name == "chunk" {
			binding = candidate
		}
	}
	var attachment *document.AttachmentContextSnapshot
	if attachmentID != "" {
		context, contextErr := document.NewAttachmentContextSnapshot("Synthetic title", "Synthetic context")
		if contextErr != nil {
			panic(contextErr)
		}
		attachment = &context
	}
	inputPolicy, err := document.NewInputPolicy(binding, embeddingCatalogTokenizer{}, lexicalFingerprint, attachment)
	if err != nil {
		panic(err)
	}
	generation, err := document.BuildEmbeddingInputs(evidence, inputPolicy, document.GenerationLimits{
		MaxInputs: 128, MaxTotalContentTokens: 4096, MaxTotalRenderedTokens: 8192,
		MaxTotalContentBytes: 1 << 20, MaxTotalRenderedBytes: 2 << 20,
		MaxFittingWorkTokens: 1 << 20, MaxFittingWorkBytes: 8 << 20,
	})
	if err != nil {
		panic(err)
	}
	return generation, evidence
}

func cloneEmbeddingSetRecord(value EmbeddingSetRecord) EmbeddingSetRecord {
	clone := value
	clone.InputGeneration.GenerationJSON = append([]byte(nil), value.InputGeneration.GenerationJSON...)
	clone.InputGeneration.EvidenceJSON = append([]byte(nil), value.InputGeneration.EvidenceJSON...)
	clone.InputGeneration.Inputs = append([]EmbeddingInputReference(nil), value.InputGeneration.Inputs...)
	clone.VectorSet.Payload = append([]byte(nil), value.VectorSet.Payload...)
	clone.VectorSet.rows = append([]EmbeddingVectorRowRecord(nil), value.VectorSet.rows...)
	return clone
}

func TestEmbeddingJobsRetainQueuedInputsUntilExplicitPurge(t *testing.T) {
	s, versionID, profile, _ := newEmbeddingCatalogFixture(t)
	request := embeddingJobTestRequest(t, s, versionID, profile, "queued-retention")
	job, err := s.EnqueueEmbeddingJob(t.Context(), request)
	require.NoError(t, err)
	_, err = s.PurgeDerivatives(t.Context(), PurgeRequest{})
	require.NoError(t, err)
	claim, work, found, err := s.ClaimNextEmbeddingWork(t.Context(), "retention-worker", time.Now().UTC(), time.Minute, []string{request.Descriptor.Fingerprint})
	require.NoError(t, err)
	require.True(t, found, "ordinary collection must retain queued work and its vector space")
	_, err = s.PurgeDerivatives(t.Context(), PurgeRequest{ContentVersionIDs: []string{versionID}})
	require.NoError(t, err)
	var remaining int
	require.NoError(t, s.db.QueryRow(`SELECT
  (SELECT COUNT(*) FROM embedding_jobs WHERE job_id=?) +
  (SELECT COUNT(*) FROM current_rendition_roots WHERE root_id=?) +
  (SELECT COUNT(*) FROM embedding_input_generations WHERE generation_id=?)`, job.ID, claim.AttemptID, request.InputGeneration.ID).Scan(&remaining))
	require.Zero(t, remaining)
	require.ErrorIs(t, s.FailEmbeddingWork(t.Context(), claim, work, EmbeddingFailureProviderUnavailable,
		EmbeddingAttemptReceipt{AttemptID: claim.AttemptID}, time.Now().UTC()), ErrEmbeddingJobFenced)
	require.NoError(t, s.AbandonEmbeddingWork(t.Context(), claim, time.Now().UTC()))
	_, err = s.EnqueueEmbeddingJob(t.Context(), request)
	require.ErrorIs(t, err, ErrEmbeddingJobFenced, "purged intent must not be resubmitted to a provider")
}

func TestEmbeddingJobAbandonmentPreservesSuccessorClaim(t *testing.T) {
	s, versionID, profile, _ := newEmbeddingCatalogFixture(t)
	request := embeddingJobTestRequest(t, s, versionID, profile, "abandonment")
	_, err := s.EnqueueEmbeddingJob(t.Context(), request)
	require.NoError(t, err)
	at := time.Now().UTC()
	first, _, found, err := s.ClaimNextEmbeddingWork(t.Context(), "first-worker", at, time.Minute, []string{request.Descriptor.Fingerprint})
	require.NoError(t, err)
	require.True(t, found)
	at = at.Add(2 * time.Minute)
	require.NoError(t, s.AbandonEmbeddingWork(t.Context(), first, at))
	successor, work, found, err := s.ClaimNextEmbeddingWork(t.Context(), "successor-worker", at, time.Minute, []string{request.Descriptor.Fingerprint})
	require.NoError(t, err)
	require.True(t, found)
	require.NoError(t, s.AbandonEmbeddingWork(t.Context(), first, at))
	require.NoError(t, s.ValidateEmbeddingWork(t.Context(), successor, work, at))
	require.NoError(t, s.AbandonEmbeddingWork(t.Context(), successor, at))
	require.ErrorIs(t, s.ValidateEmbeddingWork(t.Context(), successor, work, at), ErrEmbeddingJobFenced)
	var state string
	require.NoError(t, s.db.QueryRow(`SELECT state FROM embedding_jobs WHERE job_id=?`, successor.AttemptID).Scan(&state))
	require.Equal(t, "abandoned", state)
	var failures int
	require.NoError(t, s.db.QueryRow(`SELECT COUNT(*) FROM embedding_failures`).Scan(&failures))
	require.Zero(t, failures)
}

func TestEmbeddingEgressKeepsOriginalConsentAcrossBatches(t *testing.T) {
	s, versionID, profile, _ := newEmbeddingCatalogFixture(t)
	request := embeddingJobTestRequest(t, s, versionID, profile, "egress-consent")
	_, err := s.EnqueueEmbeddingJob(t.Context(), request)
	require.NoError(t, err)
	at := time.Now().UTC()
	claim, work, found, err := s.ClaimNextEmbeddingWork(t.Context(), "embedding-worker", at, time.Minute, []string{request.Descriptor.Fingerprint})
	require.NoError(t, err)
	require.True(t, found)
	first, fence, err := s.BeginEmbeddingProviderEgress(t.Context(), claim, work, nil, at)
	require.NoError(t, err)
	fence.Close()
	second, fence, err := s.BeginEmbeddingProviderEgress(t.Context(), claim, work, &first, at)
	require.NoError(t, err)
	fence.Close()
	require.Equal(t, first.GrantID, second.GrantID)
	consent := request.Authorization
	_, err = s.RevokeConsent(t.Context(), ProcessingConsentRevocationRequest{Principal: consent.Principal, Scope: consent.Scope})
	require.NoError(t, err)
	_, err = s.GrantConsent(t.Context(), ProcessingConsentGrantRequest{
		Principal: consent.Principal, Scope: consent.Scope, ProfileFingerprint: consent.ProfileFingerprint,
		DisclosureFingerprint: consent.DisclosureFingerprint, InputClasses: consent.InputClasses,
		RetainedArtifactClasses: consent.RetainedArtifactClasses,
	})
	require.NoError(t, err)
	_, fence, err = s.BeginEmbeddingProviderEgress(t.Context(), claim, work, &second, at)
	require.ErrorIs(t, err, ErrProcessingConsentRevoked)
	require.Nil(t, fence)
	// Fresh work can use the replacement grant after a failed prior check.
	fresh, fence, err := s.BeginEmbeddingProviderEgress(t.Context(), claim, work, nil, at)
	require.NoError(t, err)
	fence.Close()
	require.NotEqual(t, first.GrantID, fresh.GrantID)
}

func TestEmbeddingJobReplacementConsentPreservesClaimsAndRetryBudget(t *testing.T) {
	for _, state := range []string{"queued", "running", "retry_wait", "authorization_failed", "exhausted"} {
		t.Run(state, func(t *testing.T) {
			s, version, profile, _ := newEmbeddingCatalogFixture(t)
			request := embeddingJobTestRequest(t, s, version, profile, "replacement-consent")
			job, err := s.EnqueueEmbeddingJob(t.Context(), request)
			require.NoError(t, err)
			at := time.Now().UTC()
			var claim EmbeddingJobClaim
			var work EmbeddingJobWork
			count := 0
			if state != "queued" {
				attempts := 1
				if state == "exhausted" {
					attempts = embeddingJobMaxClaims
				}
				for range attempts {
					var found bool
					claim, work, found, err = s.ClaimNextEmbeddingWork(t.Context(), "worker", at, time.Minute, []string{request.Descriptor.Fingerprint})
					require.NoError(t, err)
					require.True(t, found)
					count++
					if state != "running" {
						code := EmbeddingFailureProviderUnavailable
						if state == "authorization_failed" {
							code = EmbeddingFailureAuthorization
						}
						require.NoError(t, s.FailEmbeddingWork(t.Context(), claim, work, code, EmbeddingAttemptReceipt{AttemptID: claim.AttemptID}, at))
					}
					if state == "exhausted" {
						at = at.Add(2 * time.Minute)
					}
				}
			}
			_, err = s.RevokeConsent(t.Context(), ProcessingConsentRevocationRequest{Principal: request.Authorization.Principal, Scope: request.Authorization.Scope})
			require.NoError(t, err)
			replacement := request
			replacement.Authorization.Principal = "operator:replacement"
			replacement.Authorization.Scope = "embedding:replacement"
			consent := replacement.Authorization
			_, err = s.GrantConsent(t.Context(), ProcessingConsentGrantRequest{Principal: consent.Principal, Scope: consent.Scope, ProfileFingerprint: consent.ProfileFingerprint, DisclosureFingerprint: consent.DisclosureFingerprint, InputClasses: consent.InputClasses, RetainedArtifactClasses: consent.RetainedArtifactClasses})
			require.NoError(t, err)
			same, err := s.EnqueueEmbeddingJob(t.Context(), replacement)
			require.NoError(t, err)
			require.Equal(t, job.ID, same.ID)
			var claims int
			require.NoError(t, s.db.QueryRow(`SELECT claim_count FROM embedding_jobs WHERE job_id=?`, job.ID).Scan(&claims))
			require.Equal(t, count, claims)
			if state == "running" {
				require.NoError(t, s.ValidateEmbeddingWork(t.Context(), claim, work, at))
				_, fence, err := s.BeginEmbeddingProviderEgress(t.Context(), claim, work, nil, at)
				fence.Close()
				require.ErrorIs(t, err, ErrProcessingConsentRevoked)
				require.NoError(t, s.FailEmbeddingWork(t.Context(), claim, work, EmbeddingFailureAuthorization, EmbeddingAttemptReceipt{AttemptID: claim.AttemptID}, at))
				_, err = s.EnqueueEmbeddingJob(t.Context(), replacement)
				require.NoError(t, err)
			}
			if state == "retry_wait" {
				_, _, found, err := s.ClaimNextEmbeddingWork(t.Context(), "early-worker", at, time.Minute, []string{request.Descriptor.Fingerprint})
				require.NoError(t, err)
				require.False(t, found, "replacement consent must preserve retry delay")
			}
			at = at.Add(2 * time.Minute)
			next, rebound, found, err := s.ClaimNextEmbeddingWork(t.Context(), "next-worker", at, time.Minute, []string{request.Descriptor.Fingerprint})
			require.NoError(t, err)
			if state == "exhausted" {
				require.False(t, found, "replacement consent must not restart exhausted provider retries")
				return
			}
			require.True(t, found)
			require.Equal(t, consent.Principal, rebound.Consent.Principal)
			require.Equal(t, consent.Scope, rebound.Consent.Scope)
			_, fence, err := s.BeginEmbeddingProviderEgress(t.Context(), next, rebound, nil, at)
			require.NoError(t, err)
			fence.Close()
		})
	}
}

func TestEmbeddingJobReconciliationRecoversFreshGrantForSameScope(t *testing.T) {
	s, version, profile, _ := newEmbeddingCatalogFixture(t)
	request := embeddingJobTestRequest(t, s, version, profile, "fresh-grant")
	job, err := s.EnqueueEmbeddingJob(t.Context(), request)
	require.NoError(t, err)
	at := time.Now().UTC()
	claim, work, found, err := s.ClaimNextEmbeddingWork(t.Context(), "worker", at, time.Minute, []string{request.Descriptor.Fingerprint})
	require.NoError(t, err)
	require.True(t, found)
	consent := request.Authorization
	_, err = s.RevokeConsent(t.Context(), ProcessingConsentRevocationRequest{Principal: consent.Principal, Scope: consent.Scope})
	require.NoError(t, err)
	require.NoError(t, s.FailEmbeddingWork(t.Context(), claim, work, EmbeddingFailureAuthorization, EmbeddingAttemptReceipt{AttemptID: claim.AttemptID}, at))
	_, err = s.GrantConsent(t.Context(), ProcessingConsentGrantRequest{Principal: consent.Principal, Scope: consent.Scope, ProfileFingerprint: consent.ProfileFingerprint, DisclosureFingerprint: consent.DisclosureFingerprint, InputClasses: consent.InputClasses, RetainedArtifactClasses: consent.RetainedArtifactClasses})
	require.NoError(t, err)
	_, err = s.ReconcileEmbeddingJobs(t.Context(), EmbeddingReconcileRequest{Mutate: embeddingTestMutation, At: time.Now().UTC(), Limit: 100, DescriptorFingerprints: []string{request.Descriptor.Fingerprint}})
	require.NoError(t, err)
	recovered := false
	for {
		next, rebound, found, err := s.ClaimNextEmbeddingWork(t.Context(), "next-worker", time.Now().UTC(), time.Minute, []string{request.Descriptor.Fingerprint})
		require.NoError(t, err)
		if !found {
			break
		}
		if next.AttemptID != job.ID {
			continue
		}
		_, fence, err := s.BeginEmbeddingProviderEgress(t.Context(), next, rebound, nil, time.Now().UTC())
		require.NoError(t, err)
		fence.Close()
		recovered = true
	}
	require.True(t, recovered, "reconciliation must revive the original authorization-failed job")
}

func TestEmbeddingValidationPreservesReadErrors(t *testing.T) {
	s, versionID, profile, _ := newEmbeddingCatalogFixture(t)
	request := embeddingJobTestRequest(t, s, versionID, profile, "validation-read-error")
	_, err := s.EnqueueEmbeddingJob(t.Context(), request)
	require.NoError(t, err)
	at := time.Now().UTC()
	claim, work, found, err := s.ClaimNextEmbeddingWork(t.Context(), "worker-read-error", at, time.Minute, []string{request.Descriptor.Fingerprint})
	require.NoError(t, err)
	require.True(t, found)
	err = s.withStorageTx(t.Context(), func(tx *sql.Tx) error {
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		readErr := requireEmbeddingWorkerLeaseTx(ctx, tx, claim.AttemptID, claim.Epoch, work.InputGeneration.ID, at)
		require.ErrorIs(t, readErr, context.Canceled)
		classified := validateEmbeddingWorkTx(ctx, tx, s.vaultID, claim, work, at)
		require.NotErrorIs(t, classified, ErrEmbeddingJobFenced)
		require.ErrorIs(t, classified, context.Canceled)
		require.NoError(t, validateEmbeddingWorkTx(t.Context(), tx, s.vaultID, claim, work, at))
		return nil
	})
	require.NoError(t, err)
}

func TestEmbeddingGCReleasesTerminalJobArtifacts(t *testing.T) {
	for _, state := range []string{"completed", "failed", "abandoned", "queued", "retry_wait", "running", "failed_without_set", "abandoned_without_set"} {
		t.Run(state, func(t *testing.T) {
			hasSet := !strings.HasSuffix(state, "_without_set")
			state = strings.TrimSuffix(state, "_without_set")
			s, versionID, profile, attachmentID := newEmbeddingCatalogFixture(t)
			record := embeddingSetFixture(s, versionID, profile.Fingerprint, document.EmbeddingInputRenditionChunk, "chunk", attachmentID)
			normalized, normalizeErr := normalizeEmbeddingSetRecord(record)
			require.NoError(t, normalizeErr)
			record = normalized
			binding := workerProfileEmbeddingBinding(t, profile, "chunk")
			consent := ProviderOperationAuthorizationRequest{Principal: "operator:gc-probe", Scope: "embedding:chunk", ProfileFingerprint: profile.Fingerprint, DisclosureFingerprint: binding.DisclosureFingerprint, InputClasses: []string{string(binding.InputKind)}, RetainedArtifactClasses: []string{"embedding_vector_set"}}
			_, err := s.GrantConsent(t.Context(), ProcessingConsentGrantRequest{Principal: consent.Principal, Scope: consent.Scope, ProfileFingerprint: consent.ProfileFingerprint, DisclosureFingerprint: consent.DisclosureFingerprint, InputClasses: consent.InputClasses, RetainedArtifactClasses: consent.RetainedArtifactClasses})
			require.NoError(t, err)
			job, err := s.EnqueueEmbeddingJob(t.Context(), EmbeddingJobRequest{ContentVersionID: versionID, Profile: profile, BindingID: binding.Name, Descriptor: record.VectorSpace.Descriptor, InputGeneration: record.InputGeneration, Authorization: consent})
			require.NoError(t, err)
			at := time.Now().UTC()
			claim, work, found, err := s.ClaimNextEmbeddingWork(t.Context(), "gc-worker", at, time.Minute, []string{record.VectorSpace.Descriptor.Fingerprint})
			require.NoError(t, err)
			require.True(t, found)
			if hasSet {
				require.NoError(t, s.StageEmbeddingSetWithLease(t.Context(), record, claim.AttemptID, claim.Epoch, at))
			}
			switch state {
			case "completed":
				auth, err := s.AuthorizeProviderOperation(t.Context(), consent)
				require.NoError(t, err)
				require.NoError(t, s.PublishEmbeddingWork(t.Context(), claim, work,
					EmbeddingHeadRecord{Key: EmbeddingHeadKey{versionID, binding.Name, binding.InputKind}, SetID: record.ID, VectorSpaceID: record.VectorSpace.ID, ProcessingProfileFingerprint: profile.Fingerprint, PublishedAt: embeddingCatalogTime, FencingToken: claim.Epoch},
					auth, EmbeddingAttemptReceipt{AttemptID: job.ID}, at))
			case "failed":
				require.NoError(t, s.FailEmbeddingWork(t.Context(), claim, work, EmbeddingFailureInputRejected, EmbeddingAttemptReceipt{AttemptID: job.ID}, at))
			case "abandoned":
				require.NoError(t, s.AbandonEmbeddingWork(t.Context(), claim, at))
			case "retry_wait":
				require.NoError(t, s.FailEmbeddingWork(t.Context(), claim, work, EmbeddingFailureProviderUnavailable, EmbeddingAttemptReceipt{AttemptID: job.ID}, at))
			case "queued":
				require.NoError(t, s.FailEmbeddingWork(t.Context(), claim, work, EmbeddingFailureAuthorization, EmbeddingAttemptReceipt{AttemptID: job.ID}, at))
				_, err = s.EnqueueEmbeddingJob(t.Context(), EmbeddingJobRequest{ContentVersionID: versionID, Profile: profile, BindingID: binding.Name, Descriptor: record.VectorSpace.Descriptor, InputGeneration: record.InputGeneration, Authorization: consent})
				require.NoError(t, err)
			}
			if !hasSet {
				_, err = s.PurgeDerivatives(t.Context(), PurgeRequest{})
				require.NoError(t, err)
				var retained int
				require.NoError(t, s.db.QueryRow(`SELECT COUNT(*) FROM embedding_jobs WHERE job_id=?`, job.ID).Scan(&retained))
				require.Equal(t, 1, retained, "current source must retain its terminal work")
			}
			build := catalogRenditionBuild(s, profile)
			build.ID = testSHA256([]byte("gc-probe-replacement-build"))
			build.CapturedArtifactPolicy = []byte(`{"roles":[{"max_count":1,"min_count":1,"role":"normalized_evidence"},{"max_count":1,"min_count":0,"role":"provider_markdown"},{"max_count":1,"min_count":1,"role":"sanitized_markdown"}],"version":1}`)
			build.CapturedArtifactPolicyFingerprint = testSHA256(build.CapturedArtifactPolicy)
			require.NoError(t, s.StageRenditionBuild(t.Context(), build))
			buildID := build.ID
			replacement := RenditionAttachmentRecord{ID: testSHA256([]byte("gc-probe-replacement-attachment")), VaultID: s.VaultID(), ContentVersionID: versionID, BuildID: buildID, Profile: profile, AttachedAt: embeddingCatalogTime}
			require.NoError(t, publishRenditionForTest(t, s, replacement, embeddingCatalogTime, testSHA256([]byte("gc-probe-replacement-lexical"))))
			report, err := s.PurgeDerivatives(t.Context(), PurgeRequest{})
			require.NoError(t, err)
			if state == "running" || !hasSet {
				require.Zero(t, report.RemovedEmbeddingSets)
			} else {
				require.Equal(t, 1, report.RemovedEmbeddingSets)
			}
			var jobs, generations, spaces int
			require.NoError(t, s.db.QueryRow(`SELECT (SELECT COUNT(*) FROM embedding_jobs WHERE job_id=?),(SELECT COUNT(*) FROM embedding_input_generations WHERE generation_id=?),(SELECT COUNT(*) FROM embedding_vector_spaces WHERE vector_space_id=?)`, job.ID, record.InputGeneration.ID, record.VectorSpace.ID).Scan(&jobs, &generations, &spaces))
			if state == "queued" || state == "retry_wait" || state == "running" {
				require.Equal(t, 1, jobs)
				require.Equal(t, 1, generations)
				require.Equal(t, 1, spaces)
			} else {
				require.Zero(t, jobs)
				require.Zero(t, generations)
				require.Zero(t, spaces)
			}
		})
	}
}

func TestEmbeddingJobEnqueueRejectsStaleSource(t *testing.T) {
	for _, mutation := range []string{"replace", "trash"} {
		t.Run(mutation, func(t *testing.T) {
			s, versionID, profile, _ := newEmbeddingCatalogFixture(t)
			request := embeddingJobTestRequest(t, s, versionID, profile, "stale-enqueue")
			var nodeID, revision int64
			require.NoError(t, s.db.QueryRow(`SELECT id,revision FROM nodes WHERE current_version_id=?`, versionID).Scan(&nodeID, &revision))
			var err error
			if mutation == "trash" {
				_, _, err = s.Trash(t.Context(), nodeID, revision)
			} else {
				_, _, err = s.ReplaceContent(t.Context(), nodeID, revision, fakeHash("b2"), 4, "text/plain")
			}
			require.NoError(t, err)
			_, err = s.EnqueueEmbeddingJob(t.Context(), request)
			require.ErrorIs(t, err, ErrEmbeddingJobFenced)
			var count int
			require.NoError(t, s.db.QueryRow(`SELECT COUNT(*) FROM embedding_jobs WHERE content_version_id=?`, versionID).Scan(&count))
			require.Zero(t, count)
		})
	}
}

func TestEmbeddingJobsResumeAfterSourceRestoration(t *testing.T) {
	s, versionID, profile, _ := newEmbeddingCatalogFixture(t)
	request := embeddingJobTestRequest(t, s, versionID, profile, "restore-abandoned")
	_, err := s.EnqueueEmbeddingJob(t.Context(), request)
	require.NoError(t, err)
	at := time.Now().UTC()
	claim, work, found, err := s.ClaimNextEmbeddingWork(t.Context(), "first-worker", at, time.Minute, []string{request.Descriptor.Fingerprint})
	require.NoError(t, err)
	require.True(t, found)
	var nodeID, revision int64
	require.NoError(t, s.db.QueryRow(`SELECT id,revision FROM nodes WHERE current_version_id=?`, versionID).Scan(&nodeID, &revision))
	trashed, _, err := s.Trash(t.Context(), nodeID, revision)
	require.NoError(t, err)
	require.ErrorIs(t, s.ValidateEmbeddingWork(t.Context(), claim, work, at), ErrEmbeddingJobFenced)
	require.NoError(t, s.AbandonEmbeddingWork(t.Context(), claim, at))
	_, err = s.EnqueueEmbeddingJob(t.Context(), request)
	require.ErrorIs(t, err, ErrEmbeddingJobFenced)
	reconciled, err := s.ReconcileEmbeddingJobs(t.Context(), EmbeddingReconcileRequest{Mutate: embeddingTestMutation, At: time.Now().UTC(), Limit: 100, DescriptorFingerprints: []string{request.Descriptor.Fingerprint}})
	require.NoError(t, err)
	require.Zero(t, reconciled.Enqueued, "trashed sources must not reopen jobs")
	_, _, err = s.Restore(t.Context(), trashed.ID, trashed.Revision)
	require.NoError(t, err)
	_, err = s.RevokeConsent(t.Context(), ProcessingConsentRevocationRequest{Principal: request.Authorization.Principal, Scope: request.Authorization.Scope})
	require.NoError(t, err)
	reconciled, err = s.ReconcileEmbeddingJobs(t.Context(), EmbeddingReconcileRequest{Mutate: embeddingTestMutation, At: time.Now().UTC(), Limit: 100, DescriptorFingerprints: []string{request.Descriptor.Fingerprint}})
	require.NoError(t, err)
	require.Zero(t, reconciled.Enqueued, "restoration alone must not replace consent")
	request = embeddingJobTestRequest(t, s, versionID, profile, "restore-abandoned")
	reconciled, err = s.ReconcileEmbeddingJobs(t.Context(), EmbeddingReconcileRequest{Mutate: embeddingTestMutation, At: time.Now().UTC(), Limit: 100, DescriptorFingerprints: []string{request.Descriptor.Fingerprint}})
	require.NoError(t, err)
	require.Positive(t, reconciled.Enqueued)
	resumed := false
	for i := range 10 {
		next, _, available, err := s.ClaimNextEmbeddingWork(t.Context(), "restored-worker", time.Now().UTC(), time.Minute, []string{request.Descriptor.Fingerprint})
		require.NoError(t, err)
		if !available {
			break
		}
		if next.AttemptID == claim.AttemptID {
			resumed = true
			require.Greater(t, next.Epoch, claim.Epoch)
		}
		require.Less(t, i, 9)
	}
	require.True(t, resumed, "restored source with fresh consent must resume its existing job")
	require.ErrorIs(t, s.ValidateEmbeddingWork(t.Context(), claim, work, time.Now().UTC()), ErrEmbeddingJobFenced)
}

func TestEmbeddingReconciliationSkipsSuppressedRetainedGeneration(t *testing.T) {
	s, versionID, profile, _ := newEmbeddingCatalogFixture(t)
	request := embeddingJobTestRequest(t, s, versionID, profile, "suppressed-retained")
	_, err := s.EnqueueEmbeddingJob(t.Context(), request)
	require.NoError(t, err)
	require.NoError(t, s.PutCurrentRenditionRoot(t.Context(), CurrentRenditionRoot{
		ID: "retained-input", Kind: RenditionRootRetention, TargetKind: RenditionRootEmbeddingGeneration,
		TargetID: request.InputGeneration.ID, FencingToken: 1, RecordedAt: embeddingCatalogTime,
	}))
	var siblingVersion string
	require.NoError(t, s.db.QueryRow(`SELECT version_id FROM content_versions WHERE version_id<>? LIMIT 1`, versionID).Scan(&siblingVersion))
	sibling := embeddingJobTestRequest(t, s, siblingVersion, profile, "other-source")
	_, err = s.EnqueueEmbeddingJob(t.Context(), sibling)
	require.NoError(t, err)
	_, err = s.PurgeDerivatives(t.Context(), PurgeRequest{ContentVersionIDs: []string{versionID}})
	require.NoError(t, err)
	for pass := range 3 {
		result, err := s.ReconcileEmbeddingJobs(t.Context(), EmbeddingReconcileRequest{Mutate: embeddingTestMutation, At: time.Now().UTC(), Limit: 100, DescriptorFingerprints: []string{request.Descriptor.Fingerprint}})
		require.NoError(t, err)
		if pass > 0 {
			require.Zero(t, result.Enqueued, "existing sibling jobs do not need enqueueing again")
		}
	}
	_, work, found, err := s.ClaimNextEmbeddingWork(t.Context(), "sibling-worker", time.Now().UTC(), time.Minute, []string{request.Descriptor.Fingerprint})
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, siblingVersion, work.ContentVersionID)
}

func TestEmbeddingPublicationAndJobCompletionAreAtomic(t *testing.T) {
	s, versionID, profile, _ := newEmbeddingCatalogFixture(t)
	request := embeddingJobTestRequest(t, s, versionID, profile, "atomic-completion")
	job, err := s.EnqueueEmbeddingJob(t.Context(), request)
	require.NoError(t, err)
	at := time.Now().UTC()
	claim, work, found, err := s.ClaimNextEmbeddingWork(t.Context(), "atomic-worker", at, time.Minute, []string{request.Descriptor.Fingerprint})
	require.NoError(t, err)
	require.True(t, found)
	record := embeddingSetFixture(s, versionID, profile.Fingerprint, document.EmbeddingInputOriginalFile, request.BindingID, "")
	record.InputGeneration = request.InputGeneration
	require.NoError(t, s.StageEmbeddingSetWithLease(t.Context(), record, claim.AttemptID, claim.Epoch, at))
	prior, err := s.AuthorizeProviderOperation(t.Context(), request.Authorization)
	require.NoError(t, err)
	head := EmbeddingHeadRecord{Key: EmbeddingHeadKey{versionID, request.BindingID, document.EmbeddingInputOriginalFile}, SetID: record.ID,
		VectorSpaceID: work.VectorSpaceID, ProcessingProfileFingerprint: profile.Fingerprint, PublishedAt: at.Format(timestampLayout), FencingToken: claim.Epoch}
	receipt := EmbeddingAttemptReceipt{AttemptID: job.ID}
	_, err = s.db.Exec(`CREATE TEMP TRIGGER reject_embedding_completion BEFORE UPDATE OF state ON embedding_jobs
  WHEN NEW.state='completed' BEGIN SELECT RAISE(ABORT,'synthetic completion failure'); END`)
	require.NoError(t, err)
	require.Error(t, s.PublishEmbeddingWork(t.Context(), claim, work, head, prior, receipt, at))
	var heads int
	require.NoError(t, s.db.QueryRow(`SELECT COUNT(*) FROM embedding_heads WHERE content_version_id=?`, versionID).Scan(&heads))
	require.Zero(t, heads, "completion failure must roll back publication")
	var state string
	require.NoError(t, s.db.QueryRow(`SELECT state FROM embedding_jobs WHERE job_id=?`, job.ID).Scan(&state))
	require.Equal(t, "running", state)
	_, err = s.db.Exec(`DROP TRIGGER reject_embedding_completion`)
	require.NoError(t, err)
	require.NoError(t, s.PublishEmbeddingWork(t.Context(), claim, work, head, prior, receipt, at))
	require.NoError(t, s.db.QueryRow(`SELECT state FROM embedding_jobs WHERE job_id=?`, job.ID).Scan(&state))
	require.Equal(t, "completed", state)
	require.Equal(t, record.ID, embeddingHeadSetIDForTest(t, s, versionID, profile.Fingerprint, request.BindingID, document.EmbeddingInputOriginalFile))
	var active bool
	require.NoError(t, s.db.QueryRow(`SELECT active FROM current_rendition_roots WHERE root_id=?`, claim.AttemptID).Scan(&active))
	require.False(t, active)
}

func embeddingTestMutation(_ context.Context, fn func() error) error { return fn() }

func TestEmbeddingReconciliationSkipsExistingTerminalJob(t *testing.T) {
	s, versionID, profile, attachmentID := newEmbeddingCatalogFixture(t)
	record := embeddingSetFixture(s, versionID, profile.Fingerprint,
		document.EmbeddingInputRenditionChunk, "chunk", attachmentID)
	var err error
	record, err = normalizeEmbeddingSetRecord(record)
	require.NoError(t, err)
	binding := workerProfileEmbeddingBinding(t, profile, "chunk")
	consent := ProviderOperationAuthorizationRequest{
		Principal: "operator:chunk-reconcile", Scope: "embedding:chunk",
		ProfileFingerprint: profile.Fingerprint, DisclosureFingerprint: binding.DisclosureFingerprint,
		InputClasses: []string{string(binding.InputKind)}, RetainedArtifactClasses: []string{"embedding_vector_set"},
	}
	_, err = s.GrantConsent(t.Context(), ProcessingConsentGrantRequest{
		Principal: consent.Principal, Scope: consent.Scope, ProfileFingerprint: consent.ProfileFingerprint,
		DisclosureFingerprint: consent.DisclosureFingerprint, InputClasses: consent.InputClasses,
		RetainedArtifactClasses: consent.RetainedArtifactClasses,
	})
	require.NoError(t, err)
	_, err = s.EnqueueEmbeddingJob(t.Context(), EmbeddingJobRequest{ContentVersionID: versionID,
		Profile: profile, BindingID: binding.Name, Descriptor: record.VectorSpace.Descriptor,
		InputGeneration: record.InputGeneration, Authorization: consent})
	require.NoError(t, err)
	at := time.Now().UTC()
	claim, work, found, err := s.ClaimNextEmbeddingWork(t.Context(), "failed-worker", at, time.Minute, []string{record.VectorSpace.Descriptor.Fingerprint})
	require.NoError(t, err)
	require.True(t, found)
	require.NoError(t, s.FailEmbeddingWork(t.Context(), claim, work, EmbeddingFailureInputRejected, EmbeddingAttemptReceipt{AttemptID: claim.AttemptID}, at))
	for range 2 {
		mutations := 0
		result, err := s.ReconcileEmbeddingJobs(t.Context(), EmbeddingReconcileRequest{
			At: at, Limit: 100, DescriptorFingerprints: []string{record.VectorSpace.Descriptor.Fingerprint},
			Mutate: func(_ context.Context, fn func() error) error { mutations++; return fn() },
			HydrateGeneration: func(_ context.Context, g EmbeddingInputGenerationRecord) (EmbeddingInputGenerationRecord, error) {
				return HydrateEmbeddingInputGeneration(g, record.InputGeneration.GenerationJSON, record.InputGeneration.EvidenceJSON)
			},
		})
		require.NoError(t, err)
		require.Zero(t, result.Enqueued)
		require.Zero(t, mutations)
	}
}

func TestEmbeddingReconciliationAdvancesPastUnreadableGeneration(t *testing.T) {
	s, versionID, profile, attachmentID := newEmbeddingCatalogFixture(t)
	record := embeddingSetFixture(s, versionID, profile.Fingerprint,
		document.EmbeddingInputRenditionChunk, "chunk", attachmentID)
	var err error
	record, err = normalizeEmbeddingSetRecord(record)
	require.NoError(t, err)
	binding := workerProfileEmbeddingBinding(t, profile, "chunk")
	consent := ProviderOperationAuthorizationRequest{
		Principal: "operator:chunk-reconcile", Scope: "embedding:chunk",
		ProfileFingerprint: profile.Fingerprint, DisclosureFingerprint: binding.DisclosureFingerprint,
		InputClasses: []string{string(binding.InputKind)}, RetainedArtifactClasses: []string{"embedding_vector_set"},
	}
	_, err = s.GrantConsent(t.Context(), ProcessingConsentGrantRequest{
		Principal: consent.Principal, Scope: consent.Scope, ProfileFingerprint: consent.ProfileFingerprint,
		DisclosureFingerprint: consent.DisclosureFingerprint, InputClasses: consent.InputClasses,
		RetainedArtifactClasses: consent.RetainedArtifactClasses,
	})
	require.NoError(t, err)
	_, err = s.EnqueueEmbeddingJob(t.Context(), EmbeddingJobRequest{ContentVersionID: versionID,
		Profile: profile, BindingID: binding.Name, Descriptor: record.VectorSpace.Descriptor,
		InputGeneration: record.InputGeneration, Authorization: consent})
	require.NoError(t, err)
	sibling := embeddingJobTestRequest(t, s, versionID, profile, "after-unreadable")
	sibling.InputGeneration.ID = strings.Repeat("f", 64)
	_, err = s.EnqueueEmbeddingJob(t.Context(), sibling)
	require.NoError(t, err)
	_, err = s.db.Exec(`DELETE FROM embedding_jobs`)
	require.NoError(t, err)
	inMutation := false
	request := EmbeddingReconcileRequest{At: time.Now().UTC(), Limit: 1, DescriptorFingerprints: []string{record.VectorSpace.Descriptor.Fingerprint},
		Mutate: func(_ context.Context, fn func() error) error {
			inMutation = true
			defer func() { inMutation = false }()
			return fn()
		},
		HydrateGeneration: func(_ context.Context, g EmbeddingInputGenerationRecord) (EmbeddingInputGenerationRecord, error) {
			require.False(t, inMutation, "blob reads must not hold the mutation gate")
			// An exact-size mismatch is a durable generation error from real validation.
			return HydrateEmbeddingInputGeneration(g, []byte("synthetic invalid generation"), record.InputGeneration.EvidenceJSON)
		},
	}
	first, err := s.ReconcileEmbeddingJobs(t.Context(), request)
	require.NoError(t, err)
	require.Positive(t, first.Skipped)
	require.NotEmpty(t, first.Next)
	request.After = first.Next
	second, err := s.ReconcileEmbeddingJobs(t.Context(), request)
	require.NoError(t, err)
	require.Positive(t, second.Enqueued)
	require.Empty(t, second.Next)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	request.After = ""
	request.HydrateGeneration = func(context.Context, EmbeddingInputGenerationRecord) (EmbeddingInputGenerationRecord, error) {
		cancel()
		return EmbeddingInputGenerationRecord{}, context.Canceled
	}
	_, err = s.ReconcileEmbeddingJobs(ctx, request)
	require.ErrorIs(t, err, context.Canceled)
}

func TestEmbeddingJobExpiredClaimsExhaustBudget(t *testing.T) {
	s, version, profile, _ := newEmbeddingCatalogFixture(t)
	request := embeddingJobTestRequest(t, s, version, profile, "expired-budget")
	job, err := s.EnqueueEmbeddingJob(t.Context(), request)
	require.NoError(t, err)
	at := time.Now().UTC()
	var last EmbeddingJobClaim
	for range 3 {
		claim, _, found, err := s.ClaimNextEmbeddingWork(t.Context(), "crashed-worker", at, time.Minute, []string{request.Descriptor.Fingerprint})
		require.NoError(t, err)
		require.True(t, found)
		last = claim
		// A live claim must not be retired, even on its last allowed attempt.
		_, _, found, err = s.ClaimNextEmbeddingWork(t.Context(), "other-worker", at.Add(time.Second), time.Minute, []string{request.Descriptor.Fingerprint})
		require.NoError(t, err)
		require.False(t, found)
		at = at.Add(time.Minute)
	}
	_, _, found, err := s.ClaimNextEmbeddingWork(t.Context(), "fourth-worker", at, time.Minute, []string{request.Descriptor.Fingerprint})
	require.NoError(t, err)
	require.False(t, found, "expired running jobs must not get a fourth claim")
	var state, code string
	var count int
	require.NoError(t, s.db.QueryRow(`SELECT state,failure_code,claim_count FROM embedding_jobs WHERE job_id=?`, job.ID).Scan(&state, &code, &count))
	require.Equal(t, "failed", state)
	require.Equal(t, string(EmbeddingFailureProviderUnavailable), code)
	require.Equal(t, 3, count)
	var active bool
	require.NoError(t, s.db.QueryRow(`SELECT active FROM current_rendition_roots WHERE root_id=? AND fencing_token=?`, job.ID, last.Epoch).Scan(&active))
	require.False(t, active)
	_, err = s.RenewEmbeddingWork(t.Context(), last, at, time.Minute)
	require.ErrorIs(t, err, ErrEmbeddingJobFenced)
	_, err = s.EnqueueEmbeddingJob(t.Context(), request)
	require.NoError(t, err)
	_, _, found, err = s.ClaimNextEmbeddingWork(t.Context(), "reconciled-worker", at, time.Minute, []string{request.Descriptor.Fingerprint})
	require.NoError(t, err)
	require.False(t, found, "enqueue must not reopen exhausted work")
}
