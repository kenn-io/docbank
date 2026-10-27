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

	sets         []store.EmbeddingSetRecord
	workerConfig EmbeddingWorkerConfig
	request      store.EmbeddingJobRequest
	profile      document.ProcessingProfileV1
	versions     []string
	legacySpace  string
	provider     *recoveryTextProvider
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
	fixture, _, _, request, workerConfig := newRealEmbeddingWorkerWithDescriptor(t, kind, &descriptor)
	var portable document.ProcessingProfileV1
	require.NoError(t, json.Unmarshal(request.Profile.CanonicalProfile, &portable))
	_, fp, err := document.CanonicalProfile(portable)
	require.NoError(t, err)
	result := embeddingRecoveryFixture{publicationFixture: fixture, workerConfig: workerConfig, request: request, profile: portable, legacySpace: fp.VectorSpace[request.BindingID], provider: initial}
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

func TestEmbeddingBuildReusesRetainedPayload(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"missing first legacy payload", "restored legacy space"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newEmbeddingRecoveryFixture(t)
			provider := newRecoveryProvider(t, f.request.Descriptor)
			service := f.service(t, provider.Descriptor(), provider)
			versionID, uploads := f.versions[0], provider.documents
			switch name {
			case "missing first legacy payload":
				first := f.sets[0]
				work := EmbeddingWork{VaultID: f.catalog.VaultID(), ContentVersionID: first.ContentVersionID, ProcessingProfile: f.request.Profile, Binding: f.profile.Embeddings[0], Descriptor: f.request.Descriptor, VectorSpaceID: f.legacySpace, EmbeddingInputFingerprint: first.EmbeddingInputFingerprint, InputGeneration: first.InputGeneration}
				vectors := document.EmbeddingResult{}
				for _, input := range first.InputGeneration.Inputs {
					vectors.Vectors = append(vectors.Vectors, document.EmbeddingVector{Key: input.ID, Values: []float32{3, 4}})
				}
				later, payload, err := buildEmbeddingSet(work, vectors, time.Now().UTC())
				require.NoError(t, err)
				receipt, err := f.blobs.WriteDetailedContext(t.Context(), bytes.NewReader(payload))
				require.NoError(t, err)
				require.NoError(t, f.catalog.RecordRenditionBlob(t.Context(), receipt.Hash, receipt.Size, processingBlobPhysical(t, receipt)))
				require.NoError(t, f.catalog.StageEmbeddingSet(t.Context(), later))
				require.NotEqual(t, first.ID, later.ID)
				if later.ID < first.ID {
					first = later
				}
				require.NoError(t, f.blobs.Remove(first.VectorSet.PayloadBlobHash))
			case "restored legacy space":
				receipt, err := f.blobs.WriteDetailedContext(t.Context(), bytes.NewReader(append(mediatest.PDF(), []byte("\n% selected after endpoint move")...)))
				require.NoError(t, err)
				node, err := f.catalog.CreateFile(t.Context(), f.catalog.RootID(), "new-document.pdf", receipt.Hash, receipt.Size, "application/pdf", processingBlobPhysical(t, receipt))
				require.NoError(t, err)
				selector := Selector{Profile: "current", NodeID: node.ID, ContentVersionID: node.CurrentVersionID}
				plan, err := service.Plan(t.Context(), selector)
				require.NoError(t, err)
				_, err = service.Start(t.Context(), StartRequest{Selector: selector, PlanFingerprint: plan.Fingerprint, Consent: true})
				require.NoError(t, err)
				service = f.service(t, f.request.Descriptor, f.provider)
				require.Equal(t, f.legacySpace, service.profiles["current"].vectorSpaces[f.request.BindingID])
				versionID, uploads = node.CurrentVersionID, f.provider.documents
			}
			version, err := f.catalog.ContentVersionByID(t.Context(), versionID)
			require.NoError(t, err)
			selector := Selector{Profile: "current", NodeID: version.NodeID, ContentVersionID: version.ID}
			plan, err := service.Plan(t.Context(), selector)
			require.NoError(t, err)
			job, err := service.Start(t.Context(), StartRequest{Selector: selector, PlanFingerprint: plan.Fingerprint, Consent: true})
			require.NoError(t, err)
			require.Len(t, job.EmbeddingJobIDs, 1)
			status, err := f.catalog.EmbeddingJobByID(t.Context(), job.EmbeddingJobIDs[0])
			require.NoError(t, err)
			require.Equal(t, "completed", status.State)
			require.Zero(t, uploads.Load(), "compatible retained payloads must prevent document upload")
		})
	}
}

func TestEmbeddingBuildEmbedsWhenRetainedPayloadIsUnavailable(t *testing.T) {
	t.Parallel()
	f := newEmbeddingRecoveryFixture(t)
	require.NoError(t, f.blobs.Remove(f.sets[0].VectorSet.PayloadBlobHash))
	provider := newRecoveryProvider(t, f.request.Descriptor)
	service := f.service(t, provider.Descriptor(), provider)
	version, err := f.catalog.ContentVersionByID(t.Context(), f.versions[0])
	require.NoError(t, err)
	selector := Selector{Profile: "current", NodeID: version.NodeID, ContentVersionID: version.ID}
	plan, err := service.Plan(t.Context(), selector)
	require.NoError(t, err)
	job, err := service.Start(t.Context(), StartRequest{Selector: selector, PlanFingerprint: plan.Fingerprint, Consent: true})
	require.NoError(t, err)
	require.Len(t, job.EmbeddingJobIDs, 1)
	status, err := f.catalog.EmbeddingJobByID(t.Context(), job.EmbeddingJobIDs[0])
	require.NoError(t, err)
	require.Equal(t, "completed", status.State)
	require.Positive(t, provider.documents.Load(), "a selected build without usable retained vectors must use the consented provider")
}

func TestEmbeddingConsentForOneDocumentDoesNotSelectAnother(t *testing.T) {
	t.Parallel()
	f := newEmbeddingRecoveryFixture(t)
	require.NoError(t, f.blobs.Remove(f.sets[0].VectorSet.PayloadBlobHash))
	provider := newRecoveryProvider(t, f.request.Descriptor)
	service := f.service(t, provider.Descriptor(), provider)
	other, err := f.catalog.ContentVersionByID(t.Context(), f.versions[1])
	require.NoError(t, err)
	selector := Selector{Profile: "current", NodeID: other.NodeID, ContentVersionID: other.ID}
	plan, err := service.Plan(t.Context(), selector)
	require.NoError(t, err)
	_, err = service.Start(t.Context(), StartRequest{Selector: selector, PlanFingerprint: plan.Fingerprint, Consent: true})
	require.NoError(t, err)
	config := f.workerConfig
	config.Runtime = service.embeddings
	config.DescriptorFingerprints = []string{provider.Descriptor().Fingerprint}
	worker, err := NewEmbeddingWorker(config)
	require.NoError(t, err)
	_, err = worker.ScanOnce(t.Context())
	require.NoError(t, err)
	jobs, err := f.catalog.EmbeddingJobsForVersionProfile(t.Context(), f.versions[0], service.profiles["current"].record.Fingerprint)
	require.NoError(t, err)
	require.Empty(t, jobs, "consent for one document must not enroll another document under the new profile")
	require.Zero(t, provider.documents.Load(), "the unselected document must not reach the new endpoint")
}

func TestEmbeddingSelectedSpaceRecoversAbandonedJob(t *testing.T) {
	t.Parallel()
	f := newEmbeddingRecoveryFixture(t)
	for _, name := range []string{"new semantic space", "existing semantic space after endpoint change"} {
		t.Run(name, func(t *testing.T) {
			provider := newRecoveryProvider(t, f.request.Descriptor)
			service := f.service(t, provider.Descriptor(), provider)
			f.grant(t, service, false)
			target := service.profiles["current"]
			version, err := f.catalog.ContentVersionByID(t.Context(), f.versions[0])
			require.NoError(t, err)
			node, err := f.catalog.NodeByID(t.Context(), version.NodeID)
			require.NoError(t, err)
			rendition, waiter, err := service.enqueueRendition(t.Context(), node, version, target, service.renditionConsentRequest(target), "", false)
			require.NoError(t, err)
			_, err = service.runRenditionJob(t.Context(), rendition.ID, waiter.ID)
			require.NoError(t, err)
			binding := target.portable.Embeddings[0]
			generation, err := service.chunkEmbeddingGeneration(t.Context(), version, target, binding)
			require.NoError(t, err)
			request := f.request
			request.ContentVersionID, request.Profile, request.Descriptor = version.ID, target.record, provider.Descriptor()
			request.VectorSpaceID, request.InputGeneration = target.vectorSpaces[binding.Name], generation
			request.Authorization.ProfileFingerprint, request.Authorization.DisclosureFingerprint = target.record.Fingerprint, binding.DisclosureFingerprint
			job, err := f.catalog.EnqueueEmbeddingJob(t.Context(), request)
			require.NoError(t, err)
			claim, _, found, err := f.catalog.ClaimEmbeddingWork(t.Context(), job.ID, "interrupted-selection", time.Now(), time.Minute, []string{provider.Descriptor().Fingerprint})
			require.NoError(t, err)
			require.True(t, found)
			require.NoError(t, f.catalog.AbandonEmbeddingWork(t.Context(), claim, time.Now()))
			config := f.workerConfig
			config.Runtime, config.DescriptorFingerprints = service.embeddings, []string{provider.Descriptor().Fingerprint}
			worker, err := NewEmbeddingWorker(config)
			require.NoError(t, err)
			processed, err := worker.ScanOnce(t.Context())
			require.NoError(t, err)
			require.Equal(t, 1, processed)
			status, err := f.catalog.EmbeddingJobByID(t.Context(), job.ID)
			require.NoError(t, err)
			require.Equal(t, "completed", status.State)
		})
	}
}

func TestEmbeddingUpgradeRetainsLocalSimilarityAfterConsentRevocation(t *testing.T) {
	t.Parallel()
	f := newEmbeddingRecoveryFixture(t)
	provider := newRecoveryProvider(t, f.request.Descriptor)
	service := f.service(t, provider.Descriptor(), provider)
	space, err := provider.VectorIdentity()
	require.NoError(t, err)
	require.Equal(t, space, service.profiles["current"].vectorSpaces[f.request.BindingID])
	for _, id := range f.versions {
		version, err := f.catalog.ContentVersionByID(t.Context(), id)
		require.NoError(t, err)
		selector := Selector{Profile: "current", NodeID: version.NodeID, ContentVersionID: version.ID}
		plan, err := service.Plan(t.Context(), selector)
		require.NoError(t, err)
		_, err = service.Start(t.Context(), StartRequest{Selector: selector, PlanFingerprint: plan.Fingerprint, Consent: true})
		require.NoError(t, err)
	}
	version, err := f.catalog.ContentVersionByID(t.Context(), f.versions[0])
	require.NoError(t, err)
	search, err := service.Search(t.Context(), SearchRequest{Profile: "current", Fence: SourceFence{VaultUID: f.catalog.VaultID(), ContentVersionIDs: []string{version.ID}}, Query: "synthetic", Mode: "semantic", Limit: 1})
	require.NoError(t, err)
	require.Len(t, search.Results, 1)
	require.Equal(t, version.ID, search.Results[0].Document.ContentVersionID)
	_, err = f.catalog.RevokeConsent(t.Context(), store.ProcessingConsentRevocationRequest{Principal: f.request.Authorization.Principal, Scope: f.request.Authorization.Scope})
	require.NoError(t, err)
	var before, after bytes.Buffer
	require.NoError(t, f.catalog.ExportMetadata(t.Context(), &before))
	service = f.service(t, provider.Descriptor(), provider)
	require.Equal(t, space, service.profiles["current"].vectorSpaces[f.request.BindingID])
	fence := SourceFence{VaultUID: f.catalog.VaultID(), ContentVersionIDs: f.versions}
	report, err := service.FindSimilar(t.Context(), SimilarRequest{Selector: Selector{Profile: "current", NodeID: version.NodeID, ContentVersionID: version.ID}, Fence: fence})
	require.NoError(t, err)
	require.Len(t, report.Results, 1)
	coverage, err := service.Coverage(t.Context(), "current", fence)
	require.NoError(t, err)
	require.Equal(t, 2, coverage.Embeddings[0].Complete)
	require.NoError(t, f.catalog.ExportMetadata(t.Context(), &after))
	require.Equal(t, before.String(), after.String())
}

func TestEmbeddingSemanticSearchAfterSecondEndpointMove(t *testing.T) {
	t.Parallel()
	f := newEmbeddingRecoveryFixture(t)
	first := newRecoveryProvider(t, f.request.Descriptor)
	service := f.service(t, first.Descriptor(), first)
	for _, id := range f.versions {
		version, err := f.catalog.ContentVersionByID(t.Context(), id)
		require.NoError(t, err)
		selector := Selector{Profile: "current", NodeID: version.NodeID, ContentVersionID: version.ID}
		plan, err := service.Plan(t.Context(), selector)
		require.NoError(t, err)
		_, err = service.Start(t.Context(), StartRequest{Selector: selector, PlanFingerprint: plan.Fingerprint, Consent: true})
		require.NoError(t, err)
	}
	space, err := first.VectorIdentity()
	require.NoError(t, err)
	second := newRecoveryProvider(t, f.request.Descriptor)
	require.NotEqual(t, first.Descriptor().Fingerprint, second.Descriptor().Fingerprint)
	service = f.service(t, second.Descriptor(), second)
	require.Equal(t, space, service.profiles["current"].vectorSpaces[f.request.BindingID])
	version, err := f.catalog.ContentVersionByID(t.Context(), f.versions[0])
	require.NoError(t, err)
	selector := Selector{Profile: "current", NodeID: version.NodeID, ContentVersionID: version.ID}
	plan, err := service.Plan(t.Context(), selector)
	require.NoError(t, err)
	_, err = service.Start(t.Context(), StartRequest{Selector: selector, PlanFingerprint: plan.Fingerprint, Consent: true})
	require.NoError(t, err)
	f.grant(t, service, true)
	search, err := service.Search(t.Context(), SearchRequest{Profile: "current", Fence: SourceFence{VaultUID: f.catalog.VaultID(), ContentVersionIDs: []string{version.ID}}, Query: "synthetic", Mode: "semantic", Limit: 1})
	require.NoError(t, err)
	require.Len(t, search.Results, 1)
	require.Equal(t, version.ID, search.Results[0].Document.ContentVersionID)
	require.Zero(t, second.documents.Load(), "building and searching a retained space must not re-embed its documents")
}

type rejectedEvidenceBlobs struct {
	embeddingRuntimeBlobs
	evidence string
}

func (b rejectedEvidenceBlobs) OpenContext(ctx context.Context, hash string) (io.ReadSeekCloser, error) {
	if hash == b.evidence {
		return &embeddingRuntimeReadSeekCloser{Reader: bytes.NewReader([]byte("synthetic evidence that does not match its fingerprint"))}, nil
	}
	return b.embeddingRuntimeBlobs.OpenContext(ctx, hash)
}

func TestEmbeddingReuseRejectsInvalidRetainedEvidence(t *testing.T) {
	t.Parallel()
	f := newEmbeddingRecoveryFixture(t)
	provider := newRecoveryProvider(t, f.request.Descriptor)
	service := f.service(t, provider.Descriptor(), provider)
	f.grant(t, service, false)
	target := service.profiles["current"]
	version, err := f.catalog.ContentVersionByID(t.Context(), f.versions[0])
	require.NoError(t, err)
	node, err := f.catalog.NodeByID(t.Context(), version.NodeID)
	require.NoError(t, err)
	rendition, waiter, err := service.enqueueRendition(t.Context(), node, version, target, service.renditionConsentRequest(target), "", false)
	require.NoError(t, err)
	_, err = service.runRenditionJob(t.Context(), rendition.ID, waiter.ID)
	require.NoError(t, err)
	binding := target.portable.Embeddings[0]
	generation, err := service.chunkEmbeddingGeneration(t.Context(), version, target, binding)
	require.NoError(t, err)
	request := f.request
	request.ContentVersionID, request.Profile, request.Descriptor = version.ID, target.record, provider.Descriptor()
	request.VectorSpaceID, request.InputGeneration = target.vectorSpaces[binding.Name], generation
	request.Authorization.ProfileFingerprint, request.Authorization.DisclosureFingerprint = target.record.Fingerprint, binding.DisclosureFingerprint
	job, err := f.catalog.EnqueueEmbeddingJob(t.Context(), request)
	require.NoError(t, err)
	config := f.workerConfig
	config.Runtime, config.DescriptorFingerprints = service.embeddings, []string{provider.Descriptor().Fingerprint}
	config.GenerationBlobs = rejectedEvidenceBlobs{embeddingRuntimeBlobs: f.blobs, evidence: generation.EvidenceFingerprint}
	worker, err := NewEmbeddingWorker(config)
	require.NoError(t, err)
	_, err = worker.ScanOnce(t.Context())
	require.NoError(t, err)
	status, err := f.catalog.EmbeddingJobByID(t.Context(), job.ID)
	require.NoError(t, err)
	require.Equal(t, "failed", status.State)
	require.Equal(t, store.EmbeddingFailureInputRejected, status.FailureCode, "invalid retained evidence must fail like the provider path instead of retrying")
	require.Zero(t, provider.documents.Load())
}
