package processing

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"image/color"
	"io"
	"math"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/packstore"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/media/mediatest"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/blob"
	"go.kenn.io/docbank/internal/store"
)

func TestEmbeddingWorkerPublishesBindingsAndInputKindsIndependently(t *testing.T) {
	fixture := newEmbeddingWorkerFixture(t)
	chunk := fixture.work("chunks-a", document.EmbeddingInputRenditionChunk, "chunk-a")
	second := fixture.work("chunks-b", document.EmbeddingInputRenditionChunk, "chunk-b")
	direct := fixture.work("direct", document.EmbeddingInputOriginalFile, "original")
	fixture.catalog.enqueue(chunk, second, direct)

	worker := fixture.worker(t)
	processed, err := worker.ScanOnce(t.Context())
	require.NoError(t, err)
	assert.Equal(t, 3, processed)
	assert.Equal(t, []string{"chunk-a", "chunk-b", "original"}, fixture.catalog.publishedBindings())
	assert.Len(t, fixture.catalog.heads, 3)
	assert.NotEqual(t, fixture.catalog.heads[headKey(chunk)].SetID, fixture.catalog.heads[headKey(second)].SetID)
	assert.Equal(t, 3, fixture.runtime.calls())
}

func TestEmbeddingWorkerFailureDoesNotDisturbSiblingPublication(t *testing.T) {
	fixture := newEmbeddingWorkerFixture(t)
	good := fixture.work("good", document.EmbeddingInputRenditionChunk, "good")
	retrying := fixture.work("retry", document.EmbeddingInputRenditionChunk, "retry")
	terminal := fixture.work("terminal", document.EmbeddingInputOriginalFile, "terminal")
	fixture.runtime.failures[retrying.Binding.Name] = []error{embeddingTransientError{retryAfter: 2 * time.Millisecond}}
	fixture.runtime.failures[terminal.Binding.Name] = []error{embeddingPermanentError{}}
	fixture.catalog.enqueue(good, retrying, terminal)

	processed, err := fixture.worker(t).ScanOnce(t.Context())
	require.NoError(t, err)
	assert.Equal(t, 3, processed)
	assert.Contains(t, fixture.catalog.heads, headKey(good))
	assert.Contains(t, fixture.catalog.heads, headKey(retrying))
	assert.NotContains(t, fixture.catalog.heads, headKey(terminal))
	assert.Equal(t, store.EmbeddingFailureInputRejected, fixture.catalog.failures[headKey(terminal)])
	assert.Equal(t, 2, fixture.runtime.callCount(retrying.Binding.Name))
}

func TestEmbeddingWorkerRejectsMalformedProviderResultsWithoutPublishing(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(document.EmbeddingResult) document.EmbeddingResult
	}{
		{"missing", func(value document.EmbeddingResult) document.EmbeddingResult {
			value.Vectors = value.Vectors[:1]
			return value
		}},
		{"duplicate", func(value document.EmbeddingResult) document.EmbeddingResult {
			value.Vectors[1].Key = value.Vectors[0].Key
			return value
		}},
		{"reordered", func(value document.EmbeddingResult) document.EmbeddingResult {
			value.Vectors[0], value.Vectors[1] = value.Vectors[1], value.Vectors[0]
			return value
		}},
		{"wrong dimension", func(value document.EmbeddingResult) document.EmbeddingResult {
			value.Vectors[0].Values = value.Vectors[0].Values[:1]
			return value
		}},
		{"non finite", func(value document.EmbeddingResult) document.EmbeddingResult {
			value.Vectors[0].Values[0] = float32(math.NaN())
			return value
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newEmbeddingWorkerFixture(t)
			work := fixture.work(test.name, document.EmbeddingInputRenditionChunk, "semantic")
			fixture.runtime.mutate[work.Binding.Name] = test.mutate
			fixture.catalog.enqueue(work)
			processed, err := fixture.worker(t).ScanOnce(t.Context())
			require.NoError(t, err)
			assert.Equal(t, 1, processed)
			assert.Empty(t, fixture.catalog.heads)
			assert.Equal(t, store.EmbeddingFailureInvalidResponse, fixture.catalog.failures[headKey(work)])
		})
	}
}

func TestEmbeddingWorkerRejectsMissingExactE2ArtifactBeforeProviderCall(t *testing.T) {
	fixture := newEmbeddingWorkerFixture(t)
	work := fixture.work("missing-e2-artifact", document.EmbeddingInputRenditionChunk, "semantic")
	work.InputGeneration.GenerationBlobHash = workerHash("canonical-e2-artifact")
	work.InputGeneration.GenerationEncodedSize = 128
	fixture.catalog.enqueue(work)

	processed, err := fixture.worker(t).ScanOnce(t.Context())
	require.NoError(t, err)
	assert.Equal(t, 1, processed)
	assert.Zero(t, fixture.runtime.callCount(work.Binding.Name))
	assert.Empty(t, fixture.catalog.staged)
	assert.Equal(t, store.EmbeddingFailureInvalidResponse, fixture.catalog.failures[headKey(work)])
}

func TestEmbeddingWorkerNormalizesValidIndexedResultsIntoInputOrder(t *testing.T) {
	fixture := newEmbeddingWorkerFixture(t)
	work := fixture.work("indexed-order", document.EmbeddingInputRenditionChunk, "semantic")
	fixture.runtime.mutate[work.Binding.Name] = func(value document.EmbeddingResult) document.EmbeddingResult {
		zero, one := 0, 1
		value.Vectors[0].Index = &zero
		value.Vectors[1].Index = &one
		value.Vectors[0], value.Vectors[1] = value.Vectors[1], value.Vectors[0]
		return value
	}
	fixture.catalog.enqueue(work)

	processed, err := fixture.worker(t).ScanOnce(t.Context())
	require.NoError(t, err)
	require.Equal(t, 1, processed)
	set := fixture.catalog.onlyStaged(t)
	decoded, err := document.DecodeVectorSetV1(set.VectorSet.Payload, document.VectorBounds{
		MaxRows: 2, MaxDimension: work.Descriptor.Dimension, MaxBytes: len(set.VectorSet.Payload),
	})
	require.NoError(t, err)
	assert.Equal(t, work.InputGeneration.Inputs[0].ID, decoded.InputKeys[0])
	assert.Equal(t, []float32{1, 2}, decoded.Vectors[0])
	assert.Equal(t, work.InputGeneration.Inputs[1].ID, decoded.InputKeys[1])
	assert.Equal(t, []float32{2, 2}, decoded.Vectors[1])
}

func TestEmbeddingWorkerAttemptDeadlineCoversProviderAndPersistence(t *testing.T) {
	fixture := newEmbeddingWorkerFixture(t)
	work := fixture.work("deadline", document.EmbeddingInputRenditionChunk, "semantic")
	fixture.catalog.enqueue(work)

	processed, err := fixture.worker(t).ScanOnce(t.Context())
	require.NoError(t, err)
	require.Equal(t, 1, processed)
	assert.True(t, fixture.runtime.sawDeadline())
	assert.True(t, fixture.blobs.sawDeadline())
}

func TestEmbeddingWorkerReopensAuthorizedOriginalForEveryRetry(t *testing.T) {
	fixture := newEmbeddingWorkerFixture(t)
	work := fixture.work("direct-retry", document.EmbeddingInputOriginalFile, "direct")
	fixture.runtime.failures[work.Binding.Name] = []error{embeddingTransientError{}}
	fixture.runtime.reopenOriginal = true
	fixture.runtime.consumeOriginal = true
	fixture.catalog.enqueue(work)

	processed, err := fixture.worker(t).ScanOnce(t.Context())
	require.NoError(t, err)
	require.Equal(t, 1, processed)
	assert.Equal(t, 2, fixture.runtime.callCount(work.Binding.Name))
	assert.Equal(t, 2, fixture.runtime.prepareCount(work.Binding.Name))
	assert.Contains(t, fixture.catalog.heads, headKey(work))
}

func TestProviderEmbeddingRuntimeReopensAndReauthorizesOriginalEveryPrepare(t *testing.T) {
	fixture := newEmbeddingWorkerFixture(t)
	work := fixture.work("production-direct", document.EmbeddingInputOriginalFile, "direct")
	data := mediatest.PNG(2, 2, color.White)
	work.SourceBlobHash = workerHashBytes(data)
	work.SourceBytes = int64(len(data))
	work.SourceFilename = "synthetic.png"
	work.SourceMediaType = "image/png"
	blobs := &embeddingRuntimeTestBlobs{data: data}
	provider := &embeddingWorkerProvider{runtime: fixture.runtime, binding: work.Binding.Name, descriptor: work.Descriptor}
	runtime, err := NewProviderEmbeddingRuntime(provider, blobs, t.TempDir(), fixture.runtime.Classify)
	require.NoError(t, err)

	first, err := runtime.Prepare(t.Context(), work)
	require.NoError(t, err)
	require.Len(t, first.Inputs, 1)
	assert.Equal(t, work.SourceBlobHash, first.Inputs[0].Source.Metadata().SHA256)
	require.NoError(t, first.Inputs[0].Source.Close())
	second, err := runtime.Prepare(t.Context(), work)
	require.NoError(t, err)
	require.NoError(t, second.Inputs[0].Source.Close())
	assert.Equal(t, 2, blobs.opens)
}

func TestProviderEmbeddingRuntimeRejectsUnboundedOriginalAuthority(t *testing.T) {
	fixture := newEmbeddingWorkerFixture(t)
	work := fixture.work("production-direct-bound", document.EmbeddingInputOriginalFile, "direct")
	data := mediatest.PNG(2, 2, color.White)
	work.SourceBlobHash = workerHashBytes(data)
	work.SourceBytes = work.Binding.MaxInputBytes + 1
	work.SourceFilename = "synthetic.png"
	work.SourceMediaType = "image/png"
	blobs := &embeddingRuntimeTestBlobs{data: data}
	provider := &embeddingWorkerProvider{runtime: fixture.runtime, binding: work.Binding.Name, descriptor: work.Descriptor}

	_, err := NewProviderEmbeddingRuntime(provider, blobs, "relative-spool", fixture.runtime.Classify)
	require.ErrorContains(t, err, "absolute")
	runtime, err := NewProviderEmbeddingRuntime(provider, blobs, t.TempDir(), fixture.runtime.Classify)
	require.NoError(t, err)
	_, err = runtime.Prepare(t.Context(), work)
	require.ErrorContains(t, err, "byte authority")
	assert.Zero(t, blobs.opens)
}

func TestEmbeddingWorkerFencesLeaseConsentAndAuthorityDrift(t *testing.T) {
	tests := []struct {
		name string
		hook func(*embeddingWorkerFakeCatalog)
	}{
		{"consent before call", func(c *embeddingWorkerFakeCatalog) { c.failAuthorizeAt = 1 }},
		{"consent before retry", func(c *embeddingWorkerFakeCatalog) { c.failAuthorizeAt = 2 }},
		{"source drift before stage", func(c *embeddingWorkerFakeCatalog) { c.failValidateAt = 2 }},
		{"consent before publish", func(c *embeddingWorkerFakeCatalog) { c.failPublish = store.ErrProcessingConsentRevoked }},
		{"stale attempt loses", func(c *embeddingWorkerFakeCatalog) { c.failPublish = ErrEmbeddingWorkFenced }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newEmbeddingWorkerFixture(t)
			work := fixture.work(test.name, document.EmbeddingInputRenditionChunk, "semantic")
			if test.name == "consent before retry" {
				fixture.runtime.failures[work.Binding.Name] = []error{embeddingTransientError{}}
			}
			test.hook(fixture.catalog)
			fixture.catalog.enqueue(work)
			processed, err := fixture.worker(t).ScanOnce(t.Context())
			require.NoError(t, err)
			assert.Equal(t, 1, processed)
			assert.Empty(t, fixture.catalog.heads)
			assert.NotContains(t, fixture.catalog.receiptsText(), "synthetic input")
		})
	}
}

func TestEmbeddingWorkerContinuesAfterClaimLosesPublicationFence(t *testing.T) {
	fixture := newEmbeddingWorkerFixture(t)
	first := fixture.work("lost-publication-fence", document.EmbeddingInputRenditionChunk, "first")
	second := fixture.work("after-lost-publication-fence", document.EmbeddingInputRenditionChunk, "second")
	fixture.catalog.failPublish = store.ErrEmbeddingJobFenced
	fixture.catalog.failFailure = store.ErrEmbeddingJobFenced
	fixture.catalog.enqueue(first, second)

	processed, err := fixture.worker(t).ScanOnce(t.Context())
	require.NoError(t, err)
	assert.Equal(t, 2, processed)
	assert.NotContains(t, fixture.catalog.heads, headKey(first))
	assert.Contains(t, fixture.catalog.heads, headKey(second))
}

func TestEmbeddingWorkerContinuesAfterClaimLosesStageOrFinishFence(t *testing.T) {
	for _, phase := range []string{"stage", "finish"} {
		t.Run(phase, func(t *testing.T) {
			fixture := newEmbeddingWorkerFixture(t)
			first := fixture.work("lost-"+phase+"-fence", document.EmbeddingInputRenditionChunk, "first")
			second := fixture.work("after-"+phase+"-fence", document.EmbeddingInputRenditionChunk, "second")
			if phase == "stage" {
				fixture.catalog.failStage = store.ErrCurrentRenditionRootFenced
			} else {
				fixture.catalog.failFinish = store.ErrEmbeddingJobFenced
			}
			fixture.catalog.enqueue(first, second)

			processed, err := fixture.worker(t).ScanOnce(t.Context())
			require.NoError(t, err)
			assert.Equal(t, 2, processed)
			assert.Contains(t, fixture.catalog.heads, headKey(second))
			if phase == "stage" {
				assert.NotContains(t, fixture.catalog.heads, headKey(first))
			} else {
				assert.Contains(t, fixture.catalog.heads, headKey(first))
			}
		})
	}
}

func TestEmbeddingWorkerResumesAfterLeaseExpiryAndDeduplicatesScan(t *testing.T) {
	fixture := newEmbeddingWorkerFixture(t)
	work := fixture.work("resume", document.EmbeddingInputRenditionChunk, "semantic")
	fixture.catalog.enqueue(work, work)
	first := fixture.worker(t)
	second := fixture.worker(t)

	processed, err := first.ScanOnce(t.Context())
	require.NoError(t, err)
	assert.Equal(t, 1, processed)
	processed, err = second.ScanOnce(t.Context())
	require.NoError(t, err)
	assert.Zero(t, processed)
	assert.Len(t, fixture.catalog.heads, 1)
	assert.Equal(t, 1, fixture.runtime.callCount(work.Binding.Name))
}

func TestEmbeddingWorkerBoundsRetriesAndRejectsCapacity(t *testing.T) {
	fixture := newEmbeddingWorkerFixture(t)
	retry := fixture.work("retry-bounds", document.EmbeddingInputRenditionChunk, "retry")
	capacity := fixture.work("capacity", document.EmbeddingInputRenditionChunk, "capacity")
	capacity.Inputs = capacity.Inputs[:1]
	capacity.InputGeneration.Inputs = capacity.InputGeneration.Inputs[:1]
	fixture.runtime.failures[retry.Binding.Name] = []error{
		embeddingTransientError{retryAfter: time.Hour}, embeddingTransientError{}, embeddingTransientError{},
	}
	fixture.runtime.failures[capacity.Binding.Name] = []error{embeddingCapacityError{}}
	fixture.catalog.enqueue(retry, capacity)

	processed, err := fixture.worker(t).ScanOnce(t.Context())
	require.NoError(t, err)
	assert.Equal(t, 2, processed)
	assert.Equal(t, 3, fixture.runtime.callCount(retry.Binding.Name))
	assert.Equal(t, store.EmbeddingFailureProviderUnavailable, fixture.catalog.failures[headKey(retry)])
	assert.Equal(t, store.EmbeddingFailureInputRejected, fixture.catalog.failures[headKey(capacity)])
	assert.LessOrEqual(t, fixture.totalWait, 15*time.Millisecond)
}

func TestEmbeddingWorkerSplitsBatchesByBytesAndMultiItemCapacity(t *testing.T) {
	fixture := newEmbeddingWorkerFixture(t)
	bytesWork := fixture.work("byte-batches", document.EmbeddingInputRenditionChunk, "byte-batches")
	bytesWork = mutateEmbeddingWorkerBinding(t, bytesWork, func(binding *document.EmbeddingBindingV1) {
		binding.MaxInputBytes = 40
	})
	capacityWork := fixture.work("capacity-bisect", document.EmbeddingInputRenditionChunk, "capacity-bisect")
	fixture.runtime.failures[capacityWork.Binding.Name] = []error{embeddingCapacityError{}}
	fixture.catalog.enqueue(bytesWork, capacityWork)

	processed, err := fixture.worker(t).ScanOnce(t.Context())
	require.NoError(t, err)
	assert.Equal(t, 2, processed)
	assert.Equal(t, []int{1, 1}, fixture.runtime.batchSizes(bytesWork.Binding.Name))
	assert.Equal(t, []int{2, 1, 1}, fixture.runtime.batchSizes(capacityWork.Binding.Name))
	assert.Contains(t, fixture.catalog.heads, headKey(bytesWork))
	assert.Contains(t, fixture.catalog.heads, headKey(capacityWork))
}

func TestEmbeddingWorkerCapacityBisectionReachesSingleItemBatch(t *testing.T) {
	fixture := newEmbeddingWorkerFixture(t)
	work := fixture.work("capacity-deep-bisect", document.EmbeddingInputRenditionChunk, "capacity-deep-bisect")
	work.Inputs = nil
	work.InputGeneration.Inputs = nil
	for index := range 8 {
		text := fmt.Sprintf("synthetic capacity item %d", index)
		inputID := workerHash(fmt.Sprintf("capacity-item-%d", index))
		work.Inputs = append(work.Inputs, document.EmbeddingInput{
			Key: inputID, Role: document.EmbeddingRoleDocument,
			Kind: document.EmbeddingInputRenditionChunk, Text: text,
		})
		work.InputGeneration.Inputs = append(work.InputGeneration.Inputs, store.EmbeddingInputReference{
			ID: inputID, RenderedChecksum: workerHash(work.Binding.ModelInput.EncodeDocument(text)),
		})
	}
	fixture.runtime.capacityAbove[work.Binding.Name] = 1
	fixture.catalog.enqueue(work)

	processed, err := fixture.worker(t).ScanOnce(t.Context())
	require.NoError(t, err)
	assert.Equal(t, 1, processed)
	assert.Contains(t, fixture.catalog.heads, headKey(work))
	assert.Equal(t, []int{8, 4, 2, 1, 1, 1, 1, 1, 1, 1, 1}, fixture.runtime.batchSizes(work.Binding.Name))
}

func TestEmbeddingWorkerCancellationDuringProviderAndPersistenceIsPrompt(t *testing.T) {
	for _, phase := range []string{"provider", "persistence"} {
		t.Run(phase, func(t *testing.T) {
			fixture := newEmbeddingWorkerFixture(t)
			work := fixture.work(phase, document.EmbeddingInputRenditionChunk, "semantic")
			fixture.catalog.enqueue(work)
			ctx, cancel := context.WithCancel(t.Context())
			if phase == "provider" {
				fixture.runtime.block = true
				fixture.runtime.onBlock = cancel
			} else {
				fixture.blobs.block = true
				fixture.blobs.onBlock = cancel
			}
			started := time.Now()
			_, err := fixture.worker(t).ScanOnce(ctx)
			require.ErrorIs(t, err, context.Canceled)
			assert.Less(t, time.Since(started), time.Second)
			assert.Empty(t, fixture.catalog.heads)
		})
	}
}

func TestEmbeddingWorkerRejectsCorruptOrPartiallyPersistedVectorSet(t *testing.T) {
	for _, phase := range []string{"checksum", "blob", "catalog"} {
		t.Run(phase, func(t *testing.T) {
			fixture := newEmbeddingWorkerFixture(t)
			work := fixture.work(phase, document.EmbeddingInputRenditionChunk, "semantic")
			fixture.catalog.enqueue(work)
			switch phase {
			case "checksum":
				fixture.blobs.corruptReceipt = true
			case "blob":
				fixture.blobs.writeErr = errors.New("private vector write token=secret")
			case "catalog":
				fixture.catalog.failStage = errors.New("private catalog token=secret")
			}
			_, err := fixture.worker(t).ScanOnce(t.Context())
			if phase == "checksum" {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
				assert.NotContains(t, err.Error(), "token=secret")
			}
			assert.Empty(t, fixture.catalog.heads)
		})
	}
}

func TestEmbeddingRuntimeRegistryAndRunLifecycle(t *testing.T) {
	registry := NewEmbeddingRuntimeRegistry()
	assert.False(t, registry.Ready())
	fixture := newEmbeddingWorkerFixture(t)
	work := fixture.work("registry", document.EmbeddingInputRenditionChunk, "semantic")
	require.NoError(t, registry.Register(work.Descriptor.Fingerprint, fixture.runtime))
	assert.True(t, registry.Ready())
	fixture.catalog.enqueue(work)
	fixture.runtimeRegistry = registry
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	worker := fixture.worker(t)
	go func() { done <- worker.Run(ctx) }()
	require.Eventually(t, func() bool { return fixture.catalog.headCount() == 1 }, time.Second, time.Millisecond)
	cancel()
	require.ErrorIs(t, <-done, context.Canceled)
}

func TestEmbeddingWorkerClaimsOnlyAfterMutationGateAdmission(t *testing.T) {
	fixture := newEmbeddingWorkerFixture(t)
	work := fixture.work("gate-admission", document.EmbeddingInputRenditionChunk, "semantic")
	fixture.catalog.enqueue(work)
	held := make(chan struct{})
	release := make(chan struct{})
	maintenanceDone := make(chan error, 1)
	go func() {
		maintenanceDone <- fixture.gate.MaintainContext(t.Context(), func() error {
			close(held)
			<-release
			return nil
		})
	}()
	<-held
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	worker := fixture.worker(t)
	go func() {
		_, err := worker.ScanOnce(ctx)
		done <- err
	}()
	require.Never(t, func() bool { return fixture.catalog.claimCount() != 0 }, 100*time.Millisecond, 5*time.Millisecond)
	cancel()
	require.ErrorIs(t, <-done, context.Canceled)
	close(release)
	require.NoError(t, <-maintenanceDone)
}

func TestEmbeddingWorkerReconcilesDurableAuthorityBeforeClaim(t *testing.T) {
	fixture := newEmbeddingWorkerFixture(t)
	fixture.catalog.enqueue(fixture.work("reconcile-before-claim", document.EmbeddingInputRenditionChunk, "semantic"))
	processed, err := fixture.worker(t).ScanOnce(t.Context())
	require.NoError(t, err)
	require.Equal(t, 1, processed)
	assert.Equal(t, []string{"reconcile", "claim", "claim"}, fixture.catalog.catalogEvents())
}

func TestEmbeddingRuntimeRegistryClassifiesWithExactExecutingRuntime(t *testing.T) {
	fixture := newEmbeddingWorkerFixture(t)
	work := fixture.work("exact-classifier", document.EmbeddingInputRenditionChunk, "semantic")
	fixture.runtime.failures[work.Binding.Name] = []error{embeddingPermanentError{}}
	registry := NewEmbeddingRuntimeRegistry()
	require.NoError(t, registry.Register(work.Descriptor.Fingerprint, fixture.runtime))
	require.NoError(t, registry.Register(workerHash("misleading-runtime"), embeddingTransientClassifierRuntime{}))
	fixture.runtimeRegistry = registry
	fixture.catalog.enqueue(work)

	processed, err := fixture.worker(t).ScanOnce(t.Context())
	require.NoError(t, err)
	assert.Equal(t, 1, processed)
	assert.Equal(t, 1, fixture.runtime.callCount(work.Binding.Name))
	assert.Equal(t, store.EmbeddingFailureInputRejected, fixture.catalog.failures[headKey(work)])
}

type embeddingWorkerFixture struct {
	t               *testing.T
	now             time.Time
	descriptor      document.EmbeddingDescriptor
	profile         store.ProcessingProfileRecord
	catalog         *embeddingWorkerFakeCatalog
	blobs           *embeddingWorkerFakeBlobs
	runtime         *embeddingWorkerFakeRuntime
	runtimeRegistry EmbeddingRuntime
	gate            *api.OperationGate
	totalWait       time.Duration
}

func newEmbeddingWorkerFixture(t *testing.T) *embeddingWorkerFixture {
	t.Helper()
	descriptor := embeddingWorkerDescriptor(t)
	profile := embeddingWorkerProfile(t, descriptor)
	fixture := &embeddingWorkerFixture{
		t: t, now: time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC), descriptor: descriptor,
		profile: profile, catalog: newEmbeddingWorkerFakeCatalog(), blobs: &embeddingWorkerFakeBlobs{},
		runtime: &embeddingWorkerFakeRuntime{failures: map[string][]error{}, capacityAbove: map[string]int{}, mutate: map[string]func(document.EmbeddingResult) document.EmbeddingResult{}, callsByBinding: map[string]int{}, preparesByBinding: map[string]int{}, batchesByBinding: map[string][]int{}},
		gate:    api.NewOperationGate(),
	}
	fixture.runtimeRegistry = fixture.runtime
	return fixture
}

func (fixture *embeddingWorkerFixture) worker(t *testing.T) *EmbeddingWorker {
	t.Helper()
	worker, err := NewEmbeddingWorker(EmbeddingWorkerConfig{
		Catalog: fixture.catalog, Authority: fixture.catalog, Blobs: fixture.blobs,
		GenerationBlobs: &embeddingRuntimeTestBlobs{data: []byte("unused")}, Runtime: fixture.runtimeRegistry,
		Gate: fixture.gate, Owner: "embedding-worker-test", LeaseDuration: 3 * time.Second,
		IdleDelay: time.Millisecond, RetryLimit: 3, RetryBaseDelay: time.Millisecond,
		MaxRetryDelay: 5 * time.Millisecond, AttemptLifetime: time.Minute,
		MaxRows: 100, MaxDimensions: 16, MaxVectorBlobBytes: 1 << 20,
		DescriptorFingerprints: []string{fixture.descriptor.Fingerprint},
		Clock:                  func() time.Time { return fixture.now },
		Wait: func(ctx context.Context, delay time.Duration) error {
			fixture.totalWait += delay
			select {
			case <-ctx.Done():
				return ctx.Err()
			default:
				return nil
			}
		},
	})
	require.NoError(t, err)
	return worker
}

func (fixture *embeddingWorkerFixture) work(seed string, kind document.EmbeddingInputKind, binding string) EmbeddingWork {
	fixture.t.Helper()
	versionID := "00000000-0000-4000-8000-" + workerHex(seed)[:12]
	inputRefs := []store.EmbeddingInputReference{
		{ID: workerHash(seed + "-1"), RenderedChecksum: workerHash("document: synthetic input one")},
		{ID: workerHash(seed + "-2"), RenderedChecksum: workerHash("document: synthetic input two")},
	}
	inputs := []document.EmbeddingInput{
		{Key: inputRefs[0].ID, Role: document.EmbeddingRoleDocument, Kind: kind, Text: "synthetic input one"},
		{Key: inputRefs[1].ID, Role: document.EmbeddingRoleDocument, Kind: kind, Text: "synthetic input two"},
	}
	if kind == document.EmbeddingInputOriginalFile {
		inputs = []document.EmbeddingInput{{Key: versionID, Role: document.EmbeddingRoleDocument, Kind: kind, Source: &embeddingWorkerUpload{data: []byte("synthetic original")}}}
		inputRefs = []store.EmbeddingInputReference{{ID: versionID, RenderedChecksum: workerHash("synthetic original")}}
	}
	var selected document.EmbeddingBindingV1
	var profile document.ProcessingProfileV1
	require.NoError(fixture.t, jsonUnmarshalProfile(fixture.profile.CanonicalProfile, &profile))
	for _, candidate := range profile.Embeddings {
		if candidate.InputKind == kind {
			selected = candidate
			break
		}
	}
	selected.Name = binding
	profile.Embeddings = []document.EmbeddingBindingV1{selected}
	canonical, fingerprints, err := document.CanonicalProfile(profile)
	require.NoError(fixture.t, err)
	processingProfile := store.ProcessingProfileRecord{
		Fingerprint: fingerprints.Profile, CanonicalProfile: canonical,
		RenditionRequestFingerprint:    fingerprints.RenditionRequest,
		EvidenceLexicalFingerprint:     fingerprints.EvidenceLexical,
		RetentionDisclosureFingerprint: fingerprints.RetentionDisclosure,
		AttachmentPolicyFingerprint:    profile.RetentionDisclosure.AttachmentPolicyFingerprint,
		ConsentFingerprint:             profile.RetentionDisclosure.ConsentFingerprint,
		TrustBoundary:                  profile.RetentionDisclosure.TrustBoundary,
	}
	return EmbeddingWork{
		VaultID: "00000000-0000-4000-8000-000000000001", ContentVersionID: versionID,
		ProcessingProfile: processingProfile, Binding: selected, Descriptor: fixture.descriptor,
		VectorSpaceID: fingerprints.VectorSpace[binding], EmbeddingInputFingerprint: fingerprints.EmbeddingInput[binding], Inputs: inputs,
		InputGeneration: store.EmbeddingInputGenerationRecord{
			ID: workerHash(seed + "-generation"), SourceVersionID: versionID,
			ProcessingProfileFingerprint: processingProfile.Fingerprint,
			EvidenceFingerprint:          workerHash(seed + "-evidence"), TokenizerFingerprint: workerHash(seed + "-tokenizer"),
			ChunkPolicyFingerprint: workerHash(seed + "-chunk"), FormatterFingerprint: workerHash(seed + "-formatter"),
			GenerationChecksum: workerHash(seed + "-generation-checksum"), Inputs: inputRefs,
			CreatedAt: fixture.now.Format("2006-01-02T15:04:05.000000000Z"),
		},
		Consent: store.ProviderOperationAuthorizationRequest{
			Principal: "operator:synthetic", Scope: "embedding:" + binding,
			ProfileFingerprint: processingProfile.Fingerprint, DisclosureFingerprint: selected.DisclosureFingerprint,
			InputClasses: []string{string(kind)}, RetainedArtifactClasses: []string{"embedding_vector_set"},
		},
	}
}

func mutateEmbeddingWorkerBinding(t *testing.T, work EmbeddingWork,
	mutate func(*document.EmbeddingBindingV1),
) EmbeddingWork {
	t.Helper()
	var profile document.ProcessingProfileV1
	require.NoError(t, json.Unmarshal(work.ProcessingProfile.CanonicalProfile, &profile))
	require.Len(t, profile.Embeddings, 1)
	mutate(&profile.Embeddings[0])
	canonical, fingerprints, err := document.CanonicalProfile(profile)
	require.NoError(t, err)
	work.Binding = profile.Embeddings[0]
	work.ProcessingProfile = store.ProcessingProfileRecord{Fingerprint: fingerprints.Profile,
		CanonicalProfile: canonical, RenditionRequestFingerprint: fingerprints.RenditionRequest,
		EvidenceLexicalFingerprint:     fingerprints.EvidenceLexical,
		RetentionDisclosureFingerprint: fingerprints.RetentionDisclosure,
		AttachmentPolicyFingerprint:    profile.RetentionDisclosure.AttachmentPolicyFingerprint,
		ConsentFingerprint:             profile.RetentionDisclosure.ConsentFingerprint,
		TrustBoundary:                  profile.RetentionDisclosure.TrustBoundary}
	work.VectorSpaceID = fingerprints.VectorSpace[work.Binding.Name]
	work.EmbeddingInputFingerprint = fingerprints.EmbeddingInput[work.Binding.Name]
	work.InputGeneration.ProcessingProfileFingerprint = fingerprints.Profile
	work.Consent.ProfileFingerprint = fingerprints.Profile
	return work
}

// The rest of this file is a deterministic authority harness. Assertions are
// made on worker-visible catalog state, never on fake call existence alone.
type embeddingWorkerFakeCatalog struct {
	mu                              sync.Mutex
	queue                           []EmbeddingWork
	nextEpoch                       int64
	heads                           map[string]store.EmbeddingHeadRecord
	failures                        map[string]store.EmbeddingFailureCode
	staged                          map[string]store.EmbeddingSetRecord
	receipts                        []EmbeddingAttemptReceipt
	authorizeCalls, validateCalls   int
	failAuthorizeAt, failValidateAt int
	failPublish, failStage          error
	failFinish                      error
	failFailure                     error
	events                          []string
}

func newEmbeddingWorkerFakeCatalog() *embeddingWorkerFakeCatalog {
	return &embeddingWorkerFakeCatalog{heads: map[string]store.EmbeddingHeadRecord{}, failures: map[string]store.EmbeddingFailureCode{}, staged: map[string]store.EmbeddingSetRecord{}}
}

func (c *embeddingWorkerFakeCatalog) enqueue(work ...EmbeddingWork) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.queue = append(c.queue, work...)
}
func (c *embeddingWorkerFakeCatalog) ReconcileEmbeddingJobs(context.Context, store.EmbeddingReconcileRequest) (store.EmbeddingReconcileResult, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.events = append(c.events, "reconcile")
	return store.EmbeddingReconcileResult{}, nil
}
func (c *embeddingWorkerFakeCatalog) ClaimNextEmbeddingWork(_ context.Context, owner string, now time.Time, lease time.Duration) (EmbeddingWorkClaim, EmbeddingWork, bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.events = append(c.events, "claim")
	for len(c.queue) > 0 {
		work := c.queue[0]
		c.queue = c.queue[1:]
		key := headKey(work)
		if _, done := c.heads[key]; done {
			continue
		}
		c.nextEpoch++
		return EmbeddingWorkClaim{AttemptID: workerHash(key), Owner: owner, Epoch: c.nextEpoch, LeaseExpiresAt: now.Add(lease)}, work, true, nil
	}
	return EmbeddingWorkClaim{}, EmbeddingWork{}, false, nil
}
func (c *embeddingWorkerFakeCatalog) catalogEvents() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.events)
}
func (c *embeddingWorkerFakeCatalog) RenewEmbeddingWork(_ context.Context, claim EmbeddingWorkClaim, now time.Time, lease time.Duration) (EmbeddingWorkClaim, error) {
	claim.LeaseExpiresAt = now.Add(lease)
	return claim, nil
}
func (c *embeddingWorkerFakeCatalog) ValidateEmbeddingWork(_ context.Context, _ EmbeddingWorkClaim, _ EmbeddingWork, _ time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.validateCalls++
	if c.validateCalls == c.failValidateAt {
		return ErrEmbeddingWorkFenced
	}
	return nil
}
func (c *embeddingWorkerFakeCatalog) ReauthorizeEmbeddingWork(_ context.Context, _ EmbeddingWorkClaim, work EmbeddingWork, prior *store.ProviderOperationAuthorization, now time.Time) (store.ProviderOperationAuthorization, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.authorizeCalls++
	if c.authorizeCalls == c.failAuthorizeAt {
		return store.ProviderOperationAuthorization{}, store.ErrProcessingConsentRevoked
	}
	return store.ProviderOperationAuthorization{GrantID: "00000000-0000-4000-8000-000000000002", ProcessingIncarnationID: "00000000-0000-4000-8000-000000000003", RevocationFence: 0, AuthorizedAt: now}, nil
}
func (c *embeddingWorkerFakeCatalog) RecordRenditionBlob(_ context.Context, hash string, size int64, _ store.BlobPhysical) error {
	if hash == "" || size == 0 {
		return errors.New("invalid receipt")
	}
	return nil
}
func (c *embeddingWorkerFakeCatalog) StageEmbeddingSetWithLease(_ context.Context, record store.EmbeddingSetRecord, _ string, _ int64, _ time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.failStage != nil {
		err := c.failStage
		c.failStage = nil
		return err
	}
	c.staged[record.ID] = record
	return nil
}
func (c *embeddingWorkerFakeCatalog) PublishEmbeddingHeadWithLease(_ context.Context, head store.EmbeddingHeadRecord,
	_ store.ProviderOperationAuthorizationRequest, authorization store.ProviderOperationAuthorization,
	_ string, _ int64, _ time.Time) (store.ProviderOperationAuthorization, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.failPublish != nil {
		err := c.failPublish
		c.failPublish = nil
		return store.ProviderOperationAuthorization{}, err
	}
	c.heads[headKeyParts(head.Key.ContentVersionID, head.Key.BindingID, head.Key.InputKind)] = head
	return authorization, nil
}
func (c *embeddingWorkerFakeCatalog) FinishEmbeddingWork(_ context.Context, _ EmbeddingWorkClaim, receipt EmbeddingAttemptReceipt, _ time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.failFinish != nil {
		err := c.failFinish
		c.failFinish = nil
		return err
	}
	c.receipts = append(c.receipts, receipt)
	return nil
}
func (c *embeddingWorkerFakeCatalog) FailEmbeddingWork(_ context.Context, _ EmbeddingWorkClaim, work EmbeddingWork, code store.EmbeddingFailureCode, receipt EmbeddingAttemptReceipt, _ time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.failFailure != nil {
		err := c.failFailure
		c.failFailure = nil
		return err
	}
	c.failures[headKey(work)] = code
	c.receipts = append(c.receipts, receipt)
	return nil
}
func (c *embeddingWorkerFakeCatalog) publishedBindings() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	result := make([]string, 0, len(c.heads))
	for _, head := range c.heads {
		result = append(result, head.Key.BindingID)
	}
	slicesSort(result)
	return result
}
func (c *embeddingWorkerFakeCatalog) receiptsText() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return receiptText(c.receipts)
}

func (c *embeddingWorkerFakeCatalog) headCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.heads)
}

func (c *embeddingWorkerFakeCatalog) claimCount() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.nextEpoch
}

func (c *embeddingWorkerFakeCatalog) onlyStaged(t *testing.T) store.EmbeddingSetRecord {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	require.Len(t, c.staged, 1)
	for _, value := range c.staged {
		return value
	}
	return store.EmbeddingSetRecord{}
}

type embeddingWorkerFakeRuntime struct {
	mu                sync.Mutex
	failures          map[string][]error
	capacityAbove     map[string]int
	mutate            map[string]func(document.EmbeddingResult) document.EmbeddingResult
	callsByBinding    map[string]int
	preparesByBinding map[string]int
	block             bool
	onBlock           func()
	reopenOriginal    bool
	consumeOriginal   bool
	deadlineSeen      bool
	batchesByBinding  map[string][]int
}

func (r *embeddingWorkerFakeRuntime) Ready() bool { return true }
func (r *embeddingWorkerFakeRuntime) Prepare(_ context.Context, work EmbeddingWork) (EmbeddingExecution, error) {
	r.mu.Lock()
	r.preparesByBinding[work.Binding.Name]++
	reopen := r.reopenOriginal
	r.mu.Unlock()
	inputs := slices.Clone(work.Inputs)
	if reopen {
		for index := range inputs {
			if upload, ok := inputs[index].Source.(*embeddingWorkerUpload); ok {
				inputs[index].Source = &embeddingWorkerUpload{data: slices.Clone(upload.data)}
			}
		}
	}
	return EmbeddingExecution{Provider: &embeddingWorkerProvider{runtime: r, binding: work.Binding.Name, descriptor: work.Descriptor},
		Inputs: inputs, InputGenerationJSON: slices.Clone(work.InputGeneration.GenerationJSON), Classify: r.Classify}, nil
}
func (r *embeddingWorkerFakeRuntime) Classify(err error) (EmbeddingProviderFailure, time.Duration) {
	var transient embeddingTransientError
	switch {
	case errors.As(err, &transient):
		return EmbeddingProviderTransient, transient.retryAfter
	case errors.As(err, &embeddingCapacityError{}):
		return EmbeddingProviderCapacity, 0
	default:
		return EmbeddingProviderPermanent, 0
	}
}
func (r *embeddingWorkerFakeRuntime) calls() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	total := 0
	for _, n := range r.callsByBinding {
		total += n
	}
	return total
}
func (r *embeddingWorkerFakeRuntime) callCount(binding string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.callsByBinding[binding]
}
func (r *embeddingWorkerFakeRuntime) prepareCount(binding string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.preparesByBinding[binding]
}
func (r *embeddingWorkerFakeRuntime) sawDeadline() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.deadlineSeen
}

func (r *embeddingWorkerFakeRuntime) batchSizes(binding string) []int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.batchesByBinding[binding])
}

type embeddingTransientClassifierRuntime struct{}

func (embeddingTransientClassifierRuntime) Ready() bool { return true }
func (embeddingTransientClassifierRuntime) Prepare(context.Context, EmbeddingWork) (EmbeddingExecution, error) {
	return EmbeddingExecution{}, errors.New("misleading runtime must never prepare exact work")
}
func (embeddingTransientClassifierRuntime) Classify(error) (EmbeddingProviderFailure, time.Duration) {
	return EmbeddingProviderTransient, 0
}

type embeddingWorkerProvider struct {
	runtime    *embeddingWorkerFakeRuntime
	binding    string
	descriptor document.EmbeddingDescriptor
}

func (p *embeddingWorkerProvider) Descriptor() document.EmbeddingDescriptor { return p.descriptor }
func (p *embeddingWorkerProvider) Embed(ctx context.Context, inputs []document.EmbeddingInput, _ document.EmbeddingAuthorization) (document.EmbeddingResult, error) {
	p.runtime.mu.Lock()
	p.runtime.callsByBinding[p.binding]++
	p.runtime.batchesByBinding[p.binding] = append(p.runtime.batchesByBinding[p.binding], len(inputs))
	call := p.runtime.callsByBinding[p.binding]
	block, onBlock := p.runtime.block, p.runtime.onBlock
	failures := p.runtime.failures[p.binding]
	capacityAbove := p.runtime.capacityAbove[p.binding]
	mutate := p.runtime.mutate[p.binding]
	consumeOriginal := p.runtime.consumeOriginal
	_, p.runtime.deadlineSeen = ctx.Deadline()
	p.runtime.mu.Unlock()
	if block {
		if onBlock != nil {
			onBlock()
		}
		<-ctx.Done()
		return document.EmbeddingResult{}, ctx.Err()
	}
	if consumeOriginal {
		for _, input := range inputs {
			if input.Source == nil {
				continue
			}
			data, err := io.ReadAll(input.Source)
			_ = input.Source.Close()
			if err != nil || len(data) == 0 {
				return document.EmbeddingResult{}, embeddingPermanentError{}
			}
		}
	}
	if capacityAbove > 0 && len(inputs) > capacityAbove {
		return document.EmbeddingResult{}, embeddingCapacityError{}
	}
	if call <= len(failures) {
		return document.EmbeddingResult{}, failures[call-1]
	}
	result := document.EmbeddingResult{Vectors: make([]document.EmbeddingVector, len(inputs))}
	for i, input := range inputs {
		result.Vectors[i] = document.EmbeddingVector{Key: input.Key, Values: []float32{float32(i + 1), 2}}
	}
	if mutate != nil {
		result = mutate(result)
	}
	return result, nil
}

type embeddingWorkerFakeBlobs struct {
	block          bool
	onBlock        func()
	corruptReceipt bool
	writeErr       error
	deadlineSeen   bool
}

type embeddingRuntimeTestBlobs struct {
	data  []byte
	opens int
}

func (blobs *embeddingRuntimeTestBlobs) OpenContext(context.Context, string) (io.ReadSeekCloser, error) {
	blobs.opens++
	return &embeddingRuntimeReadSeekCloser{Reader: bytes.NewReader(blobs.data)}, nil
}

type embeddingRuntimeReadSeekCloser struct{ *bytes.Reader }

func (*embeddingRuntimeReadSeekCloser) Close() error { return nil }

func (b *embeddingWorkerFakeBlobs) WithMutation(_ context.Context, fn func() error) error {
	return fn()
}
func (b *embeddingWorkerFakeBlobs) WriteDetailedContext(ctx context.Context, reader io.Reader) (blob.WriteReceipt, error) {
	_, b.deadlineSeen = ctx.Deadline()
	if b.block {
		if b.onBlock != nil {
			b.onBlock()
		}
		<-ctx.Done()
		return blob.WriteReceipt{}, ctx.Err()
	}
	data, err := io.ReadAll(reader)
	if err != nil {
		return blob.WriteReceipt{}, err
	}
	hash := workerHashBytes(data)
	if b.corruptReceipt {
		hash = workerHash("corrupt")
	}
	return blob.WriteReceipt{Hash: hash, Size: int64(len(data)), StoredSize: int64(len(data)), Encoding: packstore.LooseEncodingRaw, Created: true}, b.writeErr
}
func (b *embeddingWorkerFakeBlobs) sawDeadline() bool { return b.deadlineSeen }

type embeddingWorkerUpload struct {
	data   []byte
	offset int
}

func (u *embeddingWorkerUpload) Read(p []byte) (int, error) {
	if u.offset >= len(u.data) {
		return 0, io.EOF
	}
	n := copy(p, u.data[u.offset:])
	u.offset += n
	return n, nil
}
func (u *embeddingWorkerUpload) Close() error { return nil }
func (u *embeddingWorkerUpload) Metadata() document.AuthorizedUploadMetadata {
	return document.AuthorizedUploadMetadata{Filename: "synthetic.pdf", MediaFamily: "pdf", MediaType: "application/pdf",
		ByteLength: int64(len(u.data)), SHA256: workerHashBytes(u.data), CapabilityRecordChecksum: workerHash("capability"),
		ProviderMetadataChecksum: workerHash("provider-metadata"), InputKind: document.RenditionInputOriginalFile}
}

type embeddingTransientError struct{ retryAfter time.Duration }

func (e embeddingTransientError) Error() string { return "synthetic transient" }

type embeddingPermanentError struct{}

func (embeddingPermanentError) Error() string { return "synthetic permanent" }

type embeddingCapacityError struct{}

func (embeddingCapacityError) Error() string { return "synthetic capacity" }

func headKey(work EmbeddingWork) string {
	return headKeyParts(work.ContentVersionID, work.Binding.Name, work.Binding.InputKind)
}
func headKeyParts(version, binding string, kind document.EmbeddingInputKind) string {
	return version + "\x00" + binding + "\x00" + string(kind)
}
func workerHash(value string) string { return workerHashBytes([]byte(value)) }
func workerHex(value string) string  { return workerHash(value) }

func embeddingWorkerDescriptor(t *testing.T) document.EmbeddingDescriptor {
	t.Helper()
	contract, err := document.NewModelInputContract(document.ModelInputContractConfig{Profile: document.ModelInputProfileCustom, CompatibilityID: "synthetic-space", Document: document.ModelInputEncoder{Mode: document.ModelInputModeText, Template: "document: {{content}}"}, Query: document.ModelInputEncoder{Mode: document.ModelInputModeText, Template: "query: {{content}}"}})
	require.NoError(t, err)
	descriptor, err := document.NewEmbeddingDescriptor(document.EmbeddingDescriptor{ID: "synthetic", ContractVersion: document.EmbeddingProviderContractVersion, PolicyFingerprint: workerHash("policy"), TrustBoundary: document.EmbeddingTrustLocalProcess, Model: "synthetic", ModelRevision: "v1", Dimension: 2, Metric: document.VectorMetricCosine, Normalization: document.VectorNormalizationNone, ScalarEncoding: "float32", DocumentFormatter: "document/v1", QueryFormatter: "query/v1", InputKinds: []document.EmbeddingInputKind{document.EmbeddingInputOriginalFile, document.EmbeddingInputRenditionChunk}, CompatibilityID: "synthetic-space", SupportsTextQuery: true, ModelInput: contract, SupportedRequestModes: []document.ModelInputMode{document.ModelInputModeText}})
	require.NoError(t, err)
	return descriptor
}

func embeddingWorkerProfile(t *testing.T, descriptor document.EmbeddingDescriptor) store.ProcessingProfileRecord {
	t.Helper()
	makeBinding := func(name string, kind document.EmbeddingInputKind) document.EmbeddingBindingV1 {
		binding := document.EmbeddingBindingV1{Activation: document.EmbeddingOptional, AuthorizationFingerprint: workerHash("auth"), CompatibilityID: descriptor.CompatibilityID, CredentialBinding: "credential:synthetic", Descriptor: document.ProviderDescriptorV1{ID: descriptor.ID, Fingerprint: descriptor.Fingerprint}, Dimensions: descriptor.Dimension, DisclosureFingerprint: workerHash("disclosure"), DocumentFormatter: descriptor.DocumentFormatter, InputKind: kind, MaxBatchItems: 8, MaxInputBytes: 1 << 20, MaxResponseBytes: 1 << 20, Metric: descriptor.Metric, ModelInput: descriptor.ModelInput, Model: descriptor.Model, Name: name, Normalization: descriptor.Normalization, QueryFormatter: descriptor.QueryFormatter, ScalarEncoding: descriptor.ScalarEncoding, TrustBoundary: string(descriptor.TrustBoundary)}
		if kind == document.EmbeddingInputRenditionChunk {
			binding.Chunk = &document.EmbeddingChunkPolicyV1{ContextFingerprint: workerHash("context"), Formatter: "synthetic/v1", MaxTokens: 128, OverlapTokens: 8, Tokenizer: "synthetic@v1", TruncationPolicy: string(document.TruncationPolicyReject)}
		}
		return binding
	}
	profile := document.ProcessingProfileV1{ContractVersion: document.ProcessingProfileContractV1,
		Rendition: &document.RenditionBindingV1{AdapterContract: "synthetic/v1", AuthorizationFingerprint: workerHash("rendition-auth"),
			CredentialBinding: "credential:synthetic", DeploymentFingerprint: workerHash("deployment"),
			Descriptor:            document.ProviderDescriptorV1{ID: "synthetic-rendition", Fingerprint: workerHash("rendition-descriptor")},
			DisclosureFingerprint: workerHash("rendition-disclosure"), MaxDocumentBytes: 1 << 20, MaxResponseBytes: 1 << 20,
			MaxUnits: 100, Name: "synthetic", RequestedArtifacts: []document.EvidenceArtifactRole{document.EvidenceArtifactStructured},
			TrustBoundary: "local_process", UploadOptionsFingerprint: workerHash("upload-options")},
		EvidenceLexical: document.EvidenceLexicalPolicyV1{CompletenessFingerprint: workerHash("complete"), LexicalSegmenterFingerprint: workerHash("lexical"), MaxSegmentRunes: 1000, MaxUnitRunes: 1000, NormalizedEvidenceContract: document.NormalizedEvidenceContractV1, NormalizerFingerprint: workerHash("normalizer"), RenditionContract: document.RenditionContractV1, SanitizerFingerprint: workerHash("sanitizer"), SourceEvidenceContract: document.SourceEvidenceContractV1}, RetentionDisclosure: document.RetentionDisclosurePolicyV1{AttachmentPolicyFingerprint: workerHash("attachment"), ConsentFingerprint: workerHash("consent"), TrustBoundary: string(document.EmbeddingTrustLocalProcess)}, Retrieval: document.RetrievalPolicyV1{LexicalLimit: 10, VectorLimit: 10}, Embeddings: []document.EmbeddingBindingV1{makeBinding("chunk", document.EmbeddingInputRenditionChunk), makeBinding("direct", document.EmbeddingInputOriginalFile)}}
	canonical, fingerprints, err := document.CanonicalProfile(profile)
	require.NoError(t, err)
	return store.ProcessingProfileRecord{Fingerprint: fingerprints.Profile, CanonicalProfile: canonical, EvidenceLexicalFingerprint: fingerprints.EvidenceLexical, RetentionDisclosureFingerprint: fingerprints.RetentionDisclosure, AttachmentPolicyFingerprint: profile.RetentionDisclosure.AttachmentPolicyFingerprint, ConsentFingerprint: profile.RetentionDisclosure.ConsentFingerprint, TrustBoundary: profile.RetentionDisclosure.TrustBoundary}
}

func jsonUnmarshalProfile(data []byte, profile *document.ProcessingProfileV1) error {
	return json.Unmarshal(data, profile)
}
func slicesSort(values []string) { slices.Sort(values) }
func receiptText(receipts []EmbeddingAttemptReceipt) string {
	var buffer bytes.Buffer
	for _, receipt := range receipts {
		buffer.WriteString(receipt.AttemptID)
		buffer.WriteString(receipt.ProviderFingerprint)
		buffer.WriteString(receipt.FailureCode)
	}
	return buffer.String()
}
