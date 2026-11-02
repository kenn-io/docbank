package vector_test

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"math"
	"slices"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/kit/vector"
)

// memStore is an in-memory Store[int64, int] used to exercise the flows
// without any real backend. Documents are keyed by int64; generations by
// int. QueryGeneration ranks by cosine similarity over stored vectors.
// Setting revision enables optimistic-concurrency checks in SaveVectors.
type memStore struct {
	content  map[int64]string
	revision map[int64]int                          // nil disables revision tracking
	embedded map[int64]map[int]bool                 // doc -> gen -> done
	vectors  map[int]map[int64][]vector.ChunkVector // gen -> doc -> chunks
	live     []int                                  // descending preference
}

func newMemStore() *memStore {
	return &memStore{
		content:  map[int64]string{},
		embedded: map[int64]map[int]bool{},
		vectors:  map[int]map[int64][]vector.ChunkVector{},
	}
}

func (m *memStore) PendingForGeneration(_ context.Context, gen int, limit int) ([]vector.Pending[int64], error) {
	keys := make([]int64, 0, len(m.content))
	for doc := range m.content {
		if !m.embedded[doc][gen] {
			keys = append(keys, doc)
		}
	}
	slices.Sort(keys)
	if limit > 0 && len(keys) > limit {
		keys = keys[:limit]
	}
	out := make([]vector.Pending[int64], len(keys))
	for i, doc := range keys {
		out[i] = vector.Pending[int64]{Doc: doc, Content: m.content[doc]}
		if m.revision != nil {
			out[i].Revision = m.revision[doc]
		}
	}
	return out, nil
}

func (m *memStore) SaveVectors(_ context.Context, gen int, doc int64, revision any, vecs []vector.ChunkVector) error {
	if m.revision != nil && revision != any(m.revision[doc]) {
		return vector.ErrStale
	}
	if m.vectors[gen] == nil {
		m.vectors[gen] = map[int64][]vector.ChunkVector{}
	}
	m.vectors[gen][doc] = vecs
	if m.embedded[doc] == nil {
		m.embedded[doc] = map[int]bool{}
	}
	m.embedded[doc][gen] = true
	return nil
}

func (m *memStore) LiveGenerations(_ context.Context) ([]int, error) {
	return m.live, nil
}

func (m *memStore) QueryGeneration(_ context.Context, gen int, query vector.Vector, limit int) ([]vector.Hit[int64], error) {
	var hits []vector.Hit[int64]
	for doc, chunks := range m.vectors[gen] {
		for _, cv := range chunks {
			hits = append(hits, vector.Hit[int64]{Doc: doc, ChunkIndex: cv.ChunkIndex, Score: cosine(query, cv.Vector)})
		}
	}
	sort.SliceStable(hits, func(i, j int) bool { return hits[i].Score > hits[j].Score })
	if limit > 0 && len(hits) > limit {
		hits = hits[:limit]
	}
	return hits, nil
}

func cosine(a, b vector.Vector) float32 {
	var dot, na, nb float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return float32(dot / (math.Sqrt(na) * math.Sqrt(nb)))
}

// lenEncoder embeds each text as a 1-D vector of its rune length, enough
// to confirm Fill wired chunk content through to SaveVectors.
func lenEncoder() vector.EncodeFunc {
	return func(_ context.Context, texts []string) ([][]float32, error) {
		out := make([][]float32, len(texts))
		for i, txt := range texts {
			out[i] = []float32{float32(len([]rune(txt)))}
		}
		return out, nil
	}
}

// textEncoder makes both chunk content and order visible in saved vectors.
func textEncoder() vector.EncodeFunc {
	return func(_ context.Context, texts []string) ([][]float32, error) {
		out := make([][]float32, len(texts))
		for i, text := range texts {
			runes := []rune(text)
			out[i] = []float32{float32(len(runes)), float32(runes[0])}
		}
		return out, nil
	}
}

func TestFillEmbedsAllPendingThenStops(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ctx := t.Context()

	store := newMemStore()
	store.content[1] = "alpha"
	store.content[2] = "beta gamma delta"

	stats, err := vector.Fill(ctx, store, 7, lenEncoder(),
		vector.WithFillScanBatch[int64](1), // force multiple scan rounds
		vector.WithFillSplit[int64](vector.SplitOptions{MaxRunes: 4, Overlap: 0}),
	)
	require.NoError(err)

	assert.Equal(2, stats.Documents)
	assert.True(store.embedded[1][7] && store.embedded[2][7], "both docs stamped for gen 7")
	require.Len(store.vectors[7][1], 2, "alpha -> 2 chunks of <=4 runes")
	assert.InDelta(4, store.vectors[7][1][0].Vector[0], 1e-6, "first chunk carries its rune length")

	// A second run finds nothing pending and embeds zero documents.
	again, err := vector.Fill(ctx, store, 7, lenEncoder())
	require.NoError(err)
	assert.Equal(0, again.Documents)
}

func TestFillBatchesChunksAcrossDocumentsWithinTokenBudget(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ctx := t.Context()

	store := newMemStore()
	for doc := int64(1); doc <= 7; doc++ {
		store.content[doc] = strings.Repeat("x", int(doc))
	}

	var batchSizes []int
	enc := func(ctx context.Context, texts []string) ([][]float32, error) {
		batchSizes = append(batchSizes, len(texts))
		return lenEncoder()(ctx, texts)
	}

	stats, err := vector.Fill(ctx, store, 7, enc,
		vector.WithFillScanBatch[int64](7),
		vector.WithFillBatch[int64](
			vector.WithBatchSize(7),
			vector.WithBatchConcurrency(1),
			vector.WithBatchTokenBudget(3, 1),
		),
		vector.WithFillConcurrency[int64](1),
	)
	require.NoError(err)

	assert.Equal([]int{3, 3, 1}, batchSizes,
		"the token budget caps batches below the count limit")
	assert.Equal(7, stats.Documents)
	assert.Equal(7, stats.Chunks)
	for doc := int64(1); doc <= 7; doc++ {
		require.Len(store.vectors[7][doc], 1)
		assert.InDelta(float64(doc), float64(store.vectors[7][doc][0].Vector[0]), 1e-6,
			"doc %d keeps the vector for its own content", doc)
	}
}

func TestFillRejectsInputAboveTokenBudgetBeforeEncoder(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	store := newMemStore()
	store.content[1] = "one"
	var calls atomic.Int64
	enc := func(context.Context, []string) ([][]float32, error) {
		calls.Add(1)
		return [][]float32{{1}}, nil
	}

	stats, err := vector.Fill(t.Context(), store, 7, enc,
		vector.WithFillBatch[int64](
			vector.WithBatchSize(1),
			vector.WithBatchTokenBudget(31_999, 32_000),
		),
	)

	require.Error(err)
	require.
		ErrorContains(err, "token budget")
	assert.Zero(calls.Load(), "an invalid budget is rejected before the provider call")
	assert.Zero(stats.Documents)
	assert.False(store.embedded[1][7])
}

func TestFillDoesNotSkipInvalidTokenBudgetWithoutBatchSize(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	store := newMemStore()
	store.content[1] = "one"
	var calls, hookCalls atomic.Int64
	enc := func(context.Context, []string) ([][]float32, error) {
		calls.Add(1)
		return [][]float32{{1}}, nil
	}

	stats, err := vector.Fill(t.Context(), store, 7, enc,
		vector.WithFillBatch[int64](
			vector.WithBatchTokenBudget(31_999, 32_000),
		),
		vector.WithFillEncodeError[int64](func(int64, error) bool {
			hookCalls.Add(1)
			return true
		}),
	)

	require.Error(err)
	require.
		ErrorContains(err, "token budget")
	assert.Zero(calls.Load(), "an invalid budget is rejected before the provider call")
	assert.Zero(hookCalls.Load(), "configuration errors bypass the document error handler")
	assert.Zero(stats.Documents)
	assert.Zero(stats.Skipped)
	assert.False(store.embedded[1][7])
}

func TestFillCrossDocumentBatchingMatchesPerDocumentVectors(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ctx := t.Context()
	contents := map[int64]string{
		1: "abcde",
		2: "uvwxyz",
		3: "kit",
	}

	baseline := newMemStore()
	batched := newMemStore()
	for doc, content := range contents {
		baseline.content[doc] = content
		batched.content[doc] = content
	}
	baselineStats, err := vector.Fill(ctx, baseline, 7, textEncoder(),
		vector.WithFillScanBatch[int64](3),
		vector.WithFillSplit[int64](vector.SplitOptions{MaxRunes: 2}),
	)
	require.NoError(err)

	var batchSizes []int
	batchedEncoder := func(ctx context.Context, texts []string) ([][]float32, error) {
		batchSizes = append(batchSizes, len(texts))
		return textEncoder()(ctx, texts)
	}
	batchedStats, err := vector.Fill(ctx, batched, 7, batchedEncoder,
		vector.WithFillScanBatch[int64](3),
		vector.WithFillSplit[int64](vector.SplitOptions{MaxRunes: 2}),
		vector.WithFillBatch[int64](
			vector.WithBatchSize(3), vector.WithBatchConcurrency(1)),
		vector.WithFillConcurrency[int64](1),
	)
	require.NoError(err)

	assert.Equal([]int{3, 3, 2}, batchSizes,
		"document boundaries do not leave partially filled encode calls")
	assert.Equal(baselineStats, batchedStats)
	assert.Equal(baseline.vectors, batched.vectors,
		"cross-document scatter must preserve every document and chunk vector")
}

func TestFillCrossDocumentBatchingMatchesLegacyAcrossConfigurations(t *testing.T) {
	ctx := t.Context()
	contents := map[int64]string{
		1: "",
		2: "a",
		3: "βeta",
		4: "kit batches neighboring chunks",
		5: "世界世界世界",
	}

	baseline := newMemStore()
	maps.Copy(baseline.content, contents)
	baselineStats, err := vector.Fill(ctx, baseline, 7, textEncoder(),
		vector.WithFillScanBatch[int64](5),
		vector.WithFillSplit[int64](vector.SplitOptions{MaxRunes: 3, Overlap: 1}),
	)
	require.NoError(t, err)

	for _, scanBatch := range []int{1, 2, 5} {
		for _, batchSize := range []int{1, 2, 3, 8} {
			for _, batchConcurrency := range []int{0, 1, 2} {
				for _, fillConcurrency := range []int{0, 1, 3} {
					name := fmt.Sprintf("scan=%d/batch=%d/batch-concurrency=%d/fill-concurrency=%d",
						scanBatch, batchSize, batchConcurrency, fillConcurrency)
					t.Run(name, func(t *testing.T) {
						store := newMemStore()
						maps.Copy(store.content, contents)

						stats, err := vector.Fill(ctx, store, 7, textEncoder(),
							vector.WithFillScanBatch[int64](scanBatch),
							vector.WithFillSplit[int64](vector.SplitOptions{MaxRunes: 3, Overlap: 1}),
							vector.WithFillBatch[int64](
								vector.WithBatchSize(batchSize),
								vector.WithBatchConcurrency(batchConcurrency),
							),
							vector.WithFillConcurrency[int64](fillConcurrency),
						)
						require.NoError(t, err)
						assert.Equal(t, baselineStats, stats)
						assert.Equal(t, baseline.vectors, store.vectors)
					})
				}
			}
		}
	}
}

func TestFillCrossDocumentBatchingIsolatesPoisonDocument(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ctx := t.Context()

	store := newMemStore()
	store.content[1] = "fine one"
	store.content[2] = "poison"
	store.content[3] = "fine two"

	var batchSizes []int
	base := poisonEncoder()
	enc := func(ctx context.Context, texts []string) ([][]float32, error) {
		batchSizes = append(batchSizes, len(texts))
		return base(ctx, texts)
	}
	var skipped []int64
	stats, err := vector.Fill(ctx, store, 7, enc,
		vector.WithFillScanBatch[int64](3),
		vector.WithFillBatch[int64](
			vector.WithBatchSize(3), vector.WithBatchConcurrency(1)),
		vector.WithFillConcurrency[int64](1),
		vector.WithFillBatchErrorIsolation[int64](func(error) bool { return true }),
		vector.WithFillEncodeError[int64](func(doc int64, _ error) bool {
			skipped = append(skipped, doc)
			return true
		}),
	)
	require.NoError(err)

	assert.Equal([]int{3, 1, 1, 1}, batchSizes,
		"only a failed cross-document batch is retried at document granularity")
	assert.Equal([]int64{2}, skipped, "the hook is consulted only for the poison document")
	assert.Equal(2, stats.Documents)
	assert.Equal(1, stats.Skipped)
	assert.True(store.embedded[1][7])
	assert.True(store.embedded[2][7])
	assert.Empty(store.vectors[7][2], "the poison document is stamp-only skipped")
	assert.True(store.embedded[3][7])
}

func TestFillCrossDocumentBatchingTranslatesInvalidVectorChunkIndex(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ctx := t.Context()

	store := newMemStore()
	store.content[1] = "x"
	store.content[2] = "abc"
	enc := func(_ context.Context, texts []string) ([][]float32, error) {
		out := make([][]float32, len(texts))
		for i, text := range texts {
			out[i] = []float32{1}
			if text == "c" {
				out[i] = []float32{0}
			}
		}
		return out, nil
	}

	var gotInvalid *vector.InvalidVectorError
	stats, err := vector.Fill(ctx, store, 7, enc,
		vector.WithFillScanBatch[int64](2),
		vector.WithFillSplit[int64](vector.SplitOptions{MaxRunes: 1}),
		vector.WithFillBatch[int64](
			vector.WithBatchSize(2), vector.WithBatchConcurrency(1)),
		vector.WithFillConcurrency[int64](1),
		vector.WithFillEncodeError[int64](func(doc int64, err error) bool {
			assert.Equal(int64(2), doc)
			require.ErrorAs(err, &gotInvalid)
			return true
		}),
	)
	require.NoError(err)
	require.NotNil(gotInvalid)

	assert.Equal(2, gotInvalid.Chunk,
		"the index is relative to the failed document, not the later encode batch")
	assert.Equal(1, stats.Documents)
	assert.Equal(1, stats.Skipped)
}

func TestFillCrossDocumentBatchingLeavesOnlyChangedDocumentPending(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ctx := t.Context()

	store := newMemStore()
	store.content[1] = "alpha"
	store.content[2] = "beta"
	store.revision = map[int64]int{1: 1, 2: 1}
	enc := func(_ context.Context, texts []string) ([][]float32, error) {
		out := make([][]float32, len(texts))
		for i, text := range texts {
			if text == "alpha" {
				store.revision[1]++
			}
			out[i] = []float32{1}
		}
		return out, nil
	}

	stats, err := vector.Fill(ctx, store, 7, enc,
		vector.WithFillScanBatch[int64](2),
		vector.WithFillBatch[int64](
			vector.WithBatchSize(2), vector.WithBatchConcurrency(1)),
		vector.WithFillConcurrency[int64](1),
	)
	require.NoError(err)

	assert.Equal(1, stats.Documents)
	assert.Equal(1, stats.Stale)
	assert.False(store.embedded[1][7])
	assert.True(store.embedded[2][7], "a stale batch mate does not affect this document")
}

func TestFillCrossDocumentBatchingStampsBlankDocuments(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ctx := t.Context()

	store := newMemStore()
	store.content[1] = " \t\n"
	store.content[2] = "alpha"
	store.content[3] = "\u2003"
	var calls atomic.Int64
	enc := func(ctx context.Context, texts []string) ([][]float32, error) {
		calls.Add(1)
		return lenEncoder()(ctx, texts)
	}

	stats, err := vector.Fill(ctx, store, 7, enc,
		vector.WithFillScanBatch[int64](3),
		vector.WithFillBatch[int64](
			vector.WithBatchSize(3), vector.WithBatchConcurrency(1)),
		vector.WithFillConcurrency[int64](1),
	)
	require.NoError(err)

	assert.Equal(int64(1), calls.Load())
	assert.Equal(3, stats.Documents)
	assert.Equal(1, stats.Chunks)
	for doc := int64(1); doc <= 3; doc++ {
		assert.True(store.embedded[doc][7], "doc %d is stamped", doc)
	}
	assert.Empty(store.vectors[7][1])
	assert.Empty(store.vectors[7][3])
}

func TestFillCrossDocumentBatchingRejectsNilEncoderBeforeStampingEmptyDocuments(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	store := newMemStore()
	store.content[1] = ""
	store.content[2] = ""

	stats, err := vector.Fill(t.Context(), store, 7, nil,
		vector.WithFillScanBatch[int64](2),
		vector.WithFillBatch[int64](vector.WithBatchSize(2)),
	)
	require.Error(err)

	assert.Zero(stats.Documents)
	assert.False(store.embedded[1][7])
	assert.False(store.embedded[2][7])
}

func TestFillCrossDocumentBatchingEncodeErrorAbortsAtFailedDocument(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ctx := t.Context()

	store := newMemStore()
	store.content[1] = "fine one"
	store.content[2] = "poison"
	store.content[3] = "fine two"
	var consulted []int64
	_, err := vector.Fill(ctx, store, 7, poisonEncoder(),
		vector.WithFillScanBatch[int64](3),
		vector.WithFillBatch[int64](
			vector.WithBatchSize(3), vector.WithBatchConcurrency(1)),
		vector.WithFillConcurrency[int64](1),
		vector.WithFillBatchErrorIsolation[int64](func(error) bool { return true }),
		vector.WithFillEncodeError[int64](func(doc int64, _ error) bool {
			consulted = append(consulted, doc)
			return false
		}),
	)
	require.ErrorContains(err, "encode document 2")

	assert.Equal([]int64{2}, consulted)
	assert.True(store.embedded[1][7], "the preceding good document is saved")
	assert.False(store.embedded[2][7], "the failed document stays pending")
	assert.False(store.embedded[3][7], "a later document stays pending after abort")
}

func TestFillCrossDocumentBatchingAbortsUnattributedBatchError(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ctx := t.Context()

	store := newMemStore()
	store.content[1] = "fine one"
	store.content[2] = "fine two"
	store.content[3] = "fine three"
	var batchSizes []int
	enc := func(_ context.Context, texts []string) ([][]float32, error) {
		batchSizes = append(batchSizes, len(texts))
		if len(texts) > 1 {
			return nil, errors.New("request shape rejected")
		}
		return [][]float32{{1}}, nil
	}
	called := false
	_, err := vector.Fill(ctx, store, 7, enc,
		vector.WithFillScanBatch[int64](3),
		vector.WithFillBatch[int64](
			vector.WithBatchSize(3), vector.WithBatchConcurrency(1)),
		vector.WithFillConcurrency[int64](1),
		vector.WithFillBatchErrorIsolation[int64](func(error) bool { return true }),
		vector.WithFillEncodeError[int64](func(int64, error) bool {
			called = true
			return true
		}),
	)
	require.ErrorContains(err, "no document failed in isolation")

	assert.Equal([]int{3, 1, 1, 1}, batchSizes)
	assert.False(called, "a batch-shape failure is not attributed to an arbitrary document")
	for doc := int64(1); doc <= 3; doc++ {
		assert.True(store.embedded[doc][7], "doc %d is saved after its successful probe", doc)
		assert.NotEmpty(store.vectors[7][doc], "doc %d keeps its recovered vectors", doc)
	}
}

func TestFillCrossDocumentBatchingDoesNotSkipCancelledEncode(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ctx := t.Context()

	store := newMemStore()
	store.content[1] = "alpha"
	store.content[2] = "beta"
	called := false
	enc := func(context.Context, []string) ([][]float32, error) {
		return nil, context.Canceled
	}
	_, err := vector.Fill(ctx, store, 7, enc,
		vector.WithFillScanBatch[int64](2),
		vector.WithFillBatch[int64](
			vector.WithBatchSize(2), vector.WithBatchConcurrency(1)),
		vector.WithFillConcurrency[int64](1),
		vector.WithFillEncodeError[int64](func(int64, error) bool {
			called = true
			return true
		}),
	)
	require.ErrorIs(err, context.Canceled)

	assert.False(called, "cancellation bypasses the permanent-error hook")
	assert.False(store.embedded[1][7])
	assert.False(store.embedded[2][7])
}

func TestFillLeavesChangedDocumentPending(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ctx := t.Context()

	store := newMemStore()
	store.content[1] = "alpha"
	store.content[2] = "beta"
	store.revision = map[int64]int{1: 1, 2: 1}

	// This encoder simulates a concurrent edit: doc 1's revision is bumped
	// after the scan read its content but before SaveVectors stamps it.
	racingEnc := func(_ context.Context, texts []string) ([][]float32, error) {
		out := make([][]float32, len(texts))
		for i, txt := range texts {
			if txt == "alpha" {
				store.revision[1]++
			}
			out[i] = []float32{1}
		}
		return out, nil
	}

	stats, err := vector.Fill(ctx, store, 7, racingEnc)
	require.NoError(err)
	assert.Equal(1, stats.Documents, "the unchanged doc is embedded")
	assert.Equal(1, stats.Stale, "the changed doc is reported stale")
	assert.False(store.embedded[1][7], "a doc that changed mid-fill is not stamped")
	assert.True(store.embedded[2][7])

	// The next run re-reads the document at its new revision and succeeds.
	again, err := vector.Fill(ctx, store, 7, lenEncoder())
	require.NoError(err)
	assert.Equal(1, again.Documents)
	assert.True(store.embedded[1][7])
}

func TestFillSkipHookStampsFailedDocument(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ctx := t.Context()

	store := newMemStore()
	store.content[1] = "poison"
	store.content[2] = "fine"

	var skipped []int64
	stats, err := vector.Fill(ctx, store, 7, poisonEncoder(),
		vector.WithFillEncodeError[int64](func(doc int64, err error) bool {
			skipped = append(skipped, doc)
			return true
		}),
	)
	require.NoError(err)
	assert.Equal(1, stats.Documents)
	assert.Equal(1, stats.Skipped)
	assert.Equal([]int64{1}, skipped)
	assert.True(store.embedded[1][7], "skipped doc is stamped so it stops being pending")
	assert.Empty(store.vectors[7][1], "skipped doc has no vectors")
	assert.True(store.embedded[2][7])

	again, err := vector.Fill(ctx, store, 7, poisonEncoder())
	require.NoError(err)
	assert.Equal(0, again.Documents, "a stamped skip does not reappear as pending")
}

func TestFillEncodeErrorAbortsWithoutSkip(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ctx := t.Context()

	store := newMemStore()
	store.content[1] = "poison"

	_, err := vector.Fill(ctx, store, 7, poisonEncoder())
	require.ErrorContains(err, "encode document")

	_, err = vector.Fill(ctx, store, 7, poisonEncoder(),
		vector.WithFillEncodeError[int64](func(int64, error) bool { return false }))
	require.ErrorContains(err, "encode document")
	assert.False(store.embedded[1][7], "an aborted doc is neither embedded nor stamped")
}

func TestFillDoesNotSkipCancelledEncode(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ctx := t.Context()

	store := newMemStore()
	store.content[1] = "alpha"

	called := false
	enc := func(context.Context, []string) ([][]float32, error) {
		return nil, context.Canceled
	}
	_, err := vector.Fill(ctx, store, 7, enc,
		vector.WithFillEncodeError[int64](func(int64, error) bool {
			called = true
			return true
		}),
	)
	require.ErrorIs(err, context.Canceled)
	assert.False(called, "cancellation bypasses the permanent-error skip hook")
	assert.False(store.embedded[1][7], "a cancelled document is not stamped as handled")
}

func TestFillDoesNotStampACanceledPreparedPage(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	store := newMemStore()
	store.content[1] = "alpha"
	_, err := vector.Fill(ctx, store, 7, func(context.Context, []string) ([][]float32, error) {
		return [][]float32{{1, 0, 0}}, nil
	}, vector.WithFillPrepared(func(context.Context, vector.Pending[int64]) ([]vector.PreparedChunk, error) {
		cancel()
		return nil, nil
	}))
	require.ErrorIs(err, context.Canceled)
	assert.False(store.embedded[1][7], "a canceled prepared page is not stamped")
}

func TestFillConcurrencyEncodesDocumentsInParallel(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ctx := t.Context()

	const workers = 4
	store := newMemStore()
	for doc := int64(1); doc <= workers; doc++ {
		store.content[doc] = strings.Repeat("x", int(doc))
	}

	// Barrier encoder: every call parks until all four documents are in
	// flight at once, so the test fails with the timeout error below
	// (instead of hanging) if Fill regresses to sequential encodes.
	release := make(chan struct{})
	var arrived atomic.Int32
	enc := func(_ context.Context, texts []string) ([][]float32, error) {
		if arrived.Add(1) == workers {
			close(release)
		}
		select {
		case <-release:
		case <-time.After(5 * time.Second):
			return nil, errors.New("barrier timed out: encodes did not overlap")
		}
		out := make([][]float32, len(texts))
		for i, txt := range texts {
			out[i] = []float32{float32(len([]rune(txt)))}
		}
		return out, nil
	}

	stats, err := vector.Fill(ctx, store, 7, enc,
		vector.WithFillBatch[int64](vector.WithBatchSize(1)),
		vector.WithFillConcurrency[int64](workers))
	require.NoError(err)

	assert.Equal(workers, stats.Documents)
	assert.Equal(workers, stats.Chunks)
	for doc := int64(1); doc <= workers; doc++ {
		require.True(store.embedded[doc][7], "doc %d stamped", doc)
		require.Len(store.vectors[7][doc], 1)
		assert.InDelta(float64(doc), float64(store.vectors[7][doc][0].Vector[0]), 1e-6,
			"doc %d keeps its own vector under concurrent encodes", doc)
	}
}

// saveHookStore runs hook before delegating each SaveVectors to memStore,
// so a test can observe or stretch the save window.
type saveHookStore struct {
	*memStore
	hook func()
}

type firstSaveErrorStore struct {
	*memStore
	err   error
	saves int
}

type observingSaveStore struct {
	*memStore
	saved chan int64
}

func (s *observingSaveStore) SaveVectors(
	ctx context.Context, gen int, doc int64, revision any, vecs []vector.ChunkVector,
) error {
	if err := s.memStore.SaveVectors(ctx, gen, doc, revision, vecs); err != nil {
		return err
	}
	s.saved <- doc
	return nil
}

func (s *firstSaveErrorStore) SaveVectors(
	ctx context.Context, gen int, doc int64, revision any, vecs []vector.ChunkVector,
) error {
	s.saves++
	if s.saves == 1 {
		return s.err
	}
	return s.memStore.SaveVectors(ctx, gen, doc, revision, vecs)
}

func (s *saveHookStore) SaveVectors(ctx context.Context, gen int, doc int64, revision any, vecs []vector.ChunkVector) error {
	s.hook()
	return s.memStore.SaveVectors(ctx, gen, doc, revision, vecs)
}

// TestFillDefaultConcurrencyIsSequential pins the Concurrency <= 0 contract:
// the next document's encode must not begin until the previous document's
// save has returned, so no encoder/API call is ever made ahead of a save
// that may abort the fill. The save window is stretched slightly so a
// pipelined implementation (one encode kept in flight during the save)
// reliably trips the overlap flag; a sequential one runs encode and save on
// one goroutine and can never overlap.
func TestFillDefaultConcurrencyIsSequential(t *testing.T) {
	require := require.New(t)
	ctx := t.Context()

	store := newMemStore()
	for doc := int64(1); doc <= 6; doc++ {
		store.content[doc] = strings.Repeat("x", int(doc))
	}

	var inSave atomic.Bool
	var overlapped atomic.Bool
	enc := func(ctx context.Context, texts []string) ([][]float32, error) {
		if inSave.Load() {
			overlapped.Store(true)
		}
		return lenEncoder()(ctx, texts)
	}
	hooked := &saveHookStore{memStore: store, hook: func() {
		inSave.Store(true)
		time.Sleep(5 * time.Millisecond) //nolint:kennlint // sleeptest: widens the save window a concurrent reader must observe
		inSave.Store(false)
	}}

	stats, err := vector.Fill(ctx, hooked, 7, enc)
	require.NoError(err)
	require.Equal(6, stats.Documents)
	require.False(overlapped.Load(),
		"an encode began while a save was still running: Concurrency <= 0 must be strictly sequential")
}

func TestFillCrossDocumentBatchingDoesNotEncodeNextWindowAfterSaveFailure(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ctx := t.Context()

	base := newMemStore()
	for doc := int64(1); doc <= 7; doc++ {
		base.content[doc] = strings.Repeat("x", int(doc))
	}
	sentinel := errors.New("save failed")
	store := &firstSaveErrorStore{memStore: base, err: sentinel}
	var calls atomic.Int64
	enc := func(ctx context.Context, texts []string) ([][]float32, error) {
		calls.Add(1)
		return lenEncoder()(ctx, texts)
	}

	_, err := vector.Fill(ctx, store, 7, enc,
		vector.WithFillScanBatch[int64](7),
		vector.WithFillBatch[int64](
			vector.WithBatchSize(3), vector.WithBatchConcurrency(1)),
		vector.WithFillConcurrency[int64](1),
	)
	require.ErrorIs(err, sentinel)

	assert.Equal(int64(1), calls.Load(),
		"the next chunk window must not start after the first window reaches a failing save")
}

func TestFillCrossDocumentBatchingComposesConcurrencyBounds(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ctx := t.Context()

	const maxCalls = 4
	store := newMemStore()
	for doc := int64(1); doc <= 8; doc++ {
		store.content[doc] = strings.Repeat("x", int(doc))
	}

	release := make(chan struct{})
	var inFlight, observedMax atomic.Int64
	enc := func(_ context.Context, texts []string) ([][]float32, error) {
		current := inFlight.Add(1)
		for {
			previous := observedMax.Load()
			if current <= previous || observedMax.CompareAndSwap(previous, current) {
				break
			}
		}
		if current == maxCalls {
			close(release)
		}
		select {
		case <-release:
		case <-time.After(5 * time.Second):
			return nil, errors.New("barrier timed out: batched encodes did not overlap")
		}
		defer inFlight.Add(-1)
		out := make([][]float32, len(texts))
		for i := range out {
			out[i] = []float32{1}
		}
		return out, nil
	}

	stats, err := vector.Fill(ctx, store, 7, enc,
		vector.WithFillScanBatch[int64](8),
		vector.WithFillBatch[int64](
			vector.WithBatchSize(2), vector.WithBatchConcurrency(2)),
		vector.WithFillConcurrency[int64](2),
	)
	require.NoError(err)

	assert.Equal(int64(maxCalls), observedMax.Load(),
		"Fill and batch concurrency compose without exceeding their product")
	assert.Equal(8, stats.Documents)
	assert.Equal(8, stats.Chunks)
}

func TestFillCrossDocumentBatchingConcurrentFailureKeepsAttribution(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	store := newMemStore()
	store.content[1] = "poison"
	store.content[2] = "fine one"
	store.content[3] = "fine two"
	store.content[4] = "fine three"

	failedWindowStarted := make(chan struct{})
	laterWindowFinished := make(chan struct{})
	releaseFailedWindow := make(chan struct{})
	enc := func(_ context.Context, texts []string) ([][]float32, error) {
		hasPoison := false
		for _, text := range texts {
			hasPoison = hasPoison || text == "poison"
		}
		if hasPoison {
			if len(texts) > 1 {
				close(failedWindowStarted)
				<-releaseFailedWindow
			}
			return nil, errors.New("content rejected")
		}
		if len(texts) > 1 {
			close(laterWindowFinished)
		}
		out := make([][]float32, len(texts))
		for i := range out {
			out[i] = []float32{1}
		}
		return out, nil
	}

	type fillResult struct {
		stats vector.FillStats
		err   error
	}
	done := make(chan fillResult, 1)
	go func() {
		stats, err := vector.Fill(t.Context(), store, 7, enc,
			vector.WithFillScanBatch[int64](4),
			vector.WithFillBatch[int64](
				vector.WithBatchSize(2), vector.WithBatchConcurrency(1)),
			vector.WithFillConcurrency[int64](2),
			vector.WithFillBatchErrorIsolation[int64](func(error) bool { return true }),
			vector.WithFillEncodeError[int64](func(doc int64, _ error) bool {
				return doc == 1
			}),
		)
		done <- fillResult{stats: stats, err: err}
	}()

	<-failedWindowStarted
	<-laterWindowFinished
	close(releaseFailedWindow)
	result := <-done
	require.NoError(result.err)

	assert.Equal(3, result.stats.Documents)
	assert.Equal(1, result.stats.Skipped)
	for doc := int64(1); doc <= 4; doc++ {
		assert.True(store.embedded[doc][7], "doc %d should be stamped", doc)
	}
}

func TestFillCrossDocumentBatchingDoesNotBlockCompletedSaves(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	base := newMemStore()
	base.content[1] = "slow"
	base.content[2] = "fast"
	store := &observingSaveStore{memStore: base, saved: make(chan int64, 2)}

	slowStarted := make(chan struct{})
	fastFinished := make(chan struct{})
	releaseSlow := make(chan struct{})
	enc := func(_ context.Context, texts []string) ([][]float32, error) {
		if texts[0] == "slow" {
			close(slowStarted)
			<-releaseSlow
		} else {
			close(fastFinished)
		}
		return [][]float32{{1}}, nil
	}

	done := make(chan error, 1)
	go func() {
		_, err := vector.Fill(t.Context(), store, 7, enc,
			vector.WithFillScanBatch[int64](2),
			vector.WithFillBatch[int64](
				vector.WithBatchSize(1), vector.WithBatchConcurrency(2)),
			vector.WithFillConcurrency[int64](2),
		)
		done <- err
	}()
	<-slowStarted
	<-fastFinished

	savedBeforeRelease := false
	select {
	case doc := <-store.saved:
		savedBeforeRelease = doc == 2
	case <-time.After(5 * time.Second):
	}
	close(releaseSlow)
	require.NoError(<-done)

	assert.True(savedBeforeRelease,
		"the completed second document should be saved while the first encoder is still running")
}

func TestFillConcurrencySkipHookStampsFailedDocument(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ctx := t.Context()

	store := newMemStore()
	store.content[1] = "poison"
	store.content[2] = "fine"
	store.content[3] = "also fine"

	var skipped []int64
	stats, err := vector.Fill(ctx, store, 7, poisonEncoder(),
		vector.WithFillConcurrency[int64](3),
		vector.WithFillEncodeError[int64](func(doc int64, err error) bool {
			skipped = append(skipped, doc)
			return true
		}),
	)
	require.NoError(err)
	assert.Equal(2, stats.Documents)
	assert.Equal(1, stats.Skipped)
	assert.Equal([]int64{1}, skipped)
	assert.True(store.embedded[1][7], "skipped doc is stamped so it stops being pending")
	assert.Empty(store.vectors[7][1], "skipped doc has no vectors")
	assert.True(store.embedded[2][7])
	assert.True(store.embedded[3][7])
}

func TestFillConcurrencyEncodeErrorAborts(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ctx := t.Context()

	store := newMemStore()
	store.content[1] = "poison"
	for doc := int64(2); doc <= 8; doc++ {
		store.content[doc] = "fine"
	}

	_, err := vector.Fill(ctx, store, 7, poisonEncoder(),
		vector.WithFillConcurrency[int64](4))
	require.ErrorContains(err, "input rejected by model")
	assert.False(store.embedded[1][7], "the failed doc is neither embedded nor stamped")

	// The failed document stays pending: a later run with a working encoder
	// picks up everything the aborted page left behind.
	again, err := vector.Fill(ctx, store, 7, lenEncoder(),
		vector.WithFillConcurrency[int64](4))
	require.NoError(err)
	assert.True(store.embedded[1][7])
	assert.Equal(0, again.Stale)
}

// poisonEncoder fails any batch containing the text "poison".
func poisonEncoder() vector.EncodeFunc {
	return func(_ context.Context, texts []string) ([][]float32, error) {
		out := make([][]float32, len(texts))
		for i, txt := range texts {
			if strings.Contains(txt, "poison") {
				return nil, errors.New("input rejected by model")
			}
			out[i] = []float32{1}
		}
		return out, nil
	}
}

func TestSearchRollsUpAndPrefersBuildingGeneration(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ctx := t.Context()

	const active, building = 7, 9
	store := newMemStore()
	store.live = []int{building, active} // descending preference

	// Doc 1 is shared; active stored it at chunk 0, building at chunk 5.
	store.SaveVectors(ctx, active, 1, nil, []vector.ChunkVector{{ChunkIndex: 0, Vector: vector.Vector{1, 0}}})
	store.SaveVectors(ctx, active, 2, nil, []vector.ChunkVector{{ChunkIndex: 0, Vector: vector.Vector{0, 1}}})
	store.SaveVectors(ctx, building, 1, nil, []vector.ChunkVector{{ChunkIndex: 5, Vector: vector.Vector{1, 0}}})
	store.SaveVectors(ctx, building, 3, nil, []vector.ChunkVector{{ChunkIndex: 0, Vector: vector.Vector{1, 0}}}) // new, building-only

	// Query vector [1,0] points at docs 1 and 3.
	queryEnc := func(int) vector.EncodeFunc {
		return func(_ context.Context, texts []string) ([][]float32, error) {
			out := make([][]float32, len(texts))
			for i := range texts {
				out[i] = []float32{1, 0}
			}
			return out, nil
		}
	}

	got, err := vector.Search(ctx, store, "q", queryEnc, vector.SearchOptions{})
	require.NoError(err)

	byDoc := map[int64]vector.Hit[int64]{}
	for _, h := range got {
		byDoc[h.Doc] = h
	}
	assert.Contains(byDoc, int64(1))
	assert.Contains(byDoc, int64(2), "active-only doc is not dropped (union coverage)")
	assert.Contains(byDoc, int64(3), "building-only new doc is searchable mid-migration")
	assert.Equal(5, byDoc[1].ChunkIndex, "shared doc keeps the building generation's hit")
}

func TestSearchErrorsWhenNoEncoderForGeneration(t *testing.T) {
	ctx := t.Context()
	store := newMemStore()
	store.live = []int{1}
	store.SaveVectors(ctx, 1, 1, nil, []vector.ChunkVector{{ChunkIndex: 0, Vector: vector.Vector{1}}})

	_, err := vector.Search(ctx, store, "q", func(int) vector.EncodeFunc { return nil }, vector.SearchOptions{})
	assert.ErrorContains(t, err, "no encoder")
}
