package processing

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/store"
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
	corruptLoadedCandidate                             bool
	readStarted, allowRead                             chan struct{}
	readCalls, stageCalls, publishCalls, providerCalls int
	recordedUnavailable                                []UnavailableVectorCoverage
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
		unavailable: make(map[string]bool), staged: make(map[string]store.VectorIndexGenerationRecord)}, space
}

func newIndexWorkerForTest(t *testing.T, catalog *fakeIndexCatalog) *IndexWorker {
	t.Helper()
	worker, err := NewIndexWorker(IndexWorkerConfig{Catalog: catalog, Owner: "test-index-worker",
		Clock: func() time.Time { return indexWorkerNow }, BuildLease: time.Minute,
		ReaderLease: time.Minute, IdleDelay: time.Millisecond})
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
	return append([]string(nil), c.spaces...), nil
}
func (c *fakeIndexCatalog) ReadVectorIndexVectorSet(_ context.Context, member store.VectorIndexMember) ([]byte, error) {
	c.mu.Lock()
	c.readCalls++
	started, allow := c.readStarted, c.allowRead
	unavailable := c.unavailable[member.PayloadBlobHash]
	payload := bytes.Clone(c.payloads[member.PayloadBlobHash])
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
		return nil, store.ErrVectorSetUnavailable
	}
	return payload, nil
}
func (c *fakeIndexCatalog) ClaimVectorIndexBuild(_ context.Context, space, source, owner string, at time.Time, lease time.Duration) (store.VectorIndexBuildClaim, bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.claim = store.VectorIndexBuildClaim{VectorSpaceID: space, SourceManifestChecksum: source, Owner: owner, FencingToken: 1, ExpiresAt: at.Add(lease)}
	return c.claim, true, nil
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

var _ = errors.Is
