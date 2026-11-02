package packstore

import (
	"bytes"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFilesystemNamespaceInspectionRejectsUnmarkedContent(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	layout := layoutForStoreTest(t)
	backend, err := NewFilesystemBackend(layout, FilesystemBackendOptions{})
	require.NoError(err)
	t.Cleanup(func() { require.NoError(backend.Close()) })

	empty, err := backend.NamespaceEmpty(t.Context())
	require.NoError(err)
	assert.True(empty)
	require.NoError(os.WriteFile(
		filepath.Join(layout.Root(), "operator-note"), []byte("keep"), 0o600,
	))

	empty, err = backend.NamespaceEmpty(t.Context())
	require.NoError(err)
	assert.False(empty)
}

func TestFilesystemOwnershipCreateReattachAndTakeover(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	ctx := t.Context()
	layout := layoutForStoreTest(t)
	initial := Ownership{
		Format: OwnershipFormatV1,
		Vault:  "vault-a",
		Store:  "archive",
		Epoch:  "epoch-1",
	}

	unattached, err := NewFilesystemBackend(layout, FilesystemBackendOptions{})
	require.NoError(err)
	t.Cleanup(func() { require.NoError(unattached.Close()) })
	_, err = unattached.Ownership(ctx)
	require.ErrorIs(err, fs.ErrNotExist)
	require.NoError(unattached.ReplaceOwnership(ctx, initial, nil))

	got, err := unattached.Ownership(ctx)
	require.NoError(err)
	assert.Equal(initial, got)

	reattached, err := NewFilesystemBackend(layout, FilesystemBackendOptions{
		ExpectedOwnership: &initial,
	})
	require.NoError(err)
	t.Cleanup(func() { require.NoError(reattached.Close()) })
	content := []byte("reattached publication")
	hash := hashForTest(content)
	receipt, err := reattached.PublishLoose(
		ctx,
		hash,
		bytes.NewReader(content),
		PublishOptions{ExpectedSize: int64(len(content)), SizeKnown: true},
	)
	require.NoError(err)
	assert.Equal(hash, receipt.Hash)

	takenOver := initial
	takenOver.Epoch = "epoch-2"
	require.NoError(reattached.ReplaceOwnership(ctx, takenOver, &initial))

	replacement, err := NewFilesystemBackend(layout, FilesystemBackendOptions{
		ExpectedOwnership: &takenOver,
	})
	require.NoError(err)
	t.Cleanup(func() { require.NoError(replacement.Close()) })
	got, err = replacement.Ownership(ctx)
	require.NoError(err)
	assert.Equal(takenOver, got)
}

func TestFilesystemOwnershipMismatchFencesDestructiveWork(t *testing.T) {
	require := require.New(t)
	ctx := t.Context()
	layout := layoutForStoreTest(t)
	initial := Ownership{
		Format: OwnershipFormatV1,
		Vault:  "vault-a",
		Store:  "archive",
		Epoch:  "epoch-1",
	}
	backend, err := NewFilesystemBackend(layout, FilesystemBackendOptions{})
	require.NoError(err)
	t.Cleanup(func() { require.NoError(backend.Close()) })
	require.NoError(backend.ReplaceOwnership(ctx, initial, nil))

	stale, err := NewFilesystemBackend(layout, FilesystemBackendOptions{
		ExpectedOwnership: &initial,
	})
	require.NoError(err)
	t.Cleanup(func() { require.NoError(stale.Close()) })
	takenOver := initial
	takenOver.Epoch = "epoch-2"
	require.NoError(backend.ReplaceOwnership(ctx, takenOver, &initial))

	content := []byte("must remain unauthorized")
	_, err = stale.PublishLoose(
		ctx,
		hashForTest(content),
		bytes.NewReader(content),
		PublishOptions{ExpectedSize: int64(len(content)), SizeKnown: true},
	)
	require.ErrorIs(err, ErrStoreFenced)

	err = stale.Retire(ctx, ObjectRef{
		LooseHash: hashForTest(content), LooseEncoding: LooseEncodingRaw,
	})
	require.ErrorIs(err, ErrStoreFenced)
	assertOwnershipMismatch(t, err, initial, takenOver)
}

func TestMarshalOwnershipRejectsUnreadableMarkerSize(t *testing.T) {
	value := Ownership{
		Format: OwnershipFormatV1,
		Vault:  strings.Repeat("v", maxOwnershipMarkerBytes),
		Store:  "archive",
		Epoch:  "epoch-1",
	}

	_, err := MarshalOwnership(value)

	require.ErrorContains(t, err, "ownership marker size")
}

func assertOwnershipMismatch(
	t *testing.T,
	err error,
	expected Ownership,
	actual Ownership,
) {
	t.Helper()
	assert := assert.New(t)
	require := require.New(t)
	t.Helper()
	var mismatch *OwnershipMismatchError
	require.ErrorAs(err, &mismatch)
	require.NotNil(mismatch)
	assert.Equal(expected, mismatch.Expected)
	assert.Equal(actual, mismatch.Actual)
	assert.ErrorIs(err, ErrStoreFenced)
}
