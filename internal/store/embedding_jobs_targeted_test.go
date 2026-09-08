package store

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/document"
)

func TestTargetedEmbeddingClaimLeavesEarlierUnrelatedJobQueued(t *testing.T) {
	s, versionID, profile, _ := newEmbeddingCatalogFixture(t)
	firstRequest := embeddingJobTestRequest(t, s, versionID, profile, "target-first")
	targetRequest := embeddingJobTestRequest(t, s, versionID, profile, "target-second")
	first, err := s.EnqueueEmbeddingJob(t.Context(), firstRequest)
	require.NoError(t, err)
	target, err := s.EnqueueEmbeddingJob(t.Context(), targetRequest)
	require.NoError(t, err)
	at := time.Now().UTC().Add(time.Minute)
	_, err = s.db.Exec(`UPDATE embedding_jobs SET available_at=? WHERE job_id=?`,
		at.Add(-time.Hour).Format(timestampLayout), first.ID)
	require.NoError(t, err)

	claim, work, found, err := s.ClaimEmbeddingWork(t.Context(), target.ID,
		"target-worker", at, time.Minute, []string{targetRequest.Descriptor.Fingerprint})
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, target.ID, claim.AttemptID)
	require.Equal(t, targetRequest.InputGeneration.ID, work.InputGeneration.ID)

	var firstState, targetState string
	var firstClaims, targetClaims, targetRoots int
	var rootTarget string
	var rootEpoch int64
	require.NoError(t, s.db.QueryRow(`SELECT state,claim_count FROM embedding_jobs WHERE job_id=?`,
		first.ID).Scan(&firstState, &firstClaims))
	require.NoError(t, s.db.QueryRow(`SELECT state,claim_count FROM embedding_jobs WHERE job_id=?`,
		target.ID).Scan(&targetState, &targetClaims))
	require.NoError(t, s.db.QueryRow(`SELECT COUNT(*),COALESCE(MAX(target_id),''),COALESCE(MAX(fencing_token),0)
		FROM current_rendition_roots WHERE root_id=? AND active=1`, target.ID).
		Scan(&targetRoots, &rootTarget, &rootEpoch))
	require.Equal(t, "queued", firstState)
	require.Zero(t, firstClaims)
	require.Equal(t, "running", targetState)
	require.Equal(t, 1, targetClaims)
	require.Equal(t, 1, targetRoots)
	require.Equal(t, targetRequest.InputGeneration.ID, rootTarget)
	require.Equal(t, claim.Epoch, rootEpoch)

	next, _, found, err := s.ClaimNextEmbeddingWork(t.Context(), "ordinary-worker", at,
		time.Minute, []string{firstRequest.Descriptor.Fingerprint})
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, first.ID, next.AttemptID)
}

func TestTargetedEmbeddingClaimEligibility(t *testing.T) {
	testCases := []struct {
		name    string
		setup   string
		wantErr bool
	}{
		{name: "unknown valid ID", setup: "unknown"},
		{name: "malformed ID", setup: "malformed", wantErr: true},
		{name: "unconfigured descriptor", setup: "unconfigured"},
		{name: "empty fingerprints", setup: "empty", wantErr: true},
		{name: "future availability", setup: "future"},
		{name: "active lease", setup: "active"},
		{name: "completed", setup: "completed"},
		{name: "failed", setup: "failed"},
		{name: "abandoned", setup: "abandoned"},
		{name: "exact published head", setup: "published"},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			s, versionID, profile, _ := newEmbeddingCatalogFixture(t)
			targetRequest := embeddingJobTestRequest(t, s, versionID, profile,
				"eligibility-target-"+testCase.setup)
			unrelatedRequest := embeddingJobTestRequest(t, s, versionID, profile,
				"eligibility-unrelated-"+testCase.setup)
			target, err := s.EnqueueEmbeddingJob(t.Context(), targetRequest)
			require.NoError(t, err)
			unrelated, err := s.EnqueueEmbeddingJob(t.Context(), unrelatedRequest)
			require.NoError(t, err)

			at := time.Now().UTC().Add(time.Minute)
			jobID := target.ID
			fingerprints := []string{targetRequest.Descriptor.Fingerprint}
			switch testCase.setup {
			case "unknown":
				jobID = testSHA256([]byte("unknown-targeted-embedding-job"))
			case "malformed":
				jobID = "not-a-sha256"
			case "unconfigured":
				fingerprints = []string{fakeHash("other-runtime")}
			case "empty":
				fingerprints = nil
			case "future":
				// Seed only the availability boundary under test; the retained
				// catalog authority still comes from the real fixture.
				_, err = s.db.Exec(`UPDATE embedding_jobs SET available_at=? WHERE job_id=?`,
					at.Add(time.Hour).Format(timestampLayout), target.ID)
				require.NoError(t, err)
			case "active", "completed", "failed", "abandoned":
				claim, work, found, claimErr := s.ClaimEmbeddingWork(t.Context(), target.ID,
					"setup-worker", at, time.Minute, fingerprints)
				require.NoError(t, claimErr)
				require.True(t, found)
				at = at.Add(10 * time.Second)
				switch testCase.setup {
				case "completed":
					require.NoError(t, s.FinishEmbeddingWork(t.Context(), claim,
						EmbeddingAttemptReceipt{AttemptID: target.ID}, at))
				case "failed":
					require.NoError(t, s.FailEmbeddingWork(t.Context(), claim, work,
						EmbeddingFailureInputRejected, EmbeddingAttemptReceipt{AttemptID: target.ID}, at))
				case "abandoned":
					require.NoError(t, s.AbandonEmbeddingWork(t.Context(), claim, at))
				}
			case "published":
				set := embeddingSetFixture(s, versionID, profile.Fingerprint,
					document.EmbeddingInputOriginalFile, targetRequest.BindingID, "")
				set.ID = testSHA256([]byte("exact-target-published-set"))
				set.InputGeneration = targetRequest.InputGeneration
				require.NoError(t, s.StageEmbeddingSet(t.Context(), set))
				require.NoError(t, s.PublishEmbeddingHead(t.Context(), EmbeddingHeadRecord{
					FencingToken: 7,
					Key: EmbeddingHeadKey{ContentVersionID: versionID, BindingID: targetRequest.BindingID,
						InputKind: document.EmbeddingInputOriginalFile},
					SetID: set.ID, VectorSpaceID: set.VectorSpace.ID,
					ProcessingProfileFingerprint: profile.Fingerprint, PublishedAt: embeddingCatalogTime,
				}))
			}

			beforeClaims, beforeRoots := targetedEmbeddingMutationCounts(t, s, target.ID)
			claim, work, found, err := s.ClaimEmbeddingWork(t.Context(), jobID,
				"target-worker", at, time.Minute, fingerprints)
			if testCase.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
				require.False(t, found)
			}
			require.Equal(t, EmbeddingJobClaim{}, claim)
			require.Equal(t, EmbeddingJobWork{}, work)
			afterClaims, afterRoots := targetedEmbeddingMutationCounts(t, s, target.ID)
			require.Equal(t, beforeClaims, afterClaims)
			require.Equal(t, beforeRoots, afterRoots)

			unrelatedClaim, _, unrelatedFound, unrelatedErr := s.ClaimEmbeddingWork(t.Context(),
				unrelated.ID, "unrelated-worker", at, time.Minute,
				[]string{unrelatedRequest.Descriptor.Fingerprint})
			require.NoError(t, unrelatedErr)
			require.True(t, unrelatedFound)
			require.Equal(t, unrelated.ID, unrelatedClaim.AttemptID)
		})
	}
}

func TestTargetedEmbeddingClaimReclaimsExpiredLease(t *testing.T) {
	s, versionID, profile, _ := newEmbeddingCatalogFixture(t)
	request := embeddingJobTestRequest(t, s, versionID, profile, "expired-target")
	job, err := s.EnqueueEmbeddingJob(t.Context(), request)
	require.NoError(t, err)
	at := time.Now().UTC().Add(time.Minute)
	first, _, found, err := s.ClaimEmbeddingWork(t.Context(), job.ID, "first-worker", at,
		time.Minute, []string{request.Descriptor.Fingerprint})
	require.NoError(t, err)
	require.True(t, found)

	successor, work, found, err := s.ClaimEmbeddingWork(t.Context(), job.ID, "successor-worker",
		at.Add(2*time.Minute), time.Minute, []string{request.Descriptor.Fingerprint})
	require.NoError(t, err)
	require.True(t, found)
	require.Greater(t, successor.Epoch, first.Epoch)
	require.NoError(t, s.AbandonEmbeddingWork(t.Context(), first, at.Add(2*time.Minute)))
	require.NoError(t, s.ValidateEmbeddingWork(t.Context(), successor, work, at.Add(2*time.Minute)))
	claims, roots := targetedEmbeddingMutationCounts(t, s, job.ID)
	require.Equal(t, 2, claims)
	require.Equal(t, 1, roots)
}

func TestTargetedEmbeddingClaimPreservesRetryAndGlobalFencing(t *testing.T) {
	s, versionID, profile, _ := newEmbeddingCatalogFixture(t)
	priorSet := embeddingSetFixture(s, versionID, profile.Fingerprint,
		document.EmbeddingInputOriginalFile, "optional", "")
	require.NoError(t, s.StageEmbeddingSet(t.Context(), priorSet))
	require.NoError(t, s.PublishEmbeddingHead(t.Context(), EmbeddingHeadRecord{
		FencingToken: 10,
		Key: EmbeddingHeadKey{ContentVersionID: versionID, BindingID: "optional",
			InputKind: document.EmbeddingInputOriginalFile},
		SetID: priorSet.ID, VectorSpaceID: priorSet.VectorSpace.ID,
		ProcessingProfileFingerprint: profile.Fingerprint, PublishedAt: embeddingCatalogTime,
	}))
	require.NoError(t, s.RecordEmbeddingFailure(t.Context(), EmbeddingFailureRecord{
		FencingToken: 11, ContentVersionID: versionID,
		ProcessingProfileFingerprint: profile.Fingerprint, BindingID: "optional",
		InputKind:   document.EmbeddingInputOriginalFile,
		FailureCode: EmbeddingFailureProviderUnavailable, FailedAt: embeddingCatalogTime,
	}))

	siblingRequest := embeddingJobTestRequest(t, s, versionID, profile, "fencing-sibling")
	targetRequest := embeddingJobTestRequest(t, s, versionID, profile, "fencing-target")
	sibling, err := s.EnqueueEmbeddingJob(t.Context(), siblingRequest)
	require.NoError(t, err)
	target, err := s.EnqueueEmbeddingJob(t.Context(), targetRequest)
	require.NoError(t, err)
	at := time.Now().UTC().Add(time.Minute)
	siblingClaim, _, found, err := s.ClaimEmbeddingWork(t.Context(), sibling.ID,
		"sibling-worker", at, time.Minute, []string{siblingRequest.Descriptor.Fingerprint})
	require.NoError(t, err)
	require.True(t, found)
	require.Greater(t, siblingClaim.Epoch, int64(11))
	require.NoError(t, s.AbandonEmbeddingWork(t.Context(), siblingClaim, at.Add(10*time.Second)))

	claim, work, found, err := s.ClaimEmbeddingWork(t.Context(), target.ID, "target-worker",
		at.Add(10*time.Second), time.Minute, []string{targetRequest.Descriptor.Fingerprint})
	require.NoError(t, err)
	require.True(t, found)
	require.Greater(t, claim.Epoch, siblingClaim.Epoch)
	failureAt := at.Add(20 * time.Second)
	require.NoError(t, s.FailEmbeddingWork(t.Context(), claim, work,
		EmbeddingFailureProviderUnavailable, EmbeddingAttemptReceipt{AttemptID: target.ID}, failureAt))

	_, _, found, err = s.ClaimEmbeddingWork(t.Context(), target.ID, "early-worker",
		failureAt.Add(30*time.Second), time.Minute, []string{targetRequest.Descriptor.Fingerprint})
	require.NoError(t, err)
	require.False(t, found)

	successorAt := failureAt.Add(2 * time.Minute)
	successor, successorWork, found, err := s.ClaimEmbeddingWork(t.Context(), target.ID,
		"successor-worker", successorAt, time.Minute, []string{targetRequest.Descriptor.Fingerprint})
	require.NoError(t, err)
	require.True(t, found)
	require.Greater(t, successor.Epoch, claim.Epoch)
	var headToken, failureToken int64
	require.NoError(t, s.db.QueryRow(`SELECT fencing_token FROM embedding_heads
		WHERE content_version_id=? AND profile_fingerprint=? AND binding_id=? AND input_kind=?`,
		versionID, profile.Fingerprint, "optional", document.EmbeddingInputOriginalFile).Scan(&headToken))
	require.NoError(t, s.db.QueryRow(`SELECT fencing_token FROM embedding_failures
		WHERE content_version_id=? AND profile_fingerprint=? AND binding_id=? AND input_kind=?`,
		versionID, profile.Fingerprint, "optional", document.EmbeddingInputOriginalFile).Scan(&failureToken))
	require.Greater(t, successor.Epoch, headToken)
	require.Greater(t, successor.Epoch, failureToken)
	require.NoError(t, s.AbandonEmbeddingWork(t.Context(), claim, successorAt))
	require.NoError(t, s.ValidateEmbeddingWork(t.Context(), successor, successorWork, successorAt))
	claims, roots := targetedEmbeddingMutationCounts(t, s, target.ID)
	require.Equal(t, 2, claims)
	require.Equal(t, 1, roots)
}

func targetedEmbeddingMutationCounts(t *testing.T, s *Store, jobID string) (int, int) {
	t.Helper()
	var claims, roots int
	require.NoError(t, s.db.QueryRow(`SELECT
		COALESCE((SELECT claim_count FROM embedding_jobs WHERE job_id=?),0),
		(SELECT COUNT(*) FROM current_rendition_roots WHERE root_id=? AND active=1)`,
		jobID, jobID).Scan(&claims, &roots))
	return claims, roots
}
