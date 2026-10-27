package processing

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/media/mediatest"
	"go.kenn.io/docbank/document/openaicompat"
	"go.kenn.io/docbank/internal/store"
	"go.kenn.io/docbank/internal/vectorworker"
)

type recoveryEmbeddingProvider interface {
	document.EmbeddingProvider
	document.EmbeddingVectorIdentityProvider
}

type recoveryTextProvider struct {
	recoveryEmbeddingProvider

	endpoint  string
	documents *atomic.Int64
}

func newRecoveryProvider(t *testing.T, descriptor document.EmbeddingDescriptor) *recoveryTextProvider {
	t.Helper()
	var documents atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/v1/embeddings", r.URL.Path)
		var input struct {
			Input []string `json:"input"`
		}
		if !assert.NoError(t, json.UnmarshalRead(r.Body, &input)) {
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		data := make([]map[string]any, len(input.Input))
		for i, text := range input.Input {
			if !strings.HasPrefix(text, "search_query:") {
				documents.Add(1)
			}
			data[i] = map[string]any{"object": "embedding", "embedding": []float32{1, 2}, "index": i}
		}
		w.Header().Set("Content-Type", "application/json")
		assert.NoError(t, json.MarshalWrite(w, map[string]any{"object": "list", "model": descriptor.Model, "data": data}))
	}))
	t.Cleanup(server.Close)
	profile := openaicompat.Profile{Origin: server.URL, Descriptor: descriptor, ModelInput: descriptor.ModelInput, DeploymentEpoch: descriptor.ModelRevision, RequestTimeout: time.Second, MaxBatchItems: 8, MaxInputBytes: 1 << 20, MaxRequestBytes: 2 << 20, MaxResponseBytes: 1 << 20}
	var err error
	profile.Descriptor.PolicyFingerprint, err = openaicompat.PolicyFingerprint(profile)
	require.NoError(t, err)
	profile.Descriptor, err = document.NewEmbeddingDescriptor(profile.Descriptor)
	require.NoError(t, err)
	provider, err := openaicompat.New(profile, nil, server.Client())
	require.NoError(t, err)
	return &recoveryTextProvider{recoveryEmbeddingProvider: provider, endpoint: server.URL, documents: &documents}
}

type embeddingRecoveryFixture struct {
	publicationFixture

	sets        []store.EmbeddingSetRecord
	worker      *EmbeddingWorker
	request     store.EmbeddingJobRequest
	profile     document.ProcessingProfileV1
	versions    []string
	legacySpace string
	provider    document.EmbeddingProvider
}

func newEmbeddingRecoveryFixture(t *testing.T) embeddingRecoveryFixture {
	t.Helper()
	kind := document.EmbeddingInputRenditionChunk
	descriptor := embeddingWorkerDescriptor(t)
	input := document.ModelInputContractConfig{Profile: document.ModelInputProfileNomic}
	contract, err := document.NewModelInputContract(input)
	require.NoError(t, err)
	descriptor.ModelInput, descriptor.CompatibilityID = contract, contract.CompatibilityID
	descriptor.ID, descriptor.TrustBoundary = openaicompat.ProviderID, document.EmbeddingTrustOperatorNetwork
	descriptor.DocumentFormatter, descriptor.QueryFormatter = openaicompat.DocumentFormatterV1, openaicompat.QueryFormatterV1
	descriptor.InputKinds = []document.EmbeddingInputKind{kind}

	initial := newRecoveryProvider(t, descriptor)
	descriptor = initial.Descriptor()
	fixture, _, worker, request := newRealEmbeddingWorkerWithDescriptor(t, kind, &descriptor)
	var portable document.ProcessingProfileV1
	require.NoError(t, json.Unmarshal(request.Profile.CanonicalProfile, &portable))
	_, fp, err := document.CanonicalProfile(portable)
	require.NoError(t, err)
	result := embeddingRecoveryFixture{publicationFixture: fixture, worker: worker, request: request, profile: portable, legacySpace: fp.VectorSpace[request.BindingID], provider: initial}
	service := result.service(t, descriptor, initial)
	for index := range 2 {
		request := request
		data := append(mediatest.PDF(), []byte(fmt.Sprintf("\n%% synthetic document %d", index))...)
		receipt, err := fixture.blobs.WriteDetailedContext(t.Context(), bytes.NewReader(data))
		require.NoError(t, err)
		node, err := fixture.catalog.CreateFile(t.Context(), fixture.catalog.RootID(), fmt.Sprintf("document-%d.pdf", index), receipt.Hash, receipt.Size, "application/pdf", processingBlobPhysical(t, receipt))
		require.NoError(t, err)
		request.ContentVersionID = node.CurrentVersionID
		result.grant(t, service, false)
		job, waiter, err := service.enqueueRendition(t.Context(), node, store.ContentVersion{ID: node.CurrentVersionID, NodeID: node.ID, BlobHash: receipt.Hash, Size: receipt.Size, MimeType: "application/pdf"}, service.profiles["current"], service.renditionConsentRequest(service.profiles["current"]), "", false)
		require.NoError(t, err)
		_, err = service.runRenditionJob(t.Context(), job.ID, waiter.ID)
		require.NoError(t, err)
		version, err := fixture.catalog.ContentVersionByID(t.Context(), node.CurrentVersionID)
		require.NoError(t, err)
		request.InputGeneration, err = service.chunkEmbeddingGeneration(t.Context(), version, service.profiles["current"], portable.Embeddings[0])
		require.NoError(t, err)

		embeddingJob, err := fixture.catalog.EnqueueEmbeddingJob(t.Context(), request)
		require.NoError(t, err)
		claim, claimedWork, found, err := fixture.catalog.ClaimEmbeddingWork(t.Context(), embeddingJob.ID, "legacy-fixture", time.Now(), time.Minute, []string{request.Descriptor.Fingerprint})
		require.NoError(t, err)
		require.True(t, found)
		result.versions = append(result.versions, request.ContentVersionID)
		work := EmbeddingWork{VaultID: fixture.catalog.VaultID(), ContentVersionID: request.ContentVersionID, ProcessingProfile: request.Profile, Binding: portable.Embeddings[0], Descriptor: request.Descriptor, VectorSpaceID: result.legacySpace, EmbeddingInputFingerprint: fp.EmbeddingInput[request.BindingID], InputGeneration: request.InputGeneration}
		vectors := document.EmbeddingResult{}
		for _, input := range request.InputGeneration.Inputs {
			vectors.Vectors = append(vectors.Vectors, document.EmbeddingVector{Key: input.ID, Values: []float32{1, 2}})
		}
		record, payload, err := buildEmbeddingSet(work, vectors, time.Now().UTC())
		require.NoError(t, err)
		receipt, err = fixture.blobs.WriteDetailedContext(t.Context(), bytes.NewReader(payload))
		require.NoError(t, err)
		require.NoError(t, fixture.catalog.RecordRenditionBlob(t.Context(), receipt.Hash, receipt.Size, processingBlobPhysical(t, receipt)))
		require.NoError(t, fixture.catalog.StageEmbeddingSetWithLease(t.Context(), record, claim.AttemptID, claim.Epoch, time.Now()))
		result.sets = append(result.sets, record)
		prior, err := fixture.catalog.AuthorizeProviderOperation(t.Context(), request.Authorization)
		require.NoError(t, err)
		head := store.EmbeddingHeadRecord{Key: store.EmbeddingHeadKey{ContentVersionID: request.ContentVersionID, BindingID: request.BindingID, InputKind: kind}, SetID: record.ID, VectorSpaceID: result.legacySpace, ProcessingProfileFingerprint: request.Profile.Fingerprint, PublishedAt: metadataEmbeddingTime(time.Now()), FencingToken: claim.Epoch}
		require.NoError(t, fixture.catalog.PublishEmbeddingWork(t.Context(), claim, claimedWork, head, prior, store.EmbeddingAttemptReceipt{AttemptID: embeddingJob.ID}, time.Now()))
	}
	return result
}

func (f embeddingRecoveryFixture) service(t *testing.T, descriptor document.EmbeddingDescriptor, provider document.EmbeddingProvider) *Service {
	t.Helper()
	portable := f.profile
	portable.Embeddings = append([]document.EmbeddingBindingV1(nil), portable.Embeddings...)
	portable.Embeddings[0].Descriptor.Fingerprint = descriptor.Fingerprint
	config := ProfileConfig{Profile: portable, EmbeddingProviders: map[string]document.EmbeddingProvider{f.request.BindingID: provider}, Tokenizers: map[string]document.Tokenizer{f.request.BindingID: embeddingIntegrationTokenizer{}}}
	text, ok := provider.(*recoveryTextProvider)
	require.True(t, ok)
	config.EmbeddingDisclosures = map[string]RuntimeDisclosure{f.request.BindingID: {Endpoint: text.endpoint}}
	rendition := newWorkerProvider(t)
	rendition.textForSource = true
	config.RenditionProvider = rendition
	service, err := NewService(ServiceConfig{Catalog: f.catalog, Blobs: f.blobs, Gate: newTestOperationGate(), SpoolDirectory: t.TempDir(), Lifecycle: t.Context(), Principal: f.request.Authorization.Principal, Scope: f.request.Authorization.Scope, Profiles: map[string]ProfileConfig{"current": config}})
	require.NoError(t, err)
	return service
}

func (f embeddingRecoveryFixture) grant(t *testing.T, service *Service, query bool) {
	t.Helper()
	target := service.profiles["current"]
	for _, authorization := range service.profileConsentRequests(target) {
		if !query && authorization.InputClasses[0] == "query_text" {
			continue
		}
		_, err := f.catalog.GrantConsent(t.Context(), store.ProcessingConsentGrantRequest{Principal: service.principal, Scope: service.scope, ProfileFingerprint: target.record.Fingerprint, DisclosureFingerprint: authorization.DisclosureFingerprint, InputClasses: authorization.InputClasses, RetainedArtifactClasses: authorization.RetainedArtifactClasses})
		require.NoError(t, err)
	}
}

func (f embeddingRecoveryFixture) rebuild(t *testing.T, space string) {
	t.Helper()
	index, err := vectorworker.NewIndexWorker(vectorworker.IndexWorkerConfig{Catalog: f.catalog, Mutate: newTestOperationGate().MutateContext, Owner: "embedding-recovery-index", BuildLease: time.Minute, ReaderLease: time.Minute, IdleDelay: time.Millisecond, ReadVectorSet: func(ctx context.Context, member store.VectorIndexMember) ([]byte, error) {
		return f.catalog.ReadVectorIndexVectorSet(ctx, f.blobs, member)
	}})
	require.NoError(t, err)
	_, err = index.Rebuild(t.Context(), space)
	require.NoError(t, err)
}

func TestEmbeddingRestartCarriesCurrentProfile(t *testing.T) {
	t.Parallel()
	f := newEmbeddingRecoveryFixture(t)
	provider := newRecoveryProvider(t, f.request.Descriptor)
	descriptor := provider.Descriptor()
	service := f.service(t, descriptor, provider)
	target := service.profiles["current"]
	f.worker.runtime = service.embeddings
	f.worker.descriptorFingerprints = []string{descriptor.Fingerprint}
	f.worker.reconciliation = service.EmbeddingReconciliation()
	processed, err := f.worker.ScanOnce(t.Context())
	require.NoError(t, err)
	require.Zero(t, processed, "changed profile requires fresh consent")
	_, err = f.catalog.ActiveRendition(t.Context(), f.versions[0], target.record.Fingerprint)
	require.ErrorIs(t, err, store.ErrNotFound)
	f.grant(t, service, true)
	f.worker.reconcileAt = time.Time{}
	processed, err = f.worker.ScanOnce(t.Context())
	require.NoError(t, err)
	require.Equal(t, 2, processed)
	require.Zero(t, provider.documents.Load())
	canonical := target.vectorSpaces[f.request.BindingID]
	require.Equal(t, canonical, f.service(t, f.request.Descriptor, f.provider).profiles["current"].vectorSpaces[f.request.BindingID])
}

func TestEmbeddingUpgradeRetainsLocalSimilarityAfterConsentRevocation(t *testing.T) {
	t.Parallel()
	f := newEmbeddingRecoveryFixture(t)
	f.rebuild(t, f.legacySpace)
	_, err := f.catalog.RevokeConsent(t.Context(), store.ProcessingConsentRevocationRequest{Principal: f.request.Authorization.Principal, Scope: f.request.Authorization.Scope})
	require.NoError(t, err)
	var before, after bytes.Buffer
	require.NoError(t, f.catalog.ExportMetadata(t.Context(), &before))
	service := f.service(t, f.request.Descriptor, f.provider)
	require.Equal(t, f.legacySpace, service.profiles["current"].vectorSpaces[f.request.BindingID])
	fence := SourceFence{VaultUID: f.catalog.VaultID(), ContentVersionIDs: f.versions}
	version, err := f.catalog.ContentVersionByID(t.Context(), f.versions[0])
	require.NoError(t, err)
	report, err := service.FindSimilar(t.Context(), SimilarRequest{Selector: Selector{Profile: "current", NodeID: version.NodeID, ContentVersionID: version.ID}, Fence: fence})
	require.NoError(t, err)
	require.Len(t, report.Results, 1)
	coverage, err := service.Coverage(t.Context(), "current", fence)
	require.NoError(t, err)
	require.Equal(t, 2, coverage.Embeddings[0].Complete)
	require.NoError(t, f.catalog.ExportMetadata(t.Context(), &after))
	require.Equal(t, before.String(), after.String())
}

func TestEmbeddingRecoveryEmbedsCorruptPayload(t *testing.T) {
	t.Parallel()
	f := newEmbeddingRecoveryFixture(t)
	f.worker.generationBlobs = corruptRetainedVectors{embeddingRuntimeBlobs: f.blobs, sets: f.sets}
	provider := newRecoveryProvider(t, f.request.Descriptor)
	service := f.service(t, provider.Descriptor(), provider)
	f.grant(t, service, false)
	f.worker.runtime = service.embeddings
	f.worker.descriptorFingerprints = []string{provider.Descriptor().Fingerprint}
	f.worker.reconciliation = service.EmbeddingReconciliation()
	processed, err := f.worker.ScanOnce(t.Context())
	require.NoError(t, err)
	require.Equal(t, 2, processed)
	require.Equal(t, int64(2), provider.documents.Load())
}

func TestEmbeddingSynchronousStartRebuildsCurrentVectorSpace(t *testing.T) {
	t.Parallel()
	f := newEmbeddingRecoveryFixture(t)
	provider := newRecoveryProvider(t, f.request.Descriptor)
	service := f.service(t, provider.Descriptor(), provider)
	version, err := f.catalog.ContentVersionByID(t.Context(), f.versions[0])
	require.NoError(t, err)
	selector := Selector{Profile: "current", NodeID: version.NodeID, ContentVersionID: version.ID}
	plan, err := service.Plan(t.Context(), selector)
	require.NoError(t, err)
	_, err = service.Start(t.Context(), StartRequest{Selector: selector, PlanFingerprint: plan.Fingerprint, Consent: true})
	require.NoError(t, err)
	require.Zero(t, provider.documents.Load(), "saved vectors should survive an endpoint change")
	report, err := service.Search(t.Context(), SearchRequest{Profile: "current", Fence: SourceFence{VaultUID: f.catalog.VaultID(), ContentVersionIDs: []string{version.ID}}, Query: "synthetic", Mode: "semantic", Limit: 1})
	require.NoError(t, err)
	require.Len(t, report.Results, 1)
	require.Equal(t, version.ID, report.Results[0].Document.ContentVersionID)
}

type corruptRetainedVectors struct {
	embeddingRuntimeBlobs

	sets []store.EmbeddingSetRecord
}

func (blobs corruptRetainedVectors) OpenContext(ctx context.Context, hash string) (io.ReadSeekCloser, error) {
	for _, set := range blobs.sets {
		if set.VectorSet.PayloadBlobHash == hash {
			return &embeddingRuntimeReadSeekCloser{Reader: bytes.NewReader([]byte("corrupt"))}, nil
		}
	}
	return blobs.embeddingRuntimeBlobs.OpenContext(ctx, hash)
}

func TestEmbeddingLegacyProfilesReuseOnlyMatchingProducer(t *testing.T) {
	t.Parallel()
	f := newEmbeddingRecoveryFixture(t)
	provider := newRecoveryProvider(t, f.request.Descriptor)
	retained := f.sets[0]
	work := store.EmbeddingJobWork{ContentVersionID: retained.ContentVersionID, Binding: f.profile.Embeddings[0], Descriptor: provider.Descriptor(), VectorSpaceID: workerHash("new-space"), EmbeddingInputFingerprint: retained.EmbeddingInputFingerprint, InputGeneration: retained.InputGeneration}
	found, err := f.catalog.FindReusableEmbeddingSet(t.Context(), work)
	require.NoError(t, err)
	require.Equal(t, retained.ID, found.ID)
	work.Descriptor.ModelRevision = "other-revision"
	work.Descriptor, err = document.NewEmbeddingDescriptor(work.Descriptor)
	require.NoError(t, err)
	_, err = f.catalog.FindReusableEmbeddingSet(t.Context(), work)
	require.ErrorIs(t, err, store.ErrNotFound)
}
