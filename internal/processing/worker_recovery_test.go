package processing

import (
	"context"
	"io"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/docling"
	"go.kenn.io/docbank/document/workerprotocol"
	"go.kenn.io/docbank/internal/store"
)

type expiringWorkerProvider struct {
	*workerProvider
	expired bool
}

func (p *expiringWorkerProvider) Render(ctx context.Context, upload document.AuthorizedUpload, authorization document.RenditionAuthorization) (document.RenditionResult, error) {
	if !p.expired {
		p.expired = true
		time.Sleep(2 * time.Second)
		return document.RenditionResult{}, document.ErrRenditionAuthorizationExpired
	}
	if _, err := io.Copy(io.Discard, upload); err != nil {
		return document.RenditionResult{}, err
	}
	return p.workerProvider.Render(ctx, upload, authorization)
}

func (p *expiringWorkerProvider) RenderResumable(ctx context.Context, upload document.AuthorizedUpload, authorization document.RenditionAuthorization, _ *document.RenditionResumeHandle, _ document.RenditionResumeCheckpoint) (document.RenditionResult, error) {
	return p.Render(ctx, upload, authorization)
}

type retryExpiryRuntime struct{ provider *expiringWorkerProvider }

func (r retryExpiryRuntime) Prepare(ctx context.Context, work store.RenditionJobWork, now time.Time) (RenditionExecution, error) {
	execution, err := (workerRuntime{provider: r.provider.workerProvider}).Prepare(ctx, work, now)
	execution.Provider = r.provider
	if !r.provider.expired {
		execution.Authorization.ExpiresAt = now.Add(time.Second).Format("2006-01-02T15:04:05.000000000Z")
	}
	return execution, err
}

func TestWorkerRetriesExpiredAuthorization(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		fixture := newPublicationFixture(t)
		provider := &expiringWorkerProvider{workerProvider: newWorkerProvider(t)}
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
		processed, err := worker.RunOne(t.Context())
		require.NoError(t, err)
		require.True(t, processed)
		current, err := fixture.catalog.RenditionJobByID(t.Context(), job.ID)
		require.NoError(t, err)
		require.Equal(t, store.RenditionJobRetryWait, current.State)
		time.Sleep(31 * time.Second)
		processed, err = worker.RunOne(t.Context())
		require.NoError(t, err)
		require.True(t, processed)
		current, err = fixture.catalog.RenditionJobByID(t.Context(), job.ID)
		require.NoError(t, err)
		require.Equal(t, store.RenditionJobCompleted, current.State)
	})
}
