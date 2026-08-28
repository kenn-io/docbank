package processing

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/blob"
	"go.kenn.io/docbank/internal/config"
	"go.kenn.io/docbank/internal/store"
	"go.kenn.io/docbank/internal/vectorindex"
	"go.kenn.io/kit/packstore"
)

func TestIndexWorkerRebuildPublishesValidatedSearchableGeneration(t *testing.T) {
	catalog, space := newIndexCatalogFixture(t)
	worker := newIndexWorkerForTest(t, catalog)

	record, err := worker.Rebuild(t.Context(), space)
	require.NoError(t, err)
	assert.Equal(t, space, record.VectorSpaceID)
	assert.Equal(t, catalog.source.ManifestChecksum, record.SourceManifestChecksum)
	assert.Equal(t, record.ID, catalog.active.ID)
	assert.Equal(t, 1, catalog.stageCalls)
	assert.Equal(t, 1, catalog.publishCalls)

	lease, err := worker.Acquire(t.Context(), space, "test-reader")
	require.NoError(t, err)
	neighbors, err := lease.Search([]float32{1, 0}, 1)
	require.NoError(t, err)
	require.Len(t, neighbors, 1)
	assert.Equal(t, catalog.source.Members[0].VectorSetID, neighbors[0].SetID)
	require.NoError(t, lease.Release(t.Context()))
}

func TestIndexWorkerCorruptCandidateAndDriftLeavePriorGenerationActive(t *testing.T) {
	for _, testCase := range []struct {
		name    string
		arrange func(*fakeIndexCatalog)
		want    error
	}{
		{"corrupt candidate", func(c *fakeIndexCatalog) { c.corruptLoadedCandidate = true }, ErrVectorIndexCandidateInvalid},
		{"membership drift", func(c *fakeIndexCatalog) { c.publishErr = store.ErrVectorIndexSourceStale }, store.ErrVectorIndexSourceStale},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			catalog, space := newIndexCatalogFixture(t)
			prior := store.VectorIndexGenerationRecord{ID: indexWorkerHash("prior"),
				VectorSpaceID: space, SourceManifestChecksum: indexWorkerHash("prior-source"),
				IndexManifestChecksum: indexWorkerHash("prior-index"), Bytes: []byte("prior"),
				RowCount: 1, BuiltAt: indexWorkerTime(indexWorkerNow)}
			catalog.active = prior
			testCase.arrange(catalog)
			_, err := newIndexWorkerForTest(t, catalog).Rebuild(t.Context(), space)
			require.ErrorIs(t, err, testCase.want)
			assert.Equal(t, prior, catalog.active)
		})
	}
}

func TestIndexWorkerConcurrentRebuildSharesOneBuild(t *testing.T) {
	catalog, space := newIndexCatalogFixture(t)
	catalog.readStarted = make(chan struct{})
	catalog.allowRead = make(chan struct{})
	worker := newIndexWorkerForTest(t, catalog)

	results := make(chan error, 2)
	go func() { _, err := worker.Rebuild(t.Context(), space); results <- err }()
	<-catalog.readStarted
	go func() { _, err := worker.Rebuild(t.Context(), space); results <- err }()
	close(catalog.allowRead)
	require.NoError(t, <-results)
	require.NoError(t, <-results)
	assert.Equal(t, 1, catalog.readCalls)
	assert.Equal(t, 1, catalog.stageCalls)
	assert.Equal(t, 1, catalog.publishCalls)
}

func TestIndexWorkerRebuildReadsSecondaryOnlyManagedAuthority(t *testing.T) {
	catalog, space := newIndexCatalogFixture(t)
	member := catalog.source.Members[0]
	payload := catalog.payloads[member.PayloadBlobHash]
	root := t.TempDir()
	metadata, err := store.Open(filepath.Join(root, "metadata.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, metadata.Close()) })
	secondary, err := metadata.PrepareSecondaryBlobStore("archive", "filesystem", "archive")
	require.NoError(t, err)
	secondaryPath := filepath.Join(root, "archive")
	require.NoError(t, blob.EnsureFilesystemNamespace(secondaryPath))
	secondaryBackend, err := blob.NewFilesystemBackend(secondaryPath, nil)
	require.NoError(t, err)
	require.NoError(t, secondaryBackend.ReplaceOwnership(t.Context(), packstore.Ownership{
		Format: packstore.OwnershipFormatV1, Vault: metadata.VaultID(),
		Store: packstore.StoreID(secondary.ID), Epoch: secondary.OwnershipEpoch,
	}, nil))
	require.NoError(t, secondaryBackend.Close())
	require.NoError(t, metadata.RegisterBlobStore(t.Context(), secondary))
	registry := blob.NewRegistry(t.Context(), metadata.VaultID(), map[string]config.StoreBindingConfig{
		"archive": {Kind: "filesystem", Path: secondaryPath, Priority: 20},
	}, []blob.StoreSpec{{ID: secondary.ID, Kind: secondary.Kind, Role: secondary.Role,
		Lifecycle: secondary.Lifecycle, Binding: secondary.Binding,
		OwnershipEpoch: secondary.OwnershipEpoch}})
	managed, err := blob.NewWithOptions(store.NewPackCatalog(metadata), filepath.Join(root, "blobs"),
		blob.Options{Registry: registry})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, managed.Close()) })
	written, err := managed.WriteDetailedContext(t.Context(), bytes.NewReader(payload))
	require.NoError(t, err)
	require.Equal(t, member.PayloadBlobHash, written.Hash)
	encoding, err := written.EncodingName()
	require.NoError(t, err)
	node, err := metadata.CreateFile(t.Context(), metadata.RootID(), "synthetic-vector-set.bin",
		written.Hash, written.Size, "application/octet-stream", store.BlobPhysical{
			Encoding: encoding, StoredBytes: written.StoredSize, PackEligible: written.PackEligible,
			Created: written.Created,
		})
	require.NoError(t, err)
	plan, err := metadata.PlanPlacement(t.Context(), store.PlacementRequest{
		TargetNodeID: node.ID, SourceStoreID: metadata.PrimaryBlobStoreID(),
		DestinationStoreID: secondary.ID, RetireSource: true,
	})
	require.NoError(t, err)
	planJSON, err := json.Marshal(plan)
	require.NoError(t, err)
	requestJSON, err := json.Marshal(plan.Request)
	require.NoError(t, err)
	operation, err := metadata.CreateStorageOperation(t.Context(), store.StorageOperationCreate{
		Kind: "place", RequestDigest: plan.Digest, RequestJSON: string(requestJSON),
		PlanJSON: string(planJSON), TotalObjects: int64(len(plan.Hashes)),
	})
	require.NoError(t, err)
	require.NoError(t, (blob.PlacementRunner{Metadata: metadata, Blobs: managed}).Run(t.Context(), operation.ID))
	resolution, err := metadata.ResolveBlobLocations(t.Context(), packstore.Hash(member.PayloadBlobHash))
	require.NoError(t, err)
	require.Len(t, resolution.Candidates, 1)
	require.Equal(t, packstore.StoreID(secondary.ID), resolution.Candidates[0].StoreID)

	worker := newIndexWorkerWithConfig(t, catalog, IndexWorkerConfig{Blobs: managed})
	record, err := worker.Rebuild(t.Context(), space)
	require.NoError(t, err)
	assert.Equal(t, space, record.VectorSpaceID)
}

func TestIndexWorkerRestoreRebuildsIncludedAndReportsOmittedWithoutProvider(t *testing.T) {
	included, includedSpace := newIndexCatalogFixture(t)
	missingPayload := included.source.Members[0]
	missingPayload.PayloadBlobHash = indexWorkerHash("missing-payload")
	omittedSpace := indexWorkerHash("omitted-space")
	included.sources = map[string]store.VectorIndexSource{
		includedSpace: included.source,
		omittedSpace: {
			VectorSpaceID: omittedSpace, ManifestChecksum: indexWorkerHash("omitted-source"),
			Members: []store.VectorIndexMember{missingPayload},
		},
	}
	included.spaces = []string{omittedSpace, includedSpace}
	included.unavailable[missingPayload.PayloadBlobHash] = true

	report, err := newIndexWorkerForTest(t, included).Restore(t.Context())
	require.NoError(t, err)
	require.Len(t, report.Rebuilt, 1)
	assert.Equal(t, includedSpace, report.Rebuilt[0].VectorSpaceID)
	require.Equal(t, []UnavailableVectorCoverage{{
		VectorSpaceID: omittedSpace, SourceManifestChecksum: indexWorkerHash("omitted-source"),
		Missing: []UnavailableVectorSet{{EmbeddingSetID: missingPayload.EmbeddingSetID,
			VectorSetID: missingPayload.VectorSetID, PayloadBlobHash: missingPayload.PayloadBlobHash}},
		ExternalReembeddingRequired: true,
	}}, report.Unavailable)
	assert.Equal(t, report.Unavailable, included.recordedUnavailable)
	assert.Equal(t, 0, included.providerCalls, "the catalog exposes no provider operation to restore")
}

func TestIndexWorkerRestoreSkipsSpacesWithoutEligibleMembership(t *testing.T) {
	catalog, includedSpace := newIndexCatalogFixture(t)
	staleSpace := indexWorkerHash("stale-space")
	catalog.spaces = []string{staleSpace, includedSpace}

	report, err := newIndexWorkerForTest(t, catalog).Restore(t.Context())
	require.NoError(t, err)
	require.Len(t, report.Rebuilt, 1)
	assert.Equal(t, includedSpace, report.Rebuilt[0].VectorSpaceID)
	assert.Empty(t, report.Unavailable)
}

func TestIndexWorkerPreflightsAggregateBoundsBeforePhysicalReads(t *testing.T) {
	for _, test := range []struct {
		name    string
		arrange func(*fakeIndexCatalog)
		options vectorindex.Options
		want    string
	}{
		{name: "membership", arrange: func(c *fakeIndexCatalog) {
			member := c.source.Members[0]
			member.EmbeddingSetID = indexWorkerHash("second-embedding-set")
			member.VectorSetID = indexWorkerHash("second-vector-set")
			member.PayloadBlobHash = indexWorkerHash("second-payload")
			c.source.Members = append(c.source.Members, member)
			c.sources[c.source.VectorSpaceID] = c.source
		}, options: vectorindex.Options{MaxRows: 1, MaxDimension: 16_384, MaxBytes: 512 << 20},
			want: "membership exceeds"},
		{name: "aggregate payload", arrange: func(*fakeIndexCatalog) {},
			options: vectorindex.Options{MaxRows: 10, MaxDimension: 16_384, MaxBytes: 1},
			want:    "aggregate payload exceeds"},
	} {
		t.Run(test.name, func(t *testing.T) {
			catalog, space := newIndexCatalogFixture(t)
			test.arrange(catalog)
			worker := newIndexWorkerWithConfig(t, catalog, IndexWorkerConfig{BuildOptions: test.options})
			_, err := worker.Rebuild(t.Context(), space)
			require.ErrorContains(t, err, test.want)
			assert.Zero(t, catalog.readCalls, "aggregate preflight must precede every physical read")
		})
	}
}

func TestIndexWorkerBoundsDecodedRowsBeforeVectorMaterialization(t *testing.T) {
	catalog, space := newIndexCatalogFixture(t)
	worker := newIndexWorkerWithConfig(t, catalog, IndexWorkerConfig{BuildOptions: vectorindex.Options{
		MaxRows: 1, MaxDimension: 16_384, MaxBytes: 512 << 20,
	}})
	_, err := worker.Rebuild(t.Context(), space)
	require.ErrorContains(t, err, "decoding canonical vector set")
	assert.Equal(t, 1, catalog.readCalls)
	assert.Zero(t, catalog.stageCalls, "oversized decoded rows must never reach generation materialization")
}

func TestIndexWorkerRunMakesListCorruptionAndUnavailableCoverageObservable(t *testing.T) {
	t.Run("list failure", func(t *testing.T) {
		catalog, _ := newIndexCatalogFixture(t)
		catalog.listErr = errors.New("synthetic list failure")
		err := newIndexWorkerForTest(t, catalog).Run(t.Context())
		require.ErrorContains(t, err, "synthetic list failure")
	})

	t.Run("corrupt payload", func(t *testing.T) {
		catalog, _ := newIndexCatalogFixture(t)
		hash := catalog.source.Members[0].PayloadBlobHash
		catalog.payloads[hash][len(catalog.payloads[hash])/2] ^= 0xff
		err := newIndexWorkerForTest(t, catalog).Run(t.Context())
		require.ErrorContains(t, err, "rebuilding vector index space")
		assert.Empty(t, catalog.recordedUnavailable)
	})

	t.Run("post-capture missing candidate", func(t *testing.T) {
		catalog, _ := newIndexCatalogFixture(t)
		catalog.loadErr = store.ErrNotFound
		unexpectedWait := errors.New("unexpected idle wait")
		worker := newIndexWorkerWithConfig(t, catalog, IndexWorkerConfig{
			Wait: func(context.Context, time.Duration) error { return unexpectedWait },
		})
		err := worker.Run(t.Context())
		require.ErrorIs(t, err, store.ErrNotFound)
		require.NotErrorIs(t, err, unexpectedWait)
		assert.Empty(t, catalog.recordedUnavailable,
			"a post-capture integrity failure must not replace coverage")
	})

	t.Run("unavailable live coverage", func(t *testing.T) {
		catalog, space := newIndexCatalogFixture(t)
		member := catalog.source.Members[0]
		catalog.unavailable[member.PayloadBlobHash] = true
		stop := errors.New("stop after observable idle wait")
		worker := newIndexWorkerWithConfig(t, catalog, IndexWorkerConfig{
			Wait: func(context.Context, time.Duration) error { return stop },
		})
		err := worker.Run(t.Context())
		require.ErrorIs(t, err, stop)
		require.Equal(t, []UnavailableVectorCoverage{{VectorSpaceID: space,
			SourceManifestChecksum: catalog.source.ManifestChecksum,
			Missing: []UnavailableVectorSet{{EmbeddingSetID: member.EmbeddingSetID,
				VectorSetID: member.VectorSetID, PayloadBlobHash: member.PayloadBlobHash}},
			ExternalReembeddingRequired: true}}, catalog.recordedUnavailable)
	})
}

func TestIndexWorkerRunBacksOffExpectedContentionWithoutBusyLooping(t *testing.T) {
	catalog, _ := newIndexCatalogFixture(t)
	catalog.claimed = false
	stop := errors.New("stop bounded retry")
	var delays []time.Duration
	worker := newIndexWorkerWithConfig(t, catalog, IndexWorkerConfig{
		IdleDelay: time.Millisecond, MaxRetryDelay: 4 * time.Millisecond,
		Wait: func(_ context.Context, delay time.Duration) error {
			delays = append(delays, delay)
			if len(delays) == 3 {
				return stop
			}
			return nil
		},
	})
	err := worker.Run(t.Context())
	require.ErrorIs(t, err, stop)
	assert.Equal(t, []time.Duration{2 * time.Millisecond, 4 * time.Millisecond, 4 * time.Millisecond}, delays)
}

var indexWorkerNow = time.Date(2026, 8, 26, 14, 0, 0, 0, time.UTC)

type fakeIndexCatalog struct {
	mu                                                 sync.Mutex
	source                                             store.VectorIndexSource
	sources                                            map[string]store.VectorIndexSource
	spaces                                             []string
	payloads                                           map[string][]byte
	unavailable                                        map[string]bool
	staged                                             map[string]store.VectorIndexGenerationRecord
	active                                             store.VectorIndexGenerationRecord
	claim                                              store.VectorIndexBuildClaim
	lease                                              store.VectorIndexReaderLease
	publishErr                                         error
	loadErr                                            error
	corruptLoadedCandidate                             bool
	readStarted, allowRead                             chan struct{}
	readCalls, stageCalls, publishCalls, providerCalls int
	recordedUnavailable                                []UnavailableVectorCoverage
	listErr                                            error
	claimed                                            bool
}

func newIndexCatalogFixture(t *testing.T) (*fakeIndexCatalog, string) {
	t.Helper()
	space := indexWorkerHash("space")
	set, err := document.NewVectorSetV1(document.VectorSetV1Input{
		VectorSpaceFingerprint: space, Metric: document.VectorMetricDotProduct,
		Normalization: document.VectorNormalizationNone, Dimension: 2,
		InputKeys:      []string{"row-a", "row-b"},
		InputChecksums: []string{indexWorkerHash("row-a"), indexWorkerHash("row-b")},
		Values:         [][]float64{{1, 0}, {0, 1}},
	})
	require.NoError(t, err)
	payload, setID, err := document.EncodeVectorSetV1(set)
	require.NoError(t, err)
	payloadHash := indexWorkerHashBytes(payload)
	source := store.VectorIndexSource{VectorSpaceID: space,
		ManifestChecksum: indexWorkerHash("source-manifest"), Members: []store.VectorIndexMember{{
			EmbeddingSetID: indexWorkerHash("embedding-set"), VectorSetID: setID,
			PayloadBlobHash: payloadHash, PayloadSize: int64(len(payload)),
		}}}
	return &fakeIndexCatalog{source: source, sources: map[string]store.VectorIndexSource{space: source},
		spaces: []string{space}, payloads: map[string][]byte{payloadHash: payload},
		unavailable: make(map[string]bool), staged: make(map[string]store.VectorIndexGenerationRecord), claimed: true}, space
}

func newIndexWorkerForTest(t *testing.T, catalog *fakeIndexCatalog) *IndexWorker {
	t.Helper()
	return newIndexWorkerWithConfig(t, catalog, IndexWorkerConfig{})
}

func newIndexWorkerWithConfig(t *testing.T, catalog *fakeIndexCatalog, config IndexWorkerConfig) *IndexWorker {
	t.Helper()
	config.Catalog = catalog
	if config.Blobs == nil {
		config.Blobs = catalog
	}
	if config.Gate == nil {
		config.Gate = directIndexGate{}
	}
	config.Owner = "test-index-worker"
	config.Clock = func() time.Time { return indexWorkerNow }
	config.BuildLease, config.ReaderLease = time.Minute, time.Minute
	if config.IdleDelay == 0 {
		config.IdleDelay = time.Millisecond
	}
	worker, err := NewIndexWorker(config)
	require.NoError(t, err)
	return worker
}

func (c *fakeIndexCatalog) CaptureVectorIndexSource(_ context.Context, space string) (store.VectorIndexSource, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	source, ok := c.sources[space]
	if !ok {
		return store.VectorIndexSource{}, store.ErrNotFound
	}
	return source, nil
}
func (c *fakeIndexCatalog) ListVectorIndexSpaces(context.Context) ([]string, error) {
	return append([]string(nil), c.spaces...), c.listErr
}
func (c *fakeIndexCatalog) OpenStreamContext(_ context.Context, hash string) (packstore.VerifiedReadCloser, int64, error) {
	c.mu.Lock()
	c.readCalls++
	started, allow := c.readStarted, c.allowRead
	unavailable := c.unavailable[hash]
	payload := bytes.Clone(c.payloads[hash])
	c.mu.Unlock()
	if started != nil {
		select {
		case <-started:
		default:
			close(started)
		}
		<-allow
	}
	if unavailable {
		return nil, 0, packstore.ErrPhysicalAuthorityMissing
	}
	return &fakeVerifiedVectorReader{Reader: bytes.NewReader(payload)}, int64(len(payload)), nil
}
func (c *fakeIndexCatalog) ClaimVectorIndexBuild(_ context.Context, space, source, owner string, at time.Time, lease time.Duration) (store.VectorIndexBuildClaim, bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.claim = store.VectorIndexBuildClaim{VectorSpaceID: space, SourceManifestChecksum: source, Owner: owner, FencingToken: 1, ExpiresAt: at.Add(lease)}
	return c.claim, c.claimed, nil
}
func (c *fakeIndexCatalog) StageVectorIndexGeneration(_ context.Context, _ store.VectorIndexBuildClaim, record store.VectorIndexGenerationRecord, _ time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.stageCalls++
	c.staged[record.ID] = record
	return nil
}
func (c *fakeIndexCatalog) LoadVectorIndexGeneration(_ context.Context, id string) (store.VectorIndexGenerationRecord, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.loadErr != nil {
		return store.VectorIndexGenerationRecord{}, c.loadErr
	}
	record := c.staged[id]
	record.Bytes = bytes.Clone(record.Bytes)
	if c.corruptLoadedCandidate {
		record.Bytes[len(record.Bytes)/2] ^= 1
	}
	return record, nil
}
func (c *fakeIndexCatalog) PublishVectorIndexGeneration(_ context.Context, _ store.VectorIndexBuildClaim, id string, _ time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.publishCalls++
	if c.publishErr != nil {
		return c.publishErr
	}
	c.active = c.staged[id]
	return nil
}
func (c *fakeIndexCatalog) ActiveVectorIndexGeneration(context.Context, string) (store.VectorIndexGenerationRecord, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.active.ID == "" {
		return store.VectorIndexGenerationRecord{}, store.ErrNotFound
	}
	return c.active, nil
}
func (c *fakeIndexCatalog) AcquireVectorIndexGeneration(_ context.Context, _ string, _ string, at time.Time, duration time.Duration) (store.VectorIndexReaderLease, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.active.ID == "" {
		return store.VectorIndexReaderLease{}, store.ErrNotFound
	}
	c.lease = store.VectorIndexReaderLease{ID: "lease", FencingToken: 1, ExpiresAt: at.Add(duration), Generation: c.active}
	return c.lease, nil
}
func (c *fakeIndexCatalog) ReleaseVectorIndexGeneration(context.Context, string, int64, time.Time) error {
	return nil
}
func (c *fakeIndexCatalog) ReclaimVectorIndexGenerations(context.Context, time.Time) (int, error) {
	return 0, nil
}
func (c *fakeIndexCatalog) ReplaceVectorIndexUnavailableCoverage(_ context.Context, coverage []UnavailableVectorCoverage) error {
	c.recordedUnavailable = append([]UnavailableVectorCoverage(nil), coverage...)
	return nil
}

func indexWorkerHash(value string) string { return indexWorkerHashBytes([]byte(value)) }
func indexWorkerHashBytes(value []byte) string {
	sum := sha256.Sum256(value)
	return hex.EncodeToString(sum[:])
}
func indexWorkerTime(value time.Time) string {
	return value.UTC().Format("2006-01-02T15:04:05.000000000Z")
}

type directIndexGate struct{}

func (directIndexGate) PreserveContext(_ context.Context, fn func() error) error { return fn() }

type fakeVerifiedVectorReader struct {
	*bytes.Reader

	verified bool
}

func (reader *fakeVerifiedVectorReader) Read(buffer []byte) (int, error) {
	count, err := reader.Reader.Read(buffer)
	if errors.Is(err, io.EOF) {
		reader.verified = true
	}
	if err != nil {
		return count, fmt.Errorf("reading synthetic vector payload: %w", err)
	}
	return count, nil
}
func (*fakeVerifiedVectorReader) Close() error { return nil }
func (reader *fakeVerifiedVectorReader) Verify() error {
	reader.verified = true
	return nil
}
func (reader *fakeVerifiedVectorReader) Verified() bool { return reader.verified }

var _ = errors.Is
