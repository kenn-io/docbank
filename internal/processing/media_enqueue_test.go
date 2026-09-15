package processing

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json/v2"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/media/mediatest"
	"go.kenn.io/docbank/internal/blob"
	"go.kenn.io/docbank/internal/store"
)

// TestMediaProcessingRequiresExplicitSelection catches an input artifact or
// empty object silently selecting provider work.
func TestMediaProcessingRequiresExplicitSelection(t *testing.T) {
	require.False(t, mediaProcessingRequested(nil))
	require.False(t, mediaProcessingRequested(&MediaProcessingRequest{}))
	require.True(t, mediaProcessingRequested(&MediaProcessingRequest{Profile: "speech"}))
	require.Error(t, validateMediaProcessing(&MediaProcessingRequest{SuppliedInputID: "input"}))
}

// TestSuppliedMediaProfileAdmitsWAVAndMP3 catches the daemon advertising a
// built-in transcript profile without executable codec bindings.
func TestSuppliedMediaProfileAdmitsWAVAndMP3(t *testing.T) {
	fixture := newPublicationFixture(t)
	name, profile, err := NewSuppliedMediaProfile(
		fixture.catalog, fixture.blobs, "daemon:operator")
	require.NoError(t, err)
	require.Equal(t, "supplied-transcript", name)
	require.NotNil(t, profile.RenditionProvider)
	formats := profile.RenditionProvider.Descriptor().SupportedFormats
	require.Contains(t, formats, document.RenditionFormatCapability{
		MediaFamily: "audio", MediaType: "audio/wav", InputKind: document.RenditionInputOriginalFile,
	})
	require.Contains(t, formats, document.RenditionFormatCapability{
		MediaFamily: "audio", MediaType: "audio/mpeg", InputKind: document.RenditionInputOriginalFile,
	})
}

// TestMediaEnqueueAuthorizedReturnsBeforeProvider catches request-owned
// provider execution and implicit consent creation during media submission.
func TestMediaEnqueueAuthorizedReturnsBeforeProvider(t *testing.T) {
	fixture := newPublicationFixture(t)
	descriptor, err := document.NewRenditionDescriptor(document.RenditionDescriptor{
		ID: "synthetic.media-worker-v1", ContractVersion: document.RenditionProviderContractVersion,
		PolicyFingerprint: processingHash("media-worker-policy"),
		TrustBoundary:     document.RenditionTrustLocalProcess,
		SupportedFormats: []document.RenditionFormatCapability{{
			MediaFamily: "audio", MediaType: "audio/wav", InputKind: document.RenditionInputOriginalFile,
		}},
		ReturnsStructured: true,
		ArtifactRoles:     []document.EvidenceArtifactRole{document.EvidenceArtifactStructured},
	})
	require.NoError(t, err)
	provider := &mediaWorkerProvider{workerProvider: &workerProvider{descriptor: descriptor}}
	provider.renderStarted = make(chan struct{})
	provider.renderRelease = make(chan struct{})
	raw := mediatest.WAV()
	written, err := fixture.blobs.WriteDetailedContext(t.Context(), bytes.NewReader(raw))
	require.NoError(t, err)
	node, err := fixture.catalog.CreateFile(t.Context(), fixture.catalog.RootID(), "source.wav",
		written.Hash, written.Size, "audio/wav", processingBlobPhysical(t, written))
	require.NoError(t, err)
	record := workerProcessingProfile(t, provider.Descriptor())
	var portable document.ProcessingProfileV1
	require.NoError(t, json.Unmarshal(record.CanonicalProfile, &portable))
	portable.Rendition.TrustBoundary = string(provider.Descriptor().TrustBoundary)
	service, err := NewService(ServiceConfig{
		Catalog: fixture.catalog, Blobs: fixture.blobs, Gate: newWorkerTestGate(),
		SpoolDirectory: t.TempDir(), Principal: "operator:synthetic", Scope: "document-processing",
		Profiles: map[string]ProfileConfig{"speech": {Profile: portable, RenditionProvider: provider}},
	})
	require.NoError(t, err)
	version, err := fixture.catalog.ContentVersionByID(t.Context(), node.CurrentVersionID)
	require.NoError(t, err)
	selector := Selector{NodeID: version.NodeID, ContentVersionID: version.ID, Profile: "speech"}
	plan, err := service.Plan(t.Context(), selector)
	require.NoError(t, err)
	authorization := service.renditionConsentRequest(service.profiles["speech"])

	_, err = service.EnqueueAuthorized(t.Context(), selector, plan.Fingerprint, authorization)
	require.ErrorIs(t, err, ErrConsentRequired)
	require.Zero(t, provider.calls)
	_, err = fixture.catalog.GrantConsent(t.Context(), store.ProcessingConsentGrantRequest{
		Principal: authorization.Principal, Scope: authorization.Scope,
		ProfileFingerprint:    authorization.ProfileFingerprint,
		DisclosureFingerprint: authorization.DisclosureFingerprint,
		InputClasses:          authorization.InputClasses, RetainedArtifactClasses: authorization.RetainedArtifactClasses,
	})
	require.NoError(t, err)
	var grantsBefore bytes.Buffer
	require.NoError(t, fixture.catalog.ExportMetadata(t.Context(), &grantsBefore))
	grantCount := bytes.Count(grantsBefore.Bytes(), []byte(`"type":"processing_consent_grant"`))
	require.Equal(t, 1, grantCount)
	sourceDigest := sha256.Sum256(raw)
	mediaReceipt, err := service.SubmitSuppliedMedia(t.Context(), SuppliedMediaRequest{
		OperationID: "00000000-0000-4000-8000-000000000101",
		Filename:    "source.wav", MediaType: "audio/wav", SHA256: hex.EncodeToString(sourceDigest[:]),
		ByteLength: int64(len(raw)), ExistingContentVersionID: version.ID,
		Occurrence: MediaOccurrenceInput{Ref: "message-1", Revision: "1", Filename: "source.wav"},
		Processing: &MediaProcessingRequest{Profile: "speech"},
	})
	require.NoError(t, err)
	require.Equal(t, "queued", mediaReceipt.OperationState)
	require.Equal(t, "pending", mediaReceipt.CoverageState)
	require.NotEmpty(t, mediaReceipt.JobID)
	require.Zero(t, provider.calls, "media submission must not invoke the provider")
	var grantsAfter bytes.Buffer
	require.NoError(t, fixture.catalog.ExportMetadata(t.Context(), &grantsAfter))
	require.Equal(t, grantCount,
		bytes.Count(grantsAfter.Bytes(), []byte(`"type":"processing_consent_grant"`)),
		"media admission must not create processing consent")

	job, err := service.EnqueueAuthorized(t.Context(), selector, plan.Fingerprint, authorization)
	require.NoError(t, err)
	require.NotEmpty(t, job.ID)
	require.Zero(t, provider.calls, "enqueue must not invoke the provider")

	worker, err := NewRenditionWorker(RenditionWorkerConfig{
		Catalog: fixture.catalog, Blobs: fixture.blobs, Runtime: service.renditions,
		Gate: newWorkerTestGate(), Owner: "media-test-worker", LeaseDuration: time.Minute,
		IdleDelay: time.Millisecond,
	})
	require.NoError(t, err)
	done := make(chan error, 1)
	go func() {
		_, runErr := worker.RunJob(t.Context(), job.RenditionJobID)
		done <- runErr
	}()
	select {
	case <-provider.renderStarted:
	case <-time.After(time.Second):
		t.Fatal("supervised worker did not reach provider")
	}
	close(provider.renderRelease)
	require.NoError(t, <-done)
	terminal, err := service.Status(t.Context(), mediaReceipt.JobID)
	require.NoError(t, err)
	require.Equal(t, "completed", terminal.State)
	badReceipt := store.MediaPublicationReceipt{VaultUID: fixture.catalog.VaultID(),
		SourceID: mediaReceipt.SourceID, SourceVersionID: mediaReceipt.SourceVersionID,
		ContentVersionID: mediaReceipt.ContentVersionID, OccurrenceID: mediaReceipt.OccurrenceID,
		OperationID: "00000000-0000-4000-8000-000000000102", OperationState: "queued",
		CoverageState: "pending", ProcessingNodeID: version.NodeID, ProcessingProfile: "removed-profile",
		ProcessingPrincipal: service.principal, ProcessingScope: service.scope,
		ProcessingProfileFingerprint: processingHash("removed-profile")}
	_, err = fixture.catalog.QueueMediaRetry(t.Context(), store.MediaOperation{
		ID: badReceipt.OperationID, Principal: service.principal, Verb: "retry_media",
		RequestSHA256: processingHash("bad-continuation"), SourceID: mediaReceipt.SourceID,
	}, badReceipt)
	require.NoError(t, err)
	continuation := &MediaContinuationWorker{Service: service, IdleDelay: time.Millisecond}
	processed, err := continuation.RunOne(t.Context())
	require.NoError(t, err)
	require.True(t, processed)
	failed, err := fixture.catalog.MediaOperationReceipt(t.Context(), store.MediaOperation{
		ID: badReceipt.OperationID, Principal: service.principal, Verb: "retry_media",
		RequestSHA256: processingHash("bad-continuation"), SourceID: mediaReceipt.SourceID})
	require.NoError(t, err)
	require.Contains(t, failed, `"operation_state":"failed"`)
	processed, err = continuation.RunOne(t.Context())
	require.NoError(t, err)
	require.True(t, processed)
	pending, err := fixture.catalog.MediaProcessingContinuations(t.Context(), 10)
	require.NoError(t, err)
	require.Empty(t, pending)
}

func TestMediaContinuationCancellationBeforeEmbeddingResumesAfterReopen(t *testing.T) {
	root := t.TempDir()
	catalog, err := store.Open(filepath.Join(root, "docbank.db"))
	require.NoError(t, err)
	blobs, err := blob.New(store.NewPackCatalog(catalog), filepath.Join(root, "blobs"))
	require.NoError(t, err)
	t.Cleanup(func() {
		if blobs != nil {
			require.NoError(t, blobs.Close())
		}
		if catalog != nil {
			require.NoError(t, catalog.Close())
		}
	})

	descriptor, err := document.NewRenditionDescriptor(document.RenditionDescriptor{
		ID: "synthetic.media-cancel-v1", ContractVersion: document.RenditionProviderContractVersion,
		PolicyFingerprint: processingHash("media-cancel-policy"),
		TrustBoundary:     document.RenditionTrustLocalProcess,
		SupportedFormats: []document.RenditionFormatCapability{{
			MediaFamily: "audio", MediaType: "audio/wav", InputKind: document.RenditionInputOriginalFile,
		}},
		ReturnsStructured: true,
		ArtifactRoles:     []document.EvidenceArtifactRole{document.EvidenceArtifactStructured},
	})
	require.NoError(t, err)
	renderer := &mediaWorkerProvider{workerProvider: &workerProvider{descriptor: descriptor}}
	embeddingFixture := newEmbeddingWorkerFixture(t)
	var embeddingProfile document.ProcessingProfileV1
	require.NoError(t, json.Unmarshal(embeddingFixture.profile.CanonicalProfile, &embeddingProfile))
	var direct document.EmbeddingBindingV1
	for _, binding := range embeddingProfile.Embeddings {
		if binding.InputKind == document.EmbeddingInputOriginalFile {
			direct = binding
		}
	}
	require.NotEmpty(t, direct.Name)
	record := workerProcessingProfile(t, renderer.Descriptor())
	var portable document.ProcessingProfileV1
	require.NoError(t, json.Unmarshal(record.CanonicalProfile, &portable))
	portable.Rendition.TrustBoundary = string(renderer.Descriptor().TrustBoundary)
	portable.Embeddings = []document.EmbeddingBindingV1{direct}
	embedder := &embeddingWorkerProvider{runtime: embeddingFixture.runtime,
		binding: direct.Name, descriptor: embeddingFixture.descriptor}
	spool := filepath.Join(root, "spool")
	require.NoError(t, os.MkdirAll(spool, 0o700))
	newService := func() *Service {
		service, serviceErr := NewService(ServiceConfig{
			Catalog: catalog, Blobs: blobs, Gate: newWorkerTestGate(), SpoolDirectory: spool,
			Principal: "operator:cancel-restart", Scope: "document-processing",
			Profiles: map[string]ProfileConfig{"speech": {
				Profile: portable, RenditionProvider: renderer,
				EmbeddingProviders: map[string]document.EmbeddingProvider{direct.Name: embedder},
				EmbeddingClassifiers: map[string]func(error) (EmbeddingProviderFailure, time.Duration){
					direct.Name: embeddingFixture.runtime.Classify,
				},
			}},
		})
		require.NoError(t, serviceErr)
		return service
	}
	service := newService()
	raw := mediatest.WAV()
	written, err := blobs.WriteDetailedContext(t.Context(), bytes.NewReader(raw))
	require.NoError(t, err)
	node, err := catalog.CreateFile(t.Context(), catalog.RootID(), "cancel.wav",
		written.Hash, written.Size, "audio/wav", processingBlobPhysical(t, written))
	require.NoError(t, err)
	version, err := catalog.ContentVersionByID(t.Context(), node.CurrentVersionID)
	require.NoError(t, err)
	selector := Selector{NodeID: node.ID, ContentVersionID: version.ID, Profile: "speech"}
	plan, err := service.Plan(t.Context(), selector)
	require.NoError(t, err)
	_, err = service.GrantConsent(t.Context(), ConsentGrantRequest{Selector: selector, PlanFingerprint: plan.Fingerprint})
	require.NoError(t, err)
	receipt, err := service.SubmitSuppliedMedia(t.Context(), SuppliedMediaRequest{
		OperationID: "00000000-0000-4000-8000-000000000171",
		Filename:    "cancel.wav", MediaType: "audio/wav", SHA256: written.Hash,
		ByteLength: written.Size, ExistingContentVersionID: version.ID,
		Occurrence: MediaOccurrenceInput{Ref: "cancelled-continuation", Revision: "1"},
		Processing: &MediaProcessingRequest{Profile: "speech"},
	})
	require.NoError(t, err)
	waiter, err := catalog.RenditionJobWaiterByID(t.Context(), receipt.JobID)
	require.NoError(t, err)
	renditionWorker, err := NewRenditionWorker(RenditionWorkerConfig{
		Catalog: catalog, Blobs: blobs, Runtime: service.renditions, Gate: newWorkerTestGate(),
		Owner: "cancel-rendition-worker", LeaseDuration: time.Minute, IdleDelay: time.Millisecond,
	})
	require.NoError(t, err)
	_, err = renditionWorker.RunJob(t.Context(), waiter.JobID)
	require.NoError(t, err)
	continuations, err := catalog.MediaProcessingContinuations(t.Context(), 10, service.principal)
	require.NoError(t, err)
	require.Len(t, continuations, 1)
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	processed, err := (&MediaContinuationWorker{Service: service}).runContinuation(cancelled, continuations[0])
	require.ErrorIs(t, err, context.Canceled)
	require.False(t, processed)
	require.Zero(t, embeddingFixture.runtime.calls(), "cancellation must stop before embedding egress")
	pending, err := catalog.MediaProcessingContinuations(t.Context(), 10, service.principal)
	require.NoError(t, err)
	require.Len(t, pending, 1, "cancellation must preserve resumable media intent")

	require.NoError(t, blobs.Close())
	blobs = nil
	require.NoError(t, catalog.Close())
	catalog = nil
	catalog, err = store.Open(filepath.Join(root, "docbank.db"))
	require.NoError(t, err)
	blobs, err = blob.New(store.NewPackCatalog(catalog), filepath.Join(root, "blobs"))
	require.NoError(t, err)
	service = newService()
	embeddingFixture.runtime.failures[direct.Name] = []error{
		embeddingTransientError{}, embeddingTransientError{}, embeddingTransientError{},
	}
	var clockOffset atomic.Int64
	service.clock = func() time.Time { return time.Now().Add(time.Duration(clockOffset.Load())) }
	runContext, stopRun := context.WithTimeout(t.Context(), 5*time.Second)
	finished := make(chan struct{})
	go func() {
		processed, err = (&MediaContinuationWorker{Service: service}).RunOne(runContext)
		close(finished)
	}()
	t.Cleanup(func() { stopRun(); <-finished })
	require.Eventually(t, func() bool {
		status, statusErr := service.Status(t.Context(), receipt.JobID)
		return statusErr == nil && status.State == "retry_wait"
	}, 3*time.Second, time.Millisecond)
	pending, readErr := catalog.MediaProcessingContinuations(t.Context(), 10, service.principal)
	require.NoError(t, readErr)
	require.Len(t, pending, 1, "retrying embeddings must not finish the media intent")
	stopRun()
	<-finished
	require.ErrorIs(t, err, context.Canceled)
	require.False(t, processed)
	clockOffset.Store(int64(2 * time.Minute))
	resumeContext, stopResume := context.WithTimeout(t.Context(), 3*time.Second)
	defer stopResume()
	processed, err = (&MediaContinuationWorker{Service: service}).RunOne(resumeContext)
	require.NoError(t, err)
	require.True(t, processed)
	require.Positive(t, embeddingFixture.runtime.calls())
	status, err := service.Status(t.Context(), receipt.JobID)
	require.NoError(t, err)
	require.Equal(t, "completed", status.State)
	require.Equal(t, 1, status.CompletedBindings)
	pending, err = catalog.MediaProcessingContinuations(t.Context(), 10, service.principal)
	require.NoError(t, err)
	require.Empty(t, pending)
}

func TestMediaContinuationTransientCatalogReadRetriesNextTick(t *testing.T) {
	fixture := newPublicationFixture(t)
	worker := &MediaContinuationWorker{Service: &Service{
		catalog: fixture.catalog, gate: newWorkerTestGate(),
	}}
	processed, err := worker.failContinuation(t.Context(),
		store.MediaPublicationReceipt{}, fmt.Errorf("reading processing status: %w", sql.ErrConnDone))
	require.ErrorIs(t, err, sql.ErrConnDone)
	require.False(t, processed,
		"transient catalog failures must remain pending for the next tick")
}

type mediaWorkerProvider struct{ *workerProvider }

func (provider *mediaWorkerProvider) Render(
	ctx context.Context,
	upload document.AuthorizedUpload,
	authorization document.RenditionAuthorization,
) (document.RenditionResult, error) {
	result, err := provider.workerProvider.Render(ctx, upload, authorization)
	result.Evidence.Family = "audio"
	return result, err
}

func TestMediaCancellationAndTransientEnqueueFailuresRemainResumable(t *testing.T) {
	for _, retry := range []bool{false, true} {
		t.Run(fmt.Sprintf("retry=%t", retry), func(t *testing.T) {
			fixture := newPublicationFixture(t)
			descriptor, err := document.NewRenditionDescriptor(document.RenditionDescriptor{
				ID: "synthetic.media-worker-v1", ContractVersion: document.RenditionProviderContractVersion,
				PolicyFingerprint: processingHash("media-worker-policy"),
				TrustBoundary:     document.RenditionTrustLocalProcess,
				SupportedFormats: []document.RenditionFormatCapability{{
					MediaFamily: "audio", MediaType: "audio/wav", InputKind: document.RenditionInputOriginalFile,
				}},
				ReturnsStructured: true,
				ArtifactRoles:     []document.EvidenceArtifactRole{document.EvidenceArtifactStructured},
			})
			require.NoError(t, err)
			provider := &mediaWorkerProvider{workerProvider: &workerProvider{descriptor: descriptor}}
			raw := mediatest.WAV()
			written, err := fixture.blobs.WriteDetailedContext(t.Context(), bytes.NewReader(raw))
			require.NoError(t, err)
			node, err := fixture.catalog.CreateFile(t.Context(), fixture.catalog.RootID(), "source.wav",
				written.Hash, written.Size, "audio/wav", processingBlobPhysical(t, written))
			require.NoError(t, err)
			record := workerProcessingProfile(t, provider.Descriptor())
			var portable document.ProcessingProfileV1
			require.NoError(t, json.Unmarshal(record.CanonicalProfile, &portable))
			portable.Rendition.TrustBoundary = string(provider.Descriptor().TrustBoundary)
			service, err := NewService(ServiceConfig{
				Catalog: fixture.catalog, Blobs: fixture.blobs, Gate: newWorkerTestGate(),
				SpoolDirectory: t.TempDir(), Principal: "operator:synthetic", Scope: "document-processing",
				Profiles: map[string]ProfileConfig{"speech": {Profile: portable, RenditionProvider: provider}},
			})
			require.NoError(t, err)
			version, err := fixture.catalog.ContentVersionByID(t.Context(), node.CurrentVersionID)
			require.NoError(t, err)

			request := SuppliedMediaRequest{
				OperationID: "00000000-0000-4000-8000-000000000711", Filename: "source.wav",
				MediaType: "audio/wav", SHA256: version.BlobHash, ByteLength: version.Size,
				ExistingContentVersionID: version.ID, Occurrence: MediaOccurrenceInput{Ref: "cancelled", Revision: "1"},
			}
			var retained MediaReceipt
			if retry {
				retained, err = service.SubmitSuppliedMedia(t.Context(), request)
				require.NoError(t, err)
			}
			selector := Selector{NodeID: version.NodeID, ContentVersionID: version.ID, Profile: "speech"}
			plan, err := service.Plan(t.Context(), selector)
			require.NoError(t, err)
			_, err = service.GrantConsent(t.Context(), ConsentGrantRequest{Selector: selector, PlanFingerprint: plan.Fingerprint})
			require.NoError(t, err)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			gate := service.gate
			service.gate = &cancelMediaAfterCommitGate{processingOperationGate: gate, cancel: cancel}
			operationID := request.OperationID
			if retry {
				operationID = "00000000-0000-4000-8000-000000000712"
				_, err = service.RetryMedia(ctx, operationID, retained.SourceID, MediaProcessingRequest{Profile: "speech"})
			} else {
				request.Processing = &MediaProcessingRequest{Profile: "speech"}
				_, err = service.SubmitSuppliedMedia(ctx, request)
			}
			service.gate = gate
			require.ErrorIs(t, err, context.Canceled)
			pending, err := service.catalog.MediaProcessingContinuations(t.Context(), 10, service.principal)
			require.NoError(t, err)
			require.Len(t, pending, 1, "a committed enqueue intent must survive caller cancellation")
			require.Equal(t, operationID, pending[0].OperationID)
			require.Empty(t, pending[0].JobID)
			workerContext, stopWorker := context.WithTimeout(t.Context(), time.Second)
			defer stopWorker()
			failAt := 1 // Retry enqueue failure after admission.
			if retry {
				failAt = 2 // Retry receipt persistence after a successful enqueue.
			}
			service.gate = &retryMediaMutationGate{processingOperationGate: gate,
				failAt: failAt, cancel: stopWorker}
			err = (&MediaContinuationWorker{Service: service, IdleDelay: time.Millisecond}).Run(workerContext)
			require.ErrorIs(t, err, context.Canceled, "transient storage errors must not stop the worker")
			service.gate = gate
			pending, err = service.catalog.MediaProcessingContinuations(t.Context(), 10, service.principal)
			require.NoError(t, err)
			require.Len(t, pending, 1)
			require.NotEmpty(t, pending[0].JobID, "the worker must resume enqueueing after cancellation")
			require.Zero(t, provider.calls)
		})
	}
}

// Cancel after the actual store commit, before request-owned planning/enqueue.
type cancelMediaAfterCommitGate struct {
	processingOperationGate

	cancel context.CancelFunc
}

func (gate *cancelMediaAfterCommitGate) MutateContext(ctx context.Context, fn func() error) error {
	err := gate.processingOperationGate.MutateContext(ctx, fn)
	if err == nil {
		gate.cancel()
	}
	return err
}

// Inject one transient mutation failure, then stop after the resumed receipt is committed.
type retryMediaMutationGate struct {
	processingOperationGate

	call, failAt int
	cancel       context.CancelFunc
}

func (gate *retryMediaMutationGate) MutateContext(ctx context.Context, fn func() error) error {
	gate.call++
	if gate.call == gate.failAt {
		return sql.ErrConnDone
	}
	err := gate.processingOperationGate.MutateContext(ctx, fn)
	if err == nil && gate.call == gate.failAt+2 {
		gate.cancel()
	}
	return err
}
