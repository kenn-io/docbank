package processing

import (
	"bytes"
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/store"
)

func TestRenditionWorkerRunJobPublishesOnlyExactJob(t *testing.T) {
	fixture := newPublicationFixture(t)
	provider := newWorkerProvider(t)
	profile := workerProcessingProfile(t, provider.Descriptor())
	fixture.profile = profile
	targetRequest := workerJobRequest(fixture.versionID, profile, provider.Descriptor())
	unrelatedRequest := targetedUnrelatedRenditionRequest(t, fixture, targetRequest)
	grantWorkerConsent(t, fixture.catalog, unrelatedRequest)
	unrelated, _, err := fixture.catalog.EnqueueRenditionJob(t.Context(), unrelatedRequest)
	require.NoError(t, err)
	grantWorkerConsent(t, fixture.catalog, targetRequest)
	target, _, err := fixture.catalog.EnqueueRenditionJob(t.Context(), targetRequest)
	require.NoError(t, err)
	worker := newTargetedRenditionWorker(t, fixture, provider, api.NewOperationGate(), time.Now)

	processed, err := worker.RunJob(t.Context(), target.ID)
	require.NoError(t, err)
	require.True(t, processed)
	require.Equal(t, 1, provider.calls)

	current, err := fixture.catalog.RenditionJobByID(t.Context(), target.ID)
	require.NoError(t, err)
	assert.Equal(t, store.RenditionJobCompleted, current.State)
	view, err := fixture.catalog.ActiveRendition(
		t.Context(), targetRequest.ContentVersionID, profile.Fingerprint)
	require.NoError(t, err)
	assert.Equal(t, target.ID, view.Build.ID)
	assert.Equal(t, targetRequest.ExecutionIdentity.Upload.SHA256, view.Build.SourceSHA256)

	unrelatedCurrent, err := fixture.catalog.RenditionJobByID(t.Context(), unrelated.ID)
	require.NoError(t, err)
	assert.Equal(t, store.RenditionJobQueued, unrelatedCurrent.State)
	assert.Zero(t, unrelatedCurrent.ClaimEpoch)
}

func TestRenditionWorkerRunJobEntryAndClaimErrors(t *testing.T) {
	t.Run("nil worker", func(t *testing.T) {
		var worker *RenditionWorker
		processed, err := worker.RunJob(t.Context(), processingHash("nil-rendition-target"))
		require.EqualError(t, err, "rendition worker is nil")
		assert.False(t, processed)
	})

	t.Run("targeted interface unavailable", func(t *testing.T) {
		fixture := newPublicationFixture(t)
		provider := newWorkerProvider(t)
		profile := workerProcessingProfile(t, provider.Descriptor())
		fixture.profile = profile
		worker := newTargetedRenditionWorker(t, fixture, provider, api.NewOperationGate(), time.Now)
		worker.catalog = renditionCatalogWithoutTarget{renditionWorkerCatalog: fixture.catalog}
		processed, err := worker.RunJob(t.Context(), processingHash("unsupported-rendition-target"))
		require.EqualError(t, err, "rendition catalog does not support targeted claims")
		assert.False(t, processed)
		assert.Zero(t, provider.calls)
	})

	for _, testCase := range []struct {
		name    string
		jobID   func(store.RenditionJob) string
		setup   func(*testing.T, publicationFixture, *RenditionWorker, store.RenditionJob)
		wantErr error
	}{
		{
			name: "absent", jobID: func(store.RenditionJob) string {
				return processingHash("absent-rendition-target")
			}, wantErr: store.ErrNotFound,
		},
		{
			name: "malformed", jobID: func(store.RenditionJob) string { return "not-a-job-id" },
		},
		{
			name: "active lease", wantErr: store.ErrRenditionJobLeaseHeld,
			setup: func(t *testing.T, fixture publicationFixture, worker *RenditionWorker,
				job store.RenditionJob,
			) {
				t.Helper()
				_, err := fixture.catalog.ClaimRenditionJob(t.Context(), job.ID,
					"active-target-worker", worker.clock().UTC(), time.Minute)
				require.NoError(t, err)
			},
		},
		{
			name: "delayed", wantErr: store.ErrRenditionJobLeaseHeld,
			setup: func(t *testing.T, fixture publicationFixture, worker *RenditionWorker,
				job store.RenditionJob,
			) {
				t.Helper()
				now := worker.clock().UTC()
				claim, err := fixture.catalog.ClaimRenditionJob(t.Context(), job.ID,
					"delayed-target-worker", now, time.Minute)
				require.NoError(t, err)
				require.NoError(t, fixture.catalog.MarkRenditionJobRetry(t.Context(), claim,
					store.RenditionFailureTransient, now.Add(time.Second), now.Add(time.Hour)))
			},
		},
		{
			name: "terminal", wantErr: store.ErrRenditionJobTerminal,
			setup: func(t *testing.T, fixture publicationFixture, worker *RenditionWorker,
				job store.RenditionJob,
			) {
				t.Helper()
				now := worker.clock().UTC()
				claim, err := fixture.catalog.ClaimRenditionJob(t.Context(), job.ID,
					"terminal-target-worker", now, time.Minute)
				require.NoError(t, err)
				require.NoError(t, fixture.catalog.MarkRenditionJobFailed(t.Context(), claim,
					store.RenditionFailureTerminal, now.Add(time.Second)))
			},
		},
		{
			name: "operator required", wantErr: store.ErrRenditionJobOperatorRequired,
			setup: func(t *testing.T, fixture publicationFixture, worker *RenditionWorker,
				job store.RenditionJob,
			) {
				t.Helper()
				now := worker.clock().UTC()
				claim, err := fixture.catalog.ClaimRenditionJob(t.Context(), job.ID,
					"operator-target-worker", now, time.Minute)
				require.NoError(t, err)
				require.NoError(t, fixture.catalog.MarkRenditionJobOperatorRequired(
					t.Context(), claim, now.Add(time.Second)))
			},
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			fixture := newPublicationFixture(t)
			provider := newWorkerProvider(t)
			profile := workerProcessingProfile(t, provider.Descriptor())
			fixture.profile = profile
			request := workerJobRequest(fixture.versionID, profile, provider.Descriptor())
			grantWorkerConsent(t, fixture.catalog, request)
			job, _, err := fixture.catalog.EnqueueRenditionJob(t.Context(), request)
			require.NoError(t, err)
			now := time.Now().UTC().Add(time.Minute)
			worker := newTargetedRenditionWorker(t, fixture, provider,
				api.NewOperationGate(), func() time.Time { return now })
			if testCase.setup != nil {
				testCase.setup(t, fixture, worker, job)
			}
			jobID := job.ID
			if testCase.jobID != nil {
				jobID = testCase.jobID(job)
			}

			processed, runErr := worker.RunJob(t.Context(), jobID)
			require.Error(t, runErr)
			if testCase.wantErr != nil {
				require.ErrorIs(t, runErr, testCase.wantErr)
				require.False(t, isRenditionWorkerFatal(runErr),
					"expected exact-target conditions are not fatal worker faults")
			}
			assert.False(t, processed)
			assert.Zero(t, provider.calls)
		})
	}

	t.Run("fenced identity is preserved", func(t *testing.T) {
		fixture := newPublicationFixture(t)
		provider := newWorkerProvider(t)
		profile := workerProcessingProfile(t, provider.Descriptor())
		fixture.profile = profile
		worker := newTargetedRenditionWorker(t, fixture, provider, api.NewOperationGate(), time.Now)
		worker.catalog = renditionTargetErrorCatalog{
			renditionWorkerCatalog: fixture.catalog, err: store.ErrRenditionJobFenced,
		}
		processed, err := worker.RunJob(t.Context(), processingHash("fenced-rendition-target"))
		require.ErrorIs(t, err, store.ErrRenditionJobFenced)
		require.False(t, isRenditionWorkerFatal(err))
		assert.False(t, processed)
		assert.Zero(t, provider.calls)
	})
}

func TestRenditionWorkerRunJobDoesNotFallbackToQueuedWork(t *testing.T) {
	fixture := newPublicationFixture(t)
	provider := newWorkerProvider(t)
	profile := workerProcessingProfile(t, provider.Descriptor())
	fixture.profile = profile
	request := workerJobRequest(fixture.versionID, profile, provider.Descriptor())
	grantWorkerConsent(t, fixture.catalog, request)
	queued, _, err := fixture.catalog.EnqueueRenditionJob(t.Context(), request)
	require.NoError(t, err)
	worker := newTargetedRenditionWorker(t, fixture, provider, api.NewOperationGate(), time.Now)

	processed, err := worker.RunJob(t.Context(), processingHash("valid-absent-rendition-job"))
	require.ErrorIs(t, err, store.ErrNotFound)
	require.False(t, processed)
	require.Zero(t, provider.calls)
	current, err := fixture.catalog.RenditionJobByID(t.Context(), queued.ID)
	require.NoError(t, err)
	assert.Equal(t, store.RenditionJobQueued, current.State)
	assert.Zero(t, current.ClaimEpoch)
}

func TestRenditionWorkerRunJobHonorsCanceledAndBlockedAdmission(t *testing.T) {
	t.Run("canceled entry", func(t *testing.T) {
		fixture, provider, worker, job := targetedRenditionJobFixture(t, api.NewOperationGate())
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		processed, err := worker.RunJob(ctx, job.ID)
		require.ErrorIs(t, err, context.Canceled)
		require.False(t, processed)
		require.Zero(t, provider.calls)
		current, currentErr := fixture.catalog.RenditionJobByID(t.Context(), job.ID)
		require.NoError(t, currentErr)
		assert.Equal(t, store.RenditionJobQueued, current.State)
		assert.Zero(t, current.ClaimEpoch)
	})

	t.Run("blocked admission", func(t *testing.T) {
		gate := api.NewOperationGate()
		fixture, provider, worker, job := targetedRenditionJobFixture(t, gate)
		entered := make(chan struct{})
		worker.gate = &targetedObservedGate{OperationGate: gate, entered: entered}
		held := make(chan struct{})
		release := make(chan struct{})
		maintenanceDone := make(chan error, 1)
		var releaseOnce sync.Once
		releaseMaintenance := func() { releaseOnce.Do(func() { close(release) }) }
		maintenanceJoined := false
		t.Cleanup(func() {
			releaseMaintenance()
			if !maintenanceJoined {
				if _, joined := targetedReceiveWithin(maintenanceDone, 5*time.Second); !joined {
					t.Errorf("maintenance goroutine did not join within five seconds")
				}
			}
		})
		go func() {
			maintenanceDone <- gate.MaintainContext(t.Context(), func() error {
				close(held)
				<-release
				return nil
			})
		}()
		select {
		case <-held:
		case <-time.After(5 * time.Second):
			releaseMaintenance()
			require.FailNow(t, "maintenance did not acquire the gate")
		}

		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		done := make(chan struct {
			processed bool
			err       error
		}, 1)
		workerJoined := false
		t.Cleanup(cancel)
		t.Cleanup(func() {
			cancel()
			releaseMaintenance()
			if !workerJoined {
				if _, joined := targetedReceiveWithin(done, 5*time.Second); !joined {
					t.Errorf("rendition worker goroutine did not join within five seconds")
				}
			}
		})
		go func() {
			processed, runErr := worker.RunJob(ctx, job.ID)
			done <- struct {
				processed bool
				err       error
			}{processed, runErr}
		}()
		select {
		case <-entered:
		case <-ctx.Done():
			cancel()
			releaseMaintenance()
			require.FailNow(t, "targeted rendition worker did not enter blocked admission")
		}
		cancel()
		result, joined := targetedReceiveWithin(done, 5*time.Second)
		require.True(t, joined, "rendition worker did not stop within five seconds")
		workerJoined = true
		releaseMaintenance()
		maintenanceErr, joined := targetedReceiveWithin(maintenanceDone, 5*time.Second)
		require.True(t, joined, "maintenance did not stop within five seconds")
		maintenanceJoined = true
		require.NoError(t, maintenanceErr)
		require.ErrorIs(t, result.err, context.Canceled)
		require.False(t, result.processed)
		require.Zero(t, provider.calls)
		current, err := fixture.catalog.RenditionJobByID(t.Context(), job.ID)
		require.NoError(t, err)
		assert.Equal(t, store.RenditionJobQueued, current.State)
		assert.Zero(t, current.ClaimEpoch)
	})
}

func TestRenditionWorkerRunJobReleasesOperationGateDuringProvider(t *testing.T) {
	gate := api.NewOperationGate()
	fixture, provider, worker, job := targetedRenditionJobFixture(t, gate)
	provider.renderStarted = make(chan struct{})
	provider.renderRelease = make(chan struct{})
	var releaseOnce sync.Once
	releaseProvider := func() { releaseOnce.Do(func() { close(provider.renderRelease) }) }
	t.Cleanup(releaseProvider)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	t.Cleanup(cancel)
	done := make(chan error, 1)
	joined := false
	t.Cleanup(func() {
		releaseProvider()
		cancel()
		if !joined {
			if _, stopped := targetedReceiveWithin(done, 5*time.Second); !stopped {
				t.Errorf("rendition provider worker did not join within five seconds")
			}
		}
	})
	go func() { _, runErr := worker.RunJob(ctx, job.ID); done <- runErr }()
	select {
	case <-provider.renderStarted:
	case <-ctx.Done():
		releaseProvider()
		cancel()
		require.FailNow(t, "targeted rendition provider did not start")
	}

	maintenanceErr := gate.MaintainContext(ctx, func() error { return nil })
	mutationErr := gate.MutateContext(ctx, func() error { return nil })
	releaseProvider()
	var workerErr error
	select {
	case workerErr = <-done:
		joined = true
	case <-ctx.Done():
		cancel()
		require.FailNow(t, "targeted rendition worker did not stop")
	}
	require.NoError(t, maintenanceErr)
	require.NoError(t, mutationErr)
	require.NoError(t, workerErr)
	require.Equal(t, 1, provider.calls)
	current, err := fixture.catalog.RenditionJobByID(t.Context(), job.ID)
	require.NoError(t, err)
	assert.Equal(t, store.RenditionJobCompleted, current.State)
}

type renditionCatalogWithoutTarget struct{ renditionWorkerCatalog }

type renditionTargetErrorCatalog struct {
	renditionWorkerCatalog

	err error
}

func (catalog renditionTargetErrorCatalog) ClaimRenditionJob(
	context.Context, string, string, time.Time, time.Duration,
) (store.RenditionJobClaim, error) {
	return store.RenditionJobClaim{}, catalog.err
}

func targetedRenditionJobFixture(
	t *testing.T, gate *api.OperationGate,
) (publicationFixture, *workerProvider, *RenditionWorker, store.RenditionJob) {
	t.Helper()
	fixture := newPublicationFixture(t)
	provider := newWorkerProvider(t)
	profile := workerProcessingProfile(t, provider.Descriptor())
	fixture.profile = profile
	request := workerJobRequest(fixture.versionID, profile, provider.Descriptor())
	grantWorkerConsent(t, fixture.catalog, request)
	job, _, err := fixture.catalog.EnqueueRenditionJob(t.Context(), request)
	require.NoError(t, err)
	worker := newTargetedRenditionWorker(t, fixture, provider, gate, time.Now)
	return fixture, provider, worker, job
}

func newTargetedRenditionWorker(
	t *testing.T, fixture publicationFixture, provider *workerProvider,
	gate *api.OperationGate, clock func() time.Time,
) *RenditionWorker {
	t.Helper()
	worker, err := NewRenditionWorker(RenditionWorkerConfig{
		Catalog: fixture.catalog, Blobs: fixture.blobs,
		Runtime: workerRuntime{provider: provider}, Gate: gate,
		Owner: "targeted-rendition-worker", LeaseDuration: time.Minute,
		IdleDelay: time.Millisecond, Clock: clock,
	})
	require.NoError(t, err)
	return worker
}

func targetedUnrelatedRenditionRequest(
	t *testing.T, fixture publicationFixture, request store.RenditionJobRequest,
) store.RenditionJobRequest {
	t.Helper()
	payload := []byte("synthetic unrelated rendition source")
	receipt, err := fixture.blobs.WriteDetailedContext(t.Context(), bytes.NewReader(payload))
	require.NoError(t, err)
	node, err := fixture.catalog.CreateFile(t.Context(), fixture.catalog.RootID(),
		"unrelated-source.pdf", receipt.Hash, receipt.Size, "application/pdf",
		processingBlobPhysical(t, receipt))
	require.NoError(t, err)
	request.ContentVersionID = node.CurrentVersionID
	request.ExecutionIdentity.Upload.Filename = "unrelated-source.pdf"
	request.ExecutionIdentity.Upload.SHA256 = receipt.Hash
	request.ExecutionIdentity.Upload.ByteLength = receipt.Size
	request.ExecutionIdentity.Authorization.SourceSHA256 = receipt.Hash
	request.ExecutionIdentity.Authorization.SourceBytes = receipt.Size
	return request
}
