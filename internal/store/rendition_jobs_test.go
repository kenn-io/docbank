package store

import (
	"bytes"
	"database/sql"
	"encoding/json/jsontext"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/document"
)

func TestRenditionJobsDeduplicateSharedBuildAndFenceLeaseTheft(t *testing.T) {
	s, versions := newRenditionCatalogFixture(t)
	profile := catalogProcessingProfile(t, false)
	embeddingProfile := catalogProcessingProfile(t, true)
	request := renditionJobTestRequest(versions[0], profile)
	grantRenditionJobConsent(t, s, request)

	first, firstWaiter, err := s.EnqueueRenditionJob(t.Context(), request)
	require.NoError(t, err)
	request = renditionJobTestRequest(versions[1], embeddingProfile)
	grantRenditionJobConsent(t, s, request)
	second, secondWaiter, err := s.EnqueueRenditionJob(t.Context(), request)
	require.NoError(t, err)
	assert.Equal(t, first.ID, second.ID)
	assert.NotEqual(t, firstWaiter.ID, secondWaiter.ID)
	assert.Equal(t, first.SourceSHA256, second.SourceSHA256)
	assert.Equal(t, 2, second.WaiterCount)

	now := time.Now().UTC().Add(time.Second)
	firstClaim, err := s.ClaimRenditionJob(t.Context(), first.ID, "worker-a", now, time.Minute)
	require.NoError(t, err)
	assert.Equal(t, int64(1), firstClaim.Epoch)

	_, err = s.ClaimRenditionJob(t.Context(), first.ID, "worker-a", now.Add(time.Second), time.Minute)
	require.ErrorIs(t, err, ErrRenditionJobLeaseHeld)
	_, err = s.ClaimRenditionJob(t.Context(), first.ID, "worker-b", now.Add(time.Second), time.Minute)
	require.ErrorIs(t, err, ErrRenditionJobLeaseHeld)

	secondClaim, err := s.ClaimRenditionJob(t.Context(), first.ID, "worker-b", now.Add(2*time.Minute), time.Minute)
	require.NoError(t, err)
	assert.Equal(t, int64(2), secondClaim.Epoch)
	require.ErrorIs(t,
		s.MarkRenditionJobRetry(t.Context(), firstClaim, RenditionFailureTransient,
			now.Add(3*time.Minute), now.Add(4*time.Minute)),
		ErrRenditionJobFenced,
	)
}

func TestRenditionJobsSplitEveryOutputAffectingExecutionIdentity(t *testing.T) {
	s, versions := newRenditionCatalogFixture(t)
	profile := catalogProcessingProfile(t, false)
	baseRequest := renditionJobTestRequest(versions[0], profile)
	grantRenditionJobConsent(t, s, baseRequest)
	base, _, err := s.EnqueueRenditionJob(t.Context(), baseRequest)
	require.NoError(t, err)

	mutations := map[string]func(*document.RenditionExecutionIdentityV1){
		"filename": func(identity *document.RenditionExecutionIdentityV1) {
			identity.Upload.Filename = "different.pdf"
		},
		"capability checksum": func(identity *document.RenditionExecutionIdentityV1) {
			identity.Upload.CapabilityRecordChecksum = testSHA256([]byte("execution-capability-change"))
			identity.Authorization.CapabilityRecordChecksum = identity.Upload.CapabilityRecordChecksum
		},
		"provider metadata checksum": func(identity *document.RenditionExecutionIdentityV1) {
			identity.Upload.ProviderMetadataChecksum = testSHA256([]byte("execution-metadata-change"))
			identity.Authorization.ProviderMetadataChecksum = identity.Upload.ProviderMetadataChecksum
		},
		"result limit": func(identity *document.RenditionExecutionIdentityV1) {
			identity.Authorization.MaxTotalResultBytes++
		},
		"evidence policy": func(identity *document.RenditionExecutionIdentityV1) {
			identity.EvidencePolicy.MaxDocumentChars--
		},
		"normalization policy": func(identity *document.RenditionExecutionIdentityV1) {
			identity.RenditionPolicy.Normalization.MaxDocumentChars--
		},
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			request := baseRequest
			request.ExecutionIdentity = baseRequest.ExecutionIdentity
			request.ExecutionIdentity.Authorization.AllowedArtifactRoles = append(
				[]document.EvidenceArtifactRole(nil),
				baseRequest.ExecutionIdentity.Authorization.AllowedArtifactRoles...)
			mutate(&request.ExecutionIdentity)
			job, _, err := s.EnqueueRenditionJob(t.Context(), request)
			require.NoError(t, err)
			assert.NotEqual(t, base.ID, job.ID)
		})
	}
}

func TestEnqueueRenditionJobReusesAndRootsExistingSharedBuild(t *testing.T) {
	s, versions := newRenditionCatalogFixture(t)
	profile := catalogProcessingProfile(t, false)
	request := renditionJobTestRequest(versions[0], profile)
	_, executionFingerprint, err := document.CanonicalRenditionExecutionIdentityV1(
		request.ExecutionIdentity)
	require.NoError(t, err)
	jobID := renditionSharedBuildID(
		s.VaultID(), catalogSourceHash, profile.RenditionRequestFingerprint,
		profile.EvidenceLexicalFingerprint, digestCatalogJSON(request.CapturedArtifactPolicy),
		executionFingerprint,
	)
	build := catalogRenditionBuild(s, profile)
	build.ID = jobID
	require.NoError(t, s.StageRenditionBuild(t.Context(), build))
	grantRenditionJobConsent(t, s, request)

	job, _, err := s.EnqueueRenditionJob(t.Context(), request)
	require.NoError(t, err)
	assert.Equal(t, jobID, job.ID)
	assert.Equal(t, RenditionPhaseBuildStaged, job.Phase)
	plan, err := s.DerivativeGCPlan(t.Context())
	require.NoError(t, err)
	assert.Empty(t, plan.Builds, "a queued build-reuse job must root its immutable input")
}

func TestEnqueueCompletedRenditionJobRestoresSupersededHeadWithoutProviderEgress(t *testing.T) {
	for _, test := range []struct {
		name      string
		principal string
	}{
		{name: "same principal", principal: "operator:synthetic"},
		{name: "new principal", principal: "operator:new-principal"},
	} {
		t.Run(test.name, func(t *testing.T) {
			s, versions := newRenditionCatalogFixture(t)
			profile := catalogProcessingProfile(t, false)
			request := renditionJobTestRequest(versions[0], profile)
			grantRenditionJobConsent(t, s, request)
			job, waiter, err := s.EnqueueRenditionJob(t.Context(), request)
			require.NoError(t, err)
			now := time.Now().UTC().Add(time.Second)
			claim, err := s.ClaimRenditionJob(
				t.Context(), job.ID, "worker:initial-publication", now, time.Minute)
			require.NoError(t, err)
			_, err = s.BeginRenditionProvider(t.Context(), claim, waiter.ID,
				now.Add(time.Second), renditionJobTestSnapshot(request))
			require.NoError(t, err)
			build := catalogRenditionBuild(s, profile)
			build.ID = job.ID
			require.NoError(t, s.StageRenditionJobBuild(
				t.Context(), claim, build, now.Add(2*time.Second)))
			_, err = s.StageRenditionJobGeneration(
				t.Context(), claim, testSHA256([]byte("initial-requeue-generation")),
				now.Add(3*time.Second))
			require.NoError(t, err)
			_, err = s.PublishRenditionJob(t.Context(), claim, now.Add(4*time.Second))
			require.NoError(t, err)
			initial, err := s.ActiveRendition(t.Context(), versions[0], profile.Fingerprint)
			require.NoError(t, err)

			replacement := cloneCatalogBuild(build)
			replacement.ID = testSHA256([]byte("superseding-same-version-build"))
			replacementPolicy := jsontext.Value(`{"roles":[{"max_count":2,"min_count":1,"role":"normalized_evidence"},{"max_count":1,"min_count":1,"role":"sanitized_markdown"}],"version":1}`)
			replacement.CapturedArtifactPolicy = replacementPolicy
			replacement.CapturedArtifactPolicyFingerprint = testSHA256(replacementPolicy)
			require.NoError(t, s.StageRenditionBuild(t.Context(), replacement))
			replacementGeneration, err := s.StageLexicalGeneration(
				t.Context(), testSHA256([]byte("superseding-same-version-generation")))
			require.NoError(t, err)
			replacementAttachment := RenditionAttachmentRecord{
				ID:      renditionScopedID("attachment", replacement.ID, versions[0], profile.Fingerprint),
				VaultID: s.VaultID(), ContentVersionID: versions[0], BuildID: replacement.ID,
				Profile: profile, AttachedAt: now.Add(5 * time.Second).Format(timestampLayout),
			}
			require.NoError(t, s.PublishRenditionAndLexicalHeads(
				t.Context(), replacementAttachment, RenditionHeadRecord{
					ContentVersionID: versions[0], ProcessingProfileFingerprint: profile.Fingerprint,
					AttachmentID: replacementAttachment.ID,
					PublishedAt:  now.Add(5 * time.Second).Format(timestampLayout),
				}, replacementGeneration.ID))

			request.Authorization.Principal = test.principal
			grantRenditionJobConsent(t, s, request)
			requeued, _, err := s.EnqueueRenditionJob(t.Context(), request)
			require.NoError(t, err)
			assert.Equal(t, RenditionJobQueued, requeued.State)
			assert.Equal(t, RenditionPhaseBuildStaged, requeued.Phase)
			republishClaim, err := s.ClaimRenditionJob(
				t.Context(), job.ID, "worker:head-reconciliation", now.Add(6*time.Second), time.Minute)
			require.NoError(t, err)
			_, err = s.StageRenditionJobGeneration(
				t.Context(), republishClaim,
				testSHA256([]byte("head-reconciliation-"+test.name)), now.Add(7*time.Second))
			require.NoError(t, err)
			_, err = s.PublishRenditionJob(t.Context(), republishClaim, now.Add(8*time.Second))
			require.NoError(t, err)
			active, err := s.ActiveRendition(t.Context(), versions[0], profile.Fingerprint)
			require.NoError(t, err)
			assert.Equal(t, job.ID, active.Build.ID)
			assert.Equal(t, initial.Attachment.AttachedAt, active.Attachment.AttachedAt,
				"head reconciliation must reuse the immutable attachment timestamp")
			var providerStarted bool
			require.NoError(t, s.db.QueryRow(
				`SELECT provider_started FROM rendition_jobs WHERE job_id=?`, job.ID,
			).Scan(&providerStarted))
			assert.False(t, providerStarted,
				"head reconciliation of an immutable completed build must not repeat provider egress")
		})
	}
}

func TestPurgeDerivativesCancelsQueuedRenditionJobAndSuppressesReenqueue(t *testing.T) {
	s, versions := newRenditionCatalogFixture(t)
	profile := catalogProcessingProfile(t, false)
	request := renditionJobTestRequest(versions[0], profile)
	grantRenditionJobConsent(t, s, request)
	job, _, err := s.EnqueueRenditionJob(t.Context(), request)
	require.NoError(t, err)

	_, err = s.PurgeDerivatives(t.Context(), PurgeRequest{
		ContentVersionIDs: []string{versions[0]},
	})
	require.NoError(t, err)
	_, err = s.RenditionJobByID(t.Context(), job.ID)
	require.ErrorIs(t, err, ErrNotFound)
	_, _, err = s.EnqueueRenditionJob(t.Context(), request)
	require.ErrorIs(t, err, ErrRenditionJobStaleAuthority)
}

func TestEnqueueRenditionJobRequiresConsentForEveryRetainedArtifactClass(t *testing.T) {
	s, versions := newRenditionCatalogFixture(t)
	request := renditionJobTestRequest(versions[0], catalogProcessingProfile(t, false))
	request.Authorization.RetainedArtifactClasses = []string{"normalized_evidence"}

	_, _, err := s.EnqueueRenditionJob(t.Context(), request)
	require.ErrorContains(t, err, "retained artifact classes")
}

func TestEnqueueRenditionJobRequiresOriginalSourceConsent(t *testing.T) {
	s, versions := newRenditionCatalogFixture(t)
	request := renditionJobTestRequest(versions[0], catalogProcessingProfile(t, false))
	request.Authorization.InputClasses = []string{"derived_upload"}

	_, _, err := s.EnqueueRenditionJob(t.Context(), request)
	require.ErrorContains(t, err, "input classes")
}

func TestEnqueueRenditionJobRejectsNewWaiterForTerminalBuild(t *testing.T) {
	for _, test := range []struct {
		name      string
		finish    func(*Store, RenditionJobClaim, time.Time) error
		wantErr   error
		wantState RenditionJobState
	}{
		{
			name: "operator required",
			finish: func(s *Store, claim RenditionJobClaim, at time.Time) error {
				return s.MarkRenditionJobOperatorRequired(t.Context(), claim, at)
			},
			wantErr: ErrRenditionJobOperatorRequired, wantState: RenditionJobOperatorRequired,
		},
		{
			name: "failed",
			finish: func(s *Store, claim RenditionJobClaim, at time.Time) error {
				return s.MarkRenditionJobFailed(t.Context(), claim, RenditionFailureTerminal, at)
			},
			wantErr: ErrRenditionJobTerminal, wantState: RenditionJobFailed,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			s, versions := newRenditionCatalogFixture(t)
			profile := catalogProcessingProfile(t, false)
			request := renditionJobTestRequest(versions[0], profile)
			grantRenditionJobConsent(t, s, request)
			job, _, err := s.EnqueueRenditionJob(t.Context(), request)
			require.NoError(t, err)
			now := time.Now().UTC().Add(time.Second)
			claim, err := s.ClaimRenditionJob(t.Context(), job.ID, "worker-terminal", now, time.Minute)
			require.NoError(t, err)
			require.NoError(t, test.finish(s, claim, now.Add(time.Second)))

			request.Authorization.Principal = "operator:late-waiter"
			grantRenditionJobConsent(t, s, request)
			_, _, err = s.EnqueueRenditionJob(t.Context(), request)
			require.ErrorIs(t, err, test.wantErr)

			current, err := s.RenditionJobByID(t.Context(), job.ID)
			require.NoError(t, err)
			assert.Equal(t, test.wantState, current.State)
			assert.Equal(t, 1, current.WaiterCount,
				"a rejected join must not leave an unreachable waiting waiter")
		})
	}
}

func TestEnqueueRenditionJobRejectsUnauthorizedWaiter(t *testing.T) {
	s, versions := newRenditionCatalogFixture(t)
	request := renditionJobTestRequest(versions[0], catalogProcessingProfile(t, false))

	_, _, err := s.EnqueueRenditionJob(t.Context(), request)
	require.ErrorIs(t, err, ErrProcessingConsentRequired)
}

func TestRenditionJobWorkChoosesAnAuthorizedWaiter(t *testing.T) {
	s, versions := newRenditionCatalogFixture(t)
	profile := catalogProcessingProfile(t, false)
	firstRequest := renditionJobTestRequest(versions[0], profile)
	firstRequest.Authorization.Principal = "operator:first-candidate"
	secondRequest := renditionJobTestRequest(versions[1], profile)
	secondRequest.Authorization.Principal = "operator:second-candidate"
	grantRenditionJobConsent(t, s, firstRequest)
	grantRenditionJobConsent(t, s, secondRequest)
	job, firstWaiter, err := s.EnqueueRenditionJob(t.Context(), firstRequest)
	require.NoError(t, err)
	_, secondWaiter, err := s.EnqueueRenditionJob(t.Context(), secondRequest)
	require.NoError(t, err)
	revoked, expected := firstRequest, secondWaiter
	if secondWaiter.ID < firstWaiter.ID {
		revoked, expected = secondRequest, firstWaiter
	}
	_, err = s.RevokeConsent(t.Context(), ProcessingConsentRevocationRequest{
		Principal: revoked.Authorization.Principal,
		Scope:     revoked.Authorization.Scope,
	})
	require.NoError(t, err)
	now := time.Now().UTC().Add(time.Second)
	claim, err := s.ClaimRenditionJob(t.Context(), job.ID, "worker-a", now, time.Minute)
	require.NoError(t, err)

	work, err := s.RenditionJobWorkByClaim(t.Context(), claim, now.Add(time.Second))
	require.NoError(t, err)
	assert.Equal(t, expected.ID, work.Waiter.ID)
}

func TestRenditionJobsNeverResubmitAmbiguousProviderWorkWithoutHandle(t *testing.T) {
	for _, checkpoint := range []bool{false, true} {
		t.Run(map[bool]string{false: "missing handle", true: "durable handle"}[checkpoint], func(t *testing.T) {
			s, versions := newRenditionCatalogFixture(t)
			profile := catalogProcessingProfile(t, false)
			request := renditionJobTestRequest(versions[0], profile)
			grantRenditionJobConsent(t, s, request)
			job, waiter, err := s.EnqueueRenditionJob(t.Context(), request)
			require.NoError(t, err)
			now := time.Now().UTC().Add(time.Second)
			claim, err := s.ClaimRenditionJob(t.Context(), job.ID, "worker-a", now, time.Minute)
			require.NoError(t, err)
			_, err = s.BeginRenditionProvider(t.Context(), claim, waiter.ID,
				now.Add(time.Second), renditionJobTestSnapshot(request))
			require.NoError(t, err)
			if checkpoint {
				require.NoError(t, s.CheckpointRenditionProvider(
					t.Context(), claim, "provider-issued-handle", now.Add(2*time.Second)))
			}

			reclaimed, err := s.ClaimRenditionJob(
				t.Context(), job.ID, "worker-b", now.Add(2*time.Minute), time.Minute)
			if checkpoint {
				require.NoError(t, err)
				assert.Equal(t, "provider-issued-handle", reclaimed.ResumeHandle)
				assert.Equal(t, int64(2), reclaimed.Epoch)
				return
			}
			require.ErrorIs(t, err, ErrRenditionJobOperatorRequired)
			current, readErr := s.RenditionJobByID(t.Context(), job.ID)
			require.NoError(t, readErr)
			assert.Equal(t, RenditionJobOperatorRequired, current.State)
			assert.Equal(t, RenditionFailureAmbiguous, current.FailureCode)
		})
	}
}

func TestRenditionJobMetadataRoundTripPreservesAmbiguousFenceAndRequiresFreshConsent(t *testing.T) {
	s, versions := newRenditionCatalogFixture(t)
	profile := catalogProcessingProfile(t, false)
	request := renditionJobTestRequest(versions[0], profile)
	grantRenditionJobConsent(t, s, request)
	job, waiter, err := s.EnqueueRenditionJob(t.Context(), request)
	require.NoError(t, err)
	now := time.Now().UTC().Add(time.Second)
	claim, err := s.ClaimRenditionJob(
		t.Context(), job.ID, "worker:metadata-ambiguous", now, time.Minute)
	require.NoError(t, err)
	_, err = s.BeginRenditionProvider(t.Context(), claim, waiter.ID,
		now.Add(time.Second), renditionJobTestSnapshot(request))
	require.NoError(t, err)
	require.NoError(t, s.MarkRenditionJobOperatorRequired(
		t.Context(), claim, now.Add(2*time.Second)))

	var first, second bytes.Buffer
	require.NoError(t, s.ExportMetadata(t.Context(), &first))
	require.NoError(t, s.ExportMetadata(t.Context(), &second))
	assert.Equal(t, first.Bytes(), second.Bytes(), "job authority export must be deterministic")
	restored := newTestStore(t)
	require.NoError(t, restored.ImportMetadataForRestore(
		t.Context(), bytes.NewReader(first.Bytes())))
	restoredJob, err := restored.RenditionJobByID(t.Context(), job.ID)
	require.NoError(t, err)
	assert.Equal(t, RenditionJobOperatorRequired, restoredJob.State)
	assert.Equal(t, RenditionFailureAmbiguous, restoredJob.FailureCode)

	_, _, err = restored.EnqueueRenditionJob(t.Context(), request)
	require.ErrorIs(t, err, ErrProcessingConsentRequired,
		"restored consent belongs to the old processing incarnation")
	grantRenditionJobConsent(t, restored, request)
	_, _, err = restored.EnqueueRenditionJob(t.Context(), request)
	require.ErrorIs(t, err, ErrRenditionJobOperatorRequired,
		"fresh consent must not join or restart an ambiguous tombstone")
}

func TestRenditionJobMetadataRoundTripPreservesSealedDurableResumeAuthority(t *testing.T) {
	s, versions := newRenditionCatalogFixture(t)
	profile := catalogProcessingProfile(t, false)
	request := renditionJobTestRequest(versions[0], profile)
	grantRenditionJobConsent(t, s, request)
	job, waiter, err := s.EnqueueRenditionJob(t.Context(), request)
	require.NoError(t, err)
	now := time.Now().UTC().Add(time.Second)
	claim, err := s.ClaimRenditionJob(
		t.Context(), job.ID, "worker:metadata-resume", now, time.Minute)
	require.NoError(t, err)
	snapshot := renditionJobTestSnapshot(request)
	_, err = s.BeginRenditionProvider(
		t.Context(), claim, waiter.ID, now.Add(time.Second), snapshot)
	require.NoError(t, err)
	require.NoError(t, s.CheckpointRenditionProvider(
		t.Context(), claim, "restored-provider-handle", now.Add(2*time.Second)))
	require.NoError(t, s.MarkRenditionJobRetry(
		t.Context(), claim, RenditionFailureTransient,
		now.Add(3*time.Second), now.Add(4*time.Second)))

	var exported bytes.Buffer
	require.NoError(t, s.ExportMetadata(t.Context(), &exported))
	restored := newTestStore(t)
	require.NoError(t, restored.ImportMetadataForRestore(
		t.Context(), bytes.NewReader(exported.Bytes())))
	_, _, err = restored.EnqueueRenditionJob(t.Context(), request)
	require.ErrorIs(t, err, ErrProcessingConsentRequired)
	restoredClaim, err := restored.ClaimRenditionJob(
		t.Context(), job.ID, "worker:restored-without-consent", now.Add(5*time.Second), time.Minute)
	require.NoError(t, err)
	_, err = restored.RenditionJobWorkByClaim(
		t.Context(), restoredClaim, now.Add(5*time.Second))
	require.ErrorIs(t, err, ErrProcessingConsentRequired)
	require.NoError(t, restored.MarkRenditionJobFailed(
		t.Context(), restoredClaim, RenditionFailureConsent, now.Add(6*time.Second)))

	grantRenditionJobConsent(t, restored, request)
	restoredJob, _, err := restored.EnqueueRenditionJob(t.Context(), request)
	require.NoError(t, err)
	assert.Equal(t, RenditionJobQueued, restoredJob.State)
	restoredClaim, err = restored.ClaimRenditionJob(
		t.Context(), job.ID, "worker:restored-resume", now.Add(7*time.Second), time.Minute)
	require.NoError(t, err)
	assert.Equal(t, "restored-provider-handle", restoredClaim.ResumeHandle)
	work, err := restored.RenditionJobWorkByClaim(
		t.Context(), restoredClaim, now.Add(7*time.Second))
	require.NoError(t, err)
	require.NotNil(t, work.ExecutionSnapshot)
	assert.Equal(t, snapshot.Authorization, work.ExecutionSnapshot.Authorization)
	_, err = restored.BeginRenditionProvider(
		t.Context(), restoredClaim, work.Waiter.ID, now.Add(8*time.Second))
	require.NoError(t, err,
		"fresh consent may authorize only the known durable handle; no new snapshot is accepted")
}

func TestRenditionJobMetadataRoundTripPreservesActiveStagedBuildRoot(t *testing.T) {
	s, versions := newRenditionCatalogFixture(t)
	profile := catalogProcessingProfile(t, false)
	request := renditionJobTestRequest(versions[0], profile)
	grantRenditionJobConsent(t, s, request)
	job, waiter, err := s.EnqueueRenditionJob(t.Context(), request)
	require.NoError(t, err)
	now := time.Now().UTC().Add(time.Second)
	claim, err := s.ClaimRenditionJob(
		t.Context(), job.ID, "worker:metadata-staged-root", now, time.Minute)
	require.NoError(t, err)
	_, err = s.BeginRenditionProvider(t.Context(), claim, waiter.ID,
		now.Add(time.Second), renditionJobTestSnapshot(request))
	require.NoError(t, err)
	build := catalogRenditionBuild(s, profile)
	build.ID = job.ID
	require.NoError(t, s.StageRenditionJobBuild(
		t.Context(), claim, build, now.Add(2*time.Second)))

	var exported bytes.Buffer
	require.NoError(t, s.ExportMetadata(t.Context(), &exported))
	restored := newTestStore(t)
	require.NoError(t, restored.ImportMetadataForRestore(
		t.Context(), bytes.NewReader(exported.Bytes())))
	restoredJob, err := restored.RenditionJobByID(t.Context(), job.ID)
	require.NoError(t, err)
	assert.Equal(t, RenditionJobRunning, restoredJob.State)
	assert.Equal(t, RenditionPhaseBuildStaged, restoredJob.Phase)
	plan, err := restored.DerivativeGCPlan(t.Context())
	require.NoError(t, err)
	for _, candidate := range plan.Builds {
		assert.NotEqual(t, job.ID, candidate.BuildID,
			"restored epoch-fenced job roots keep staged immutable work live")
	}

	restoredClaim, err := restored.ClaimRenditionJob(
		t.Context(), job.ID, "worker:restored-staged-no-consent", now.Add(2*time.Minute), time.Minute)
	require.NoError(t, err)
	_, err = restored.RenditionJobWorkByClaim(
		t.Context(), restoredClaim, now.Add(2*time.Minute))
	require.ErrorIs(t, err, ErrProcessingConsentRequired,
		"restored staged work must reacquire consent before local publication")
	require.NoError(t, restored.MarkRenditionJobFailed(
		t.Context(), restoredClaim, RenditionFailureConsent, now.Add(2*time.Minute+time.Second)))
	grantRenditionJobConsent(t, restored, request)
	restoredJob, _, err = restored.EnqueueRenditionJob(t.Context(), request)
	require.NoError(t, err)
	assert.Equal(t, RenditionJobQueued, restoredJob.State)
	assert.Equal(t, RenditionPhaseBuildStaged, restoredJob.Phase)
	rescuedClaim, err := restored.ClaimRenditionJob(
		t.Context(), job.ID, "worker:restored-staged-consented",
		now.Add(2*time.Minute+2*time.Second), time.Minute)
	require.NoError(t, err)
	_, err = restored.RenditionJobWorkByClaim(
		t.Context(), rescuedClaim, now.Add(2*time.Minute+2*time.Second))
	require.NoError(t, err)
	_, err = restored.StageRenditionJobGeneration(
		t.Context(), rescuedClaim, testSHA256([]byte("restored-staged-generation")),
		now.Add(2*time.Minute+3*time.Second))
	require.NoError(t, err)
	publication, err := restored.PublishRenditionJob(
		t.Context(), rescuedClaim, now.Add(2*time.Minute+4*time.Second))
	require.NoError(t, err)
	assert.Equal(t, 1, publication.PublishedWaiterCount)
}

func TestRenditionJobStagedBuildRootSurvivesLeaseExpiryUntilTerminalState(t *testing.T) {
	s, versions := newRenditionCatalogFixture(t)
	profile := catalogProcessingProfile(t, false)
	request := renditionJobTestRequest(versions[0], profile)
	grantRenditionJobConsent(t, s, request)
	job, waiter, err := s.EnqueueRenditionJob(t.Context(), request)
	require.NoError(t, err)
	started := time.Now().UTC()
	claim, err := s.ClaimRenditionJob(
		t.Context(), job.ID, "worker:crash-before-reclaim", started, time.Second)
	require.NoError(t, err)
	_, err = s.BeginRenditionProvider(t.Context(), claim, waiter.ID,
		started.Add(100*time.Millisecond), renditionJobTestSnapshot(request))
	require.NoError(t, err)
	build := catalogRenditionBuild(s, profile)
	build.ID = job.ID
	require.NoError(t, s.StageRenditionJobBuild(
		t.Context(), claim, build, started.Add(200*time.Millisecond)))
	time.Sleep(1100 * time.Millisecond)

	plan, err := s.DerivativeGCPlan(t.Context())
	require.NoError(t, err)
	assert.Empty(t, plan.Builds, "durable staged work must survive an expired worker lease")
	require.NoError(t, s.MarkRenditionJobFailed(
		t.Context(), claim, RenditionFailureTerminal, started.Add(300*time.Millisecond)))
	plan, err = s.DerivativeGCPlan(t.Context())
	require.NoError(t, err)
	require.Len(t, plan.Builds, 1)
	assert.Equal(t, job.ID, plan.Builds[0].BuildID)
}

func TestRenditionJobStagingIsIdempotentWithinOneClaim(t *testing.T) {
	s, versions := newRenditionCatalogFixture(t)
	profile := catalogProcessingProfile(t, false)
	request := renditionJobTestRequest(versions[0], profile)
	grantRenditionJobConsent(t, s, request)
	job, waiter, err := s.EnqueueRenditionJob(t.Context(), request)
	require.NoError(t, err)
	now := time.Now().UTC().Add(time.Second)
	claim, err := s.ClaimRenditionJob(t.Context(), job.ID, "worker:idempotent-stage", now, time.Minute)
	require.NoError(t, err)
	_, err = s.BeginRenditionProvider(t.Context(), claim, waiter.ID,
		now.Add(time.Second), renditionJobTestSnapshot(request))
	require.NoError(t, err)
	build := catalogRenditionBuild(s, profile)
	build.ID = job.ID
	require.NoError(t, s.StageRenditionJobBuild(
		t.Context(), claim, build, now.Add(2*time.Second)))
	require.NoError(t, s.StageRenditionJobBuild(
		t.Context(), claim, build, now.Add(3*time.Second)))
	generationID := testSHA256([]byte("idempotent-job-generation"))
	_, err = s.StageRenditionJobGeneration(
		t.Context(), claim, generationID, now.Add(4*time.Second))
	require.NoError(t, err)
	_, err = s.StageRenditionJobGeneration(
		t.Context(), claim, generationID, now.Add(5*time.Second))
	require.NoError(t, err)
}

func TestRenditionJobProviderFenceRechecksConsentAndSource(t *testing.T) {
	t.Run("consent revocation", func(t *testing.T) {
		s, versions := newRenditionCatalogFixture(t)
		profile := catalogProcessingProfile(t, false)
		request := renditionJobTestRequest(versions[0], profile)
		grantRenditionJobConsent(t, s, request)
		job, waiter, err := s.EnqueueRenditionJob(t.Context(), request)
		require.NoError(t, err)
		now := time.Now().UTC().Add(time.Second)
		claim, err := s.ClaimRenditionJob(t.Context(), job.ID, "worker-a", now, time.Minute)
		require.NoError(t, err)

		_, err = s.RevokeConsent(t.Context(), ProcessingConsentRevocationRequest{
			Principal: request.Authorization.Principal, Scope: request.Authorization.Scope,
		})
		require.NoError(t, err)
		_, err = s.BeginRenditionProvider(t.Context(), claim, waiter.ID,
			now.Add(time.Second), renditionJobTestSnapshot(request))
		require.ErrorIs(t, err, ErrProcessingConsentRevoked)
	})

	t.Run("source drift", func(t *testing.T) {
		s, versions := newRenditionCatalogFixture(t)
		profile := catalogProcessingProfile(t, false)
		request := renditionJobTestRequest(versions[0], profile)
		grantRenditionJobConsent(t, s, request)
		job, waiter, err := s.EnqueueRenditionJob(t.Context(), request)
		require.NoError(t, err)
		now := time.Now().UTC().Add(time.Second)
		claim, err := s.ClaimRenditionJob(t.Context(), job.ID, "worker-a", now, time.Minute)
		require.NoError(t, err)
		driftedHash := testSHA256([]byte("synthetic drifted authority"))
		require.NoError(t, s.withStorageTx(t.Context(), func(tx *sql.Tx) error {
			if err := s.EnsureBlobTx(tx, driftedHash, 20); err != nil {
				return err
			}
			_, err := tx.ExecContext(t.Context(),
				`UPDATE content_versions SET blob_hash=? WHERE version_id=?`, driftedHash, versions[0])
			return err
		}))
		_, err = s.BeginRenditionProvider(t.Context(), claim, waiter.ID,
			now.Add(time.Second), renditionJobTestSnapshot(request))
		require.ErrorContains(t, err, "source or profile authority drifted")
	})
}

func renditionJobTestRequest(
	versionID string, profile ProcessingProfileRecord,
) RenditionJobRequest {
	return RenditionJobRequest{
		ContentVersionID:       versionID,
		Profile:                profile,
		CapturedArtifactPolicy: []byte(catalogCapturedPolicy),
		ExecutionIdentity:      renditionJobTestExecutionIdentity(profile),
		Authorization: ProviderOperationAuthorizationRequest{
			Principal: "operator:synthetic", Scope: "document-processing",
			ProfileFingerprint:      profile.Fingerprint,
			DisclosureFingerprint:   profile.RenditionDisclosureFingerprint,
			InputClasses:            []string{"original_file"},
			RetainedArtifactClasses: []string{"normalized_evidence", "sanitized_markdown"},
		},
	}
}

func renditionJobTestExecutionIdentity(
	profile ProcessingProfileRecord,
) document.RenditionExecutionIdentityV1 {
	metadata := document.AuthorizedUploadMetadata{
		Filename: "source.pdf", MediaFamily: "pdf", MediaType: "application/pdf",
		ByteLength: 20, SHA256: catalogSourceHash,
		CapabilityRecordChecksum: testSHA256([]byte("execution-capability")),
		ProviderMetadataChecksum: testSHA256([]byte("execution-provider-metadata")),
		InputKind:                document.RenditionInputOriginalFile,
	}
	authorization := document.RenditionAuthorization{
		ProviderID: "synthetic-rendition", DescriptorFingerprint: fakeHash("a3"),
		PolicyFingerprint:           testSHA256([]byte("execution-policy")),
		RenditionRequestFingerprint: profile.RenditionRequestFingerprint,
		SourceSHA256:                catalogSourceHash, SourceBytes: 20,
		CapabilityRecordChecksum: metadata.CapabilityRecordChecksum,
		ProviderMetadataChecksum: metadata.ProviderMetadataChecksum,
		MediaFamily:              metadata.MediaFamily, MediaType: metadata.MediaType,
		InputKind:            metadata.InputKind,
		AllowedArtifactRoles: []document.EvidenceArtifactRole{document.EvidenceArtifactStructured},
		MaxArtifacts:         1, MaxArtifactBytes: 1 << 20, MaxTotalResultBytes: 1 << 20,
		AuthorizedAt: "2026-08-25T00:00:00.000000000Z",
		ExpiresAt:    "2026-08-25T00:10:00.000000000Z",
	}
	evidence, err := document.NewEvidencePolicy(100_000)
	if err != nil {
		panic(err)
	}
	normalization, err := document.NewNormalizePolicy(100_000)
	if err != nil {
		panic(err)
	}
	rendition, err := document.NewRenditionPolicy(normalization, 100)
	if err != nil {
		panic(err)
	}
	identity, err := document.NewRenditionExecutionIdentityV1(
		metadata, authorization, evidence, rendition)
	if err != nil {
		panic(err)
	}
	return identity
}

func renditionJobTestSnapshot(
	request RenditionJobRequest,
) document.RenditionExecutionSnapshotV1 {
	identity := request.ExecutionIdentity.Authorization
	authorization := document.RenditionAuthorization{
		ProviderID: identity.ProviderID, DescriptorFingerprint: identity.DescriptorFingerprint,
		PolicyFingerprint:           identity.PolicyFingerprint,
		RenditionRequestFingerprint: identity.RenditionRequestFingerprint,
		SourceSHA256:                identity.SourceSHA256, SourceBytes: identity.SourceBytes,
		CapabilityRecordChecksum: identity.CapabilityRecordChecksum,
		ProviderMetadataChecksum: identity.ProviderMetadataChecksum,
		MediaFamily:              identity.MediaFamily, MediaType: identity.MediaType,
		InputKind:                identity.InputKind,
		AllowedArtifactRoles:     append([]document.EvidenceArtifactRole(nil), identity.AllowedArtifactRoles...),
		MaxProviderMarkdownBytes: identity.MaxProviderMarkdownBytes,
		MaxArtifactBytes:         identity.MaxArtifactBytes, MaxArtifacts: identity.MaxArtifacts,
		MaxTotalResultBytes: identity.MaxTotalResultBytes,
		AuthorizedAt:        "2026-08-25T00:00:00.000000000Z",
		ExpiresAt:           "2026-08-25T00:10:00.000000000Z",
	}
	snapshot, err := document.NewRenditionExecutionSnapshotV1(
		request.ExecutionIdentity, authorization)
	if err != nil {
		panic(err)
	}
	return snapshot
}

func grantRenditionJobConsent(t *testing.T, s *Store, request RenditionJobRequest) {
	t.Helper()
	_, err := s.GrantConsent(t.Context(), grantRequestForAuthorization(request.Authorization, nil))
	require.NoError(t, err)
}

func TestRenditionJobFailureCodesRemainAggregateAndBounded(t *testing.T) {
	for _, code := range []RenditionFailureCode{
		RenditionFailureTransient, RenditionFailureTerminal, RenditionFailureAmbiguous,
		RenditionFailureConsent, RenditionFailureStaleAuthority,
	} {
		assert.LessOrEqual(t, len(code), 32)
		assert.NotContains(t, string(code), "provider")
	}
}

func TestPublishRenditionJobAtomicallyActivatesAuthorizedWaiters(t *testing.T) {
	s, versions := newRenditionCatalogFixture(t)
	baseProfile := catalogProcessingProfile(t, false)
	embeddingProfile := catalogProcessingProfile(t, true)
	firstRequest := renditionJobTestRequest(versions[0], baseProfile)
	secondRequest := renditionJobTestRequest(versions[1], embeddingProfile)
	secondRequest.Authorization.Principal = "operator:second"
	grantRenditionJobConsent(t, s, firstRequest)
	grantRenditionJobConsent(t, s, secondRequest)
	job, firstWaiter, err := s.EnqueueRenditionJob(t.Context(), firstRequest)
	require.NoError(t, err)
	_, secondWaiter, err := s.EnqueueRenditionJob(t.Context(), secondRequest)
	require.NoError(t, err)
	now := time.Now().UTC().Add(time.Second)
	claim, err := s.ClaimRenditionJob(t.Context(), job.ID, "worker-a", now, time.Minute)
	require.NoError(t, err)
	_, err = s.BeginRenditionProvider(t.Context(), claim, firstWaiter.ID,
		now.Add(time.Second), renditionJobTestSnapshot(firstRequest))
	require.NoError(t, err)

	build := catalogRenditionBuild(s, baseProfile)
	build.ID = job.ID
	require.NoError(t, s.StageRenditionJobBuild(
		t.Context(), claim, build, now.Add(2*time.Second)))
	generationID := testSHA256([]byte("rendition-job-generation"))
	_, err = s.StageRenditionJobGeneration(
		t.Context(), claim, generationID, now.Add(3*time.Second))
	require.NoError(t, err)

	publication, err := s.PublishRenditionJob(t.Context(), claim, now.Add(4*time.Second))
	require.NoError(t, err)
	assert.Equal(t, 2, publication.PublishedWaiterCount)
	assert.Zero(t, publication.RejectedWaiterCount)
	assert.Equal(t, generationID, publication.LexicalGenerationID)
	first, err := s.ActiveRendition(t.Context(), versions[0], baseProfile.Fingerprint)
	require.NoError(t, err)
	second, err := s.ActiveRendition(t.Context(), versions[1], embeddingProfile.Fingerprint)
	require.NoError(t, err)
	assert.Equal(t, job.ID, first.Build.ID)
	assert.Equal(t, job.ID, second.Build.ID)
	assert.NotEqual(t, first.Attachment.ID, second.Attachment.ID)
	assert.Equal(t, firstWaiter.AttachmentID, first.Attachment.ID)
	assert.Equal(t, secondWaiter.AttachmentID, second.Attachment.ID)
}

func TestPublishRenditionJobFencesLeaseTheft(t *testing.T) {
	s, versions := newRenditionCatalogFixture(t)
	profile := catalogProcessingProfile(t, false)
	request := renditionJobTestRequest(versions[0], profile)
	grantRenditionJobConsent(t, s, request)
	job, waiter, err := s.EnqueueRenditionJob(t.Context(), request)
	require.NoError(t, err)
	now := time.Now().UTC().Add(time.Second)
	staleClaim, err := s.ClaimRenditionJob(t.Context(), job.ID, "worker-a", now, 5*time.Second)
	require.NoError(t, err)
	_, err = s.BeginRenditionProvider(t.Context(), staleClaim, waiter.ID,
		now.Add(time.Second), renditionJobTestSnapshot(request))
	require.NoError(t, err)
	build := catalogRenditionBuild(s, profile)
	build.ID = job.ID
	require.NoError(t, s.StageRenditionJobBuild(
		t.Context(), staleClaim, build, now.Add(2*time.Second)))
	_, err = s.StageRenditionJobGeneration(
		t.Context(), staleClaim, testSHA256([]byte("lease-theft-generation")),
		now.Add(3*time.Second))
	require.NoError(t, err)

	winningClaim, err := s.ClaimRenditionJob(
		t.Context(), job.ID, "worker-b", now.Add(6*time.Second), time.Minute)
	require.NoError(t, err)
	_, err = s.PublishRenditionJob(t.Context(), staleClaim, now.Add(7*time.Second))
	require.ErrorIs(t, err, ErrRenditionJobFenced)
	_, err = s.ActiveRendition(t.Context(), versions[0], profile.Fingerprint)
	require.ErrorIs(t, err, ErrNotFound)

	_, err = s.PublishRenditionJob(t.Context(), winningClaim, now.Add(7*time.Second))
	require.NoError(t, err)
	_, err = s.ActiveRendition(t.Context(), versions[0], profile.Fingerprint)
	require.NoError(t, err)
}

func TestRenditionJobRecoversFromStaleLexicalPublicationWithoutRepeatingEgress(t *testing.T) {
	s, versions := newRenditionCatalogFixture(t)
	profile := catalogProcessingProfile(t, false)
	firstRequest := renditionJobTestRequest(versions[0], profile)
	secondRequest := renditionJobTestRequest(versions[1], profile)
	secondRequest.Authorization.Principal = "operator:concurrent-publication"
	secondPolicy := []byte(`{"roles":[{"max_count":2,"min_count":1,"role":"normalized_evidence"},{"max_count":1,"min_count":1,"role":"sanitized_markdown"}],"version":1}`)
	secondRequest.CapturedArtifactPolicy = secondPolicy
	grantRenditionJobConsent(t, s, firstRequest)
	grantRenditionJobConsent(t, s, secondRequest)
	firstJob, firstWaiter, err := s.EnqueueRenditionJob(t.Context(), firstRequest)
	require.NoError(t, err)
	secondJob, secondWaiter, err := s.EnqueueRenditionJob(t.Context(), secondRequest)
	require.NoError(t, err)
	require.NotEqual(t, firstJob.ID, secondJob.ID)
	now := time.Now().UTC().Add(time.Second)
	firstClaim, err := s.ClaimRenditionJob(
		t.Context(), firstJob.ID, "worker:first-generation", now, time.Minute)
	require.NoError(t, err)
	secondClaim, err := s.ClaimRenditionJob(
		t.Context(), secondJob.ID, "worker:second-generation", now, time.Minute)
	require.NoError(t, err)
	_, err = s.BeginRenditionProvider(
		t.Context(), firstClaim, firstWaiter.ID, now.Add(time.Second),
		renditionJobTestSnapshot(firstRequest))
	require.NoError(t, err)
	_, err = s.BeginRenditionProvider(
		t.Context(), secondClaim, secondWaiter.ID, now.Add(time.Second),
		renditionJobTestSnapshot(secondRequest))
	require.NoError(t, err)
	firstBuild := catalogRenditionBuild(s, profile)
	firstBuild.ID = firstJob.ID
	require.NoError(t, s.StageRenditionJobBuild(
		t.Context(), firstClaim, firstBuild, now.Add(2*time.Second)))
	firstGenerationID := testSHA256([]byte("concurrent-first-generation"))
	_, err = s.StageRenditionJobGeneration(
		t.Context(), firstClaim, firstGenerationID, now.Add(3*time.Second))
	require.NoError(t, err)

	secondBuild := cloneCatalogBuild(firstBuild)
	secondBuild.ID = secondJob.ID
	secondBuild.CapturedArtifactPolicy = secondPolicy
	secondBuild.CapturedArtifactPolicyFingerprint = testSHA256(secondPolicy)
	require.NoError(t, s.StageRenditionJobBuild(
		t.Context(), secondClaim, secondBuild, now.Add(2*time.Second)))
	secondGenerationID := testSHA256([]byte("concurrent-second-generation"))
	_, err = s.StageRenditionJobGeneration(
		t.Context(), secondClaim, secondGenerationID, now.Add(3*time.Second))
	require.NoError(t, err)
	_, err = s.PublishRenditionJob(t.Context(), secondClaim, now.Add(4*time.Second))
	require.NoError(t, err)

	_, err = s.PublishRenditionJob(t.Context(), firstClaim, now.Add(5*time.Second))
	require.ErrorContains(t, err, "omits current rendition head build")
	require.ErrorIs(t, err, ErrLexicalGenerationStale)
	require.NoError(t, s.MarkRenditionJobRetry(
		t.Context(), firstClaim, RenditionFailureTransient,
		now.Add(6*time.Second), now.Add(7*time.Second)))
	var providerStarted bool
	require.NoError(t, s.db.QueryRow(
		`SELECT provider_started FROM rendition_jobs WHERE job_id=?`, firstJob.ID,
	).Scan(&providerStarted))
	assert.True(t, providerStarted, "lexical-only retry must retain original egress authority")

	recoveredClaim, err := s.ClaimRenditionJob(
		t.Context(), firstJob.ID, "worker:fresh-generation", now.Add(7*time.Second), time.Minute)
	require.NoError(t, err)
	freshGenerationID := testSHA256([]byte("concurrent-fresh-generation"))
	_, err = s.StageRenditionJobGeneration(
		t.Context(), recoveredClaim, freshGenerationID, now.Add(8*time.Second))
	require.NoError(t, err)
	_, err = s.PublishRenditionJob(t.Context(), recoveredClaim, now.Add(9*time.Second))
	require.NoError(t, err)
	active, err := s.ActiveRendition(t.Context(), versions[0], profile.Fingerprint)
	require.NoError(t, err)
	assert.Equal(t, firstJob.ID, active.Build.ID)
}

func TestPublishRenditionJobRejectsRevokedEgressAndPreservesPreviousHead(t *testing.T) {
	s, versions := newRenditionCatalogFixture(t)
	profile := catalogProcessingProfile(t, false)
	oldBuild := catalogRenditionBuild(s, profile)
	oldAttachment := RenditionAttachmentRecord{
		ID: catalogAttachmentFirst, VaultID: s.VaultID(), ContentVersionID: versions[0],
		BuildID: oldBuild.ID, Profile: profile, AttachedAt: nowRFC3339(),
	}
	require.NoError(t, s.StageRenditionBuild(t.Context(), oldBuild))
	oldGeneration, err := s.StageLexicalGeneration(t.Context(), testSHA256([]byte("old-generation")))
	require.NoError(t, err)
	require.NoError(t, s.PublishRenditionAndLexicalHeads(t.Context(), oldAttachment,
		RenditionHeadRecord{ContentVersionID: versions[0],
			ProcessingProfileFingerprint: profile.Fingerprint,
			AttachmentID:                 oldAttachment.ID, PublishedAt: nowRFC3339()}, oldGeneration.ID))

	policy := []byte(`{"roles":[{"max_count":2,"min_count":1,"role":"normalized_evidence"},{"max_count":1,"min_count":1,"role":"sanitized_markdown"}],"version":1}`)
	request := renditionJobTestRequest(versions[0], profile)
	request.CapturedArtifactPolicy = policy
	grantRenditionJobConsent(t, s, request)
	job, waiter, err := s.EnqueueRenditionJob(t.Context(), request)
	require.NoError(t, err)
	now := time.Now().UTC().Add(time.Second)
	claim, err := s.ClaimRenditionJob(t.Context(), job.ID, "worker-a", now, time.Minute)
	require.NoError(t, err)
	_, err = s.BeginRenditionProvider(t.Context(), claim, waiter.ID,
		now.Add(time.Second), renditionJobTestSnapshot(request))
	require.NoError(t, err)
	_, err = s.RevokeConsent(t.Context(), ProcessingConsentRevocationRequest{
		Principal: request.Authorization.Principal, Scope: request.Authorization.Scope,
	})
	require.NoError(t, err)

	newBuild := cloneCatalogBuild(oldBuild)
	newBuild.ID = job.ID
	newBuild.CapturedArtifactPolicy = policy
	newBuild.CapturedArtifactPolicyFingerprint = testSHA256(policy)
	require.NoError(t, s.StageRenditionJobBuild(
		t.Context(), claim, newBuild, now.Add(2*time.Second)))
	_, err = s.StageRenditionJobGeneration(
		t.Context(), claim, testSHA256([]byte("new-generation")), now.Add(3*time.Second))
	require.NoError(t, err)

	_, err = s.PublishRenditionJob(t.Context(), claim, now.Add(4*time.Second))
	require.ErrorIs(t, err, ErrProcessingConsentRevoked)
	active, err := s.ActiveRendition(t.Context(), versions[0], profile.Fingerprint)
	require.NoError(t, err)
	assert.Equal(t, oldBuild.ID, active.Build.ID)
	lexical, err := s.ActiveLexicalGeneration(t.Context())
	require.NoError(t, err)
	assert.Equal(t, oldGeneration.ID, lexical.ID)
}

func TestPublishRenditionJobAllowsDegradedActivationForRevokedWaiter(t *testing.T) {
	s, versions := newRenditionCatalogFixture(t)
	profile := catalogProcessingProfile(t, false)
	firstRequest := renditionJobTestRequest(versions[0], profile)
	secondRequest := renditionJobTestRequest(versions[1], profile)
	secondRequest.Authorization.Principal = "operator:revoked-waiter"
	grantRenditionJobConsent(t, s, firstRequest)
	grantRenditionJobConsent(t, s, secondRequest)
	job, firstWaiter, err := s.EnqueueRenditionJob(t.Context(), firstRequest)
	require.NoError(t, err)
	_, secondWaiter, err := s.EnqueueRenditionJob(t.Context(), secondRequest)
	require.NoError(t, err)
	now := time.Now().UTC().Add(time.Second)
	claim, err := s.ClaimRenditionJob(t.Context(), job.ID, "worker-a", now, time.Minute)
	require.NoError(t, err)
	_, err = s.BeginRenditionProvider(t.Context(), claim, firstWaiter.ID,
		now.Add(time.Second), renditionJobTestSnapshot(firstRequest))
	require.NoError(t, err)
	_, err = s.RevokeConsent(t.Context(), ProcessingConsentRevocationRequest{
		Principal: secondRequest.Authorization.Principal, Scope: secondRequest.Authorization.Scope,
	})
	require.NoError(t, err)
	build := catalogRenditionBuild(s, profile)
	build.ID = job.ID
	require.NoError(t, s.StageRenditionJobBuild(
		t.Context(), claim, build, now.Add(2*time.Second)))
	_, err = s.StageRenditionJobGeneration(
		t.Context(), claim, testSHA256([]byte("degraded-generation")), now.Add(3*time.Second))
	require.NoError(t, err)

	publication, err := s.PublishRenditionJob(t.Context(), claim, now.Add(4*time.Second))
	require.NoError(t, err)
	assert.Equal(t, 1, publication.PublishedWaiterCount)
	assert.Equal(t, 1, publication.RejectedWaiterCount)
	_, err = s.ActiveRendition(t.Context(), versions[0], profile.Fingerprint)
	require.NoError(t, err)
	_, err = s.ActiveRendition(t.Context(), versions[1], profile.Fingerprint)
	require.ErrorIs(t, err, ErrNotFound)
	publishedWaiter, err := s.RenditionJobWaiterByID(t.Context(), firstWaiter.ID)
	require.NoError(t, err)
	assert.Equal(t, "published", publishedWaiter.State)
	assert.Empty(t, publishedWaiter.FailureCode)
	rejectedWaiter, err := s.RenditionJobWaiterByID(t.Context(), secondWaiter.ID)
	require.NoError(t, err)
	assert.Equal(t, "rejected", rejectedWaiter.State)
	assert.Equal(t, RenditionFailureConsent, rejectedWaiter.FailureCode)
}

func TestPublishRenditionJobAllowsDegradedActivationForStaleWaiter(t *testing.T) {
	s, versions := newRenditionCatalogFixture(t)
	profile := catalogProcessingProfile(t, false)
	firstRequest := renditionJobTestRequest(versions[0], profile)
	secondRequest := renditionJobTestRequest(versions[1], profile)
	secondRequest.Authorization.Principal = "operator:stale-waiter"
	grantRenditionJobConsent(t, s, firstRequest)
	grantRenditionJobConsent(t, s, secondRequest)
	job, firstWaiter, err := s.EnqueueRenditionJob(t.Context(), firstRequest)
	require.NoError(t, err)
	_, secondWaiter, err := s.EnqueueRenditionJob(t.Context(), secondRequest)
	require.NoError(t, err)
	now := time.Now().UTC().Add(time.Second)
	claim, err := s.ClaimRenditionJob(t.Context(), job.ID, "worker-a", now, time.Minute)
	require.NoError(t, err)
	_, err = s.BeginRenditionProvider(t.Context(), claim, firstWaiter.ID,
		now.Add(time.Second), renditionJobTestSnapshot(firstRequest))
	require.NoError(t, err)
	driftedHash := testSHA256([]byte("synthetic secondary waiter drift"))
	require.NoError(t, s.withStorageTx(t.Context(), func(tx *sql.Tx) error {
		if err := s.EnsureBlobTx(tx, driftedHash, 32); err != nil {
			return err
		}
		_, err := tx.ExecContext(t.Context(),
			`UPDATE content_versions SET blob_hash=? WHERE version_id=?`, driftedHash, versions[1])
		return err
	}))
	build := catalogRenditionBuild(s, profile)
	build.ID = job.ID
	require.NoError(t, s.StageRenditionJobBuild(
		t.Context(), claim, build, now.Add(2*time.Second)))
	_, err = s.StageRenditionJobGeneration(
		t.Context(), claim, testSHA256([]byte("stale-waiter-generation")), now.Add(3*time.Second))
	require.NoError(t, err)

	publication, err := s.PublishRenditionJob(t.Context(), claim, now.Add(4*time.Second))
	require.NoError(t, err)
	assert.Equal(t, 1, publication.PublishedWaiterCount)
	assert.Equal(t, 1, publication.RejectedWaiterCount)
	_, err = s.ActiveRendition(t.Context(), versions[0], profile.Fingerprint)
	require.NoError(t, err)
	_, err = s.ActiveRendition(t.Context(), versions[1], profile.Fingerprint)
	require.ErrorIs(t, err, ErrNotFound)
	publishedWaiter, err := s.RenditionJobWaiterByID(t.Context(), firstWaiter.ID)
	require.NoError(t, err)
	assert.Equal(t, "published", publishedWaiter.State)
	assert.Empty(t, publishedWaiter.FailureCode)
	rejectedWaiter, err := s.RenditionJobWaiterByID(t.Context(), secondWaiter.ID)
	require.NoError(t, err)
	assert.Equal(t, "rejected", rejectedWaiter.State)
	assert.Equal(t, RenditionFailureStaleAuthority, rejectedWaiter.FailureCode)
}
