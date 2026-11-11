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
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/docling"
	"go.kenn.io/docbank/document/media"
	"go.kenn.io/docbank/document/media/mediatest"
	"go.kenn.io/docbank/document/mediatranscript"
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
	service, err := NewService(t.Context(), ServiceConfig{
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
	case runErr := <-done:
		t.Fatalf("supervised worker did not reach provider: %v", runErr)
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
	// Embedding retry delays and the continuation's poll run on the bubble's clock.
	synctest.Test(t, func(t *testing.T) {
		root := t.TempDir()
		catalog, err := store.Open(t.Context(), filepath.Join(root, "docbank.db"))
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
			service, serviceErr := NewService(t.Context(), ServiceConfig{
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
		catalog, err = store.Open(t.Context(), filepath.Join(root, "docbank.db"))
		require.NoError(t, err)
		blobs, err = blob.New(store.NewPackCatalog(catalog), filepath.Join(root, "blobs"))
		require.NoError(t, err)
		service = newService()
		embeddingFixture.runtime.failures[direct.Name] = []error{
			embeddingTransientError{}, embeddingTransientError{}, embeddingTransientError{},
		}
		exhausted := make(chan struct{})
		embeddingFixture.runtime.inspectInputs = func([]document.EmbeddingInput) {
			if embeddingFixture.runtime.callCount(direct.Name) == 3 {
				close(exhausted)
			}
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
		<-exhausted
		// The continuation now polls the embedding job through its retry backoff.
		synctest.Wait()
		retrying, statusErr := service.Status(t.Context(), receipt.JobID)
		require.NoError(t, statusErr)
		require.Equal(t, "retry_wait", retrying.State)
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
	})
}

type mediaWorkerProvider struct {
	*workerProvider

	timedWithoutArtifact bool
}

func (provider *mediaWorkerProvider) Render(
	ctx context.Context,
	upload document.AuthorizedUpload,
	authorization document.RenditionAuthorization,
) (document.RenditionResult, error) {
	result, err := provider.workerProvider.Render(ctx, upload, authorization)
	if err == nil && provider.timedWithoutArtifact {
		policy, policyErr := document.NewEvidencePolicy(1 << 20)
		if policyErr != nil {
			return document.RenditionResult{}, policyErr
		}
		result.Evidence, _, err = mediatranscript.Build(mediatranscript.ArtifactV1{
			ContractVersion: "media-transcript/v1", Origin: "generated", Provider: "synthetic",
			Segments: []mediatranscript.Segment{{Text: "generated speech", EndMS: 1000}},
		}, "audio", policy)
		result.Evidence.Artifacts = nil
	}
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

func newMediaStateFixture(tb testing.TB) mediaStateFixture {
	tb.Helper()
	f := newMediaStateFixtureWithoutConsent(tb)
	plan, err := f.service.Plan(tb.Context(), f.selector)
	require.NoError(tb, err)
	_, err = f.service.GrantConsent(tb.Context(), ConsentGrantRequest{Selector: f.selector, PlanFingerprint: plan.Fingerprint})
	require.NoError(tb, err)
	return f
}

func newMediaStateFixtureWithoutConsent(tb testing.TB, contracts ...string) mediaStateFixture {
	tb.Helper()
	fixture := newPublicationFixture(tb)
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
	require.NoError(tb, err)
	if len(contracts) > 0 {
		descriptor.SupportedFormats = []document.RenditionFormatCapability{{MediaFamily: "video", MediaType: "video/mp4", InputKind: document.RenditionInputOriginalFile}}
		if contracts[0] == docling.ASRQuickTimeAdapterContract {
			descriptor.SupportedFormats = append(descriptor.SupportedFormats, document.RenditionFormatCapability{
				MediaFamily: "video", MediaType: "video/quicktime", InputKind: document.RenditionInputOriginalFile,
			})
		}
		descriptor.ArtifactRoles = []document.EvidenceArtifactRole{document.EvidenceArtifactTranscript}
		descriptor.Fingerprint = ""
		descriptor, err = document.NewRenditionDescriptor(descriptor)
		require.NoError(tb, err)
	}
	provider := &mediaWorkerProvider{workerProvider: &workerProvider{descriptor: descriptor}}
	record := workerProcessingProfile(tb, provider.Descriptor())
	var portable document.ProcessingProfileV1
	require.NoError(tb, json.Unmarshal(record.CanonicalProfile, &portable))
	if len(contracts) > 0 {
		portable.Rendition.AdapterContract = contracts[0]
		portable.Rendition.RequestedArtifacts = descriptor.ArtifactRoles
	}
	portable.Rendition.TrustBoundary = string(provider.Descriptor().TrustBoundary)
	service, err := NewService(tb.Context(), ServiceConfig{
		Catalog: fixture.catalog, Blobs: fixture.blobs, Gate: newWorkerTestGate(),
		SpoolDirectory: tb.TempDir(), Principal: "operator:synthetic", Scope: "document-processing",
		Profiles: map[string]ProfileConfig{"speech": {Profile: portable, RenditionProvider: provider}},
	})
	require.NoError(tb, err)
	f := mediaStateFixture{publicationFixture: fixture, service: service, provider: provider}
	f.version, f.selector = f.addWAV(tb, "source.wav", mediatest.WAV())
	return f
}

func (f mediaStateFixture) addWAV(tb testing.TB, name string, raw []byte) (store.ContentVersion, Selector) {
	tb.Helper()
	written, err := f.blobs.WriteDetailedContext(tb.Context(), bytes.NewReader(raw))
	require.NoError(tb, err)
	node, err := f.catalog.CreateFile(tb.Context(), f.catalog.RootID(), name,
		written.Hash, written.Size, "audio/wav", processingBlobPhysical(tb, written))
	require.NoError(tb, err)
	version, err := f.catalog.ContentVersionByID(tb.Context(), node.CurrentVersionID)
	require.NoError(tb, err)
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

func (f mediaStateFixture) run(tb testing.TB, jobID string) error {
	tb.Helper()
	waiter, err := f.catalog.RenditionJobWaiterByID(tb.Context(), jobID)
	require.NoError(tb, err)
	worker, err := NewRenditionWorker(RenditionWorkerConfig{
		Catalog: f.catalog, Blobs: f.blobs, Runtime: f.service.renditions, Gate: newWorkerTestGate(),
		Owner: "media-state-worker", LeaseDuration: time.Minute, IdleDelay: time.Millisecond,
	})
	require.NoError(tb, err)
	_, err = worker.RunJob(tb.Context(), waiter.JobID)
	return err
}

func TestMediaBackfillPreservesCompletedRevokedInput(t *testing.T) {
	t.Parallel()
	for _, completed := range []bool{false, true} {
		t.Run(fmt.Sprintf("completed=%t", completed), func(t *testing.T) {
			f := newMediaStateFixture(t)
			name, profile, err := NewSuppliedMediaProfile(f.catalog, f.blobs, f.service.principal)
			require.NoError(t, err)
			f.service, err = NewService(t.Context(), ServiceConfig{
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
			if completed {
				require.NoError(t, f.run(t, queued.JobID))
			}
			targets, err := f.service.MediaProcessingTargets(t.Context(), "", 10)
			require.NoError(t, err)
			require.Len(t, targets, 1)
			_, err = f.service.RevokeMediaOccurrence(t.Context(), "00000000-0000-4000-8000-000000000724", retained.OccurrenceID, "1")
			require.NoError(t, err)
			for _, phase := range []string{"before backfill", "after backfill"} {
				t.Run(phase, func(t *testing.T) {
					status, err := f.service.MediaStatus(t.Context(), retained.SourceID)
					require.NoError(t, err)
					require.Equal(t, map[bool]string{true: "succeeded", false: "queued"}[completed], status.OperationState)
					require.Equal(t, "stale", status.CoverageState)
				})
				require.NoError(t, f.service.ContinueMediaProcessing(t.Context(), targets[0]))
			}
			targets, err = f.service.MediaProcessingTargets(t.Context(), "", 10)
			require.NoError(t, err)
			if completed {
				require.Empty(t, targets)
			} else {
				require.Len(t, targets, 1)
				require.NoError(t, f.run(t, queued.JobID))
				require.NoError(t, f.service.ContinueMediaProcessing(t.Context(), targets[0]))
				status, err := f.service.MediaStatus(t.Context(), retained.SourceID)
				require.NoError(t, err)
				require.Equal(t, "failed", status.OperationState)
			}
		})
	}
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
			service, err = NewService(t.Context(), ServiceConfig{Catalog: f.catalog, Blobs: f.blobs, Gate: newWorkerTestGate(),
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
	restored, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "restored.db"))
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

func TestWatchImportAdmission(t *testing.T) {
	t.Parallel()
	for _, state := range []string{"required", "active", "revoked", "expired", "stale version", "unsupported source"} {
		t.Run(state, func(t *testing.T) {
			f := newMediaStateFixtureWithoutConsent(t)
			node, err := f.catalog.NodeByID(t.Context(), f.selector.NodeID)
			require.NoError(t, err)
			if state != "required" {
				plan, err := f.service.Plan(t.Context(), f.selector)
				require.NoError(t, err)
				_, err = f.service.GrantConsent(t.Context(), ConsentGrantRequest{Selector: f.selector, PlanFingerprint: plan.Fingerprint})
				require.NoError(t, err)
			}
			switch state {
			case "active", "stale version":
				raw := mediatest.WAV()
				raw[len(raw)-1] = 129
				written, err := f.blobs.WriteDetailedContext(t.Context(), bytes.NewReader(raw))
				require.NoError(t, err)
				updated, _, err := f.catalog.ReplaceContent(t.Context(), node.ID, node.Revision, written.Hash, written.Size, "audio/wav", processingBlobPhysical(t, written))
				require.NoError(t, err)
				require.NotEqual(t, node.CurrentVersionID, updated.CurrentVersionID)
				if state == "active" {
					node = updated
				}
			case "unsupported source":
				written, err := f.blobs.WriteDetailedContext(t.Context(), bytes.NewBufferString("synthetic text"))
				require.NoError(t, err)
				node, err = f.catalog.CreateFile(t.Context(), f.catalog.RootID(), "notes.txt", written.Hash, written.Size, "text/plain", processingBlobPhysical(t, written))
				require.NoError(t, err)
			case "revoked":
				_, err := f.service.RevokeConsent(t.Context())
				require.NoError(t, err)
			case "expired":
				_, err := f.service.RevokeConsent(t.Context())
				require.NoError(t, err)
				authorization := f.service.renditionConsentRequest(f.service.profiles["speech"])
				_, err = f.catalog.GrantConsent(t.Context(), store.ProcessingConsentGrantRequest{
					Principal: authorization.Principal, Scope: authorization.Scope,
					ProfileFingerprint: authorization.ProfileFingerprint, DisclosureFingerprint: authorization.DisclosureFingerprint,
					InputClasses: authorization.InputClasses, RetainedArtifactClasses: authorization.RetainedArtifactClasses,
					ExpiresAt: new(time.Now().Add(-time.Minute)),
				})
				require.NoError(t, err)
				got, err := f.service.RenditionConsentState(t.Context(), "speech")
				require.NoError(t, err)
				require.Equal(t, "expired", got)
			}
			require.NoError(t, f.service.AdmitImportedRendition(t.Context(), node, "speech"))
			claim, queued, err := f.catalog.ClaimNextRenditionJob(t.Context(), "synthetic-worker", time.Now(), time.Minute)
			require.NoError(t, err)
			require.Equal(t, state == "active", queued)
			if queued {
				work, err := f.catalog.RenditionJobWorkByClaim(t.Context(), claim, time.Now())
				require.NoError(t, err)
				require.Equal(t, node.CurrentVersionID, work.Waiter.ContentVersionID)
			}
		})
	}
}

func TestWatchMP4ImportAdmission(t *testing.T) {
	t.Parallel()
	for _, state := range []string{"missing", "active", "revoked"} {
		t.Run(state, func(t *testing.T) {
			f := newMediaStateFixtureWithoutConsent(t, docling.ASRVideoAdapterContract)
			raw := mediatest.H264AACMP4()
			written, err := f.blobs.WriteDetailedContext(t.Context(), bytes.NewReader(raw))
			require.NoError(t, err)
			node, err := f.catalog.CreateFile(t.Context(), f.catalog.RootID(), "phone.mp4", written.Hash, written.Size, "video/mp4", processingBlobPhysical(t, written))
			require.NoError(t, err)
			selector := Selector{NodeID: node.ID, ContentVersionID: node.CurrentVersionID, Profile: "speech"}
			plan, err := f.service.Plan(t.Context(), selector)
			require.NoError(t, err)
			if state != "missing" {
				_, err = f.service.GrantConsent(t.Context(), ConsentGrantRequest{Selector: selector, PlanFingerprint: plan.Fingerprint})
				require.NoError(t, err)
			}
			if state == "revoked" {
				_, err = f.service.RevokeConsent(t.Context())
				require.NoError(t, err)
			}
			require.NoError(t, f.service.AdmitImportedRendition(t.Context(), node, "speech"))
			_, queued, err := f.catalog.ClaimNextRenditionJob(t.Context(), "synthetic-worker", time.Now(), time.Minute)
			require.NoError(t, err)
			require.Equal(t, state == "active", queued)
		})
	}
}

func TestMP4InspectionContractSelection(t *testing.T) {
	t.Parallel()
	data := mediatest.H264AACMP4()
	for _, contract := range []string{docling.ASRVideoAdapterContract, docling.ASRM4AAdapterContract, "rendition-adapter/v1"} {
		t.Run(contract, func(t *testing.T) {
			f := newMediaStateFixtureWithoutConsent(t, contract)
			profile := f.service.profiles["speech"]
			version := store.ContentVersion{Size: int64(len(data)), MimeType: "video/mp4", BlobHash: processingSHA256(data)}
			policy := inspectionPolicy("phone.mp4", version, profile)
			require.Equal(t, contract == docling.ASRVideoAdapterContract, policy.VideoAudioTrack)
			record, err := media.InspectCapability(t.Context(), bytes.NewReader(data), policy)
			require.NoError(t, err)
			if contract != docling.ASRVideoAdapterContract {
				legacy := policy
				legacy.VideoAudioTrack = false
				previous, err := media.InspectCapability(t.Context(), bytes.NewReader(data), legacy)
				require.NoError(t, err)
				require.Equal(t, previous, record, "supplied captions and existing contracts retain sealed identities")
				require.Equal(t, previous.Checksum, record.Checksum)
			}
		})
	}
}

func TestSuppliedCaptionsMP4InspectionIdentity(t *testing.T) {
	t.Parallel()
	f := newPublicationFixture(t)
	name, supplied, err := NewSuppliedCaptionProfile(f.catalog, f.blobs, "operator:synthetic")
	require.NoError(t, err)
	service, err := NewService(t.Context(), ServiceConfig{Catalog: f.catalog, Blobs: f.blobs,
		Gate: newWorkerTestGate(), SpoolDirectory: t.TempDir(), Principal: "operator:synthetic",
		Profiles: map[string]ProfileConfig{name: supplied}})
	require.NoError(t, err)
	data := mediatest.H264AACMP4()
	version := store.ContentVersion{Size: int64(len(data)), MimeType: "video/mp4", BlobHash: processingSHA256(data)}
	policy := inspectionPolicy("phone.mp4", version, service.profiles[name])
	require.False(t, policy.VideoAudioTrack)
	record, err := media.InspectCapability(t.Context(), bytes.NewReader(data), policy)
	require.NoError(t, err)
	legacy := policy
	legacy.VideoAudioTrack = false
	previous, err := media.InspectCapability(t.Context(), bytes.NewReader(data), legacy)
	require.NoError(t, err)
	require.Equal(t, previous, record)
	require.Equal(t, previous.Checksum, record.Checksum)
	encoded, err := json.Marshal(policy)
	require.NoError(t, err)
	require.NotContains(t, string(encoded), "video_audio_track", "legacy policy bytes must omit the new flag")
}

func TestWatchQuickTimeImportAdmission(t *testing.T) {
	t.Parallel()
	for _, disclose := range []bool{false, true} {
		t.Run(fmt.Sprintf("disclose=%t", disclose), func(t *testing.T) {
			f := newMediaStateFixtureWithoutConsent(t, docling.ASRQuickTimeAdapterContract)
			legacy := newMediaStateFixtureWithoutConsent(t, docling.ASRVideoAdapterContract)
			v4 := f.service.profiles["speech"].portable
			v4.Rendition.DiscloseFilename = disclose
			v3 := legacy.service.profiles["speech"].portable
			service, err := NewService(t.Context(), ServiceConfig{
				Catalog: f.catalog, Blobs: f.blobs, Gate: newWorkerTestGate(), SpoolDirectory: t.TempDir(),
				Principal: "operator:synthetic", Scope: "document-processing",
				Profiles: map[string]ProfileConfig{
					"v3": {Profile: v3, RenditionProvider: legacy.provider},
					"v4": {Profile: v4, RenditionProvider: f.provider},
				},
			})
			require.NoError(t, err)
			importFile := func(name, mime string, data []byte) store.Node {
				written, err := f.blobs.WriteDetailedContext(t.Context(), bytes.NewReader(data))
				require.NoError(t, err)
				node, err := f.catalog.CreateFile(t.Context(), f.catalog.RootID(), name, written.Hash, written.Size, mime, processingBlobPhysical(t, written))
				require.NoError(t, err)
				return node
			}
			grant := func(node store.Node, profile string) {
				selector := Selector{NodeID: node.ID, ContentVersionID: node.CurrentVersionID, Profile: profile}
				plan, err := service.Plan(t.Context(), selector)
				require.NoError(t, err)
				require.Empty(t, plan.RenditionIneligibleReason)
				_, err = service.GrantConsent(t.Context(), ConsentGrantRequest{Selector: selector, PlanFingerprint: plan.Fingerprint})
				require.NoError(t, err)
			}

			mp4 := importFile("phone.mp4", "video/mp4", mediatest.H264AACMP4())
			grant(mp4, "v3")
			mov := importFile("phone.mov", "video/quicktime", mediatest.H264AACMOV())
			require.NoError(t, service.AdmitImportedRendition(t.Context(), mov, "v3"))
			require.NoError(t, service.AdmitImportedRendition(t.Context(), mov, "v4"))
			_, queued, err := f.catalog.ClaimNextRenditionJob(t.Context(), "synthetic-worker", time.Now(), time.Minute)
			require.NoError(t, err)
			require.False(t, queued, "the v3 grant cannot authorize MOV under either contract")

			grant(mov, "v4")
			wrong := importFile("wrong.mp4", "video/quicktime", mediatest.H264AACMOV())
			plan, err := service.Plan(t.Context(), Selector{NodeID: wrong.ID, ContentVersionID: wrong.CurrentVersionID, Profile: "v4"})
			require.NoError(t, err)
			require.Equal(t, string(media.CapabilityReasonSourceName), plan.RenditionIneligibleReason)
			require.NoError(t, service.AdmitImportedRendition(t.Context(), wrong, "v4"))
			_, queued, err = f.catalog.ClaimNextRenditionJob(t.Context(), "synthetic-worker", time.Now(), time.Minute)
			require.NoError(t, err)
			require.False(t, queued)

			require.NoError(t, service.AdmitImportedRendition(t.Context(), mov, "v4"))
			claim, queued, err := f.catalog.ClaimNextRenditionJob(t.Context(), "synthetic-worker", time.Now(), time.Minute)
			require.NoError(t, err)
			require.True(t, queued)
			work, err := f.catalog.RenditionJobWorkByClaim(t.Context(), claim, time.Now())
			require.NoError(t, err)
			require.Equal(t, mov.CurrentVersionID, work.Waiter.ContentVersionID)
		})
	}
}

func TestV3InspectionPolicyFingerprints(t *testing.T) {
	t.Parallel()
	data := []byte("synthetic policy identity source")
	profile := configuredProfile{
		portable: document.ProcessingProfileV1{Rendition: &document.RenditionBindingV1{
			AdapterContract: docling.ASRVideoAdapterContract, DisclosureFingerprint: strings.Repeat("3", 64),
			MaxDocumentBytes: 1 << 20, MaxUnits: 100,
		}},
		record:   store.ProcessingProfileRecord{Fingerprint: strings.Repeat("2", 64)},
		provider: &workerProvider{descriptor: document.RenditionDescriptor{Fingerprint: strings.Repeat("1", 64)}},
	}
	for _, tc := range []struct{ name, mime, fingerprint string }{
		{"phone.wav", "audio/wav", "09fe4b44b4a0d47b1246b5c328b3423ddceb9ca1ba2f5ff09b60bc35f9483219"},
		{"phone.mp3", "audio/mpeg", "8e174c16775e142ed798526e60765c1c682edb5f652e9653b13a6dc1bc402693"},
		{"phone.m4a", "audio/mp4", "97b6cb5f29f3fad7caf1b930a8f8437a0116eec339550ac6154f85edd899e9a3"},
		{"phone.mp4", "video/mp4", "910e30e4a37650af43d0bd6c0385fe6f1385ba2979f235e2af73b3c38e01645e"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			version := store.ContentVersion{MimeType: tc.mime, BlobHash: processingSHA256(data), Size: int64(len(data))}
			policy := inspectionPolicy(tc.name, version, profile)
			record, err := media.InspectCapability(t.Context(), bytes.NewReader(data), policy)
			require.NoError(t, err)
			require.Equal(t, tc.fingerprint, record.PolicyFingerprint)
		})
	}
}
