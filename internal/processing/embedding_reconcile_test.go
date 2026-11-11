package processing

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/media/mediatest"
	"go.kenn.io/docbank/internal/store"
	"go.kenn.io/docbank/internal/vectorworker"
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

type watchedVideoTestBlobs struct {
	embeddingRuntimeBlobs
	opens map[string]int
}

func (b *watchedVideoTestBlobs) OpenContext(ctx context.Context, hash string) (io.ReadSeekCloser, error) {
	b.opens[hash]++
	return b.embeddingRuntimeBlobs.OpenContext(ctx, hash)
}

func TestEmbeddingWorkerDiscoversWatchedMP4s(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	f := newPublicationFixture(t)
	descriptor := embeddingWorkerDescriptor(t)
	profileRecord := embeddingWorkerProfile(t, descriptor)
	var portable document.ProcessingProfileV1
	require.NoError(t, json.Unmarshal(profileRecord.CanonicalProfile, &portable))
	portable.Rendition = nil
	portable.RetentionDisclosure.RetainSanitizedMarkdown = false
	portable.Embeddings = portable.Embeddings[1:]
	require.Equal(t, "direct", portable.Embeddings[0].Name)
	portable.Embeddings[0].MaxInputBytes = 64 << 10

	fake := newEmbeddingWorkerFixture(t)
	provider := &embeddingWorkerProvider{runtime: fake.runtime, binding: "direct", descriptor: descriptor}
	var uploaded []string
	fake.runtime.inspectInputs = func(inputs []document.EmbeddingInput) {
		require.Len(t, inputs, 1)
		require.Equal(t, document.EmbeddingInputOriginalFile, inputs[0].Kind)
		data, err := io.ReadAll(inputs[0].Source)
		require.NoError(t, err)
		uploaded = append(uploaded, workerHashBytes(data))
	}
	gate := newTestOperationGate()
	service, err := NewService(ctx, ServiceConfig{Catalog: f.catalog, Blobs: f.blobs, Gate: gate, SpoolDirectory: t.TempDir(),
		Profiles: map[string]ProfileConfig{"video": {Profile: portable,
			AutomaticVideoBindings: map[string]bool{"direct": true},
			EmbeddingProviders:     map[string]document.EmbeddingProvider{"direct": provider}}}})
	require.NoError(t, err)
	runtimeBlobs := &watchedVideoTestBlobs{embeddingRuntimeBlobs: f.blobs, opens: make(map[string]int)}
	service.profiles["video"].embeddingRuntimes["direct"].blobs = runtimeBlobs
	now := time.Now().UTC()
	workerConfig := EmbeddingWorkerConfig{Clock: func() time.Time { return now }, Catalog: f.catalog, Authority: f.catalog,
		Blobs: f.blobs, GenerationBlobs: f.blobs, Runtime: service.embeddings, Gate: gate, Owner: "video-worker",
		LeaseDuration: 3 * time.Second, IdleDelay: time.Millisecond, RetryLimit: 3, MaxRetryDelay: time.Second,
		AttemptLifetime: time.Minute, MaxRows: 100, MaxDimensions: 8, MaxVectorBlobBytes: 1 << 20,
		DescriptorFingerprints: service.embeddings.Fingerprints()}
	worker, err := NewEmbeddingWorker(workerConfig)
	require.NoError(t, err)
	run, err := f.catalog.BeginIngest(ctx, "watch", "synthetic-watch")
	require.NoError(t, err)
	add := func(name string, data []byte) store.Node {
		t.Helper()
		receipt, err := f.blobs.WriteDetailedContext(ctx, bytes.NewReader(data))
		require.NoError(t, err)
		node, err := f.catalog.IngestFileExact(ctx, run, f.catalog.RootID(), name, receipt.Hash, receipt.Size,
			"video/mp4", name, "", processingBlobPhysical(t, receipt))
		require.NoError(t, err)
		asset, err := f.catalog.PhotoAssetForNode(ctx, node.ID)
		require.NoError(t, err)
		require.Equal(t, store.PhotoKindVideo, asset.Kind)
		return node
	}
	firstBytes, secondBytes := mediatest.H265MP4(), mediatest.VP9MP4()
	first := add("first.mp4", firstBytes)
	selector := Selector{NodeID: first.ID, ContentVersionID: first.CurrentVersionID, Profile: "video"}
	plan, err := service.Plan(ctx, selector)
	require.NoError(t, err)
	require.Contains(t, plan.Estimate.Scope, "automatically sends eligible MP4 originals throughout this vault")
	_, err = worker.ScanOnce(ctx)
	require.NoError(t, err)
	require.Empty(t, uploaded)

	_, err = service.GrantConsent(ctx, ConsentGrantRequest{Selector: selector, PlanFingerprint: plan.Fingerprint})
	require.NoError(t, err)
	now = now.Add(time.Minute)
	_, err = worker.ScanOnce(ctx)
	require.NoError(t, err)
	require.Equal(t, []string{workerHashBytes(firstBytes)}, uploaded)

	// Drive the same catalog writes as watch ingest for a new and a replaced file.
	second := add("second.mp4", secondBytes)
	invalid := mediatest.MP4(16, 16, 1_000)
	add("invalid.mp4", invalid)
	oversized := make([]byte, portable.Embeddings[0].MaxInputBytes+1)
	copy(oversized, firstBytes)
	add("oversized.mp4", oversized)
	replacement := mediatest.H264AACMP4()
	receipt, err := f.blobs.WriteDetailedContext(ctx, bytes.NewReader(replacement))
	require.NoError(t, err)
	_, current, changed, err := f.catalog.SyncWatchedContent(ctx, "synthetic-watch", "first.mp4",
		receipt.Hash, receipt.Size, "video/mp4", processingBlobPhysical(t, receipt))
	require.NoError(t, err)
	require.True(t, changed)
	now = now.Add(time.Minute)
	_, err = worker.ScanOnce(ctx)
	require.NoError(t, err)
	_, err = worker.ScanOnce(ctx)
	require.NoError(t, err)
	require.ElementsMatch(t, []string{workerHashBytes(firstBytes), workerHashBytes(secondBytes), workerHashBytes(replacement)}, uploaded)
	require.Zero(t, runtimeBlobs.opens[workerHashBytes(oversized)])
	require.Positive(t, runtimeBlobs.opens[workerHashBytes(invalid)], "the byte proof rejected the malformed MP4")

	index, err := vectorworker.NewIndexWorker(vectorworker.IndexWorkerConfig{Catalog: f.catalog,
		Mutate: gate.MutateContext, Owner: "video-index", BuildLease: time.Minute, ReaderLease: time.Minute,
		IdleDelay: time.Second, ReadVectorSet: func(ctx context.Context, member store.VectorIndexMember) ([]byte, error) {
			return f.catalog.ReadVectorIndexVectorSet(ctx, f.blobs, member)
		}})
	require.NoError(t, err)
	_, err = index.Rebuild(ctx, service.profiles["video"].vectorSpaces["direct"])
	require.NoError(t, err)
	selector.ContentVersionID = current.ID
	report, err := service.FindSimilar(ctx, SimilarRequest{Selector: selector, BindingID: "direct",
		Fence: SourceFence{VaultUID: f.catalog.VaultID(), ContentVersionIDs: []string{
			first.CurrentVersionID, current.ID, second.CurrentVersionID,
		}}})
	require.NoError(t, err)
	require.Len(t, report.Results, 1)
	require.Equal(t, second.CurrentVersionID, report.Results[0].Document.ContentVersionID)
	require.Len(t, uploaded, 3, "similarity uses stored vectors")
}
