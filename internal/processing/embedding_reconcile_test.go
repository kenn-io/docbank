package processing

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/store"
	"go.kenn.io/docbank/internal/vectorworker"
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
	f, _, worker, request := newRealEmbeddingWorker(t, document.EmbeddingInputOriginalFile)
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
	spaces, err := f.catalog.ListVectorIndexSpaces(t.Context())
	require.NoError(t, err)
	indexer, err := vectorworker.NewIndexWorker(vectorworker.IndexWorkerConfig{Catalog: f.catalog, Owner: "idle-index", Mutate: worker.gate.MutateContext, BuildLease: time.Minute, ReaderLease: time.Minute, IdleDelay: time.Millisecond, ReadVectorSet: func(ctx context.Context, member store.VectorIndexMember) ([]byte, error) {
		return readExactEmbeddingBlob(ctx, f.blobs, member.PayloadBlobHash, member.PayloadSize)
	}})
	require.NoError(t, err)
	_, err = indexer.Rebuild(t.Context(), spaces[0])
	require.NoError(t, err)
	_, err = worker.ScanOnce(t.Context())
	require.NoError(t, err)
	now = now.Add(time.Minute)
	lease, err := f.catalog.AcquireVectorIndexGeneration(t.Context(), spaces[0], "search-reader", now, time.Minute)
	require.NoError(t, err)
	require.NoError(t, f.catalog.ReleaseVectorIndexGeneration(t.Context(), lease.ID, lease.FencingToken, now))
	require.Positive(t, catalog.examined)
	catalog.examined = 0
	_, err = worker.ScanOnce(t.Context())
	require.NoError(t, err)
	require.Zero(t, catalog.examined, "an unchanged vault stays idle after a clean sweep")
	now = now.Add(time.Minute)
	_, err = f.catalog.GrantConsent(t.Context(), store.ProcessingConsentGrantRequest{
		Principal: request.Authorization.Principal, Scope: request.Authorization.Scope, ProfileFingerprint: f.profile.Fingerprint,
		DisclosureFingerprint: request.Authorization.DisclosureFingerprint, InputClasses: request.Authorization.InputClasses, RetainedArtifactClasses: request.Authorization.RetainedArtifactClasses,
	})
	require.NoError(t, err)
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
	old, err := f.catalog.EnqueueEmbeddingJob(t.Context(), request)
	require.NoError(t, err)
	claim, _, found, err := f.catalog.ClaimEmbeddingWork(t.Context(), old.ID, "old-worker", now, time.Minute, worker.descriptorFingerprints)
	require.NoError(t, err)
	require.True(t, found)
	require.NoError(t, f.catalog.AbandonEmbeddingWork(t.Context(), claim, now))
	expires := now.Add(5 * time.Minute)
	consent := request.Authorization
	_, err = f.catalog.GrantConsent(t.Context(), store.ProcessingConsentGrantRequest{
		Principal: consent.Principal, Scope: consent.Scope, ProfileFingerprint: consent.ProfileFingerprint,
		DisclosureFingerprint: consent.DisclosureFingerprint, InputClasses: consent.InputClasses,
		RetainedArtifactClasses: consent.RetainedArtifactClasses, ExpiresAt: &expires,
	})
	require.NoError(t, err)
	scan()
	require.Positive(t, catalog.enqueued, "a fresh grant creates its own authorized job")
	newer, err := f.catalog.EnqueueEmbeddingJob(t.Context(), request)
	require.NoError(t, err)
	require.NotEqual(t, old.ID, newer.ID)
	claim, work, found, err := f.catalog.ClaimEmbeddingWork(t.Context(), newer.ID, "new-worker", now, time.Minute, worker.descriptorFingerprints)
	require.NoError(t, err)
	require.True(t, found)
	require.NoError(t, f.catalog.FailEmbeddingWork(t.Context(), claim, work, store.EmbeddingFailureInputRejected, store.EmbeddingAttemptReceipt{}, now))
	scan() // Observe the terminal job write before settling.
	scan()
	require.Zero(t, catalog.examined)
	now = expires
	scan()
	require.Positive(t, catalog.enqueued, "expiry returns to the older grant and resumes its abandoned job")
	scan() // Observe the resumed job write.
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
	unavailable := &embeddingRuntimeTestBlobs{openErr: errors.New("synthetic blob temporarily unavailable")}
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

func TestEmbeddingWorkerSettlesRejectedHydration(t *testing.T) {
	t.Parallel()
	for _, oversized := range []bool{false, true} {
		t.Run(map[bool]string{false: "checksum mismatch", true: "evidence over limit"}[oversized], func(t *testing.T) {
			t.Parallel()
			f, _, worker, request := newRealEmbeddingWorker(t, document.EmbeddingInputRenditionChunk)
			job, err := f.catalog.EnqueueEmbeddingJob(t.Context(), request)
			require.NoError(t, err)
			claim, _, found, err := f.catalog.ClaimEmbeddingWork(t.Context(), job.ID, "abandoned-worker", time.Now(), time.Minute, worker.descriptorFingerprints)
			require.NoError(t, err)
			require.True(t, found)
			require.NoError(t, f.catalog.AbandonEmbeddingWork(t.Context(), claim, time.Now()))
			blobs := &embeddingRuntimeTestBlobs{data: request.InputGeneration.GenerationJSON}
			if oversized {
				blobs.byHash = map[string][]byte{
					request.InputGeneration.GenerationBlobHash:  request.InputGeneration.GenerationJSON,
					request.InputGeneration.EvidenceFingerprint: make([]byte, (64<<20)+1),
				}
			}
			worker.generationBlobs = blobs
			catalog := &countedReconcileCatalog{Store: f.catalog, pauseClaims: true}
			worker.catalog = catalog
			now := time.Now().UTC()
			worker.clock = func() time.Time { return now }
			_, err = worker.ScanOnce(t.Context())
			require.NoError(t, err)
			require.Positive(t, catalog.examined)
			require.Zero(t, catalog.enqueued)
			now = now.Add(time.Minute)
			catalog.examined = 0
			_, err = worker.ScanOnce(t.Context())
			require.NoError(t, err)
			require.Zero(t, catalog.examined, "permanently rejected input permits an idle vault")
		})
	}
}
