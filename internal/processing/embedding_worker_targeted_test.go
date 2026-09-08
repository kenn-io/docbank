package processing

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/store"
)

func TestEmbeddingWorkerRunJobPublishesOnlyExactJob(t *testing.T) {
	fixture, fake, worker, request := newRealEmbeddingWorker(t, document.EmbeddingInputOriginalFile)
	target, err := fixture.catalog.EnqueueEmbeddingJob(t.Context(), request)
	require.NoError(t, err)

	// Put the target into retry_wait so the separately enqueued request is the
	// earlier eligible queue entry at the targeted worker's clock.
	setupAt := time.Now().UTC().Add(time.Minute)
	claim, work, found, err := fixture.catalog.ClaimEmbeddingWork(t.Context(), target.ID,
		"targeted-setup", setupAt, time.Minute, worker.descriptorFingerprints)
	require.NoError(t, err)
	require.True(t, found)
	require.NoError(t, fixture.catalog.FailEmbeddingWork(t.Context(), claim, work,
		store.EmbeddingFailureProviderUnavailable,
		store.EmbeddingAttemptReceipt{AttemptID: target.ID}, setupAt.Add(time.Second)))

	unrelatedRequest := request
	unrelatedRequest.InputGeneration.ID = workerHash("targeted-earlier-generation")
	unrelated, err := fixture.catalog.EnqueueEmbeddingJob(t.Context(), unrelatedRequest)
	require.NoError(t, err)
	runAt := setupAt.Add(2 * time.Minute)
	worker.clock = func() time.Time { return runAt }
	worker.reconcileAfter = "targeted-cursor-sentinel"

	processed, err := worker.RunJob(t.Context(), target.ID)
	require.NoError(t, err)
	require.True(t, processed)
	require.Equal(t, 1, fake.runtime.calls())
	require.Equal(t, "targeted-cursor-sentinel", worker.reconcileAfter)

	head := targetedEmbeddingHead(t, fixture.catalog, request)
	assert.Equal(t, request.ContentVersionID, head.ContentVersionID)
	assert.Equal(t, request.BindingID, head.BindingID)
	assert.Equal(t, request.Profile.Fingerprint, head.ProfileFingerprint)
	assert.Equal(t, document.EmbeddingInputOriginalFile, head.InputKind)

	unrelatedClaim, _, unrelatedFound, err := fixture.catalog.ClaimEmbeddingWork(
		t.Context(), unrelated.ID, "unrelated-worker", runAt, time.Minute,
		worker.descriptorFingerprints)
	require.NoError(t, err)
	require.True(t, unrelatedFound, "targeted execution must leave the earlier queue entry unclaimed")
	assert.Equal(t, unrelated.ID, unrelatedClaim.AttemptID)
}

func TestEmbeddingWorkerRunJobEntryAndEligibility(t *testing.T) {
	t.Run("nil worker", func(t *testing.T) {
		var worker *EmbeddingWorker
		processed, err := worker.RunJob(t.Context(), workerHash("nil-target"))
		require.EqualError(t, err, "embedding worker is nil")
		assert.False(t, processed)
	})

	t.Run("targeted interface unavailable", func(t *testing.T) {
		fixture := newEmbeddingWorkerFixture(t)
		worker := fixture.worker(t)
		processed, err := worker.RunJob(t.Context(), workerHash("unsupported-target"))
		require.EqualError(t, err, "embedding catalog does not support targeted claims")
		assert.False(t, processed)
		assert.Zero(t, fixture.runtime.calls())
	})

	for _, testCase := range []struct {
		name    string
		jobID   func(store.EmbeddingJob) string
		setup   func(*testing.T, publicationFixture, *embeddingWorkerFixture, *EmbeddingWorker, store.EmbeddingJob)
		wantErr error
	}{
		{
			name:  "absent",
			jobID: func(store.EmbeddingJob) string { return workerHash("absent-target") },
		},
		{
			name:    "malformed",
			jobID:   func(store.EmbeddingJob) string { return "not-a-job-id" },
			wantErr: ErrEmbeddingPersistence,
		},
		{
			name: "runtime mismatch",
			setup: func(_ *testing.T, _ publicationFixture, _ *embeddingWorkerFixture,
				worker *EmbeddingWorker, _ store.EmbeddingJob,
			) {
				worker.descriptorFingerprints = []string{workerHash("unavailable-runtime")}
			},
		},
		{
			name: "active lease",
			setup: func(t *testing.T, fixture publicationFixture, _ *embeddingWorkerFixture,
				worker *EmbeddingWorker, job store.EmbeddingJob,
			) {
				t.Helper()
				_, _, found, err := fixture.catalog.ClaimEmbeddingWork(t.Context(), job.ID,
					"active-worker", worker.clock().UTC(), time.Minute,
					worker.descriptorFingerprints)
				require.NoError(t, err)
				require.True(t, found)
			},
		},
		{
			name: "delayed retry",
			setup: func(t *testing.T, fixture publicationFixture, _ *embeddingWorkerFixture,
				worker *EmbeddingWorker, job store.EmbeddingJob,
			) {
				t.Helper()
				now := time.Now().UTC().Add(time.Minute)
				claim, work, found, err := fixture.catalog.ClaimEmbeddingWork(t.Context(), job.ID,
					"retry-worker", now, time.Minute, worker.descriptorFingerprints)
				require.NoError(t, err)
				require.True(t, found)
				require.NoError(t, fixture.catalog.FailEmbeddingWork(t.Context(), claim, work,
					store.EmbeddingFailureProviderUnavailable,
					store.EmbeddingAttemptReceipt{AttemptID: job.ID}, now.Add(time.Second)))
				worker.clock = func() time.Time { return now.Add(2 * time.Second) }
			},
		},
		{
			name: "terminal",
			setup: func(t *testing.T, fixture publicationFixture, _ *embeddingWorkerFixture,
				worker *EmbeddingWorker, job store.EmbeddingJob,
			) {
				t.Helper()
				now := time.Now().UTC().Add(time.Minute)
				claim, _, found, err := fixture.catalog.ClaimEmbeddingWork(t.Context(), job.ID,
					"terminal-worker", now, time.Minute, worker.descriptorFingerprints)
				require.NoError(t, err)
				require.True(t, found)
				require.NoError(t, fixture.catalog.FinishEmbeddingWork(t.Context(), claim,
					store.EmbeddingAttemptReceipt{AttemptID: job.ID}, now.Add(time.Second)))
				worker.clock = func() time.Time { return now.Add(2 * time.Second) }
			},
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			fixture, fake, worker, request := newRealEmbeddingWorker(
				t, document.EmbeddingInputOriginalFile)
			job, err := fixture.catalog.EnqueueEmbeddingJob(t.Context(), request)
			require.NoError(t, err)
			worker.reconcileAfter = "unchanged-targeted-cursor"
			if testCase.setup != nil {
				testCase.setup(t, fixture, fake, worker, job)
			}
			jobID := job.ID
			if testCase.jobID != nil {
				jobID = testCase.jobID(job)
			}

			processed, runErr := worker.RunJob(t.Context(), jobID)
			if testCase.wantErr != nil {
				require.ErrorIs(t, runErr, testCase.wantErr)
			} else {
				require.NoError(t, runErr)
			}
			assert.False(t, processed)
			assert.Zero(t, fake.runtime.calls())
			assert.Equal(t, "unchanged-targeted-cursor", worker.reconcileAfter)
		})
	}
}

func TestEmbeddingWorkerRunJobDoesNotFallbackToQueuedWork(t *testing.T) {
	fixture, fake, worker, request := newRealEmbeddingWorker(t, document.EmbeddingInputOriginalFile)
	job, err := fixture.catalog.EnqueueEmbeddingJob(t.Context(), request)
	require.NoError(t, err)
	worker.reconcileAfter = "no-reconcile"

	processed, err := worker.RunJob(t.Context(), workerHash("valid-but-absent"))
	require.NoError(t, err)
	require.False(t, processed)
	require.Zero(t, fake.runtime.calls())
	require.Equal(t, "no-reconcile", worker.reconcileAfter)

	claim, _, found, err := fixture.catalog.ClaimEmbeddingWork(t.Context(), job.ID,
		"proof-worker", time.Now().UTC().Add(time.Minute), time.Minute,
		worker.descriptorFingerprints)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, job.ID, claim.AttemptID)
}

func TestEmbeddingWorkerRunJobHonorsCanceledAndBlockedAdmission(t *testing.T) {
	t.Run("canceled entry", func(t *testing.T) {
		fixture, fake, worker, request := newRealEmbeddingWorker(t, document.EmbeddingInputOriginalFile)
		job, err := fixture.catalog.EnqueueEmbeddingJob(t.Context(), request)
		require.NoError(t, err)
		ctx, cancel := context.WithCancel(t.Context())
		cancel()

		processed, err := worker.RunJob(ctx, job.ID)
		require.ErrorIs(t, err, context.Canceled)
		require.False(t, processed)
		require.Zero(t, fake.runtime.calls())
		_, _, found, err := fixture.catalog.ClaimEmbeddingWork(t.Context(), job.ID,
			"proof-worker", time.Now().UTC().Add(time.Minute), time.Minute,
			worker.descriptorFingerprints)
		require.NoError(t, err)
		require.True(t, found)
	})

	t.Run("blocked admission", func(t *testing.T) {
		fixture, fake, worker, request := newRealEmbeddingWorker(t, document.EmbeddingInputOriginalFile)
		job, err := fixture.catalog.EnqueueEmbeddingJob(t.Context(), request)
		require.NoError(t, err)
		gate := api.NewOperationGate()
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
					t.Errorf("embedding worker goroutine did not join within five seconds")
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
			require.FailNow(t, "targeted embedding worker did not enter blocked admission")
		}
		cancel()
		result, joined := targetedReceiveWithin(done, 5*time.Second)
		require.True(t, joined, "embedding worker did not stop within five seconds")
		workerJoined = true
		releaseMaintenance()
		maintenanceErr, joined := targetedReceiveWithin(maintenanceDone, 5*time.Second)
		require.True(t, joined, "maintenance did not stop within five seconds")
		maintenanceJoined = true
		require.NoError(t, maintenanceErr)
		require.ErrorIs(t, result.err, context.Canceled)
		require.False(t, result.processed)
		require.Zero(t, fake.runtime.calls())
		_, _, found, err := fixture.catalog.ClaimEmbeddingWork(t.Context(), job.ID,
			"proof-worker", time.Now().UTC().Add(time.Minute), time.Minute,
			worker.descriptorFingerprints)
		require.NoError(t, err)
		require.True(t, found)
	})
}

func TestEmbeddingWorkerRunJobReleasesOperationGateDuringProvider(t *testing.T) {
	fixture, fake, worker, request := newRealEmbeddingWorker(t, document.EmbeddingInputOriginalFile)
	job, err := fixture.catalog.EnqueueEmbeddingJob(t.Context(), request)
	require.NoError(t, err)
	gate := api.NewOperationGate()
	worker.gate = gate
	started := make(chan struct{})
	release := make(chan struct{})
	var startedOnce, releaseOnce sync.Once
	fake.runtime.mutate[request.BindingID] = func(result document.EmbeddingResult) document.EmbeddingResult {
		startedOnce.Do(func() { close(started) })
		<-release
		return result
	}
	releaseProvider := func() { releaseOnce.Do(func() { close(release) }) }
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
				t.Errorf("embedding provider worker did not join within five seconds")
			}
		}
	})
	go func() { _, runErr := worker.RunJob(ctx, job.ID); done <- runErr }()
	select {
	case <-started:
	case <-ctx.Done():
		releaseProvider()
		cancel()
		require.FailNow(t, "targeted embedding provider did not start")
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
		require.FailNow(t, "targeted embedding worker did not stop")
	}
	require.NoError(t, maintenanceErr)
	require.NoError(t, mutationErr)
	require.NoError(t, workerErr)
	require.Equal(t, 1, fake.runtime.calls())
}

func TestEmbeddingWorkerRunJobAbandonsReplacedRendition(t *testing.T) {
	for _, duringProvider := range []bool{false, true} {
		t.Run(fmt.Sprintf("during-provider=%t", duringProvider), func(t *testing.T) {
			fixture, fake, worker, request := newRealEmbeddingWorker(t, document.EmbeddingInputRenditionChunk)
			job, err := fixture.catalog.EnqueueEmbeddingJob(t.Context(), request)
			require.NoError(t, err)
			var successorClaim EmbeddingWorkClaim
			var successorWork EmbeddingWork
			staged := fixture.stage(t, publicationIDs{"targeted-ba", "targeted-5a", "targeted-9a"},
				"targeted replacement evidence", "Targeted replacement")
			staged.Build.CapturedArtifactPolicy = []byte(`{"roles":[{"max_count":2,"min_count":1,"role":"normalized_evidence"},{"max_count":1,"min_count":1,"role":"sanitized_markdown"}],"version":1}`)
			staged.Build.CapturedArtifactPolicyFingerprint = workerHashBytes(staged.Build.CapturedArtifactPolicy)
			publisher, err := NewArtifactPublisher(fixture.catalog, fixture.blobs)
			require.NoError(t, err)
			publications := 0
			publish := func() error {
				_, publishErr := publisher.PublishRendition(t.Context(), staged)
				if publishErr != nil {
					return publishErr
				}
				publications++
				successorRequest := targetedSuccessorEmbeddingRequest(t, fixture, request, staged)
				successor, enqueueErr := fixture.catalog.EnqueueEmbeddingJob(t.Context(), successorRequest)
				if enqueueErr != nil {
					return enqueueErr
				}
				var successorFound bool
				successorClaim, successorWork, successorFound, enqueueErr = fixture.catalog.ClaimEmbeddingWork(
					t.Context(), successor.ID, "successor-worker", time.Now().UTC(), time.Minute,
					worker.descriptorFingerprints)
				if enqueueErr != nil {
					return enqueueErr
				}
				if !successorFound {
					return errors.New("successor embedding work was not claimable")
				}
				return nil
			}
			if duringProvider {
				fake.runtime.mutate[request.BindingID] = func(document.EmbeddingResult) document.EmbeddingResult {
					require.NoError(t, publish())
					return document.EmbeddingResult{}
				}
			} else {
				worker.runtime = afterEmbeddingPrepare{EmbeddingRuntime: worker.runtime, after: publish}
			}

			processed, err := worker.RunJob(t.Context(), job.ID)
			require.NoError(t, err)
			require.True(t, processed)
			require.Equal(t, 1, publications)
			if duringProvider {
				require.Equal(t, 1, fake.runtime.calls())
			} else {
				require.Zero(t, fake.runtime.calls())
			}

			_, _, found, err := fixture.catalog.ClaimEmbeddingWork(t.Context(), job.ID,
				"immediate-worker", time.Now().UTC(), time.Minute, worker.descriptorFingerprints)
			require.NoError(t, err)
			require.False(t, found, "stale target must not be claimable after the worker returns")
			_, _, found, err = fixture.catalog.ClaimEmbeddingWork(t.Context(), job.ID,
				"later-worker", time.Now().UTC().Add(time.Hour), time.Minute,
				worker.descriptorFingerprints)
			require.NoError(t, err)
			require.False(t, found, "abandoned work must not revive after lease expiry")
			require.NotEmpty(t, successorClaim.AttemptID,
				"replacement publication must install successor embedding work")
			require.Greater(t, successorClaim.Epoch, int64(1),
				"successor claim must advance past stale claim authority")
			require.NoError(t, fixture.catalog.ValidateEmbeddingWork(
				t.Context(), successorClaim, successorWork, time.Now().UTC()))

			active, err := fixture.catalog.ActiveRendition(
				t.Context(), request.ContentVersionID, request.Profile.Fingerprint)
			require.NoError(t, err)
			assert.Equal(t, staged.Build.ID, active.Build.ID,
				"stale embedding abandonment must not disturb replacement authority")
			var metadata bytes.Buffer
			require.NoError(t, fixture.catalog.ExportMetadata(t.Context(), &metadata))
			for line := range bytes.SplitSeq(metadata.Bytes(), []byte{'\n'}) {
				if len(line) == 0 {
					continue
				}
				var record struct {
					Type               string `json:"type"`
					ContentVersionID   string `json:"content_version_id"`
					ProfileFingerprint string `json:"profile_fingerprint"`
					BindingID          string `json:"binding_id"`
				}
				require.NoError(t, json.Unmarshal(line, &record))
				if record.Type == "embedding_head" || record.Type == "embedding_failure" {
					assert.False(t, record.ContentVersionID == request.ContentVersionID &&
						record.ProfileFingerprint == request.Profile.Fingerprint &&
						record.BindingID == request.BindingID,
						"stale work cannot publish a head or failure against its replacement")
				}
			}
		})
	}
}

type targetedEmbeddingHeadRecord struct {
	ContentVersionID   string
	BindingID          string
	InputKind          document.EmbeddingInputKind
	ProfileFingerprint string
}

type targetedObservedGate struct {
	*api.OperationGate

	entered chan struct{}
	once    sync.Once
}

func (gate *targetedObservedGate) MutateContext(ctx context.Context, fn func() error) error {
	gate.once.Do(func() { close(gate.entered) })
	return gate.OperationGate.MutateContext(ctx, fn)
}

func targetedReceiveWithin[T any](channel <-chan T, timeout time.Duration) (T, bool) {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case value := <-channel:
		return value, true
	case <-timer.C:
		var zero T
		return zero, false
	}
}

func targetedSuccessorEmbeddingRequest(
	t *testing.T, fixture publicationFixture, request store.EmbeddingJobRequest,
	staged StagedRendition,
) store.EmbeddingJobRequest {
	t.Helper()
	var profile document.ProcessingProfileV1
	require.NoError(t, json.Unmarshal(request.Profile.CanonicalProfile, &profile))
	var binding document.EmbeddingBindingV1
	for _, candidate := range profile.Embeddings {
		if candidate.Name == request.BindingID {
			binding = candidate
			break
		}
	}
	require.NotEmpty(t, binding.Name)
	artifact := staged.Build.Artifacts[0]
	require.Equal(t, "normalized_evidence", artifact.Role)
	evidenceJSON, err := readExactEmbeddingBlob(
		t.Context(), fixture.blobs, artifact.BlobHash, artifact.Size)
	require.NoError(t, err)
	var evidence document.NormalizedEvidenceV1
	require.NoError(t, json.Unmarshal(evidenceJSON, &evidence))
	evidence.Checksum = artifact.BlobHash
	policy, err := document.NewInputPolicy(binding, embeddingIntegrationTokenizer{},
		request.Profile.EvidenceLexicalFingerprint, nil)
	require.NoError(t, err)
	generated, err := document.BuildEmbeddingInputs(evidence, policy, document.GenerationLimits{
		MaxInputs: 100, MaxTotalContentTokens: 10000, MaxTotalRenderedTokens: 10000,
		MaxTotalContentBytes: 1 << 20, MaxTotalRenderedBytes: 1 << 20,
		MaxFittingWorkTokens: 100000, MaxFittingWorkBytes: 1 << 20,
	})
	require.NoError(t, err)
	generationJSON, err := document.MarshalEmbeddingInputGeneration(generated)
	require.NoError(t, err)
	receipt, err := fixture.blobs.WriteDetailedContext(t.Context(), bytes.NewReader(generationJSON))
	require.NoError(t, err)
	require.NoError(t, fixture.catalog.RecordRenditionBlob(
		t.Context(), receipt.Hash, receipt.Size, processingBlobPhysical(t, receipt)))
	generation := store.EmbeddingInputGenerationRecord{
		ID:                           workerHash("embedding-generation-attachment/v1\x00" + generated.Checksum + "\x00" + staged.Attachment.ID),
		GenerationBlobHash:           receipt.Hash,
		GenerationEncodedSize:        receipt.Size,
		GenerationChecksum:           generated.Checksum,
		SourceVersionID:              request.ContentVersionID,
		ProcessingProfileFingerprint: request.Profile.Fingerprint,
		EvidenceFingerprint:          generated.EvidenceChecksum,
		TokenizerFingerprint: workerHash(
			binding.Chunk.Tokenizer + "\x00" + binding.Chunk.TokenizerRevision),
		ChunkPolicyFingerprint: generated.PolicyFingerprint,
		FormatterFingerprint:   workerHash(binding.Chunk.Formatter),
		AttachmentID:           staged.Attachment.ID,
		CreatedAt:              metadataEmbeddingTime(time.Now().UTC()),
		GenerationJSON:         generationJSON,
		EvidenceJSON:           evidenceJSON,
	}
	for _, input := range generated.Inputs {
		generation.Inputs = append(generation.Inputs, store.EmbeddingInputReference{
			ID: input.Key, RenderedChecksum: input.Checksum,
		})
	}
	request.InputGeneration = generation
	return request
}

func targetedEmbeddingHead(
	t *testing.T, catalog *store.Store, request store.EmbeddingJobRequest,
) targetedEmbeddingHeadRecord {
	t.Helper()
	var metadata bytes.Buffer
	require.NoError(t, catalog.ExportMetadata(t.Context(), &metadata))
	var matched []targetedEmbeddingHeadRecord
	for line := range bytes.SplitSeq(metadata.Bytes(), []byte{'\n'}) {
		if len(line) == 0 {
			continue
		}
		var record struct {
			Type               string                      `json:"type"`
			ContentVersionID   string                      `json:"content_version_id"`
			BindingID          string                      `json:"binding_id"`
			InputKind          document.EmbeddingInputKind `json:"input_kind"`
			ProfileFingerprint string                      `json:"profile_fingerprint"`
		}
		require.NoError(t, json.Unmarshal(line, &record))
		if record.Type == "embedding_head" && record.ContentVersionID == request.ContentVersionID &&
			record.BindingID == request.BindingID &&
			record.ProfileFingerprint == request.Profile.Fingerprint {
			matched = append(matched, targetedEmbeddingHeadRecord{
				ContentVersionID: record.ContentVersionID, BindingID: record.BindingID,
				InputKind: record.InputKind, ProfileFingerprint: record.ProfileFingerprint,
			})
		}
	}
	require.Len(t, matched, 1)
	return matched[0]
}
