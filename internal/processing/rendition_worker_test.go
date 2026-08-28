package processing

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"io"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/blob"
	"go.kenn.io/docbank/internal/store"
)

func TestRenditionWorkerPublishesNormalizedBuildAndAllAuthorizedWaiters(t *testing.T) {
	fixture := newPublicationFixture(t)
	provider := newWorkerProvider(t)
	profile := workerProcessingProfile(t, provider.Descriptor())
	fixture.profile = profile
	request := workerJobRequest(fixture.versionID, profile, provider.Descriptor())
	grantWorkerConsent(t, fixture.catalog, request)
	job, _, err := fixture.catalog.EnqueueRenditionJob(t.Context(), request)
	require.NoError(t, err)
	second, err := fixture.catalog.CreateFile(t.Context(), fixture.catalog.RootID(),
		"same-source.pdf", fixture.mustSourceHash(), int64(len(workerSourceBytes)), "application/pdf")
	require.NoError(t, err)
	request.ContentVersionID = second.CurrentVersionID
	_, _, err = fixture.catalog.EnqueueRenditionJob(t.Context(), request)
	require.NoError(t, err)

	now := time.Now().UTC()
	worker, err := NewRenditionWorker(RenditionWorkerConfig{
		Catalog: fixture.catalog, Blobs: fixture.blobs,
		Runtime: workerRuntime{provider: provider}, Gate: newWorkerTestGate(),
		Owner:         "rendition-worker-test",
		LeaseDuration: time.Minute, IdleDelay: time.Millisecond,
		Clock: func() time.Time { return now },
	})
	require.NoError(t, err)
	processed, err := worker.RunOne(t.Context())
	require.NoError(t, err)
	assert.True(t, processed)
	assert.Equal(t, 1, provider.calls)

	current, err := fixture.catalog.RenditionJobByID(t.Context(), job.ID)
	require.NoError(t, err)
	assert.Equal(t, store.RenditionJobCompleted, current.State)
	assert.Equal(t, 2, current.PublishedWaiterCount)
	firstView, err := fixture.catalog.ActiveRendition(
		t.Context(), fixture.versionID, profile.Fingerprint)
	require.NoError(t, err)
	secondView, err := fixture.catalog.ActiveRendition(
		t.Context(), second.CurrentVersionID, profile.Fingerprint)
	require.NoError(t, err)
	assert.Equal(t, job.ID, firstView.Build.ID)
	assert.Equal(t, job.ID, secondView.Build.ID)
	assert.Equal(t, firstView.Build.Artifacts, secondView.Build.Artifacts,
		"version-scoped attachment identity must not enter shared artifact records")
	assert.Equal(t, document.EvidenceDegradedProvenance, firstView.Build.Completeness)

	late, err := fixture.catalog.CreateFile(t.Context(), fixture.catalog.RootID(),
		"late-same-source.pdf", fixture.mustSourceHash(), int64(len(workerSourceBytes)), "application/pdf")
	require.NoError(t, err)
	request.ContentVersionID = late.CurrentVersionID
	reopened, _, err := fixture.catalog.EnqueueRenditionJob(t.Context(), request)
	require.NoError(t, err)
	assert.Equal(t, store.RenditionJobQueued, reopened.State)
	assert.Equal(t, store.RenditionPhaseBuildStaged, reopened.Phase)
	now = time.Now().UTC().Add(time.Second)
	processed, err = worker.RunOne(t.Context())
	require.NoError(t, err)
	assert.True(t, processed)
	assert.Equal(t, 1, provider.calls, "a late waiter must reuse the completed shared build")
	lateView, err := fixture.catalog.ActiveRendition(
		t.Context(), late.CurrentVersionID, profile.Fingerprint)
	require.NoError(t, err)
	assert.Equal(t, job.ID, lateView.Build.ID)

	request.Authorization.Principal = "operator:already-active"
	grantWorkerConsent(t, fixture.catalog, request)
	alreadyActive, waiter, err := fixture.catalog.EnqueueRenditionJob(t.Context(), request)
	require.NoError(t, err)
	assert.Equal(t, store.RenditionJobCompleted, alreadyActive.State)
	assert.Equal(t, "published", waiter.State)
	processed, err = worker.RunOne(t.Context())
	require.NoError(t, err)
	assert.False(t, processed)
	assert.Equal(t, 1, provider.calls)
}

func TestRenditionWorkerHonorsDaemonOperationGateAndCancellation(t *testing.T) {
	for _, cancelWhileHeld := range []bool{false, true} {
		name := "release"
		if cancelWhileHeld {
			name = "cancel"
		}
		t.Run(name, func(t *testing.T) {
			fixture := newPublicationFixture(t)
			provider := newWorkerProvider(t)
			profile := workerProcessingProfile(t, provider.Descriptor())
			fixture.profile = profile
			request := workerJobRequest(fixture.versionID, profile, provider.Descriptor())
			grantWorkerConsent(t, fixture.catalog, request)
			job, _, err := fixture.catalog.EnqueueRenditionJob(t.Context(), request)
			require.NoError(t, err)
			gate := newWorkerTestGate()
			held := make(chan struct{})
			release := make(chan struct{})
			maintenanceDone := make(chan error, 1)
			go func() {
				maintenanceDone <- gate.MaintainContext(t.Context(), func() error {
					close(held)
					<-release
					return nil
				})
			}()
			<-held
			ctx, cancel := context.WithCancel(t.Context())
			worker, err := NewRenditionWorker(RenditionWorkerConfig{
				Catalog: fixture.catalog, Blobs: fixture.blobs,
				Runtime: workerRuntime{provider: provider}, Gate: gate,
				Owner: "rendition-worker-gate-test", LeaseDuration: time.Minute,
				IdleDelay: time.Millisecond,
			})
			require.NoError(t, err)
			result := make(chan error, 1)
			go func() {
				_, runErr := worker.RunOne(ctx)
				result <- runErr
			}()
			require.Never(t, func() bool {
				return provider.calls != 0
			}, 100*time.Millisecond, 5*time.Millisecond)
			current, err := fixture.catalog.RenditionJobByID(t.Context(), job.ID)
			require.NoError(t, err)
			assert.Equal(t, store.RenditionJobQueued, current.State)
			assert.Zero(t, current.ClaimEpoch,
				"claim and every later physical/catalog mutation stay behind the daemon gate")

			if cancelWhileHeld {
				cancel()
				require.ErrorIs(t, <-result, context.Canceled)
				close(release)
			} else {
				close(release)
				require.NoError(t, <-result)
				cancel()
				assert.Equal(t, 1, provider.calls)
			}
			require.NoError(t, <-maintenanceDone)
		})
	}
}

func TestRenditionWorkerRetriesTransientCatalogFailuresWithinClaim(t *testing.T) {
	for _, failurePoint := range []string{"claim", "post-egress record"} {
		t.Run(failurePoint, func(t *testing.T) {
			fixture := newPublicationFixture(t)
			provider := newWorkerProvider(t)
			profile := workerProcessingProfile(t, provider.Descriptor())
			fixture.profile = profile
			request := workerJobRequest(fixture.versionID, profile, provider.Descriptor())
			grantWorkerConsent(t, fixture.catalog, request)
			job, _, err := fixture.catalog.EnqueueRenditionJob(t.Context(), request)
			require.NoError(t, err)
			catalog := &transientRenditionCatalog{Store: fixture.catalog}
			if failurePoint == "claim" {
				catalog.claimFailures.Store(1)
			} else {
				catalog.recordFailures.Store(5)
			}
			worker, err := NewRenditionWorker(RenditionWorkerConfig{
				Catalog: catalog, Blobs: fixture.blobs,
				Runtime: workerRuntime{provider: provider}, Gate: newWorkerTestGate(),
				Owner: "rendition-worker-transient-store", LeaseDuration: time.Minute,
				IdleDelay: time.Millisecond,
			})
			require.NoError(t, err)

			if failurePoint == "claim" {
				processed, runErr := worker.RunOne(t.Context())
				assert.False(t, processed)
				require.True(t, isRenditionWorkerRetryable(runErr),
					"claim contention must unwind the daemon operation gate")
				processed, runErr = worker.RunOne(t.Context())
				require.NoError(t, runErr)
				assert.True(t, processed)
			} else {
				processed, runErr := worker.RunOne(t.Context())
				require.NoError(t, runErr)
				assert.True(t, processed)
			}
			assert.Equal(t, 1, provider.calls,
				"retrying a catalog mutation must not repeat provider egress")
			current, err := fixture.catalog.RenditionJobByID(t.Context(), job.ID)
			require.NoError(t, err)
			assert.Equal(t, store.RenditionJobCompleted, current.State)
		})
	}
}

func TestRenditionWorkerTransientCatalogRetryCancelsCleanly(t *testing.T) {
	fixture := newPublicationFixture(t)
	catalog := &transientRenditionCatalog{Store: fixture.catalog, failClaimsForever: true}
	worker, err := NewRenditionWorker(RenditionWorkerConfig{
		Catalog: catalog, Blobs: fixture.blobs,
		Runtime: workerRuntime{provider: newWorkerProvider(t)}, Gate: newWorkerTestGate(),
		Owner: "rendition-worker-transient-cancel", LeaseDuration: time.Minute,
		IdleDelay: time.Millisecond,
	})
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- worker.Run(ctx) }()
	require.Eventually(t, func() bool {
		return catalog.claimAttempts.Load() >= 2
	}, time.Second, time.Millisecond)
	cancel()
	require.ErrorIs(t, <-done, context.Canceled)
}

func TestRenditionWorkerStopsLeaseWhileRenewalRetries(t *testing.T) {
	fixture := newPublicationFixture(t)
	catalog := &transientRenditionCatalog{
		Store: fixture.catalog, failRenewalsForever: true,
	}
	worker, err := NewRenditionWorker(RenditionWorkerConfig{
		Catalog: catalog, Blobs: fixture.blobs,
		Runtime: workerRuntime{provider: newWorkerProvider(t)}, Gate: newWorkerTestGate(),
		Owner: "rendition-worker-lease-stop", LeaseDuration: 3 * time.Second,
		IdleDelay: time.Millisecond,
	})
	require.NoError(t, err)
	leaseCtx, stopLease := worker.keepLease(t.Context(), store.RenditionJobClaim{})
	require.Eventually(t, func() bool {
		return catalog.renewalAttempts.Load() >= 1
	}, 2*time.Second, time.Millisecond)

	done := make(chan error, 1)
	go func() { done <- stopLease() }()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("lease shutdown waited for a retry that only its own cancellation could stop")
	}
	require.ErrorIs(t, context.Cause(leaseCtx), errRenditionLeaseStopped)
}

func TestRenditionWorkerPostEgressCatalogRetryCancelsWithoutTombstone(t *testing.T) {
	fixture := newPublicationFixture(t)
	provider := newWorkerProvider(t)
	profile := workerProcessingProfile(t, provider.Descriptor())
	fixture.profile = profile
	request := workerJobRequest(fixture.versionID, profile, provider.Descriptor())
	grantWorkerConsent(t, fixture.catalog, request)
	job, _, err := fixture.catalog.EnqueueRenditionJob(t.Context(), request)
	require.NoError(t, err)
	catalog := &transientRenditionCatalog{
		Store: fixture.catalog, failRecordsForever: true,
	}
	worker, err := NewRenditionWorker(RenditionWorkerConfig{
		Catalog: catalog, Blobs: fixture.blobs,
		Runtime: workerRuntime{provider: provider}, Gate: newWorkerTestGate(),
		Owner: "rendition-worker-post-egress-cancel", LeaseDuration: time.Minute,
		IdleDelay: time.Millisecond,
	})
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		_, runErr := worker.RunOne(ctx)
		done <- runErr
	}()
	require.Eventually(t, func() bool {
		return catalog.recordAttempts.Load() > 3
	}, 10*time.Second, time.Millisecond)
	cancel()
	require.ErrorIs(t, <-done, context.Canceled)
	assert.Equal(t, 1, provider.calls)
	current, err := fixture.catalog.RenditionJobByID(t.Context(), job.ID)
	require.NoError(t, err)
	assert.Equal(t, store.RenditionJobRunning, current.State)
	assert.NotEqual(t, store.RenditionJobOperatorRequired, current.State)
}

func TestRenditionWorkerPersistsAmbiguousOutcomeWithoutResubmission(t *testing.T) {
	fixture := newPublicationFixture(t)
	provider := newWorkerProvider(t)
	provider.renderErr = workerProviderError(t, document.RenditionErrorAmbiguousSubmission)
	profile := workerProcessingProfile(t, provider.Descriptor())
	fixture.profile = profile
	request := workerJobRequest(fixture.versionID, profile, provider.Descriptor())
	grantWorkerConsent(t, fixture.catalog, request)
	job, _, err := fixture.catalog.EnqueueRenditionJob(t.Context(), request)
	require.NoError(t, err)
	now := time.Now().UTC()
	worker, err := NewRenditionWorker(RenditionWorkerConfig{
		Catalog: fixture.catalog, Blobs: fixture.blobs,
		Runtime: workerRuntime{provider: provider}, Gate: newWorkerTestGate(),
		Owner:         "rendition-worker-test",
		LeaseDuration: time.Minute, IdleDelay: time.Millisecond,
		Clock: func() time.Time { return now },
	})
	require.NoError(t, err)

	processed, err := worker.RunOne(t.Context())
	require.NoError(t, err)
	assert.True(t, processed)
	current, err := fixture.catalog.RenditionJobByID(t.Context(), job.ID)
	require.NoError(t, err)
	assert.Equal(t, store.RenditionJobOperatorRequired, current.State)
	assert.Equal(t, store.RenditionFailureAmbiguous, current.FailureCode)

	processed, err = worker.RunOne(t.Context())
	require.NoError(t, err)
	assert.False(t, processed)
	assert.Equal(t, 1, provider.calls, "operator-required work must never be resubmitted")
}

func TestRenditionWorkerResubmitsDefinitiveTransientWithFreshSealedAuthority(t *testing.T) {
	fixture := newPublicationFixture(t)
	provider := newWorkerProvider(t)
	providerErr, err := document.NewRenditionProviderError(
		document.RenditionErrorTransient, "synthetic definitive failure", time.Nanosecond,
		errors.New("private cause"),
	)
	require.NoError(t, err)
	provider.renderErr = providerErr
	profile := workerProcessingProfile(t, provider.Descriptor())
	fixture.profile = profile
	request := workerJobRequest(fixture.versionID, profile, provider.Descriptor())
	grantWorkerConsent(t, fixture.catalog, request)
	job, _, err := fixture.catalog.EnqueueRenditionJob(t.Context(), request)
	require.NoError(t, err)
	now := time.Now().UTC()
	prepareCalls := 0
	runtime := &countingWorkerRuntime{provider: provider, prepareCalls: &prepareCalls}
	worker, err := NewRenditionWorker(RenditionWorkerConfig{
		Catalog: fixture.catalog, Blobs: fixture.blobs,
		Runtime: runtime, Gate: newWorkerTestGate(),
		Owner: "rendition-worker-definitive-retry", LeaseDuration: time.Minute,
		IdleDelay: time.Millisecond, Clock: func() time.Time { return now },
	})
	require.NoError(t, err)

	processed, err := worker.RunOne(t.Context())
	require.NoError(t, err)
	assert.True(t, processed)
	current, err := fixture.catalog.RenditionJobByID(t.Context(), job.ID)
	require.NoError(t, err)
	assert.Equal(t, store.RenditionJobRetryWait, current.State)

	provider.renderErr = nil
	now = now.Add(time.Nanosecond)
	processed, err = worker.RunOne(t.Context())
	require.NoError(t, err)
	assert.True(t, processed)
	assert.Equal(t, 2, provider.calls)
	assert.Equal(t, 2, prepareCalls,
		"a definitive no-handle retry is a new sealed submission, not a resume")
	current, err = fixture.catalog.RenditionJobByID(t.Context(), job.ID)
	require.NoError(t, err)
	assert.Equal(t, store.RenditionJobCompleted, current.State)
}

func TestRenditionWorkerRetainsProviderCheckpointAcrossLocalStagingFailure(t *testing.T) {
	fixture := newPublicationFixture(t)
	baseProvider := newWorkerProvider(t)
	provider := &resumableWorkerProvider{workerProvider: baseProvider}
	profile := workerProcessingProfile(t, provider.Descriptor())
	fixture.profile = profile
	request := workerJobRequest(fixture.versionID, profile, provider.Descriptor())
	grantWorkerConsent(t, fixture.catalog, request)
	job, _, err := fixture.catalog.EnqueueRenditionJob(t.Context(), request)
	require.NoError(t, err)
	now := time.Now().UTC()
	worker, err := NewRenditionWorker(RenditionWorkerConfig{
		Catalog: fixture.catalog,
		Blobs:   &failOnceRenditionBlobWriter{delegate: fixture.blobs},
		Runtime: workerRuntime{provider: baseProvider}, Gate: newWorkerTestGate(),
		Owner:         "rendition-worker-test",
		LeaseDuration: time.Minute, IdleDelay: time.Millisecond,
		Clock: func() time.Time { return now },
	})
	require.NoError(t, err)
	worker.runtime = resumableWorkerRuntime{provider: provider}

	processed, err := worker.RunOne(t.Context())
	require.NoError(t, err)
	assert.True(t, processed)
	assert.Equal(t, 1, baseProvider.calls)
	current, err := fixture.catalog.RenditionJobByID(t.Context(), job.ID)
	require.NoError(t, err)
	assert.Equal(t, store.RenditionJobRetryWait, current.State)
	assert.Equal(t, store.RenditionFailureTransient, current.FailureCode)
}

func TestRenditionWorkerResumesAmbiguousProviderOutcomeWithDurableHandle(t *testing.T) {
	fixture := newPublicationFixture(t)
	baseProvider := newWorkerProvider(t)
	providerErr, err := document.NewRenditionProviderError(
		document.RenditionErrorAmbiguousSubmission, "synthetic failure", time.Nanosecond,
		errors.New("private cause"),
	)
	require.NoError(t, err)
	baseProvider.renderErr = providerErr
	provider := &resumableWorkerProvider{workerProvider: baseProvider}
	profile := workerProcessingProfile(t, provider.Descriptor())
	fixture.profile = profile
	request := workerJobRequest(fixture.versionID, profile, provider.Descriptor())
	grantWorkerConsent(t, fixture.catalog, request)
	job, _, err := fixture.catalog.EnqueueRenditionJob(t.Context(), request)
	require.NoError(t, err)
	now := time.Now().UTC()
	prepareCalls := 0
	resumeRuntimeCalls := 0
	runtime := &resumableWorkerRuntime{
		provider: provider, prepareCalls: &prepareCalls, resumeCalls: &resumeRuntimeCalls,
	}
	worker, err := NewRenditionWorker(RenditionWorkerConfig{
		Catalog: fixture.catalog, Blobs: fixture.blobs,
		Runtime: runtime, Gate: newWorkerTestGate(),
		Owner:         "rendition-worker-test",
		LeaseDuration: time.Minute, IdleDelay: time.Millisecond,
		Clock: func() time.Time { return now },
	})
	require.NoError(t, err)

	processed, err := worker.RunOne(t.Context())
	require.NoError(t, err)
	assert.True(t, processed)
	current, err := fixture.catalog.RenditionJobByID(t.Context(), job.ID)
	require.NoError(t, err)
	assert.Equal(t, store.RenditionJobRetryWait, current.State)
	assert.Equal(t, store.RenditionFailureTransient, current.FailureCode)

	baseProvider.renderErr = nil
	now = now.Add(20 * time.Minute)
	processed, err = worker.RunOne(t.Context())
	require.NoError(t, err)
	assert.True(t, processed)
	assert.Equal(t, []string{"remote-job-1"}, provider.resumes)
	assert.Equal(t, 2, baseProvider.calls)
	assert.Equal(t, 1, prepareCalls,
		"durable resume must not reopen or reseal the original source")
	assert.Equal(t, 1, resumeRuntimeCalls)
	assert.Equal(t, 1, provider.sourceUploads)
	require.Len(t, baseProvider.authorizations, 2)
	assert.Equal(t, baseProvider.authorizations[0], baseProvider.authorizations[1],
		"resume must validate the receipt against the original sealed authorization interval")
	current, err = fixture.catalog.RenditionJobByID(t.Context(), job.ID)
	require.NoError(t, err)
	assert.Equal(t, store.RenditionJobCompleted, current.State)
}

func TestRenditionWorkerFailsClosedWhenConsentIsRevokedBeforeEgress(t *testing.T) {
	fixture := newPublicationFixture(t)
	provider := newWorkerProvider(t)
	profile := workerProcessingProfile(t, provider.Descriptor())
	fixture.profile = profile
	request := workerJobRequest(fixture.versionID, profile, provider.Descriptor())
	grantWorkerConsent(t, fixture.catalog, request)
	job, _, err := fixture.catalog.EnqueueRenditionJob(t.Context(), request)
	require.NoError(t, err)
	_, err = fixture.catalog.RevokeConsent(t.Context(), store.ProcessingConsentRevocationRequest{
		Principal: request.Authorization.Principal,
		Scope:     request.Authorization.Scope,
	})
	require.NoError(t, err)
	now := time.Now().UTC()
	worker, err := NewRenditionWorker(RenditionWorkerConfig{
		Catalog: fixture.catalog, Blobs: fixture.blobs,
		Runtime: workerRuntime{provider: provider}, Gate: newWorkerTestGate(),
		Owner:         "rendition-worker-test",
		LeaseDuration: time.Minute, IdleDelay: time.Millisecond,
		Clock: func() time.Time { return now },
	})
	require.NoError(t, err)

	processed, err := worker.RunOne(t.Context())
	require.NoError(t, err)
	assert.True(t, processed)
	assert.Zero(t, provider.calls)
	current, err := fixture.catalog.RenditionJobByID(t.Context(), job.ID)
	require.NoError(t, err)
	assert.Equal(t, store.RenditionJobFailed, current.State)
	assert.Equal(t, store.RenditionFailureConsent, current.FailureCode)

	grantWorkerConsent(t, fixture.catalog, request)
	requeued, waiter, err := fixture.catalog.EnqueueRenditionJob(t.Context(), request)
	require.NoError(t, err)
	assert.Equal(t, store.RenditionJobQueued, requeued.State)
	assert.Equal(t, "waiting", waiter.State)
	now = time.Now().UTC()
	processed, err = worker.RunOne(t.Context())
	require.NoError(t, err)
	assert.True(t, processed)
	assert.Equal(t, 1, provider.calls)
	current, err = fixture.catalog.RenditionJobByID(t.Context(), job.ID)
	require.NoError(t, err)
	assert.Equal(t, store.RenditionJobCompleted, current.State)
}

func TestRenditionWorkerRejectsPreparedExecutionIdentityDriftBeforeEgress(t *testing.T) {
	fixture := newPublicationFixture(t)
	provider := newWorkerProvider(t)
	profile := workerProcessingProfile(t, provider.Descriptor())
	fixture.profile = profile
	request := workerJobRequest(fixture.versionID, profile, provider.Descriptor())
	grantWorkerConsent(t, fixture.catalog, request)
	job, _, err := fixture.catalog.EnqueueRenditionJob(t.Context(), request)
	require.NoError(t, err)
	now := time.Now().UTC()
	worker, err := NewRenditionWorker(RenditionWorkerConfig{
		Catalog: fixture.catalog, Blobs: fixture.blobs,
		Runtime: driftedWorkerRuntime{provider: provider}, Gate: newWorkerTestGate(),
		Owner: "rendition-worker-drift-test", LeaseDuration: time.Minute,
		IdleDelay: time.Millisecond, Clock: func() time.Time { return now },
	})
	require.NoError(t, err)
	processed, err := worker.RunOne(t.Context())
	require.NoError(t, err)
	assert.True(t, processed)
	assert.Zero(t, provider.calls)
	current, err := fixture.catalog.RenditionJobByID(t.Context(), job.ID)
	require.NoError(t, err)
	assert.Equal(t, store.RenditionJobFailed, current.State)
	assert.Equal(t, store.RenditionFailureTerminal, current.FailureCode)
}

func TestRenditionWorkerReclaimsStagedBuildWithoutCallingProviderAgain(t *testing.T) {
	fixture := newPublicationFixture(t)
	provider := newWorkerProvider(t)
	profile := workerProcessingProfile(t, provider.Descriptor())
	fixture.profile = profile
	request := workerJobRequest(fixture.versionID, profile, provider.Descriptor())
	grantWorkerConsent(t, fixture.catalog, request)
	job, waiter, err := fixture.catalog.EnqueueRenditionJob(t.Context(), request)
	require.NoError(t, err)
	started := time.Now().UTC()
	claim, err := fixture.catalog.ClaimRenditionJob(
		t.Context(), job.ID, "crashed-worker", started, time.Second)
	require.NoError(t, err)
	work, err := fixture.catalog.RenditionJobWorkByClaim(t.Context(), claim, started)
	require.NoError(t, err)
	runtime := workerRuntime{provider: provider}
	execution, err := runtime.Prepare(t.Context(), work, started)
	require.NoError(t, err)
	defer func() { require.NoError(t, execution.Upload.Close()) }()
	snapshot, err := document.SealRenditionExecutionAt(
		started, execution.Provider, execution.Upload, execution.Authorization,
		execution.EvidencePolicy, execution.RenditionPolicy)
	require.NoError(t, err)
	_, err = fixture.catalog.BeginRenditionProvider(
		t.Context(), claim, waiter.ID, started, snapshot)
	require.NoError(t, err)
	result, err := document.RenderRenditionWithResume(
		t.Context(), provider, execution.Upload, execution.Authorization, nil, nil)
	require.NoError(t, err)
	staged, err := buildRenditionJobCandidate(
		work, execution, result, claim.Epoch, started)
	require.NoError(t, err)
	assert.Equal(t, renditionJobGenerationID(job.ID, claim.Epoch), staged.LexicalGenerationID)
	nextEpoch := claim.Epoch + 1
	reclaimedCandidate, err := buildRenditionJobCandidate(
		work, execution, result, nextEpoch, started)
	require.NoError(t, err)
	assert.Equal(t, renditionJobGenerationID(job.ID, nextEpoch),
		reclaimedCandidate.LexicalGenerationID)
	crashedWorker, err := NewRenditionWorker(RenditionWorkerConfig{
		Catalog: fixture.catalog, Blobs: fixture.blobs, Runtime: runtime,
		Gate:  newWorkerTestGate(),
		Owner: "crashed-worker", LeaseDuration: time.Minute, IdleDelay: time.Millisecond,
		Clock: func() time.Time { return started },
	})
	require.NoError(t, err)
	require.NoError(t, crashedWorker.stageArtifactsAndBuild(t.Context(), claim, staged))
	assert.Equal(t, 1, provider.calls)

	reclaimedAt := started.Add(2 * time.Minute)
	recoveredWorker, err := NewRenditionWorker(RenditionWorkerConfig{
		Catalog: fixture.catalog, Blobs: fixture.blobs, Runtime: runtime,
		Gate:  newWorkerTestGate(),
		Owner: "recovered-worker", LeaseDuration: time.Minute, IdleDelay: time.Millisecond,
		Clock: func() time.Time { return reclaimedAt },
	})
	require.NoError(t, err)
	processed, err := recoveredWorker.RunOne(t.Context())
	require.NoError(t, err)
	assert.True(t, processed)
	assert.Equal(t, 1, provider.calls, "a staged immutable build resumes after provider egress")
	current, err := fixture.catalog.RenditionJobByID(t.Context(), job.ID)
	require.NoError(t, err)
	assert.Equal(t, store.RenditionJobCompleted, current.State)
}

func TestRenditionWorkerRefreshesStaleLexicalGenerationWithoutProviderEgress(t *testing.T) {
	fixture := newPublicationFixture(t)
	provider := newWorkerProvider(t)
	profile := workerProcessingProfile(t, provider.Descriptor())
	fixture.profile = profile
	request := workerJobRequest(fixture.versionID, profile, provider.Descriptor())
	grantWorkerConsent(t, fixture.catalog, request)
	job, waiter, err := fixture.catalog.EnqueueRenditionJob(t.Context(), request)
	require.NoError(t, err)
	now := time.Now().UTC()
	claim, err := fixture.catalog.ClaimRenditionJob(
		t.Context(), job.ID, "stale-generation-worker", now, time.Minute)
	require.NoError(t, err)
	runtime := workerRuntime{provider: provider}
	work, err := fixture.catalog.RenditionJobWorkByClaim(t.Context(), claim, now)
	require.NoError(t, err)
	execution, err := runtime.Prepare(t.Context(), work, now)
	require.NoError(t, err)
	defer func() { require.NoError(t, execution.Upload.Close()) }()
	snapshot, err := document.SealRenditionExecutionAt(
		now, execution.Provider, execution.Upload, execution.Authorization,
		execution.EvidencePolicy, execution.RenditionPolicy)
	require.NoError(t, err)
	_, err = fixture.catalog.BeginRenditionProvider(
		t.Context(), claim, waiter.ID, now, snapshot)
	require.NoError(t, err)
	result, err := document.RenderRenditionWithResume(
		t.Context(), provider, execution.Upload, execution.Authorization, nil, nil)
	require.NoError(t, err)
	staged, err := buildRenditionJobCandidate(
		work, execution, result, claim.Epoch, now)
	require.NoError(t, err)
	worker, err := NewRenditionWorker(RenditionWorkerConfig{
		Catalog: fixture.catalog, Blobs: fixture.blobs, Runtime: runtime,
		Gate:  newWorkerTestGate(),
		Owner: "fresh-generation-worker", LeaseDuration: time.Minute,
		IdleDelay: time.Millisecond, Clock: func() time.Time { return now },
	})
	require.NoError(t, err)
	require.NoError(t, worker.stageArtifactsAndBuild(t.Context(), claim, staged))
	staleGenerationID := processingHash("stale-lexical-generation")
	_, err = fixture.catalog.StageRenditionJobGeneration(
		t.Context(), claim, staleGenerationID, now)
	require.NoError(t, err)

	require.NoError(t, worker.classifyPublicationError(
		t.Context(), claim, store.ErrLexicalGenerationStale))
	current, err := fixture.catalog.RenditionJobByID(t.Context(), job.ID)
	require.NoError(t, err)
	assert.Equal(t, store.RenditionJobRetryWait, current.State)
	now = now.Add(31 * time.Second)
	processed, err := worker.RunOne(t.Context())
	require.NoError(t, err)
	assert.True(t, processed)
	assert.Equal(t, 1, provider.calls)
	activeGeneration, err := fixture.catalog.ActiveLexicalGeneration(t.Context())
	require.NoError(t, err)
	assert.NotEqual(t, staleGenerationID, activeGeneration.ID)
}

var workerSourceBytes = []byte("synthetic private-free source")

type workerUpload struct {
	*bytes.Reader

	metadata document.AuthorizedUploadMetadata
}

func (upload *workerUpload) Close() error                                { return nil }
func (upload *workerUpload) Metadata() document.AuthorizedUploadMetadata { return upload.metadata }

type workerRuntime struct{ provider *workerProvider }

type countingWorkerRuntime struct {
	provider     *workerProvider
	prepareCalls *int
}

func (runtime countingWorkerRuntime) Prepare(
	ctx context.Context, work store.RenditionJobWork, now time.Time,
) (RenditionExecution, error) {
	*runtime.prepareCalls++
	return (workerRuntime{provider: runtime.provider}).Prepare(ctx, work, now)
}

type driftedWorkerRuntime struct{ provider *workerProvider }

func (runtime driftedWorkerRuntime) Prepare(
	ctx context.Context, work store.RenditionJobWork, now time.Time,
) (RenditionExecution, error) {
	execution, err := (workerRuntime(runtime)).Prepare(ctx, work, now)
	if err != nil {
		return RenditionExecution{}, err
	}
	upload, ok := execution.Upload.(*workerUpload)
	if !ok {
		return RenditionExecution{}, errors.New("synthetic runtime returned unexpected upload type")
	}
	upload.metadata.Filename = "runtime-drift.pdf"
	return execution, nil
}

func (runtime workerRuntime) Prepare(
	_ context.Context, work store.RenditionJobWork, now time.Time,
) (RenditionExecution, error) {
	digest := sha256.Sum256(workerSourceBytes)
	metadata := document.AuthorizedUploadMetadata{
		Filename: "source.pdf", MediaFamily: "pdf", MediaType: "application/pdf",
		ByteLength: int64(len(workerSourceBytes)), SHA256: hex.EncodeToString(digest[:]),
		CapabilityRecordChecksum: processingHash("worker-capability"),
		ProviderMetadataChecksum: processingHash("worker-provider-metadata"),
		InputKind:                document.RenditionInputOriginalFile,
	}
	authorization := document.RenditionAuthorization{
		ProviderID:                  runtime.provider.descriptor.ID,
		DescriptorFingerprint:       runtime.provider.descriptor.Fingerprint,
		PolicyFingerprint:           runtime.provider.descriptor.PolicyFingerprint,
		RenditionRequestFingerprint: work.Profile.RenditionRequestFingerprint,
		SourceSHA256:                work.Job.SourceSHA256, SourceBytes: metadata.ByteLength,
		CapabilityRecordChecksum: metadata.CapabilityRecordChecksum,
		ProviderMetadataChecksum: metadata.ProviderMetadataChecksum,
		MediaFamily:              metadata.MediaFamily, MediaType: metadata.MediaType,
		InputKind: metadata.InputKind,
		AllowedArtifactRoles: []document.EvidenceArtifactRole{
			document.EvidenceArtifactStructured,
		}, MaxArtifacts: 1,
		MaxProviderMarkdownBytes: 0, MaxArtifactBytes: 1 << 20, MaxTotalResultBytes: 1 << 20,
		AuthorizedAt: now.Format("2006-01-02T15:04:05.000000000Z"),
		ExpiresAt:    now.Add(10 * time.Minute).Format("2006-01-02T15:04:05.000000000Z"),
	}
	evidence, err := document.NewEvidencePolicy(100_000)
	if err != nil {
		return RenditionExecution{}, err
	}
	normalization, err := document.NewNormalizePolicy(100_000)
	if err != nil {
		return RenditionExecution{}, err
	}
	rendition, err := document.NewRenditionPolicy(normalization, 100)
	if err != nil {
		return RenditionExecution{}, err
	}
	return RenditionExecution{
		Provider:      runtime.provider,
		Upload:        &workerUpload{Reader: bytes.NewReader(workerSourceBytes), metadata: metadata},
		Authorization: authorization, EvidencePolicy: evidence, RenditionPolicy: rendition,
	}, nil
}

type resumableWorkerRuntime struct {
	provider     *resumableWorkerProvider
	prepareCalls *int
	resumeCalls  *int
}

func (runtime resumableWorkerRuntime) Prepare(
	ctx context.Context, work store.RenditionJobWork, now time.Time,
) (RenditionExecution, error) {
	if runtime.prepareCalls != nil {
		*runtime.prepareCalls++
	}
	execution, err := (workerRuntime{provider: runtime.provider.workerProvider}).Prepare(ctx, work, now)
	execution.Provider = runtime.provider
	return execution, err
}

func (runtime resumableWorkerRuntime) ResumeProvider(
	_ context.Context, _ store.RenditionJobWork, _ document.RenditionExecutionSnapshotV1,
) (document.RenditionProvider, error) {
	if runtime.resumeCalls != nil {
		*runtime.resumeCalls++
	}
	return runtime.provider, nil
}

type workerProvider struct {
	descriptor     document.RenditionDescriptor
	renderErr      error
	calls          int
	authorizations []document.RenditionAuthorization
}

type resumableWorkerProvider struct {
	*workerProvider

	resumes       []string
	sourceUploads int
}

func (provider *resumableWorkerProvider) RenderResumable(
	ctx context.Context, upload document.AuthorizedUpload,
	authorization document.RenditionAuthorization, resume *document.RenditionResumeHandle,
	checkpoint document.RenditionResumeCheckpoint,
) (document.RenditionResult, error) {
	if resume == nil {
		if _, err := io.Copy(io.Discard, upload); err != nil {
			return document.RenditionResult{}, err
		}
		provider.sourceUploads++
		if err := checkpoint(document.RenditionResumeHandle{Value: "remote-job-1"}); err != nil {
			return document.RenditionResult{}, err
		}
	} else {
		provider.resumes = append(provider.resumes, resume.Value)
	}
	return provider.Render(ctx, upload, authorization)
}

type failOnceRenditionBlobWriter struct {
	delegate *blob.Store
	failed   bool
}

var errSyntheticTransientCatalog = errors.New("synthetic transient catalog failure")

type transientRenditionCatalog struct {
	*store.Store

	claimFailures       atomic.Int32
	recordFailures      atomic.Int32
	claimAttempts       atomic.Int32
	recordAttempts      atomic.Int32
	renewalAttempts     atomic.Int32
	failClaimsForever   bool
	failRecordsForever  bool
	failRenewalsForever bool
}

func (catalog *transientRenditionCatalog) ClaimNextRenditionJob(
	ctx context.Context, owner string, at time.Time, lease time.Duration,
) (store.RenditionJobClaim, bool, error) {
	catalog.claimAttempts.Add(1)
	if catalog.failClaimsForever || catalog.claimFailures.Add(-1) >= 0 {
		return store.RenditionJobClaim{}, false, errSyntheticTransientCatalog
	}
	return catalog.Store.ClaimNextRenditionJob(ctx, owner, at, lease)
}

func (catalog *transientRenditionCatalog) RecordRenditionBlob(
	ctx context.Context, hash string, size int64, physical store.BlobPhysical,
) error {
	catalog.recordAttempts.Add(1)
	if catalog.failRecordsForever || catalog.recordFailures.Add(-1) >= 0 {
		return errSyntheticTransientCatalog
	}
	return catalog.Store.RecordRenditionBlob(ctx, hash, size, physical)
}

func (catalog *transientRenditionCatalog) RenewRenditionJobClaim(
	ctx context.Context, claim store.RenditionJobClaim, at time.Time, lease time.Duration,
) (store.RenditionJobClaim, error) {
	catalog.renewalAttempts.Add(1)
	if catalog.failRenewalsForever {
		return store.RenditionJobClaim{}, errSyntheticTransientCatalog
	}
	return catalog.Store.RenewRenditionJobClaim(ctx, claim, at, lease)
}

func (catalog *transientRenditionCatalog) RenditionJobErrorRetryable(err error) bool {
	return errors.Is(err, errSyntheticTransientCatalog) ||
		catalog.Store.RenditionJobErrorRetryable(err)
}

func (writer *failOnceRenditionBlobWriter) WriteDetailedContext(
	ctx context.Context, reader io.Reader,
) (blob.WriteReceipt, error) {
	if !writer.failed {
		writer.failed = true
		return blob.WriteReceipt{}, errors.New("synthetic local staging failure")
	}
	return writer.delegate.WriteDetailedContext(ctx, reader)
}

func (writer *failOnceRenditionBlobWriter) WithMutation(
	ctx context.Context, fn func() error,
) error {
	return writer.delegate.WithMutation(ctx, fn)
}

func newWorkerProvider(t *testing.T) *workerProvider {
	t.Helper()
	descriptor, err := document.NewRenditionDescriptor(document.RenditionDescriptor{
		ID: "synthetic.worker-v1", ContractVersion: document.RenditionProviderContractVersion,
		PolicyFingerprint: processingHash("worker-policy"),
		TrustBoundary:     document.RenditionTrustLocalProcess,
		SupportedFormats: []document.RenditionFormatCapability{{
			MediaFamily: "pdf", MediaType: "application/pdf", InputKind: document.RenditionInputOriginalFile,
		}},
		ReturnsStructured: true,
		ArtifactRoles:     []document.EvidenceArtifactRole{document.EvidenceArtifactStructured},
	})
	require.NoError(t, err)
	return &workerProvider{descriptor: descriptor}
}

func (provider *workerProvider) Descriptor() document.RenditionDescriptor {
	return provider.descriptor
}

func (provider *workerProvider) Render(
	_ context.Context, upload document.AuthorizedUpload, authorization document.RenditionAuthorization,
) (document.RenditionResult, error) {
	provider.calls++
	provider.authorizations = append(provider.authorizations, authorization)
	if provider.renderErr != nil {
		return document.RenditionResult{}, provider.renderErr
	}
	if upload != nil {
		if _, err := io.Copy(io.Discard, upload); err != nil {
			return document.RenditionResult{}, err
		}
	}
	authorizationFingerprint, err := authorization.Fingerprint()
	if err != nil {
		return document.RenditionResult{}, err
	}
	now := time.Now().UTC()
	return document.RenditionResult{
		Evidence: document.SourceEvidenceV1{
			ContractVersion: document.SourceEvidenceContractV1,
			Completeness:    document.EvidenceDegradedProvenance,
			Family:          "pdf", UnitKind: document.EvidenceUnitGeneric,
			Omissions: []document.SourceEvidenceOmissionV1{{
				Kind: document.EvidenceOmissionField, Field: "natural_provenance",
				Reason: "synthetic provider exposes generic provenance",
			}},
			Units: []document.SourceEvidenceUnitV1{{
				Order: 0, Text: "Synthetic worker output",
				Locator: document.SourceEvidenceLocatorV1{
					Kind:        document.EvidenceLocatorGeneric,
					IndexOrigin: document.EvidenceIndexOriginNone,
				},
			}},
		},
		Receipt: document.RenditionReceipt{
			ProviderID:                  provider.descriptor.ID,
			DescriptorFingerprint:       provider.descriptor.Fingerprint,
			PolicyFingerprint:           provider.descriptor.PolicyFingerprint,
			RenditionRequestFingerprint: authorization.RenditionRequestFingerprint,
			AuthorizationFingerprint:    authorizationFingerprint,
			SourceSHA256:                authorization.SourceSHA256, OperationID: "synthetic-operation",
			StartedAt:   now.Format("2006-01-02T15:04:05.000000000Z"),
			CompletedAt: now.Format("2006-01-02T15:04:05.000000000Z"),
			Usage:       document.RenditionUsage{Requests: 1, InputBytes: authorization.SourceBytes},
		},
	}, nil
}

func workerProcessingProfile(
	t *testing.T, descriptor document.RenditionDescriptor,
) store.ProcessingProfileRecord {
	t.Helper()
	profile := document.ProcessingProfileV1{
		ContractVersion: document.ProcessingProfileContractV1,
		Rendition: &document.RenditionBindingV1{
			AdapterContract:          "rendition-adapter/v1",
			AuthorizationFingerprint: processingHash("worker-authorization"),
			CredentialBinding:        "credential:synthetic",
			DeploymentFingerprint:    processingHash("worker-deployment"),
			Descriptor: document.ProviderDescriptorV1{
				ID: descriptor.ID, Fingerprint: descriptor.Fingerprint,
			},
			DisclosureFingerprint: processingHash("worker-disclosure"),
			MaxDocumentBytes:      1 << 20, MaxResponseBytes: 1 << 20, MaxUnits: 100,
			Name: "primary", RequestedArtifacts: []document.EvidenceArtifactRole{
				document.EvidenceArtifactStructured,
			}, TrustBoundary: "synthetic-vault",
			UploadOptionsFingerprint: processingHash("worker-upload"),
		},
		EvidenceLexical: document.EvidenceLexicalPolicyV1{
			CompletenessFingerprint:     processingHash("worker-completeness"),
			LexicalSegmenterFingerprint: processingHash("worker-segmenter"),
			MaxSegmentRunes:             100, MaxUnitRunes: 1000,
			NormalizedEvidenceContract: document.NormalizedEvidenceContractV1,
			NormalizerFingerprint:      processingHash("worker-normalizer"),
			RenditionContract:          document.RenditionContractV1,
			SanitizerFingerprint:       processingHash("worker-sanitizer"),
			SourceEvidenceContract:     document.SourceEvidenceContractV1,
		},
		Retrieval: document.RetrievalPolicyV1{LexicalLimit: 100, VectorLimit: 100},
		RetentionDisclosure: document.RetentionDisclosurePolicyV1{
			AttachmentPolicyFingerprint: processingHash("worker-attachment-policy"),
			ConsentFingerprint:          processingHash("worker-consent"),
			RetainSanitizedMarkdown:     true, TrustBoundary: "synthetic-vault",
		},
	}
	canonical, fingerprints, err := document.CanonicalProfile(profile)
	require.NoError(t, err)
	return store.ProcessingProfileRecord{
		Fingerprint: fingerprints.Profile, CanonicalProfile: jsontext.Value(canonical),
		RenditionRequestFingerprint:    fingerprints.RenditionRequest,
		EvidenceLexicalFingerprint:     fingerprints.EvidenceLexical,
		RetentionDisclosureFingerprint: fingerprints.RetentionDisclosure,
		AttachmentPolicyFingerprint:    profile.RetentionDisclosure.AttachmentPolicyFingerprint,
		ConsentFingerprint:             profile.RetentionDisclosure.ConsentFingerprint,
		RenditionDisclosureFingerprint: profile.Rendition.DisclosureFingerprint,
		TrustBoundary:                  profile.RetentionDisclosure.TrustBoundary,
	}
}

func workerJobRequest(
	versionID string, profile store.ProcessingProfileRecord, descriptor document.RenditionDescriptor,
) store.RenditionJobRequest {
	digest := sha256.Sum256(workerSourceBytes)
	metadata := document.AuthorizedUploadMetadata{
		Filename: "source.pdf", MediaFamily: "pdf", MediaType: "application/pdf",
		ByteLength: int64(len(workerSourceBytes)), SHA256: hex.EncodeToString(digest[:]),
		CapabilityRecordChecksum: processingHash("worker-capability"),
		ProviderMetadataChecksum: processingHash("worker-provider-metadata"),
		InputKind:                document.RenditionInputOriginalFile,
	}
	authorization := document.RenditionAuthorization{
		ProviderID: descriptor.ID, DescriptorFingerprint: descriptor.Fingerprint,
		PolicyFingerprint:           descriptor.PolicyFingerprint,
		RenditionRequestFingerprint: profile.RenditionRequestFingerprint,
		SourceSHA256:                metadata.SHA256, SourceBytes: metadata.ByteLength,
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
	executionIdentity, err := document.NewRenditionExecutionIdentityV1(
		metadata, authorization, evidence, rendition)
	if err != nil {
		panic(err)
	}
	return store.RenditionJobRequest{
		ContentVersionID: versionID, Profile: profile,
		ExecutionIdentity: executionIdentity,
		CapturedArtifactPolicy: jsontext.Value(
			`{"roles":[{"max_count":1,"min_count":1,"role":"normalized_evidence"},{"max_count":1,"min_count":1,"role":"sanitized_markdown"}],"version":1}`),
		Authorization: store.ProviderOperationAuthorizationRequest{
			Principal: "operator:synthetic", Scope: "document-processing",
			ProfileFingerprint:      profile.Fingerprint,
			DisclosureFingerprint:   profile.RenditionDisclosureFingerprint,
			InputClasses:            []string{"original_file"},
			RetainedArtifactClasses: []string{"normalized_evidence", "sanitized_markdown"},
		},
	}
}

func grantWorkerConsent(t *testing.T, catalog *store.Store, request store.RenditionJobRequest) {
	t.Helper()
	_, err := catalog.GrantConsent(t.Context(), store.ProcessingConsentGrantRequest{
		Principal: request.Authorization.Principal, Scope: request.Authorization.Scope,
		ProfileFingerprint:      request.Authorization.ProfileFingerprint,
		DisclosureFingerprint:   request.Authorization.DisclosureFingerprint,
		InputClasses:            request.Authorization.InputClasses,
		RetainedArtifactClasses: request.Authorization.RetainedArtifactClasses,
	})
	require.NoError(t, err)
}

func workerProviderError(t *testing.T, code document.RenditionErrorCode) error {
	t.Helper()
	err, makeErr := document.NewRenditionProviderError(code, "synthetic failure", 0, errors.New("private cause"))
	require.NoError(t, makeErr)
	return err
}

var _ io.ReadCloser = (*workerUpload)(nil)

func TestRenditionWorkerReceiptEncodingIsDeterministic(t *testing.T) {
	receipt := document.RenditionReceipt{ProviderID: "synthetic", OperationID: "operation"}
	first, err := json.Marshal(receipt, json.Deterministic(true))
	require.NoError(t, err)
	second, err := json.Marshal(receipt, json.Deterministic(true))
	require.NoError(t, err)
	assert.Equal(t, first, second)
}

func TestRenditionWorkerRejectsTypedNilRuntime(t *testing.T) {
	fixture := newPublicationFixture(t)
	var runtime *RenditionRuntimeRegistry

	assert.NotPanics(t, func() {
		_, err := NewRenditionWorker(RenditionWorkerConfig{
			Catalog: fixture.catalog, Blobs: fixture.blobs, Runtime: runtime,
			Gate:  newWorkerTestGate(),
			Owner: "rendition-worker-test", LeaseDuration: time.Minute,
			IdleDelay: time.Millisecond,
		})
		require.ErrorContains(t, err, "requires catalog, blob store, runtime, and operation gate")
	})
}

func TestRenditionRuntimeRegistryStartsWorkerOnlyAfterRegistration(t *testing.T) {
	registry := NewRenditionRuntimeRegistry()
	assert.False(t, registry.Ready(),
		"a restored queue must remain dormant until a provider adapter is available")

	provider := newWorkerProvider(t)
	require.NoError(t, registry.Register(
		provider.Descriptor().Fingerprint, workerRuntime{provider: provider}))
	assert.True(t, registry.Ready())
}
