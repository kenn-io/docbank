package transfer

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDiskRelationFindsDuplicatesAndMissingKeysAcrossRuns(t *testing.T) {
	index, err := newValidationIndex()
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, index.Close()) })
	relation := index.relation("sources")
	for number := range validationRunKeys + 1 {
		key := fmt.Sprintf("source-%08d", number)
		require.NoError(t, relation.addDefinition(key))
		require.NoError(t, relation.addReference(key))
	}
	require.NoError(t, relation.addDefinition("source-00000000"))
	require.NoError(t, relation.addReference("source-missing"))

	result, err := relation.validate(t.Context(), false)
	require.NoError(t, err)
	require.True(t, result.Duplicate)
	require.True(t, result.Missing)
	require.False(t, result.Unexpected)
}

func TestDiskRelationCanRequireAnExactSet(t *testing.T) {
	index, err := newValidationIndex()
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, index.Close()) })
	relation := index.relation("blobs")
	require.NoError(t, relation.addDefinition("present-but-unreferenced"))

	result, err := relation.validate(t.Context(), true)
	require.NoError(t, err)
	require.True(t, result.Unexpected)
}

func TestDiskRelationMergeHonorsCancellation(t *testing.T) {
	index, err := newValidationIndex()
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, index.Close()) })
	relation := index.relation("records")
	require.NoError(t, relation.addDefinition("record-1"))
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	_, err = relation.validate(ctx, false)
	require.ErrorIs(t, err, context.Canceled)
}

func TestDiskRelationUsesBoundedMultipassMergeFanIn(t *testing.T) {
	index, err := newValidationIndexWithOptions(validationIndexOptions{runKeys: 2, mergeFanIn: 2})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, index.Close()) })
	relation := index.relation("records")
	for number := range 11 {
		key := fmt.Sprintf("record-%02d", number)
		require.NoError(t, relation.addDefinition(key))
		require.NoError(t, relation.addReference(key))
	}
	require.NoError(t, relation.addDefinition("record-00"))

	result, err := relation.validate(t.Context(), true)
	require.NoError(t, err)
	require.Equal(t, relationResult{Duplicate: true}, result)
	require.LessOrEqual(t, len(relation.definitions.paths), 2)
	require.LessOrEqual(t, len(relation.references.paths), 2)
}

func TestDiskRelationDuplicateSkippingHonorsCancellation(t *testing.T) {
	index, err := newValidationIndexWithOptions(validationIndexOptions{runKeys: 2, mergeFanIn: 2})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, index.Close()) })
	relation := index.relation("records")
	for range 20 {
		require.NoError(t, relation.addDefinition("duplicate"))
	}
	ctx, cancel := context.WithCancel(t.Context())
	iterator, err := relation.definitions.iterator(ctx)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, iterator.close()) })
	keys := uniqueKeys{source: iterator}
	_, ok, err := keys.next()
	require.NoError(t, err)
	require.True(t, ok)
	cancel()
	_, _, err = keys.next()
	require.ErrorIs(t, err, context.Canceled)
}

func TestMergedIteratorClosesAnExhaustedRunBeforeCompletion(t *testing.T) {
	index, err := newValidationIndexWithOptions(validationIndexOptions{runKeys: 2, mergeFanIn: 2})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, index.Close()) })
	runs := index.relation("records").definitions
	for _, key := range []string{"a", "b", "z", "zz"} {
		require.NoError(t, runs.add(key))
	}
	iterator, err := runs.iterator(t.Context())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, iterator.close()) })
	_, ok, err := iterator.next()
	require.NoError(t, err)
	require.True(t, ok)
	_, ok, err = iterator.next()
	require.NoError(t, err)
	require.True(t, ok)
	require.Nil(t, iterator.runs[0].file)
	require.NotNil(t, iterator.runs[1].file)
}
