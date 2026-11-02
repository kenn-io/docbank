package processing

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/store"
)

type countedReconcileCatalog struct {
	*store.Store

	examined    int
	enqueued    int
	pauseClaims bool
	limit       int
	afterPage   func()
	failNext    bool
}

func (c *countedReconcileCatalog) ReconcileEmbeddingJobs(ctx context.Context, request store.EmbeddingReconcileRequest) (store.EmbeddingReconcileResult, error) {
	if c.failNext {
		c.failNext = false
		return store.EmbeddingReconcileResult{}, errors.New("synthetic discovery read failure")
	}
	if c.limit > 0 {
		request.Limit = c.limit
	}
	result, err := c.Store.ReconcileEmbeddingJobs(ctx, request)
	c.examined += result.Examined
	c.enqueued += result.Enqueued
	if c.afterPage != nil {
		c.afterPage()
	}
	return result, err
}

func TestEmbeddingWorkerSettlesUnchangedCatalog(t *testing.T) {
	t.Parallel()
	f, _, worker, _ := newRealEmbeddingWorker(t, document.EmbeddingInputOriginalFile)
	catalog := &countedReconcileCatalog{Store: f.catalog}
	worker.catalog = catalog
	now := time.Now().UTC()
	worker.clock = func() time.Time { return now }
	// The first scan publishes work; the next pass observes that completed write.
	for range 2 {
		_, err := worker.ScanOnce(t.Context())
		require.NoError(t, err)
		now = now.Add(time.Minute)
	}
	require.Positive(t, catalog.examined)
	catalog.examined = 0
	_, err := worker.ScanOnce(t.Context())
	require.NoError(t, err)
	require.Zero(t, catalog.examined, "an unchanged vault stays idle after a clean sweep")
	now = now.Add(time.Minute)
	worker.descriptorFingerprints = append(worker.descriptorFingerprints, workerHash("added-runtime"))
	catalog.failNext = true
	_, err = worker.ScanOnce(t.Context())
	require.ErrorIs(t, err, ErrEmbeddingPersistence)
	_, err = worker.ScanOnce(t.Context())
	require.NoError(t, err)
	require.Positive(t, catalog.examined, "an interrupted sweep stays unsettled")
}

func (c *countedReconcileCatalog) ClaimNextEmbeddingWork(ctx context.Context, owner string, at time.Time, lease time.Duration, fingerprints []string) (EmbeddingWorkClaim, EmbeddingWork, bool, error) {
	if c.pauseClaims {
		return EmbeddingWorkClaim{}, EmbeddingWork{}, false, nil
	}
	return c.Store.ClaimNextEmbeddingWork(ctx, owner, at, lease, fingerprints)
}

func TestEmbeddingWorkerWakesForConsentAndExpiry(t *testing.T) {
	t.Parallel()
	f, _, worker, request, config := newRealEmbeddingWorkerWithDescriptor(t, document.EmbeddingInputOriginalFile, nil)
	catalog := &countedReconcileCatalog{Store: f.catalog, pauseClaims: true}
	worker.catalog = catalog
	now := time.Now().UTC()
	worker.clock = func() time.Time { return now }
	scan := func() {
		t.Helper()
		catalog.examined, catalog.enqueued = 0, 0
		_, err := worker.ScanOnce(t.Context())
		require.NoError(t, err)
		now = now.Add(time.Minute)
	}
	scan()
	scan()
	require.Zero(t, catalog.examined)
	expires := now.Add(5 * time.Minute)
	consent := request.Authorization
	_, err := f.catalog.GrantConsent(t.Context(), store.ProcessingConsentGrantRequest{
		Principal: consent.Principal, Scope: consent.Scope, ProfileFingerprint: consent.ProfileFingerprint,
		DisclosureFingerprint: consent.DisclosureFingerprint, InputClasses: consent.InputClasses,
		RetainedArtifactClasses: consent.RetainedArtifactClasses, ExpiresAt: &expires,
	})
	require.NoError(t, err)
	scan()
	require.Positive(t, catalog.enqueued, "a fresh grant creates its own authorized job")
	scan() // Observe the enqueue write before settling.
	scan()
	require.Zero(t, catalog.examined)
	now = expires
	scan()
	require.Positive(t, catalog.examined, "expiry wakes discovery without another write")
	scan()
	require.Zero(t, catalog.examined)
	restarted, err := NewEmbeddingWorker(config)
	require.NoError(t, err)
	restarted.catalog = catalog
	restarted.clock = worker.clock
	catalog.examined = 0
	_, err = restarted.ScanOnce(t.Context())
	require.NoError(t, err)
	require.Positive(t, catalog.examined, "restart always recovers durable authority")
}

type unavailableReconcileBlobs struct{ opens int }

func (b *unavailableReconcileBlobs) OpenContext(context.Context, string) (io.ReadSeekCloser, error) {
	b.opens++
	return nil, errors.New("synthetic blob temporarily unavailable")
}

func TestEmbeddingWorkerRetriesSkippedHydration(t *testing.T) {
	t.Parallel()
	f, _, worker, request := newRealEmbeddingWorker(t, document.EmbeddingInputRenditionChunk)
	job, err := f.catalog.EnqueueEmbeddingJob(t.Context(), request)
	require.NoError(t, err)
	claim, _, found, err := f.catalog.ClaimEmbeddingWork(t.Context(), job.ID, "abandoned-worker", time.Now(), time.Minute, worker.descriptorFingerprints)
	require.NoError(t, err)
	require.True(t, found)
	require.NoError(t, f.catalog.AbandonEmbeddingWork(t.Context(), claim, time.Now()))
	catalog := &countedReconcileCatalog{Store: f.catalog, pauseClaims: true}
	worker.catalog = catalog
	unavailable := &unavailableReconcileBlobs{}
	worker.generationBlobs = unavailable
	now := time.Now().UTC()
	worker.clock = func() time.Time { return now }
	for range 2 {
		_, err := worker.ScanOnce(t.Context())
		require.NoError(t, err)
		now = now.Add(time.Minute)
	}
	require.Equal(t, 2, unavailable.opens, "skipped hydration keeps the next sweep active")
	worker.generationBlobs = f.blobs
	catalog.enqueued = 0
	_, err = worker.ScanOnce(t.Context())
	require.NoError(t, err)
	require.Positive(t, catalog.enqueued, "recovered input resumes abandoned work")
}

func TestEmbeddingWorkerKeepsWritesBehindCursorUnsettled(t *testing.T) {
	t.Parallel()
	f, _, worker, request := newRealEmbeddingWorker(t, document.EmbeddingInputOriginalFile)
	high := request
	high.InputGeneration.ID = strings.Repeat("f", 64)
	_, err := f.catalog.EnqueueEmbeddingJob(t.Context(), high)
	require.NoError(t, err)
	catalog := &countedReconcileCatalog{Store: f.catalog, pauseClaims: true, limit: 1}
	worker.catalog = catalog
	now := time.Now().UTC()
	worker.clock = func() time.Time { return now }
	catalog.afterPage = func() {
		catalog.afterPage = nil
		behind := request
		behind.InputGeneration.ID = strings.Repeat("0", 64)
		_, err := f.catalog.EnqueueEmbeddingJob(t.Context(), behind)
		require.NoError(t, err)
	}
	for range 2 {
		_, err := worker.ScanOnce(t.Context())
		require.NoError(t, err)
	}
	require.Equal(t, 2, catalog.examined, "the new generation lies behind the active cursor")
	now = now.Add(time.Minute)
	catalog.examined = 0
	_, err = worker.ScanOnce(t.Context())
	require.NoError(t, err)
	require.Equal(t, 1, catalog.examined, "a write during a sweep requires a fresh pass")
}
