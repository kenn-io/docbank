package processing

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/store"
	docsqlite "go.kenn.io/docbank/sqlite"
)

type countedReconcileCatalog struct {
	*store.Store

	examined           int
	enqueued           int
	pauseClaims        bool
	beforePrepareImage func()
}

func (c *countedReconcileCatalog) ReconcileEmbeddingJobs(ctx context.Context, request store.EmbeddingReconcileRequest) (store.EmbeddingReconcileResult, error) {
	if c.beforePrepareImage != nil {
		prepare := request.PrepareImageGeneration
		request.PrepareImageGeneration = func(ctx context.Context, profile store.ProcessingProfileRecord, binding document.EmbeddingBindingV1, version string) (store.EmbeddingInputGenerationRecord, document.EmbeddingDescriptor, error) {
			c.beforePrepareImage()
			return prepare(ctx, profile, binding, version)
		}
	}
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

	expires = now.Add(5 * time.Minute)
	grantRequest := store.ProcessingConsentGrantRequest{
		Principal: consent.Principal, Scope: consent.Scope, ProfileFingerprint: consent.ProfileFingerprint,
		DisclosureFingerprint: consent.DisclosureFingerprint, InputClasses: consent.InputClasses,
		RetainedArtifactClasses: consent.RetainedArtifactClasses, ExpiresAt: &expires,
	}
	_, err = f.catalog.GrantConsent(t.Context(), grantRequest)
	require.NoError(t, err)
	grantRequest.ExpiresAt = nil
	_, err = f.catalog.GrantConsent(t.Context(), grantRequest)
	require.NoError(t, err)
	permanent, err := f.catalog.EnqueueEmbeddingJob(t.Context(), request)
	require.NoError(t, err)
	claim, work, found, err = f.catalog.ClaimEmbeddingWork(t.Context(), permanent.ID, "permanent-worker", now, time.Minute, worker.descriptorFingerprints)
	require.NoError(t, err)
	require.True(t, found)
	require.NoError(t, f.catalog.FailEmbeddingWork(t.Context(), claim, work, store.EmbeddingFailureInputRejected, store.EmbeddingAttemptReceipt{}, now))
	scan()
	require.Positive(t, catalog.examined, "the permanent grant write wakes discovery")
	scan()
	require.Zero(t, catalog.examined)
	revision := f.catalog.WriteRevision()
	now = expires
	scan()
	require.Equal(t, revision, f.catalog.WriteRevision())
	require.Zero(t, catalog.enqueued)
	require.Zero(t, catalog.examined, "an unused older grant's expiry keeps discovery idle")
}

func TestEmbeddingWorkerSettlesOnlyPermanentInputFailures(t *testing.T) {
	t.Parallel()
	t.Run("photo preview blob recovers", func(t *testing.T) {
		t.Parallel()
		f := newPhotoDescriptionFixture(t)
		x := newImageEmbeddingService(t, f)
		f.grant(t, x.service, "images")
		view, reason, err := photoDescriptionPreview(t.Context(), f.catalog, f.node.CurrentVersionID)
		require.NoError(t, err)
		require.Empty(t, reason)
		hash := view.Generation.Preview.Output.BlobSHA256
		source, err := f.blobs.OpenContext(t.Context(), hash)
		require.NoError(t, err)
		raw, err := io.ReadAll(source)
		require.NoError(t, err)
		require.NoError(t, source.Close())
		require.NoError(t, f.blobs.Remove(hash))
		_, _, err = x.service.EmbeddingRuntimes().ImageGeneration(t.Context(), x.service.profiles["images"].record, x.binding, f.node.CurrentVersionID)
		require.ErrorIs(t, err, errPhotoDescriptionInput)
		catalog := &countedReconcileCatalog{Store: f.catalog, pauseClaims: true}
		preparations := 0
		catalog.beforePrepareImage = func() { preparations++ }
		worker, err := NewEmbeddingWorker(EmbeddingWorkerConfig{Catalog: catalog, Authority: f.catalog, Blobs: f.blobs, GenerationBlobs: f.blobs, Runtime: x.service.EmbeddingRuntimes(), Gate: newTestOperationGate(), Owner: "synthetic-images", RetryLimit: 3, RetryBaseDelay: time.Millisecond, MaxRetryDelay: time.Second, AttemptLifetime: 10 * time.Minute, MaxRows: 100, MaxDimensions: 100, MaxVectorBlobBytes: 1 << 20, LeaseDuration: time.Minute, IdleDelay: time.Millisecond, DescriptorFingerprints: []string{x.descriptor.Fingerprint}})
		require.NoError(t, err)
		now := time.Now().UTC()
		worker.clock = func() time.Time { return now }
		_, err = worker.ScanOnce(t.Context())
		require.NoError(t, err)
		require.Equal(t, 1, preparations)
		require.Zero(t, catalog.enqueued)

		revision := f.catalog.WriteRevision()
		_, err = f.blobs.WriteDetailedContext(t.Context(), bytes.NewReader(raw))
		require.NoError(t, err)
		require.Equal(t, revision, f.catalog.WriteRevision(), "blob recovery needs no catalog write")
		now = now.Add(time.Minute)
		catalog.pauseClaims = false
		processed, err := worker.ScanOnce(t.Context())
		require.NoError(t, err)
		require.Equal(t, 1, processed, "retry image preparation after the preview blob recovers")
	})
	for _, tc := range []struct {
		name   string
		column string
		value  func(*testing.T, publicationFixture) string
	}{
		{"checksum mismatch", "generation_checksum", func(_ *testing.T, _ publicationFixture) string {
			return workerHash("damaged generation checksum")
		}},
		{"evidence over limit", "evidence_fingerprint", func(t *testing.T, f publicationFixture) string {
			t.Helper()
			receipt, err := f.blobs.WriteDetailedContext(t.Context(), bytes.NewReader(make([]byte, (64<<20)+1)))
			require.NoError(t, err)
			require.NoError(t, f.catalog.RecordRenditionBlob(t.Context(), receipt.Hash, receipt.Size, processingBlobPhysical(t, receipt)))
			return receipt.Hash
		}},
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
			// Damage catalog authority while retaining hash-valid physical blobs.
			value := tc.value(t, f)
			db, err := store.DefaultSQLiteDriver().Open(f.databasePath, docsqlite.OpenOptions{Access: docsqlite.ReadWriteExisting, TransactionMode: docsqlite.Immediate})
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, db.Close()) })
			_, err = db.ExecContext(t.Context(), `DROP TRIGGER embedding_input_generations_immutable_update`)
			require.NoError(t, err)
			_, err = db.ExecContext(t.Context(), `UPDATE embedding_input_generations SET `+tc.column+`=? WHERE generation_id=?`, value, request.InputGeneration.ID)
			require.NoError(t, err)
			generation := request.InputGeneration
			if tc.column == "generation_checksum" {
				generation.GenerationChecksum = value
			} else {
				generation.EvidenceFingerprint = value
			}
			_, err = hydrateEmbeddingGeneration(t.Context(), f.blobs, generation)
			require.ErrorIs(t, err, ErrEmbeddingInputRejected)
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
			require.Zero(t, catalog.examined, "permanently rejected input lets the vault settle")
		})
	}
}

func TestEmbeddingWorkerRecoversBlobOperationContextErrors(t *testing.T) {
	t.Parallel()
	for _, inputErr := range []error{context.DeadlineExceeded, errors.New("synthetic blob temporarily unavailable")} {
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
