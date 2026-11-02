package processing

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/media/mediatest"
	"go.kenn.io/docbank/document/plaintext"
	"go.kenn.io/docbank/internal/config"
	"go.kenn.io/docbank/internal/retrieval"
	"go.kenn.io/docbank/internal/store"
	"go.kenn.io/docbank/internal/vectorworker"
)

func TestLocalTextFailedConsentAndSavePreserveSelection(t *testing.T) {
	t.Parallel()
	fixture := newPublicationFixture(t)
	plain, err := plaintext.New(plaintext.Profile{MaxDocumentBytes: 1 << 20})
	require.NoError(t, err)
	renderer := plain
	stored := workerProcessingProfile(t, renderer.Descriptor())
	var portable document.ProcessingProfileV1
	require.NoError(t, json.Unmarshal(stored.CanonicalProfile, &portable))
	portable.Rendition.TrustBoundary = string(renderer.Descriptor().TrustBoundary)
	config := ServiceConfig{Catalog: fixture.catalog, Blobs: fixture.blobs, Gate: newWorkerTestGate(), SpoolDirectory: t.TempDir(),
		Profiles: map[string]ProfileConfig{"local-text": {Profile: portable, RenditionProvider: renderer,
			RerankingProvider:   consumerReranker{source: &consumerSource{}},
			RerankingProfile:    retrieval.RerankingProfile{ID: "synthetic-reranker", MaxCandidates: 2, MaxExcerptBytes: 4096},
			RerankingDeadline:   time.Second,
			RerankingDisclosure: RuntimeDisclosure{ImmediateProcessor: "synthetic", UltimateProcessor: "synthetic", Endpoint: "https://rerank.example.invalid", Deployment: workerHash("reranker")},
		}}}
	service, err := NewService(t.Context(), config)
	require.NoError(t, err)
	defer service.Stop()
	setup := &LocalTextSetup{Service: service, Selected: "local-text"}
	gateFailure := errors.New("synthetic grant failure")
	saveFailure := errors.New("synthetic save failure")
	setup.Candidate = func(string) (*Service, error) {
		next := config
		next.Gate = &localTextGrantGate{workerTestGate: newWorkerTestGate(), err: gateFailure}
		s, err := NewService(t.Context(), next)
		return s, err
	}
	setup.Startup = func(context.Context, bool, string) error { return nil }
	saved := "previous-profile"
	setup.Save = func(name, _ string) error { saved = name; return nil }
	// Preview and apply review the operation without reopening originals.
	plan, err := setup.Plan(t.Context(), "local")
	require.NoError(t, err)
	require.Len(t, plan.Flow, 2)
	require.Equal(t, "reranking", plan.Flow[1].Capability)
	_, err = setup.Apply(t.Context(), "local", plan.Fingerprint)
	require.ErrorIs(t, err, gateFailure)
	require.Equal(t, "previous-profile", saved)
	setup.Candidate = func(string) (*Service, error) {
		s, err := NewService(t.Context(), config)
		return s, err
	}
	setup.Save = func(string, string) error { return saveFailure }
	_, err = setup.Apply(t.Context(), "local", plan.Fingerprint)
	require.ErrorIs(t, err, ErrConsentSaved)
	require.ErrorIs(t, err, saveFailure)
	require.Equal(t, "previous-profile", saved)
	for _, request := range service.profileConsentRequests(service.profiles["local-text"]) {
		_, err := fixture.catalog.AuthorizeProviderOperation(t.Context(), request)
		require.NoError(t, err, "consent remains saved when the selection write fails")
	}
}

type localTextGrantGate struct {
	*workerTestGate

	err error
}

func (gate *localTextGrantGate) MutateContext(context.Context, func() error) error { return gate.err }

func TestLocalTextReconsidersRenamedPDF(t *testing.T) {
	t.Parallel()
	renderer := newWorkerProvider(t)
	fixture, setup := newLocalTextBackfillFixture(t, renderer)
	written, err := fixture.blobs.WriteDetailedContext(t.Context(), bytes.NewReader(mediatest.PDF()))
	require.NoError(t, err)
	node, err := fixture.catalog.CreateFile(t.Context(), fixture.catalog.RootID(), "report.txt", written.Hash, written.Size, "application/pdf", processingBlobPhysical(t, written))
	require.NoError(t, err)
	item := store.DocumentSummary{NodeID: node.ID, ContentVersionID: node.CurrentVersionID, Name: node.Name}
	require.NoError(t, setup.process(t.Context(), item))
	require.Zero(t, renderer.calls)
	renamed, _, err := fixture.catalog.Move(t.Context(), node.ID, *node.ParentID, "report.pdf", store.UnconditionalRev)
	require.NoError(t, err)
	require.Equal(t, item.ContentVersionID, renamed.CurrentVersionID)
	item.Name = renamed.Name
	targets, err := setup.targets(t.Context(), "", 50)
	require.NoError(t, err)
	require.Contains(t, targets, item)
	require.NoError(t, setup.process(t.Context(), item))
	require.Equal(t, 1, renderer.calls)
}

func newLocalTextBackfillFixture(tb testing.TB, renderer document.RenditionProvider, providers ...document.EmbeddingProvider) (publicationFixture, *LocalTextSetup) {
	tb.Helper()
	fixture := newPublicationFixture(tb)
	stored := workerProcessingProfile(tb, renderer.Descriptor())
	var portable, embeddings document.ProcessingProfileV1
	require.NoError(tb, json.Unmarshal(stored.CanonicalProfile, &portable))
	embedder := document.EmbeddingProvider(localTextEmbeddingProvider{descriptor: embeddingWorkerDescriptor(tb)})
	if len(providers) != 0 {
		embedder = providers[0]
	}
	descriptor := embedder.Descriptor()
	require.NoError(tb, json.Unmarshal(embeddingWorkerProfile(tb, descriptor).CanonicalProfile, &embeddings))
	portable.Embeddings = embeddings.Embeddings[:1]
	portable.Rendition.DiscloseFilename = true
	portable.Rendition.TrustBoundary = string(renderer.Descriptor().TrustBoundary)
	service, err := NewService(tb.Context(), ServiceConfig{Catalog: fixture.catalog, Blobs: fixture.blobs, Gate: newWorkerTestGate(), SpoolDirectory: tb.TempDir(),
		Profiles: map[string]ProfileConfig{"pdf": {Profile: portable, RenditionProvider: renderer,
			EmbeddingProviders: map[string]document.EmbeddingProvider{"chunk": embedder},
			Tokenizers:         map[string]document.Tokenizer{"chunk": embeddingIntegrationTokenizer{}},
		}}})
	require.NoError(tb, err)
	tb.Cleanup(service.Stop)
	require.NoError(tb, service.grantProfileConsent(tb.Context(), service.profiles["pdf"], nil))
	return fixture, &LocalTextSetup{Service: service, Selected: "pdf"}
}

func TestLocalTextSkipsIneligibleSweepAndPrunesAbsentInputs(t *testing.T) {
	t.Parallel()
	fixture, setup := newLocalTextBackfillFixture(t, newWorkerProvider(t))
	version, err := fixture.catalog.ContentVersionByID(t.Context(), fixture.versionID)
	require.NoError(t, err)
	for i := range 1029 {
		_, err := fixture.catalog.CreateFile(t.Context(), fixture.catalog.RootID(), fmt.Sprintf("ineligible-%04d.pdf", i), version.BlobHash, version.Size, "application/pdf")
		require.NoError(t, err)
	}
	count, after := 0, ""
	for {
		targets, err := setup.targets(t.Context(), after, 50)
		require.NoError(t, err)
		if len(targets) == 0 {
			break
		}
		for _, target := range targets {
			require.NoError(t, setup.process(t.Context(), target))
		}
		count += len(targets)
		after = targets[len(targets)-1].ContentVersionID
		if len(targets) < 50 {
			targets, err := setup.targets(t.Context(), after, 50)
			require.NoError(t, err)
			require.Empty(t, targets, "the last examined cursor stays at the end of the sweep")
			break
		}
	}
	require.Equal(t, 1030, count)
	targets, err := setup.targets(t.Context(), "", 50)
	require.NoError(t, err)
	require.Empty(t, targets, "a fully ineligible sweep lets the lane idle")
	node, err := fixture.catalog.NodeByID(t.Context(), version.NodeID)
	require.NoError(t, err)
	trashed, _, err := fixture.catalog.Trash(t.Context(), node.ID, node.Revision)
	require.NoError(t, err)
	targets, err = setup.targets(t.Context(), "", 50)
	require.NoError(t, err)
	require.Empty(t, targets)
	_, _, err = fixture.catalog.Restore(t.Context(), node.ID, trashed.Revision)
	require.NoError(t, err)
	targets, err = setup.targets(t.Context(), "", 50)
	require.NoError(t, err)
	require.Equal(t, []store.DocumentSummary{{NodeID: node.ID, ContentVersionID: version.ID, Name: node.Name}}, targets)
}

func TestLocalTextInspectionErrorsRemainRetryable(t *testing.T) {
	t.Parallel()
	fixture, setup := newLocalTextBackfillFixture(t, newWorkerProvider(t))
	targets, err := setup.targets(t.Context(), "", 50)
	require.NoError(t, err)
	require.Len(t, targets, 1)
	version, err := fixture.catalog.ContentVersionByID(t.Context(), fixture.versionID)
	require.NoError(t, err)
	require.NoError(t, fixture.blobs.Remove(version.BlobHash))
	require.Error(t, setup.process(t.Context(), targets[0]))
	retry, err := setup.targets(t.Context(), "", 50)
	require.NoError(t, err)
	require.Equal(t, targets, retry)
}

type localTextEmbeddingProvider struct {
	descriptor document.EmbeddingDescriptor
	embed      func(context.Context) error
	values     []float32
}

func (p localTextEmbeddingProvider) Descriptor() document.EmbeddingDescriptor { return p.descriptor }
func (p localTextEmbeddingProvider) Embed(ctx context.Context, inputs []document.EmbeddingInput, _ document.EmbeddingAuthorization) (document.EmbeddingResult, error) {
	if p.embed != nil {
		if err := p.embed(ctx); err != nil {
			return document.EmbeddingResult{}, err
		}
	}
	values := p.values
	if values == nil {
		values = []float32{1, 0}
	}
	result := document.EmbeddingResult{}
	for _, input := range inputs {
		result.Vectors = append(result.Vectors, document.EmbeddingVector{Key: input.Key, Values: values})
	}
	return result, nil
}

func TestLocalTextBackfillRecoversAfterProviderOutage(t *testing.T) {
	t.Parallel()
	var ready atomic.Bool
	var calls, clock atomic.Int64
	provider := localTextEmbeddingProvider{descriptor: embeddingWorkerDescriptor(t), embed: func(context.Context) error {
		calls.Add(1)
		if !ready.Load() {
			return errors.New("synthetic server unavailable")
		}
		return nil
	}}
	fixture, setup := newLocalTextBackfillFixture(t, newWorkerProvider(t), provider)
	setup.Service.clock = func() time.Time {
		if calls.Load() == 0 {
			return time.Now().UTC()
		}
		return time.Now().UTC().Add(time.Duration(clock.Add(1)) * 10 * time.Second)
	}
	written, err := fixture.blobs.WriteDetailedContext(t.Context(), bytes.NewReader(mediatest.PDF()))
	require.NoError(t, err)
	node, err := fixture.catalog.CreateFile(t.Context(), fixture.catalog.RootID(), "outage.pdf", written.Hash, written.Size, "application/pdf", processingBlobPhysical(t, written))
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	done := make(chan error, 1)
	go func() { done <- setup.Backfill(ctx, nil) }()
	defer func() { cancel(); require.ErrorIs(t, <-done, ctx.Err()) }()
	profile := setup.Service.profiles["pdf"]
	require.Eventually(t, func() bool {
		jobs, err := fixture.catalog.EmbeddingJobsForVersionProfile(ctx, node.CurrentVersionID, profile.record.Fingerprint)
		return err == nil && len(jobs) == 1 && jobs[0].State == "failed" && jobs[0].FailureCode == store.EmbeddingFailureProviderUnavailable
	}, 10*time.Second, 10*time.Millisecond)
	ready.Store(true)
	require.Eventually(t, func() bool {
		jobs, err := fixture.catalog.EmbeddingJobsForVersionProfile(ctx, node.CurrentVersionID, profile.record.Fingerprint)
		return err == nil && len(jobs) == 1 && jobs[0].State == "completed"
	}, 10*time.Second, 10*time.Millisecond)
}

func TestLocalTextTokenizerLimitLeavesSweep(t *testing.T) {
	t.Parallel()
	renderer, err := plaintext.New(plaintext.Profile{MaxDocumentBytes: 16 << 20})
	require.NoError(t, err)
	fixture, setup := newLocalTextBackfillFixture(t, renderer)
	cfg, err := config.Default().WithLocalText()
	require.NoError(t, err)
	resolved, err := cfg.ProcessingProfile(config.LocalTextProfile)
	require.NoError(t, err)
	portable := resolved.Document
	portable.Embeddings = setup.Service.profiles[setup.Selected].portable.Embeddings
	portable.Embeddings[0].MaxInputBytes = 16 << 20
	service, err := NewService(t.Context(), ServiceConfig{Catalog: fixture.catalog, Blobs: fixture.blobs, Gate: newWorkerTestGate(), SpoolDirectory: t.TempDir(), Profiles: map[string]ProfileConfig{"text": {Profile: portable, RenditionProvider: renderer, EmbeddingProviders: map[string]document.EmbeddingProvider{"chunk": localTextEmbeddingProvider{descriptor: embeddingWorkerDescriptor(t)}}, Tokenizers: map[string]document.Tokenizer{"chunk": embeddingIntegrationTokenizer{}}}}})
	require.NoError(t, err)
	defer service.Stop()
	setup = &LocalTextSetup{Service: service, Selected: "text"}
	require.NoError(t, service.grantProfileConsent(t.Context(), service.profiles["text"], nil))
	data := strings.Repeat("word ", 250_001)
	written, err := fixture.blobs.WriteDetailedContext(t.Context(), strings.NewReader(data))
	require.NoError(t, err)
	node, err := fixture.catalog.CreateFile(t.Context(), fixture.catalog.RootID(), "large.txt", written.Hash, written.Size, "text/plain", processingBlobPhysical(t, written))
	require.NoError(t, err)
	item := store.DocumentSummary{NodeID: node.ID, ContentVersionID: node.CurrentVersionID, Name: node.Name}
	plan, err := setup.Service.Plan(t.Context(), Selector{Profile: setup.Selected, NodeID: node.ID, ContentVersionID: node.CurrentVersionID})
	require.NoError(t, err)
	require.Empty(t, plan.RenditionIneligibleReason)
	var logs bytes.Buffer
	setup.logger = slog.New(slog.NewTextHandler(&logs, nil))
	require.NoError(t, setup.process(t.Context(), item))
	require.Contains(t, logs.String(), "tokenizer_limit")
	firstLog := logs.String()
	for range 2 {
		targets, err := setup.targets(t.Context(), "", 50)
		require.NoError(t, err)
		require.NotContains(t, targets, item)
	}
	require.Equal(t, firstLog, logs.String())
	replacement, err := fixture.blobs.WriteDetailedContext(t.Context(), strings.NewReader("short replacement"))
	require.NoError(t, err)
	changed, _, err := fixture.catalog.ReplaceContent(t.Context(), node.ID, node.Revision, replacement.Hash, replacement.Size, "text/plain", processingBlobPhysical(t, replacement))
	require.NoError(t, err)
	targets, err := setup.targets(t.Context(), "", 50)
	require.NoError(t, err)
	require.Contains(t, targets, store.DocumentSummary{NodeID: changed.ID, ContentVersionID: changed.CurrentVersionID, Name: changed.Name})
}

func TestLocalTextHoldsActivityUntilPublication(t *testing.T) {
	t.Parallel()
	for _, missing := range []bool{false, true} {
		name := "published"
		if missing {
			name = "missing_payload"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			fixture, setup := newLocalTextBackfillFixture(t, newWorkerProvider(t))
			initial, err := setup.targets(t.Context(), "", 50)
			require.NoError(t, err)
			for _, item := range initial {
				require.NoError(t, setup.process(t.Context(), item))
			}
			written, err := fixture.blobs.WriteDetailedContext(t.Context(), bytes.NewReader(mediatest.PDF()))
			require.NoError(t, err)
			node, err := fixture.catalog.CreateFile(t.Context(), fixture.catalog.RootID(), "pending-index.pdf", written.Hash, written.Size, "application/pdf", processingBlobPhysical(t, written))
			require.NoError(t, err)
			require.NoError(t, setup.process(t.Context(), store.DocumentSummary{NodeID: node.ID, ContentVersionID: node.CurrentVersionID, Name: node.Name}))
			space := setup.Service.profiles[setup.Selected].vectorSpaces["chunk"]
			if missing {
				source, err := fixture.catalog.CaptureVectorIndexSource(t.Context(), space)
				require.NoError(t, err)
				require.Len(t, source.Members, 1)
				require.NoError(t, fixture.blobs.Remove(source.Members[0].PayloadBlobHash))
				_, err = fixture.catalog.ReadVectorIndexVectorSet(t.Context(), fixture.blobs, source.Members[0])
				require.ErrorIs(t, err, store.ErrVectorSetUnavailable)
			}
			var active atomic.Int64
			setup.ActivityBegin = func() { active.Add(1) }
			setup.ActivityEnd = func() { active.Add(-1) }
			ctx, cancel := context.WithCancel(t.Context())
			done := make(chan error, 1)
			go func() { done <- setup.Backfill(ctx, nil) }()
			defer func() { cancel(); require.ErrorIs(t, <-done, ctx.Err()) }()
			require.Eventually(t, func() bool { return active.Load() == 1 }, time.Second, 10*time.Millisecond, "completed embeddings still need a published vector generation")
			indexer, err := vectorworker.NewIndexWorker(vectorworker.IndexWorkerConfig{Catalog: fixture.catalog, ReadVectorSet: func(ctx context.Context, member store.VectorIndexMember) ([]byte, error) {
				return fixture.catalog.ReadVectorIndexVectorSet(ctx, fixture.blobs, member)
			}, Mutate: func(_ context.Context, fn func() error) error { return fn() }, Owner: "local-text-publication-test", BuildLease: time.Minute, ReaderLease: time.Minute, IdleDelay: time.Second})
			require.NoError(t, err)
			if missing {
				indexed := make(chan error, 1)
				go func() { indexed <- indexer.Run(ctx) }()
				defer func() { cancel(); require.ErrorIs(t, <-indexed, ctx.Err()) }()
			} else {
				generation, err := indexer.Rebuild(ctx, space)
				require.NoError(t, err)
				require.Positive(t, generation.RowCount)
			}
			require.Eventually(t, func() bool { return active.Load() == 0 }, 5*time.Second, 10*time.Millisecond)
			pending, err := setup.Pending(ctx)
			require.NoError(t, err)
			require.False(t, pending, "external re-embedding does not count as active publication")
			if missing {
				_, err = fixture.catalog.ActiveVectorIndexGeneration(ctx, space)
				require.ErrorIs(t, err, store.ErrNotFound)
			}
		})
	}
}

func TestLocalTextPurgeSuppressionControlsTargets(t *testing.T) {
	t.Parallel()
	fixture, setup := newLocalTextBackfillFixture(t, newWorkerProvider(t))
	written, err := fixture.blobs.WriteDetailedContext(t.Context(), bytes.NewReader(mediatest.PDF()))
	require.NoError(t, err)
	node, err := fixture.catalog.CreateFile(t.Context(), fixture.catalog.RootID(), "purged.pdf", written.Hash, written.Size, "application/pdf", processingBlobPhysical(t, written))
	require.NoError(t, err)
	item := store.DocumentSummary{NodeID: node.ID, ContentVersionID: node.CurrentVersionID, Name: node.Name}
	require.NoError(t, setup.process(t.Context(), item))
	profile := setup.Service.profiles[setup.Selected]
	jobs, err := fixture.catalog.EmbeddingJobsForVersionProfile(t.Context(), node.CurrentVersionID, profile.record.Fingerprint)
	require.NoError(t, err)
	require.Len(t, jobs, 1)
	require.Equal(t, "completed", jobs[0].State)
	_, err = fixture.catalog.PurgeDerivatives(t.Context(), store.PurgeRequest{ContentVersionIDs: []string{node.CurrentVersionID}})
	require.NoError(t, err)
	targets, err := setup.targets(t.Context(), "", 50)
	require.NoError(t, err)
	require.NotContains(t, targets, item)
	binding := profile.portable.Embeddings[0]
	require.NoError(t, fixture.catalog.AuthorizeEmbeddingRebuild(t.Context(), store.EmbeddingRebuildAuthorization{
		Key: store.EmbeddingHeadKey{ContentVersionID: node.CurrentVersionID, BindingID: binding.Name, InputKind: binding.InputKind}, ProcessingProfileFingerprint: profile.record.Fingerprint, SetID: workerHash("authorized-local-text-replacement"), AuthorizedAt: time.Now().UTC().Format("2006-01-02T15:04:05.000000000Z07:00")}))
	targets, err = setup.targets(t.Context(), "", 50)
	require.NoError(t, err)
	require.Contains(t, targets, item)
}

func TestLocalTextPermanentFailureLeavesSweep(t *testing.T) {
	t.Parallel()
	for _, failure := range []string{"invalid_response", "rendition_failed", "stale_authority"} {
		t.Run(failure, func(t *testing.T) {
			renderer := newWorkerProvider(t)
			data, name, mime := mediatest.PDF(), "failed.pdf", "application/pdf"
			if failure == "rendition_failed" {
				renderer.renderErr = workerProviderError(t, document.RenditionErrorUnsupportedInput)
			}
			var calls int
			provider := localTextEmbeddingProvider{descriptor: embeddingWorkerDescriptor(t), values: []float32{1}, embed: func(context.Context) error { calls++; return nil }}
			fixture, setup := newLocalTextBackfillFixture(t, renderer, provider)
			initial, err := setup.targets(t.Context(), "", 50)
			require.NoError(t, err)
			for _, item := range initial {
				require.NoError(t, setup.process(t.Context(), item))
			}
			written, err := fixture.blobs.WriteDetailedContext(t.Context(), bytes.NewReader(data))
			require.NoError(t, err)
			node, err := fixture.catalog.CreateFile(t.Context(), fixture.catalog.RootID(), name, written.Hash, written.Size, mime, processingBlobPhysical(t, written))
			require.NoError(t, err)
			item := store.DocumentSummary{NodeID: node.ID, ContentVersionID: node.CurrentVersionID, Name: name}
			if failure == "stale_authority" {
				profile := setup.Service.profiles[setup.Selected]
				version, err := fixture.catalog.ContentVersionByID(t.Context(), node.CurrentVersionID)
				require.NoError(t, err)
				_, err = setup.Service.runRendition(t.Context(), node, version, setup.Selected, profile, setup.Service.principal, setup.Service.scope, nil)
				require.NoError(t, err)
				binding := profile.portable.Embeddings[0]
				generation, err := setup.Service.chunkEmbeddingGeneration(t.Context(), version, profile, binding)
				require.NoError(t, err)
				job, err := fixture.catalog.EnqueueEmbeddingJob(t.Context(), store.EmbeddingJobRequest{ContentVersionID: version.ID, Profile: profile.record, BindingID: binding.Name, VectorSpaceID: profile.vectorSpaces[binding.Name], Descriptor: provider.Descriptor(), InputGeneration: generation,
					Authorization: store.ProviderOperationAuthorizationRequest{Principal: setup.Service.principal, Scope: setup.Service.scope, ProfileFingerprint: profile.record.Fingerprint, DisclosureFingerprint: binding.DisclosureFingerprint, InputClasses: []string{string(binding.InputKind)}, RetainedArtifactClasses: []string{"embedding_vector_set"}}})
				require.NoError(t, err)
				at := time.Now().UTC()
				claim, work, found, err := fixture.catalog.ClaimEmbeddingWork(t.Context(), job.ID, "stale-test", at, time.Minute, []string{provider.Descriptor().Fingerprint})
				require.NoError(t, err)
				require.True(t, found)
				require.NoError(t, fixture.catalog.FailEmbeddingWork(t.Context(), claim, work, store.EmbeddingFailureStaleAuthority, store.EmbeddingAttemptReceipt{AttemptID: job.ID}, at))
			}
			var logs bytes.Buffer
			setup.logger = slog.New(slog.NewTextHandler(&logs, nil))
			require.NoError(t, setup.process(t.Context(), item))
			logged := logs.String()
			require.Contains(t, logged, failure)
			targets, err := setup.targets(t.Context(), "", 50)
			require.NoError(t, err)
			require.Empty(t, targets)
			targets, err = setup.targets(t.Context(), "", 50)
			require.NoError(t, err)
			require.Empty(t, targets)
			require.Equal(t, logged, logs.String())
			require.Equal(t, 1, renderer.calls)
			if failure == "invalid_response" {
				require.Equal(t, 1, calls)
			}
			replacement, err := fixture.blobs.WriteDetailedContext(t.Context(), bytes.NewReader(append(data, '\n')))
			require.NoError(t, err)
			changed, _, err := fixture.catalog.ReplaceContent(t.Context(), node.ID, node.Revision, replacement.Hash, replacement.Size, mime, processingBlobPhysical(t, replacement))
			require.NoError(t, err)
			targets, err = setup.targets(t.Context(), "", 50)
			require.NoError(t, err)
			require.Equal(t, []store.DocumentSummary{{NodeID: changed.ID, ContentVersionID: changed.CurrentVersionID, Name: name}}, targets)
		})
	}
}

func TestLocalTextPublishesDuringBackfillAndOnIdle(t *testing.T) {
	t.Parallel()
	thirdStarted, release := make(chan struct{}), make(chan struct{})
	calls := 0
	provider := localTextEmbeddingProvider{descriptor: embeddingWorkerDescriptor(t), embed: func(ctx context.Context) error {
		calls++
		if calls == 3 {
			close(thirdStarted)
			select {
			case <-release:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		return nil
	}}
	renderer := newWorkerProvider(t)
	renderer.textForSource = true
	fixture, setup := newLocalTextBackfillFixture(t, renderer, provider)
	for i := range 8 {
		data := append(mediatest.PDF(), []byte(fmt.Sprintf("\n%% source %d", i))...)
		written, err := fixture.blobs.WriteDetailedContext(t.Context(), bytes.NewReader(data))
		require.NoError(t, err)
		_, err = fixture.catalog.CreateFile(t.Context(), fixture.catalog.RootID(), fmt.Sprintf("batch-%d.pdf", i), written.Hash, written.Size, "application/pdf", processingBlobPhysical(t, written))
		require.NoError(t, err)
	}
	indexer, err := vectorworker.NewIndexWorker(vectorworker.IndexWorkerConfig{Catalog: fixture.catalog,
		ReadVectorSet: func(ctx context.Context, member store.VectorIndexMember) ([]byte, error) {
			return fixture.catalog.ReadVectorIndexVectorSet(ctx, fixture.blobs, member)
		}, Mutate: setup.Service.gate.MutateContext, Owner: "index-worker", BuildLease: 5 * time.Minute, ReaderLease: 5 * time.Minute, IdleDelay: time.Second})
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	backfillDone, indexDone := make(chan error, 1), make(chan error, 1)
	indexStarted := false
	go func() { backfillDone <- setup.Backfill(ctx, nil) }()
	defer func() {
		cancel()
		require.ErrorIs(t, <-backfillDone, ctx.Err())
		if indexStarted {
			require.ErrorIs(t, <-indexDone, ctx.Err())
		}
	}()
	select {
	case <-thirdStarted:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	space := setup.Service.profiles["pdf"].vectorSpaces["chunk"]
	before, err := fixture.catalog.ActiveVectorIndexGeneration(ctx, space)
	if !errors.Is(err, store.ErrNotFound) {
		require.NoError(t, err)
		require.Zero(t, before.RowCount, "backfill defers publication to the recurring worker")
	}
	go func() { indexDone <- indexer.Run(ctx) }()
	indexStarted = true
	var during store.VectorIndexGenerationRecord
	require.Eventually(t, func() bool {
		var err error
		during, err = fixture.catalog.ActiveVectorIndexGeneration(ctx, space)
		return err == nil && during.RowCount == 2
	}, 5*time.Second, 10*time.Millisecond, "the recurring publisher runs while backfill is still active")
	close(release)
	require.Eventually(t, func() bool {
		generation, err := fixture.catalog.ActiveVectorIndexGeneration(ctx, space)
		return err == nil && generation.RowCount == 8 && generation.ID != during.ID
	}, 5*time.Second, 10*time.Millisecond, "the recurring publisher includes the final documents")
}

func BenchmarkLocalTextIdleSweep(b *testing.B) {
	fixture, setup := newLocalTextBackfillFixture(b, newWorkerProvider(b), localTextEmbeddingProvider{descriptor: embeddingWorkerDescriptor(b)})
	version, err := fixture.catalog.ContentVersionByID(b.Context(), fixture.versionID)
	require.NoError(b, err)
	written, err := fixture.blobs.WriteDetailedContext(b.Context(), bytes.NewReader(mediatest.PDF()))
	require.NoError(b, err)
	setup.skips = map[localTextSkipKey]*localTextSkip{{version.ID, "source.pdf"}: {seen: true}}
	for i := range 19999 {
		name := fmt.Sprintf("input-%05d.pdf", i)
		if i < 18999 {
			node, err := fixture.catalog.CreateFile(b.Context(), fixture.catalog.RootID(), name, version.BlobHash, version.Size, "application/pdf")
			require.NoError(b, err)
			setup.skips[localTextSkipKey{node.CurrentVersionID, name}] = &localTextSkip{seen: true}
			continue
		}
		node, err := fixture.catalog.CreateFile(b.Context(), fixture.catalog.RootID(), name, written.Hash, written.Size, "application/pdf", processingBlobPhysical(b, written))
		require.NoError(b, err)
		selector := Selector{Profile: setup.Selected, NodeID: node.ID, ContentVersionID: node.CurrentVersionID}
		plan, err := setup.Service.Plan(b.Context(), selector)
		require.NoError(b, err)
		job, err := setup.Service.Start(b.Context(), StartRequest{Selector: selector, PlanFingerprint: plan.Fingerprint,
			runOptions: processingRunOptions{deferIndex: true}})
		require.NoError(b, err)
		require.Len(b, job.EmbeddingJobIDs, 1)
		status, err := fixture.catalog.EmbeddingJobByID(b.Context(), job.EmbeddingJobIDs[0])
		require.NoError(b, err)
		require.Equal(b, "completed", status.State)
	}
	b.ResetTimer()
	for b.Loop() {
		targets, err := setup.targets(b.Context(), "", 50)
		require.NoError(b, err)
		require.Empty(b, targets)
	}
}
