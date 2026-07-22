package store

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestInfoSummarizesLogicalVaultAuthority(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()

	docs, err := s.Mkdir(ctx, s.RootID(), "docs")
	require.NoError(t, err)
	archive, err := s.Mkdir(ctx, docs.ID, "archive")
	require.NoError(t, err)
	first, err := s.CreateFile(ctx, docs.ID, "first.txt", fakeHash("shared"), 7, "text/plain")
	require.NoError(t, err)
	_, err = s.CreateFile(ctx, archive.ID, "second.txt", fakeHash("shared"), 7, "text/plain")
	require.NoError(t, err)
	_, err = s.CreateFile(ctx, docs.ID, "third.txt", fakeHash("unique"), 11, "text/plain")
	require.NoError(t, err)
	_, _, err = s.Trash(ctx, first.ID, -1)
	require.NoError(t, err)

	info, err := s.Info(ctx)
	require.NoError(t, err)
	assert.Equal(t, s.VaultID(), info.VaultID)
	assert.Equal(t, int64(2), info.LiveFiles)
	assert.Equal(t, int64(2), info.LiveDirectories)
	assert.Equal(t, int64(1), info.TrashedNodes)
	assert.Equal(t, int64(3), info.ContentVersions)
	assert.Equal(t, int64(25), info.LogicalVersionBytes)
	assert.Equal(t, int64(2), info.TrackedBlobs)
	assert.Equal(t, int64(18), info.TrackedBlobBytes)
}

func TestInfoEmptyVaultExcludesVirtualRoot(t *testing.T) {
	s := newTestStore(t)

	info, err := s.Info(t.Context())
	require.NoError(t, err)
	assert.Zero(t, info.LiveFiles)
	assert.Zero(t, info.LiveDirectories)
	assert.Zero(t, info.TrashedNodes)
	assert.Zero(t, info.ContentVersions)
	assert.Zero(t, info.LogicalVersionBytes)
	assert.Zero(t, info.TrackedBlobs)
	assert.Zero(t, info.TrackedBlobBytes)
}
