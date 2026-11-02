package vector

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type internalFillProviderError struct{}

func (*internalFillProviderError) Error() string { return "input rejected" }

type internalFillCompanionError struct{}

func (*internalFillCompanionError) Error() string { return "companion provider error" }

type noOpFillStore struct{}

func (noOpFillStore) PendingForGeneration(context.Context, int, int) ([]Pending[int64], error) {
	return nil, nil
}

func (noOpFillStore) SaveVectors(context.Context, int, int64, any, []ChunkVector) error {
	return nil
}

func (noOpFillStore) LiveGenerations(context.Context) ([]int, error) { return nil, nil }

func (noOpFillStore) QueryGeneration(context.Context, int, Vector, int) ([]Hit[int64], error) {
	return nil, nil
}

func TestApplyFillBatchProbeInvalidVectorAddsSliceAndLocalOffsets(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	refs := []fillChunkRef{
		{doc: 0, chunk: 3, value: Chunk{Index: 3, Text: "d"}},
		{doc: 0, chunk: 4, value: Chunk{Index: 4, Text: "e"}},
		{doc: 1, chunk: 0, value: Chunk{Index: 0, Text: "z"}},
	}
	states := []fillDocumentState[int64]{
		{
			encoded: fillEncoded[int64]{
				doc:     Pending[int64]{Doc: 10},
				chunks:  []Chunk{{Index: 0}, {Index: 1}, {Index: 2}, {Index: 3}, {Index: 4}},
				vectors: []Vector{{1}, {1}, {1}, nil, nil},
			},
			remaining: 2,
		},
		{
			encoded: fillEncoded[int64]{
				doc: Pending[int64]{Doc: 20}, chunks: []Chunk{{Index: 0}}, vectors: make([]Vector, 1),
			},
			remaining: 1,
		},
	}
	var calls int
	enc := func(_ context.Context, texts []string) ([][]float32, error) {
		calls++
		if calls == 1 {
			assert.Equal([]string{"d", "e", "z"}, texts)
			return nil, &internalFillProviderError{}
		}
		assert.Equal([]string{"d", "e"}, texts)
		return [][]float32{{1}, {0}}, nil
	}
	batch := encodeFillBatch(t.Context(), enc, refs)
	var got *InvalidVectorError
	err := applyFillBatch(t.Context(), noOpFillStore{}, 7,
		fillOptions[int64]{
			shouldIsolateBatchError: func(err error) bool {
				var providerErr *internalFillProviderError
				require.ErrorAs(err, &providerErr)
				return true
			},
			onEncodeError: func(doc int64, err error) bool {
				assert.Equal(int64(10), doc)
				require.ErrorAs(err, &got)
				return false
			},
		},
		enc, batch, states, true, map[int64]struct{}{}, &FillStats{})
	require.Error(err)
	require.NotNil(got)
	assert.Equal(4, got.Chunk, "slice start 3 plus local invalid index 1")
	assert.Equal(2, calls)
	assert.ErrorAs(err, &got)
}

func TestApplyFillBatchProbeInvalidVectorPreservesCompanionCauses(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	refs := []fillChunkRef{
		{doc: 0, chunk: 3, value: Chunk{Index: 3, Text: "d"}},
		{doc: 0, chunk: 4, value: Chunk{Index: 4, Text: "e"}},
		{doc: 1, chunk: 0, value: Chunk{Index: 0, Text: "z"}},
	}
	states := []fillDocumentState[int64]{
		{
			encoded: fillEncoded[int64]{
				doc:     Pending[int64]{Doc: 10},
				chunks:  []Chunk{{Index: 0}, {Index: 1}, {Index: 2}, {Index: 3}, {Index: 4}},
				vectors: []Vector{{1}, {1}, {1}, nil, nil},
			},
			remaining: 2,
		},
		{
			encoded: fillEncoded[int64]{
				doc: Pending[int64]{Doc: 20}, chunks: []Chunk{{Index: 0}}, vectors: make([]Vector, 1),
			},
			remaining: 1,
		},
	}
	companion := &internalFillCompanionError{}
	sentinel := errors.New("companion sentinel")
	var calls int
	enc := func(_ context.Context, texts []string) ([][]float32, error) {
		calls++
		if calls == 1 {
			assert.Equal([]string{"d", "e", "z"}, texts)
			return nil, &internalFillProviderError{}
		}
		assert.Equal([]string{"d", "e"}, texts)
		return nil, errors.Join(
			&InvalidVectorError{Chunk: 1, Component: -1, Reason: "zero norm"},
			companion,
			sentinel,
		)
	}
	batch := encodeFillBatch(t.Context(), enc, refs)
	var gotInvalid *InvalidVectorError
	err := applyFillBatch(t.Context(), noOpFillStore{}, 7,
		fillOptions[int64]{
			shouldIsolateBatchError: func(err error) bool {
				var providerErr *internalFillProviderError
				require.ErrorAs(err, &providerErr)
				return true
			},
			onEncodeError: func(doc int64, err error) bool {
				assert.Equal(int64(10), doc)
				require.ErrorAs(err, &gotInvalid)
				assert.Equal(4, gotInvalid.Chunk)
				var gotCompanion *internalFillCompanionError
				require.ErrorAs(err, &gotCompanion)
				assert.Same(companion, gotCompanion)
				assert.ErrorIs(err, sentinel)
				return false
			},
		},
		enc, batch, states, true, map[int64]struct{}{}, &FillStats{})
	require.Error(err)
	require.NotNil(gotInvalid)
	assert.Equal(4, gotInvalid.Chunk)
	assert.Equal(2, calls)
	var gotCompanion *internalFillCompanionError
	require.ErrorAs(err, &gotCompanion)
	assert.Same(companion, gotCompanion)
	assert.ErrorIs(err, sentinel)
}
