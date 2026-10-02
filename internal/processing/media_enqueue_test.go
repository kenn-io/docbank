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
	t.Parallel()
	require.False(t, mediaProcessingRequested(nil))
	require.False(t, mediaProcessingRequested(&MediaProcessingRequest{}))
	require.True(t, mediaProcessingRequested(&MediaProcessingRequest{Profile: "speech"}))
	require.Error(t, validateMediaProcessing(&MediaProcessingRequest{SuppliedInputID: "input"}))
}

// TestSuppliedMediaProfileAdmitsWAVAndMP3 catches the daemon advertising a
// built-in transcript profile without executable codec bindings.
func TestSuppliedMediaProfileAdmitsWAVAndMP3(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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

	_, err = service.EnqueueAuthorized(t.Context(), selector, mediaSourceBinding{}, plan.Fingerprint, authorization, "")
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
	submit := SuppliedMediaRequest{
		OperationID: "00000000-0000-4000-8000-000000000101",
		Filename:    "source.wav", MediaType: "audio/wav", SHA256: hex.EncodeToString(sourceDigest[:]),
		ByteLength: int64(len(raw)), ExistingContentVersionID: version.ID,
		Occurrence: MediaOccurrenceInput{Ref: "message-1", Revision: "1", Filename: "source.wav"},
		Processing: &MediaProcessingRequest{Profile: "speech"},
	}
	mediaReceipt, err := service.SubmitSuppliedMedia(t.Context(), submit)
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

	// A plan obtained before media admission must remain valid with source binding.
	source := mediaSourceBinding{sourceID: mediaReceipt.SourceID, sourceVersionID: mediaReceipt.SourceVersionID}
	job, err := service.EnqueueAuthorized(t.Context(), selector, source, plan.Fingerprint, authorization, "")
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
	case <-time.After(10 * time.Second):
		t.Fatal("supervised worker did not reach provider")
	}
	close(provider.renderRelease)
	require.NoError(t, <-done)
	terminal, err := service.Status(t.Context(), mediaReceipt.JobID)
	require.NoError(t, err)
	require.Equal(t, "completed", terminal.State)
	replayed, err := service.SubmitSuppliedMedia(t.Context(), submit)
	require.NoError(t, err)
	require.Equal(t, mediaReceipt.JobID, replayed.JobID)
	require.Equal(t, []string{"succeeded", "transcribed"}, []string{replayed.OperationState, replayed.CoverageState},
		"replay derives state from the completed job")
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
	require.NoError(t, service.ContinueMediaProcessing(t.Context(), badReceipt))
	failed, err := fixture.catalog.MediaOperationReceipt(t.Context(), store.MediaOperation{
		ID: badReceipt.OperationID, Principal: service.principal, Verb: "retry_media",
		RequestSHA256: processingHash("bad-continuation"), SourceID: mediaReceipt.SourceID})
	require.NoError(t, err)
	require.Contains(t, failed, `"operation_state":"failed"`)
	pending, err := service.MediaProcessingTargets(t.Context(), "", 10)
	require.NoError(t, err)
	require.Len(t, pending, 1, "only the completed submission awaits its recorded outcome")
	require.NoError(t, service.ContinueMediaProcessing(t.Context(), pending[0]))
	pending, err = service.MediaProcessingTargets(t.Context(), "", 10)
	require.NoError(t, err)
	require.Empty(t, pending)
}

// TestMediaContinuationCancellationBeforeEmbeddingResumesAfterReopen catches
// an interrupted or abandoned embedding job ending its media operation instead
// of resuming.
func TestMediaContinuationCancellationBeforeEmbeddingResumesAfterReopen(t *testing.T) {
	t.Parallel()
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
	// Required, so an abandoned job is not reported as an optional partial result.
	portable.Embeddings[0].Activation = document.EmbeddingRequired
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
	targets, err := service.MediaProcessingTargets(t.Context(), "", 10)
	require.NoError(t, err)
	require.Len(t, targets, 1)
	target := targets[0]
	derived, err := service.mediaOperationState(t.Context(), target)
	require.NoError(t, err)
	require.Equal(t, "queued", derived.OperationState, "a published rendition still awaits its embeddings")
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	require.ErrorIs(t, service.ContinueMediaProcessing(cancelled, target), context.Canceled)
	require.Zero(t, embeddingFixture.runtime.calls(), "cancellation must stop before embedding egress")
	pending, err := service.MediaProcessingTargets(t.Context(), "", 10)
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
	runContext, stopRun := context.WithTimeout(t.Context(), 10*time.Second)
	finished := make(chan struct{})
	go func() {
		err = service.ContinueMediaProcessing(runContext, target)
		close(finished)
	}()
	t.Cleanup(func() { stopRun(); <-finished })
	require.Eventually(t, func() bool {
		status, statusErr := service.Status(t.Context(), receipt.JobID)
		return statusErr == nil && status.State == "retry_wait"
	}, 10*time.Second, time.Millisecond)
	pending, readErr := service.MediaProcessingTargets(t.Context(), "", 10)
	require.NoError(t, readErr)
	require.Len(t, pending, 1, "retrying embeddings must not finish the media intent")
	stopRun()
	<-finished
	require.ErrorIs(t, err, context.Canceled)
	clockOffset.Store(int64(2 * time.Minute))
	// A fenced attempt abandons the embedding job. Re-running embeddings reopens it.
	waiting, err := service.Status(t.Context(), receipt.JobID)
	require.NoError(t, err)
	require.Len(t, waiting.EmbeddingJobIDs, 1)
	at := service.clock()
	claim, _, claimed, err := catalog.ClaimEmbeddingWork(t.Context(), waiting.EmbeddingJobIDs[0], "fenced-worker",
		at, time.Minute, []string{embeddingFixture.descriptor.Fingerprint})
	require.NoError(t, err)
	require.True(t, claimed)
	require.NoError(t, catalog.AbandonEmbeddingWork(t.Context(), claim, at))
	abandoned, err := service.MediaStatus(t.Context(), receipt.SourceID)
	require.NoError(t, err)
	require.Equal(t, "queued", abandoned.OperationState, "an abandoned embedding job can still finish")
	resumeContext, stopResume := context.WithTimeout(t.Context(), 10*time.Second)
	defer stopResume()
	require.NoError(t, service.ContinueMediaProcessing(resumeContext, target))
	require.Positive(t, embeddingFixture.runtime.calls())
	status, err := service.Status(t.Context(), receipt.JobID)
	require.NoError(t, err)
	require.Equal(t, "completed", status.State)
	require.Equal(t, 1, status.CompletedBindings)
	pending, err = service.MediaProcessingTargets(t.Context(), "", 10)
	require.NoError(t, err)
	require.Empty(t, pending)
	recorded, err := service.MediaStatus(t.Context(), receipt.SourceID)
	require.NoError(t, err)
	require.Equal(t, []string{"succeeded", "transcribed"}, []string{recorded.OperationState, recorded.CoverageState})
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

// mediaStateFixture is one WAV original and a consented rendition profile
// whose synthetic provider runs only when a test drives the worker.
type mediaStateFixture struct {
	publicationFixture

	service  *Service
	provider *mediaWorkerProvider
	version  store.ContentVersion
	selector Selector
}

func newMediaStateFixture(t *testing.T) mediaStateFixture {
	t.Helper()
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
	f := mediaStateFixture{publicationFixture: fixture, service: service, provider: provider}
	f.version, f.selector = f.addWAV(t, "source.wav", mediatest.WAV())
	plan, err := service.Plan(t.Context(), f.selector)
	require.NoError(t, err)
	_, err = service.GrantConsent(t.Context(), ConsentGrantRequest{Selector: f.selector, PlanFingerprint: plan.Fingerprint})
	require.NoError(t, err)
	return f
}

func (f mediaStateFixture) addWAV(t *testing.T, name string, raw []byte) (store.ContentVersion, Selector) {
	t.Helper()
	written, err := f.blobs.WriteDetailedContext(t.Context(), bytes.NewReader(raw))
	require.NoError(t, err)
	node, err := f.catalog.CreateFile(t.Context(), f.catalog.RootID(), name,
		written.Hash, written.Size, "audio/wav", processingBlobPhysical(t, written))
	require.NoError(t, err)
	version, err := f.catalog.ContentVersionByID(t.Context(), node.CurrentVersionID)
	require.NoError(t, err)
	return version, Selector{NodeID: version.NodeID, ContentVersionID: version.ID, Profile: "speech"}
}

func (f mediaStateFixture) suppliedRequest(operationID string, processing *MediaProcessingRequest) SuppliedMediaRequest {
	return SuppliedMediaRequest{OperationID: operationID, Filename: "source.wav", MediaType: "audio/wav",
		SHA256: f.version.BlobHash, ByteLength: f.version.Size, ExistingContentVersionID: f.version.ID,
		Occurrence: MediaOccurrenceInput{Ref: "message", Revision: "1"}, Processing: processing}
}

func (f mediaStateFixture) enqueue(t *testing.T, selector Selector) Job {
	t.Helper()
	plan, err := f.service.Plan(t.Context(), selector)
	require.NoError(t, err)
	job, err := f.service.EnqueueAuthorized(t.Context(), selector, mediaSourceBinding{}, plan.Fingerprint,
		f.service.renditionConsentRequest(f.service.profiles["speech"]), "")
	require.NoError(t, err)
	return job
}

func (f mediaStateFixture) run(t *testing.T, jobID string) error {
	t.Helper()
	waiter, err := f.catalog.RenditionJobWaiterByID(t.Context(), jobID)
	require.NoError(t, err)
	worker, err := NewRenditionWorker(RenditionWorkerConfig{
		Catalog: f.catalog, Blobs: f.blobs, Runtime: f.service.renditions, Gate: newWorkerTestGate(),
		Owner: "media-state-worker", LeaseDuration: time.Minute, IdleDelay: time.Millisecond,
	})
	require.NoError(t, err)
	_, err = worker.RunJob(t.Context(), waiter.JobID)
	return err
}

func TestMediaBackfillPreservesCompletedRevokedInput(t *testing.T) {
	t.Parallel()
	f := newMediaStateFixture(t)
	name, profile, err := NewSuppliedMediaProfile(f.catalog, f.blobs, f.service.principal)
	require.NoError(t, err)
	f.service, err = NewService(ServiceConfig{
		Catalog: f.catalog, Blobs: f.blobs, Gate: newWorkerTestGate(),
		SpoolDirectory: t.TempDir(), Principal: f.service.principal, Scope: f.service.scope,
		Profiles: map[string]ProfileConfig{name: profile},
	})
	require.NoError(t, err)
	f.selector.Profile = name
	retained, err := f.service.SubmitSuppliedMedia(t.Context(), f.suppliedRequest("00000000-0000-4000-8000-000000000721", nil))
	require.NoError(t, err)
	_, err = f.service.DeclareMediaOccurrence(t.Context(), "00000000-0000-4000-8000-000000000725", retained.SourceID,
		MediaOccurrenceInput{Ref: "second", Revision: "1"})
	require.NoError(t, err)
	text := "synthetic transcript\n"
	input, err := f.service.ImportRecordingArtifact(t.Context(), MediaArtifactRequest{
		OperationID: "00000000-0000-4000-8000-000000000722", SourceID: retained.SourceID, OccurrenceID: retained.OccurrenceID,
		Kind: "transcript", Filename: "source.txt", MediaType: "text/plain",
		SHA256: processingHash(text), ByteLength: int64(len(text)), Content: bytes.NewBufferString(text),
	})
	require.NoError(t, err)
	plan, err := f.service.Plan(t.Context(), f.selector)
	require.NoError(t, err)
	_, err = f.service.GrantConsent(t.Context(), ConsentGrantRequest{Selector: f.selector, PlanFingerprint: plan.Fingerprint})
	require.NoError(t, err)
	queued, err := f.service.RetryMedia(t.Context(), "00000000-0000-4000-8000-000000000723", retained.SourceID,
		MediaProcessingRequest{Profile: name, SuppliedInputID: input.SuppliedInputID})
	require.NoError(t, err)
	require.NoError(t, f.run(t, queued.JobID))
	targets, err := f.service.MediaProcessingTargets(t.Context(), "", 10)
	require.NoError(t, err)
	require.Len(t, targets, 1)
	_, err = f.service.RevokeMediaOccurrence(t.Context(), "00000000-0000-4000-8000-000000000724", retained.OccurrenceID, "1")
	require.NoError(t, err)
	for _, phase := range []string{"before backfill", "after backfill"} {
		t.Run(phase, func(t *testing.T) {
			status, err := f.service.MediaStatus(t.Context(), retained.SourceID)
			require.NoError(t, err)
			require.Equal(t, "succeeded", status.OperationState)
			require.Equal(t, "stale", status.CoverageState)
		})
		require.NoError(t, f.service.ContinueMediaProcessing(t.Context(), targets[0]))
	}
	targets, err = f.service.MediaProcessingTargets(t.Context(), "", 10)
	require.NoError(t, err)
	require.Empty(t, targets)
}

// TestMediaAdmissionInterruptedBeforeJobResumesThroughBackfill catches a
// receipt committed without its job staying unbound after an interruption.
func TestMediaAdmissionInterruptedBeforeJobResumesThroughBackfill(t *testing.T) {
	t.Parallel()
	for _, retry := range []bool{false, true} {
		t.Run(fmt.Sprintf("retry=%t", retry), func(t *testing.T) {
			f := newMediaStateFixture(t)
			service := f.service
			request := f.suppliedRequest("00000000-0000-4000-8000-000000000711", nil)
			admit := func(ctx context.Context) (MediaReceipt, error) {
				request.Processing = &MediaProcessingRequest{Profile: "speech"}
				return service.SubmitSuppliedMedia(ctx, request)
			}
			operationID := request.OperationID
			if retry {
				retained, err := service.SubmitSuppliedMedia(t.Context(), request)
				require.NoError(t, err)
				operationID = "00000000-0000-4000-8000-000000000712"
				admit = func(ctx context.Context) (MediaReceipt, error) {
					return service.RetryMedia(ctx, operationID, retained.SourceID, MediaProcessingRequest{Profile: "speech"})
				}
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			gate := service.gate
			service.gate = &cancelMediaAfterCommitGate{processingOperationGate: gate, cancel: cancel}
			_, err := admit(ctx)
			service.gate = gate
			require.ErrorIs(t, err, context.Canceled)
			// A restarted process keeps only the catalog; the job-less receipt is its sole record.
			service, err = NewService(ServiceConfig{Catalog: f.catalog, Blobs: f.blobs, Gate: newWorkerTestGate(),
				SpoolDirectory: t.TempDir(), Principal: "operator:synthetic", Scope: "document-processing",
				Profiles: map[string]ProfileConfig{"speech": {Profile: service.profiles["speech"].portable,
					RenditionProvider: f.provider}}})
			require.NoError(t, err)
			targets, err := service.MediaProcessingTargets(t.Context(), "", 10)
			require.NoError(t, err)
			require.Len(t, targets, 1, "a committed admission must survive caller cancellation")
			require.Equal(t, operationID, targets[0].OperationID)
			require.Empty(t, targets[0].JobID)

			require.NoError(t, service.ContinueMediaProcessing(t.Context(), targets[0]))
			replayed, err := admit(t.Context())
			require.NoError(t, err)
			require.NotEmpty(t, replayed.JobID, "the backfill binds the job the replay reports")
			require.Equal(t, "queued", replayed.OperationState)
			status, err := service.Status(t.Context(), replayed.JobID)
			require.NoError(t, err)
			require.Equal(t, "queued", status.State)
			require.Zero(t, f.provider.calls)
			require.NoError(t, f.run(t, replayed.JobID))
			targets, err = service.MediaProcessingTargets(t.Context(), "", 10)
			require.NoError(t, err)
			require.Len(t, targets, 1, "a queued receipt stays listed until its outcome is recorded")
			require.NoError(t, service.ContinueMediaProcessing(t.Context(), targets[0]))
			targets, err = service.MediaProcessingTargets(t.Context(), "", 10)
			require.NoError(t, err)
			require.Empty(t, targets)
		})
	}
}

// TestMediaOperationStateFollowsJob catches a receipt reporting a state its
// bound rendition job does not have.
func TestMediaOperationStateFollowsJob(t *testing.T) {
	t.Parallel()
	f := newMediaStateFixture(t)
	ctx := t.Context()
	job := f.enqueue(t, f.selector)
	failingRaw := mediatest.WAV()
	failingRaw[len(failingRaw)-1]++
	_, failingSelector := f.addWAV(t, "failing.wav", failingRaw)
	failingJob := f.enqueue(t, failingSelector)
	coverage := map[string]string{"queued": "pending", "succeeded": "transcribed", "failed": "unavailable"}
	receipt := func(jobID, state string) store.MediaPublicationReceipt {
		return store.MediaPublicationReceipt{OperationID: "00000000-0000-4000-8000-000000000901", JobID: jobID,
			ProcessingProfile: "speech", ProcessingProfileFingerprint: f.service.profiles["speech"].record.Fingerprint,
			OperationState: state, CoverageState: coverage[state]}
	}
	derive := func(service *Service, stored store.MediaPublicationReceipt) string {
		t.Helper()
		derived, err := service.mediaOperationState(ctx, stored)
		require.NoError(t, err)
		return derived.OperationState + "/" + derived.CoverageState
	}
	// A restore starts a new processing incarnation while both jobs are queued.
	var archive bytes.Buffer
	require.NoError(t, f.catalog.ExportMetadata(ctx, &archive))
	restored, err := store.Open(filepath.Join(t.TempDir(), "restored.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, restored.Close()) })
	require.NoError(t, restored.ImportMetadata(ctx, &archive))

	unprocessed := receipt(job.ID, "succeeded")
	unprocessed.ProcessingProfile, unprocessed.CoverageState = "", "unprocessed"
	for _, check := range []struct {
		name, want string
		service    *Service
		stored     store.MediaPublicationReceipt
	}{
		{"rendition queued", "queued/pending", f.service, receipt(job.ID, "queued")},
		{"older processing incarnation", "failed/unavailable", &Service{catalog: restored}, receipt(job.ID, "queued")},
		{"unknown job while queued", "failed/unavailable", f.service, receipt(processingHash("missing"), "queued")},
		{"unknown job after success", "succeeded/transcribed", f.service, receipt(processingHash("missing"), "succeeded")},
		{"admission without a job", "queued/pending", f.service, receipt("", "queued")},
		{"no processing profile", "succeeded/unprocessed", f.service, unprocessed},
	} {
		require.Equal(t, check.want, derive(check.service, check.stored), check.name)
	}
	require.NoError(t, f.run(t, job.ID))
	require.Equal(t, "succeeded/transcribed", derive(f.service, receipt(job.ID, "queued")), "rendition completed")
	changedProfile := receipt(job.ID, "queued")
	changedProfile.ProcessingProfileFingerprint = processingHash("earlier-speech-profile")
	require.Equal(t, "failed/unavailable", derive(f.service, changedProfile),
		"the backfill fails a receipt whose profile changed after admission")
	require.Equal(t, "failed/unavailable", derive(f.service, receipt(job.ID, "failed")), "stored failure wins")
	f.provider.renderErr = workerProviderError(t, document.RenditionErrorUnsupportedInput)
	_ = f.run(t, failingJob.ID)
	require.Equal(t, "failed/unavailable", derive(f.service, receipt(failingJob.ID, "queued")), "rendition failed")
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

// TestMediaProcessingTransientStorageErrorKeepsReceiptQueued catches a
// temporary catalog failure permanently failing a media operation.
func TestMediaProcessingTransientStorageErrorKeepsReceiptQueued(t *testing.T) {
	t.Parallel()
	f := newMediaStateFixture(t)
	_, err := f.service.SubmitSuppliedMedia(t.Context(), f.suppliedRequest("00000000-0000-4000-8000-000000000921",
		&MediaProcessingRequest{Profile: "speech"}))
	require.NoError(t, err)
	targets, err := f.service.MediaProcessingTargets(t.Context(), "", 10)
	require.NoError(t, err)
	require.Len(t, targets, 1)
	transient := fmt.Errorf("reading processing status: %w", sql.ErrConnDone)
	require.ErrorIs(t, f.service.failMediaProcessing(t.Context(), targets[0], transient), sql.ErrConnDone)
	targets, err = f.service.MediaProcessingTargets(t.Context(), "", 10)
	require.NoError(t, err)
	require.Len(t, targets, 1, "the backfill retries the receipt")
}
