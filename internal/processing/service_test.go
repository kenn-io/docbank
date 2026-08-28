package processing

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/store"
)

func TestProcessingServiceSourceFenceIsBoundedCanonicalAuthority(t *testing.T) {
	ids := []string{"00000000-0000-4000-8000-000000000002", "00000000-0000-4000-8000-000000000001"}
	normalized, err := normalizeFenceIDs(ids)
	require.NoError(t, err)
	require.Equal(t, []string{"00000000-0000-4000-8000-000000000001", "00000000-0000-4000-8000-000000000002"}, normalized)
	require.Equal(t, []string{"00000000-0000-4000-8000-000000000002", "00000000-0000-4000-8000-000000000001"}, ids)

	_, err = normalizeFenceIDs(nil)
	require.ErrorContains(t, err, "between 1")
	_, err = normalizeFenceIDs([]string{ids[0], ids[0]})
	require.ErrorContains(t, err, "duplicate")
	_, err = normalizeFenceIDs(make([]string, MaxSourceFenceIDs+1))
	require.ErrorContains(t, err, strconv.Itoa(MaxSourceFenceIDs))
}

func TestProcessingServicePlanFingerprintSealsDisclosure(t *testing.T) {
	plan := Plan{VaultUID: "00000000-0000-4000-8000-000000000001",
		Selector:           Selector{NodeID: 1, ContentVersionID: "00000000-0000-4000-8000-000000000002", Profile: "private"},
		ProfileFingerprint: frontmatterHashForService("profile"),
		Flow: []FlowHop{{Capability: "rendition", ProviderID: "local", TrustBoundary: "local_process",
			InputClasses: []string{"original_file"}}}, RetainedClasses: []string{"sanitized_markdown"},
		ConsentRequired: true}
	first, err := planFingerprint(plan)
	require.NoError(t, err)
	second, err := planFingerprint(plan)
	require.NoError(t, err)
	require.Equal(t, first, second)
	plan.Flow[0].TrustBoundary = "hosted_provider"
	changed, err := planFingerprint(plan)
	require.NoError(t, err)
	require.NotEqual(t, first, changed)
}

func TestAggregateStatusNeverReportsUnfinishedEmbeddingsAsCompleted(t *testing.T) {
	embeddings := []store.EmbeddingJobStatus{{ID: "a", State: "completed"}, {ID: "b", State: "abandoned"}}
	status := aggregateStatus("a", nil, embeddings)
	require.Equal(t, "abandoned", status.State)
	require.Equal(t, 1, status.CompletedBindings)

	embeddings[1].State = "completed"
	require.Equal(t, "completed", aggregateStatus("a", nil, embeddings).State)

	embeddings[1].State = "unexpected"
	require.Equal(t, "unexpected", aggregateStatus("a", nil, embeddings).State)
}

func BenchmarkProcessingServiceSourceFence4096(b *testing.B) {
	ids := make([]string, MaxSourceFenceIDs)
	for index := range ids {
		ids[index] = fmt.Sprintf("00000000-0000-4000-8000-%012d", index)
	}
	b.ReportAllocs()
	for b.Loop() {
		if _, err := normalizeFenceIDs(ids); err != nil {
			b.Fatal(err)
		}
	}
}

func frontmatterHashForService(value string) string {
	return fmt.Sprintf("%064s", value)
}

func TestProcessingServiceWaitsForEmbeddingRetryAndHonorsCancellation(t *testing.T) {
	fixture, fake, _, request := newRealEmbeddingWorker(t, document.EmbeddingInputOriginalFile)
	fake.runtime.failures[request.BindingID] = []error{embeddingTransientError{}, embeddingTransientError{}, embeddingTransientError{}}
	provider := &embeddingWorkerProvider{runtime: fake.runtime, binding: request.BindingID, descriptor: request.Descriptor}
	runtime, err := NewProviderEmbeddingRuntime(provider, fixture.blobs, t.TempDir(), fake.runtime.Classify)
	require.NoError(t, err)
	var portable document.ProcessingProfileV1
	require.NoError(t, json.Unmarshal(request.Profile.CanonicalProfile, &portable))
	profile := configuredProfile{portable: portable, record: request.Profile,
		embedders:         map[string]document.EmbeddingProvider{request.BindingID: provider},
		embeddingRuntimes: map[string]*ProviderEmbeddingRuntime{request.BindingID: runtime}}
	var clockOffset atomic.Int64
	clockOffset.Store(int64(time.Second))
	service := &Service{catalog: fixture.catalog, blobs: fixture.blobs, gate: processingServiceTestGate{fake.gate},
		clock: func() time.Time { return time.Now().UTC().Add(time.Duration(clockOffset.Load())) }}
	version, err := fixture.catalog.ContentVersionByID(t.Context(), request.ContentVersionID)
	require.NoError(t, err)
	jobs, err := service.runEmbeddings(t.Context(), version, profile, request.Authorization.Principal, request.Authorization.Scope)
	require.NoError(t, err)
	require.Len(t, jobs, 1)
	status, err := fixture.catalog.EmbeddingJobByID(t.Context(), jobs[0])
	require.NoError(t, err)
	require.Equal(t, "retry_wait", status.State)
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	_, err = service.runEmbeddings(ctx, version, profile, request.Authorization.Principal, request.Authorization.Scope)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.Equal(t, 3, fake.runtime.callCount(request.BindingID), "waiting must not call the provider before backoff expires")
	clockOffset.Store(int64(2 * time.Minute))
	retried, err := service.runEmbeddings(t.Context(), version, profile, request.Authorization.Principal, request.Authorization.Scope)
	require.NoError(t, err)
	require.Equal(t, jobs, retried)
	status, err = fixture.catalog.EmbeddingJobByID(t.Context(), jobs[0])
	require.NoError(t, err)
	require.Equal(t, "completed", status.State)
}

func TestAggregateStatusUsesBindingActivation(t *testing.T) {
	for _, activation := range []document.EmbeddingActivation{document.EmbeddingRequired, document.EmbeddingOptional} {
		for _, state := range []string{"failed", "abandoned"} {
			t.Run(string(activation)+"/"+state, func(t *testing.T) {
				embeddings := []store.EmbeddingJobStatus{{ID: "a", State: "completed", Activation: document.EmbeddingRequired}, {ID: "b", State: state, Activation: activation}}
				status := aggregateStatus("a", nil, embeddings)
				want := state
				if activation == document.EmbeddingOptional {
					want = "partial"
				}
				require.Equal(t, want, status.State)
				require.Equal(t, 1, status.CompletedBindings)
				embeddings = append(embeddings, store.EmbeddingJobStatus{ID: "c", State: "failed", Activation: document.EmbeddingRequired})
				require.Equal(t, "failed", aggregateStatus("a", nil, embeddings).State, "required failure must take precedence")
			})
		}
	}
}

func TestProcessingServiceRejectsRevokedRenditionWaiter(t *testing.T) {
	fixture := newPublicationFixture(t)
	provider := newWorkerProvider(t)
	profile := workerProcessingProfile(t, provider.Descriptor())
	request := workerJobRequest(fixture.versionID, profile, provider.Descriptor())
	grantWorkerConsent(t, fixture.catalog, request)
	job, published, err := fixture.catalog.EnqueueRenditionJob(t.Context(), request)
	require.NoError(t, err)
	request.Authorization.Principal = "operator:revoked"
	grantWorkerConsent(t, fixture.catalog, request)
	_, rejected, err := fixture.catalog.EnqueueRenditionJob(t.Context(), request)
	require.NoError(t, err)
	_, err = fixture.catalog.RevokeConsent(t.Context(), store.ProcessingConsentRevocationRequest{
		Principal: request.Authorization.Principal, Scope: request.Authorization.Scope,
	})
	require.NoError(t, err)
	worker, err := NewRenditionWorker(RenditionWorkerConfig{
		Catalog: fixture.catalog, Blobs: fixture.blobs, Runtime: workerRuntime{provider: provider},
		Gate: newWorkerTestGate(), Owner: "rendition-waiter-test",
		LeaseDuration: time.Minute, IdleDelay: time.Millisecond,
	})
	require.NoError(t, err)
	processed, err := worker.RunJob(t.Context(), job.ID)
	require.NoError(t, err)
	require.True(t, processed)
	current, err := fixture.catalog.RenditionJobByID(t.Context(), job.ID)
	require.NoError(t, err)
	require.Equal(t, store.RenditionJobCompleted, current.State)
	service := &Service{catalog: fixture.catalog}
	result, err := service.renditionResult(t.Context(), published.ID)
	require.NoError(t, err)
	require.Equal(t, renditionRun{jobID: job.ID, waiterID: published.ID, attachmentID: published.AttachmentID}, result)
	_, err = service.renditionResult(t.Context(), rejected.ID)
	require.ErrorIs(t, err, ErrConsentRequired)
}

type processingServiceTestGate struct{ *workerTestGate }

func (gate processingServiceTestGate) PreserveContext(ctx context.Context, fn func() error) error {
	return gate.MutateContext(ctx, fn)
}
