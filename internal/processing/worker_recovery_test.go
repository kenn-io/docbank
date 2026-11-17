package processing

import (
	"context"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/docling"
	"go.kenn.io/docbank/document/workerprotocol"
	"go.kenn.io/docbank/internal/store"
)

type retryExpiryRuntime struct{ provider *workerProvider }

func (r retryExpiryRuntime) Prepare(ctx context.Context, work store.RenditionJobWork, now time.Time) (RenditionExecution, error) {
	execution, err := workerRuntime(r).Prepare(ctx, work, now)
	if r.provider.calls == 0 {
		execution.Authorization.ExpiresAt = now.Add(time.Second).Format("2006-01-02T15:04:05.000000000Z")
	}
	return execution, err
}

func TestWorkerRetriesExpiredAuthorization(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		fixture := newPublicationFixture(t)
		provider := newWorkerProvider(t)
		provider.renderStarted = make(chan struct{})
		provider.renderRelease = make(chan struct{})
		descriptor, err := docling.WorkerDocumentDescriptor()
		require.NoError(t, err)
		provider.descriptor = descriptor
		profile := workerProcessingProfile(t, descriptor, func(p *document.ProcessingProfileV1) {
			p.Rendition.AdapterContract = workerprotocol.DocumentAdapter
			p.Rendition.TrustBoundary = string(document.RenditionTrustOperatorNetwork)
			p.RetentionDisclosure.TrustBoundary = string(document.RenditionTrustOperatorNetwork)
		})
		fixture.profile = profile
		request := workerJobRequest(fixture.versionID, profile, provider.Descriptor())
		grantWorkerConsent(t, fixture.catalog, request)
		job, _, err := fixture.catalog.EnqueueRenditionJob(t.Context(), request)
		require.NoError(t, err)
		worker, err := NewRenditionWorker(RenditionWorkerConfig{Catalog: fixture.catalog, Blobs: fixture.blobs, Runtime: retryExpiryRuntime{provider: provider}, Gate: newTestOperationGate(), Owner: "worker-expiry", LeaseDuration: time.Minute, IdleDelay: time.Millisecond})
		require.NoError(t, err)
		type runResult struct {
			processed bool
			err       error
		}
		done := make(chan runResult, 1)
		go func() {
			processed, err := worker.RunOne(t.Context())
			done <- runResult{processed: processed, err: err}
		}()
		<-provider.renderStarted
		time.Sleep(2 * time.Second)
		provider.renderErr = document.ErrRenditionAuthorizationExpired
		close(provider.renderRelease)
		first := <-done
		require.NoError(t, first.err)
		require.True(t, first.processed)
		provider.renderStarted, provider.renderRelease, provider.renderErr = nil, nil, nil
		current, err := fixture.catalog.RenditionJobByID(t.Context(), job.ID)
		require.NoError(t, err)
		require.Equal(t, store.RenditionJobRetryWait, current.State)
		time.Sleep(31 * time.Second)
		processed, err := worker.RunOne(t.Context())
		require.NoError(t, err)
		require.True(t, processed)
		current, err = fixture.catalog.RenditionJobByID(t.Context(), job.ID)
		require.NoError(t, err)
		require.Equal(t, store.RenditionJobCompleted, current.State)
	})
}
