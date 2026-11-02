package vector_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/kit/vector"
)

func TestFillDocumentLimitStopsAfterStartedDocuments(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	store := newMemStore()
	for id := int64(1); id <= 5; id++ {
		store.content[id] = "doc"
	}

	stats, err := vector.Fill(t.Context(), store, 1, lenEncoder(),
		vector.WithFillScanBatch[int64](2),
		vector.WithFillDocumentLimit[int64](3),
	)
	require.NoError(err)
	assert.Equal(3, stats.Documents)
	assert.Len(store.embedded, 3)
	for id := int64(1); id <= 3; id++ {
		assert.True(store.embedded[id][1], "document %d was started", id)
	}
	assert.Nil(store.embedded[4])
	assert.Nil(store.embedded[5])

	stats, err = vector.Fill(t.Context(), store, 1, lenEncoder(),
		vector.WithFillDocumentLimit[int64](3),
	)
	require.NoError(err)
	assert.Equal(2, stats.Documents)
	assert.True(store.embedded[4][1])
	assert.True(store.embedded[5][1])
}

func TestFillDocumentLimitCountsAStaleDocumentAsStarted(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	store := newMemStore()
	store.revision = map[int64]int{1: 1, 2: 1}
	store.content[1] = "one"
	store.content[2] = "two"
	var progressed []int64
	enc := func(ctx context.Context, texts []string) ([][]float32, error) {
		store.revision[1] = 9
		return lenEncoder()(ctx, texts)
	}

	stats, err := vector.Fill(t.Context(), store, 1, enc,
		vector.WithFillDocumentLimit[int64](1),
		vector.WithFillProgress[int64](func(progress vector.FillProgress[int64]) {
			progressed = append(progressed, progress.Doc)
		}),
	)
	require.NoError(err)
	assert.Equal(1, stats.Stale)
	assert.Zero(stats.Documents)
	assert.Empty(progressed, "a stale save is not progress")
	assert.Nil(store.embedded[1])
	assert.Nil(store.embedded[2])
}

func TestFillProgressReportsSavedChunks(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	store := newMemStore()
	store.content[1] = "cat"
	store.content[2] = "   "
	var got []vector.FillProgress[int64]

	stats, err := vector.Fill(t.Context(), store, 1, lenEncoder(),
		vector.WithFillProgress[int64](func(progress vector.FillProgress[int64]) {
			got = append(got, progress)
		}),
	)
	require.NoError(err)
	assert.Equal(2, stats.Documents)
	require.Len(got, 2)
	assert.Equal(int64(1), got[0].Doc)
	assert.Equal(1, got[0].Vectors)
	require.Len(got[0].Chunks, 1)
	assert.Equal("cat", got[0].Chunks[0].Text)
	assert.Nil(got[0].Chunks[0].Span)
	assert.Equal(int64(2), got[1].Doc)
	assert.Zero(got[1].Vectors)
	assert.Empty(got[1].Chunks)
}

func TestFillPreparedChunksKeepSourceSpanAndEncoderText(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	store := newMemStore()
	store.content[1] = "source"
	store.content[2] = "other"
	var encoded []string
	enc := func(ctx context.Context, texts []string) ([][]float32, error) {
		encoded = append(encoded, texts...)
		return lenEncoder()(ctx, texts)
	}
	span := &vector.SourceSpan{Start: 0, End: 6}
	var progressed []vector.FillProgress[int64]

	ctx := t.Context()
	stats, err := vector.Fill(ctx, store, 1, enc,
		vector.WithFillBatch[int64](vector.WithBatchSize(10)),
		vector.WithFillPrepared[int64](func(got context.Context, pending vector.Pending[int64]) ([]vector.PreparedChunk, error) {
			require.Equal(ctx, got)
			if pending.Doc == 2 {
				return nil, nil
			}
			return []vector.PreparedChunk{{
				Index:     4,
				Text:      "heading\n" + pending.Content,
				Span:      span,
				Truncated: true,
			}}, nil
		}),
		vector.WithFillProgress[int64](func(progress vector.FillProgress[int64]) {
			progressed = append(progressed, progress)
		}),
	)
	require.NoError(err)
	assert.Equal(2, stats.Documents)
	assert.Equal(1, stats.Chunks)
	assert.Equal([]string{"heading\nsource"}, encoded)
	require.Len(store.vectors[1][1], 1)
	assert.Equal(4, store.vectors[1][1][0].ChunkIndex)
	assert.Empty(store.vectors[1][2], "an empty prepared list is a stamp-only save")
	assert.True(store.embedded[2][1])

	require.Len(progressed, 2)
	assert.Equal(int64(1), progressed[0].Doc)
	require.Len(progressed[0].Chunks, 1)
	assert.Equal("heading\nsource", progressed[0].Chunks[0].Text)
	assert.Equal(span, progressed[0].Chunks[0].Span)
	assert.True(progressed[0].Chunks[0].Truncated)
	assert.Equal(4, progressed[0].Chunks[0].Index)
	assert.Zero(progressed[1].Vectors)
}

func TestFillPreparedErrorStampsNothing(t *testing.T) {
	require := require.New(t)
	store := newMemStore()
	store.content[1] = "one"
	store.content[2] = "two"
	prepareErr := errors.New("prepare failed")
	calls := 0
	enc := func(context.Context, []string) ([][]float32, error) {
		calls++
		return nil, nil
	}

	_, err := vector.Fill(t.Context(), store, 1, enc,
		vector.WithFillPrepared[int64](func(_ context.Context, pending vector.Pending[int64]) ([]vector.PreparedChunk, error) {
			if pending.Doc == 2 {
				return nil, prepareErr
			}
			return []vector.PreparedChunk{{Index: 0, Text: pending.Content}}, nil
		}),
	)
	require.ErrorIs(err, prepareErr)
	assert.Zero(t, calls)
	assert.Empty(t, store.embedded)
}

func TestFillPreparedBlankTextIsAnEncodeError(t *testing.T) {
	require := require.New(t)
	store := newMemStore()
	store.content[1] = "source"
	_, err := vector.Fill(t.Context(), store, 1, lenEncoder(),
		vector.WithFillPrepared[int64](func(context.Context, vector.Pending[int64]) ([]vector.PreparedChunk, error) {
			return []vector.PreparedChunk{{Index: 0, Text: " \n"}}, nil
		}),
	)
	require.ErrorIs(err, vector.ErrEmptyEmbeddingInput)
	assert.Empty(t, store.embedded)
}

func TestFillNegativeDocumentLimitDoesNotStopEarly(t *testing.T) {
	require := require.New(t)
	store := newMemStore()
	store.content[1] = "a"
	store.content[2] = "b"
	stats, err := vector.Fill(t.Context(), store, 1, lenEncoder(),
		vector.WithFillDocumentLimit[int64](-1),
	)
	require.NoError(err)
	assert.Equal(t, 2, stats.Documents)
}

// cancelOnPendingScan cancels after the pending scan returns so the page
// loop observes a context that is already done.
type cancelOnPendingScan struct {
	*memStore
	cancel context.CancelFunc
}

func (s cancelOnPendingScan) PendingForGeneration(ctx context.Context, gen int, limit int) ([]vector.Pending[int64], error) {
	pending, err := s.memStore.PendingForGeneration(ctx, gen, limit)
	s.cancel()
	return pending, err
}

func TestFillPreparedPassesFillContextAndStopsWhenCancelled(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	store := newMemStore()
	store.content[1] = "one"
	store.content[2] = "two"
	store.content[3] = "three"
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	var calls []int64
	_, err := vector.Fill(ctx, store, 1, lenEncoder(),
		vector.WithFillPrepared[int64](func(got context.Context, pending vector.Pending[int64]) ([]vector.PreparedChunk, error) {
			require.Equal(ctx, got)
			calls = append(calls, pending.Doc)
			if pending.Doc == 1 {
				cancel()
			}
			return []vector.PreparedChunk{{Index: 0, Text: pending.Content}}, nil
		}),
	)
	require.ErrorIs(err, context.Canceled)
	assert.Equal([]int64{1}, calls)
	assert.Empty(store.embedded)
}

func TestFillPreparedSkipsCallbackWhenPageContextIsAlreadyCancelled(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	base := newMemStore()
	base.content[1] = "one"
	base.content[2] = "two"
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	calls := 0
	_, err := vector.Fill(ctx, cancelOnPendingScan{memStore: base, cancel: cancel}, 1, lenEncoder(),
		vector.WithFillPrepared[int64](func(context.Context, vector.Pending[int64]) ([]vector.PreparedChunk, error) {
			calls++
			return []vector.PreparedChunk{{Index: 0, Text: "kept"}}, nil
		}),
	)
	require.ErrorIs(err, context.Canceled)
	assert.Zero(calls)
	assert.Empty(base.embedded)
}

func TestFillRejectsBadPreparedIndexesBeforeEncoding(t *testing.T) {
	for _, tc := range []struct {
		name    string
		indexes []int
		want    string
	}{
		{name: "repeated", indexes: []int{0, 0}, want: "prepared chunk index 0 repeats"},
		{name: "negative", indexes: []int{-1}, want: "prepared chunk index -1 is negative"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := newMemStore()
			store.content[1] = "doc"
			encoded := 0
			enc := func(ctx context.Context, texts []string) ([][]float32, error) {
				encoded++
				return lenEncoder()(ctx, texts)
			}
			_, err := vector.Fill(t.Context(), store, 1, enc,
				vector.WithFillPrepared(func(context.Context, vector.Pending[int64]) ([]vector.PreparedChunk, error) {
					chunks := make([]vector.PreparedChunk, len(tc.indexes))
					for i, index := range tc.indexes {
						chunks[i] = vector.PreparedChunk{Index: index, Text: "text"}
					}
					return chunks, nil
				}),
			)
			require.ErrorContains(t, err, "prepare document 1: "+tc.want)
			assert.Zero(t, encoded, "a bad index fails before the encode is paid for")
			assert.Nil(t, store.embedded[1])
		})
	}
}

func TestFillBatchesAcrossDocumentsByDefault(t *testing.T) {
	store := newMemStore()
	for id := int64(1); id <= 3; id++ {
		store.content[id] = "doc"
	}
	var sizes []int
	enc := func(ctx context.Context, texts []string) ([][]float32, error) {
		sizes = append(sizes, len(texts))
		return lenEncoder()(ctx, texts)
	}
	stats, err := vector.Fill(t.Context(), store, 1, enc)
	require.NoError(t, err)
	assert.Equal(t, 3, stats.Documents)
	assert.Equal(t, []int{3}, sizes, "one encode call carries every document in the page")
}

func TestFillStillSkipsOneBadDocumentByDefault(t *testing.T) {
	store := newMemStore()
	store.content[1] = "fine"
	store.content[2] = "poison"
	store.content[3] = "fine"
	var skipped []int64
	stats, err := vector.Fill(t.Context(), store, 1, poisonEncoder(),
		vector.WithFillEncodeError[int64](func(doc int64, _ error) bool {
			skipped = append(skipped, doc)
			return true
		}),
	)
	require.NoError(t, err)
	assert.Equal(t, []int64{2}, skipped, "the shared failure is traced to the one bad document")
	assert.Equal(t, 1, stats.Skipped)
	assert.True(t, store.embedded[1][1])
	assert.True(t, store.embedded[3][1])
}

func TestFillPacksEncodeCallsByCountAndEstimatedTokens(t *testing.T) {
	for _, tc := range []struct {
		name    string
		docs    []string
		options []vector.BatchOption
		want    []int
	}{
		{
			name: "at most 32 chunks per call",
			docs: repeatDocs(40, "short text"),
			want: []int{32, 8},
		},
		{
			// 6000 CJK runes estimate 6000 tokens each, so two fit under
			// 16384. A flat runes/4 estimate would pack all three.
			name: "CJK text counts one token per rune",
			docs: repeatDocs(3, strings.Repeat("語", 6000)),
			want: []int{2, 1},
		},
		{
			name: "a chunk over the budget goes alone",
			docs: repeatDocs(2, strings.Repeat("a", 80000)),
			want: []int{1, 1},
		},
		{
			name:    "a caller budget replaces the estimate",
			docs:    repeatDocs(3, strings.Repeat("語", 6000)),
			options: []vector.BatchOption{vector.WithBatchTokenBudget(1<<20, 1)},
			want:    []int{3},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := newMemStore()
			for i, text := range tc.docs {
				store.content[int64(i+1)] = text
			}
			var sizes []int
			enc := func(ctx context.Context, texts []string) ([][]float32, error) {
				sizes = append(sizes, len(texts))
				return lenEncoder()(ctx, texts)
			}
			options := []vector.FillOption[int64]{vector.WithFillScanBatch[int64](len(tc.docs))}
			if tc.options != nil {
				options = append(options, vector.WithFillBatch[int64](tc.options...))
			}
			_, err := vector.Fill(t.Context(), store, 1, enc, options...)
			require.NoError(t, err)
			assert.Equal(t, tc.want, sizes)
		})
	}
}

func repeatDocs(n int, text string) []string {
	docs := make([]string, n)
	for i := range docs {
		docs[i] = text
	}
	return docs
}
