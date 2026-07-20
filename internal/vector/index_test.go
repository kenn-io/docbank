package vector

import (
	"cmp"
	"context"
	"errors"
	"math"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	kitvec "go.kenn.io/kit/vector"
)

func TestBuildRefreshesCurrentDocumentsAndActivatesCompleteGeneration(t *testing.T) {
	ctx := t.Context()
	index, err := Open(ctx, filepath.Join(t.TempDir(), "vectors.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, index.Close()) })

	documents := []Document{
		{BlobHash: vectorTestHash("a"), ExtractorVersion: 1, Text: "alpha"},
		{BlobHash: vectorTestHash("b"), ExtractorVersion: 1, Text: "beta"},
	}
	source := documentSource(&documents)
	generation := kitvec.Generation{Model: "test-model", Dimensions: 2,
		Params: map[string]string{"recipe": "1"}}
	var progress []Progress
	result, err := index.Build(ctx, source, generation, testEncoder(2), 8, 2,
		func(event Progress) { progress = append(progress, event) })
	require.NoError(t, err)
	assert.True(t, result.Activated)
	assert.Equal(t, 2, result.Embedded)
	assert.Equal(t, 2, result.Chunks)
	assert.Equal(t, "scanning", progress[0].Phase)
	assert.Equal(t, Progress{Phase: "embedding", Done: 2, Total: 2}, progress[len(progress)-1])

	items, err := index.Generations(ctx)
	require.NoError(t, err)
	require.Len(t, items, 1)
	assert.Equal(t, "active", items[0].State)
	assert.Equal(t, 2, items[0].Embedded)
	assert.Zero(t, items[0].Pending)

	result, err = index.Build(ctx, source, generation, testEncoder(2), 8, 1, nil)
	require.NoError(t, err)
	assert.Zero(t, result.Embedded)

	documents = []Document{{
		BlobHash: vectorTestHash("a"), ExtractorVersion: 2, Text: "changed",
	}}
	result, err = index.Build(ctx, source, generation, testEncoder(2), 8, 1, nil)
	require.NoError(t, err)
	assert.Equal(t, 1, result.Embedded)
	assert.Equal(t, 1, result.Removed)
	items, err = index.Generations(ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, items[0].Embedded)
	assert.Zero(t, items[0].Pending)

	documents[0].Text = "corrected extraction"
	result, err = index.Build(ctx, source, generation, testEncoder(2), 8, 1, nil)
	require.NoError(t, err)
	assert.Equal(t, 1, result.Embedded,
		"corrected text must invalidate vectors even when extractor version is unchanged")
}

func TestBuildRetiresPriorGenerationOnlyAfterReplacementCompletes(t *testing.T) {
	ctx := t.Context()
	index, err := Open(ctx, filepath.Join(t.TempDir(), "vectors.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, index.Close()) })
	documents := []Document{{
		BlobHash: vectorTestHash("a"), ExtractorVersion: 1, Text: "alpha",
	}}
	source := documentSource(&documents)
	first := kitvec.Generation{Model: "first", Dimensions: 2}
	second := kitvec.Generation{Model: "second", Dimensions: 3}

	_, err = index.Build(ctx, source, first, testEncoder(2), 8, 1, nil)
	require.NoError(t, err)
	_, err = index.Build(ctx, source, second,
		func(context.Context, []string) ([][]float32, error) {
			return nil, errors.New("encoder unavailable")
		}, 8, 1, nil)
	require.ErrorContains(t, err, "encoder unavailable")
	items, err := index.Generations(ctx)
	require.NoError(t, err)
	require.Len(t, items, 2)
	assert.Equal(t, "building", items[0].State)
	assert.Equal(t, "active", items[1].State,
		"a failed replacement must not retire the searchable generation")

	_, err = index.Build(ctx, source, second, testEncoder(3), 8, 1, nil)
	require.NoError(t, err)
	items, err = index.Generations(ctx)
	require.NoError(t, err)
	require.Len(t, items, 2)
	assert.Equal(t, "active", items[0].State)
	assert.Equal(t, "second", items[0].Model)
	assert.Equal(t, "retired", items[1].State)
}

func TestBuildRejectsConcurrentRun(t *testing.T) {
	ctx := t.Context()
	index, err := Open(ctx, filepath.Join(t.TempDir(), "vectors.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, index.Close()) })
	documents := []Document{{
		BlobHash: vectorTestHash("a"), ExtractorVersion: 1, Text: "alpha",
	}}
	started := make(chan struct{})
	release := make(chan struct{})
	encoder := func(ctx context.Context, _ []string) ([][]float32, error) {
		close(started)
		select {
		case <-release:
			return [][]float32{{1, 0}}, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	generation := kitvec.Generation{Model: "test", Dimensions: 2}
	errCh := make(chan error, 1)
	go func() {
		_, buildErr := index.Build(ctx, documentSource(&documents), generation, encoder, 8, 1, nil)
		errCh <- buildErr
	}()
	<-started
	_, err = index.Build(ctx, documentSource(&documents), generation, testEncoder(2), 8, 1, nil)
	require.ErrorIs(t, err, ErrBuildRunning)
	close(release)
	require.NoError(t, <-errCh)
}

func documentSource(documents *[]Document) SourceFunc {
	return func(_ context.Context, after string, limit int) ([]Document, error) {
		items := slices.Clone(*documents)
		slices.SortFunc(items, func(a, b Document) int { return cmp.Compare(a.BlobHash, b.BlobHash) })
		page := make([]Document, 0, limit)
		for _, item := range items {
			if item.BlobHash > after && len(page) < limit {
				page = append(page, item)
			}
		}
		return page, nil
	}
}

func vectorTestHash(digit string) string { return strings.Repeat(digit, 64) }

func testEncoder(dimensions int) kitvec.EncodeFunc {
	return func(_ context.Context, texts []string) ([][]float32, error) {
		vectors := make([][]float32, len(texts))
		for i, text := range texts {
			vector := make([]float32, dimensions)
			vector[0] = 1
			if dimensions > 1 {
				vector[1] = float32(len(text)) / 100
			}
			var sum float64
			for _, value := range vector {
				sum += float64(value * value)
			}
			scale := float32(1 / math.Sqrt(sum))
			for j := range vector {
				vector[j] *= scale
			}
			vectors[i] = vector
		}
		return vectors, nil
	}
}
