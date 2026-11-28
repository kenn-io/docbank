package processing

import (
	"bytes"
	"compress/zlib"
	"context"
	"crypto/sha256"
	"encoding/json/v2"
	"errors"
	"fmt"
	"image/color"
	"math"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/media"
	"go.kenn.io/docbank/document/media/mediatest"
	"go.kenn.io/docbank/document/openaicompat"
	"go.kenn.io/docbank/internal/retrieval"
	"go.kenn.io/docbank/internal/store"
	"go.kenn.io/kit/embedclient"
	"go.kenn.io/kit/embedconfig"
	"go.kenn.io/kit/embedserver"
)

type kitMediaBackend struct {
	mu     sync.Mutex
	hashes map[[32]byte]string
	labels []string
	inputs []embedclient.Input
}

func (b *kitMediaBackend) Encode(_ context.Context, _ int, inputs []embedclient.Input) ([][]float32, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	vectors := make([][]float32, len(inputs))
	for i, input := range inputs {
		b.inputs = append(b.inputs, input)
		label := b.hashes[sha256.Sum256(input.Data)]
		if label == "" {
			for _, candidate := range b.labels {
				if strings.Contains(input.Text, candidate) {
					label = candidate
					break
				}
			}
		}
		index := slices.Index(b.labels, label)
		if index < 0 {
			return nil, errors.New("unrecognized synthetic input")
		}
		angle := float64(index) * math.Pi / 6
		vectors[i] = []float32{float32(math.Cos(angle)), float32(math.Sin(angle))}
	}
	return vectors, nil
}
func (b *kitMediaBackend) recorded() []embedclient.Input {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]embedclient.Input(nil), b.inputs...)
}

type kitMediaResolver struct{ provider document.EmbeddingProvider }

func (r kitMediaResolver) ResolveQueryEncoder(_ context.Context, _ document.EmbeddingDescriptor) (document.EmbeddingProvider, error) {
	return r.provider, nil
}

// PDF FlateDecode streams use zlib. The scan shares one image across its pages.
func kitFlateScan(t *testing.T, pages, width, height int) []byte {
	t.Helper()
	var compressed bytes.Buffer
	writer := zlib.NewWriter(&compressed)
	_, err := writer.Write(bytes.Repeat([]byte{255}, width*height*3))
	require.NoError(t, err)
	require.NoError(t, writer.Close())
	objects := []string{"<< /Type /Catalog /Pages 2 0 R >>", "", fmt.Sprintf("<< /Type /XObject /Subtype /Image /Width %d /Height %d /ColorSpace /DeviceRGB /BitsPerComponent 8 /Length %d /Filter /FlateDecode >>\nstream\n%s\nendstream", width, height, compressed.Len(), compressed.Bytes())}
	kids := make([]string, 0, pages)
	for range pages {
		page := len(objects) + 1
		kids = append(kids, fmt.Sprintf("%d 0 R", page))
		content := "q 100 0 0 100 0 0 cm /Im Do Q"
		objects = append(objects, fmt.Sprintf("<< /Type /Page /Parent 2 0 R /MediaBox [0 0 100 100] /Resources << /XObject << /Im 3 0 R >> >> /Contents %d 0 R >>", page+1), fmt.Sprintf("<< /Length %d >>\nstream\n%s\nendstream", len(content), content))
	}
	objects[1] = fmt.Sprintf("<< /Type /Pages /Kids [%s] /Count %d >>", strings.Join(kids, " "), pages)
	return mediatest.PDFObjects("synthetic", objects)
}

func TestKitMediaFallbackEligibility(t *testing.T) {
	t.Parallel()
	fixture := newPublicationFixture(t)
	descriptor := embeddingWorkerDescriptor(t)
	descriptor.ID = openaicompat.ProviderID
	binding := document.EmbeddingBindingV1{Name: "kit", InputKind: document.EmbeddingInputOriginalFile, MaxInputBytes: media.MaxBytes, DisclosureFingerprint: workerHash("disclosure")}
	profile := configuredProfile{record: fixture.profile, embedders: map[string]document.EmbeddingProvider{"kit": localTextEmbeddingProvider{descriptor: descriptor}}}
	service := Service{blobs: fixture.blobs}
	for _, tc := range []struct {
		name, mediaType string
		data            []byte
		eligible        bool
	}{
		{"image", "image/png", mediatest.PNG(2, 2, color.White), true},
		{"six pages", "application/pdf", kitFlateScan(t, 6, 8, 8), true},
		{"seven pages", "application/pdf", kitFlateScan(t, 7, 8, 8), false},
		{"video", "video/mp4", mediatest.H265MP4(), true},
		{"long video", "video/mp4", mediatest.H264LongMP4(), false},
		{"unsupported", "text/plain", []byte("synthetic text"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			receipt, err := fixture.blobs.WriteDetailedContext(t.Context(), bytes.NewReader(tc.data))
			require.NoError(t, err)
			_, err = fixture.catalog.CreateFile(t.Context(), fixture.catalog.RootID(), tc.name, receipt.Hash, receipt.Size, tc.mediaType, processingBlobPhysical(t, receipt))
			require.NoError(t, err)
			eligible, err := service.kitMediaEligible(t.Context(), store.ContentVersion{BlobHash: receipt.Hash, Size: receipt.Size, MimeType: tc.mediaType}, profile, binding)
			require.NoError(t, err)
			require.Equal(t, tc.eligible, eligible)
		})
	}
}

func TestKitMediaIndexAndSearch(t *testing.T) {
	t.Parallel()
	items := []struct {
		name, media, label string
		data               []byte
	}{
		{"white.jpg", "image/jpeg", "amber", mediatest.JPEG(2, 2, color.White)},
		{"image.webp", "image/webp", "birch", mediatest.WebP(16, 16)},
		{"scan.pdf", "application/pdf", "grove", kitFlateScan(t, 6, 4096, 1707)},
		{"green.mp4", "video/mp4", "ember", mediatest.H265MP4()},
	}
	backend := &kitMediaBackend{hashes: make(map[[32]byte]string)}
	for _, item := range items {
		backend.labels = append(backend.labels, item.label)
		backend.hashes[sha256.Sum256(item.data)] = item.label
	}
	server := httptest.NewServer(embedserver.Handler(backend, []string{"gemini-embedding-2"}))
	defer server.Close()
	descriptor := embeddingWorkerDescriptor(t)
	contract, err := document.NewModelInputContract(document.ModelInputContractConfig{Profile: document.ModelInputProfileNomic})
	require.NoError(t, err)
	descriptor.ModelInput, descriptor.CompatibilityID = contract, contract.CompatibilityID
	descriptor.ID, descriptor.TrustBoundary = openaicompat.ProviderID, document.EmbeddingTrustOperatorNetwork
	descriptor.DocumentFormatter, descriptor.QueryFormatter = openaicompat.DocumentFormatterV1, openaicompat.QueryFormatterV1
	descriptor.InputKinds = []document.EmbeddingInputKind{document.EmbeddingInputOriginalFile}
	descriptor.Model = "gemini-embedding-2"
	profile := openaicompat.Profile{API: embedconfig.APIKit, Origin: server.URL, Descriptor: descriptor, ModelInput: contract, DeploymentEpoch: descriptor.ModelRevision, RequestTimeout: time.Second, MaxBatchItems: 8, MaxInputBytes: 64 << 20, MaxRequestBytes: 96 << 20, MaxResponseBytes: 1 << 20}
	profile.Descriptor.PolicyFingerprint, err = openaicompat.PolicyFingerprint(profile)
	require.NoError(t, err)
	profile.Descriptor, err = document.NewEmbeddingDescriptor(profile.Descriptor)
	require.NoError(t, err)
	provider, err := openaicompat.New(profile, nil, server.Client())
	require.NoError(t, err)
	fixture, _, _, request, config := embeddingWorkerWithDescriptorFixture(t, newPublicationFixture, document.EmbeddingInputOriginalFile, &profile.Descriptor)
	var portable document.ProcessingProfileV1
	require.NoError(t, json.Unmarshal(request.Profile.CanonicalProfile, &portable))
	portable.Embeddings[0].MaxInputBytes = 64 << 20
	canonical, fingerprints, err := document.CanonicalProfile(portable)
	require.NoError(t, err)
	fixture.profile.CanonicalProfile, fixture.profile.Fingerprint = canonical, fingerprints.Profile
	fixture.profile.RenditionRequestFingerprint = fingerprints.RenditionRequest
	fixture.profile.RetentionDisclosureFingerprint = fingerprints.RetentionDisclosure
	staged := fixture.stage(t, publicationIDs{build: "aa", attachment: "bb"}, "synthetic evidence", "Example")
	publisher, err := NewArtifactPublisher(fixture.catalog, fixture.blobs)
	require.NoError(t, err)
	_, err = publisher.PublishRendition(t.Context(), staged)
	require.NoError(t, err)
	request.Profile = fixture.profile
	request.InputGeneration.ProcessingProfileFingerprint = fingerprints.Profile
	request.Authorization.ProfileFingerprint = fingerprints.Profile
	_, err = fixture.catalog.GrantConsent(t.Context(), store.ProcessingConsentGrantRequest{
		Principal: request.Authorization.Principal, Scope: request.Authorization.Scope,
		ProfileFingerprint: fingerprints.Profile, DisclosureFingerprint: request.Authorization.DisclosureFingerprint,
		InputClasses: request.Authorization.InputClasses, RetainedArtifactClasses: request.Authorization.RetainedArtifactClasses,
	})
	require.NoError(t, err)
	runtime, err := NewProviderEmbeddingRuntime(provider, fixture.blobs, t.TempDir(), func(error) (EmbeddingProviderFailure, time.Duration) { return EmbeddingProviderPermanent, 0 })
	require.NoError(t, err)
	config.Runtime = runtime
	worker, err := NewEmbeddingWorker(config)
	require.NoError(t, err)
	enqueue := func(i int, name, media string, data []byte) (string, store.EmbeddingJobStatus) {
		t.Helper()
		receipt, err := fixture.blobs.WriteDetailedContext(t.Context(), bytes.NewReader(data))
		require.NoError(t, err)
		node, err := fixture.catalog.CreateFile(t.Context(), fixture.catalog.RootID(), name, receipt.Hash, receipt.Size, media, processingBlobPhysical(t, receipt))
		require.NoError(t, err)
		work := request
		work.ContentVersionID = node.CurrentVersionID
		work.InputGeneration.SourceVersionID = node.CurrentVersionID
		work.InputGeneration.ID = workerHash(fmt.Sprintf("kit-media-%d", i))
		work.InputGeneration.GenerationChecksum = receipt.Hash
		work.InputGeneration.Inputs = []store.EmbeddingInputReference{{ID: node.CurrentVersionID, RenderedChecksum: receipt.Hash}}
		job, err := fixture.catalog.EnqueueEmbeddingJob(t.Context(), work)
		require.NoError(t, err)
		_, err = worker.ScanOnce(t.Context())
		require.NoError(t, err)
		status, err := fixture.catalog.EmbeddingJobByID(t.Context(), job.ID)
		require.NoError(t, err)
		return node.CurrentVersionID, status
	}
	ids := make([]string, len(items))
	for i, item := range items {
		var status store.EmbeddingJobStatus
		ids[i], status = enqueue(i, item.name, item.media, item.data)
		require.Equal(t, "completed", status.State, "%s: %+v", item.name, status)
		if i == 0 {
			before := len(backend.recorded())
			for j, rejectedItem := range []struct {
				name, media string
				data        []byte
			}{
				{"seven.pdf", "application/pdf", kitFlateScan(t, 7, 8, 8)},
				{"long.mp4", "video/mp4", mediatest.H264LongMP4()},
			} {
				_, rejected := enqueue(len(items)+j, rejectedItem.name, rejectedItem.media, rejectedItem.data)
				require.Equal(t, "failed", rejected.State, rejectedItem.name)
				require.Equal(t, store.EmbeddingFailureInputRejected, rejected.FailureCode, rejectedItem.name)
				require.Len(t, backend.recorded(), before)
			}
		}
	}
	require.Len(t, backend.recorded(), len(items))
	spaces, err := fixture.catalog.ListVectorIndexSpaces(t.Context())
	require.NoError(t, err)
	require.Len(t, spaces, 1)
	index, err := NewIndexWorker(IndexWorkerConfig{Catalog: fixture.catalog, Mutate: newTestOperationGate().MutateContext, Owner: "kit-test", BuildLease: time.Minute, ReaderLease: time.Minute, IdleDelay: time.Millisecond, ReadVectorSet: func(ctx context.Context, member store.VectorIndexMember) ([]byte, error) {
		return fixture.catalog.ReadVectorIndexVectorSet(ctx, fixture.blobs, member)
	}})
	require.NoError(t, err)
	_, err = index.Rebuild(t.Context(), spaces[0])
	require.NoError(t, err)
	searcher, err := retrieval.NewSearcher(retrieval.SearcherConfig{Backend: fixture.catalog, Encoders: kitMediaResolver{provider}, Owner: "kit-test", LeaseDuration: time.Minute})
	require.NoError(t, err)
	for i, item := range items {
		report, err := searcher.Search(t.Context(), retrieval.Query{Text: item.label, Mode: retrieval.ModeSemantic, Limit: 10, ProcessingProfileFingerprint: request.Profile.Fingerprint, BindingID: request.BindingID, Authorization: document.EmbeddingAuthorization{ProviderID: profile.Descriptor.ID, DescriptorFingerprint: profile.Descriptor.Fingerprint, PolicyFingerprint: profile.Descriptor.PolicyFingerprint, MaxBatchItems: 1, MaxInputBytes: 1 << 20, MaxResponseBytes: 1 << 20}})
		require.NoError(t, err)
		var resultIDs []string
		for _, result := range report.Results {
			resultIDs = append(resultIDs, result.Document.ContentVersionID)
		}
		require.Contains(t, resultIDs, ids[i], item.label)
	}
}

func TestKitMediaLimitBeforeBlobRead(t *testing.T) {
	t.Parallel()
	openError := errors.New("synthetic open")
	for _, tc := range []struct {
		name, providerID string
		size             int64
		rejected         bool
	}{
		{"kit over limit", openaicompat.ProviderID, media.MaxBytes + 1, true},
		{"kit at limit", openaicompat.ProviderID, media.MaxBytes, false},
		{"other", "synthetic", media.MaxBytes + 1, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			descriptor := embeddingWorkerDescriptor(t)
			descriptor.ID = tc.providerID
			blobs := &embeddingRuntimeTestBlobs{openErr: openError}
			provider := &embeddingWorkerProvider{descriptor: descriptor}
			runtime, err := NewProviderEmbeddingRuntime(provider, blobs, t.TempDir(), func(error) (EmbeddingProviderFailure, time.Duration) { return EmbeddingProviderPermanent, 0 })
			require.NoError(t, err)
			work := EmbeddingWork{Descriptor: descriptor, Binding: document.EmbeddingBindingV1{InputKind: document.EmbeddingInputOriginalFile, MaxInputBytes: 64 << 20}, SourceBytes: tc.size, SourceMediaType: "application/pdf"}
			_, err = runtime.Prepare(t.Context(), work)
			if tc.rejected {
				require.ErrorIs(t, err, ErrEmbeddingInputRejected)
				require.Zero(t, blobs.opens)
			} else {
				require.ErrorIs(t, err, openError)
				require.Equal(t, 1, blobs.opens)
			}
		})
	}
}
