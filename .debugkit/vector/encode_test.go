package vector_test

import (
	"context"
	"errors"
	"math"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/kit/vector"
)

func chunks(texts ...string) []vector.Chunk {
	out := make([]vector.Chunk, len(texts))
	for i, txt := range texts {
		out[i] = vector.Chunk{Index: i, Text: txt}
	}
	return out
}

// echoEncoder returns one vector per text whose single component encodes
// the text length, so results can be matched back to their input order.
func echoEncoder(record func(batch []string)) vector.EncodeFunc {
	return func(_ context.Context, texts []string) ([][]float32, error) {
		if record != nil {
			record(texts)
		}
		out := make([][]float32, len(texts))
		for i, txt := range texts {
			out[i] = []float32{float32(len(txt))}
		}
		return out, nil
	}
}

func TestEncodeBatchedPreservesOrderAcrossBatches(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	var mu sync.Mutex
	var sizes []int
	enc := echoEncoder(func(batch []string) {
		mu.Lock()
		sizes = append(sizes, len(batch))
		mu.Unlock()
	})

	in := chunks("a", "bb", "ccc", "dddd", "eeeee")
	out, err := vector.EncodeBatched(t.Context(), enc, in,
		vector.WithBatchSize(2), vector.WithBatchConcurrency(3))
	require.NoError(err)
	require.Len(out, len(in))
	for i, c := range in {
		assert.InDelta(float32(len(c.Text)), out[i][0], 1e-6, "vector %d matches its input", i)
	}

	mu.Lock()
	defer mu.Unlock()
	assert.ElementsMatch([]int{2, 2, 1}, sizes, "batches are sized by BatchSize")
}

func TestEncodeBatchedCapsBatchSizeByTokenBudget(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	var batches [][]string
	enc := echoEncoder(func(batch []string) {
		batches = append(batches, append([]string(nil), batch...))
	})
	in := chunks("a", "bb", "ccc", "dddd")

	out, err := vector.EncodeBatched(t.Context(), enc, in,
		vector.WithBatchSize(4),
		vector.WithBatchTokenBudget(120_000, 32_000),
	)

	require.NoError(err)
	require.Len(out, len(in))
	assert.Equal([][]string{{"a", "bb", "ccc"}, {"dddd"}}, batches)
}

func TestEncodeBatchedRejectsInputAboveTokenBudget(t *testing.T) {
	var calls atomic.Int64
	enc := echoEncoder(func([]string) { calls.Add(1) })

	_, err := vector.EncodeBatched(t.Context(), enc, chunks("a"),
		vector.WithBatchTokenBudget(31_999, 32_000),
	)

	require.Error(t, err)
	require.
		ErrorContains(t, err, "token budget")
	assert.Zero(t, calls.Load(), "a known-oversized input is rejected before the provider call")
}

func TestEncodeBatchedRespectsConcurrencyBound(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	var inFlight, maxInFlight atomic.Int64
	enc := func(_ context.Context, texts []string) ([][]float32, error) {
		cur := inFlight.Add(1)
		for {
			prev := maxInFlight.Load()
			if cur <= prev || maxInFlight.CompareAndSwap(prev, cur) {
				break
			}
		}
		defer inFlight.Add(-1)
		out := make([][]float32, len(texts))
		for i := range out {
			out[i] = []float32{1}
		}
		return out, nil
	}

	in := chunks("a", "b", "c", "d", "e", "f", "g", "h")
	_, err := vector.EncodeBatched(t.Context(), enc, in,
		vector.WithBatchSize(1), vector.WithBatchConcurrency(2))
	require.NoError(err)
	assert.LessOrEqual(maxInFlight.Load(), int64(2), "never exceeds the concurrency bound")
}

func TestEncodeBatchedSurfacesEncodeError(t *testing.T) {
	assert := assert.New(t)
	sentinel := errors.New("boom")
	enc := func(_ context.Context, _ []string) ([][]float32, error) { return nil, sentinel }

	_, err := vector.EncodeBatched(t.Context(), enc, chunks("a", "b"), vector.WithBatchSize(1))
	assert.ErrorIs(err, sentinel)
}

func TestEncodeBatchedDoesNotLaunchBatchAfterBlockedDispatchSeesError(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	sentinel := errors.New("boom")
	firstStarted := make(chan struct{})
	releaseFirst := make(chan struct{})
	var calls atomic.Int64
	enc := func(_ context.Context, texts []string) ([][]float32, error) {
		call := calls.Add(1)
		if call == 1 {
			close(firstStarted)
			<-releaseFirst
			return nil, sentinel
		}
		return make([][]float32, len(texts)), nil
	}

	done := make(chan error, 1)
	go func() {
		_, err := vector.EncodeBatched(t.Context(), enc, chunks("a", "b", "c"),
			vector.WithBatchSize(1), vector.WithBatchConcurrency(1))
		done <- err
	}()

	<-firstStarted
	for range 1000 {
		runtime.Gosched()
	}
	close(releaseFirst)

	err := <-done
	require.ErrorIs(err, sentinel)
	assert.Equal(int64(1), calls.Load(), "no later batch is launched after the blocked dispatch observes the first error")
}

func TestEncodeBatchedRejectsCountMismatch(t *testing.T) {
	assert := assert.New(t)
	enc := func(_ context.Context, _ []string) ([][]float32, error) {
		return [][]float32{{1}}, nil // one vector for two texts
	}

	_, err := vector.EncodeBatched(t.Context(), enc, chunks("a", "b"))
	assert.ErrorContains(err, "vectors for")
}

func TestEncodeBatchedRejectsNonFiniteComponent(t *testing.T) {
	for name, bad := range map[string]float32{
		"NaN":  float32(math.NaN()),
		"+Inf": float32(math.Inf(1)),
		"-Inf": float32(math.Inf(-1)),
	} {
		t.Run(name, func(t *testing.T) {
			enc := func(_ context.Context, texts []string) ([][]float32, error) {
				out := make([][]float32, len(texts))
				for i := range texts {
					out[i] = []float32{1, 2}
				}
				out[len(out)-1] = []float32{1, bad}
				return out, nil
			}

			_, err := vector.EncodeBatched(t.Context(), enc, chunks("a", "b", "c"))
			var invalid *vector.InvalidVectorError
			require.ErrorAs(t, err, &invalid)
			assert.Equal(t, 2, invalid.Chunk, "chunk index is global, not batch-relative")
			assert.Equal(t, 1, invalid.Component)
		})
	}
}

func TestEncodeBatchedRejectsZeroNormVector(t *testing.T) {
	enc := func(_ context.Context, texts []string) ([][]float32, error) {
		out := make([][]float32, len(texts))
		for i, txt := range texts {
			if txt == "c" {
				out[i] = []float32{0, 0}
			} else {
				out[i] = []float32{1, 2}
			}
		}
		return out, nil
	}

	// BatchSize 2 puts the zero vector in the second batch, so a
	// batch-relative index would wrongly report 0.
	_, err := vector.EncodeBatched(t.Context(), enc, chunks("a", "b", "c"), vector.WithBatchSize(2))
	var invalid *vector.InvalidVectorError
	require.ErrorAs(t, err, &invalid)
	assert.Equal(t, 2, invalid.Chunk)
	assert.Equal(t, -1, invalid.Component, "zero norm reports no single component")
}

func TestEncodeBatchedNilEncoder(t *testing.T) {
	_, err := vector.EncodeBatched(t.Context(), nil, chunks("a"))
	assert.Error(t, err)
}

func TestEncodeBatchedEmptyInput(t *testing.T) {
	out, err := vector.EncodeBatched(t.Context(), echoEncoder(nil), nil)
	require.NoError(t, err)
	assert.Empty(t, out)
}

func TestEncodeBatchedRejectsWhitespaceBeforeCallingEncoder(t *testing.T) {
	var calls atomic.Int64
	enc := echoEncoder(func([]string) { calls.Add(1) })

	_, err := vector.EncodeBatched(t.Context(), enc, chunks("alpha", " \t\n\u2003"),
		vector.WithBatchSize(1), vector.WithBatchConcurrency(2))

	require.ErrorIs(t, err, vector.ErrEmptyEmbeddingInput)
	assert.Zero(t, calls.Load(), "the batch is validated before any provider call")
}

// TestEncodeBatchedRejectsInvisibleTextBeforeCallingEncoder covers text a
// provider sees as non-empty while it carries nothing to embed: zero-width
// and formatting runes are not unicode.IsSpace, so trimming whitespace alone
// would let this chunk through and leave the provider to reject the call.
func TestEncodeBatchedRejectsInvisibleTextBeforeCallingEncoder(t *testing.T) {
	var calls atomic.Int64
	enc := echoEncoder(func([]string) { calls.Add(1) })

	_, err := vector.EncodeBatched(t.Context(), enc,
		chunks("alpha", "\u200b\ufeff\u200d"),
		vector.WithBatchSize(1), vector.WithBatchConcurrency(2))

	require.ErrorIs(t, err, vector.ErrEmptyEmbeddingInput)
	assert.Zero(t, calls.Load(), "the batch is validated before any provider call")
}

func TestEncodeBatchedStopsOnCancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := vector.EncodeBatched(ctx, echoEncoder(nil), chunks("a", "b"), vector.WithBatchSize(1))
	assert.ErrorIs(t, err, context.Canceled)
}

func TestEncodeOneReturnsTheSingleVector(t *testing.T) {
	require := require.New(t)
	ctx := t.Context()
	var batches [][]string
	vec, err := vector.EncodeOne(ctx, echoEncoder(func(batch []string) { batches = append(batches, batch) }), "hello")
	require.NoError(err)
	require.Len(vec, 1)
	require.Equal([][]string{{"hello"}}, batches)

	_, err = vector.EncodeOne(ctx, echoEncoder(nil), "   ")
	require.ErrorIs(err, vector.ErrEmptyEmbeddingInput)
}
