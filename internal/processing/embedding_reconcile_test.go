package processing

import (
	"context"
	"errors"
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
}

func (c *countedReconcileCatalog) ReconcileEmbeddingJobs(ctx context.Context, request store.EmbeddingReconcileRequest) (store.EmbeddingReconcileResult, error) {
	result, err := c.Store.ReconcileEmbeddingJobs(ctx, request)
	c.examined += result.Examined
	c.enqueued += result.Enqueued
	return result, err
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

func TestEmbeddingWorkerSettlesOnlyPermanentInputFailures(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		blobs   func(store.EmbeddingJobRequest) *embeddingRuntimeTestBlobs
		settles bool
	}{
		{"blob unavailable", func(store.EmbeddingJobRequest) *embeddingRuntimeTestBlobs {
			return &embeddingRuntimeTestBlobs{openErr: errors.New("synthetic blob temporarily unavailable")}
		}, false},
		{"checksum mismatch", func(request store.EmbeddingJobRequest) *embeddingRuntimeTestBlobs {
			return &embeddingRuntimeTestBlobs{data: request.InputGeneration.GenerationJSON}
		}, true},
		{"evidence over limit", func(request store.EmbeddingJobRequest) *embeddingRuntimeTestBlobs {
			return &embeddingRuntimeTestBlobs{byHash: map[string][]byte{
				request.InputGeneration.GenerationBlobHash:  request.InputGeneration.GenerationJSON,
				request.InputGeneration.EvidenceFingerprint: make([]byte, (64<<20)+1),
			}}
		}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f, _, worker, request := newRealEmbeddingWorker(t, document.EmbeddingInputRenditionChunk)
			job, err := f.catalog.EnqueueEmbeddingJob(t.Context(), request)
			require.NoError(t, err)
			claim, _, found, err := f.catalog.ClaimEmbeddingWork(t.Context(), job.ID, "abandoned-worker", time.Now(), time.Minute, worker.descriptorFingerprints)
			require.NoError(t, err)
			require.True(t, found)
			require.NoError(t, f.catalog.AbandonEmbeddingWork(t.Context(), claim, time.Now()))
			worker.generationBlobs = tc.blobs(request)
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
			require.Equal(t, tc.settles, catalog.examined == 0, "only permanently rejected input lets the vault settle")
		})
	}
}

func TestEmbeddingWorkerRecoversBlobOperationContextErrors(t *testing.T) {
	t.Parallel()
	for _, inputErr := range []error{context.DeadlineExceeded, context.Canceled} {
		t.Run(inputErr.Error(), func(t *testing.T) {
			t.Parallel()
			f, _, worker, request := newRealEmbeddingWorker(t, document.EmbeddingInputRenditionChunk)
			job, err := f.catalog.EnqueueEmbeddingJob(t.Context(), request)
			require.NoError(t, err)
			now := time.Now().UTC()
			claim, _, found, err := f.catalog.ClaimEmbeddingWork(t.Context(), job.ID, "abandoned-worker", now, time.Minute, worker.descriptorFingerprints)
			require.NoError(t, err)
			require.True(t, found)
			require.NoError(t, f.catalog.AbandonEmbeddingWork(t.Context(), claim, now))
			catalog := &countedReconcileCatalog{Store: f.catalog}
			worker.catalog = catalog
			worker.clock = func() time.Time { return now }
			worker.generationBlobs = &embeddingRuntimeTestBlobs{openErr: inputErr}
			processed, err := worker.ScanOnce(t.Context())
			require.NoError(t, err)
			require.NoError(t, t.Context().Err(), "the blob operation failed while its caller stayed active")
			require.Zero(t, processed)
			require.Positive(t, catalog.examined)
			require.Zero(t, catalog.enqueued)

			revision := f.catalog.WriteRevision()
			worker.generationBlobs = f.blobs
			now = now.Add(time.Minute)
			require.Equal(t, revision, f.catalog.WriteRevision(), "blob recovery needs no catalog write")
			processed, err = worker.ScanOnce(t.Context())
			require.NoError(t, err)
			require.Equal(t, 1, catalog.enqueued, "retry discovery after the blob operation recovers")
			require.Equal(t, 1, processed)
			completed, err := f.catalog.EmbeddingJobByID(t.Context(), job.ID)
			require.NoError(t, err)
			require.Equal(t, "completed", completed.State)
		})
	}
}
